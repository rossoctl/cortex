// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	rcpex "github.com/contextforge-org/cpex/go/cpex"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/spiffe"
	"github.com/vmihailenco/msgpack/v5"
	"gopkg.in/yaml.v3"
)

// The hr-cpex demo's policy, loaded on the real engine with Keycloak
// replaced by a local IdP that serves the same realm paths. Each test
// is one of the demo's scenarios.

const hrRealmPath = "/realms/cpex-demo"

// hrSpirePath stands in for SPIRE's OIDC discovery provider, and
// hrAgentSPIFFEID is the agent's registered SPIFFE ID.
const (
	hrSpirePath     = "/spire"
	hrAgentSPIFFEID = "spiffe://cpex.demo/ns/cpex-demo/sa/hr-cpex-agent"
)

type hrIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	spireKey  *ecdsa.PrivateKey // SPIRE's JWT-SVID signing key
	mu        sync.Mutex
	exchanges []string // audience of each token exchange
	subjects  []string // subject_token of each token exchange
	issued    []string // access_token of each token exchange
	ciba      []*cibaRequest
	// assertions holds the client_assertion of each workload leg 1.
	assertions []string
}

// cibaRequest is one backchannel authentication request. The test
// answers it by setting state, as the approver's phone would.
type cibaRequest struct {
	id, loginHint, bindingMessage string
	state                         string // pending, approved, denied, spent
}

func newHRIdP(t *testing.T) *hrIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	spireKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := &hrIdP{key: key, spireKey: spireKey}
	mux := http.NewServeMux()
	// SPIRE's OIDC discovery provider: the JWKS the workload resolver uses.
	mux.HandleFunc(hrSpirePath+"/keys", func(w http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		pad := func(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "spire-test",
			"x": b64(pad(spireKey.X.Bytes())), "y": b64(pad(spireKey.Y.Bytes())),
		}}})
	})
	mux.HandleFunc(hrRealmPath+"/protocol/openid-connect/certs", func(w http.ResponseWriter, _ *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "hr-test",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc(hrRealmPath+"/protocol/openid-connect/ext/ciba/auth", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		m.mu.Lock()
		req := &cibaRequest{
			id:        fmt.Sprintf("ciba-%d", len(m.ciba)+1),
			loginHint: r.PostForm.Get("login_hint"), bindingMessage: r.PostForm.Get("binding_message"),
			state: "pending",
		}
		m.ciba = append(m.ciba, req)
		m.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"auth_req_id": req.id, "expires_in": 300, "interval": 1})
	})
	mux.HandleFunc(hrRealmPath+"/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") == "urn:openid:params:grant-type:ciba" {
			m.cibaPoll(t, w, r.PostForm.Get("auth_req_id"))
			return
		}
		if r.PostForm.Get("client_assertion_type") == "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe" {
			m.clientAssertion(t, w, r)
			return
		}
		aud := r.PostForm.Get("audience")
		m.mu.Lock()
		m.exchanges = append(m.exchanges, aud)
		m.subjects = append(m.subjects, r.PostForm.Get("subject_token"))
		m.mu.Unlock()
		// Keycloak keeps the subject token's subject, and CPEX checks it.
		claims := map[string]any{"sub": jwtSubject(r.PostForm.Get("subject_token")), "aud": aud}
		if aud == "github-api" {
			claims["permissions"] = []string{"repo:read:internal"}
		}
		tok := m.sign(t, claims)
		m.mu.Lock()
		m.issued = append(m.issued, tok)
		m.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      tok,
			"token_type":        "Bearer",
			"expires_in":        300,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// clientAssertion answers leg 1 of a workload delegation the way
// Keycloak's SPIFFE client authentication does: a client_credentials
// grant authenticated by the SVID, for the client bound to its subject.
func (m *hrIdP) clientAssertion(t *testing.T, w http.ResponseWriter, r *http.Request) {
	svid := r.PostForm.Get("client_assertion")
	m.mu.Lock()
	m.assertions = append(m.assertions, svid)
	m.mu.Unlock()
	w.Header().Set("content-type", "application/json")
	if r.PostForm.Get("grant_type") != "client_credentials" || jwtSubject(svid) != hrAgentSPIFFEID {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
		return
	}
	tok := m.sign(t, map[string]any{"sub": "hr-copilot-agent-sa", "azp": "hr-copilot-agent", "aud": "cpex-gateway"})
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 300})
}

