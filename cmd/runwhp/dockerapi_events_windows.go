//go:build windows

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/dockerapi"
)

// eventBroker keeps the server's events: those its handlers make, and
// those a watcher finds by looking at the store's containers, which shows
// what other runwhps did too.
type eventBroker struct {
	s *apiServer
	*dockerapi.Broker

	mu    sync.Mutex
	known map[string]dockerapi.ContainerSnap
	// poke wakes the watcher early, when a subscriber comes.
	poke chan struct{}
}

func newEventBroker(s *apiServer) *eventBroker {
	return &eventBroker{s: s, Broker: dockerapi.NewBroker(1024), poke: make(chan struct{}, 1)}
}

// snapOf is what the watcher remembers of c.
func snapOf(c *container.Container) dockerapi.ContainerSnap {
	return dockerapi.ContainerSnap{
		ID: c.ID, Name: c.Name, Image: imageOf(c.Config), Labels: c.Config.Labels, Created: c.Config.Created,
		Running: c.State.Active() && c.State.Status != container.Created && c.State.Status != container.Creating,
		Paused:  c.State.Paused && c.State.Active(), StartedAt: c.State.StartedAt, FinishedAt: c.State.FinishedAt,
		ExitCode: c.State.ExitCode, Health: healthOf(c),
	}
}

func healthOf(c *container.Container) string {
	if c.State.Health == nil || c.Config.Healthcheck == nil {
		return ""
	}
	return c.State.Health.Status
}

// look compares the store's containers with what it saw last, and emits
// what changed.
func (b *eventBroker) look() {
	cs, err := b.s.containers.List()
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	prev := b.known
	cur := make(map[string]dockerapi.ContainerSnap, len(cs))
	for _, c := range cs {
		if c.State.Status == container.Creating {
			// Being made, or booted again to start: as it was, until it
			// is one or the other.
			if p, ok := prev[c.ID]; ok {
				cur[c.ID] = p
			}
			continue
		}
		cur[c.ID] = snapOf(c)
	}
	b.known = cur
	if prev == nil {
		return // the first look: nothing happened yet
	}
	for _, m := range dockerapi.ContainerEvents(prev, cur, time.Now()) {
		b.Emit(m)
	}
}

// watch looks at the store every 250 ms while anyone follows the events,
// and every second otherwise, until the server stops.
func (b *eventBroker) watch(ctx context.Context) {
	b.look()
	for {
		d := time.Second
		if b.Subscribers() > 0 {
			d = 250 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-b.poke:
		case <-time.After(d):
		}
		b.look()
	}
}

func (s *apiServer) eventListeners() int { return s.events.Subscribers() }

// emitContainer emits an event of c that no state shows: a kill, an
// attach, an exec.
func (s *apiServer) emitContainer(c *container.Container, action events.Action, extra map[string]string) {
	// What the store shows happened first comes first.
	s.events.look()
	s.events.Emit(dockerapi.ContainerMessage(snapOf(c), action, extra, time.Now()))
}

// emit emits an event of an image, a network or a volume.
func (s *apiServer) emit(typ events.Type, action events.Action, id string, attrs map[string]string) {
	if attrs == nil {
		attrs = map[string]string{}
	}
	now := time.Now()
	s.events.Emit(events.Message{Type: typ, Action: action, Scope: "local",
		Actor: events.Actor{ID: id, Attributes: attrs}, Time: now.Unix(), TimeNano: now.UnixNano()})
}

func itoa(n int) string { return strconv.Itoa(n) }

