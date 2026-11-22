package guestspec

import (
	"fmt"
	"strconv"
	"strings"
)

// Linux's signal numbers on x86-64, which is what the guest runs. The host
// is Windows, whose syscall package has only a handful of them.
var signals = []struct {
	name string
	num  int
}{
	{"HUP", 1}, {"INT", 2}, {"QUIT", 3}, {"ILL", 4}, {"TRAP", 5}, {"ABRT", 6},
	{"IOT", 6}, {"BUS", 7}, {"FPE", 8}, {"KILL", 9}, {"USR1", 10}, {"SEGV", 11},
	{"USR2", 12}, {"PIPE", 13}, {"ALRM", 14}, {"TERM", 15}, {"STKFLT", 16},
	{"CHLD", 17}, {"CONT", 18}, {"STOP", 19}, {"TSTP", 20}, {"TTIN", 21},
	{"TTOU", 22}, {"URG", 23}, {"XCPU", 24}, {"XFSZ", 25}, {"VTALRM", 26},
	{"PROF", 27}, {"WINCH", 28}, {"IO", 29}, {"POLL", 29}, {"PWR", 30},
	{"SYS", 31},
}

// Signal numbers runwhp itself sends.
const (
	SIGINT  = 2
	SIGKILL = 9
	SIGTERM = 15
)

// Real-time signals run from SIGRTMIN to SIGRTMAX. glibc and musl keep the
// first two or three for themselves and call SIGRTMIN 34; so does
// kill -l, and so does Docker.
const (
	sigRTMin = 34
	sigRTMax = 64
)

// ParseSignal reads a signal as Docker and kill do: a number, or a name
// with or without SIG, in any case, including RTMIN+n and RTMAX-n.
func ParseSignal(s string) (int, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if n, err := strconv.Atoi(t); err == nil {
		if n < 1 || n > sigRTMax {
			return 0, fmt.Errorf("invalid signal %q: numbers run from 1 to %d", s, sigRTMax)
		}
		return n, nil
	}
	t = strings.TrimPrefix(t, "SIG")
	for _, sig := range signals {
		if sig.name == t {
			return sig.num, nil
		}
	}
	for _, rt := range []struct {
		prefix string
		base   int
		sign   int
	}{{"RTMIN", sigRTMin, 1}, {"RTMAX", sigRTMax, -1}} {
		rest, ok := strings.CutPrefix(t, rt.prefix)
		if !ok {
			continue
		}
		if rest == "" {
			return rt.base, nil
		}
		op := "+"
		if rt.sign < 0 {
			op = "-"
		}
		if d, ok := strings.CutPrefix(rest, op); ok {
			if n, err := strconv.Atoi(d); err == nil && n >= 0 && n <= sigRTMax-sigRTMin {
				return rt.base + rt.sign*n, nil
			}
		}
	}
	return 0, fmt.Errorf("invalid signal %q", s)
}

// SignalName names signal n as kill -l does, as in SIGTERM or SIGRTMIN+2,
// or gives its number when it has no name.
func SignalName(n int) string {
	for _, sig := range signals {
		if sig.num == n {
			return "SIG" + sig.name
		}
	}
	switch {
	case n == sigRTMin:
		return "SIGRTMIN"
	case n > sigRTMin && n <= sigRTMin+(sigRTMax-sigRTMin)/2:
		return fmt.Sprintf("SIGRTMIN+%d", n-sigRTMin)
	case n == sigRTMax:
		return "SIGRTMAX"
	case n > sigRTMin && n < sigRTMax:
		return fmt.Sprintf("SIGRTMAX-%d", sigRTMax-n)
	}
	return strconv.Itoa(n)
}
