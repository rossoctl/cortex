package inferencerouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/memstore"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
)

const (
	claudeUA   = "claude-cli/2.1.286 (external, cli)"
	opencodeUA = "opencode/latest/2.0.21/cli"

	eteHost = "ete.example.com"
	glmHost = "glm.example.com:8443"
)

// routerConfig is two servers and the agents block given, which may be empty.
func routerConfig(agents string) string {
	return `{
		"servers": {
			"ete": {"url": "https://ete.example.com", "key": "ete-key", "main": "opus", "helper": "haiku"},
			"glm": {"url": "https://glm.example.com:8443", "key": "glm-key", "main": "opus", "helper": "haiku"}
		},
		"agents": {` + agents + `}
	}`
}

// build configures a router and wraps it in a one-plugin pipeline. A redirect is
// accepted only from inside Pipeline.Run, so OnRequest is never called directly.
func build(t *testing.T, config string, opts ...pipeline.Option) *pipeline.Pipeline {
	t.Helper()
	r := New()
	if err := r.Configure(json.RawMessage(config)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	p, err := pipeline.New([]pipeline.Plugin{r}, opts...)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

func newStore(t *testing.T) *memstore.Store {
	t.Helper()
	s := memstore.New()
	t.Cleanup(s.Close)
	return s
}

// request is a context the forward proxy would build for a decrypted request to
// host from an agent with User-Agent ua, in session (none when ""), carrying the
// client's own key as a bearer token, that inference-parser read as inference.
func request(store pipeline.SharedStore, host, ua, session string) *pipeline.Context {
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    http.MethodPost,
		Scheme:    "https",
		Host:      host,
		Path:      "/v1/messages",
		Headers:   http.Header{"User-Agent": {ua}, "Authorization": {"Bearer client-key"}},
	}
	if store != nil {
		pctx.Shared = store
	}
	if session != "" {
		pctx.Session = &pipeline.SessionView{ID: session}
	}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{}
	pctx.ResolveClient()
	pctx.MarkRedirectable()
	return pctx
}

func run(t *testing.T, p *pipeline.Pipeline, pctx *pipeline.Context) pipeline.Action {
	t.Helper()
	return p.Run(context.Background(), pctx)
}

// routerRecord is the router's own invocation: the last one on the outbound pass,
// after the framework's modify/redirected when there was a redirect.
func routerRecord(t *testing.T, pctx *pipeline.Context) pipeline.Invocation {
	t.Helper()
	if pctx.Extensions.Invocations == nil || len(pctx.Extensions.Invocations.Outbound) == 0 {
		t.Fatal("the router recorded nothing")
	}
	invs := pctx.Extensions.Invocations.Outbound
	return invs[len(invs)-1]
}

func assertRecord(t *testing.T, pctx *pipeline.Context, action pipeline.InvocationAction, reason string, details map[string]string) {
	t.Helper()
	inv := routerRecord(t, pctx)
	if inv.Plugin != Name || inv.Action != action || inv.Reason != reason {
		t.Errorf("record = %s %s/%s, want %s %s/%s", inv.Plugin, inv.Action, inv.Reason, Name, action, reason)
	}
	for k, want := range details {
		if got := inv.Details[k]; got != want {
			t.Errorf("Details[%q] = %q, want %q (all: %v)", k, got, want, inv.Details)
		}
	}
}

// assertUntouched fails unless pctx still goes to host with the client's own key.
func assertUntouched(t *testing.T, pctx *pipeline.Context, host string) {
	t.Helper()
	if pctx.Host != host || pctx.Redirected() {
		t.Errorf("Host = %q, Redirected = %v; want %q and no redirect", pctx.Host, pctx.Redirected(), host)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer client-key" {
		t.Errorf("Authorization = %q, want the client's own key", got)
	}
}

// assertRouted fails unless pctx goes to host with key as its bearer token.
func assertRouted(t *testing.T, pctx *pipeline.Context, host, key string) {
	t.Helper()
	if pctx.Host != host {
		t.Errorf("Host = %q, want %q", pctx.Host, host)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer "+key {
		t.Errorf("Authorization = %q, want Bearer %s", got, key)
	}
}

// pinOf is the server session is pinned to, "" for not routed, and whether it is
// pinned at all. A pin is its agent's, and every pin these tests make is
// claude-code's but where a test checks the agent itself.
func pinOf(t *testing.T, store *memstore.Store, session string) (string, bool) {
	t.Helper()
	v, ok := store.Get(pinPrefix + session)
	if !ok {
		return "", false
	}
	pn, ok := v.(pin)
	if !ok {
		t.Fatalf("pin for %s is a %T, want a pin", session, v)
	}
	return pn.server, true
}

// A request to a host that is no server, and that no parser read as inference, is
// not the router's: it goes where the agent sent it. (An inference request there is
// captured; see models_test.go.)
func TestRouter_IgnoresAHostThatIsNoServer(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, "api.anthropic.com", claudeUA, "s1")
	pctx.Extensions.Inference = nil
	run(t, p, pctx)

	assertUntouched(t, pctx, "api.anthropic.com")
	assertRecord(t, pctx, pipeline.ActionSkip, "not_an_inference_server", nil)
	if _, ok := pinOf(t, store, "s1"); ok {
		t.Error("a request to no server pinned its session")
	}
}

