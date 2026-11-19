package forwardproxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// If the client half-closes but the upstream never does, tunnel still has to
// return so the close row can settle: halfCloseDrainVar of idle time bounds the wait.
func TestTunnel_ReturnsWhenUpstreamNeverHalfCloses(t *testing.T) {
	cl, clPeer := tcpPair(t)
	up, _ := tcpPair(t) // upstream peer kept alive by tcpPair's cleanup; never writes EOF

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		tunnelTicked(cl, up, testTicker(ticks))
		close(done)
	}()
	_, _ = clPeer.Write([]byte("hello"))
	_ = clPeer.(*net.TCPConn).CloseWrite()

	// Four idle ticks make halfCloseDrainVar.
	for range 4 {
		tick(ticks, done)
	}
	<-done
}

// A reply that keeps streaming past halfCloseDrainVar is not truncated: the
// drain extends while bytes continue flowing and only full-closes once the
// peer actually goes idle.
func TestTunnel_ReplyStreamingPastDrainCompletes(t *testing.T) {
	cl, clPeer := tcpPair(t)
	up, upPeer := tcpPair(t)

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		tunnelTicked(cl, up, testTicker(ticks))
		close(done)
	}()
	_, _ = clPeer.Write([]byte("hello"))
	_ = clPeer.(*net.TCPConn).CloseWrite()
	_, _ = io.ReadAll(upPeer) // the client's half-close reached the upstream

	// One tick per chunk, so the reply spans twice halfCloseDrainVar. The relay counts
	// a chunk before it reads the next, so at most two ticks in a row see no new bytes:
	// half of halfCloseDrainVar.
	chunks := 8
	var got []byte
	buf := make([]byte, len("chunk"))
	for range chunks {
		if _, err := upPeer.Write([]byte("chunk")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(clPeer, buf); err != nil {
			t.Fatalf("reply = %d bytes, then %v; drain truncated a slow stream", len(got), err)
		}
		got = append(got, buf...)
		tick(ticks, done)
	}
	_ = upPeer.Close()
	rest, _ := io.ReadAll(clPeer)
	got = append(got, rest...)
	if want := chunks * len("chunk"); len(got) != want {
		t.Fatalf("reply = %d bytes, want %d", len(got), want)
	}
	<-done
}

// testTicker is a drainTicker whose ticks the test sends on ticks.
func testTicker(ticks chan time.Time) drainTicker {
	return func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
}

// tick hands the drain one tick, unless the tunnel has returned.
func tick(ticks chan<- time.Time, done <-chan struct{}) {
	select {
	case ticks <- time.Time{}:
	case <-done:
	}
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
