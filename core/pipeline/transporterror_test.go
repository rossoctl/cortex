package pipeline

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// secretQuery rides on every request below, because the forward proxy's errors
// quote the request URL — query string and all — and a query is where clients
// put API keys (Gemini's ?key=). It must never reach Message.
const secretQuery = "?api_key=SECRET1045"

// TestTransportError drives each branch with an error from a REAL failed
// request rather than a hand-built one. That is the whole point of the test:
// the classification is built out of errors.Is / errors.As against types the
// error is wrapped in (*url.Error over a *net.OpError over a syscall errno),
// and a fabricated error reproduces the type the test author expected rather
// than the one net/http actually returns. A hand-built fixture would have
// passed just as happily against a classifier that never fires in production.
func TestTransportError(t *testing.T) {
	// One hanging server shared by both timeout cases. It blocks until the test
	// ends rather than sleeping a fixed span: a sleep has to be comfortably
	// longer than the 150ms budgets below to avoid flaking, and httptest.Close
	// waits for outstanding handlers, so that duration is then added to every
	// run. Releasing on a channel keeps the handler outstanding for exactly as
	// long as it is needed and makes Close immediate.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer slow.Close()
	defer close(release)

	tests := []struct {
		name string
		want string
		do   func(*testing.T) error
	}{
		{
			name: "connection refused",
			want: "upstream_refused",
			do: func(t *testing.T) error {
				_, err := hermeticClient(nil).Get("http://" + closedAddr(t) + "/" + secretQuery)
				return err
			},
		},
		{
			name: "client timeout",
			want: "upstream_timeout",
			do: func(*testing.T) error {
				c := hermeticClient(nil)
				c.Timeout = 150 * time.Millisecond
				_, err := c.Get(slow.URL + "/" + secretQuery)
				return err
			},
		},
		{
			// Distinct from the case above — a deadline on the caller's context
			// rather than on the client — and it must land in the same bucket.
			name: "context deadline",
			want: "upstream_timeout",
			do: func(t *testing.T) error {
				ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, slow.URL+"/"+secretQuery, nil)
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				_, err = hermeticClient(nil).Do(req)
				return err
			},
		},
		{
			// A deadline UNDER a wrapping layer, shaped exactly as forwardproxy's
			// mtlsDialer returns a handshake that ran out of time. *url.Error's
			// Timeout() only asks its immediate cause, and that is the %w wrapper,
			// so this read as upstream_error until the classifier asked for the
			// deadline itself.
			name: "wrapped handshake deadline",
			want: "upstream_timeout",
			do: func(t *testing.T) error {
				addr := silentAddr(t)
				tr := hermeticTransport()
				tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					plain, err := (&net.Dialer{}).DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					hsCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
					defer cancel()
					tlsConn := tls.Client(plain, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // never completes
					if err := tlsConn.HandshakeContext(hsCtx); err != nil {
						_ = tlsConn.Close()
						return nil, fmt.Errorf("forwardproxy mtls: handshake to %s failed: %w", addr, err)
					}
					return tlsConn, nil
				}
				_, err := (&http.Client{Transport: tr}).Get("http://" + addr + "/" + secretQuery)
				return err
			},
		},
		{
			name: "dns failure",
			want: "upstream_dns",
			do: func(t *testing.T) error {
				// Resolved against a stub that answers NXDOMAIN to everything, so
				// the case depends neither on the host's resolver answering
				// promptly nor on .invalid staying unresolvable there.
				_, err := hermeticClient(nxdomainResolver(t)).Get("http://no-such-host.1045.invalid/" + secretQuery)
				return err
			},
		},
		{
			name: "untrusted certificate",
			want: "upstream_tls",
			do: func(*testing.T) error {
				// NewTLSServer's cert is signed by its own ephemeral CA, which
				// the default client does not trust.
				srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				defer srv.Close()
				_, err := hermeticClient(nil).Get(srv.URL + "/" + secretQuery)
				return err
			},
		},
		{
			// The peer refusing the handshake with an alert. Over TCP that is a
			// *net.OpError with Op "remote error", not tls.AlertError — that type
			// is QUIC's, and matching it caught nothing here.
			name: "peer alert",
			want: "upstream_tls",
			do: func(*testing.T) error {
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				srv.TLS = &tls.Config{MaxVersion: tls.VersionTLS12}
				srv.StartTLS()
				defer srv.Close()
				tr := hermeticTransport()
				tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true} //nolint:gosec // the version is under test
				_, err := (&http.Client{Transport: tr}).Get(srv.URL + "/" + secretQuery)
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.do(t)
			if err == nil {
				t.Fatal("request succeeded; wanted a transport failure")
			}
			got := TransportError(err)
			if got == nil {
				t.Fatalf("TransportError(%v) = nil, want an EventError", err)
			}
			if got.Kind != tc.want {
				t.Errorf("Kind = %q, want %q (err: %v)", got.Kind, tc.want, err)
			}
			assertCause(t, got.Message, err)
			// Code is for an HTTP status; a transport failure has none, and a
			// synthetic "502" here would claim the upstream answered.
			if got.Code != "" {
				t.Errorf("Code = %q, want empty", got.Code)
			}
		})
	}
}

