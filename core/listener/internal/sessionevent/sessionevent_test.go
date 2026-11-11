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

// withProtocolParses adds the two extensions each listener used to keep for one
// direction only, plus a response the pipeline saw.
func withProtocolParses(pctx *pipeline.Context) *pipeline.Context {
	pctx.Body = []byte(`{"jsonrpc":"2.0"}`)
	pctx.ResponseBytes = 42
	pctx.ResponseHeadersAt = time.Now()
	pctx.Extensions.A2A = &pipeline.A2AExtension{Method: "message/send"}
	pctx.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/call"}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m"}
	return pctx
}

// Request and Response carry every extension in BOTH directions (#1575). Each listener
// used to build its own literal, and the copies kept A2A inbound only and MCP outbound
// only, while the parsers run on either chain.
func TestRequestResponse_CarryEveryProtocolInEitherDirection(t *testing.T) {
	for _, dir := range []pipeline.Direction{pipeline.Inbound, pipeline.Outbound} {
		pctx := withProtocolParses(richContext())
		for _, ev := range []pipeline.SessionEvent{
			Request(pctx, dir),
			Response(pctx, dir, 200, nil),
		} {
			name := dir.String() + "/" + ev.Phase.String()
			if ev.Direction != dir {
				t.Errorf("%s: Direction = %v", name, ev.Direction)
			}
			if ev.A2A == nil || ev.A2A.Method != "message/send" {
				t.Errorf("%s: A2A = %+v, want the parse", name, ev.A2A)
			}
			if ev.MCP == nil || ev.MCP.Method != "tools/call" {
				t.Errorf("%s: MCP = %+v, want the parse", name, ev.MCP)
			}
			if ev.Inference == nil || ev.Inference.Model != "m" {
				t.Errorf("%s: Inference = %+v, want the parse", name, ev.Inference)
			}
			if ev.Identity == nil || ev.Identity.Subject != "alice" {
				t.Errorf("%s: Identity = %+v", name, ev.Identity)
			}
			if _, ok := ev.Plugins["parity-spy-a"]; !ok {
				t.Errorf("%s: Plugins missing the spy's event", name)
			}
			if ev.TLS == nil || ev.Client == nil || ev.Subagent == "" || ev.ParentSubagent == "" {
				t.Errorf("%s: TLS=%v Client=%v Subagent=%q ParentSubagent=%q, want all set",
					name, ev.TLS, ev.Client, ev.Subagent, ev.ParentSubagent)
			}
		}
	}
}

// The extensions are snapshots: a response-phase change to the live struct, such as
// token counts set in OnResponse, must not reach a row that was already built.
func TestRequest_SnapshotsTheExtensions(t *testing.T) {
	pctx := withProtocolParses(richContext())
	ev := Request(pctx, pipeline.Outbound)
	pctx.Extensions.A2A.Method = "changed"
	pctx.Extensions.MCP.Method = "changed"
	pctx.Extensions.Inference.Model = "changed"
	if ev.A2A.Method != "message/send" || ev.MCP.Method != "tools/call" || ev.Inference.Model != "m" {
		t.Errorf("row followed the live extensions: A2A=%q MCP=%q Inference=%q",
			ev.A2A.Method, ev.MCP.Method, ev.Inference.Model)
	}
}

