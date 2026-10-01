package pipeline

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
				_, err := http.Get("http://" + closedAddr(t))
				return err
			},
		},
		{
			name: "client timeout",
			want: "upstream_timeout",
			do: func(*testing.T) error {
				c := &http.Client{Timeout: 150 * time.Millisecond}
				_, err := c.Get(slow.URL)
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
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, slow.URL, nil)
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				_, err = http.DefaultClient.Do(req)
				return err
			},
		},
		{
			name: "dns failure",
			want: "upstream_dns",
			do: func(*testing.T) error {
				// .invalid is reserved by RFC 2606 and can never resolve, so
				// this does not depend on the resolver the test host happens
				// to have.
				_, err := http.Get("http://no-such-host.1045.invalid")
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
				_, err := http.Get(srv.URL)
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
			// The message is the operator's only route to the address and the
			// cause, so an empty one would make the kind the whole diagnosis.
			if got.Message != err.Error() {
				t.Errorf("Message = %q, want the error verbatim (%q)", got.Message, err.Error())
			}
			// Code is for an HTTP status; a transport failure has none, and a
			// synthetic "502" here would claim the upstream answered.
			if got.Code != "" {
				t.Errorf("Code = %q, want empty", got.Code)
			}
		})
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
}

type errUnclassified struct{}

func (errUnclassified) Error() string { return "something else went wrong" }

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
