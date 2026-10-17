// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rcpex "github.com/contextforge-org/cpex/go/cpex"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/vmihailenco/msgpack/v5"
)

func TestUnchangedPayloadIsNotApplied(t *testing.T) {
	// CPEX returns a final payload on every allowed pipeline. Its
	// presence alone must not trigger a body rewrite or a modify decision.
	pres := &rcpex.PipelineResult{
		ContinueProcessing: true,
		ModifiedPayload:    []byte("not a MessagePack payload"),
	}
	changed, err := applyModificationsToPctx(&pipeline.Context{}, pres, false, false, nil)
	if err != nil {
		t.Fatalf("unchanged payload was decoded: %v", err)
	}
	if got := mapResult(pres, changed).Decision; got != DecisionAllow {
		t.Fatalf("decision = %v, want allow", got)
	}
}

func TestModifiedPayloadRequiresBytes(t *testing.T) {
	pres := &rcpex.PipelineResult{
		ContinueProcessing: true,
		PayloadModified:    true,
	}
	if _, err := applyModificationsToPctx(&pipeline.Context{}, pres, false, false, nil); err == nil || !strings.Contains(err.Error(), "no payload") {
		t.Fatalf("missing modified payload error = %v", err)
	}
}

// A plugin panic inside CPEX comes back as an ordinary denied result with
// violation code plugin_panic, not as an FFI error.
func TestPluginPanicIsAPlainDeny(t *testing.T) {
	res := mapResult(&rcpex.PipelineResult{
		Violation: &rcpex.PluginViolation{Code: "plugin_panic", Reason: "plugin panicked", PluginName: "boom"},
	}, false)
	if res.Decision != DecisionDeny || res.Code != "plugin_panic" {
		t.Fatalf("result = %+v, want deny plugin_panic", res)
	}
	p := &CPEX{}
	action, stop := p.applyDecision(&pipeline.Context{}, res)
	if !stop || action.Type != pipeline.Reject || action.Violation.Code != "cpex.plugin_panic" {
		t.Fatalf("action = %+v, want reject cpex.plugin_panic", action)
	}
	if action.Violation.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", action.Violation.Status)
	}
}

// newEngineManager returns a real CPEX-backed manager. Tests that reach a
// loopback IdP pass rcpex.WithPrivateDestinations().
func newEngineManager(t *testing.T, opts ...rcpex.APLOption) Manager {
	t.Helper()
	eng, err := rcpex.NewPolicyEngineDefault()
	if err != nil {
		t.Fatalf("NewPolicyEngineDefault: %v", err)
	}
	if err := eng.EnableAPL(opts...); err != nil {
		eng.Shutdown()
		t.Fatalf("EnableAPL: %v", err)
	}
	return &cpexManager{mgr: eng}
}

// enginePlugin configures and initializes a cpex plugin over mgr with
// policy as its CPEX config, firing cmf.tool_pre_invoke on requests.
func enginePlugin(t *testing.T, mgr Manager, policy string) *CPEX {
	t.Helper()
	return enginePluginHook(t, mgr, policy, HookToolPreInvoke)
}

// enginePluginHook is enginePlugin firing hook on requests.
func enginePluginHook(t *testing.T, mgr Manager, policy, hook string) *CPEX {
	t.Helper()
	p := NewCPEX()
	p.newManager = func(ManagerOptions) (Manager, error) { return mgr, nil }
	raw, err := json.Marshal(map[string]any{
		"hooks":  map[string]any{"on_request": []string{hook}},
		"config": policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func toolCall(name string, headers http.Header) *pipeline.Context {
	if headers == nil {
		headers = http.Header{}
	}
	return &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "tools.example",
		Headers:   headers,
		Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{
			Method: "tools/call",
			Params: map[string]any{"name": name, "arguments": map[string]any{"city": "Paris"}},
		}},
	}
}

type claimsIdentity struct {
	sub    string
	scopes []string
	claims map[string]string
}

func (c claimsIdentity) Subject() string           { return c.sub }
func (c claimsIdentity) ClientID() string          { return "" }
func (c claimsIdentity) Scopes() []string          { return c.scopes }
func (c claimsIdentity) Issuer() string            { return c.claims["issuer"] }
func (c claimsIdentity) Audience() []string        { return nil }
func (c claimsIdentity) AuthMethod() string        { return "jwt" }
func (c claimsIdentity) Claims() map[string]string { return c.claims }

func wantContinue(t *testing.T, label string, a pipeline.Action) {
	t.Helper()
	if a.Type != pipeline.Continue {
		t.Fatalf("%s: action = %+v, want continue", label, a.Violation)
	}
}

func wantReject(t *testing.T, label string, a pipeline.Action, code string) {
	t.Helper()
	if a.Type != pipeline.Reject || a.Violation == nil || a.Violation.Code != code {
		t.Fatalf("%s: action = %v %+v, want reject %s", label, a.Type, a.Violation, code)
	}
}

func TestEngineAllowsToolCall(t *testing.T) {
	p := enginePlugin(t, newEngineManager(t), `
routes:
  - tool: get_weather
    authorization:
      pre_invocation: ["allow"]
`)
	wantContinue(t, "get_weather", p.OnRequest(context.Background(), toolCall("get_weather", nil)))
}

func TestEngineSeesSubjectRolesAndClaims(t *testing.T) {
	p := enginePlugin(t, newEngineManager(t), `
routes:
  - tool: get_compensation
    authorization:
      pre_invocation:
        - "require(role.hr)"
        - "require(claim.issuer == 'https://idp.example')"
`)
	claims := map[string]string{"issuer": "https://idp.example"}

	hr := toolCall("get_compensation", nil)
	hr.Identity = claimsIdentity{sub: "alice", scopes: []string{"hr"}, claims: claims}
	wantContinue(t, "hr", p.OnRequest(context.Background(), hr))

	eng := toolCall("get_compensation", nil)
	eng.Identity = claimsIdentity{sub: "evan", scopes: []string{"engineer"}, claims: claims}
	if a := p.OnRequest(context.Background(), eng); a.Type != pipeline.Reject {
		t.Fatalf("engineer: action = %v, want reject", a.Type)
	}

	other := toolCall("get_compensation", nil)
	other.Identity = claimsIdentity{sub: "mallory", scopes: []string{"hr"}, claims: map[string]string{"issuer": "https://other"}}
	if a := p.OnRequest(context.Background(), other); a.Type != pipeline.Reject {
		t.Fatalf("foreign issuer: action = %v, want reject", a.Type)
	}
}

// The agent's own workload identity travels in security.this_workload.
// APL route steps cannot read it (read_workload is not in their baseline
// capabilities), so this checks the wire shape the engine receives.
func TestBuildCMFThisWorkload(t *testing.T) {
	pctx := toolCall("get_weather", nil)
	pctx.Agent = &pipeline.AgentIdentity{
		ClientID:    "agent-client",
		WorkloadID:  "spiffe://example.org/ns/team1/sa/agent",
		TrustDomain: "example.org",
	}
	_, ext := buildCMF(pctx, false, ManagerOptions{})
	raw, err := msgpack.Marshal(ext)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Security struct {
			ThisWorkload map[string]any `msgpack:"this_workload"`
		} `msgpack:"security"`
	}
	if err := msgpack.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"spiffe_id":    "spiffe://example.org/ns/team1/sa/agent",
		"trust_domain": "example.org",
		"client_id":    "agent-client",
	}
	for k, v := range want {
		if wire.Security.ThisWorkload[k] != v {
			t.Errorf("this_workload.%s = %v, want %v", k, wire.Security.ThisWorkload[k], v)
		}
	}
}

