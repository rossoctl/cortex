package sessionevent

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// stubIdentity is a pipeline.Identity with fixed answers.
type stubIdentity struct {
	subject  string
	clientID string
	scopes   []string
}

func (s stubIdentity) Subject() string  { return s.subject }
func (s stubIdentity) ClientID() string { return s.clientID }
func (s stubIdentity) Scopes() []string { return s.scopes }

// richContext is a pctx carrying every input Deny reads, so a field that
// stops being populated shows up as a zero rather than as an equal-to-equal
// comparison between two listeners that both dropped it.
func richContext() *pipeline.Context {
	spiffeURI, _ := url.Parse("spiffe://example.org/ns/team1/sa/weather-agent")
	hdr := http.Header{}
	hdr.Set("User-Agent", "claude-cli/1.2.3")
	hdr.Set(pipeline.ClaudeCodeAgentIDHeader, "a1a2e36cb5946f31c")
	hdr.Set(pipeline.ClaudeCodeParentAgentIDHeader, "a0974b0b8591dc76b")
	return &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    "POST",
		Path:      "/parity/deny",
		Host:      "weather-agent.team1.svc.cluster.local",
		Headers:   hdr,
		StartedAt: time.Now().Add(-10 * time.Millisecond),
		Identity: stubIdentity{
			subject:  "alice",
			clientID: "weather-agent",
			scopes:   []string{"openid", "weather-aud"},
		},
		TLS: &tls.ConnectionState{
			Version:          tls.VersionTLS13,
			CipherSuite:      tls.TLS_AES_128_GCM_SHA256,
			PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{spiffeURI}}},
		},
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{{
					Plugin: "parity-spy-a",
					Phase:  pipeline.InvocationPhaseRequest,
					Action: pipeline.ActionDeny,
					Reason: "spy.denied",
				}},
			},
			Custom: map[string]any{
				"parity-spy-a" + pipeline.PluginEventSuffix: map[string]string{"marker": "denied"},
			},
		},
	}
}

// TestDeny_PopulatesEveryFieldTheContextCarries is the assertion the four
// recorders could not make for themselves. Each had drifted to its own
// subset — Plugins, Identity and Duration on extproc; TLS and none of those
// three on the reverse proxy; none of the four on the forward proxy — and
// because the fields were simply absent rather than wrong, every listener's
// own tests passed. This pins the whole set in one place.
func TestDeny_PopulatesEveryFieldTheContextCarries(t *testing.T) {
	pctx := richContext()
	action := pipeline.DenyStatus(429, "spy.denied", "quota exhausted")

	ev := Deny(pctx, action, pipeline.Inbound)

	if ev.Phase != pipeline.SessionDenied {
		t.Errorf("Phase = %v, want SessionDenied", ev.Phase)
	}
	if ev.Direction != pipeline.Inbound {
		t.Errorf("Direction = %v, want Inbound", ev.Direction)
	}
	if ev.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", ev.StatusCode)
	}
	if ev.Host != "weather-agent.team1.svc.cluster.local" {
		t.Errorf("Host = %q", ev.Host)
	}
	if ev.HTTPMethod != "POST" || ev.HTTPPath != "/parity/deny" {
		t.Errorf("HTTPMethod/HTTPPath = %q/%q, want POST//parity/deny", ev.HTTPMethod, ev.HTTPPath)
	}
	if ev.Error == nil || ev.Error.Kind != "policy" || ev.Error.Code != "spy.denied" || ev.Error.Message != "quota exhausted" {
		t.Errorf("Error = %+v, want kind=policy code=spy.denied message=quota exhausted", ev.Error)
	}

	// The four fields #936 is about.
	if ev.Identity == nil {
		t.Error("Identity not snapshotted — the field the reverse and forward proxies dropped")
	} else {
		if ev.Identity.Subject != "alice" || ev.Identity.ClientID != "weather-agent" {
			t.Errorf("Identity = %+v, want subject=alice clientId=weather-agent", ev.Identity)
		}
		if len(ev.Identity.Scopes) != 2 {
			t.Errorf("Identity.Scopes = %v, want 2 scopes", ev.Identity.Scopes)
		}
	}
	if _, ok := ev.Plugins["parity-spy-a"]; !ok {
		t.Errorf("Plugins missing the spy's event, got keys %v", keysOf(ev.Plugins))
	}
	if ev.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0 — a denial ends the request, so it has one", ev.Duration)
	}
	if ev.TLS == nil {
		t.Error("TLS not snapshotted — the field extproc and the forward proxy dropped")
	} else {
		if ev.TLS.Version != "TLS 1.3" {
			t.Errorf("TLS.Version = %q, want TLS 1.3", ev.TLS.Version)
		}
		if ev.TLS.PeerSPIFFEID != "spiffe://example.org/ns/team1/sa/weather-agent" {
			t.Errorf("TLS.PeerSPIFFEID = %q", ev.TLS.PeerSPIFFEID)
		}
	}

	// Carried over from the original bodies, so the refactor is not a
	// silent trade of new fields for old ones.
	if ev.Invocations == nil || len(ev.Invocations.Inbound) != 1 {
		t.Fatalf("Invocations = %+v, want 1 inbound record", ev.Invocations)
	}
	if ev.Invocations.Inbound[0].Action != pipeline.ActionDeny {
		t.Errorf("Invocations.Inbound[0].Action = %v, want deny", ev.Invocations.Inbound[0].Action)
	}
	if ev.Client == nil || ev.Client.Name == "" {
		t.Errorf("Client = %+v, want the User-Agent resolved", ev.Client)
	}
	if ev.Subagent != "a1a2e36cb5946f31c" {
		t.Errorf("Subagent = %q, want a1a2e36cb5946f31c from X-Claude-Code-Agent-Id", ev.Subagent)
	}
	if ev.ParentSubagent != "a0974b0b8591dc76b" {
		t.Errorf("ParentSubagent = %q, want a0974b0b8591dc76b from X-Claude-Code-Parent-Agent-Id", ev.ParentSubagent)
	}
	if ev.At.IsZero() {
		t.Error("At is zero")
	}
}