// A CONNECT is dialed where the client chose. Pinning on it would pin a session on
// a request that cannot be routed.
func TestRouter_IgnoresAContextTheListenerCannotRedirect(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound, Method: http.MethodConnect, Scheme: "tcp", Host: eteHost + ":443",
		Headers: http.Header{"User-Agent": {claudeUA}}, Shared: store, Session: &pipeline.SessionView{ID: "s1"},
	}
	run(t, p, pctx)

	assertRecord(t, pctx, pipeline.ActionSkip, "not_redirectable", nil)
	if _, ok := pinOf(t, store, "s1"); ok {
		t.Error("a CONNECT pinned its session")
	}
}

func TestRouter_LeavesAnUnlistedAgentAloneAndPinsItAsNotRouted(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, opencodeUA, "s1")
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinNew})
	if pin, ok := pinOf(t, store, "s1"); !ok || pin != "" {
		t.Errorf("pin = %q, %v; want the session pinned as not routed (\"\")", pin, ok)
	}
}

func TestRouter_DoesNotRouteARequestWithNoUserAgent(t *testing.T) {
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(newStore(t), eteHost, "", "s1")
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", nil)
}

func TestRouter_RoutesAListedAgentsSessionToItsServer(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	if a := run(t, p, pctx); a.Type != pipeline.Continue {
		t.Fatalf("action = %+v, want Continue", a)
	}

	assertRouted(t, pctx, glmHost, "glm-key")
	if !pctx.Redirected() || pctx.RequestedHost() != eteHost {
		t.Errorf("Redirected = %v, RequestedHost = %q; want a redirect from %s", pctx.Redirected(), pctx.RequestedHost(), eteHost)
	}
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
	if pin, _ := pinOf(t, store, "s1"); pin != "glm" {
		t.Errorf("pin = %q, want glm", pin)
	}
}

// Every path on a server's host belongs to the session's server, not only the
// inference call.
func TestRouter_RoutesEveryPathOnAServersHost(t *testing.T) {
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(newStore(t), eteHost, claudeUA, "s1")
	pctx.Method, pctx.Path, pctx.Extensions.Inference = http.MethodGet, "/v1/models", nil
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
}

func TestRouter_PutsTheKeyInTheHeaderTheClientSent(t *testing.T) {
	for _, tc := range []struct {
		name               string
		sent               http.Header
		wantAPIKey, wantAu string
	}{
		{"x-api-key only", http.Header{"X-Api-Key": {"client-key"}}, "glm-key", ""},
		{"authorization only", http.Header{"Authorization": {"Bearer client-key"}}, "", "Bearer glm-key"},
		{"both", http.Header{"X-Api-Key": {"client-key"}, "Authorization": {"Bearer client-key"}}, "glm-key", "Bearer glm-key"},
		{"neither", http.Header{}, "", "Bearer glm-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, routerConfig(`"claude-code": "glm"`))
			pctx := request(newStore(t), eteHost, claudeUA, "s1")
			pctx.Headers = tc.sent
			pctx.Headers.Set("User-Agent", claudeUA)
			run(t, p, pctx)

			if got := pctx.Headers.Get("X-Api-Key"); got != tc.wantAPIKey {
				t.Errorf("X-Api-Key = %q, want %q", got, tc.wantAPIKey)
			}
			if got := pctx.Headers.Get("Authorization"); got != tc.wantAu {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAu)
			}
		})
	}
}

// The switching story rests on these two: a choice made now applies only to
// sessions that have not started.
func TestRouter_RoutingAnAgentLaterDoesNotMoveASessionPinnedAsNotRouted(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(``))
	run(t, before, request(store, eteHost, claudeUA, "running"))

	after := build(t, routerConfig(`"claude-code": "glm"`))
	running := request(store, eteHost, claudeUA, "running")
	run(t, after, running)
	assertUntouched(t, running, eteHost)
	assertRecord(t, running, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinExisting})

	fresh := request(store, eteHost, claudeUA, "fresh")
	run(t, after, fresh)
	assertRouted(t, fresh, glmHost, "glm-key")
}

