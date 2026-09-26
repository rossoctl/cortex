package transparentproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
)

// fakeAddr is a net.Addr with a caller-chosen String(), so CheckDst's
// "dst equals the listener's own address" arm can be exercised without
// binding a real socket.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// TestCheckDst covers the guard shared by the outbound dispatcher and the
// inbound listener. The inbound cases are the ones the original outbound-only
// guard did not consider: a pod's own IP on the app's port is the NORMAL
// recovered destination for captured ingress and must be allowed through.
func TestCheckDst(t *testing.T) {
	tests := []struct {
		name    string
		local   net.Addr
		dst     string
		wantErr error
	}{
		{
			name:  "outbound: external host is fine",
			local: fakeAddr("10.244.0.5:8082"),
			dst:   "93.184.216.34:443",
		},
		{
			name:  "inbound: pod's own IP on the app port is the normal case",
			local: fakeAddr("10.244.0.5:8083"),
			dst:   "10.244.0.5:8000",
		},
		{
			name:    "self-dial to the listener's own address",
			local:   fakeAddr("10.244.0.5:8083"),
			dst:     "10.244.0.5:8083",
			wantErr: ErrSelfReferential,
		},
		{
			name:    "loopback dst would spiral",
			local:   fakeAddr("10.244.0.5:8083"),
			dst:     "127.0.0.1:8000",
			wantErr: ErrSelfReferential,
		},
		{
			name:    "IPv6 loopback dst",
			local:   fakeAddr("[fd00::5]:8083"),
			dst:     "[::1]:8000",
			wantErr: ErrSelfReferential,
		},
		{
			name:  "nil local skips the self-address arm",
			local: nil,
			dst:   "10.244.0.5:8000",
		},
		{
			name:  "malformed dst is rejected, but not as self-referential",
			local: fakeAddr("10.244.0.5:8083"),
			dst:   "not-a-host-port",
			// non-nil, but deliberately NOT ErrSelfReferential
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckDst(tc.local, tc.dst)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("CheckDst(%v, %q) = %v, want %v", tc.local, tc.dst, err, tc.wantErr)
				}
			case tc.name == "malformed dst is rejected, but not as self-referential":
				if err == nil {
					t.Fatal("malformed dst must be rejected")
				}
				if errors.Is(err, ErrSelfReferential) {
					t.Error("malformed dst must not be reported as self-referential")
				}
			default:
				if err != nil {
					t.Fatalf("CheckDst(%v, %q) = %v, want nil", tc.local, tc.dst, err)
				}
			}
		})
	}
}

// TestOrigDstFromConn_UnwrapsThroughWrappers locks the reason OrigDstFromConn
// walks a chain instead of doing a single type assertion: under mTLS the
// transparent conn sits TWO layers down (tlssniff's peeked wrapper, then
// tls.Server), so a direct assertion in ConnContext would silently find nothing
// and every request would fail closed with 502.
func TestOrigDstFromConn_UnwrapsThroughWrappers(t *testing.T) {
	base := &Conn{dst: "10.244.0.5:8000"}

	if dst, ok := OrigDstFromConn(base); !ok || dst != "10.244.0.5:8000" {
		t.Fatalf("bare conn: got (%q, %v), want (10.244.0.5:8000, true)", dst, ok)
	}

	// One layer: a peek-style wrapper exposing NetConn, as tlssniff's does.
	one := &wrapConn{inner: base}
	if dst, ok := OrigDstFromConn(one); !ok || dst != "10.244.0.5:8000" {
		t.Fatalf("one wrapper: got (%q, %v), want (10.244.0.5:8000, true)", dst, ok)
	}

	// Two layers: tls.Conn over the peeked wrapper. *tls.Conn.NetConn() is the
	// stdlib method this walk relies on.
	two := tls.Server(one, &tls.Config{})
	if dst, ok := OrigDstFromConn(two); !ok || dst != "10.244.0.5:8000" {
		t.Fatalf("tls over wrapper: got (%q, %v), want (10.244.0.5:8000, true)", dst, ok)
	}
}

// TestOrigDstFromConn_NoTransparentConn ensures a plain connection reports
// absence rather than an empty-string false positive — the reverse proxy keys
// its fail-closed 502 on this.
func TestOrigDstFromConn_NoTransparentConn(t *testing.T) {
	if dst, ok := OrigDstFromConn(&wrapConn{inner: &plainConn{}}); ok {
		t.Fatalf("plain conn chain: got (%q, true), want absent", dst)
	}
}

// TestOrigDstFromConn_CycleTerminates guards the maxUnwrapDepth bound: a
// wrapper that returns itself must not spin forever.
func TestOrigDstFromConn_CycleTerminates(t *testing.T) {
	c := &selfWrap{}
	if _, ok := OrigDstFromConn(c); ok {
		t.Fatal("cyclic wrapper must not report a destination")
	}
}

// TestConnContextHook covers both directions of the context round-trip, since
// http.Server calls the hook and the reverse proxy's Director reads it back.
func TestConnContextHook(t *testing.T) {
	ctx := ConnContextHook(context.Background(), &Conn{dst: "10.244.0.5:9000"})
	dst, ok := OrigDstFromContext(ctx)
	if !ok || dst != "10.244.0.5:9000" {
		t.Fatalf("round-trip: got (%q, %v), want (10.244.0.5:9000, true)", dst, ok)
	}

	// A connection with nothing to contribute must leave ctx untouched, so the
	// handler sees a clean absence.
	plain := ConnContextHook(context.Background(), &plainConn{})
	if _, ok := OrigDstFromContext(plain); ok {
		t.Error("plain conn must not populate the context")
	}
}

// TestOrigDstFromContext_EmptyStringIsAbsent stops an empty destination from
// reading as present, which would send the Director to "127.0.0.1:" .
func TestOrigDstFromContext_EmptyStringIsAbsent(t *testing.T) {
	if _, ok := OrigDstFromContext(ContextWithOrigDst(context.Background(), "")); ok {
		t.Error("empty destination must report absent")
	}
}

// --- test doubles ------------------------------------------------------------

// plainConn is a net.Conn that knows nothing about transparent capture.
type plainConn struct{ net.Conn }

func (c *plainConn) Close() error        { return nil }
func (c *plainConn) LocalAddr() net.Addr { return fakeAddr("10.244.0.5:8083") }

// wrapConn mimics tlssniff's peekedConn: it wraps another conn and exposes it
// via NetConn.
type wrapConn struct {
	net.Conn
	inner net.Conn
}

func (c *wrapConn) NetConn() net.Conn { return c.inner }

// selfWrap returns itself from NetConn, the pathological case maxUnwrapDepth
// exists for.
type selfWrap struct{ net.Conn }

func (c *selfWrap) NetConn() net.Conn { return c }