// Each phase carries its own half: the request row the body sent and the request's own
// attribution, the response row the outcome. The other half stays zero.
func TestRequestResponse_EachPhaseCarriesItsOwnHalf(t *testing.T) {
	pctx := withProtocolParses(richContext())
	pctx.SetAttribution("100.200", "101.201", "X-Session")
	pctx.UpgradeStripped = "h2c"

	req := Request(pctx, pipeline.Outbound)
	if req.Phase != pipeline.SessionRequest || req.BytesUp != int64(len(pctx.Body)) {
		t.Errorf("request: Phase=%v BytesUp=%d", req.Phase, req.BytesUp)
	}
	if req.Proc != "100.200" || req.ClientProc != "101.201" || req.SessionHeader != "X-Session" || req.UpgradeStripped != "h2c" {
		t.Errorf("request: Proc=%q ClientProc=%q SessionHeader=%q UpgradeStripped=%q",
			req.Proc, req.ClientProc, req.SessionHeader, req.UpgradeStripped)
	}
	if req.BytesDown != 0 || req.StatusCode != 0 || req.Error != nil || req.Duration != 0 || req.Ttfb != 0 {
		t.Errorf("request carries response fields: %+v", req)
	}

	fail := &pipeline.EventError{Kind: "upstream_timeout"}
	resp := Response(pctx, pipeline.Outbound, 502, fail)
	if resp.Phase != pipeline.SessionResponse || resp.StatusCode != 502 || resp.Error != fail {
		t.Errorf("response: Phase=%v StatusCode=%d Error=%+v", resp.Phase, resp.StatusCode, resp.Error)
	}
	if resp.BytesDown != 42 || resp.Duration <= 0 || resp.Ttfb <= 0 || resp.Proc != "100.200" {
		t.Errorf("response: BytesDown=%d Duration=%v Ttfb=%v Proc=%q", resp.BytesDown, resp.Duration, resp.Ttfb, resp.Proc)
	}
	if resp.BytesUp != 0 || resp.ClientProc != "" || resp.SessionHeader != "" || resp.UpgradeStripped != "" {
		t.Errorf("response carries request-only fields: BytesUp=%d ClientProc=%q SessionHeader=%q UpgradeStripped=%q",
			resp.BytesUp, resp.ClientProc, resp.SessionHeader, resp.UpgradeStripped)
	}
}

// With no explicit failure, Response falls back to what the response implies, as the
// buffered recorders' DeriveError did before they shared this builder.
func TestResponse_DerivesTheErrorWhenNoneIsGiven(t *testing.T) {
	pctx := &pipeline.Context{StatusCode: 503}
	ev := Response(pctx, pipeline.Inbound, 503, nil)
	if ev.Error == nil || ev.Error.Kind != "backend_error" || ev.Error.Code != "503" {
		t.Errorf("Error = %+v, want backend_error 503", ev.Error)
	}
	if ev := Response(&pipeline.Context{StatusCode: 200}, pipeline.Inbound, 200, nil); ev.Error != nil {
		t.Errorf("Error = %+v, want nil for a 200", ev.Error)
	}
}

// A listener that cannot know a field records nothing for it — the contract that lets
// one builder serve all three.
func TestRequestResponse_OmitWhatTheContextDoesNotCarry(t *testing.T) {
	pctx := &pipeline.Context{}
	for _, ev := range []pipeline.SessionEvent{Request(pctx, pipeline.Inbound), Response(pctx, pipeline.Inbound, 200, nil)} {
		if ev.A2A != nil || ev.MCP != nil || ev.Inference != nil || ev.Identity != nil || ev.TLS != nil ||
			ev.Plugins != nil || ev.Invocations != nil || ev.RequestedHost != "" || ev.Proc != "" {
			t.Errorf("%v: populated fields pctx did not carry: %+v", ev.Phase, ev)
		}
	}
}

// TunnelOpen records every field of a tunnel's open row. The forward proxy built this
// row by hand before the builder existed.
func TestTunnelOpen_PopulatesEveryField(t *testing.T) {
	pctx := richContext()
	pctx.SetAttribution("100.200", "101.201", "X-Session")
	pctx.DialedAddr = "203.0.113.7:443"

	ev := TunnelOpen(pctx, pipeline.TunnelPassthroughHost)
	if ev.At.IsZero() || ev.Direction != pipeline.Outbound || ev.Phase != pipeline.SessionRequest {
		t.Errorf("At/Direction/Phase = %v/%v/%v", ev.At, ev.Direction, ev.Phase)
	}
	if ev.RequestID != pctx.RequestID() || ev.Host != pctx.Host || ev.HTTPMethod != "POST" || ev.HTTPPath != "/parity/deny" {
		t.Errorf("RequestID/Host/Method/Path = %q/%q/%q/%q", ev.RequestID, ev.Host, ev.HTTPMethod, ev.HTTPPath)
	}
	if !ev.Tunnel || ev.TunnelReason != pipeline.TunnelPassthroughHost || ev.DialAddr != "203.0.113.7:443" {
		t.Errorf("Tunnel/TunnelReason/DialAddr = %v/%q/%q", ev.Tunnel, ev.TunnelReason, ev.DialAddr)
	}
	if ev.Invocations == nil || ev.Plugins == nil || ev.Identity == nil || ev.Client == nil {
		t.Errorf("Invocations/Plugins/Identity/Client not snapshotted: %+v", ev)
	}
	if ev.Subagent != "a1a2e36cb5946f31c" || ev.ParentSubagent != "a0974b0b8591dc76b" || ev.Proc != "100.200" {
		t.Errorf("Subagent/ParentSubagent/Proc = %q/%q/%q", ev.Subagent, ev.ParentSubagent, ev.Proc)
	}
	// The open row has no outcome, and the request-only fields stay on Request.
	if ev.StatusCode != 0 || ev.Error != nil || ev.Duration != 0 || ev.BytesUp != 0 || ev.BytesDown != 0 ||
		ev.ClientProc != "" || ev.SessionHeader != "" || ev.TLS != nil || ev.RequestedHost != "" {
		t.Errorf("open row carries fields it must not: %+v", ev)
	}
}

