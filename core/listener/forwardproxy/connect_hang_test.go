package forwardproxy

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/session"
)

// A server-first protocol (SSH, SMTP, FTP) sends a banner before the client
// writes anything. Through CONNECT with the TLS bridge on, the proxy peeks
// five bytes of the client to classify TLS vs opaque — the client sends
// nothing until it sees the banner, so both sides wait on each other until
// the sniff deadline elapses.
func TestHandleConnect_ServerFirstBannerArrivesPromptly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("SSH-2.0-banner\r\n"))
	}()
	target := ln.Addr().String()

	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	s := connectServer(t, store, bridgeEngine(t, portOf(target), nil))
	proxyAddr, _ := connectProxy(t, s)

	raw, br, resp := sendConnect(t, proxyAddr, target)
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT = %d", resp.StatusCode)
	}
	// Pre-fix the sniff peeks the client with no deadline and the client sends
	// nothing, so the tunnel never starts and the banner never flows. Post-fix
	// the peek is bound by connectSniffTimeout, after which the opaque path
	// takes over.
	_ = raw.SetReadDeadline(time.Now().Add(connectSniffTimeout + 2*time.Second))
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read banner: %v", err)
	}
	if !strings.HasPrefix(line, "SSH-2.0-") {
		t.Fatalf("got %q, want an SSH banner", line)
	}
}

// Half-close: a client that sends its request, calls CloseWrite, then reads
// the reply must receive it. Today tunnel closes both halves as soon as the
// client→upstream copy sees EOF.
func TestTunnel_HalfCloseRelaysTheReply(t *testing.T) {
	// Two localhost TCP pairs so CloseWrite is available on both sides.
	cl, clPeer := tcpPair(t)
	up, upPeer := tcpPair(t)

	// Upstream: read to EOF, then write a reply, then close.
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		_, _ = io.ReadAll(upPeer)
		_, _ = upPeer.Write([]byte("REPLY"))
		_ = upPeer.Close()
	}()

	// Client: write the request, half-close send, read reply.
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

	select {
	case got := <-reply:
		if string(got) != "REPLY" {
			t.Fatalf("reply = %q, want the upstream's answer", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client never got the reply; half-close tore the tunnel down")
	}
	<-done
	<-upDone
}

// A wrapped client conn (as the TLS-bridge CONNECT path uses) must still
// deliver the half-close through tunnel, since peekedConn alone embeds
// net.Conn and would otherwise miss the CloseWrite type assertion.
func TestTunnel_HalfCloseThroughPeekedConn(t *testing.T) {
	cl, clPeer := tcpPair(t)
	up, upPeer := tcpPair(t)

	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		_, _ = io.ReadAll(upPeer)
		_, _ = upPeer.Write([]byte("REPLY"))
		_ = upPeer.Close()
	}()

	reply := make(chan []byte, 1)
	go func() {
		_, _ = clPeer.Write([]byte("hello"))
		_ = clPeer.(*net.TCPConn).CloseWrite()
		b, _ := io.ReadAll(clPeer)
		reply <- b
	}()

	wrapped := &peekedConn{Conn: cl, r: bufio.NewReader(cl)}
	done := make(chan struct{})
	go func() {
		tunnel(wrapped, up)
		close(done)
	}()

	select {
	case got := <-reply:
		if string(got) != "REPLY" {
			t.Fatalf("reply = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wrapped client never got the reply")
	}
	<-done
	<-upDone
}

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
