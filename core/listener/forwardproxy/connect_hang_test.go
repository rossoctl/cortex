package forwardproxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// If the client half-closes but the upstream never does, tunnel still has to
// return so the close row can settle. Pre-fix the downstream io.Copy blocked
// forever; post-fix halfCloseDrainVar bounds the wait.
func TestTunnel_ReturnsWhenUpstreamNeverHalfCloses(t *testing.T) {
	cl, clPeer := tcpPair(t)
	up, _ := tcpPair(t) // upstream peer kept alive by tcpPair's cleanup; never writes EOF

	go func() {
		_, _ = clPeer.Write([]byte("hello"))
		_ = clPeer.(*net.TCPConn).CloseWrite()
	}()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		tunnel(cl, up)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(halfCloseDrainVar + 2*time.Second):
		t.Fatalf("tunnel did not return after %v", time.Since(start))
	}
}

// A reply that keeps streaming past halfCloseDrainVar is not truncated: the
// drain extends while bytes continue flowing and only full-closes once the
// peer actually goes idle.
func TestTunnel_ReplyStreamingPastDrainCompletes(t *testing.T) {
	cl, clPeer := tcpPair(t)
	up, upPeer := tcpPair(t)

	// Upstream: read to EOF, then drip-feed a reply longer than halfCloseDrainVar.
	chunks := 6
	gap := halfCloseDrainVar / 2 // < halfCloseDrainVar, so each gap extends the drain
	wantTotal := chunks * 5      // "chunk" × 5 bytes
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		_, _ = io.ReadAll(upPeer)
		for range chunks {
			if _, err := upPeer.Write([]byte("chunk")); err != nil {
				return
			}
			time.Sleep(gap)
		}
		_ = upPeer.Close()
	}()

	reply := make(chan []byte, 1)
	go func() {
		_, _ = clPeer.Write([]byte("hello"))
		_ = clPeer.(*net.TCPConn).CloseWrite()
		b, _ := io.ReadAll(clPeer)
		reply <- b
	}()

	done := make(chan struct{})
	go func() {
		tunnel(cl, up)
		close(done)
	}()

	deadline := time.Duration(chunks)*gap + halfCloseDrainVar + 2*time.Second
	select {
	case got := <-reply:
		if len(got) != wantTotal {
			t.Fatalf("reply = %d bytes, want %d; drain truncated a slow stream", len(got), wantTotal)
		}
	case <-time.After(deadline):
		t.Fatal("tunnel never returned a complete reply")
	}
	<-done
	<-upDone
}

// tcpPair returns two ends of a localhost TCP connection as *net.TCPConn,
// so CloseWrite is available on both.
func tcpPair(t *testing.T) (a, b net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type accepted struct {
		c   net.Conn
		err error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		ch <- accepted{c, err}
	}()
	dialer, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatal(got.err)
	}
	t.Cleanup(func() { _ = dialer.Close(); _ = got.c.Close() })
	return dialer, got.c
}
