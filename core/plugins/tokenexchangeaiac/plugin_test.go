package tokenexchangeaiac

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/placeholder"
)

// fakeKeycloak answers the token endpoint the way Keycloak 26.5's standard
// token exchange did in a live probe, for a caller whose audience scopes are
// all default scopes:
//
//   - an audience in granted gets a token narrowed to it
//   - an audience in existing but not granted: 400 invalid_request
//   - any other audience: 400 invalid_client "Audience not found"
//   - client_credentials ignores audience and returns every granted audience
//   - a subject_token it did not issue: 400 invalid_request "Invalid token",
//     whatever the audience, because Keycloak checks the subject first
type fakeKeycloak struct {
	granted  map[string]bool
	existing map[string]bool
	calls    atomic.Int64

	mu       sync.Mutex
	override func(w http.ResponseWriter, form map[string]string) bool
	// subjects are the subject tokens it accepts: the user tokens the tests
	// send, and every token it issued itself.
	subjects map[string]bool
}

func newFakeKeycloak(t *testing.T) (*fakeKeycloak, *httptest.Server) {
	kc := &fakeKeycloak{
		granted:  map[string]bool{"github-tool": true, "weather-agent": true},
		existing: map[string]bool{"github-tool": true, "weather-agent": true, "hr-tool": true},
		subjects: map[string]bool{"user-token": true, " user-token": true, "real-user-token": true},
	}
	srv := httptest.NewServer(kc)
	t.Cleanup(srv.Close)
	return kc, srv
}

func (kc *fakeKeycloak) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kc.calls.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	form := map[string]string{}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	kc.mu.Lock()
	override := kc.override
	kc.mu.Unlock()
	if override != nil && override(w, form) {
		return
	}
	if form["client_id"] != "agent" || form["client_secret"] != "s3cret" {
		oauthError(w, 401, "invalid_client", "Invalid client or Invalid client credentials")
		return
	}
	switch form["grant_type"] {
	case "client_credentials":
		var all []string
		for a := range kc.granted {
			all = append(all, a)
		}
		kc.accept(issue(w, append(all, "account")))
	case grantTokenExchange:
		aud := form["audience"]
		kc.mu.Lock()
		known := kc.subjects[form["subject_token"]]
		kc.mu.Unlock()
		switch {
		case !known:
			oauthError(w, 400, "invalid_request", "Invalid token")
		case aud == "":
			issue(w, []string{"github-tool", "weather-agent", "account"})
		case kc.granted[aud]:
			issueClaims(w, map[string]any{"aud": []string{aud}, "sub": "alice", "scope": "openid github-read"})
		case kc.existing[aud]:
			oauthError(w, 400, "invalid_request", "Requested audience not available: "+aud)
		default:
			oauthError(w, 400, "invalid_client", "Audience not found")
		}
	default:
		oauthError(w, 400, "unsupported_grant_type", "")
	}
}

func (kc *fakeKeycloak) accept(subject string) {
	kc.mu.Lock()
	kc.subjects[subject] = true
	kc.mu.Unlock()
}

func issue(w http.ResponseWriter, aud []string) string {
	return issueClaims(w, map[string]any{"aud": aud})
}