func TestRouter_MovingAnAgentDoesNotMoveASessionPinnedToTheFirstServer(t *testing.T) {
	store := newStore(t)
	before := build(t, routerConfig(`"claude-code": "glm"`))
	run(t, before, request(store, eteHost, claudeUA, "running"))

	after := build(t, routerConfig(`"claude-code": "ete"`))
	running := request(store, eteHost, claudeUA, "running")
	run(t, after, running)
	assertRouted(t, running, glmHost, "glm-key")
	assertRecord(t, running, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinExisting})

	fresh := request(store, eteHost, claudeUA, "fresh")
	run(t, after, fresh)
	assertRouted(t, fresh, eteHost, "ete-key")
}

func TestRouter_ASessionlessRequestFollowsTheCurrentChoiceUnpinned(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, "")
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"pin": pinNone})
}

func TestRouter_APinToARemovedServerIsA503(t *testing.T) {
	store := newStore(t)
	store.Put(pinPrefix+"s1", pin{agent: "claude-code", server: "east"}, pinTTL)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := request(store, eteHost, claudeUA, "s1")
	a := run(t, p, pctx)

	if a.Type != pipeline.Reject {
		t.Fatalf("action = %+v, want Reject", a)
	}
	status, _, body := a.Violation.Render()
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
	if want := `pinned to inference server \"east\", which is no longer configured`; !strings.Contains(string(body), want) {
		t.Errorf("body = %s, want it to contain %s", body, want)
	}
	assertRecord(t, pctx, pipeline.ActionDeny, "pinned_server_removed", map[string]string{"server": "east", "pin": pinExisting})
	assertUntouched(t, pctx, eteHost)
}

// assertRedirectedTo fails unless the listener will dial scheme://host for pctx:
// RedirectTarget is what it applies, whatever the Host header said.
func assertRedirectedTo(t *testing.T, pctx *pipeline.Context, scheme, host string) {
	t.Helper()
	gotScheme, gotHost, ok := pctx.RedirectTarget()
	if !ok || gotScheme != scheme || gotHost != host {
		t.Errorf("RedirectTarget = %q %q %v, want %q %q true", gotScheme, gotHost, ok, scheme, host)
	}
}

// Claude Code pointed at the server it is routed to is still redirected there. The
// Host header is the client's word: on a TLS-bridged request the listener otherwise
// dials the CONNECT authority, which need not be this host, and the server's key
// would go with it. Including when the request names the default port the server's
// URL leaves out.
func TestRouter_ARoutedRequestOnItsServersHostIsStillRedirected(t *testing.T) {
	for _, host := range []string{eteHost, "ETE.example.com:443"} {
		t.Run(host, func(t *testing.T) {
			p := build(t, routerConfig(`"claude-code": "ete"`))
			pctx := request(newStore(t), host, claudeUA, "s1")
			run(t, p, pctx)

			if !pctx.Redirected() {
				t.Fatal("Redirected = false: a routed request must be redirected even on its server's own host")
			}
			assertRedirectedTo(t, pctx, "https", eteHost)
			if got := pctx.Headers.Get("Authorization"); got != "Bearer ete-key" {
				t.Errorf("Authorization = %q, want Bearer ete-key", got)
			}
			assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "ete", "pin": pinNew})
		})
	}
	// A redirect back to the host the client named records no requested host, so the
	// session row does not show a move that did not happen, and the framework's record
	// says it went from the server to the server.
	pctx := request(newStore(t), eteHost, claudeUA, "s1")
	run(t, build(t, routerConfig(`"claude-code": "ete"`)), pctx)
	if got := pctx.RequestedHost(); got != "" {
		t.Errorf("RequestedHost = %q, want \"\" for a request already on the server's host", got)
	}
	if invs := pctx.Extensions.Invocations.Outbound; len(invs) != 2 || invs[0].Reason != "redirected" ||
		invs[0].Details["from"] != eteHost || invs[0].Details["to"] != eteHost {
		t.Errorf("invocations = %+v, want modify/redirected from %s to %s, then routed", invs, eteHost, eteHost)
	}
}

// A plain-http request naming an https server's host is moved to https before the
// key is set, so the key never crosses the network in plaintext.
func TestRouter_APlaintextRequestOnAnHTTPSServersHostIsRedirectedToHTTPS(t *testing.T) {
	p := build(t, routerConfig(`"claude-code": "ete"`))
	pctx := request(newStore(t), eteHost, claudeUA, "s1")
	pctx.Scheme = "http"
	run(t, p, pctx)

	assertRedirectedTo(t, pctx, "https", eteHost)
	if pctx.Scheme != "https" {
		t.Errorf("Scheme = %q, want https", pctx.Scheme)
	}
	if got := pctx.Headers.Get("Authorization"); got != "Bearer ete-key" {
		t.Errorf("Authorization = %q, want Bearer ete-key", got)
	}
}

