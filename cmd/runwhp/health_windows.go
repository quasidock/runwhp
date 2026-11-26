//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/guestspec"
)

// healthOverrides are what --health-* and --no-healthcheck, or a compose
// service's healthcheck, set over an image's HEALTHCHECK: a zero field
// keeps the image's.
type healthOverrides struct {
	test                                          []string // ["CMD", ...] or ["CMD-SHELL", command]
	interval, timeout, startPeriod, startInterval time.Duration
	retries                                       int
	disable                                       bool
}

// parseHealthFlags reads run's healthcheck flags, with Docker's checks.
func parseHealthFlags(o *runOptions) (healthOverrides, error) {
	var h healthOverrides
	given := o.healthCmd != "" || o.healthInterval != "" || o.healthTimeout != "" ||
		o.healthStartPeriod != "" || o.healthStartInterval != "" || o.healthRetries != ""
	if o.noHealthcheck {
		if given {
			return h, errors.New("--no-healthcheck conflicts with --health-* options")
		}
		h.disable = true
		return h, nil
	}
	if o.healthCmd != "" {
		h.test = []string{"CMD-SHELL", o.healthCmd}
	}
	for _, d := range []struct {
		flag, value string
		to          *time.Duration
	}{
		{"interval", o.healthInterval, &h.interval},
		{"timeout", o.healthTimeout, &h.timeout},
		{"start-period", o.healthStartPeriod, &h.startPeriod},
		{"start-interval", o.healthStartInterval, &h.startInterval},
	} {
		if d.value == "" {
			continue
		}
		v, err := time.ParseDuration(d.value)
		if err != nil {
			return h, fmt.Errorf("invalid argument %q for \"--health-%s\" flag: %v", d.value, d.flag, err)
		}
		*d.to = v
	}
	if o.healthRetries != "" {
		n, err := strconv.Atoi(o.healthRetries)
		if err != nil {
			return h, fmt.Errorf("invalid argument %q for \"--health-retries\" flag: give a whole number", o.healthRetries)
		}
		h.retries = n
	}
	return h, h.check()
}

// check refuses what Docker refuses of a healthcheck.
func (h healthOverrides) check() error {
	for _, d := range []struct {
		name, flag string
		v          time.Duration
	}{
		{"Interval", "interval", h.interval},
		{"Timeout", "timeout", h.timeout},
		{"StartPeriod", "start-period", h.startPeriod},
		{"StartInterval", "start-interval", h.startInterval},
	} {
		switch {
		case d.v < 0:
			return fmt.Errorf("--health-%s cannot be negative", d.flag)
		case d.v != 0 && d.v < time.Millisecond:
			return fmt.Errorf("%s in Healthcheck cannot be less than %s", d.name, time.Millisecond)
		}
	}
	if h.retries < 0 {
		return errors.New("--health-retries cannot be negative")
	}
	return nil
}

// imageHealth is the HEALTHCHECK in an image's raw configuration, or nil.
// go-containerregistry's configuration has no start interval, so it is
// read here.
func imageHealth(raw []byte) *container.Healthcheck {
	var cf struct {
		Config struct {
			Healthcheck *struct {
				Test                                          []string
				Interval, Timeout, StartPeriod, StartInterval time.Duration
				Retries                                       int
			}
		} `json:"config"`
	}
	if json.Unmarshal(raw, &cf) != nil || cf.Config.Healthcheck == nil {
		return nil
	}
	h := cf.Config.Healthcheck
	return &container.Healthcheck{Test: h.Test, Interval: h.Interval, Timeout: h.Timeout,
		StartPeriod: h.StartPeriod, StartInterval: h.StartInterval, Retries: h.Retries}
}

// resolveHealth is a container's healthcheck: the image's, with the
// overrides over it, as Docker's daemon merges them; nil if there is none.
func resolveHealth(img *container.Healthcheck, o healthOverrides) *container.Healthcheck {
	if o.disable {
		return &container.Healthcheck{Test: []string{"NONE"}}
	}
	var h container.Healthcheck
	if img != nil {
		h = *img
	}
	if len(o.test) > 0 {
		h.Test = o.test
	}
	for _, f := range []struct{ to, from *time.Duration }{
		{&h.Interval, &o.interval}, {&h.Timeout, &o.timeout}, {&h.StartPeriod, &o.startPeriod}, {&h.StartInterval, &o.startInterval},
	} {
		if *f.from != 0 {
			*f.to = *f.from
		}
	}
	if o.retries != 0 {
		h.Retries = o.retries
	}
	if len(h.Test) == 0 {
		return nil
	}
	return &h
}

// parseStopSignal checks --stop-signal, keeping it as given.
func parseStopSignal(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if _, err := guestspec.ParseSignal(s); err != nil {
		return "", fmt.Errorf("Invalid signal: %s", s)
	}
	return s, nil
}

// healthSuffix is what list's STATUS adds for a running container's
// health, as docker ps does: nothing without a healthcheck.
func healthSuffix(st container.State) string {
	if st.Health == nil {
		return ""
	}
	switch st.Health.Status {
	case container.HealthStarting:
		return " (health: starting)"
	case container.HealthHealthy, container.HealthUnhealthy:
		return " (" + st.Health.Status + ")"
	}
	return ""
}
