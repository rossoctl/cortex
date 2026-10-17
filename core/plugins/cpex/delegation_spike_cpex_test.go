// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	rcpex "github.com/contextforge-org/cpex/go/cpex"
	"github.com/rossoctl/cortex/core/pipeline"
)

// The delegation tests below drive the CPEX engine against a loopback
// IdP and pin down how a delegate() step's minted token reaches the
// forwarded request: only through PipelineResult.DelegatedCredentials of
// an engine enabled WithDelegatedCredentials, never through the result's
// extensions, payload or metadata, and never into a session record or log.

const (
	spikeIssuer   = "https://idp.spike.test"
	spikeAudience = "cortex-spike"
	spikeMinted   = "mQ7x2ZpL9vKd4RwT8hNc"
)

var spikeSecret = []byte("cortex-spike-only-hs256-secret-0123456789")

type spikeIdP struct {
	srv       *httptest.Server
	mu        sync.Mutex
	exchanges []url.Values
}

// newSpikeIdP serves an RFC 8693 token endpoint granting audiences
// workday-api and github-api and refusing every other audience.
func newSpikeIdP(t *testing.T) *spikeIdP {
	t.Helper()
	m := &spikeIdP{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.exchanges = append(m.exchanges, r.PostForm)
		m.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		if aud := r.PostForm.Get("audience"); aud != "workday-api" && aud != "github-api" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_target"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      spikeMinted,
			"token_type":        "Bearer",
			"expires_in":        300,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *spikeIdP) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.exchanges)
}

func spikeJWT(sub string, roles []string) string {
	b64 := base64.RawURLEncoding.EncodeToString
	now := time.Now().Unix()
	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	body, _ := json.Marshal(map[string]any{
		"iss": spikeIssuer, "aud": spikeAudience, "sub": sub,
		"iat": now - 5, "exp": now + 600, "roles": roles,
	})
	signing := b64(hdr) + "." + b64(body)
	mac := hmac.New(sha256.New, spikeSecret)
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}

