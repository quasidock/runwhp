package container

import (
	"slices"
	"time"
)

// Healthcheck is how a container's runwhp checks that its container is
// healthy, in Docker's HealthConfig shape: the image's HEALTHCHECK with
// run's --health-* flags over it.
type Healthcheck struct {
	// Test is ["CMD", args...], ["CMD-SHELL", command] or ["NONE"].
	Test []string `json:"test,omitempty"`
	// Zero durations and retries mean Docker's defaults.
	Interval      time.Duration `json:"interval,omitempty"`
	Timeout       time.Duration `json:"timeout,omitempty"`
	StartPeriod   time.Duration `json:"startPeriod,omitempty"`
	StartInterval time.Duration `json:"startInterval,omitempty"`
	Retries       int           `json:"retries,omitempty"`
}

// Docker's defaults for a healthcheck.
const (
	DefaultHealthInterval      = 30 * time.Second
	DefaultHealthTimeout       = 30 * time.Second
	DefaultHealthStartInterval = 5 * time.Second
	DefaultHealthRetries       = 3
)

// Enabled reports whether h names a check to run.
func (h *Healthcheck) Enabled() bool {
	return h != nil && len(h.Test) > 0 && h.Test[0] != "NONE" && len(h.Args()) > 0
}

// Args is the command the check runs in the container.
func (h *Healthcheck) Args() []string {
	if h == nil || len(h.Test) == 0 {
		return nil
	}
	switch h.Test[0] {
	case "CMD":
		return h.Test[1:]
	case "CMD-SHELL":
		if len(h.Test) < 2 {
			return nil
		}
		return []string{"/bin/sh", "-c", h.Test[1]}
	}
	return nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// IntervalOrDefault, and the methods after it, give a value or Docker's
// default for it.
func (h *Healthcheck) IntervalOrDefault() time.Duration {
	return orDefault(h.Interval, DefaultHealthInterval)
}

func (h *Healthcheck) TimeoutOrDefault() time.Duration {
	return orDefault(h.Timeout, DefaultHealthTimeout)
}

func (h *Healthcheck) StartIntervalOrDefault() time.Duration {
	return orDefault(h.StartInterval, DefaultHealthStartInterval)
}

func (h *Healthcheck) RetriesOrDefault() int {
	if h.Retries > 0 {
		return h.Retries
	}
	return DefaultHealthRetries
}

// The statuses of a container's health, as Docker names them; none is a
// container without a check, for list's filter.
const (
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthNone      = "none"
)

// Health is how a container's checks have gone, as docker inspect's
// State.Health describes it.
type Health struct {
	Status        string         `json:"status"`
	FailingStreak int            `json:"failingStreak"`
	Log           []HealthResult `json:"log,omitempty"`
}

// HealthResult is one check.
type HealthResult struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	ExitCode int       `json:"exitCode"`
	Output   string    `json:"output"`
}

// HealthLogSize is how many results Health keeps, and HealthOutputSize how
// much of a check's output each keeps, as Docker keeps them.
const (
	HealthLogSize    = 5
	HealthOutputSize = 4096
)

// TruncateHealthOutput cuts a check's output as Docker does: to
// HealthOutputSize bytes, followed by "..." if there was more.
func TruncateHealthOutput(out []byte) string {
	if len(out) <= HealthOutputSize {
		return string(out)
	}
	return string(out[:HealthOutputSize]) + "..."
}

// StartHealth is a container's health as its process starts: starting,
// with the results of its last run kept, as Docker keeps them.
func StartHealth(prev *Health) *Health {
	h := &Health{Status: HealthStarting}
	if prev != nil {
		h.Log = slices.Clone(prev.Log)
	}
	return h
}

// NextHealth is a container's health after one more check. inStartPeriod
// says the check began within the start period, when failures do not count
// against a container still starting.
func NextHealth(prev *Health, hc *Healthcheck, r HealthResult, inStartPeriod bool) *Health {
	h := StartHealth(prev)
	if prev != nil {
		h.Status, h.FailingStreak = prev.Status, prev.FailingStreak
	}
	h.Log = append(h.Log, r)
	if n := len(h.Log); n > HealthLogSize {
		h.Log = slices.Clone(h.Log[n-HealthLogSize:])
	}
	switch {
	case r.ExitCode == 0:
		h.Status, h.FailingStreak = HealthHealthy, 0
	case h.Status == HealthStarting && inStartPeriod:
	default:
		h.FailingStreak++
		if h.FailingStreak >= hc.RetriesOrDefault() {
			h.Status = HealthUnhealthy
		}
	}
	return h
}

// NextHealthInterval is how long to wait before the next check: the start
// interval while a container is starting within its start period, as
// Docker's, the interval otherwise.
func NextHealthInterval(h *Health, hc *Healthcheck, sinceStart time.Duration) time.Duration {
	if sinceStart < hc.StartPeriod && (h == nil || h.Status == HealthStarting) {
		return hc.StartIntervalOrDefault()
	}
	return hc.IntervalOrDefault()
}
