package container

import (
	"bytes"
	"io"
	"testing"
)

// TestFrames round-trips frames of each kind through AppendFrame and
// ReadFrame, as attach sends them: an empty one included, and one too big
// to be a frame refused.
func TestFrames(t *testing.T) {
	var buf []byte
	big := bytes.Repeat([]byte("y"), maxFrame+1)
	for kind := byte(0); kind <= 4; kind++ {
		buf = AppendFrame(buf, kind, []byte{'a' + kind})
	}
	buf = AppendFrame(buf, AttachStdin, nil)
	buf = AppendFrame(buf, AttachStdout, big)
	r := bytes.NewReader(buf)
	for kind := byte(0); kind <= 4; kind++ {
		if k, p, err := ReadFrame(r); err != nil || k != kind || string(p) != string(rune('a'+kind)) {
			t.Errorf("frame %d: kind %d, %q, %v", kind, k, p, err)
		}
	}
	if k, p, err := ReadFrame(r); err != nil || k != AttachStdin || len(p) != 0 {
		t.Errorf("empty frame: kind %d, %q, %v", k, p, err)
	}
	var got []byte
	for range 2 {
		_, p, err := ReadFrame(r)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p...)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("big write came back as %d bytes", len(got))
	}
	if _, _, err := ReadFrame(r); err != io.EOF {
		t.Errorf("at the end: %v", err)
	}

	bad := AppendFrame(nil, AttachStdout, []byte("x"))
	bad[4] = 0x7f // a length past maxFrame
	if _, _, err := ReadFrame(bytes.NewReader(bad)); err == nil {
		t.Error("read a frame too big to be one")
	}
	if _, _, err := ReadFrame(bytes.NewReader(AppendFrame(nil, AttachStdout, []byte("abc"))[:9])); err != io.ErrUnexpectedEOF {
		t.Errorf("a frame cut short: %v", err)
	}
}
