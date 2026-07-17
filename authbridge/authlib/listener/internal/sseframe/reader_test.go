package sseframe

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReader_SingleFrame(t *testing.T) {
	r := NewReader(strings.NewReader("data: hello\n\n"), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != "hello" {
		t.Errorf("frame = %q, want %q", frame, "hello")
	}
	if _, err := r.ReadFrame(); err != io.EOF {
		t.Errorf("second ReadFrame err = %v, want io.EOF", err)
	}
}

func TestReader_MultipleFrames(t *testing.T) {
	body := "data: one\n\ndata: two\n\ndata: three\n\n"
	r := NewReader(strings.NewReader(body), 0)
	want := []string{"one", "two", "three"}
	for i, w := range want {
		frame, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if string(frame) != w {
			t.Errorf("frame %d = %q, want %q", i, frame, w)
		}
	}
	if _, err := r.ReadFrame(); err != io.EOF {
		t.Errorf("trailing err = %v, want io.EOF", err)
	}
}

func TestReader_MultilineData(t *testing.T) {
	// Two data: lines for the same event are joined with \n per spec.
	body := "data: line1\ndata: line2\n\n"
	r := NewReader(strings.NewReader(body), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != "line1\nline2" {
		t.Errorf("frame = %q, want %q", frame, "line1\nline2")
	}
}

func TestReader_CommentLines(t *testing.T) {
	body := ": this is a heartbeat\n: another comment\ndata: payload\n\n"
	r := NewReader(strings.NewReader(body), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != "payload" {
		t.Errorf("frame = %q, want payload", frame)
	}
}

func TestReader_EventAndIDIgnored(t *testing.T) {
	body := "event: status-update\nid: 42\ndata: {\"k\":1}\n\n"
	r := NewReader(strings.NewReader(body), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != `{"k":1}` {
		t.Errorf("frame = %q, want JSON payload", frame)
	}
}

func TestReader_TrailingFrameWithoutBlankLine(t *testing.T) {
	// Per spec: end-of-stream dispatches whatever was accumulated
	// even without a final blank-line terminator.
	body := "data: only\n"
	r := NewReader(strings.NewReader(body), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != "only" {
		t.Errorf("frame = %q, want only", frame)
	}
	if _, err := r.ReadFrame(); err != io.EOF {
		t.Errorf("trailing err = %v, want EOF", err)
	}
}

func TestReader_EmptyStreamEOF(t *testing.T) {
	r := NewReader(strings.NewReader(""), 0)
	if _, err := r.ReadFrame(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestReader_OnlyComments(t *testing.T) {
	r := NewReader(strings.NewReader(": ping\n\n: pong\n\n"), 0)
	if _, err := r.ReadFrame(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF (no data frames)", err)
	}
}

func TestReader_CRLF(t *testing.T) {
	body := "data: a\r\n\r\ndata: b\r\n\r\n"
	r := NewReader(strings.NewReader(body), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if string(frame) != "a" {
		t.Errorf("first frame = %q, want a", frame)
	}
	frame, err = r.ReadFrame()
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if string(frame) != "b" {
		t.Errorf("second frame = %q, want b", frame)
	}
}

func TestReader_FrameTooLarge(t *testing.T) {
	// Cap at 4 bytes so even a small payload exceeds.
	r := NewReader(strings.NewReader("data: too-long\n\n"), 4)
	_, err := r.ReadFrame()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReader_FieldWithoutSpace(t *testing.T) {
	// Per spec the leading space after ":" is optional. "data:hi" is valid.
	r := NewReader(strings.NewReader("data:hi\n\n"), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != "hi" {
		t.Errorf("frame = %q, want hi", frame)
	}
}

func TestReader_LongDataLineExceedsBufioBuffer(t *testing.T) {
	// Build a single data line larger than bufio's default 4 KiB
	// buffer so the loop accumulates across ReadSlice's
	// ErrBufferFull returns.
	const n = 8192
	payload := strings.Repeat("x", n)
	body := "data: " + payload + "\n\n"
	r := NewReader(strings.NewReader(body), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != payload {
		t.Errorf("frame len = %d, want %d", len(frame), n)
	}
}

// TestReader_LastEvent_Captured locks in the fix that lets a
// re-framing proxy reproduce the upstream's "event:" line: after
// ReadFrame returns the data payload, LastEvent must return the
// event type named on the preceding "event:" line.
func TestReader_LastEvent_Captured(t *testing.T) {
	body := "event: message_start\ndata: {\"x\":1}\n\n"
	r := NewReader(strings.NewReader(body), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != `{"x":1}` {
		t.Errorf("frame = %q, want %q", frame, `{"x":1}`)
	}
	if got := string(r.LastEvent()); got != "message_start" {
		t.Errorf("LastEvent() = %q, want %q", got, "message_start")
	}
}

// TestReader_LastEvent_ResetPerFrame confirms the event type does not
// stick across frames: a frame with no "event:" line must report an
// empty LastEvent even when the previous frame named one.
func TestReader_LastEvent_ResetPerFrame(t *testing.T) {
	body := "event: message_start\ndata: one\n\ndata: two\n\n"
	r := NewReader(strings.NewReader(body), 0)

	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("first ReadFrame: %v", err)
	}
	if string(frame) != "one" {
		t.Errorf("first frame = %q, want one", frame)
	}
	if got := string(r.LastEvent()); got != "message_start" {
		t.Errorf("first LastEvent() = %q, want %q", got, "message_start")
	}

	frame, err = r.ReadFrame()
	if err != nil {
		t.Fatalf("second ReadFrame: %v", err)
	}
	if string(frame) != "two" {
		t.Errorf("second frame = %q, want two", frame)
	}
	if got := r.LastEvent(); len(got) != 0 {
		t.Errorf("second LastEvent() = %q, want empty (not sticky across frames)", got)
	}
}

// TestReader_LastEvent_EmptyForDataOnlyFrame preserves the existing
// data-only behavior: a frame with no "event:" line at all reports an
// empty LastEvent.
func TestReader_LastEvent_EmptyForDataOnlyFrame(t *testing.T) {
	r := NewReader(strings.NewReader("data: hello\n\n"), 0)
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame) != "hello" {
		t.Errorf("frame = %q, want hello", frame)
	}
	if got := r.LastEvent(); len(got) != 0 {
		t.Errorf("LastEvent() = %q, want empty", got)
	}
}