// A delegate() step that is granted on a manager that does not forward
// delegated credentials denies the request rather than forward it with the
// caller's own bearer.
func TestDelegationWithoutHostCredentialDenies(t *testing.T) {
	secret := []byte("cortex-test-only-hs256-secret-0123456789")
	var exchanges atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      "delegated-token",
			"token_type":        "Bearer",
			"expires_in":        300,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	}))
	t.Cleanup(idp.Close)

	b64 := base64.RawURLEncoding.EncodeToString
	jwk, _ := json.Marshal(map[string]string{"kty": "oct", "alg": "HS256", "k": b64(secret)})
	p := enginePlugin(t, newEngineManager(t, rcpex.WithPrivateDestinations()), `
plugins:
  - name: idp
    kind: identity/jwt
    hooks: [identity.resolve]
    config:
      claim_mapper: standard
      trusted_issuers:
        - issuer: https://idp.example
          audiences: [cortex-test]
          algorithms: [HS256]
          decoding_key: {kind: jwk, jwk: `+string(jwk)+`}
  - name: oauth
    kind: delegator/oauth
    hooks: [token.delegate]
    capabilities: [read_inbound_credentials, write_delegated_tokens, perform_http]
    config:
      token_endpoint: "`+idp.URL+`/token"
      client_id: cortex
      client_secret_source: {kind: literal, secret: test-only}
      insecure_http: true
routes:
  - tool: get_compensation
    authentication: [idp]
    authorization:
      pre_invocation:
        - "delegate(oauth, target: workday-api, audience: workday-api)"
        - "require(delegation.granted)"
  - tool: get_weather
    authentication: [idp]
    authorization:
      pre_invocation: ["require(authenticated)"]
`)

	now := time.Now().Unix()
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	body, _ := json.Marshal(map[string]any{
		"iss": "https://idp.example", "aud": "cortex-test", "sub": "alice",
		"iat": now - 5, "exp": now + 600, "roles": []string{"hr"},
	})
	signing := b64(hdr) + "." + b64(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signing))
	bearer := "Bearer " + signing + "." + b64(mac.Sum(nil))

	wantContinue(t, "no delegation", p.OnRequest(context.Background(),
		toolCall("get_weather", http.Header{"Authorization": {bearer}})))

	pctx := toolCall("get_compensation", http.Header{"Authorization": {bearer}})
	wantReject(t, "delegated", p.OnRequest(context.Background(), pctx), "cpex.delegation_unavailable")
	if exchanges.Load() != 1 {
		t.Fatalf("token exchanges = %d, want 1 (the delegate step must have run)", exchanges.Load())
	}
}

// A request presented as http_egress has no body re-serializer, even when
// a parser claimed it: changed arguments are an error, never dropped.
func TestEgressPayloadModificationFailsClosed(t *testing.T) {
	pctx := egressRequest("POST", "evil.example", "/mcp", `{"method":"tools/list","params":{"cursor":"s3cr3t"}}`, nil)
	pctx.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/list", Params: map[string]any{"cursor": "s3cr3t"}}
	msg := func(args map[string]any) *rcpex.Message {
		m := rcpex.NewMessage("user", rcpex.NewToolCallPart(rcpex.ToolCall{Name: egressToolName, Arguments: args}))
		return &m
	}
	if err := checkEgressUnmodified(pctx, msg(egressPart(pctx).Arguments)); err != nil {
		t.Fatalf("unchanged arguments: %v", err)
	}
	redacted := egressPart(pctx).Arguments
	redacted["body"] = "[REDACTED]"
	if err := checkEgressUnmodified(pctx, msg(redacted)); err == nil {
		t.Fatal("changed arguments accepted")
	}
	if err := checkEgressUnmodified(pctx, msg(nil)); err == nil {
		t.Fatal("stripped arguments accepted")
	}
}
