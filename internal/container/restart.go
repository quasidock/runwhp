package container

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Restart policies, Docker's. Without a daemon to start containers again
// when it starts, always and unless-stopped do the same.
const (
	RestartNo            = "no"
	RestartOnFailure     = "on-failure"
	RestartAlways        = "always"
	RestartUnlessStopped = "unless-stopped"
)

// RestartPolicy says when a container whose process has ended is started
// again, by the runwhp running it.
type RestartPolicy struct {
	Name string `json:"name,omitempty"`
	// MaxRetries bounds on-failure's restarts; 0 means no bound.
	MaxRetries int `json:"maxRetries,omitempty"`
}

// ParseRestartPolicy reads --restart: no, on-failure[:N], always or
// unless-stopped.
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	name, n, hasN := strings.Cut(strings.TrimSpace(s), ":")
	switch name {
	case "", RestartNo:
		if hasN {
			break
		}
		return RestartPolicy{}, nil
	case RestartAlways, RestartUnlessStopped:
		if hasN {
			return RestartPolicy{}, fmt.Errorf("--restart %s: only on-failure takes a maximum number of restarts", s)
		}
		return RestartPolicy{Name: name}, nil
	case RestartOnFailure:
		p := RestartPolicy{Name: name}
		if hasN {
			max, err := strconv.Atoi(n)
			if err != nil || max < 0 {
				return RestartPolicy{}, fmt.Errorf("--restart %s: give the maximum number of restarts as a number", s)
			}
			p.MaxRetries = max
		}
		return p, nil
	}
	return RestartPolicy{}, fmt.Errorf("--restart %s: give no, on-failure[:N], always or unless-stopped", s)
}

// IsSet reports whether the policy restarts at all.
func (p RestartPolicy) IsSet() bool { return p.Name != "" && p.Name != RestartNo }

func (p RestartPolicy) String() string {
	switch {
	case !p.IsSet():
		return RestartNo
	case p.Name == RestartOnFailure && p.MaxRetries > 0:
		return fmt.Sprintf("%s:%d", p.Name, p.MaxRetries)
	}
	return p.Name
}

// ShouldRestart decides, as Docker's restart manager does, whether a
// container whose process ended with exit, after count restarts, starts
// again. It is not asked about a container stopped by hand.
func (p RestartPolicy) ShouldRestart(exit, count int) bool {
	switch p.Name {
	case RestartAlways, RestartUnlessStopped:
		return true
	case RestartOnFailure:
		return exit != 0 && (p.MaxRetries == 0 || count < p.MaxRetries)
	}
	return false
}

// Docker's delays between restarts: the first after 100 ms, each twice the
// one before, up to a minute; a run that lasted 10 s starts them over.
const (
	RestartDelayInitial = 100 * time.Millisecond
	RestartDelayMax     = time.Minute
	restartDelayReset   = 10 * time.Second
)

// Backoff is the delay before each restart.
type Backoff struct {
	Initial, Max time.Duration
	cur          time.Duration
}

// Next returns the delay before the restart after a run of ran.
func (b *Backoff) Next(ran time.Duration) time.Duration {
	if b.Initial <= 0 {
		b.Initial = RestartDelayInitial
	}
	if b.Max <= 0 {
		b.Max = RestartDelayMax
	}
	switch {
	case b.cur == 0 || ran >= restartDelayReset:
		b.cur = b.Initial
	default:
		b.cur = min(2*b.cur, b.Max)
	}
	return b.cur
}

// Reset starts the delays over.
func (b *Backoff) Reset() { b.cur = 0 }
