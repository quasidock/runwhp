package dockerapi

import (
	"encoding/json"
	"io"
	"sync"

	"github.com/moby/moby/api/types/jsonstream"
)

// Stream writes a stream of Docker's JSON messages, one a line, as pulls,
// pushes, builds and loads answer: each message is flushed as written, if
// the writer can be.
type Stream struct {
	mu    sync.Mutex
	w     io.Writer
	flush func()
	// Failed is set once Error has been written.
	Failed bool
}

// NewStream returns a Stream writing to w, calling flush after each
// message if it is not nil.
func NewStream(w io.Writer, flush func()) *Stream {
	return &Stream{w: w, flush: flush}
}

// message is jsonstream.Message with the error's text on its own as well,
// as Docker's daemon still writes it for older clients.
type message struct {
	jsonstream.Message
	ErrorText string `json:"error,omitempty"`
}

func (s *Stream) write(m message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(append(data, '\r', '\n')); err != nil {
		return err
	}
	if s.flush != nil {
		s.flush()
	}
	return nil
}

// Status writes a status line, for the layer or image id if not empty,
// with progress if not nil.
func (s *Stream) Status(id, status string, p *jsonstream.Progress) error {
	return s.write(message{Message: jsonstream.Message{ID: id, Status: status, Progress: p}})
}

// Text writes text as a stream message, as builds and loads print.
func (s *Stream) Text(text string) error {
	return s.write(message{Message: jsonstream.Message{Stream: text}})
}

// Aux writes out-of-band data, such as a built image's ID.
func (s *Stream) Aux(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw := json.RawMessage(data)
	return s.write(message{Message: jsonstream.Message{Aux: &raw}})
}

// Error writes err as the stream's last message: the client prints it
// and fails.
func (s *Stream) Error(err error) error {
	s.Failed = true
	return s.write(message{Message: jsonstream.Message{Error: &jsonstream.Error{Message: err.Error()}}, ErrorText: err.Error()})
}

// Writer returns a writer whose writes become Text messages, a line at a
// time where it can: what a build prints as plain text.
func (s *Stream) Writer() io.Writer { return textWriter{s} }

type textWriter struct{ s *Stream }

func (t textWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := t.s.Text(string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}
