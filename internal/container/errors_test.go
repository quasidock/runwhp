package container

import (
	"errors"
	"testing"
)

func TestReplyError(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		kind error
	}{
		{"container is not running", ErrNotRunning},
		{"container 0123 is not running", ErrNotRunning},
		{"container 0123 is already paused", ErrPaused},
		{"Container 0123 is not paused", ErrNotPaused},
		{"the container is still being created", ErrBusy},
		{"the container is being created or stopped; try again", ErrBusy},
		{"container is running", ErrRunning},
		{ErrNetworksRunning.Error(), ErrRunning},
		{"the image has no command", nil},
	} {
		err := ReplyError(Reply{Error: tc.msg})
		if err == nil || err.Error() != tc.msg {
			t.Errorf("%q: got %v, want the same words", tc.msg, err)
			continue
		}
		for _, k := range []error{ErrNotRunning, ErrPaused, ErrNotPaused, ErrBusy, ErrRunning} {
			if got := errors.Is(err, k); got != (k == tc.kind) {
				t.Errorf("%q: errors.Is(%v) = %v", tc.msg, k, got)
			}
		}
	}
	if err := ReplyError(Reply{OK: true}); err != nil {
		t.Errorf("an OK reply gave %v", err)
	}
	if err := ReplyError(Reply{}); err == nil {
		t.Error("a refusal without words gave no error")
	}
}

func TestRefuseWording(t *testing.T) {
	err := Refuse(ErrBusy, "container %s is busy: %s", "abc", "a cp holds it")
	if err.Error() != "container abc is busy: a cp holds it" || !errors.Is(err, ErrBusy) {
		t.Fatalf("got %q", err)
	}
}
