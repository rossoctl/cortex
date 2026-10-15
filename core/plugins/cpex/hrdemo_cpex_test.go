// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rcpex "github.com/contextforge-org/cpex/go/cpex"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/vmihailenco/msgpack/v5"
)

// The hr-cpex demo's policy, loaded on the real engine with Keycloak
// replaced by a local IdP that serves the same realm paths. Each test
// is one of the demo's scenarios.

const hrRealmPath = "/realms/cpex-demo"

type hrIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	mu        sync.Mutex
	exchanges []string // audience of each token exchange
}

func newHRIdP(t *testing.T) *hrIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &hrIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc(hrRealmPath+"/protocol/openid-connect/certs", func(w http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "hr-test",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc(hrRealmPath+"/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		aud := r.PostForm.Get("audience")
		m.mu.Lock()
		m.exchanges = append(m.exchanges, aud)
		m.mu.Unlock()
		// Keycloak keeps the subject token's subject, and CPEX checks it.
		claims := map[string]any{"sub": jwtSubject(r.PostForm.Get("subject_token")), "aud": aud}
		if aud == "github-api" {
			claims["permissions"] = []string{"repo:read:internal"}
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      m.sign(t, claims),
			"token_type":        "Bearer",
			"expires_in":        300,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func jwtSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.Sub
}

func (m *hrIdP) exchangeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.exchanges)
}

func (m *hrIdP) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	now := time.Now().Unix()
	claims["iss"] = m.srv.URL + hrRealmPath
	claims["iat"] = now - 5
	claims["exp"] = now + 600
	b64 := base64.RawURLEncoding.EncodeToString
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "hr-test"})
	body, _ := json.Marshal(claims)
	signing := b64(hdr) + "." + b64(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

// The demo personas, with the claims realm-export.json's mappers put on
// a user token.
var hrPersonas = map[string]map[string]any{
	"alice": {"roles": []string{"engineer"}, "teams": []string{"engineering"}, "permissions": []string{"tool_execute"}},
	"bob":   {"roles": []string{"hr"}, "teams": []string{"hr"}, "permissions": []string{"tool_execute", "view_ssn", "pii_access", "email_send"}},
	"eve":   {"roles": []string{"hr"}, "teams": []string{"hr"}, "permissions": []string{"tool_execute"}},
}

func (m *hrIdP) userToken(t *testing.T, name string) string {
	claims := map[string]any{"sub": name + "-id", "preferred_username": name, "aud": []string{"cpex-gateway", "account"}, "azp": "hr-copilot"}
	for k, v := range hrPersonas[name] {
		claims[k] = v
	}
	return m.sign(t, claims)
}

func (m *hrIdP) clientToken(t *testing.T) string {
	return m.sign(t, map[string]any{"sub": "hr-copilot-sa", "preferred_username": "service-account-hr-copilot", "aud": "account", "azp": "hr-copilot"})
}

// hrPolicy returns the demo policy pointed at idp, with the gateway
// secret in a temp file.
func hrPolicy(t *testing.T, idp *hrIdP) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "demos", "hr-cpex", "k8s", "cpex-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secret, []byte("cpex-gateway-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := strings.ReplaceAll(string(raw), "http://keycloak.cpex-demo:8080", idp.srv.URL)
	return strings.ReplaceAll(policy, "/etc/cpex-secrets/client-secret", secret)
}

// hrPlugin configures the cpex plugin the way k8s/30-agent.yaml does.
func hrPlugin(t *testing.T, idp *hrIdP) *CPEX {
	t.Helper()
	p := NewCPEX()
	raw, _ := json.Marshal(map[string]any{
		"hooks": map[string]any{
			"on_request":  []string{HookToolPreInvoke},
			"on_response": []string{HookToolPostInvoke},
		},
		"config":               hrPolicy(t, idp),
		"fail_open":            false,
		"private_destinations": true,
	})
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

type hrCall struct {
	user, sid string
	tool      string
	args      map[string]any
	extra     http.Header
}

func (c hrCall) pctx(t *testing.T, idp *hrIdP) *pipeline.Context {
	h := http.Header{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer " + idp.clientToken(t)},
		"X-User-Token":  {idp.userToken(t, c.user)},
	}
	if c.sid != "" {
		h.Set("X-Session-Id", c.sid)
	}
	for k, v := range c.extra {
		h[k] = v
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": c.tool, "arguments": c.args},
	})
	return &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "hr-mcp.cpex-demo:9100",
		Path:      "/mcp",
		Headers:   h,
		Body:      body,
		Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{
			Method: "tools/call",
			RPCID:  float64(1),
			Params: map[string]any{"name": c.tool, "arguments": c.args},
		}},
	}
}

func compensationArgs(includeSSN bool) map[string]any {
	return map[string]any{"employee_id": "EMP-001234", "include_ssn": includeSSN, "ssn": "would-be-removed-if-redact-fires"}
}