func (s *apiServer) getEvents(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	now := time.Now()
	since, err := apiTime(q.Get("since"), now)
	if err != nil {
		return err
	}
	until, err := apiTime(q.Get("until"), now)
	if err != nil {
		return err
	}
	m, err := dockerapi.DecodeFilters(q.Get("filters"))
	if err != nil {
		return badRequest("%v", err)
	}
	if err := dockerapi.CheckFilterKeys(m, "type", "event", "container", "image", "network", "volume", "label", "scope",
		"daemon", "node", "plugin", "service", "secret", "config"); err != nil {
		return badRequest("%v", err)
	}
	f := dockerapi.EventFilter(m)
	// Catch up first, so that what happened just now is in the backlog.
	s.events.look()
	past, ch, cancel := s.events.Subscribe()
	defer cancel()
	select {
	case s.events.poke <- struct{}{}:
	default:
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	in := func(m events.Message) bool {
		t := time.Unix(0, m.TimeNano)
		return (since.IsZero() || !t.Before(since)) && (until.IsZero() || !t.After(until))
	}
	write := func(m events.Message) error {
		if !f.Match(m) || !in(m) {
			return nil
		}
		if err := enc.Encode(m); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	if !since.IsZero() {
		for _, m := range past {
			if write(m) != nil {
				return nil
			}
		}
	}
	var end <-chan time.Time
	if !until.IsZero() {
		if !until.After(time.Now()) {
			return nil
		}
		t := time.NewTimer(time.Until(until))
		defer t.Stop()
		end = t.C
	}
	for {
		select {
		case m := <-ch:
			if write(m) != nil {
				return nil
			}
		case <-end:
			return nil
		case <-r.Context().Done():
			return nil
		case <-s.ctx.Done():
			return nil
		}
	}
}

func (s *apiServer) getContainerStats(w http.ResponseWriter, r *http.Request) error {
	c, err := s.resolve(r.PathValue("id"))
	if err != nil {
		return err
	}
	stream := true
	if v := r.URL.Query().Get("stream"); v != "" {
		stream = boolParam(r, "stream")
	}
	oneShot := boolParam(r, "one-shot")
	w.Header().Set("Content-Type", "application/json")
	sample := func() *container.Stats {
		if !c.State.Active() {
			return nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		rep, err := container.Ask(ctx, s.root, c.ID, container.Request{Op: container.OpStats})
		if err != nil || !rep.OK || rep.Stats == nil {
			return nil
		}
		return rep.Stats
	}
	write := func(cur, prev *container.Stats) error {
		var resp any
		if cur == nil {
			// A container that is not running uses nothing, as Docker
			// reports it.
			resp = dockerapiZeroStats(c)
		} else {
			resp = dockerapi.Stats(c.ID, containerName(c), cur, prev)
		}
		data, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}
	prev := sample()
	if !stream {
		if oneShot || prev == nil {
			return write(prev, nil)
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			return nil
		}
		return write(sample(), prev)
	}
	if err := write(prev, nil); err != nil {
		return nil
	}
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-s.ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
		if cur, err := s.containers.Get(c.ID); err == nil {
			c = cur
		} else {
			return nil
		}
		cur := sample()
		if write(cur, prev) != nil {
			return nil
		}
		prev = cur
	}
}

func dockerapiZeroStats(c *container.Container) map[string]any {
	return map[string]any{
		"id": c.ID, "name": containerName(c), "os_type": "linux",
		"read": "0001-01-01T00:00:00Z", "preread": "0001-01-01T00:00:00Z",
		"pids_stats": map[string]any{}, "blkio_stats": map[string]any{
			"io_service_bytes_recursive": nil, "io_serviced_recursive": nil, "io_queue_recursive": nil,
			"io_service_time_recursive": nil, "io_wait_time_recursive": nil, "io_merged_recursive": nil,
			"io_time_recursive": nil, "sectors_recursive": nil,
		},
		"num_procs": 0, "storage_stats": map[string]any{},
		"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": 0, "usage_in_kernelmode": 0, "usage_in_usermode": 0}, "throttling_data": map[string]any{"periods": 0, "throttled_periods": 0, "throttled_time": 0}},
		"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 0, "usage_in_kernelmode": 0, "usage_in_usermode": 0}, "throttling_data": map[string]any{"periods": 0, "throttled_periods": 0, "throttled_time": 0}},
		"memory_stats": map[string]any{},
	}
}
