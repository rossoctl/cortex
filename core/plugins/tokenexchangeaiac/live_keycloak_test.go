package tokenexchangeaiac

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestLiveKeycloak runs the plugin against a real Keycloak, because classify
// matches on Keycloak's error_description text and a reworded message would
// pass every fake-server test while turning passthrough into deny. Run it
// before upgrading Keycloak:
//
//	TOKEN_EXCHANGE_AIAC_LIVE_KEYCLOAK=http://keycloak.localtest.me:8080 \
//	  go test -run TestLiveKeycloak ./plugins/tokenexchangeaiac/
//
// It creates a throwaway realm through the admin API (admin/admin unless
// TOKEN_EXCHANGE_AIAC_LIVE_ADMIN / _PASSWORD say otherwise) and deletes it at
// the end. Requires Keycloak 26.2+ with standard token exchange.
func TestLiveKeycloak(t *testing.T) {
	base := os.Getenv("TOKEN_EXCHANGE_AIAC_LIVE_KEYCLOAK")
	if base == "" {
		t.Skip("TOKEN_EXCHANGE_AIAC_LIVE_KEYCLOAK not set")
	}
	base = strings.TrimSuffix(base, "/")
	adminUser := envOr("TOKEN_EXCHANGE_AIAC_LIVE_ADMIN", "admin")
	adminPass := envOr("TOKEN_EXCHANGE_AIAC_LIVE_PASSWORD", "admin")
	realm := fmt.Sprintf("token-exchange-aiac-test-%d", time.Now().UnixNano())

	kc := &liveAdmin{t: t, base: base, user: adminUser, pass: adminPass}
	kc.do("POST", "", map[string]any{"realm": realm, "enabled": true})
	t.Cleanup(func() { kc.do("DELETE", "/"+realm, nil) })

	r := "/" + realm
	kc.do("POST", r+"/clients", map[string]any{
		"clientId": "agent", "secret": "s3cret", "publicClient": false,
		"serviceAccountsEnabled": true, "directAccessGrantsEnabled": true, "standardFlowEnabled": false,
		"attributes": map[string]string{"standard.token.exchange.enabled": "true"},
	})
	for _, c := range []string{"github-tool", "weather-agent", "hr-tool"} {
		kc.do("POST", r+"/clients", map[string]any{"clientId": c, "publicClient": false, "standardFlowEnabled": false})
	}
	agentID := kc.id(r+"/clients?clientId=agent", "id")
	// Granted = an audience scope that is a DEFAULT scope of the caller.
	// hr-tool exists and is not granted.
	for _, target := range []string{"github-tool", "weather-agent"} {
		kc.do("POST", r+"/client-scopes", map[string]any{
			"name": target + "-aud", "protocol": "openid-connect",
			"protocolMappers": []map[string]any{{
				"name": "aud", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
				"config": map[string]string{"included.client.audience": target, "access.token.claim": "true"},
			}},
		})
		scopeID := kc.idByName(r+"/client-scopes", target+"-aud")
		kc.do("PUT", r+"/clients/"+agentID+"/default-client-scopes/"+scopeID, nil)
	}
	kc.do("POST", r+"/users", map[string]any{
		"username": "alice", "enabled": true, "email": "alice@example.com", "emailVerified": true,
		"firstName": "Alice", "lastName": "Test",
		"credentials": []map[string]any{{"type": "password", "value": "alice", "temporary": false}},
	})

	tokenURL := base + "/realms/" + realm + "/protocol/openid-connect/token"
	userToken := passwordGrant(t, tokenURL)

	p := New()
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id":"agent","client_secret":"s3cret","no_token_policy":"client-credentials","namespace":"team1"}`, tokenURL)
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, host := range []string{"github-tool", "weather-agent.team1.svc.cluster.local"} {
		pctx := outbound(host, "Bearer "+userToken)
		if a := p.OnRequest(ctx, pctx); a.Type != pipeline.Continue {
			t.Fatalf("%s: %v %+v", host, a.Type, a.Violation)
		}
		want := strings.SplitN(host, ".", 2)[0]
		if got := audOf(t, pctx); len(got) != 1 || got[0] != want {
			t.Errorf("%s: aud = %v, want [%s]", host, got, want)
		}
	}

	pctx := outbound("hr-tool", "Bearer "+userToken)
	if a := p.OnRequest(ctx, pctx); a.Type != pipeline.Reject || a.Violation.Status != http.StatusForbidden {
		t.Errorf("hr-tool (exists, not granted): %v %+v, want 403", a.Type, a.Violation)
	}

	pctx = outbound("no-such-client", "Bearer "+userToken)
	if a := p.OnRequest(ctx, pctx); a.Type != pipeline.Continue {
		t.Errorf("unknown audience: %v %+v, want passthrough", a.Type, a.Violation)
	}
	if inv := lastInvocation(t, pctx); inv.Reason != reasonAudienceUnknown {
		t.Errorf("unknown audience reason = %s — has Keycloak reworded \"Audience not found\"?", inv.Reason)
	}

	// Keycloak rejects a subject it did not issue before it looks at the
	// audience. A non-target must still pass through with that bearer, and a
	// target must answer 401.
	pctx = outbound("other-gateway", "Bearer sk-not-a-keycloak-token")
	if a := p.OnRequest(ctx, pctx); a.Type != pipeline.Continue {
		t.Errorf("foreign bearer, unknown audience: %v %+v, want passthrough", a.Type, a.Violation)
	}
	if inv := lastInvocation(t, pctx); inv.Reason != reasonAudienceUnknown {
		t.Errorf("foreign bearer, unknown audience: reason = %s — has Keycloak reworded \"Invalid token\"?", inv.Reason)
	}
	pctx = outbound("github-tool", "Bearer sk-not-a-keycloak-token")
	if a := p.OnRequest(ctx, pctx); a.Type != pipeline.Reject || a.Violation.Status != http.StatusUnauthorized {
		t.Errorf("foreign bearer, target: %v %+v, want 401", a.Type, a.Violation)
	}

	pctx = outbound("github-tool", "")
	if a := p.OnRequest(ctx, pctx); a.Type != pipeline.Continue {
		t.Fatalf("no token: %v %+v", a.Type, a.Violation)
	}
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "github-tool" {
		t.Errorf("no token: aud = %v, want the client-credentials token narrowed to [github-tool]", got)
	}
}