// assertCause pins what Message may and may not carry. It is the operator's
// only route to the cause, so it must hold it; and it must not hold the request
// URL a *url.Error quotes, because that URL keeps its query string and the
// session API serving this message is unauthenticated.
func assertCause(t *testing.T, msg string, err error) {
	t.Helper()
	if strings.Contains(msg, "SECRET1045") {
		t.Errorf("Message = %q carries the request's query string", msg)
	}
	if msg == "" || !strings.HasSuffix(err.Error(), msg) {
		t.Errorf("Message = %q, want the cause at the tail of %q", msg, err.Error())
	}
}

// TestExchangeFailure_ClientCanceled is the case that made ExchangeFailure
// necessary. Neither proxy times out a slow upstream, so a hung one ends when
// the CLIENT gives up — and the error the proxy holds then is a cancellation,
// not a failure of the upstream. It must not be recorded as a 502 nobody got.
func TestExchangeFailure_ClientCanceled(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer slow.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slow.URL+"/"+secretQuery, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	_, err = hermeticClient(nil).Do(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation", err)
	}

	status, fail := ExchangeFailure(ctx, err)
	if status != StatusClientClosedRequest {
		t.Errorf("status = %d, want %d — a 502 here counts a response nobody received", status, StatusClientClosedRequest)
	}
	if fail == nil || fail.Kind != "client_canceled" {
		t.Fatalf("fail = %+v, want kind client_canceled", fail)
	}
	assertCause(t, fail.Message, err)

	// The same error with the client still there is the upstream's failure, and
	// the client got a 502 for it.
	status, fail = ExchangeFailure(context.Background(), err)
	if status != http.StatusBadGateway || fail == nil || fail.Kind != "upstream_error" {
		t.Errorf("live client: got %d %+v, want 502 upstream_error", status, fail)
	}
}

// TestTransportError_Nil pins that a nil error yields no event error, so a
// caller can hand its err through unconditionally.
func TestTransportError_Nil(t *testing.T) {
	if got := TransportError(nil); got != nil {
		t.Errorf("TransportError(nil) = %+v, want nil", got)
	}
}

// TestTransportError_UnclassifiedStillReports pins the fallback: an error none
// of the typed checks match is still reported, with its message intact. The
// alternative — returning nil — would put us back where issue #1045 started,
// with a failure that exists only on the wire.
func TestTransportError_UnclassifiedStillReports(t *testing.T) {
	got := TransportError(errUnclassified{})
	if got == nil {
		t.Fatal("TransportError = nil, want an EventError for an unclassified failure")
	}
	if got.Kind != "upstream_error" {
		t.Errorf("Kind = %q, want %q", got.Kind, "upstream_error")
	}
	if got.Message != (errUnclassified{}).Error() {
		t.Errorf("Message = %q, want the error verbatim", got.Message)
	}
}

type errUnclassified struct{}

func (errUnclassified) Error() string { return "something else went wrong" }

// hermeticTransport is a transport that ignores HTTP_PROXY / HTTPS_PROXY. The
// default one honours them, and this repo's own laptop setup exports one — under
// it the refused case came back as a proxyconnect error and the DNS case as the
// proxy's answer.
func hermeticTransport() *http.Transport {
	return &http.Transport{Proxy: nil}
}

// hermeticClient is a client over hermeticTransport, resolving through r when
// it is non-nil.
func hermeticClient(r *net.Resolver) *http.Client {
	tr := hermeticTransport()
	if r != nil {
		tr.DialContext = (&net.Dialer{Resolver: r}).DialContext
	}
	return &http.Client{Transport: tr}
}

// nxdomainResolver is a pure-Go resolver whose only nameserver is a stub that
// answers every query NXDOMAIN, so a lookup fails the way an unknown name does,
// through the real resolver code, without consulting the host's.
func nxdomainResolver(t *testing.T) *net.Resolver {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			// The query echoed back as its own answer: same ID and question, the
			// QR bit set, RCODE 3 (NXDOMAIN).
			msg := append([]byte(nil), buf[:n]...)
			msg[2] |= 0x80
			msg[3] = msg[3]&0xF0 | 0x03
			_, _ = pc.WriteTo(msg, from)
		}
	}()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
		},
	}
}

// silentAddr returns an address that accepts TCP connections and never writes
// a byte, so a TLS handshake against it can only run out of time.
func silentAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// closedAddr returns an address nothing is listening on: bind to an ephemeral
// port, note it, then release it. Mirrors the helper of the same name in
// listener/forwardproxy. A hardcoded port could collide with a real service on
// a developer's machine and turn a refusal into a confusing success.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}
