package forwardproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// MOCA MI1 §8.5 item 5: on a decrypted request, Host must equal the CONNECT host,
// else 421 and an audit event.
//
// Why this is worth enforcing even though the request is always dialed to the
// CONNECT host regardless: bridgedHandler sets r.URL.Host to the authority, so the
// bytes reach the right origin whatever Host says. What a mismatched Host reaches is
// the PIPELINE — pctx.Host is what a plugin reads to choose a credential or a route.
// A guest that CONNECTs to an allowlisted host and then sends `Host: elsewhere`
// inside the tunnel shows one name to the policy layer and another to the socket.

// mismatchServer builds a Server whose bridged handler can be driven directly,
// without forging TLS: bridgedHandler is an ordinary http.Handler over an
// authority, which is exactly the unit under test.
func mismatchServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	p, err := pipeline.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
		Client:           http.DefaultClient,
	}, store
}

func TestBridgedHandler_HostMismatch_Is421AndRecorded(t *testing.T) {
	s, store := mismatchServer(t)

	req := httptest.NewRequest("GET", "https://evil.example/steal", nil)
	req.Host = "evil.example" // the guest's claim, inside a tunnel to api.example
	rec := httptest.NewRecorder()

	s.bridgedHandler("api.example:443", discardTunnel()).ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want 421", rec.Code)
	}

	// The event must name BOTH hosts: either one alone looks unremarkable, and the
	// disagreement is the entire diagnostic.
	var found *pipeline.SessionEvent
	for _, sid := range []string{session.DefaultSessionID} {
		if v := store.View(sid); v != nil {
			for i := range v.Events {
				if v.Events[i].Error != nil && v.Events[i].Error.Kind == "host_mismatch" {
					found = &v.Events[i]
				}
			}
		}
	}
	if found == nil {
		t.Fatal("no host_mismatch event recorded — the refusal must be auditable")
	}
	if found.Phase != pipeline.SessionDenied {
		t.Errorf("phase = %v, want denied", found.Phase)
	}
	if found.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("event statusCode = %d, want 421", found.StatusCode)
	}
	if found.Host != "api.example" {
		t.Errorf("event host = %q, want the CONNECT host api.example", found.Host)
	}
	if msg := found.Error.Message; msg == "" {
		t.Error("error message empty — it must name the host the guest claimed")
	}
}

// The overwhelmingly common case: a matching Host passes through untouched. Without
// this, a check that refused everything would pass the test above.
func TestBridgedHandler_HostMatches_PassesThrough(t *testing.T) {
	var reached bool
	// TLS, because bridgedHandler sets the scheme to https — a decrypted request on a
	// bridged tunnel is always one the client sent over TLS.
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	s, store := mismatchServer(t)
	s.Client = backend.Client() // trusts the test server's forged CA
	// Point the bridged request at the test backend rather than a real origin.
	authority := backend.Listener.Addr().String()

	req := httptest.NewRequest("GET", "https://"+authority+"/ok", nil)
	req.Host = authority
	rec := httptest.NewRecorder()

	s.bridgedHandler(authority, discardTunnel()).ServeHTTP(rec, req)

	if rec.Code == http.StatusMisdirectedRequest {
		t.Fatal("a matching Host was refused with 421")
	}
	if !reached {
		t.Error("request never reached the origin")
	}
	if v := store.View(session.DefaultSessionID); v != nil {
		for _, e := range v.Events {
			if e.Error != nil && e.Error.Kind == "host_mismatch" {
				t.Error("a matching Host recorded a host_mismatch event")
			}
		}
	}
}

// A client may legitimately omit a default :443 from Host while the authority carries
// it. Comparing authority-to-Host verbatim would refuse these, so the comparison is
// host-only — and a port disagreement cannot reach a different origin anyway, because
// the dial uses the authority and not Host.
func TestBridgedHandler_HostWithoutDefaultPort_Matches(t *testing.T) {
	s, _ := mismatchServer(t)

	for _, tc := range []struct{ name, authority, host string }{
		{"host omits default port", "api.example:443", "api.example"},
		{"host carries the port", "api.example:443", "api.example:443"},
		{"non-443 origin, port omitted", "api.example:8443", "api.example"},
		{"case differs", "API.example:443", "api.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "https://"+tc.host+"/x", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()

			// Dial will fail (nothing is listening) — that is fine and not what is
			// asserted. All that matters is that it was not refused as a mismatch.
			s.bridgedHandler(tc.authority, discardTunnel()).ServeHTTP(rec, req)

			if rec.Code == http.StatusMisdirectedRequest {
				t.Errorf("Host %q against CONNECT %q was refused 421; ports must not be compared",
					tc.host, tc.authority)
			}
		})
	}
}

// An empty Host must be refused rather than treated as matching. net/http moves
// HTTP/1.1's Host header to r.Host and fills r.Host from :authority on HTTP/2, so
// reading the Host *header* directly would see "" on both and pass everything — the
// mistake this pins against.
func TestBridgedHandler_EmptyHost_IsRefused(t *testing.T) {
	s, _ := mismatchServer(t)

	req := httptest.NewRequest("GET", "https://api.example/x", nil)
	req.Host = ""
	rec := httptest.NewRecorder()

	s.bridgedHandler("api.example:443", discardTunnel()).ServeHTTP(rec, req)

	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want 421 — an absent Host names no host and cannot match", rec.Code)
	}
}
