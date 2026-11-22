package contextguru

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/cheapmodel"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/cortex/core/pipeline"
)

// anthropicPctx is a Claude Code style request to host, with a credential.
func anthropicPctx(host string, role pipeline.AgentRole) *pipeline.Context {
	p := inferencePctx("/v1/messages", []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	p.Host = host
	p.Headers = http.Header{"X-Api-Key": {"sk-caller"}}
	p.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-5", AgentRole: role}
	return p
}

// An Anthropic request gets an incoming model. Without one, every model.source: incoming
// component has none on Claude Code traffic.
func TestAnthropicRequestGetsIncomingModel(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	spec := p.modelsFor(anthropicPctx("gw", pipeline.AgentRoleMain), bschemas.Anthropic, "")
	if _, ok := spec.Incoming.(cheapmodel.Anthropic); !ok {
		t.Fatalf("Incoming = %#v, want cheapmodel.Anthropic", spec.Incoming)
	}
}

func TestAnthropicRequestWithoutCredentialGetsNoModel(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	pctx := anthropicPctx("gw", pipeline.AgentRoleMain)
	pctx.Headers = http.Header{}
	if spec := p.modelsFor(pctx, bschemas.Anthropic, ""); spec.Incoming != nil {
		t.Fatalf("Incoming = %#v, want nil without a credential", spec.Incoming)
	}
}

// The Responses API has no incoming model: llmclient speaks chat completions only.
func TestResponsesRequestGetsNoIncomingModel(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	pctx := inferencePctx("/v1/responses", []byte(`{"model":"gpt-5.6","input":"hi"}`))
	if spec := p.modelsFor(pctx, bschemas.OpenAI, "responses"); spec.Incoming != nil {
		t.Fatalf("Incoming = %#v, want nil on the Responses API", spec.Incoming)
	}
}

// One hold per credential, reused across requests, released once idle past the TTL. The
// summarizer's call is detached, so the hold must outlive the request that made it.
func TestCredHoldsReuseAndExpire(t *testing.T) {
	d := newCortexDispatcher(newPingClient(false))
	h := newCredHolds(d)
	now := time.Unix(1_700_000_000, 0)
	pctx := anthropicPctx("gw", pipeline.AgentRoleMain)
	a := h.hold(pctx, now)
	if b := h.hold(pctx, now.Add(time.Minute)); b != a {
		t.Fatalf("second hold = %q, want reuse of %q", b, a)
	}
	other := anthropicPctx("gw", pipeline.AgentRoleMain)
	other.Headers.Set("X-Api-Key", "sk-other")
	if c := h.hold(other, now.Add(incomingHoldTTL+2*time.Minute)); c == a {
		t.Fatal("a different credential reused the first hold")
	}
	if _, held := d.held[a]; held {
		t.Fatal("hold idle past the TTL was not released")
	}
	h.releaseAll()
	if len(d.held) != 0 {
		t.Fatalf("releaseAll left %d holds", len(d.held))
	}
}

// Shutdown releases every incoming-model hold: on a hot-reload this instance is the old one.
func TestShutdownReleasesIncomingHolds(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	if _, ok := p.anthropicIncoming(anthropicPctx("gw", pipeline.AgentRoleMain)); !ok {
		t.Fatal("no incoming model")
	}
	if len(p.holds.d.held) != 1 {
		t.Fatalf("held = %d, want 1", len(p.holds.d.held))
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.holds.d.held) != 0 {
		t.Fatalf("Shutdown left %d holds", len(p.holds.d.held))
	}
}

// Without startSweep, an expired hold was released only by a later call to hold() (the
// review finding: on a quiet installation with no later request, it was never released at
// all). startSweep's first pass runs immediately, so an already-expired hold is released
// without any request arriving.
func TestStartSweepReleasesAlreadyExpiredHoldWithNoRequest(t *testing.T) {
	d := newCortexDispatcher(newPingClient(false))
	h := newCredHolds(d)
	pctx := anthropicPctx("gw", pipeline.AgentRoleMain)
	past := time.Now().Add(-incomingHoldTTL - time.Minute)
	ref := h.hold(pctx, past)
	if _, held := d.held[ref]; !held {
		t.Fatal("hold not recorded")
	}

	h.startSweep()
	defer h.stopSweep()

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.m)
		h.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startSweep did not release an already-expired hold within 2s, with no request")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, held := d.held[ref]; held {
		t.Fatal("hold still recorded on the dispatcher after the sweep released it")
	}
}

// stopSweep on a credHolds that never had startSweep called (Init did not run, as a test-built
// instance from newCredHolds directly, or from Configure without Init) must not block or panic.
func TestStopSweepWithoutStartIsANoop(t *testing.T) {
	h := newCredHolds(newCortexDispatcher(newPingClient(false)))
	h.stopSweep()
}

