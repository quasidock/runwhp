package hostfs

import (
	"errors"
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/fuse"
)

// esc is the host's stand-in for a character Windows forbids.
func esc(c rune) string { return string(escapeBase + c) }

func TestNames(t *testing.T) {
	for _, tc := range []struct{ guest, host string }{
		{"plain.txt", "plain.txt"},
		{"a:b", "a" + esc(':') + "b"},
		{`what?*<>|"\`, "what" + esc('?') + esc('*') + esc('<') + esc('>') + esc('|') + esc('"') + esc('\\')},
		{"tab\there", "tab" + esc('\t') + "here"},
		{"trailing dot.", "trailing dot."},
		{"CON", "CON"},
		{"日本語", "日本語"},
	} {
		got, err := hostName(tc.guest)
		if err != nil || got != tc.host {
			t.Errorf("hostName(%q) = %q, %v; want %q", tc.guest, got, err, tc.host)
		}
		if back := guestName(tc.host); back != tc.guest {
			t.Errorf("guestName(%q) = %q, want %q", tc.host, back, tc.guest)
		}
	}
	// A private-use character that does not stand for a forbidden one stays.
	if got := guestName(esc('A')); got != esc('A') {
		t.Errorf("guestName changed %q to %q", esc('A'), got)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "nul\x00", "\xff\xfe", strings.Repeat("x", 256)} {
		if _, err := hostName(bad); !errors.Is(err, fuse.EINVAL) && !errors.Is(err, fuse.ENAMETOOLONG) {
			t.Errorf("hostName(%q) = %v, want an error", bad, err)
		}
	}
}