// A session is pinned by where its first request went, not by the choice. Under
// observe that request stays on the host the client named, so the session is pinned
// as not routed; turning enforce on later must not move it to a server it never
// used, which is the mid-conversation switch the pin exists to prevent.
func TestRouter_ASessionStartedUnderObserveStaysPutUnderEnforce(t *testing.T) {
	store := newStore(t)
	observe := build(t, routerConfig(`"claude-code": "glm"`), pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	first := request(store, eteHost, claudeUA, "s1")
	run(t, observe, first)
	assertRecord(t, first, pipeline.ActionObserve, "would_route", map[string]string{"server": "glm", "pin": pinNew})
	if pin, ok := pinOf(t, store, "s1"); !ok || pin != "" {
		t.Fatalf("pin = %q, %v; want the session pinned as not routed (\"\"), since it went nowhere else", pin, ok)
	}

	enforce := build(t, routerConfig(`"claude-code": "glm"`))
	next := request(store, eteHost, claudeUA, "s1")
	run(t, enforce, next)
	assertUntouched(t, next, eteHost)
	assertRecord(t, next, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinExisting})
}

// undeclared is the router without WritesDestination, so the pipeline refuses its
// Redirect: the one way to make a redirect fail from a test.
type undeclared struct{ *Router }

func (u undeclared) Capabilities() pipeline.PluginCapabilities {
	caps := u.Router.Capabilities()
	caps.WritesDestination = false
	return caps
}

// A first request whose redirect failed went nowhere, so it pins nothing: the next
// request decides again rather than inheriting a server the session never reached.
func TestRouter_AFailedRedirectPinsNothing(t *testing.T) {
	store := newStore(t)
	r := New()
	if err := r.Configure(json.RawMessage(routerConfig(`"claude-code": "glm"`))); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	failing, err := pipeline.New([]pipeline.Plugin{undeclared{r}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	pctx := request(store, eteHost, claudeUA, "s1")
	a := run(t, failing, pctx)
	if a.Type != pipeline.Reject {
		t.Fatalf("action = %+v, want Reject", a)
	}
	if status, _, _ := a.Violation.Render(); status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
	assertRecord(t, pctx, pipeline.ActionDeny, "redirect_failed", map[string]string{"server": "glm", "pin": pinNone})
	assertUntouched(t, pctx, eteHost)
	if pin, ok := pinOf(t, store, "s1"); ok {
		t.Fatalf("pin = %q after a failed redirect, want none", pin)
	}

	next := request(store, eteHost, claudeUA, "s1")
	run(t, build(t, routerConfig(`"claude-code": "glm"`)), next)
	assertRouted(t, next, glmHost, "glm-key")
	assertRecord(t, next, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
}

// The listener's synthetic sessions are shared buckets, not conversations: default
// collects traffic it cannot attribute, and pending:<agent> an agent's calls before
// its session is known. Pinning one would hold every later conversation filed there
// to the first one's server, so they route by the agent's current choice, unpinned.
func TestRouter_NeverPinsTheListenersSyntheticSessions(t *testing.T) {
	for _, id := range []string{session.DefaultSessionID, session.PendingPrefix + "claude-code"} {
		t.Run(id, func(t *testing.T) {
			store := newStore(t)
			p := build(t, routerConfig(`"claude-code": "glm"`))
			pctx := request(store, eteHost, claudeUA, id)
			run(t, p, pctx)

			assertRouted(t, pctx, glmHost, "glm-key")
			assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNone})
			if pin, ok := pinOf(t, store, id); ok {
				t.Errorf("pin = %q, want no pin for the synthetic session %q", pin, id)
			}
		})
	}
}

// Under observe, Redirect returns nil and moves nothing. The server's key must not
// reach the host the client named.
func TestRouter_UnderObserveLeavesTheClientsKeyAlone(t *testing.T) {
	p := build(t, routerConfig(`"claude-code": "glm"`), pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	pctx := request(newStore(t), eteHost, claudeUA, "s1")
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionObserve, "would_route", map[string]string{"server": "glm"})
	invs := pctx.Extensions.Invocations.Outbound
	if len(invs) != 2 || invs[0].Reason != "redirected" || !invs[0].Shadow {
		t.Errorf("invocations = %+v, want the framework's shadow redirect, then would_route", invs)
	}
}

func TestRouter_WithoutAStoreRoutesUnpinnedAndWarnsOnce(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := build(t, routerConfig(`"claude-code": "glm"`))
	for range 2 {
		pctx := request(nil, eteHost, claudeUA, "s1")
		run(t, p, pctx)
		assertRouted(t, pctx, glmHost, "glm-key")
		assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"pin": pinNone})
	}
	if n := strings.Count(logs.String(), "sessions are not pinned"); n != 1 {
		t.Errorf("warned %d times, want once:\n%s", n, logs.String())
	}
}