func spikePolicy(tokenURL string) string {
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
  - name: workday-oauth
    kind: delegator/oauth
    hooks: [token.delegate]
    capabilities: [read_inbound_credentials, write_delegated_tokens, perform_http]
    config:
      token_endpoint: "` + tokenURL + `"
      client_id: cortex
      client_secret_source: {kind: literal, secret: spike-only}
      insecure_http: true
routes:
  - tool: get_compensation
    authentication: [idp]
    authorization:
      pre_invocation:
        - "require(role.hr)"
        - "delegate(workday-oauth, target: workday-api, audience: workday-api)"
        - "require(delegation.granted)"
  - tool: get_both
    authentication: [idp]
    authorization:
      pre_invocation:
        - "delegate(workday-oauth, target: workday-api, audience: workday-api)"
        - "delegate(workday-oauth, target: github-api, audience: github-api)"
        - "require(delegation.granted)"
`
}

func spikeEngine(t *testing.T, policy string, opts ...rcpex.APLOption) *rcpex.PolicyEngine {
	t.Helper()
	eng, err := rcpex.NewPolicyEngineDefault()
	if err != nil {
		t.Fatalf("NewPolicyEngineDefault: %v", err)
	}
	t.Cleanup(eng.Shutdown)
	if err := eng.EnableAPL(append([]rcpex.APLOption{rcpex.WithPrivateDestinations()}, opts...)...); err != nil {
		t.Fatalf("EnableAPL: %v", err)
	}
	if err := eng.LoadConfig(policy); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := eng.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return eng
}

func spikeCall(t *testing.T, eng *rcpex.PolicyEngine, bearer, tool string) *rcpex.PipelineResult {
	t.Helper()
	idp := rcpex.NewIdentityPayload(rcpex.TokenSourceBearer, map[string]string{"authorization": "Bearer " + bearer})
	ext := &rcpex.Extensions{Meta: &rcpex.MetaExtension{EntityType: "tool", EntityName: tool}}
	payload := rcpex.MessagePayload{Message: rcpex.NewMessage("assistant", rcpex.NewToolCallPart(rcpex.ToolCall{
		ToolCallID: "spike-1", Name: tool, Arguments: map[string]any{"employee_id": "e-1"},
	}))}
	res, ct, bg, err := eng.InvokeResolved(idp, "cmf.tool_pre_invoke", rcpex.PayloadCMFMessage, payload, ext, nil)
	if err != nil {
		t.Fatalf("InvokeResolved(%s): %v", tool, err)
	}
	if bg != nil {
		bg.Close()
	}
	if ct != nil {
		ct.Close()
	}
	return res
}

// Without the opt-in a granted delegation reports the token's metadata,
// but no channel of the Go result carries the minted value.
func TestDelegationMintedTokenWithheldWithoutOptIn(t *testing.T) {
	idp := newSpikeIdP(t)
	eng := spikeEngine(t, spikePolicy(idp.srv.URL+"/token"))

	res := spikeCall(t, eng, spikeJWT("alice", []string{"hr"}), "get_compensation")
	if res.IsDenied() {
		t.Fatalf("expected allowed, got %+v", res.Violation)
	}
	if got := idp.count(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1", got)
	}
	if len(res.DelegatedCredentials) != 0 {
		t.Fatalf("credentials returned without the opt-in: %d", len(res.DelegatedCredentials))
	}
	if n, err := delegatedTokenCount(res); err != nil || n != 1 {
		t.Fatalf("delegatedTokenCount = %d, %v; want 1", n, err)
	}
	assertNoToken(t, "engine result", res.ModifiedExtensions, res.ModifiedPayload, []byte(fmt.Sprintf("%+v %#v", res, res)))
}

// With the opt-in the engine returns the minted credential, and nothing
// but Reveal prints it.
func TestDelegationMintedTokenReturnedWithOptIn(t *testing.T) {
	idp := newSpikeIdP(t)
	eng := spikeEngine(t, spikePolicy(idp.srv.URL+"/token"), rcpex.WithDelegatedCredentials())

	res := spikeCall(t, eng, spikeJWT("alice", []string{"hr"}), "get_compensation")
	if res.IsDenied() || len(res.DelegatedCredentials) != 1 {
		t.Fatalf("denied=%v credentials=%d, want one credential", res.IsDenied(), len(res.DelegatedCredentials))
	}
	c := res.DelegatedCredentials[0]
	if c.Header != "Authorization" || c.Value.Reveal() != "Bearer "+spikeMinted || c.Target != "workday-api" {
		t.Fatalf("credential = %+v", c)
	}
	assertNoToken(t, "engine result", res.ModifiedExtensions, res.ModifiedPayload, []byte(fmt.Sprintf("%v %+v %#v", res, res, c)))
}

// spikePlugin configures the cpex plugin on spikePolicy, with delegated
// credentials forwarded or not.
func spikePlugin(t *testing.T, idp *spikeIdP, forward bool) *CPEX {
	t.Helper()
	cfg := map[string]any{
		"config":               spikePolicy(idp.srv.URL + "/token"),
		"private_destinations": true,
	}
	if forward {
		cfg["delegated_credentials"] = true
		cfg["delegated_credential_hosts"] = map[string][]string{"workday-api": {"tools.example"}}
	}
	return configuredEngine(t, cfg, HookToolPreInvoke)
}

// spikePluginBound configures the cpex plugin on spikePolicy forwarding
// delegated credentials to the hosts bindings name.
func spikePluginBound(t *testing.T, idp *spikeIdP, bindings map[string][]string) *CPEX {
	t.Helper()
	return configuredEngine(t, map[string]any{
		"config":                     spikePolicy(idp.srv.URL + "/token"),
		"private_destinations":       true,
		"delegated_credentials":      true,
		"delegated_credential_hosts": bindings,
	}, HookToolPreInvoke)
}

// A minted credential reaches only a host its target is bound to. Any
// other destination, or a target with no binding, denies the request
// with nothing written to it, even when another credential would fit.
func TestDelegationCredentialBoundToDestination(t *testing.T) {
	logs := captureLogs(t)
	idp := newSpikeIdP(t)
	caller := "Bearer " + spikeJWT("alice", []string{"hr"})
	cases := []struct {
		name     string
		bindings map[string][]string
		tool     string
		host     string
		forward  bool
	}{
		{"bound host", map[string][]string{"workday-api": {"tools.example"}}, "get_compensation", "tools.example:8080", true},
		{"other host", map[string][]string{"workday-api": {"tools.example"}}, "get_compensation", "attacker.example", false},
		{"unbound target", map[string][]string{"github-api": {"tools.example"}}, "get_compensation", "tools.example", false},
		{"both bound", map[string][]string{"workday-api": {"tools.example"}, "github-api": {"tools.example"}}, "get_both", "tools.example", true},
		{"one of two bound", map[string][]string{"workday-api": {"tools.example"}, "github-api": {"github.example"}}, "get_both", "tools.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := spikePluginBound(t, idp, tc.bindings)
			pctx := toolCall(tc.tool, http.Header{"Authorization": {caller}, "X-Keep": {"1"}})
			pctx.Host = tc.host
			before := pctx.Headers.Clone()
			a := p.OnRequest(context.Background(), pctx)
			if tc.forward {
				wantContinue(t, tc.name, a)
				if got := pctx.Headers.Get("Authorization"); got != "Bearer "+spikeMinted {
					t.Fatal("bound destination did not get the minted credential")
				}
				return
			}
			wantReject(t, tc.name, a, "cpex."+codeDelegationDestinationMismatch)
			if a.Violation.Status != http.StatusForbidden || a.Violation.Reason != deniedMessage {
				t.Fatalf("violation = %d %q, want 403 with the static message", a.Violation.Status, a.Violation.Reason)
			}
			if !reflect.DeepEqual(pctx.Headers, before) {
				t.Fatal("a denied request's headers were changed")
			}
		})
	}
	assertNoToken(t, "logs", logs.Bytes())
}

// captureLogs sends every slog record, debug included, to a buffer for
// the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// assertNoToken fails when any 8-byte run of the minted token appears in
// one of bufs.
func assertNoToken(t *testing.T, label string, bufs ...[]byte) {
	t.Helper()
	for _, b := range bufs {
		for i := 0; i+8 <= len(spikeMinted); i++ {
			if bytes.Contains(b, []byte(spikeMinted[i:i+8])) {
				t.Fatalf("%s carries part of the minted token", label)
			}
		}
	}
}

// The minted credential replaces the caller's Authorization on the
// forwarded request, and stays out of the session record and the logs.
func TestDelegationCredentialForwarded(t *testing.T) {
	logs := captureLogs(t)
	idp := newSpikeIdP(t)
	p := spikePlugin(t, idp, true)

	caller := "Bearer " + spikeJWT("alice", []string{"hr"})
	pctx := toolCall("get_compensation", http.Header{"Authorization": {caller}})
	a := p.OnRequest(context.Background(), pctx)
	wantContinue(t, "alice get_compensation", a)
	if got := idp.count(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1", got)
	}
	if got := pctx.Headers.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+spikeMinted {
		t.Fatalf("forwarded Authorization = %d values, want the minted token alone", len(got))
	}
	if inv := lastInvocation(t, pctx); inv.Action != "modify" {
		t.Fatalf("invocation action = %q, want modify", inv.Action)
	}

	recorded, err := json.Marshal(map[string]any{"extensions": pctx.Extensions, "identity": pctx.Identity, "action": a})
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, "session record", recorded)
	assertNoToken(t, "logs", logs.Bytes())
}

// A request the policy denies before its delegate() step reaches no IdP
// and gets no credential.
func TestDelegationDeniedBeforeExchange(t *testing.T) {
	idp := newSpikeIdP(t)
	eng := spikeEngine(t, spikePolicy(idp.srv.URL+"/token"), rcpex.WithDelegatedCredentials())

	res := spikeCall(t, eng, spikeJWT("evan", []string{"engineer"}), "get_compensation")
	if !res.IsDenied() || len(res.DelegatedCredentials) != 0 {
		t.Fatalf("denied=%v credentials=%d, want a deny with none", res.IsDenied(), len(res.DelegatedCredentials))
	}
	if n, _ := delegatedTokenCount(res); n != 0 {
		t.Fatal("a denied result must not report a granted delegation")
	}

	p := spikePlugin(t, idp, true)
	caller := "Bearer " + spikeJWT("evan", []string{"engineer"})
	pctx := toolCall("get_compensation", http.Header{"Authorization": {caller}})
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation.Code == "cpex."+codeDelegationUnavailable {
		t.Fatalf("evan get_compensation: %v %+v, want a policy deny", a.Type, a.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != caller {
		t.Fatal("a denied request's Authorization was rewritten")
	}
	if got := idp.count(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0", got)
	}
}

// Without delegated_credentials the plugin cannot forward the minted
// token, so it fails closed rather than send the caller's bearer.
func TestDelegationUnavailableWithoutOptIn(t *testing.T) {
	idp := newSpikeIdP(t)
	p := spikePlugin(t, idp, false)
	caller := "Bearer " + spikeJWT("alice", []string{"hr"})
	pctx := toolCall("get_compensation", http.Header{"Authorization": {caller}})
	wantReject(t, "alice get_compensation", p.OnRequest(context.Background(), pctx), "cpex."+codeDelegationUnavailable)
	if got := idp.count(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1", got)
	}
}

// A credential for a header outside delegated_credential_headers is not
// forwarded, and the request fails closed.
func TestDelegationCredentialHeaderNotAllowed(t *testing.T) {
	idp := newSpikeIdP(t)
	p := configuredEngine(t, map[string]any{
		"config":                       spikePolicy(idp.srv.URL + "/token"),
		"private_destinations":         true,
		"delegated_credentials":        true,
		"delegated_credential_headers": []string{"X-Workday-Token"},
		"delegated_credential_hosts":   map[string][]string{"workday-api": {"tools.example"}},
	}, HookToolPreInvoke)
	caller := "Bearer " + spikeJWT("alice", []string{"hr"})
	pctx := toolCall("get_compensation", http.Header{"Authorization": {caller}})
	wantReject(t, "alice get_compensation", p.OnRequest(context.Background(), pctx), "cpex."+codeDelegationUnavailable)
	if got := pctx.Headers.Get("Authorization"); got != caller {
		t.Fatal("Authorization was rewritten")
	}
}