// TunnelClose records every field of a tunnel's close row. The WebSocket close row once
// dropped Proc, Subagent and ParentSubagent because it was built by hand.
func TestTunnelClose_PopulatesEveryField(t *testing.T) {
	pctx := richContext()
	pctx.SetAttribution("100.200", "101.201", "X-Session")
	pctx.DialedAddr = "203.0.113.7:443"
	fail := &pipeline.EventError{Kind: "upstream_unreachable"}

	ev := TunnelClose(pctx, pipeline.TunnelWebSocket, 502, fail, 10, 20)
	if ev.At.IsZero() || ev.Direction != pipeline.Outbound || ev.Phase != pipeline.SessionResponse {
		t.Errorf("At/Direction/Phase = %v/%v/%v", ev.At, ev.Direction, ev.Phase)
	}
	if ev.RequestID != pctx.RequestID() || ev.Host != pctx.Host || ev.HTTPMethod != "POST" || ev.HTTPPath != "/parity/deny" {
		t.Errorf("RequestID/Host/Method/Path = %q/%q/%q/%q", ev.RequestID, ev.Host, ev.HTTPMethod, ev.HTTPPath)
	}
	if ev.StatusCode != 502 || ev.Error != fail || ev.Duration <= 0 || ev.BytesUp != 10 || ev.BytesDown != 20 {
		t.Errorf("StatusCode/Error/Duration/BytesUp/BytesDown = %d/%+v/%v/%d/%d",
			ev.StatusCode, ev.Error, ev.Duration, ev.BytesUp, ev.BytesDown)
	}
	if !ev.Tunnel || ev.TunnelReason != pipeline.TunnelWebSocket || ev.Identity == nil || ev.Client == nil {
		t.Errorf("Tunnel/TunnelReason/Identity/Client = %v/%q/%+v/%+v", ev.Tunnel, ev.TunnelReason, ev.Identity, ev.Client)
	}
	if ev.Subagent != "a1a2e36cb5946f31c" || ev.ParentSubagent != "a0974b0b8591dc76b" || ev.Proc != "100.200" {
		t.Errorf("Subagent/ParentSubagent/Proc = %q/%q/%q", ev.Subagent, ev.ParentSubagent, ev.Proc)
	}
	// Nothing runs on a tunnel's response, and the open row carries the dial address.
	if ev.Invocations != nil || ev.Plugins != nil || ev.DialAddr != "" || ev.ClientProc != "" ||
		ev.SessionHeader != "" || ev.TLS != nil || ev.RequestedHost != "" {
		t.Errorf("close row carries fields it must not: %+v", ev)
	}
}

// A Bob subtask's request, response and denied rows all carry its kind, so agentop can
// read the kind from any row of the block.
func TestBuilders_CarryTheSubagentKind(t *testing.T) {
	hdr := http.Header{}
	hdr.Set(pipeline.BobTaskIDHeader, "c1")
	hdr.Set(pipeline.BobParentTaskIDHeader, "r1")
	pctx := &pipeline.Context{
		Headers: hdr,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Tools: []pipeline.InferenceTool{{Name: "end_subtask"}},
		}},
	}
	for name, ev := range map[string]pipeline.SessionEvent{
		"request":  Request(pctx, pipeline.Outbound),
		"response": Response(pctx, pipeline.Outbound, 200, nil),
		"deny":     Deny(pctx, pipeline.DenyStatus(403, "p.denied", "no"), pipeline.Outbound),
	} {
		if ev.SubagentKind != pipeline.SubagentKindSubtask || ev.Subagent != "c1" {
			t.Errorf("%s: subagent %q kind %q, want c1 subtask", name, ev.Subagent, ev.SubagentKind)
		}
	}
}
