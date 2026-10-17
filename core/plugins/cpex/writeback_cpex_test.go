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
	"testing"
	"time"

	rcpex "github.com/contextforge-org/cpex/go/cpex"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// CPEX 0.3 hands back the full request header map on every allowed
// invocation, not only when a policy changed it. The write-back is a diff
// for that reason.
func TestEngineReturnsHeadersOnEveryInvocation(t *testing.T) {
	m := newEngineManager(t).(*cpexManager)
	if err := m.LoadConfig(`
routes:
  - tool: get_weather
    authorization:
      pre_invocation: ["allow"]
`); err != nil {
		t.Fatal(err)
	}
	if err := m.mgr.Initialize(); err != nil {
		t.Fatal(err)
	}
	payload, ext := buildCMF(toolCall("get_weather", http.Header{"X-Foo": {"bar"}}), false, ManagerOptions{})
	pres, ct, _, err := m.mgr.InvokeByName(HookToolPreInvoke, rcpex.PayloadCMFMessage, payload, ext, nil)
	if ct != nil {
		ct.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := pres.DeserializeExtensions()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Http == nil || got.Http.RequestHeaders["X-Foo"] != "bar" {
		t.Fatalf("modified extensions = %+v, want the request headers back", got)
	}
}

func assertionPolicy() string {
	jwk, _ := json.Marshal(map[string]string{"kty": "oct", "alg": "HS256", "k": base64.RawURLEncoding.EncodeToString(spikeSecret)})
	return `
plugins:
  - name: idp
    kind: identity/jwt
    hooks: [identity.resolve]
    config:
      claim_mapper: standard
      trusted_issuers:
        - issuer: ` + spikeIssuer + `
          audiences: [` + spikeAudience + `]
          algorithms: [HS256]
          decoding_key: {kind: jwk, jwk: ` + string(jwk) + `}
global:
  assertions:
    request:
      headers:
        - name: x-auth-user-id
          from: subject.id
      strip: [authorization]
routes:
  - tool: get_weather
    authentication: [idp]
    authorization:
      pre_invocation: ["require(authenticated)"]
`
}

// Assertions strip the caller's bearer and replace a spoofed identity
// header with the engine's; untouched headers survive as they were.
func TestEngineAssertionsRewriteForwardedHeaders(t *testing.T) {
	p := enginePlugin(t, newEngineManager(t), assertionPolicy())
	h := http.Header{
		"Authorization":  {"Bearer " + spikeJWT("alice", nil)},
		"X-Auth-User-Id": {"root"},
		"Accept":         {"a", "b"},
		"Cookie":         {"sid=1"},
	}
	pctx := toolCall("get_weather", h)
	wantContinue(t, "get_weather", p.OnRequest(context.Background(), pctx))

	if v, ok := pctx.Headers["Authorization"]; ok {
		t.Errorf("Authorization still forwarded: %v", v)
	}
	if got := pctx.Headers.Values("X-Auth-User-Id"); len(got) != 1 || got[0] != "alice" {
		t.Errorf("X-Auth-User-Id = %v, want [alice]", got)
	}
	if got := pctx.Headers.Values("Accept"); len(got) != 2 {
		t.Errorf("Accept = %v, want both values", got)
	}
	if pctx.Headers.Get("Cookie") != "sid=1" {
		t.Errorf("Cookie = %q, want it untouched", pctx.Headers.Get("Cookie"))
	}
}

const taintPolicy = `
routes:
  - tool: read_env
    authorization:
      pre_invocation: ["taint(secret, session)"]
  - tool: send_report
    authorization:
      pre_invocation:
        - "security.labels contains \"secret\": deny('session touched secret data', 'session_tainted')"
`

// Under session_source: resolved a caller cannot leave a tainted session
// by naming another one in X-Session-Id.
func TestEngineResolvedSessionIgnoresCallerHeader(t *testing.T) {
	mgr := newEngineManager(t)
	mgr.(*cpexManager).opts.SessionSource = sessionSourceResolved
	p := enginePlugin(t, mgr, taintPolicy)
	ctx := context.Background()

	call := func(tool, callerSID string, src pipeline.SessionSource, header string) *pipeline.Context {
		pctx := toolCall(tool, http.Header{"X-Session-Id": {callerSID}})
		pctx.Identity = claimsIdentity{sub: "alice"}
		pctx.Session = &pipeline.SessionView{ID: "claude-1"}
		pctx.SessionSource = src
		pctx.SessionHeader = header
		return pctx
	}
	wantContinue(t, "read_env", p.OnRequest(ctx,
		call("read_env", "a", pipeline.SessionSourceHeader, session.ClaudeCodeSessionHeader)))
	wantReject(t, "send_report, spoofed X-Session-Id", p.OnRequest(ctx,
		call("send_report", "fresh", pipeline.SessionSourceProcess, "")), "cpex.session_tainted")
	// A guessed session carries no session id, so the taint does not apply
	// to it either way.
	wantContinue(t, "send_report, active-session guess", p.OnRequest(ctx,
		call("send_report", "fresh", pipeline.SessionSourceActive, "")))
}

// The default header mode keys sessions on X-Session-Id as before.
func TestEngineHeaderSessionSource(t *testing.T) {
	p := enginePlugin(t, newEngineManager(t), taintPolicy)
	ctx := context.Background()
	call := func(tool, sid string) *pipeline.Context {
		pctx := toolCall(tool, http.Header{"X-Session-Id": {sid}})
		pctx.Identity = claimsIdentity{sub: "alice"}
		return pctx
	}
	wantContinue(t, "read_env", p.OnRequest(ctx, call("read_env", "s1")))
	wantReject(t, "send_report s1", p.OnRequest(ctx, call("send_report", "s1")), "cpex.session_tainted")
	wantContinue(t, "send_report s2", p.OnRequest(ctx, call("send_report", "s2")))
}

// A JWKS on loopback is reachable only with private_destinations.
func TestEnginePrivateDestinations(t *testing.T) {
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "oct", "alg": "HS256", "use": "sig", "kid": "k1",
			"k": base64.RawURLEncoding.EncodeToString(spikeSecret),
		}}})
	}))
	t.Cleanup(jwks.Close)
	policy := `
plugins:
  - name: idp
    kind: identity/jwt
    hooks: [identity.resolve]
    capabilities: [perform_http]
    config:
      claim_mapper: standard
      trusted_issuers:
        - issuer: ` + spikeIssuer + `
          audiences: [` + spikeAudience + `]
          algorithms: [HS256]
          decoding_key:
            kind: jwks_url
            url: "` + jwks.URL + `"
            insecure_http: true
routes:
  - tool: get_weather
    authentication: [idp]
    authorization:
      pre_invocation: ["require(authenticated)"]
`
	run := func(private bool) (pipeline.Action, error) {
		p := NewCPEX()
		raw, _ := json.Marshal(map[string]any{
			"hooks":                map[string]any{"on_request": []string{HookToolPreInvoke}},
			"config":               policy,
			"private_destinations": private,
		})
		if err := p.Configure(raw); err != nil {
			return pipeline.Action{}, err
		}
		if err := p.Init(context.Background()); err != nil {
			return pipeline.Action{}, err
		}
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		pctx := toolCall("get_weather", http.Header{"Authorization": {"Bearer " + kidJWT("alice")}})
		return p.OnRequest(context.Background(), pctx), nil
	}

	a, err := run(false)
	if err != nil {
		t.Fatalf("private_destinations off: %v", err)
	}
	wantReject(t, "private_destinations off", a, "cpex.auth_jwks_unavailable")

	a, err = run(true)
	if err != nil {
		t.Fatalf("private_destinations on: %v", err)
	}
	if a.Type != pipeline.Continue {
		t.Fatalf("private_destinations on: action = %v %+v, want continue", a.Type, a.Violation)
	}
}

// kidJWT is spikeJWT with a kid header, which a JWKS-backed issuer needs.
func kidJWT(sub string) string {
	b64 := base64.RawURLEncoding.EncodeToString
	now := time.Now().Unix()
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(map[string]any{
		"iss": spikeIssuer, "aud": spikeAudience, "sub": sub, "iat": now - 5, "exp": now + 600,
	})
	signing := b64(hdr) + "." + b64(body)
	mac := hmac.New(sha256.New, spikeSecret)
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}