func TestConfigure_WrapsErrorsInThePluginsName(t *testing.T) {
	err := New().Configure(json.RawMessage(`{}`))
	if err == nil || !strings.HasPrefix(err.Error(), "inference-router config: ") {
		t.Fatalf("err = %v, want it prefixed with inference-router config:", err)
	}
}

func TestConfigure_WarnsOnPlainHTTPToAnotherMachine(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	err := New().Configure(json.RawMessage(`{"servers": {
		"lan": {"url": "http://10.0.0.5:4000", "key": "k", "main": "opus", "helper": "haiku"},
		"local": {"url": "http://localhost:4000", "key": "k", "main": "opus", "helper": "haiku"}}}`))
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "server=lan") || !strings.Contains(out, "plain http") {
		t.Errorf("no plaintext warning for lan:\n%s", out)
	}
	if strings.Contains(out, "server=local") {
		t.Errorf("warned about a loopback server:\n%s", out)
	}
}

// The router writes the request body to map models, and never the response, which
// would cost every routed response its streaming.
func TestCapabilities_DeclareARedirectAndARequestBodyWrite(t *testing.T) {
	caps := New().Capabilities()
	if !caps.WritesDestination || !caps.WritesRequestBody || caps.WritesResponseBody {
		t.Errorf("caps = %+v, want WritesDestination and WritesRequestBody, and no response write", caps)
	}
	if n := len(caps.Description); n == 0 || n > 80 {
		t.Errorf("description is %d chars, want 1-80", n)
	}
}

// routerconfig copies the no-User-Agent label rather than importing pipeline.
func TestNoAgentIsThePipelinesUnknownLabel(t *testing.T) {
	if routerconfig.NoAgent != pipeline.UnknownClientLabel {
		t.Errorf("routerconfig.NoAgent = %q, pipeline.UnknownClientLabel = %q", routerconfig.NoAgent, pipeline.UnknownClientLabel)
	}
}

// A listener that cannot honor a redirect refuses the plugin at build time.
func TestRouter_IsRefusedWhereTheListenerCannotRedirect(t *testing.T) {
	_, err := plugins.BuildWithDeps(entries(routerConfig(``)), plugins.Deps{})
	if err == nil || !strings.Contains(err.Error(), "WritesDestination") {
		t.Fatalf("err = %v, want a refusal naming WritesDestination", err)
	}
}

// sent is a request row the forward proxy recorded in a session: an outbound
// inference request from the agent with User-Agent ua, which went to host.
func sent(host, ua string) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: host,
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5-5"}, Client: pipeline.ParseUserAgent(ua),
	}
}

// recorded is the request row the forward proxy records for pctx once the pipeline
// has run: where it went, and the parse it carried.
func recorded(pctx *pipeline.Context) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: pctx.Host,
		Inference: pctx.Extensions.Inference, Client: pipeline.ParseUserAgent(pctx.Headers.Get("User-Agent")),
	}
}

// withHistory gives pctx's session the events the store recorded before it.
func withHistory(pctx *pipeline.Context, events ...pipeline.SessionEvent) *pipeline.Context {
	pctx.Session.Events = events
	return pctx
}

// The reviewer's probe. A Claude Code session already running on ete, but quiet
// while routing was first configured, has no pin: the router never saw it. Its next
// turn is not a new session's first request, and the store's history says so: it
// keeps going where it already went, with ete's key, rather than moving to glm in
// the middle of a conversation.
func TestRouter_ARunningSessionTheRouterHasNotSeenStaysWhereItWent(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := withHistory(request(store, eteHost, claudeUA, "running"), sent(eteHost, claudeUA))
	run(t, p, pctx)

	assertRouted(t, pctx, eteHost, "ete-key")
	assertRedirectedTo(t, pctx, "https", eteHost)
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "ete", "pin": pinNew})
	if v, _ := store.Get(pinPrefix + "running"); v != (pin{agent: "claude-code", server: "ete"}) {
		t.Errorf("pin = %#v, want claude-code's session pinned to ete", v)
	}

	// Pinned now: the next request needs no history to stay.
	next := request(store, eteHost, claudeUA, "running")
	run(t, p, next)
	assertRouted(t, next, eteHost, "ete-key")
	assertRecord(t, next, pipeline.ActionModify, "routed", map[string]string{"server": "ete", "pin": pinExisting})
}