func issueClaims(w http.ResponseWriter, claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	tok := enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".sig"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": 300})
	return tok
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func configured(t *testing.T, srv *httptest.Server, extra string) *TokenExchangeAIAC {
	t.Helper()
	p := New()
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id":"agent","client_secret":"s3cret"%s}`, srv.URL, extra)
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func outbound(host, authz string) *pipeline.Context {
	h := http.Header{}
	if authz != "" {
		h.Set("Authorization", authz)
	}
	return &pipeline.Context{Direction: pipeline.Outbound, Host: host, Headers: h}
}

func lastInvocation(t *testing.T, pctx *pipeline.Context) pipeline.Invocation {
	t.Helper()
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) == 0 {
		t.Fatal("no outbound invocation recorded")
	}
	inv := pctx.Extensions.Invocations.Outbound
	return inv[len(inv)-1]
}

// audOf decodes the aud claim of the token now on the request.
func audOf(t *testing.T, pctx *pipeline.Context) []string {
	t.Helper()
	tok := strings.TrimPrefix(pctx.Headers.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("Authorization is not a JWT: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	// Keycloak encodes a single audience as a string, several as an array.
	var c struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(c.Aud, &many); err != nil {
		t.Fatalf("aud is neither a string nor an array: %s", c.Aud)
	}
	return many
}

func TestGrantedTargetIsExchangedAndCached(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := configured(t, srv, `,"namespace":"team1"`)

	pctx := outbound("github-tool:9090", "Bearer user-token")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %v (%+v), want Continue", a.Type, a.Violation)
	}
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "github-tool" {
		t.Errorf("forwarded token aud = %v, want [github-tool]", got)
	}
	if inv := lastInvocation(t, pctx); inv.Action != pipeline.ActionModify || inv.Reason != reasonExchanged {
		t.Errorf("invocation = %s/%s, want modify/%s", inv.Action, inv.Reason, reasonExchanged)
	}

	again := outbound("github-tool.team1.svc.cluster.local", "Bearer user-token")
	p.OnRequest(context.Background(), again)
	if inv := lastInvocation(t, again); inv.Reason != reasonCacheHit {
		t.Errorf("second request reason = %s, want %s", inv.Reason, reasonCacheHit)
	}
	if n := kc.calls.Load(); n != 1 {
		t.Errorf("IdP calls = %d, want 1 (second request served from cache)", n)
	}
}

// An exchange records a delegation hop, which the opa plugin turns into the
// outbound input.identity; a request that is not exchanged records none.
func TestExchangeRecordsDelegationHop(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")

	for i, wantCache := range []bool{false, true} {
		pctx := outbound("github-tool", "Bearer user-token")
		p.OnRequest(context.Background(), pctx)
		d := pctx.Extensions.Delegation
		if d == nil || len(d.Chain()) != 1 {
			t.Fatalf("request %d: delegation = %+v, want one hop", i, d)
		}
		hop := d.Chain()[0]
		if hop.SubjectID != "alice" || hop.Audience != "github-tool" || hop.Strategy != PluginName || hop.FromCache != wantCache {
			t.Errorf("request %d: hop = %+v", i, hop)
		}
		if strings.Join(hop.Scopes, " ") != "openid github-read" {
			t.Errorf("request %d: scopes = %v", i, hop.Scopes)
		}
		if d.Origin != "alice" || d.Actor != "alice" {
			t.Errorf("request %d: origin/actor = %q/%q", i, d.Origin, d.Actor)
		}
	}

	for _, host := range []string{"ollama", "hr-tool", "api.openai.com"} {
		pctx := outbound(host, "Bearer user-token")
		p.OnRequest(context.Background(), pctx)
		if pctx.Extensions.Delegation != nil {
			t.Errorf("%s: delegation recorded for a request that was not exchanged", host)
		}
	}
}

// An agent is a target like any other: its audience is just another client.
func TestAgentTargetIsExchanged(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")
	pctx := outbound("weather-agent", "Bearer user-token")
	p.OnRequest(context.Background(), pctx)
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "weather-agent" {
		t.Errorf("aud = %v, want [weather-agent]", got)
	}
}

func TestUnknownAudiencePassesThroughAndIsRemembered(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	now := time.Unix(1_000_000, 0)
	p := New()
	p.now = func() time.Time { return now }
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id":"agent","client_secret":"s3cret","unknown_audience_ttl":"1m"}`, srv.URL)
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}

	pctx := outbound("ollama", "Bearer user-token")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %v (%+v), want Continue", a.Type, a.Violation)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer user-token" {
		t.Errorf("Authorization = %q, want it forwarded unchanged", got)
	}
	inv := lastInvocation(t, pctx)
	if inv.Action != pipeline.ActionSkip || inv.Reason != reasonAudienceUnknown || inv.Details["cached"] != "false" {
		t.Errorf("invocation = %s/%s %v", inv.Action, inv.Reason, inv.Details)
	}

	second := outbound("ollama", "Bearer user-token")
	p.OnRequest(context.Background(), second)
	if inv := lastInvocation(t, second); inv.Details["cached"] != "true" {
		t.Errorf("second request not answered from the unknown cache: %v", inv.Details)
	}
	if n := kc.calls.Load(); n != 1 {
		t.Errorf("IdP calls = %d, want 1", n)
	}

	now = now.Add(61 * time.Second)
	p.OnRequest(context.Background(), outbound("ollama", "Bearer user-token"))
	if n := kc.calls.Load(); n != 2 {
		t.Errorf("IdP calls after TTL = %d, want 2 (expired entry asks again)", n)
	}
}

func TestExistingButNotGrantedIsDenied(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")
	pctx := outbound("hr-tool", "Bearer user-token")
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusForbidden {
		t.Fatalf("action = %v %+v, want 403 reject", a.Type, a.Violation)
	}
	if inv := lastInvocation(t, pctx); inv.Action != pipeline.ActionDeny || inv.Reason != reasonNotGranted {
		t.Errorf("invocation = %s/%s", inv.Action, inv.Reason)
	}
}

func TestDisabledTargetIsForbidden(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	kc.override = func(w http.ResponseWriter, _ map[string]string) bool {
		oauthError(w, 400, "invalid_client", "Client disabled")
		return true
	}
	p := configured(t, srv, "")
	pctx := outbound("github-tool", "Bearer user-token")
	a := p.OnRequest(context.Background(), pctx)
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusForbidden || a.Violation.Code != "policy.forbidden" {
		t.Fatalf("action = %v %+v, want 403 policy.forbidden reject", a.Type, a.Violation)
	}
	if inv := lastInvocation(t, pctx); inv.Action != pipeline.ActionDeny || inv.Reason != reasonTargetDisabled {
		t.Errorf("invocation = %s/%s", inv.Action, inv.Reason)
	}
}

func TestIdPFailuresDenyNeverPassThrough(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, form map[string]string) bool{
		"server error": func(w http.ResponseWriter, _ map[string]string) bool {
			oauthError(w, 500, "", "")
			return true
		},
		"non-JSON error": func(w http.ResponseWriter, _ map[string]string) bool {
			w.WriteHeader(400)
			_, _ = w.Write([]byte("<html>Audience not found</html>"))
			return true
		},
		// The IdP answers 200 with a token for some other audience.
		"wrong audience issued": func(w http.ResponseWriter, _ map[string]string) bool {
			issue(w, []string{"account", "weather-agent"})
			return true
		},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			kc, srv := newFakeKeycloak(t)
			kc.override = override
			p := configured(t, srv, "")
			pctx := outbound("github-tool", "Bearer user-token")
			a := p.OnRequest(context.Background(), pctx)
			if a.Type != pipeline.Reject || a.Violation.Status != http.StatusServiceUnavailable {
				t.Fatalf("action = %v %+v, want 503 reject", a.Type, a.Violation)
			}
			if got := pctx.Headers.Get("Authorization"); got != "Bearer user-token" {
				t.Errorf("Authorization changed to %q on a denied request", got)
			}
		})
	}
}