func emailArgs(body string) map[string]any {
	return map[string]any{"to": "teammate@example.com", "subject": "team update", "body": body}
}

const cleanEmail = "Quarterly planning sync moved to Thursday."

// Scenario 01: Bob passes the role gate and the exchange runs, but the
// minted token cannot be forwarded, so the call fails closed.
func TestHRDemoBobCompensationFailsClosed(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	pctx := hrCall{user: "bob", tool: "get_compensation", args: compensationArgs(true)}.pctx(t, idp)
	wantReject(t, "bob get_compensation", p.OnRequest(context.Background(), pctx), "cpex.delegation_unavailable")
	if got := idp.exchangeCount(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1", got)
	}
}

// Scenario 02: the role gate denies before any exchange.
func TestHRDemoAliceCompensationDenied(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	pctx := hrCall{user: "alice", tool: "get_compensation", args: compensationArgs(false)}.pctx(t, idp)
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation == nil || a.Violation.Code == "cpex.delegation_unavailable" {
		t.Fatalf("alice get_compensation: %v %+v, want a policy deny", a.Type, a.Violation)
	}
	t.Logf("deny code: %s", a.Violation.Code)
	if got := idp.exchangeCount(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0", got)
	}
}

// Scenario 03: the engine redacts Eve's ssn argument, and redacts the SSN
// in the tool's result. The request itself then fails closed on the
// delegation, so the redaction is checked on the engine's output.
func TestHRDemoEveRedaction(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	call := hrCall{user: "eve", tool: "get_compensation", args: compensationArgs(false)}

	wantReject(t, "eve get_compensation", p.OnRequest(context.Background(), call.pctx(t, idp)), "cpex.delegation_unavailable")

	// Request side, read off the engine's result.
	m := p.manager.(*cpexManager)
	pctx := call.pctx(t, idp)
	payload, ext := buildCMF(pctx, false, m.opts)
	headers, err := identityHeaders(pctx.Headers, false, m.opts.Principal)
	if err != nil {
		t.Fatal(err)
	}
	res, ct, bg, err := m.mgr.InvokeResolved(rcpex.NewIdentityPayload(rcpex.TokenSourceBearer, headers),
		HookToolPreInvoke, rcpex.PayloadCMFMessage, payload, ext, nil)
	if ct != nil {
		defer ct.Close()
	}
	if bg != nil {
		bg.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if !res.ContinueProcessing {
		t.Fatalf("engine denied eve: %+v", res.Violation)
	}
	var mod rcpex.MessagePayload
	if err := msgpack.Unmarshal(res.ModifiedPayload, &mod); err != nil {
		t.Fatal(err)
	}
	if args := toolCallArgs(t, &mod); args["ssn"] != "[REDACTED]" || args["employee_id"] != "EMP-001234" {
		t.Fatalf("forwarded args = %v, want ssn redacted and the rest intact", args)
	}

	// Response side, through the plugin.
	resp := call.pctx(t, idp)
	resp.ResponseBody = []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"name\": \"Jane Smith\", \"ssn\": \"123-45-6789\"}"}]}}`)
	resp.SetCurrentPlugin("cpex", pipeline.InvocationPhaseResponse)
	wantContinue(t, "eve result", p.OnResponse(context.Background(), resp))
	if strings.Contains(string(resp.ResponseBody), "123-45-6789") {
		t.Fatalf("SSN reached eve: %s", resp.ResponseBody)
	}
}

// toolCallArgs returns the arguments of the tool call in a modified
// payload.
func toolCallArgs(t *testing.T, mod *rcpex.MessagePayload) map[string]any {
	t.Helper()
	for _, part := range mod.Message.Content {
		if part.ToolCallContent != nil {
			return part.ToolCallContent.Arguments
		}
	}
	t.Fatal("no tool call in the modified payload")
	return nil
}

// Scenarios 04-06: the APL gate, Cedar, and the delegation post-check.
func TestHRDemoSearchRepos(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	ctx := context.Background()
	repo := func(user, name, vis string) *pipeline.Context {
		args := map[string]any{"visibility": vis}
		if name != "" {
			args["repo_name"] = name
		}
		return hrCall{user: user, tool: "search_repos", args: args}.pctx(t, idp)
	}

	// 04: every layer passes and the exchange runs; forwarding fails closed.
	wantReject(t, "alice internal", p.OnRequest(ctx, repo("alice", "web-app", "internal")), "cpex.delegation_unavailable")
	if got := idp.exchangeCount(); got != 1 {
		t.Fatalf("token exchanges after 04 = %d, want 1", got)
	}

	// 05: Cedar denies an external repo before any exchange.
	a := p.OnRequest(ctx, repo("alice", "partner-sdk", "external"))
	wantReject(t, "alice external", a, "cpex.cedar_default_deny")

	// 06: the APL team gate denies Bob before Cedar.
	a = p.OnRequest(ctx, repo("bob", "", "internal"))
	if a.Type != pipeline.Reject || a.Violation == nil || a.Violation.Code == "cpex.cedar_default_deny" {
		t.Fatalf("bob search_repos: %v %+v, want the APL gate's deny", a.Type, a.Violation)
	}
	t.Logf("deny code: %s", a.Violation.Code)
	if got := idp.exchangeCount(); got != 1 {
		t.Fatalf("token exchanges after 05/06 = %d, want still 1", got)
	}
}

// Scenario 07: an SSN in the email body is denied by the CEL step, and
// the decision is recorded on the session timeline.
func TestHRDemoPIIDenied(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	ctx := context.Background()
	pctx := hrCall{user: "bob", tool: "send_email", args: emailArgs("FYI — Jane Smith. Her SSN is 555-12-3456 if you need to update payroll.")}.pctx(t, idp)
	wantReject(t, "ssn body", p.OnRequest(ctx, pctx), "cpex.pii_detected")
	inv := pctx.Extensions.Invocations
	if inv == nil || len(inv.Outbound) == 0 || inv.Outbound[len(inv.Outbound)-1].Action != pipeline.ActionDeny {
		t.Fatalf("invocations = %+v, want a recorded deny", inv)
	}

	card := hrCall{user: "bob", tool: "send_email", args: emailArgs("card 4111 1111 1111 1111")}.pctx(t, idp)
	wantReject(t, "card body", p.OnRequest(ctx, card), "cpex.pii_detected")
	clean := hrCall{user: "bob", tool: "send_email", args: emailArgs(cleanEmail)}.pctx(t, idp)
	wantContinue(t, "clean body", p.OnRequest(ctx, clean))
}

// Scenarios 08 and 09: reading compensation taints the caller's session
// even though the call itself fails closed on the delegation; the taint
// is bound to the subject.
func TestHRDemoSessionTaint(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	ctx := context.Background()
	email := func(user, sid string) *pipeline.Context {
		return hrCall{user: user, sid: sid, tool: "send_email", args: emailArgs(cleanEmail)}.pctx(t, idp)
	}
	comp := func(user, sid string) *pipeline.Context {
		return hrCall{user: user, sid: sid, tool: "get_compensation", args: compensationArgs(true)}.pctx(t, idp)
	}

	// 08
	wantContinue(t, "S1 clean session", p.OnRequest(ctx, email("bob", "clean")))
	wantReject(t, "S2 get_compensation", p.OnRequest(ctx, comp("bob", "tainted")), "cpex.delegation_unavailable")
	wantReject(t, "S3 tainted session", p.OnRequest(ctx, email("bob", "tainted")), "cpex.session_tainted_secret")

	// 09
	wantContinue(t, "S1 baseline", p.OnRequest(ctx, email("bob", "baseline")))
	wantReject(t, "S2 eve taints", p.OnRequest(ctx, comp("eve", "shared")), "cpex.delegation_unavailable")
	wantContinue(t, "S3 bob, same id", p.OnRequest(ctx, email("bob", "shared")))
}

// Scenario 12: the forwarded request carries the engine's identity
// headers, not the caller's raw token or a spoofed identity.
func TestHRDemoAssertions(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	pctx := hrCall{user: "bob", tool: "send_email", args: emailArgs(cleanEmail),
		extra: http.Header{"X-Auth-User-Id": {"root"}}}.pctx(t, idp)
	wantContinue(t, "bob send_email", p.OnRequest(context.Background(), pctx))

	want := map[string]string{"X-Auth-User-Id": "bob-id", "X-Auth-Username": "bob", "X-Auth-Roles": "hr"}
	for k, v := range want {
		if got := pctx.Headers.Values(k); len(got) != 1 || got[0] != v {
			t.Errorf("%s = %v, want [%s]", k, got, v)
		}
	}
	if v, ok := pctx.Headers["X-User-Token"]; ok {
		t.Errorf("X-User-Token still forwarded: %v", v)
	}
	if !strings.HasPrefix(pctx.Headers.Get("Authorization"), "Bearer ") {
		t.Errorf("Authorization = %q, want the client token kept", pctx.Headers.Get("Authorization"))
	}

	// The response is resolved as the same caller, though the request
	// no longer carries X-User-Token.
	pctx.ResponseBody = []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"status\": \"sent\"}"}]}}`)
	pctx.SetCurrentPlugin("cpex", pipeline.InvocationPhaseResponse)
	wantContinue(t, "bob send_email result", p.OnResponse(context.Background(), pctx))
}

// Protocol traffic the agent sends besides tool calls is not refused.
func TestHRDemoToolsListAllowed(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	pctx := mcpRequest("tools/list")
	pctx.Headers.Set("X-User-Token", idp.userToken(t, "bob"))
	wantContinue(t, "tools/list", p.OnRequest(context.Background(), pctx))
}