// An earlier request to a host that is no server says where a session ran when it
// is one the router captures. Recorded where the agent sent it, it was made while
// the router was not routing the agent, so the session ran unrouted and stays so:
// switching an agent onto a server must not move a conversation already running on
// its provider. A request the router captured is recorded under its server's host,
// so the session is on that server. A row naming no method or path, from before
// rows carried them, is no evidence either way.
func TestRouter_AnEarlierRequestToAnotherHost(t *testing.T) {
	zen := func(requestedHost, host string) pipeline.SessionEvent {
		e := sent(host, opencodeUA)
		e.HTTPMethod, e.HTTPPath, e.RequestedHost = http.MethodPost, "/zen/v1/chat/completions", requestedHost
		return e
	}
	for _, tc := range []struct {
		name     string
		earlier  pipeline.SessionEvent
		wantHost string
		wantPin  string
	}{
		{"ran unrouted on its provider", zen("", "opencode.ai"), "opencode.ai", ""},
		{"captured to a server", zen("opencode.ai", "ete.example.com"), eteHost, "ete"},
		{"a row with no method or path", sent("opencode.ai", opencodeUA), glmHost, "glm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			p := build(t, routerConfig(`"opencode": "glm"`))
			pctx := withHistory(request(store, "opencode.ai", opencodeUA, "running"), tc.earlier)
			pctx.Path = "/zen/v1/chat/completions"
			run(t, p, pctx)

			if pctx.Host != tc.wantHost {
				t.Errorf("Host = %q, want %q", pctx.Host, tc.wantHost)
			}
			if pin, ok := pinOf(t, store, "running"); !ok || pin != tc.wantPin {
				t.Errorf("pin = %q, %v; want %q", pin, ok, tc.wantPin)
			}
		})
	}
}

// Only the latest earlier request to a server's host decides, so a session is held
// to the server it went to last; a request to any other host in between is passed
// over.
func TestRouter_TheLatestEarlierRequestToAServerDecides(t *testing.T) {
	for _, tc := range []struct {
		name    string
		history []pipeline.SessionEvent
		want    string // the server's host
	}{
		{"elsewhere, then ete", []pipeline.SessionEvent{sent("api.anthropic.com", claudeUA), sent(eteHost, claudeUA)}, eteHost},
		{"ete, then elsewhere", []pipeline.SessionEvent{sent(eteHost, claudeUA), sent("api.anthropic.com", claudeUA)}, eteHost},
		{"glm, then ete with case and its default port", []pipeline.SessionEvent{sent(glmHost, claudeUA), sent("ETE.example.com:443", claudeUA)}, eteHost},
		{"ete, then glm with its port", []pipeline.SessionEvent{sent(eteHost, claudeUA), sent("GLM.example.com:8443", claudeUA)}, glmHost},
		{"only elsewhere: new, the current server", []pipeline.SessionEvent{sent("api.anthropic.com", claudeUA)}, glmHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, routerConfig(`"claude-code": "glm"`))
			pctx := withHistory(request(newStore(t), eteHost, claudeUA, "running"), tc.history...)
			run(t, p, pctx)
			assertRedirectedTo(t, pctx, "https", tc.want)
		})
	}
}