func TestIdPUnreachableDenies(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")
	srv.Close()
	a := p.OnRequest(context.Background(), outbound("github-tool", "Bearer user-token"))
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusServiceUnavailable {
		t.Fatalf("action = %v %+v, want 503 reject", a.Type, a.Violation)
	}
}

func TestHostsOutsideTheClusterNeverReachTheIdP(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := configured(t, srv, "")
	for _, host := range []string{"api.openai.com", "github-tool.evil.example", "10.1.2.3:443"} {
		pctx := outbound(host, "Bearer user-token")
		if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
			t.Errorf("%s: action = %v, want Continue", host, a.Type)
		}
		if got := pctx.Headers.Get("Authorization"); got != "Bearer user-token" {
			t.Errorf("%s: Authorization = %q, want unchanged", host, got)
		}
		if inv := lastInvocation(t, pctx); inv.Reason != reasonHostNotInCluster {
			t.Errorf("%s: reason = %s", host, inv.Reason)
		}
	}
	if n := kc.calls.Load(); n != 0 {
		t.Errorf("IdP calls = %d, want 0", n)
	}
}

func TestNoTokenClientCredentialsIsNarrowed(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	var mu sync.Mutex
	var grants []string
	kc.override = func(_ http.ResponseWriter, form map[string]string) bool {
		mu.Lock()
		defer mu.Unlock()
		grants = append(grants, form["grant_type"])
		return false
	}
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), grants...)
	}
	p := configured(t, srv, `,"no_token_policy":"client-credentials"`)

	pctx := outbound("github-tool", "")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %v %+v", a.Type, a.Violation)
	}
	// Keycloak's client_credentials token carries every granted audience; the
	// one forwarded must carry only the destination's.
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "github-tool" {
		t.Errorf("forwarded aud = %v, want [github-tool]", got)
	}
	if g := seen(); len(g) != 2 || g[0] != "client_credentials" || g[1] != grantTokenExchange {
		t.Errorf("grants = %v, want client_credentials then token-exchange", g)
	}

	// The workload's own token is cached; a second target costs one exchange.
	p.OnRequest(context.Background(), outbound("weather-agent", ""))
	if g := seen(); len(g) != 3 {
		t.Errorf("grants after second target = %v, want one more exchange only", g)
	}

	// Discovery works without a user token too.
	unknown := outbound("ollama", "")
	p.OnRequest(context.Background(), unknown)
	if inv := lastInvocation(t, unknown); inv.Reason != reasonAudienceUnknown {
		t.Errorf("no-token unknown host reason = %s", inv.Reason)
	}
	if got := unknown.Headers.Get("Authorization"); got != "" {
		t.Errorf("unknown host got Authorization %q; the workload's own token must not leak", got)
	}
}

