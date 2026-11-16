package bodycount

import (
	"bytes"
	"io"
	"sync/atomic"
	"testing"
	"testing/iotest"
)

// The Timeline reads the counter from another goroutine while the body streams (#1861).
func TestWrap_CounterReadableWhileReading(t *testing.T) {
	var n int64
	rc := Wrap(io.NopCloser(iotest.OneByteReader(bytes.NewReader(make([]byte, 64<<10)))), &n)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Errorf("copy: %v", err)
		}
	}()

	var last int64
	for reading := true; reading; {
		select {
		case <-done:
			reading = false
		default:
		}
		got := atomic.LoadInt64(&n)
		if got < last {
			t.Fatalf("count went down: %d after %d", got, last)
		}
		last = got
	}
	if got := atomic.LoadInt64(&n); got != 65536 {
		t.Fatalf("n = %d, want 65536", got)
	}
}