// liveHTTP bounds every setup call, so a stalled Keycloak fails the test
// instead of hanging it until go test's own timeout.
var liveHTTP = &http.Client{Timeout: 30 * time.Second}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func passwordGrant(t *testing.T, tokenURL string) string {
	t.Helper()
	resp, err := liveHTTP.PostForm(tokenURL, url.Values{
		"grant_type": {"password"}, "client_id": {"agent"}, "client_secret": {"s3cret"},
		"username": {"alice"}, "password": {"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("password grant: status %d, %v", resp.StatusCode, err)
	}
	return body.AccessToken
}

// liveAdmin is a minimal Keycloak admin API client for test setup.
type liveAdmin struct {
	t                *testing.T
	base, user, pass string
}

func (a *liveAdmin) token() string {
	resp, err := liveHTTP.PostForm(a.base+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {a.user}, "password": {a.pass},
	})
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		a.t.Fatalf("admin login: status %d, %v", resp.StatusCode, err)
	}
	return body.AccessToken
}

func (a *liveAdmin) request(method, path string, payload any) *http.Response {
	var body bytes.Buffer
	if payload != nil {
		if err := json.NewEncoder(&body).Encode(payload); err != nil {
			a.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, a.base+"/admin/realms"+path, &body)
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := liveHTTP.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	return resp
}

func (a *liveAdmin) do(method, path string, payload any) {
	resp := a.request(method, path, payload)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		a.t.Fatalf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
}

func (a *liveAdmin) list(path string) []map[string]any {
	resp := a.request("GET", path, nil)
	defer resp.Body.Close()
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		a.t.Fatalf("GET %s: %v", path, err)
	}
	return out
}

func (a *liveAdmin) id(path, field string) string {
	items := a.list(path)
	if len(items) != 1 {
		a.t.Fatalf("GET %s: %d results, want 1", path, len(items))
	}
	return items[0][field].(string)
}

func (a *liveAdmin) idByName(path, name string) string {
	for _, it := range a.list(path) {
		if it["name"] == name {
			return it["id"].(string)
		}
	}
	a.t.Fatalf("GET %s: no item named %q", path, name)
	return ""
}