func TestNoTokenPolicies(t *testing.T) {
	kc, srv := newFakeKeycloak(t)

	allow := configured(t, srv, `,"no_token_policy":"allow"`)
	pctx := outbound("github-tool", "")
	if a := allow.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Errorf("allow: action = %v", a.Type)
	}
	if pctx.Headers.Get("Authorization") != "" {
		t.Error("allow added an Authorization header")
	}
	if inv := lastInvocation(t, pctx); inv.Reason != reasonNoToken {
		t.Errorf("allow: reason = %s, want %s", inv.Reason, reasonNoToken)
	}

	if n := kc.calls.Load(); n != 0 {
		t.Errorf("allow: IdP calls = %d, want 0", n)
	}

	// deny is the default, as in token-exchange. It denies a target only: a
	// destination the IdP has no client for still passes through, as it does
	// with a bearer, and is never handed the workload's own token.
	for name, extra := range map[string]string{"explicit": `,"no_token_policy":"deny"`, "default": ""} {
		deny := configured(t, srv, extra)
		target := outbound("github-tool", "")
		a := deny.OnRequest(context.Background(), target)
		if a.Type != pipeline.Reject || a.Violation.Status != http.StatusUnauthorized ||
			a.Violation.Code != "auth.unauthorized" || a.Violation.Reason != "missing Authorization header" {
			t.Errorf("deny (%s): action = %v %+v, want 401 auth.unauthorized", name, a.Type, a.Violation)
		}
		if got := target.Headers.Get("Authorization"); got != "" {
			t.Errorf("deny (%s): denied request got Authorization %q", name, got)
		}
		if inv := lastInvocation(t, target); inv.Reason != reasonNoToken {
			t.Errorf("deny (%s): reason = %s, want %s", name, inv.Reason, reasonNoToken)
		}
		// A second request is answered from the cache, still as a 401.
		if a := deny.OnRequest(context.Background(), outbound("github-tool", "")); a.Type != pipeline.Reject ||
			a.Violation.Status != http.StatusUnauthorized {
			t.Errorf("deny (%s), cached: action = %v %+v, want 401", name, a.Type, a.Violation)
		}

		collector := outbound("otel-collector", "")
		if a := deny.OnRequest(context.Background(), collector); a.Type != pipeline.Continue {
			t.Errorf("deny (%s): non-target denied: %+v", name, a.Violation)
		}
		if got := collector.Headers.Get("Authorization"); got != "" {
			t.Errorf("deny (%s): non-target got Authorization %q", name, got)
		}
		if inv := lastInvocation(t, collector); inv.Reason != reasonAudienceUnknown {
			t.Errorf("deny (%s): non-target reason = %s, want %s", name, inv.Reason, reasonAudienceUnknown)
		}
	}
}

// A CONNECT tunnel, or a transparently redirected connection, carries opaque
// bytes: nothing the plugin set would reach the destination, so it is never
// gated, exchanged or sent to the IdP.
func TestOpaqueTunnelIsSkipped(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	for _, policy := range []string{"deny", "client-credentials"} {
		p := configured(t, srv, `,"no_token_policy":"`+policy+`"`)
		for _, host := range []string{"github-tool.team1.svc:443", "hr-tool:443"} {
			pctx := outbound(host, "")
			pctx.Method = http.MethodConnect
			pctx.Scheme = "tcp"
			if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
				t.Errorf("%s %s: action = %v %+v, want Continue", policy, host, a.Type, a.Violation)
			}
			if got := pctx.Headers.Get("Authorization"); got != "" {
				t.Errorf("%s %s: tunnel got Authorization %q", policy, host, got)
			}
			if inv := lastInvocation(t, pctx); inv.Reason != reasonOpaqueTunnel {
				t.Errorf("%s %s: reason = %s, want %s", policy, host, inv.Reason, reasonOpaqueTunnel)
			}
		}
	}
	if n := kc.calls.Load(); n != 0 {
		t.Errorf("IdP calls = %d, want 0", n)
	}
}

// subjectSeen records the subject_token of every token exchange.
func subjectSeen(kc *fakeKeycloak) func() []string {
	var mu sync.Mutex
	var subjects []string
	kc.override = func(_ http.ResponseWriter, form map[string]string) bool {
		if form["grant_type"] == grantTokenExchange {
			mu.Lock()
			subjects = append(subjects, form["subject_token"])
			mu.Unlock()
		}
		return false
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), subjects...)
	}
}

func withPlaceholder(t *testing.T, pctx *pipeline.Context, real string) string {
	t.Helper()
	st := memstore.New()
	handle, err := placeholder.New()
	if err != nil {
		t.Fatal(err)
	}
	st.Put(placeholder.Key(handle), real, time.Hour)
	pctx.Shared = st
	pctx.Headers.Set("Authorization", "Bearer "+handle)
	return handle
}

