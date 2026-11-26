package container

import (
	"errors"
	"fmt"
	"strings"
)

// Errors that say why a container refused what was asked of it. They come
// wrapped in Refuse's, which words them for people, so that callers can
// tell them apart with errors.Is: the Engine API answers each with a status
// of its own.
var (
	ErrRunning    = errors.New("container is running")
	ErrNotRunning = errors.New("container is not running")
	ErrBusy       = errors.New("container is busy")
	ErrPaused     = errors.New("container is paused")
	ErrNotPaused  = errors.New("container is not paused")
)

type refusal struct {
	msg  string
	kind error
}

func (r *refusal) Error() string { return r.msg }
func (r *refusal) Unwrap() error { return r.kind }

// Refuse returns an error that reads as format says and is kind to
// errors.Is.
func Refuse(kind error, format string, a ...any) error {
	return &refusal{msg: fmt.Sprintf(format, a...), kind: kind}
}

// ReplyError is a Reply's Error as an error, recognising the refusals a
// container's runwhp words: it returns nil for a reply that is OK.
func ReplyError(rep Reply) error {
	if rep.OK {
		return nil
	}
	msg := rep.Error
	if msg == "" {
		msg = "the container's runwhp refused, without saying why"
	}
	switch {
	case strings.Contains(msg, ErrRunning.Error()):
		return Refuse(ErrRunning, "%s", msg)
	case strings.Contains(msg, "is not running"), strings.HasSuffix(msg, ErrNotRunning.Error()):
		return Refuse(ErrNotRunning, "%s", msg)
	case strings.Contains(msg, "is already paused"):
		return Refuse(ErrPaused, "%s", msg)
	case strings.Contains(msg, "is not paused"):
		return Refuse(ErrNotPaused, "%s", msg)
	case strings.Contains(msg, "still being created"), strings.Contains(msg, "being created or stopped"):
		return Refuse(ErrBusy, "%s", msg)
	case strings.Contains(msg, ErrNetworksRunning.Error()):
		return Refuse(ErrRunning, "%s", msg)
	}
	return errors.New(msg)
}
