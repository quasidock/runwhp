package dockerapi

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"
)

// ContainerSnap is what the event watcher remembers of a container between
// looks at the store: enough to tell what happened to it since.
type ContainerSnap struct {
	ID, Name, Image string
	Created         time.Time
	Labels          map[string]string
	Running, Paused bool
	StartedAt       time.Time
	FinishedAt      time.Time
	ExitCode        int
	Health          string // starting, healthy, unhealthy, or empty
}

// ContainerEvents is what happened between two looks at the store's
// containers, as Docker's daemon reports it: created, started, died,
// paused, unpaused, renamed, health changed and destroyed, at now.
func ContainerEvents(prev, cur map[string]ContainerSnap, now time.Time) []events.Message {
	var out []events.Message
	// Each event is dated as the container's state dates it, where it
	// does, so that what the watcher finds late still sorts in order.
	add := func(c ContainerSnap, action events.Action, extra map[string]string) {
		at := now
		switch {
		case action == events.ActionCreate && !c.Created.IsZero():
			at = c.Created
		case action == events.ActionStart && !c.StartedAt.IsZero():
			at = c.StartedAt
		case action == events.ActionDie && !c.FinishedAt.IsZero():
			at = c.FinishedAt
		}
		if at.After(now) {
			at = now
		}
		out = append(out, ContainerMessage(c, action, extra, at))
	}
	ids := make([]string, 0, len(cur))
	for id := range cur {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return cur[ids[i]].StartedAt.Before(cur[ids[j]].StartedAt) || ids[i] < ids[j] })
	for _, id := range ids {
		c := cur[id]
		p, known := prev[id]
		if !known {
			add(c, events.ActionCreate, nil)
			if !c.StartedAt.IsZero() {
				add(c, events.ActionStart, nil)
			}
			if !c.FinishedAt.IsZero() && !c.FinishedAt.Before(c.StartedAt) && !c.Running {
				add(c, events.ActionDie, map[string]string{"exitCode": strconv.Itoa(c.ExitCode)})
			}
			continue
		}
		if p.Name != c.Name && p.Name != "" {
			add(c, events.ActionRename, map[string]string{"oldName": "/" + p.Name})
		}
		// A container that died and started again since shows both, in
		// that order.
		if !c.FinishedAt.Equal(p.FinishedAt) && !c.FinishedAt.IsZero() {
			add(c, events.ActionDie, map[string]string{"exitCode": strconv.Itoa(c.ExitCode)})
		}
		if !c.StartedAt.Equal(p.StartedAt) && !c.StartedAt.IsZero() {
			add(c, events.ActionStart, nil)
		}
		if c.Paused != p.Paused && c.Running {
			if c.Paused {
				add(c, events.ActionPause, nil)
			} else {
				add(c, events.ActionUnPause, nil)
			}
		}
		if c.Health != p.Health && c.Health != "" {
			add(c, events.Action(string(events.ActionHealthStatus)+": "+c.Health), nil)
		}
	}
	gone := make([]string, 0)
	for id := range prev {
		if _, ok := cur[id]; !ok {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		p := prev[id]
		if p.Running {
			add(p, events.ActionDie, map[string]string{"exitCode": strconv.Itoa(p.ExitCode)})
		}
		add(p, events.ActionDestroy, nil)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TimeNano < out[j].TimeNano })
	return out
}

// ContainerMessage is an event of a container, with Docker's attributes:
// its labels, name and image, and extra.
func ContainerMessage(c ContainerSnap, action events.Action, extra map[string]string, now time.Time) events.Message {
	attrs := map[string]string{}
	for k, v := range c.Labels {
		attrs[k] = v
	}
	attrs["name"] = c.Name
	attrs["image"] = c.Image
	for k, v := range extra {
		attrs[k] = v
	}
	return events.Message{
		Type: events.ContainerEventType, Action: action, Scope: "local",
		Actor: events.Actor{ID: c.ID, Attributes: attrs},
		Time:  now.Unix(), TimeNano: now.UnixNano(),
	}
}

// Broker keeps the last events and hands new ones to subscribers.
type Broker struct {
	mu   sync.Mutex
	ring []events.Message
	max  int
	subs map[chan events.Message]struct{}
}

// NewBroker returns a Broker that keeps the last max events.
func NewBroker(max int) *Broker {
	return &Broker{max: max, subs: map[chan events.Message]struct{}{}}
}

// Emit records m and hands it to every subscriber that keeps up; one that
// does not loses it.
func (b *Broker) Emit(m events.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ring = append(b.ring, m)
	if len(b.ring) > b.max {
		b.ring = append(b.ring[:0], b.ring[len(b.ring)-b.max:]...)
	}
	for ch := range b.subs {
		select {
		case ch <- m:
		default:
		}
	}
}

// Subscribe returns the events kept so far, a channel of those to come,
// and a function that ends the subscription.
func (b *Broker) Subscribe() ([]events.Message, <-chan events.Message, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan events.Message, 256)
	b.subs[ch] = struct{}{}
	past := append([]events.Message(nil), b.ring...)
	return past, ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs, ch)
	}
}

// Subscribers is how many follow the events.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// EventFilter is an events request's filters.
type EventFilter map[string][]string

// Match reports whether m passes the filters: a key's values are
// alternatives, and every key must match, as Docker's.
func (f EventFilter) Match(m events.Message) bool {
	any := func(key string, ok func(string) bool) bool {
		vs := f[key]
		if len(vs) == 0 {
			return true
		}
		for _, v := range vs {
			if ok(v) {
				return true
			}
		}
		return false
	}
	actor := func(v string) bool {
		return v == m.Actor.ID || strings.HasPrefix(m.Actor.ID, v) || v == m.Actor.Attributes["name"]
	}
	return any("type", func(v string) bool { return v == string(m.Type) }) &&
		any("event", func(v string) bool {
			return v == string(m.Action) || strings.HasPrefix(string(m.Action), v+":")
		}) &&
		any("container", func(v string) bool { return m.Type == events.ContainerEventType && actor(v) }) &&
		any("image", func(v string) bool {
			return m.Type == events.ImageEventType && actor(v) || m.Actor.Attributes["image"] == v
		}) &&
		any("network", func(v string) bool { return m.Type == events.NetworkEventType && actor(v) }) &&
		any("volume", func(v string) bool { return m.Type == events.VolumeEventType && actor(v) }) &&
		any("scope", func(v string) bool { return v == "local" }) &&
		f.matchLabels(m.Actor.Attributes)
}

func (f EventFilter) matchLabels(attrs map[string]string) bool {
	for _, l := range f["label"] {
		k, v, hasValue := strings.Cut(l, "=")
		got, ok := attrs[k]
		if !ok || hasValue && got != v {
			return false
		}
	}
	return true
}
