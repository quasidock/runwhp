package guestspec

import "testing"

func TestParseSignal(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"TERM", 15}, {"SIGTERM", 15}, {"sigterm", 15}, {" kill ", 9}, {"9", 9},
		{"HUP", 1}, {"IOT", 6}, {"POLL", 29}, {"SIGSYS", 31}, {"64", 64},
		{"RTMIN", 34}, {"SIGRTMIN+2", 36}, {"rtmax", 64}, {"RTMAX-3", 61}, {"RTMIN+30", 64},
	} {
		got, err := ParseSignal(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseSignal(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "0", "65", "-1", "FOO", "SIG", "RTMIN+31", "RTMIN-1", "RTMAX+1", "RTMIN+x", "1.5"} {
		if n, err := ParseSignal(in); err == nil {
			t.Errorf("ParseSignal(%q) = %d, want an error", in, n)
		}
	}
}

func TestSignalName(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{
		{15, "SIGTERM"}, {9, "SIGKILL"}, {6, "SIGABRT"}, {29, "SIGIO"}, {34, "SIGRTMIN"},
		{36, "SIGRTMIN+2"}, {49, "SIGRTMIN+15"}, {50, "SIGRTMAX-14"}, {64, "SIGRTMAX"},
		{32, "32"}, {99, "99"},
	} {
		if got := SignalName(c.n); got != c.want {
			t.Errorf("SignalName(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	// Every name parses back to its number.
	for n := 1; n <= 64; n++ {
		if n == 32 || n == 33 {
			continue
		}
		if got, err := ParseSignal(SignalName(n)); err != nil || got != n {
			t.Errorf("ParseSignal(SignalName(%d)) = %d, %v", n, got, err)
		}
	}
}
