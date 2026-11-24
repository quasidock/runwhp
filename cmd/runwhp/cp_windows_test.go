package main

import (
	"slices"
	"testing"
)

func TestSplitCpArg(t *testing.T) {
	for _, c := range []struct{ arg, ctr, path string }{
		{`C:\src\f`, "", `C:\src\f`},
		{`\src\f`, "", `\src\f`},
		{`.\a:b`, "", `.\a:b`},
		{`./a:b`, "", `./a:b`},
		{"web:/etc/hosts", "web", "/etc/hosts"},
		{"web:etc", "web", "etc"},
		{"plain", "", "plain"},
		{"-", "", "-"},
	} {
		if ctr, p := splitCpArg(c.arg); ctr != c.ctr || p != c.path {
			t.Errorf("splitCpArg(%q) = %q, %q; want %q, %q", c.arg, ctr, p, c.ctr, c.path)
		}
	}
}

func TestJoinLink(t *testing.T) {
	for _, c := range [][3]string{
		{"/link", "/d/f1", "/d/f1"},
		{"/d/rel", "sub", "/d/sub"},
		{"/d/rel", "../e", "/e"},
		{"/d/rel/", "sub", "/d/sub"},
	} {
		if got := joinLink(c[0], c[1]); got != c[2] {
			t.Errorf("joinLink(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestBoolFlags(t *testing.T) {
	got := boolFlags([]string{"-q", "--quiet=false", "-a", "x:/y", "--", "-q"}, "q", "quiet")
	if want := []string{"-q=true", "--quiet=false", "-a", "x:/y", "--", "-q"}; !slices.Equal(got, want) {
		t.Errorf("boolFlags = %q, want %q", got, want)
	}
}
