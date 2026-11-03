package forwardproxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/session"
)

// Tests for #1553: a tunnel must carry real protocol timing in both directions the way a
// direct connection does. Each case runs direct first, which proves the case itself, then
// through every tunnel path. #1544's two bugs (server-first hang with the bridge on, lost
// half-close) are the first two cases.

const (
	tunnelBanner = "SSH-2.0-banner\r\n"
	bigTransfer  = 4 << 20 // larger than the socket buffers, so a one-way-at-a-time copy stalls
)

var bigPayload = bytes.Repeat([]byte("0123456789abcdef"), bigTransfer/16)

// tunnelCase is one protocol shape. serve runs the origin side and returns what the origin
// observed; drive runs the client side over conn (reading from r, which may hold bytes
// the CONNECT response read buffered) and returns what the client observed. up and down
// are the bytes the tunnel's close row must report.
type tunnelCase struct {
	name                   string
	serve                  func(c net.Conn) string
	drive                  func(conn net.Conn, r io.Reader) string
	wantOrigin, wantClient string
	up, down               int64
}

var tunnelCases = []tunnelCase{
	{
		name: "server speaks first",
		serve: func(c net.Conn) string {
			if _, err := io.WriteString(c, tunnelBanner); err != nil {
				return err.Error()
			}
			return readCount(c)
		},
		drive: func(conn net.Conn, r io.Reader) string {
			buf := make([]byte, len(tunnelBanner))
			if _, err := io.ReadFull(r, buf); err != nil {
				return err.Error()
			}
			_ = conn.(*net.TCPConn).CloseWrite()
			return string(buf)
		},
		wantOrigin: "0", wantClient: tunnelBanner,
		down: int64(len(tunnelBanner)),
	},
	{
		name: "client half-closes then reads",
		serve: func(c net.Conn) string {
			n := readCount(c)
			if _, err := fmt.Fprintf(c, "GOT %s BYTES", n); err != nil {
				return err.Error()
			}
			return n
		},
		drive: func(conn net.Conn, r io.Reader) string {
			if _, err := io.WriteString(conn, "hello"); err != nil {
				return err.Error()
			}
			_ = conn.(*net.TCPConn).CloseWrite()
			b, err := io.ReadAll(r)
			if err != nil {
				return err.Error()
			}
			return string(b)
		},
		wantOrigin: "5", wantClient: "GOT 5 BYTES",
		up: 5, down: int64(len("GOT 5 BYTES")),
	},
	{
		name: "large transfer both ways at once",
		serve: func(c net.Conn) string {
			wrote := make(chan error, 1)
			go func() { _, err := c.Write(bigPayload); wrote <- err }()
			n := readCount(c)
			if err := <-wrote; err != nil {
				return err.Error()
			}
			return n
		},
		drive: func(conn net.Conn, r io.Reader) string {
			wrote := make(chan error, 1)
			go func() {
				_, err := conn.Write(bigPayload)
				_ = conn.(*net.TCPConn).CloseWrite()
				wrote <- err
			}()
			b, err := io.ReadAll(r)
			if werr := <-wrote; werr != nil {
				return werr.Error()
			}
			if err != nil {
				return err.Error()
			}
			if !bytes.Equal(b, bigPayload) {
				return fmt.Sprintf("%d bytes, not the payload", len(b))
			}
			return "payload"
		},
		wantOrigin: strconv.Itoa(bigTransfer), wantClient: "payload",
		up: bigTransfer, down: bigTransfer,
	},
	{
		name:  "client sends nothing then closes",
		serve: readCount,
		drive: func(conn net.Conn, _ io.Reader) string {
			_ = conn.Close()
			return ""
		},
		wantOrigin: "0", wantClient: "",
	},
}

// readCount reads c to EOF and returns how many bytes arrived, or the error that stopped
// it — a deadline included, which is how a hung tunnel shows up as a failure.
func readCount(c net.Conn) string {
	n, err := io.Copy(io.Discard, c)
	if err != nil {
		return err.Error()
	}
	return strconv.FormatInt(n, 10)
}