// Init starts the sweep, and Shutdown stops it and waits for it to exit before releasing every
// hold — so the sweep goroutine cannot race Shutdown's own release of the same hold.
func TestInitStartsAndShutdownStopsTheHoldSweep(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	if err := p.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.holds.sweepStop == nil {
		t.Fatal("Init did not start the hold sweep")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Shutdown's stopSweep blocks on sweepDone, so by the time it returns the goroutine has
	// already exited; a second close would panic if it had not.
	select {
	case <-p.holds.sweepDone:
	default:
		t.Fatal("sweep goroutine did not exit by the time Shutdown returned")
	}
}

func TestSentStashReplacesAndDropsOversizedBodies(t *testing.T) {
	s := newSentStash()
	s.put("k", bschemas.Anthropic, []byte("pre"), []byte("sent-1"))
	s.put("k", bschemas.Anthropic, []byte("pre"), []byte("sent-2"))
	if got := string(s.get("k")); got != "sent-2" {
		t.Fatalf("get = %q, want the latest body", got)
	}
	// An oversized body must not leave the older one behind: Ask would answer about a
	// transcript missing everything since.
	s.put("k", bschemas.Anthropic, nil, make([]byte, maxStashBody+1))
	if got := s.get("k"); got != nil {
		t.Fatalf("get after oversized put = %d bytes, want nothing", len(got))
	}
}

// OnFinish stashes a successful main-agent request: the forwarded body for Ask, the received one
// for CoversSpan. A subagent or a failed request stashes nothing.
func TestOnFinishStashesOnlySuccessfulMainRequests(t *testing.T) {
	for name, tc := range map[string]struct {
		role   pipeline.AgentRole
		status int
		want   bool
	}{
		"main 200": {pipeline.AgentRoleMain, 200, true},
		"subagent": {pipeline.AgentRoleSubagent, 200, false},
		"main 500": {pipeline.AgentRoleMain, 500, false},
	} {
		p := configured(t, `{`+offline+`}}`)
		pctx := anthropicPctx("gw", tc.role)
		invokeReq(p, pctx)
		pctx.StatusCode = tc.status
		p.OnFinish(context.Background(), pctx)
		k, _ := p.cgSessions.get("s1")
		if got := p.stash.get(k) != nil; got != tc.want {
			t.Errorf("%s: stashed=%v, want %v", name, got, tc.want)
		}
	}
}

// The body OnRequest snapshots for stashSent is kept on the REQUEST's own pctx
// (pipeline.SetState), not in a map keyed by session id. Two requests that share one session id
// — Claude Code sends one-shot calls (title, permission monitor) under the main turn's session —
// can overlap in flight: a one-shot request B starts, then the main turn A starts before B
// finishes. A session-keyed map let A's Store overwrite B's entry, so B's own OnFinish paired
// A's received body with B's forwarded body (review finding: silent context loss once the
// summarizer's prefix ask sent the mismatched pair). Keying on pctx instead makes each
// request's own pair exact by construction, whatever else is in flight on the same session.
func TestStashedBodyIsNotMixedUpBetweenOverlappingRequestsOnOneSession(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	pctxA := anthropicPctx("gw", pipeline.AgentRoleMain)
	pctxA.Body = []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"A-received"}]}`)
	pctxB := anthropicPctx("gw", pipeline.AgentRoleMain)
	pctxB.Body = []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"B-received"}]}`)

	// B (the one-shot) starts first; A (the main turn) starts on the same session id before B
	// finishes. With the old session-keyed map, A's Store here would overwrite B's entry.
	invokeReq(p, pctxB)
	invokeReq(p, pctxA)

	// B finishes first, successfully.
	pctxB.StatusCode = 200
	p.OnFinish(context.Background(), pctxB)

	key, ok := p.cgSessions.get("s1")
	if !ok {
		t.Fatal("no engine session key for s1")
	}
	p.stash.mu.Lock()
	entry, ok := p.stash.m[key]
	p.stash.mu.Unlock()
	if !ok {
		t.Fatal("B's OnFinish did not stash anything")
	}
	if !bytes.Contains(entry.preBody, []byte("B-received")) {
		t.Errorf("stashed preBody = %s, want B's own received body (B-received), not A's", entry.preBody)
	}
	if !bytes.Equal(entry.forwarded, pctxB.Body) {
		t.Errorf("stashed forwarded = %s, want B's own forwarded body", entry.forwarded)
	}
}

// redirectingPlugin calls pctx.Redirect from OnRequest, as inference-router does — the one
// production plugin that declares WritesDestination. A test puts it after context-guru, as
// agentop server add does (inference-router is always appended last).
type redirectingPlugin struct{ target *url.URL }