// cibaPoll answers a CIBA token poll the way Keycloak does: pending
// until answered, then the approver's tokens exactly once.
func (m *hrIdP) cibaPoll(t *testing.T, w http.ResponseWriter, id string) {
	m.mu.Lock()
	var req *cibaRequest
	for _, c := range m.ciba {
		if c.id == id {
			req = c
		}
	}
	state := "invalid_grant"
	if req != nil {
		state = req.state
		if state == "approved" {
			req.state = "spent"
		}
	}
	m.mu.Unlock()
	w.Header().Set("content-type", "application/json")
	switch state {
	case "approved":
		tok := m.sign(t, map[string]any{"sub": req.loginHint + "-id", "preferred_username": req.loginHint, "aud": "cpex-gateway"})
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "id_token": tok, "token_type": "Bearer", "expires_in": 300})
	case "pending":
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	case "denied":
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	default:
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
	}
}

// answer sets the state of the newest backchannel request and returns it.
func (m *hrIdP) answer(t *testing.T, state string) cibaRequest {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ciba) == 0 {
		t.Fatal("no CIBA request was dispatched")
	}
	req := m.ciba[len(m.ciba)-1]
	req.state = state
	return *req
}

func (m *hrIdP) cibaCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ciba)
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
	"bob":   {"roles": []string{"hr"}, "teams": []string{"hr"}, "permissions": []string{"tool_execute", "view_ssn", "pii_access", "email_send"}, "manager": "alice"},
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
	policy = strings.ReplaceAll(policy, "http://spire-oidc.spire:8443", idp.srv.URL+hrSpirePath)
	return strings.ReplaceAll(policy, "/etc/cpex-secrets/client-secret", secret)
}

// hrPlugin configures the cpex plugin the way k8s/authbridge.yaml does,
// without SPIRE.
func hrPlugin(t *testing.T, idp *hrIdP) *CPEX {
	t.Helper()
	return hrPluginWorkload(t, idp, nil)
}

// hrPluginWorkload is hrPlugin with svid standing in for the SPIFFE
// provider's JWT-SVID source; nil is a sidecar with no spiffe: block.
func hrPluginWorkload(t *testing.T, idp *hrIdP, svid spiffe.JWTSource) *CPEX {
	t.Helper()
	return hrPluginWith(t, idp, svid, nil)
}

// hrPluginForwarding is hrPluginWorkload with delegated credentials
// forwarded, as `make apply CPEX_DELEGATION=available` configures it, to
// the hosts k8s/authbridge.yaml binds.
func hrPluginForwarding(t *testing.T, idp *hrIdP, svid spiffe.JWTSource) *CPEX {
	t.Helper()
	return hrPluginWith(t, idp, svid, map[string]any{
		"delegated_credentials":      true,
		"delegated_credential_hosts": hrCredentialHosts(t),
	})
}

// hrCPEXConfig is the cpex plugin's config in k8s/authbridge.yaml, the
// keys these tests copy from it.
type hrCPEXConfig struct {
	Hosts       map[string][]string `yaml:"delegated_credential_hosts"`
	StrictHosts []string            `yaml:"strict_hosts"`
}