// tunnelDeadline bounds every case on every path. The bridge paths spend up to
// connectSniffTimeout in the classify peek before a silent client's tunnel starts.
const tunnelDeadline = connectSniffTimeout + 3*time.Second

// serveOnce starts an origin that accepts one connection, runs serve on it and closes it.
// The returned channel carries what serve observed.
func serveOnce(t *testing.T, serve func(net.Conn) string) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen origin: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- err.Error()
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(tunnelDeadline))
		got <- serve(c)
	}()
	return ln.Addr().String(), got
}

func TestTunnel_BehavesLikeADirectConnection(t *testing.T) {
	// Each path connects a client to origin and returns the client's conn, the reader to
	// drive it through, and a wait that returns once the tunnel has ended. store is nil
	// for the direct path, which has no rows to check.
	type path struct {
		name    string
		connect func(t *testing.T, store *session.Store, origin string) (net.Conn, io.Reader, func())
	}
	viaConnect := func(bridgePort func(origin string) int) func(*testing.T, *session.Store, string) (net.Conn, io.Reader, func()) {
		return func(t *testing.T, store *session.Store, origin string) (net.Conn, io.Reader, func()) {
			s := connectServer(t, store, nil)
			if bridgePort != nil {
				s.TLSBridge = bridgeEngine(t, bridgePort(origin), nil)
			}
			proxyAddr, done := connectProxy(t, s)
			raw, br, resp := sendConnect(t, proxyAddr, origin)
			if resp.StatusCode != 200 {
				t.Fatalf("CONNECT = %d, want 200", resp.StatusCode)
			}
			return raw, br, func() { waitDone(t, done) }
		}
	}
	paths := []path{
		{"direct", func(t *testing.T, _ *session.Store, origin string) (net.Conn, io.Reader, func()) {
			c, err := net.Dial("tcp", origin)
			if err != nil {
				t.Fatalf("dial origin: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			return c, c, func() {}
		}},
		{"CONNECT, bridge off", viaConnect(nil)},
		{"CONNECT, bridge on, port not bridged", viaConnect(func(string) int { return 443 })},
		{"CONNECT, bridge on, port bridged", viaConnect(portOf)},
		{"transparent", func(t *testing.T, store *session.Store, origin string) (net.Conn, io.Reader, func()) {
			s := connectServer(t, store, nil)
			client, proxySide := tcpPair(t)
			done := make(chan struct{}, 1)
			go func() { s.HandleTransparentConn(proxySide, origin); done <- struct{}{} }()
			return client, client, func() { waitDone(t, done) }
		}},
	}

	for _, p := range paths {
		for _, tc := range tunnelCases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				var store *session.Store
				if p.name != "direct" {
					store = session.New(5*time.Minute, 100, 0)
					t.Cleanup(store.Close)
				}
				origin, originGot := serveOnce(t, tc.serve)
				conn, r, wait := p.connect(t, store, origin)
				_ = conn.SetDeadline(time.Now().Add(tunnelDeadline))

				if got := tc.drive(conn, r); got != tc.wantClient {
					t.Errorf("client observed %q, want %q", got, tc.wantClient)
				}
				select {
				case got := <-originGot:
					if got != tc.wantOrigin {
						t.Errorf("origin observed %q, want %q", got, tc.wantOrigin)
					}
				case <-time.After(tunnelDeadline + time.Second):
					t.Fatal("origin never finished")
				}
				wait()
				if store == nil {
					return
				}
				_, closed := onePair(t, store, session.DefaultSessionID)
				if closed.BytesUp != tc.up || closed.BytesDown != tc.down {
					t.Errorf("close row bytes up %d down %d, want up %d down %d",
						closed.BytesUp, closed.BytesDown, tc.up, tc.down)
				}
			})
		}
	}
}