func (redirectingPlugin) Name() string { return "redirector" }
func (redirectingPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesDestination: true}
}
func (r redirectingPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if err := pctx.Redirect(r.target); err != nil {
		panic(err) // test setup error, not a case under test
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (redirectingPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Nothing is stashed for a redirected request (review finding): the incoming-model hold is on
// the host and credential the client named, captured in OnRequest before inference-router (which
// always runs after context-guru) redirects — but pctx.Body by OnFinish can be the redirected
// server's own rewrite. Stashing it anyway would have the next turn's prefix ask send that body,
// in whatever shape the OTHER server expects, to the client's original (non-redirected) host.
func TestStashSentSkipsARedirectedRequest(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	target, err := url.Parse("http://other-host")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := pipeline.New([]pipeline.Plugin{p, redirectingPlugin{target: target}})
	if err != nil {
		t.Fatal(err)
	}
	pctx := anthropicPctx("gw", pipeline.AgentRoleMain)
	pctx.MarkRedirectable()
	pl.Run(context.Background(), pctx)
	if !pctx.Redirected() {
		t.Fatal("test setup: the request was not redirected")
	}

	pctx.StatusCode = 200
	p.OnFinish(context.Background(), pctx)

	key, _ := p.cgSessions.get("s1")
	p.stash.mu.Lock()
	_, stashed := p.stash.m[key]
	p.stash.mu.Unlock()
	if stashed {
		t.Error("OnFinish stashed a redirected request's body")
	}
}

// The asker appends its question to the stashed forwarded body and sends it on the caller's own
// credential; with nothing stashed it reports ErrNoPrefix, which the summarizer handles. The call
// carries the plugin's sentinel, so Cortex skips it if it comes back through the proxy.
func TestPrefixAskerAppendsToStashedBody(t *testing.T) {
	var gotKey, gotBody, gotSentinel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotSentinel = r.Header.Get(sentinelHeader)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"model":"claude-sonnet-5","content":[{"type":"text","text":"summary"}],`+
			`"usage":{"input_tokens":1,"cache_read_input_tokens":9,"output_tokens":2}}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := configured(t, `{`+offline+`}}`)
	pctx := anthropicPctx(u.Host, pipeline.AgentRoleMain)
	pctx.Scheme = "http"
	cli, ok := p.anthropicIncoming(pctx)
	if !ok {
		t.Fatal("no incoming model")
	}
	a := prefixAsker{stash: p.stash, cli: cli}
	if _, _, err := a.Ask(context.Background(), "k", "summarize"); !errors.Is(err, cgcomponents.ErrNoPrefix) {
		t.Fatalf("Ask with nothing stashed: err = %v, want ErrNoPrefix", err)
	}
	p.stash.put("k", bschemas.Anthropic, []byte(`{}`),
		[]byte(`{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"the transcript"}]}`))
	reply, _, err := a.Ask(context.Background(), "k", "summarize")
	if err != nil || reply != "summary" {
		t.Fatalf("Ask = %q, %v; want summary", reply, err)
	}
	if gotKey != "sk-caller" || !strings.Contains(gotBody, "the transcript") || !strings.Contains(gotBody, "summarize") {
		t.Fatalf("upstream got key %q and body %q; want the caller's key and the stashed body plus the ask", gotKey, gotBody)
	}
	if gotSentinel == "" || gotSentinel != p.sentinel {
		t.Fatalf("upstream got sentinel %q, want the plugin's own", gotSentinel)
	}
}

// Each call the incoming Anthropic model makes is one KindSpend op on the request it was made
// for, from the response's usage block, as cgModel does on the OpenAI path.
func TestAnthropicIncomingCallIsSpendOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"model":"claude-sonnet-5-20261101","content":[{"type":"text","text":"ok"}],`+
			`"usage":{"input_tokens":7,"cache_read_input_tokens":90,"cache_creation_input_tokens":30,`+
			`"cache_creation":{"ephemeral_1h_input_tokens":10},"output_tokens":5}}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := configured(t, `{`+offline+`}}`)
	pctx := anthropicPctx(u.Host, pipeline.AgentRoleMain)
	pctx.Scheme = "http"
	cli, ok := p.anthropicIncoming(pctx)
	if !ok {
		t.Fatal("no incoming model")
	}
	pctx.SetCurrentPlugin(pluginName, pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	if _, err := cli.Complete(context.Background(), "summarize this"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	ops := pctx.ReportedOps()
	if len(ops) != 1 {
		t.Fatalf("reported ops = %d, want 1: %+v", len(ops), ops)
	}
	op := ops[0]
	if op.Kind != accountant.KindSpend || op.Call == nil || op.Operator != pluginName {
		t.Fatalf("op = %+v, want a KindSpend op from %s", op, pluginName)
	}
	want := accountant.ModelCall{Host: u.Host, Model: "claude-sonnet-5-20261101", Usage: accountant.CallUsage{
		Usage:  accountant.Usage{Input: 7, CacheRead: 90, CacheWrite: 30, CacheWrite1h: 10},
		Output: 5,
	}}
	if op.Call.Host != want.Host || op.Call.Model != want.Model || op.Call.Usage != want.Usage {
		t.Fatalf("Call = %+v, want %+v", *op.Call, want)
	}
}

// A refused call bills nothing, so it reports no op.
func TestAnthropicIncomingRefusedCallReportsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error"}}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := configured(t, `{`+offline+`}}`)
	pctx := anthropicPctx(u.Host, pipeline.AgentRoleMain)
	pctx.Scheme = "http"
	cli, _ := p.anthropicIncoming(pctx)
	pctx.SetCurrentPlugin(pluginName, pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	_, _ = cli.Complete(context.Background(), "summarize this")
	if ops := pctx.ReportedOps(); len(ops) != 0 {
		t.Fatalf("reported ops = %+v, want none for a refused call", ops)
	}
}