func hrManifestConfig(t *testing.T) hrCPEXConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "demos", "hr-cpex", "k8s", "authbridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Pipeline struct {
			Outbound struct {
				Plugins []struct {
					Name   string       `yaml:"name"`
					Config hrCPEXConfig `yaml:"config"`
				} `yaml:"plugins"`
			} `yaml:"outbound"`
		} `yaml:"pipeline"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range cfg.Pipeline.Outbound.Plugins {
		if p.Name == "cpex" {
			return p.Config
		}
	}
	t.Fatal("k8s/authbridge.yaml has no cpex plugin")
	return hrCPEXConfig{}
}

// hrCredentialHosts returns the cpex plugin's delegated_credential_hosts
// from k8s/authbridge.yaml.
func hrCredentialHosts(t *testing.T) map[string][]string {
	t.Helper()
	hosts := hrManifestConfig(t).Hosts
	if len(hosts) == 0 {
		t.Fatal("k8s/authbridge.yaml binds no delegated_credential_hosts")
	}
	return hosts
}

// hrStrictHosts returns the cpex plugin's strict_hosts from
// k8s/authbridge.yaml.
func hrStrictHosts(t *testing.T) []string {
	t.Helper()
	hosts := hrManifestConfig(t).StrictHosts
	if len(hosts) == 0 {
		t.Fatal("k8s/authbridge.yaml lists no strict_hosts")
	}
	return hosts
}

func hrPluginWith(t *testing.T, idp *hrIdP, svid spiffe.JWTSource, extra map[string]any) *CPEX {
	t.Helper()
	p := NewCPEX()
	p.testWorkloadJWT = svid
	cfg := map[string]any{
		"workload": map[string]any{"audience": idp.srv.URL + hrRealmPath},
		"hooks": map[string]any{
			"on_request":  []string{HookToolPreInvoke},
			"on_response": []string{HookToolPostInvoke},
		},
		"config":               hrPolicy(t, idp),
		"fail_open":            false,
		"private_destinations": true,
		"strict_hosts":         hrStrictHosts(t),
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, _ := json.Marshal(cfg)
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
	// Params as the mcp-parser leaves them, decoded from the body, so a
	// number is a float64 here exactly as it is in the sidecar.
	var rpc struct {
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(body, &rpc)
	return &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      "hr-mcp.cpex-demo:9100",
		Path:      "/mcp",
		Headers:   h,
		Body:      body,
		Extensions: pipeline.Extensions{MCP: &pipeline.MCPExtension{
			Method: "tools/call",
			RPCID:  float64(1),
			Params: rpc.Params,
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

// lastIssued returns the token the IdP issued on its latest exchange.
func (m *hrIdP) lastIssued(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.issued) == 0 {
		t.Fatal("no token exchange")
	}
	return m.issued[len(m.issued)-1]
}

// Scenarios 01 and 03 with delegated credentials forwarded: the tool
// receives the exchanged token in place of the agent's, and Eve's ssn
// argument redacted.
func TestHRDemoDelegatedCredentialForwarded(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPluginForwarding(t, idp, nil)

	bob := hrCall{user: "bob", tool: "get_compensation", args: compensationArgs(true)}.pctx(t, idp)
	wantContinue(t, "bob get_compensation", p.OnRequest(context.Background(), bob))
	if got := bob.Headers.Get("Authorization"); got != "Bearer "+idp.lastIssued(t) {
		t.Fatal("bob's call does not carry the exchanged token")
	}

	eve := hrCall{user: "eve", tool: "get_compensation", args: compensationArgs(false)}.pctx(t, idp)
	wantContinue(t, "eve get_compensation", p.OnRequest(context.Background(), eve))
	if got := eve.Headers.Get("Authorization"); got != "Bearer "+idp.lastIssued(t) {
		t.Fatal("eve's call does not carry the exchanged token")
	}
	if strings.Contains(string(eve.Body), "would-be-removed") || !strings.Contains(string(eve.Body), "[REDACTED]") {
		t.Fatalf("eve's forwarded body = %s, want ssn redacted", eve.Body)
	}
}

// Every delegating route's target is bound to hr-mcp, and only there: the
// same calls to another host are denied after the exchange, and nothing
// is written to them.
func TestHRDemoDelegatedCredentialBoundToHRMCP(t *testing.T) {
	idp := newHRIdP(t)
	svid := idp.svid(t, hrAgentSPIFFEID, idp.srv.URL+hrRealmPath)
	p := hrPluginForwarding(t, idp, &fakeJWT{tok: svid})
	calls := map[string]func(host string) *pipeline.Context{
		"get_compensation": func(host string) *pipeline.Context {
			c := hrCall{user: "bob", tool: "get_compensation", args: compensationArgs(true)}.pctx(t, idp)
			c.Host = host
			return c
		},
		"search_repos": func(host string) *pipeline.Context {
			c := hrCall{user: "alice", tool: "search_repos", args: map[string]any{"repo_name": "web-app", "visibility": "internal"}}.pctx(t, idp)
			c.Host = host
			return c
		},
		"get_headcount": func(host string) *pipeline.Context {
			c := headcount(t, idp, nil)
			c.Host = host
			return c
		},
	}
	for tool, call := range calls {
		for _, host := range []string{"hr-mcp:9100", "hr-mcp.cpex-demo.svc.cluster.local:9100"} {
			pctx := call(host)
			wantContinue(t, tool+" to "+host, p.OnRequest(context.Background(), pctx))
			if got := pctx.Headers.Get("Authorization"); got != "Bearer "+idp.lastIssued(t) {
				t.Fatalf("%s to %s does not carry the exchanged token", tool, host)
			}
		}
		pctx := call("attacker.example:9100")
		before := pctx.Headers.Clone()
		wantReject(t, tool+" to attacker.example", p.OnRequest(context.Background(), pctx), "cpex.delegation_destination_mismatch")
		if !reflect.DeepEqual(pctx.Headers, before) {
			t.Fatalf("%s to attacker.example: headers changed", tool)
		}
	}
}

// Scenario 02: the role gate denies before any exchange.
func TestHRDemoAliceCompensationDenied(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	pctx := hrCall{user: "alice", tool: "get_compensation", args: compensationArgs(false)}.pctx(t, idp)
	wantReject(t, "alice get_compensation", p.OnRequest(context.Background(), pctx),
		"cpex.routes_tool_get_compensation_pre_invocation_0_")
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
	wantReject(t, "bob search_repos", a, "cpex.routes_tool_search_repos_pre_invocation_0_")
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

func adjustArgs(amount any) map[string]any {
	return map[string]any{"employee_id": "EMP-001234", "amount": amount}
}

// adjust sends Bob's adjust_compensation call, resuming eid when set.
func adjust(t *testing.T, p *CPEX, idp *hrIdP, amount any, eid string) (pipeline.Action, *pipeline.Context) {
	t.Helper()
	call := hrCall{user: "bob", tool: "adjust_compensation", args: adjustArgs(amount)}
	if eid != "" {
		call.extra = http.Header{"X-Policy-Elicitation-Id": {eid}}
	}
	pctx := call.pctx(t, idp)
	return p.OnRequest(context.Background(), pctx), pctx
}

// wantPending checks the call was suspended and returns the id to resume
// with. The approver is named when the approval is first requested.
func wantPending(t *testing.T, label string, a pipeline.Action) string {
	t.Helper()
	wantReject(t, label, a, "cpex.elicitation_pending")
	if a.Violation.RPCCode != rpcElicitationPending {
		t.Fatalf("%s: RPCCode = %d, want %d", label, a.Violation.RPCCode, rpcElicitationPending)
	}
	eid, _ := a.Violation.Details["elicitation_id"].(string)
	if eid == "" {
		t.Fatalf("%s: details = %v, want an elicitation id", label, a.Violation.Details)
	}
	if ap, ok := a.Violation.Details["approver"]; ok && ap != "alice" {
		t.Fatalf("%s: approver = %v, want alice", label, ap)
	}
	return eid
}

// Scenario 10: an adjustment at or under $10,000 needs no approval.
func TestHRDemoAdjustUnderThreshold(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	a, _ := adjust(t, p, idp, 5000, "")
	wantContinue(t, "bob +5000", a)
	if n := idp.cibaCount(); n != 0 {
		t.Fatalf("CIBA requests = %d, want 0", n)
	}
}

// Scenario 11: over $10,000 the call is suspended until Bob's manager
// approves; the approval is spent by the call it lets through.
func TestHRDemoAdjustNeedsApproval(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)

	eid := wantPending(t, "bob +15000", first(adjust(t, p, idp, 15000, "")))
	if req := idp.answer(t, "pending"); req.loginHint != "alice" {
		t.Fatalf("approval asked of %q, want alice", req.loginHint)
	}
	if got := wantPending(t, "retry before approval", first(adjust(t, p, idp, 15000, eid))); got != eid {
		t.Fatalf("retry opened a new approval %q, want %q", got, eid)
	}

	idp.answer(t, "approved")
	wantContinue(t, "retry after approval", first(adjust(t, p, idp, 15000, eid)))

	// Spent: the same id does not authorize a second change.
	wantReject(t, "replayed approval", first(adjust(t, p, idp, 20000, eid)), "cpex.elicitation_unknown_id")

	// A new $20,000 change asks again.
	if got := wantPending(t, "bob +20000", first(adjust(t, p, idp, 20000, ""))); got == eid {
		t.Fatal("a new change reused the spent approval")
	}
}

// An approval does not stretch past the route's scope, and a refusal
// is a deny.
func TestHRDemoAdjustScopeAndRefusal(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)

	eid := wantPending(t, "bob +30000", first(adjust(t, p, idp, 30000, "")))
	idp.answer(t, "approved")
	wantReject(t, "over scope", first(adjust(t, p, idp, 30000, eid)), approvalStepDeny)

	eid = wantPending(t, "bob +12000", first(adjust(t, p, idp, 12000, "")))
	idp.answer(t, "denied")
	wantReject(t, "refused", first(adjust(t, p, idp, 12000, eid)), approvalStepDeny)
}

// approvalStepDeny is the code of a deny from the route's approval step.
const approvalStepDeny = "cpex.routes_tool_adjust_compensation_pre_invocation_2_"

func first(a pipeline.Action, _ *pipeline.Context) pipeline.Action { return a }

// The threshold reads a numeric amount; anything else is refused before
// it, so a string or missing amount cannot slip under it. A cut over
// $10,000 needs approval like a raise.
func TestHRDemoAdjustAmountChecked(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	const argDeny = "cpex.routes_tool_adjust_compensation_args_amount"
	for _, amount := range []any{"15000", "5000", true, nil, []any{15000}} {
		wantReject(t, fmt.Sprintf("amount %#v", amount), first(adjust(t, p, idp, amount, "")), argDeny)
	}
	missing := hrCall{user: "bob", tool: "adjust_compensation", args: map[string]any{"employee_id": "EMP-001234", "Amount": 50000}}
	wantReject(t, "missing amount", p.OnRequest(context.Background(), missing.pctx(t, idp)),
		"cpex.routes_tool_adjust_compensation_pre_invocation_1_")
	if n := idp.cibaCount(); n != 0 {
		t.Fatalf("CIBA requests = %d, want 0", n)
	}
	wantContinue(t, "bob -5000", first(adjust(t, p, idp, -5000, "")))
	wantPending(t, "bob -50000", first(adjust(t, p, idp, -50000, "")))
	wantPending(t, "bob +10000.5", first(adjust(t, p, idp, 10000.5, "")))
}

// A tool with no route of its own, a differently cased name included,
// is refused rather than passed with no policy.
func TestHRDemoUnroutedToolDenied(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	for _, tool := range []string{"Adjust_Compensation", "delete_employee"} {
		pctx := hrCall{user: "bob", tool: tool, args: adjustArgs(50000)}.pctx(t, idp)
		wantReject(t, tool, p.OnRequest(context.Background(), pctx), "cpex.tool_unrouted")
	}
	for _, tool := range []string{"display_compensation", "get_directory"} {
		pctx := hrCall{user: "alice", tool: tool, args: map[string]any{"employee_id": "EMP-001234"}}.pctx(t, idp)
		wantContinue(t, tool, p.OnRequest(context.Background(), pctx))
		anon := hrCall{user: "alice", tool: tool}.pctx(t, idp)
		anon.Headers.Del("X-User-Token")
		if a := p.OnRequest(context.Background(), anon); a.Type != pipeline.Reject {
			t.Fatalf("%s without a user token: %v, want a reject", tool, a.Type)
		}
	}
}

// svid mints a JWT-SVID as SPIRE would for the agent: ES256, issued by
// the discovery provider, for the given audience.
func (m *hrIdP) svid(t *testing.T, sub, aud string) string {
	t.Helper()
	now := time.Now().Unix()
	b64 := base64.RawURLEncoding.EncodeToString
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": "spire-test"})
	body, _ := json.Marshal(map[string]any{
		"iss": m.srv.URL + hrSpirePath, "sub": sub, "aud": []string{aud},
		"iat": now - 5, "exp": now + 300,
	})
	signing := b64(hdr) + "." + b64(body)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, m.spireKey, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signing + "." + b64(sig)
}

func (m *hrIdP) legs() (assertions []string, exchanges, subjects []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.assertions...), append([]string(nil), m.exchanges...), append([]string(nil), m.subjects...)
}

// headcount is the agent's own get_headcount call: no user token, no
// client token.
func headcount(t *testing.T, idp *hrIdP, extra http.Header) *pipeline.Context {
	pctx := hrCall{user: "bob", tool: "get_headcount", args: map[string]any{}, extra: extra}.pctx(t, idp)
	pctx.Headers.Del("X-User-Token")
	pctx.Headers.Del("Authorization")
	return pctx
}

// Scenario 13: the route authenticates by the agent's SVID alone, and
// the delegator presents it as a client assertion (leg 1) and exchanges
// the token that buys for workday-api (leg 2). The plugin does not take the
// minted token from CPEX, so it cannot be forwarded, so the call then fails closed.
func TestHRDemoWorkloadDelegation(t *testing.T) {
	idp := newHRIdP(t)
	svid := idp.svid(t, hrAgentSPIFFEID, idp.srv.URL+hrRealmPath)
	p := hrPluginWorkload(t, idp, &fakeJWT{tok: svid})

	wantReject(t, "agent get_headcount", p.OnRequest(context.Background(), headcount(t, idp, nil)), "cpex.delegation_unavailable")
	assertions, exchanges, subjects := idp.legs()
	if len(assertions) != 1 || assertions[0] != svid {
		t.Fatalf("leg 1 assertions = %d, want the agent's SVID once", len(assertions))
	}
	if len(exchanges) != 1 || exchanges[0] != "workday-api" {
		t.Fatalf("leg 2 exchanges = %v, want [workday-api]", exchanges)
	}
	if got := jwtSubject(subjects[0]); got != "hr-copilot-agent-sa" {
		t.Fatalf("leg 2 subject_token sub = %q, want the leg-1 token's", got)
	}
}

// Scenario 13 with delegated credentials forwarded: the tool receives the
// token leg 2 bought.
func TestHRDemoWorkloadDelegationForwarded(t *testing.T) {
	idp := newHRIdP(t)
	svid := idp.svid(t, hrAgentSPIFFEID, idp.srv.URL+hrRealmPath)
	p := hrPluginForwarding(t, idp, &fakeJWT{tok: svid})

	pctx := headcount(t, idp, nil)
	wantContinue(t, "agent get_headcount", p.OnRequest(context.Background(), pctx))
	if got := pctx.Headers.Get("Authorization"); got != "Bearer "+idp.lastIssued(t) {
		t.Fatal("the agent's call does not carry the leg-2 token")
	}
}

// Without an SVID from Cortex the route has no credential: a caller's
// own X-Workload-Token is dropped, a user token does not stand in, and
// an SVID for another audience is refused. No leg runs.
func TestHRDemoWorkloadRequiresSVID(t *testing.T) {
	idp := newHRIdP(t)
	stolen := idp.svid(t, hrAgentSPIFFEID, idp.srv.URL+hrRealmPath)

	noSPIRE := hrPluginWorkload(t, idp, nil)
	for label, pctx := range map[string]*pipeline.Context{
		"caller-supplied SVID": headcount(t, idp, http.Header{"X-Workload-Token": {stolen}}),
		"user token only":      hrCall{user: "bob", tool: "get_headcount", args: map[string]any{}}.pctx(t, idp),
	} {
		if a := noSPIRE.OnRequest(context.Background(), pctx); a.Type != pipeline.Reject || a.Violation.Code == "cpex.delegation_unavailable" {
			t.Fatalf("%s: %v %+v, want an authentication deny", label, a.Type, a.Violation)
		}
	}

	// Nor does policy see the caller's copy.
	_, ext := buildCMF(headcount(t, idp, http.Header{"X-Workload-Token": {stolen}}), false, noSPIRE.manager.(*cpexManager).opts)
	for k := range ext.Http.RequestHeaders {
		if strings.EqualFold(k, "x-workload-token") {
			t.Fatalf("caller's X-Workload-Token reached the http extension")
		}
	}

	wrongAud := hrPluginWorkload(t, idp, &fakeJWT{tok: idp.svid(t, hrAgentSPIFFEID, "some-other-service")})
	if a := wrongAud.OnRequest(context.Background(), headcount(t, idp, nil)); a.Type != pipeline.Reject || a.Violation.Code == "cpex.delegation_unavailable" {
		t.Fatalf("wrong audience: %v %+v, want an authentication deny", a.Type, a.Violation)
	}
	if a, e, _ := idp.legs(); len(a) != 0 || len(e) != 0 {
		t.Fatalf("legs ran: %d assertions, %d exchanges", len(a), len(e))
	}
}