func TestPlaceholderIsResolvedBeforeExchange(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	subjects := subjectSeen(kc)
	p := configured(t, srv, `,"resolve_placeholders":true`)

	pctx := outbound("github-tool", "")
	withPlaceholder(t, pctx, "real-user-token")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %v %+v", a.Type, a.Violation)
	}
	if got := subjects(); len(got) != 1 || got[0] != "real-user-token" {
		t.Errorf("subject tokens = %v, want [real-user-token]", got)
	}
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "github-tool" {
		t.Errorf("forwarded aud = %v, want [github-tool]", got)
	}
}

func TestPlaceholderIsNotResolvedByDefault(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	subjects := subjectSeen(kc)
	p := configured(t, srv, "")

	pctx := outbound("github-tool", "")
	handle := withPlaceholder(t, pctx, "real-user-token")
	p.OnRequest(context.Background(), pctx)
	// The IdP rejects the handle as a subject, so the plugin asks again with
	// its own token to learn whether github-tool is a target.
	if got := subjects(); len(got) == 0 || got[0] != handle {
		t.Errorf("subject tokens = %v, want the handle sent as is first", got)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer "+handle {
		t.Errorf("Authorization = %q, want the handle left in place", got)
	}
}

func TestUnresolvablePlaceholderIsDeniedForEveryHost(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := configured(t, srv, `,"resolve_placeholders":true`)
	handle, _ := placeholder.New()

	for _, host := range []string{"github-tool", "api.openai.com"} {
		for name, shared := range map[string]bool{"no store": false, "unknown handle": true} {
			pctx := outbound(host, "Bearer "+handle)
			if shared {
				pctx.Shared = memstore.New()
			}
			a := p.OnRequest(context.Background(), pctx)
			if a.Type != pipeline.Reject || a.Violation.Code != "auth.unauthorized" {
				t.Errorf("%s, %s: action = %v %+v, want auth.unauthorized", host, name, a.Type, a.Violation)
				continue
			}
			if inv := lastInvocation(t, pctx); inv.Reason != reasonPlaceholderUnresolved {
				t.Errorf("%s, %s: reason = %s", host, name, inv.Reason)
			}
		}
	}
	if n := kc.calls.Load(); n != 0 {
		t.Errorf("IdP calls = %d, want 0", n)
	}
}

func TestPassedThroughRequestKeepsThePlaceholder(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := configured(t, srv, `,"resolve_placeholders":true`)

	for _, host := range []string{"ollama", "api.openai.com"} {
		pctx := outbound(host, "")
		handle := withPlaceholder(t, pctx, "real-user-token")
		if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
			t.Fatalf("%s: action = %v %+v", host, a.Type, a.Violation)
		}
		if got := pctx.Headers.Get("Authorization"); got != "Bearer "+handle {
			t.Errorf("%s: Authorization = %q, want the placeholder, never the real token", host, got)
		}
	}
}

// The bearer is read with auth.ExtractBearer, as in token-exchange: the
// token is everything after "Bearer ", spaces included.
func TestBearerIsTakenVerbatim(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	subjects := subjectSeen(kc)
	p := configured(t, srv, "")

	p.OnRequest(context.Background(), outbound("github-tool", "bearer  user-token"))
	if got := subjects(); len(got) != 1 || got[0] != " user-token" {
		t.Errorf("subject tokens = %q, want [\" user-token\"]", got)
	}
}

func TestDeniesWhileCredentialsAreMissing(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	p := New()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id_file":%q,"client_secret_file":%q}`,
		srv.URL, dir+"/id", dir+"/secret")
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	if p.Ready() {
		t.Fatal("Ready() before credentials exist")
	}
	a := p.OnRequest(context.Background(), outbound("github-tool", "Bearer user-token"))
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusServiceUnavailable {
		t.Errorf("action = %v %+v, want 503", a.Type, a.Violation)
	}
	// A non-target is unaffected by missing credentials.
	if a := p.OnRequest(context.Background(), outbound("api.openai.com", "")); a.Type != pipeline.Continue {
		t.Errorf("non-target denied while not ready: %+v", a.Violation)
	}
}

// no_token_policy: allow forwards a bearer-less request whatever the
// destination, so it does not wait for credentials it would never use.
func TestAllowNeedsNoCredentials(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := New()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id_file":%q,"client_secret_file":%q,"no_token_policy":"allow"}`,
		srv.URL, dir+"/id", dir+"/secret")
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	pctx := outbound("github-tool", "")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Errorf("action = %v %+v, want Continue", a.Type, a.Violation)
	}
	if inv := lastInvocation(t, pctx); inv.Reason != reasonNoToken {
		t.Errorf("reason = %s, want %s", inv.Reason, reasonNoToken)
	}
	if n := kc.calls.Load(); n != 0 {
		t.Errorf("IdP calls = %d, want 0", n)
	}
}