// A row's Host is where the request went, which for a routed request is the
// server it was redirected to; RequestedHost is where the client asked to go. The
// session is on the server the bytes reached, so Host decides.
func TestRouter_TheServerARequestWentToDecidesNotTheOneItAskedFor(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "ete"`))
	routed := sent(glmHost, claudeUA)
	routed.RequestedHost = eteHost
	pctx := withHistory(request(store, eteHost, claudeUA, "running"), routed)
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	if pin, _ := pinOf(t, store, "running"); pin != "glm" {
		t.Errorf("pin = %q, want glm, the server the earlier request reached", pin)
	}
}

// A view can be empty, or hold no inference request, for a session that exists,
// so only an earlier inference request this agent sent and the proxy sent on is
// evidence of a running conversation. Each row here is not, and the session is new:
// it goes to the agent's current server.
func TestRouter_ASessionWithNoEarlierInferenceRequestIsNew(t *testing.T) {
	tunnel := pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: eteHost + ":443",
		Tunnel: true, HTTPMethod: http.MethodConnect, Client: pipeline.ParseUserAgent(claudeUA)}
	denied := sent(eteHost, claudeUA)
	denied.Phase = pipeline.SessionDenied
	response := sent(eteHost, claudeUA)
	response.Phase = pipeline.SessionResponse
	inbound := sent(eteHost, claudeUA)
	inbound.Direction = pipeline.Inbound
	unparsed := sent(eteHost, claudeUA)
	unparsed.Inference = nil
	for _, tc := range []struct {
		name    string
		history []pipeline.SessionEvent
	}{
		{"no events", nil},
		{"only the bridged CONNECT's tunnel row", []pipeline.SessionEvent{tunnel}},
		{"a denied request", []pipeline.SessionEvent{denied}},
		{"a response row", []pipeline.SessionEvent{response}},
		{"an inbound request", []pipeline.SessionEvent{inbound}},
		{"a request no parser read as inference", []pipeline.SessionEvent{unparsed}},
		{"another agent's inference request", []pipeline.SessionEvent{sent(eteHost, opencodeUA)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(t, routerConfig(`"claude-code": "glm"`))
			pctx := withHistory(request(newStore(t), eteHost, claudeUA, "s1"), tc.history...)
			run(t, p, pctx)
			assertRouted(t, pctx, glmHost, "glm-key")
			assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
		})
	}
}

// The history rule is for an agent that is routed. An unlisted agent is left
// alone whatever its session did.
func TestRouter_AnUnlistedAgentsHistoryRoutesNothing(t *testing.T) {
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := withHistory(request(newStore(t), eteHost, opencodeUA, "s1"), sent(glmHost, opencodeUA))
	run(t, p, pctx)
	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", nil)
}

// A session id is the listener's answer, and another agent's request can be filed
// under one: by process attribution (a command an agent runs is filed under its
// session), the ActiveSession fallback with client affinity off, or a header id
// two clients share. A pin is that agent's and holds for it alone. Another agent's
// request is decided as if the session were unpinned — by its own choice, or left
// alone when it is not listed — and never takes the session's server or key, nor
// disturbs its pin.
func TestRouter_APinHoldsOnlyForTheAgentItWasMadeFor(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	run(t, p, request(store, eteHost, claudeUA, "s1"))
	history := sent(glmHost, claudeUA) // where claude-code's request went

	unlisted := withHistory(request(store, eteHost, opencodeUA, "s1"), history)
	run(t, p, unlisted)
	assertUntouched(t, unlisted, eteHost)
	assertRecord(t, unlisted, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinNone})

	both := build(t, routerConfig(`"claude-code": "glm", "opencode": "ete"`))
	listed := withHistory(request(store, eteHost, opencodeUA, "s1"), history)
	run(t, both, listed)
	assertRouted(t, listed, eteHost, "ete-key")
	assertRecord(t, listed, pipeline.ActionModify, "routed", map[string]string{"server": "ete", "pin": pinNone})

	// A request with no User-Agent is no agent's, and no pin is its.
	anonymous := withHistory(request(store, eteHost, "", "s1"), history)
	run(t, both, anonymous)
	assertUntouched(t, anonymous, eteHost)

	if v, _ := store.Get(pinPrefix + "s1"); v != (pin{agent: "claude-code", server: "glm"}) {
		t.Errorf("pin = %#v after other agents' requests, want claude-code's glm pin untouched", v)
	}
	owner := request(store, eteHost, claudeUA, "s1")
	run(t, both, owner)
	assertRouted(t, owner, glmHost, "glm-key")
	assertRecord(t, owner, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinExisting})
}

// claudeCode gives pctx inference-parser's reading of a Claude Code request: role is
// the caller its system prompt declares, tools how many tools it carries, and turns
// how many assistant turns came earlier in its conversation.
func claudeCode(pctx *pipeline.Context, role pipeline.AgentRole, tools, turns int) *pipeline.Context {
	ext := &pipeline.InferenceExtension{Model: "claude-opus-5-5", AgentRole: role, Messages: []pipeline.InferenceMessage{
		{Role: "system", Content: "x-anthropic-billing-header: cc_version=2.1.286; cc_entrypoint=cli;"},
		{Role: "user", Content: "hello"},
	}}
	for range turns {
		ext.Messages = append(ext.Messages,
			pipeline.InferenceMessage{Role: "assistant", Content: "Hi."},
			pipeline.InferenceMessage{Role: "user", Content: "go on"})
	}
	for i := range tools {
		ext.Tools = append(ext.Tools, pipeline.InferenceTool{Name: fmt.Sprintf("Tool%d", i)})
	}
	pctx.Extensions.Inference = ext
	return pctx
}

// The live case behind this rule: a Claude Code conversation running on ete before
// the router knew it — started before routing was set up, or forgotten by a restart —
// was taken for a new session, sent to glm and held there, 298 assistant turns in.
// Its request carries those turns, so the router knows it did not see the
// conversation begin, and leaves it where Claude Code sent it.
func TestRouter_AClaudeCodeConversationItDidNotSeeBeginIsNotRouted(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := claudeCode(request(store, eteHost, claudeUA, "running"), pipeline.AgentRoleMain, 26, 3)
	run(t, p, pctx)

	assertUntouched(t, pctx, eteHost)
	assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinNew, "turn": turnContinuation})
	if pin, pinned := pinOf(t, store, "running"); !pinned || pin != "" {
		t.Errorf("pin = %q (pinned %v), want the session pinned as not routed", pin, pinned)
	}

	// Pinned now: the session stays unrouted whatever its later requests look like.
	next := claudeCode(request(store, eteHost, claudeUA, "running"), pipeline.AgentRoleMain, 26, 4)
	run(t, p, next)
	assertUntouched(t, next, eteHost)
	assertRecord(t, next, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinExisting})
}

// A conversation's opening turn is the beginning the router assigns a server at.
func TestRouter_AClaudeCodeOpeningTurnGoesToItsAgentsServer(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := claudeCode(request(store, eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, 0)
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
	if pin, _ := pinOf(t, store, "s1"); pin != "glm" {
		t.Errorf("pin = %q, want glm", pin)
	}
}

// Claude Code's one-shots — the title request, auto mode's classifier, the
// permission monitor — carry no tools and come at any point in a conversation, and
// a subagent's requests are its own conversation's, not the session's. None says
// whether the session began now, so none decides: it goes where Claude Code sent
// it, and the session stays unpinned for its conversation's next request.
func TestRouter_AClaudeCodeSideRequestDecidesNothing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		role         pipeline.AgentRole
		tools, turns int
	}{
		{"a one-shot with no tools", pipeline.AgentRoleMain, 0, 0},
		{"a subagent's first turn", pipeline.AgentRoleSubagent, 11, 0},
		{"a subagent mid-task", pipeline.AgentRoleSubagent, 11, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			p := build(t, routerConfig(`"claude-code": "glm"`))
			pctx := claudeCode(request(store, eteHost, claudeUA, "s1"), tc.role, tc.tools, tc.turns)
			run(t, p, pctx)

			assertUntouched(t, pctx, eteHost)
			assertRecord(t, pctx, pipeline.ActionSkip, "not_routed", map[string]string{"pin": pinNone, "turn": turnAside})
			if _, pinned := pinOf(t, store, "s1"); pinned {
				t.Error("a side request pinned the session")
			}
		})
	}
}

// After a side request, the conversation's own next request decides: an opening turn
// starts the session on its agent's server, a continuation leaves it unrouted.
func TestRouter_AfterASideRequestTheConversationDecides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		turns int
		host  string // where the conversation's request goes
		pin   string
	}{
		{"an opening turn", 0, glmHost, "glm"},
		{"a continuation", 2, eteHost, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			p := build(t, routerConfig(`"claude-code": "glm"`))
			side := claudeCode(request(store, eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 0, 0)
			run(t, p, side)
			// The forward proxy records the side request's row, which the next request sees.
			pctx := withHistory(claudeCode(request(store, eteHost, claudeUA, "s1"), pipeline.AgentRoleMain, 26, tc.turns),
				recorded(side))
			run(t, p, pctx)

			if pctx.Host != tc.host {
				t.Errorf("Host = %q, want %q", pctx.Host, tc.host)
			}
			if pin, pinned := pinOf(t, store, "s1"); !pinned || pin != tc.pin {
				t.Errorf("pin = %q (pinned %v), want %q", pin, pinned, tc.pin)
			}
		})
	}
}

// The request's turn is read only where the request states Claude Code's role. Any
// other agent's earlier turns may have gone to another provider — OpenCode starts on
// Zen and addresses a server later — so its first request to a server's host is
// where its use of the servers begins, and it goes to its agent's server as before.
func TestRouter_ARequestThatStatesNoRoleIsDecidedAsBefore(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"opencode": "glm"`))
	pctx := claudeCode(request(store, eteHost, opencodeUA, "s1"), "", 12, 5)
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	assertRecord(t, pctx, pipeline.ActionModify, "routed", map[string]string{"server": "glm", "pin": pinNew})
}

// Where the session's recorded history says which server it is on, that is evidence
// and outranks the request: the continuation follows its history to glm.
func TestRouter_RecordedHistoryOutranksTheRequestsTurn(t *testing.T) {
	store := newStore(t)
	p := build(t, routerConfig(`"claude-code": "glm"`))
	pctx := withHistory(claudeCode(request(store, eteHost, claudeUA, "running"), pipeline.AgentRoleMain, 26, 3),
		sent(glmHost, claudeUA))
	run(t, p, pctx)

	assertRouted(t, pctx, glmHost, "glm-key")
	if pin, _ := pinOf(t, store, "running"); pin != "glm" {
		t.Errorf("pin = %q, want glm", pin)
	}
}
