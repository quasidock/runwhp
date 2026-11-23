//go:build linux

package main

import (
	"testing"

	"github.com/quasidock/runwhp/internal/guestspec"
)

func TestShareSource(t *testing.T) {
	for _, c := range []struct {
		m    guestspec.Mount
		want string
	}{
		{guestspec.Mount{Kind: guestspec.MountBind, Share: 0}, "/shares/0"},
		{guestspec.Mount{Kind: guestspec.MountBind, Share: 3}, "/shares/3"},
		{guestspec.Mount{Kind: guestspec.MountBind, Share: 0, Delegated: true}, "/shares-delegated/0"},
		{guestspec.Mount{Kind: guestspec.MountBind, Share: 2, Delegated: true}, "/shares-delegated/2"},
	} {
		if got := shareSource(c.m); got != c.want {
			t.Errorf("shareSource(%+v) = %s, want %s", c.m, got, c.want)
		}
	}
}