func TestConfigure(t *testing.T) {
	cases := map[string]string{
		"no token url":   `{"client_id":"a","client_secret":"b"}`,
		"bad policy":     `{"token_url":"http://kc","no_token_policy":"exchange"}`,
		"passthrough":    `{"token_url":"http://kc","no_token_policy":"passthrough"}`,
		"bad template":   `{"token_url":"http://kc","audience_template":"fixed"}`,
		"bad ttl":        `{"token_url":"http://kc","unknown_audience_ttl":"-1s"}`,
		"bad timeout":    `{"token_url":"http://kc","timeout":"0s"}`,
		"unknown field":  `{"token_url":"http://kc","routes":{"file":"/etc/authproxy/routes.yaml"}}`,
		"token scopes":   `{"token_url":"http://kc","token_scopes":"openid"}`,
		"empty cluster":  `{"token_url":"http://kc","cluster_domain":"."}`,
		"bad namespace":  `{"token_url":"http://kc","namespace":"Team1"}`,
		"malformed json": `{`,
	}
	for name, cfg := range cases {
		if err := New().Configure(json.RawMessage(cfg)); err == nil {
			t.Errorf("%s: Configure accepted %s", name, cfg)
		}
	}

	p := New()
	if err := p.Configure(json.RawMessage(`{"keycloak_url":"http://kc:8080/","keycloak_realm":"rossoctl","client_id":"a","client_secret":"b"}`)); err != nil {
		t.Fatal(err)
	}
	if want := "http://kc:8080/realms/rossoctl/protocol/openid-connect/token"; p.cfg.TokenURL != want {
		t.Errorf("derived token_url = %q, want %q", p.cfg.TokenURL, want)
	}
	if !p.Ready() {
		t.Error("inline credentials did not make the plugin ready")
	}
}

func TestNamespaceFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	base := `"token_url":"http://kc","client_id":"a","client_secret":"b","audience_template":"spiffe://td/ns/{namespace}/sa/{service}"`

	p := New()
	cfg := fmt.Sprintf(`{%s,"namespace_file":%q}`, base, write("ns", "team2\n"))
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	if got, why := p.targets.resolve("github-tool:9090"); why != "" || got.audience != "spiffe://td/ns/team2/sa/github-tool" {
		t.Errorf("short name with namespace_file: %+v %q, want the team2 audience", got, why)
	}

	// An inline namespace wins, and the file is then not read at all.
	p = New()
	cfg = fmt.Sprintf(`{%s,"namespace":"team1","namespace_file":%q}`, base, dir+"/absent")
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	if p.cfg.Namespace != "team1" {
		t.Errorf("namespace = %q, want the inline team1", p.cfg.Namespace)
	}

	for name, path := range map[string]string{
		"absent":    dir + "/absent",
		"blank":     write("blank", "\n"),
		"not label": write("bad", "Team_1"),
	} {
		cfg := fmt.Sprintf(`{%s,"namespace_file":%q}`, base, path)
		if err := New().Configure(json.RawMessage(cfg)); err == nil {
			t.Errorf("%s: Configure accepted namespace_file %s", name, path)
		}
	}
}

func TestUnconfiguredDenies(t *testing.T) {
	a := New().OnRequest(context.Background(), outbound("github-tool", "Bearer x"))
	if a.Type != pipeline.Reject {
		t.Errorf("unconfigured plugin let a request through")
	}
}

