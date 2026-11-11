package docker

import (
	"bytes"
	"strings"
	"testing"
)

// The A0 output bound: RAM in the control plane stays ~max bytes per
// stream regardless of how much the command prints; the head is kept in
// full, the tail overwrites itself, and the truncated flag flips.
func TestHeadTailWriterBounded(t *testing.T) {
	var buf bytes.Buffer
	var trunc bool
	w := newHeadTailWriter(&buf, 1000, &trunc)

	payload := strings.Repeat("A", 50_000)
	if n, err := w.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	w.flushTail()

	if !trunc {
		t.Error("truncated flag not set")
	}
	if int64(buf.Len()) > 1000+250+64 { // head + tail + marker slack
		t.Errorf("buffer = %d bytes, RAM bound exceeded", buf.Len())
	}
	if !strings.HasPrefix(buf.String(), strings.Repeat("A", 1000)) {
		t.Error("head (first max bytes) not preserved")
	}
	if !strings.Contains(buf.String(), "-- output truncated --") {
		t.Error("truncation marker missing")
	}
	if !strings.HasSuffix(buf.String(), strings.Repeat("A", 250)) {
		t.Error("tail (last max/4 bytes) not preserved")
	}
}

func TestHeadTailWriterUnderLimit(t *testing.T) {
	var buf bytes.Buffer
	var trunc bool
	w := newHeadTailWriter(&buf, 1000, &trunc)
	w.Write([]byte("hello"))
	w.flushTail()
	if trunc {
		t.Error("truncated set for small output")
	}
	if buf.String() != "hello" {
		t.Errorf("buf = %q, want hello", buf.String())
	}
}

func TestHeadTailWriterMultiWrite(t *testing.T) {
	var buf bytes.Buffer
	var trunc bool
	w := newHeadTailWriter(&buf, 10, &trunc)
	for i := 0; i < 100; i++ {
		w.Write([]byte("0123456789"))
	}
	w.flushTail()
	if !trunc {
		t.Error("truncated not set")
	}
	if !strings.HasPrefix(buf.String(), "0123456789") {
		t.Error("head wrong")
	}
	// tail keeps the LAST max/4 bytes; with max=10 that is 2 bytes ("89").
	if !strings.HasSuffix(buf.String(), "89") {
		t.Errorf("tail wrong: %q", buf.String()[buf.Len()-20:])
	}
}

// ExecV2 with no v2 fields produces the identical docker argv as Exec —
// the legacy contract ("{cmd} behaves exactly as before") — covered
// end-to-end by scripts/e2e.sh.