// TestDeny_OmitsWhatTheContextDoesNotCarry is the other half of the
// contract: a listener that cannot know something must record nothing for
// it, not a zero value that a consumer reads as real. extproc is the live
// case — Envoy terminates the client's TLS, so pctx.TLS is never set there
// and the event must carry no handshake rather than an empty one.
func TestDeny_OmitsWhatTheContextDoesNotCarry(t *testing.T) {
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{{Plugin: "p", Phase: pipeline.InvocationPhaseRequest}},
			},
		},
	}

	ev := Deny(pctx, pipeline.DenyStatus(403, "p.denied", "no"), pipeline.Outbound)

	if ev.Identity != nil {
		t.Errorf("Identity = %+v, want nil with no identity on pctx", ev.Identity)
	}
	if ev.TLS != nil {
		t.Errorf("TLS = %+v, want nil for a plaintext request", ev.TLS)
	}
	if ev.Plugins != nil {
		t.Errorf("Plugins = %v, want nil with no plugin events", ev.Plugins)
	}
	if ev.Duration != 0 {
		t.Errorf("Duration = %v, want 0 with a zero StartedAt", ev.Duration)
	}
	if ev.Direction != pipeline.Outbound {
		t.Errorf("Direction = %v, want the dir the caller passed", ev.Direction)
	}
}

// TestDeny_DerivesStatusFromCode covers the one piece of logic in here that
// is not a copy: a plugin that set no HTTP status on its Violation gets one
// mapped from the semantic code, as the wire path does via Render().
func TestDeny_DerivesStatusFromCode(t *testing.T) {
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{Inbound: []pipeline.Invocation{{Plugin: "p"}}},
		},
	}
	action := pipeline.Action{Type: pipeline.Reject, Violation: &pipeline.Violation{
		Code:   "auth.unauthorized",
		Reason: "token validation failed",
	}}

	ev := Deny(pctx, action, pipeline.Inbound)

	if want := pipeline.StatusFromCode("auth.unauthorized"); ev.StatusCode != want {
		t.Errorf("StatusCode = %d, want %d from StatusFromCode", ev.StatusCode, want)
	}
}

// TestDeny_NoViolation records the event anyway rather than panicking. A
// Reject with no Violation is a plugin bug, but the denial still happened
// and an event with no code beats no event at all.
func TestDeny_NoViolation(t *testing.T) {
	pctx := &pipeline.Context{
		Extensions: pipeline.Extensions{
			Invocations: &pipeline.Invocations{Inbound: []pipeline.Invocation{{Plugin: "p"}}},
		},
	}

	ev := Deny(pctx, pipeline.Action{Type: pipeline.Reject}, pipeline.Inbound)

	if ev.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 when there is no violation to read one from", ev.StatusCode)
	}
	if ev.Error == nil || ev.Error.Kind != "policy" {
		t.Errorf("Error = %+v, want a policy error with empty code", ev.Error)
	}
}

func TestTLS_NilCases(t *testing.T) {
	if got := TLS(nil); got != nil {
		t.Errorf("TLS(nil) = %+v, want nil", got)
	}
	if got := TLS(&pipeline.Context{}); got != nil {
		t.Errorf("TLS(plaintext pctx) = %+v, want nil", got)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