func TestInitPicksUpCredentialFilesThatArriveLater(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	dir := t.TempDir()
	p := New()
	cfg := fmt.Sprintf(`{"token_url":%q,"client_id_file":%q,"client_secret_file":%q}`,
		srv.URL, dir+"/id", dir+"/secret")
	if err := p.Configure(json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	if err := os.WriteFile(dir+"/id", []byte("agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/secret", []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !p.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("credentials written to disk were never loaded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	pctx := outbound("github-tool", "Bearer user-token")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("after load: %v %+v", a.Type, a.Violation)
	}
}

// Keycloak rejects a subject it did not issue, or one that has expired, with
// "Invalid token" before it looks at the audience, so that answer cannot tell a
// target from a stranger. The plugin asks again with its own token: a
// destination the IdP has no client for still passes through, bearer intact,
// and is remembered; a target answers 401 rather than 503.
func TestInvalidBearerIsJudgedByTheDestination(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := configured(t, srv, `,"namespace":"team1"`)

	gateway := outbound("litellm.team1.svc", "Bearer sk-not-a-keycloak-token")
	if a := p.OnRequest(context.Background(), gateway); a.Type != pipeline.Continue {
		t.Fatalf("non-target: action = %v %+v, want Continue", a.Type, a.Violation)
	}
	if got := gateway.Headers.Get("Authorization"); got != "Bearer sk-not-a-keycloak-token" {
		t.Errorf("non-target: Authorization = %q, want it forwarded unchanged", got)
	}
	if inv := lastInvocation(t, gateway); inv.Reason != reasonAudienceUnknown {
		t.Errorf("non-target: reason = %s, want %s", inv.Reason, reasonAudienceUnknown)
	}
	calls := kc.calls.Load()
	p.OnRequest(context.Background(), outbound("litellm", "Bearer sk-not-a-keycloak-token"))
	if n := kc.calls.Load(); n != calls {
		t.Errorf("IdP calls rose from %d to %d; the unknown destination was not remembered", calls, n)
	}

	target := outbound("github-tool", "Bearer expired-user-token")
	a := p.OnRequest(context.Background(), target)
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusUnauthorized || a.Violation.Code != "auth.unauthorized" {
		t.Fatalf("target: action = %v %+v, want 401 auth.unauthorized", a.Type, a.Violation)
	}
	if inv := lastInvocation(t, target); inv.Action != pipeline.ActionDeny || inv.Reason != reasonSubjectInvalid {
		t.Errorf("target: invocation = %s/%s, want deny/%s", inv.Action, inv.Reason, reasonSubjectInvalid)
	}
	if got := target.Headers.Get("Authorization"); got != "Bearer expired-user-token" {
		t.Errorf("target: Authorization changed to %q on a denied request", got)
	}

	notGranted := outbound("hr-tool", "Bearer expired-user-token")
	if a := p.OnRequest(context.Background(), notGranted); a.Type != pipeline.Reject || a.Violation.Status != http.StatusForbidden {
		t.Errorf("not granted: action = %v %+v, want 403", a.Type, a.Violation)
	}
}

// The workload's own token is the one the re-ask relies on; if the IdP rejects
// that too, nothing is known about the destination and the request is denied.
func TestInvalidSelfTokenStillDenies(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	kc.override = func(w http.ResponseWriter, form map[string]string) bool {
		if form["grant_type"] == grantTokenExchange {
			oauthError(w, 400, "invalid_request", "Invalid token")
			return true
		}
		return false
	}
	p := configured(t, srv, "")
	a := p.OnRequest(context.Background(), outbound("litellm", "Bearer sk-key"))
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusServiceUnavailable {
		t.Errorf("action = %v %+v, want 503", a.Type, a.Violation)
	}
}

// A cached own token the IdP stops accepting (a Keycloak restart, a realm key
// rotation) is fetched again rather than failing every request until it would
// have expired.
func TestStaleSelfTokenIsRefetched(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	p := configured(t, srv, `,"no_token_policy":"client-credentials"`)
	if a := p.OnRequest(context.Background(), outbound("github-tool", "")); a.Type != pipeline.Continue {
		t.Fatalf("first request: %v %+v", a.Type, a.Violation)
	}

	// Keycloak forgets every token it issued.
	kc.mu.Lock()
	kc.subjects = map[string]bool{}
	kc.mu.Unlock()

	pctx := outbound("weather-agent", "")
	if a := p.OnRequest(context.Background(), pctx); a.Type != pipeline.Continue {
		t.Fatalf("after the IdP forgot the cached own token: %v %+v, want Continue", a.Type, a.Violation)
	}
	if got := audOf(t, pctx); len(got) != 1 || got[0] != "weather-agent" {
		t.Errorf("aud = %v, want [weather-agent]", got)
	}
}

// Tests never read this machine's service-account mount: a CI runner in a pod
// has one, and its namespace would leak into every config that sets none.
func TestMain(m *testing.M) {
	serviceAccountNamespaceFile = "/nonexistent/serviceaccount/namespace"
	os.Exit(m.Run())
}

// A host in another namespace, under a template without {namespace}, is never
// exchanged. It passes through when its name is no client's, and is denied —
// not forwarded with the caller's bearer — when it shares a target's name.
func TestForeignNamespaceIsJudgedNotExchanged(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	subjects := subjectSeen(kc)
	p := configured(t, srv, `,"namespace":"team1"`)

	for _, host := range []string{"github-tool.attacker.svc", "hr-tool.attacker.svc.cluster.local"} {
		pctx := outbound(host, "Bearer user-token")
		a := p.OnRequest(context.Background(), pctx)
		if a.Type != pipeline.Reject || a.Violation.Status != http.StatusForbidden || a.Violation.Code != "policy.forbidden" {
			t.Errorf("%s: action = %v %+v, want 403 policy.forbidden", host, a.Type, a.Violation)
		}
		if inv := lastInvocation(t, pctx); inv.Action != pipeline.ActionDeny || inv.Reason != reasonForeignNamespace {
			t.Errorf("%s: invocation = %s/%s, want deny/%s", host, inv.Action, inv.Reason, reasonForeignNamespace)
		}
		if pctx.Extensions.Delegation != nil {
			t.Errorf("%s: a foreign host was exchanged", host)
		}
	}
	for _, s := range subjects() {
		if s == "user-token" {
			t.Error("the caller's bearer was sent to the IdP for a foreign host")
		}
	}

	gateway := outbound("litellm.models.svc", "Bearer sk-key")
	if a := p.OnRequest(context.Background(), gateway); a.Type != pipeline.Continue {
		t.Fatalf("foreign non-target: action = %v %+v, want Continue", a.Type, a.Violation)
	}
	if got := gateway.Headers.Get("Authorization"); got != "Bearer sk-key" {
		t.Errorf("foreign non-target: Authorization = %q, want unchanged", got)
	}
	if inv := lastInvocation(t, gateway); inv.Reason != reasonAudienceUnknown {
		t.Errorf("foreign non-target: reason = %s, want %s", inv.Reason, reasonAudienceUnknown)
	}

	// Bearer-less under allow: nothing to leak, and allow promises no IdP call.
	calls := kc.calls.Load()
	allow := configured(t, srv, `,"namespace":"team1","no_token_policy":"allow"`)
	if a := allow.OnRequest(context.Background(), outbound("github-tool.attacker.svc", "")); a.Type != pipeline.Continue {
		t.Errorf("allow, no bearer: action = %v %+v, want Continue", a.Type, a.Violation)
	}
	if n := kc.calls.Load(); n != calls {
		t.Errorf("allow, no bearer: IdP calls rose from %d to %d", calls, n)
	}
}

// Neither namespace nor namespace_file set: the service-account file is read
// when it exists, and ignored when it does not.
func TestServiceAccountNamespaceIsTheDefault(t *testing.T) {
	_, srv := newFakeKeycloak(t)
	saved := serviceAccountNamespaceFile
	t.Cleanup(func() { serviceAccountNamespaceFile = saved })

	path := t.TempDir() + "/namespace"
	if err := os.WriteFile(path, []byte("team3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceAccountNamespaceFile = path
	if p := configured(t, srv, ""); p.cfg.Namespace != "team3" {
		t.Errorf("namespace = %q, want team3 from the service-account file", p.cfg.Namespace)
	}
	if p := configured(t, srv, `,"namespace":"team1"`); p.cfg.Namespace != "team1" {
		t.Errorf("namespace = %q, want the inline team1", p.cfg.Namespace)
	}

	serviceAccountNamespaceFile = path + ".absent"
	if p := configured(t, srv, ""); p.cfg.Namespace != "" {
		t.Errorf("namespace = %q, want none without the file", p.cfg.Namespace)
	}
}

// The stale own-token retry runs once. Here every own token is rejected after
// it is cached and the cache always holds one, as when concurrent requests
// keep refilling it between the drop and the retry; the request must end in a
// bounded number of IdP calls rather than loop.
func TestStaleSelfTokenRetryIsBounded(t *testing.T) {
	kc, srv := newFakeKeycloak(t)
	kc.override = func(w http.ResponseWriter, form map[string]string) bool {
		if form["grant_type"] == grantTokenExchange {
			oauthError(w, 400, "invalid_request", "Invalid token")
			return true
		}
		return false
	}
	p := configured(t, srv, `,"no_token_policy":"client-credentials"`)
	refill := func() {
		p.tokens.put(selfTokenKey, issuedToken{accessToken: "stale-self", expiresIn: time.Hour})
	}
	refill()
	calls := kc.calls.Load()

	// A cache that always answers stands in for the racing writers.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				refill()
			}
		}
	}()
	a := p.OnRequest(context.Background(), outbound("github-tool", ""))
	close(done)
	if a.Type != pipeline.Reject || a.Violation.Status != http.StatusServiceUnavailable {
		t.Errorf("action = %v %+v, want 503", a.Type, a.Violation)
	}
	// At most: exchange with the stale token, client_credentials, exchange again.
	if n := kc.calls.Load() - calls; n > 3 {
		t.Errorf("IdP calls = %d, want at most 3: the retry is not bounded", n)
	}
}
