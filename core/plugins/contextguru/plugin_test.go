package contextguru

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/llmclient"
	"github.com/rossoctl/cortex/core/pipeline"
)

func configured(t *testing.T, cfgJSON string) *ContextGuru {
	t.Helper()
	p := New()
	if err := p.Configure(json.RawMessage(cfgJSON)); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return p
}

func invokeReq(p pipeline.Plugin, pctx *pipeline.Context) pipeline.Action {
	pctx.SetCurrentPlugin(p.Name(), pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	return p.OnRequest(context.Background(), pctx)
}

// chatBody builds a minimal OpenAI chat-completions request with one large tool
// output — the shape context-guru's deterministic offloaders act on.
func chatBody(toolContent string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model": "gpt-4o-mini",
		"messages": []map[string]any{
			{"role": "user", "content": "do the thing"},
			{"role": "tool", "tool_call_id": "c1", "content": toolContent},
		},
	})
	return b
}

func bigToolOutput() string {
	var b strings.Builder
	for i := 0; i < 120; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 20))
		b.WriteString("\n")
	}
	return b.String()
}

// collapseEngine is a deterministic (no-model) engine that collapses a large
// tool output — enough to prove the plugin mutates the outbound body.
const collapseEngine = `{"engine":{"pipeline":["collapse"],"components":{"collapse":{"max_tokens":10,"head_lines":2,"tail_lines":2}}}}`

func inferencePctx(path string, body []byte) *pipeline.Context {
	return &pipeline.Context{
		Direction: pipeline.Outbound, Method: "POST", Scheme: "http",
		Host: "ollama", Path: path, Headers: http.Header{},
		Body:      body,
		Session:   &pipeline.SessionView{ID: "s1"},
		SessionID: "s1",
	}
}

func TestConfigure_DefaultsBuildXHigh(t *testing.T) {
	p := configured(t, `{}`)
	if p.pipe == nil || p.store == nil {
		t.Fatal("empty config should default to the xhigh preset and build a pipeline + store")
	}
	want := configured(t, `{"engine":{"preset":"xhigh"}}`).pipe.Names()
	if got := p.pipe.Names(); !slices.Equal(got, want) {
		t.Errorf("default pipeline = %v, want the xhigh preset's %v", got, want)
	}
	if balanced := configured(t, `{"engine":{"preset":"balanced"}}`).pipe.Names(); slices.Equal(p.pipe.Names(), balanced) {
		t.Errorf("default pipeline is balanced's %v, want xhigh", balanced)
	}
}

func TestConfigure_RejectsBadEngine(t *testing.T) {
	if err := New().Configure(json.RawMessage(`{"engine":{"preset":"does-not-exist"}}`)); err == nil {
		t.Fatal("expected error for unknown preset")
	}
}

// model and paths are retired top-level keys, same as keep_alive: json.Unmarshal into
// contextGuruConfig silently drops an unknown field, which would otherwise let `model: {...}`
// or `paths: [...]` look respected when neither does anything any more -- cheap_model's rename
// from model, and the hardcoded paths list, were both silent (review finding).
func TestConfigure_WarnsOnRetiredModelAndPathsKeys(t *testing.T) {
	for _, tc := range []struct {
		name, cfg, want string
	}{
		{"model", `{"model":{"base_url":"http://x","model":"m"}}`, "model is retired"},
		{"paths", `{"paths":["/v1/messages"]}`, "paths is retired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(old)

			if err := New().Configure(json.RawMessage(tc.cfg)); err != nil {
				t.Fatalf("configure: %v", err)
			}
			if got := buf.String(); !strings.Contains(got, tc.want) {
				t.Fatalf("log = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// Cortex cannot answer the engine's expand tool (no streamed-frame replacement, spec §4), so a
// compacted body must never carry a reversible marker or the injected expand tool — whatever the
// preset defaults to.
func TestConfigure_NoReversibleMarkersEvenUnderAFullPreset(t *testing.T) {
	p := configured(t, `{"engine": {"preset": "general"}}`)
	pctx := inferencePctx("/v1/chat/completions", chatBody(bigToolOutput()))
	invokeReq(p, pctx)
	if bytes.Contains(pctx.Body, []byte("<<cg:")) {
		t.Fatal("compacted body carries a reversible <<cg:…>> marker")
	}
	if bytes.Contains(pctx.Body, []byte("context_guru_expand")) {
		t.Fatal("compacted body carries the expand tool")
	}
}

func TestDialectFor(t *testing.T) {
	cases := []struct {
		path     string
		provider bschemas.ModelProvider
		api      string
	}{
		{"/v1/messages", bschemas.Anthropic, ""},
		{"/anthropic/v1/messages", bschemas.Anthropic, ""},
		{"/v1/responses", bschemas.OpenAI, "responses"},
		{"/openai/v1/responses", bschemas.OpenAI, "responses"},
		{"/v1/chat/completions", bschemas.OpenAI, ""},
	}
	for _, c := range cases {
		p, api := dialectFor(c.path)
		if p != c.provider || api != c.api {
			t.Errorf("dialectFor(%q) = (%s, %q), want (%s, %q)", c.path, p, api, c.provider, c.api)
		}
	}
}

func TestConfigure_DefaultPathsIncludeResponses(t *testing.T) {
	p := configured(t, `{}`)
	for _, want := range []string{"/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1/responses"} {
		if !p.gated(want) {
			t.Errorf("default paths do not gate %s", want)
		}
	}
}

func TestOnRequest_CompactsAResponsesBody(t *testing.T) {
	p := configured(t, collapseEngine)
	body := []byte(`{"model":"gpt-5.6","input":[` +
		`{"type":"message","role":"user","content":"run it"},` +
		`{"type":"function_call","call_id":"c1","name":"sh","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":` + strconv.Quote(bigToolOutput()) + `}]}`)
	pctx := inferencePctx("/v1/responses", body)
	invokeReq(p, pctx)
	if len(pctx.Body) >= len(body) {
		t.Fatalf("Responses body not compacted: %d >= %d bytes", len(pctx.Body), len(body))
	}
	if !json.Valid(pctx.Body) {
		t.Fatal("compacted Responses body is not valid JSON")
	}
}

func TestOnRequest_NonInferencePath_PassThrough(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/some/other/path", body)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if pctx.BodyMutated() {
		t.Fatal("non-inference path must not be compacted")
	}
}

// cache.strategy: 1-hour-head's whole effect is apply.Opts.HeadTTL1h/HeadTTLMinBytes relabelling
// an existing head breakpoint's ttl to "1h" — nothing else reads cachePolicy's resolved fields.
// Without wiring them into the OnRequest call (review finding), the strategy resolved a policy
// nothing used, and the tools mark below would still read ttl "5m".
func TestOnRequest_HeadTTL1hStrategyRelabelsAnExistingHeadBreakpoint(t *testing.T) {
	p := configured(t, `{"engine":{"cache":{"strategy":"1-hour-head","head_ttl_min_bytes":1}}}`)
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5",
		"tools": []map[string]any{
			{"name": "Read", "description": "read a file", "input_schema": map[string]any{"type": "object"},
				"cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
		},
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	pctx := inferencePctx("/v1/messages", body)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if !pctx.BodyMutated() {
		t.Fatal("expected the tools breakpoint to be relabelled (SetBody)")
	}
	if got := string(pctx.Body); !strings.Contains(got, `"ttl":"1h"`) {
		t.Fatalf("tools cache_control ttl not relabelled to 1h: %s", got)
	}
}

// A body already at ttl "1h" sets Trace.HeadTTL1h too (apply's own doc: "count it as head all
// the same"), with nothing actually rewritten — reportResult's byte comparison must not read
// that as a change (review suggestion: an earlier version called SetBody on identical bytes).
func TestOnRequest_HeadTTL1hStrategyIsANoOpWhenAlreadyRelabelled(t *testing.T) {
	p := configured(t, `{"engine":{"cache":{"strategy":"1-hour-head","head_ttl_min_bytes":1}}}`)
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5",
		"tools": []map[string]any{
			{"name": "Read", "description": "read a file", "input_schema": map[string]any{"type": "object"},
				"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}},
		},
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	pctx := inferencePctx("/v1/messages", body)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if pctx.BodyMutated() {
		t.Fatal("expected no SetBody: the body was already at ttl 1h")
	}
}

// cache.strategy: 1-hour-always's whole effect is apply.Opts.OneHourFirst asking
// keepalive.Keeper.OneHourFirst, which is exported as of context-guru v0.7.7 (review finding: an
// earlier pass left this unwired on the belief that the decision was unexported, which stopped
// being true once the dependency bumped). Without it wired, the strategy resolves a policy
// nothing asks, and the tools mark below stays unrelabelled.
func TestOnRequest_OneHourAlwaysStrategyRelabelsEveryExistingMark(t *testing.T) {
	p := configured(t, `{"engine":{"cache":{"strategy":"1-hour-always"}}}`)
	if err := p.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5",
		"tools": []map[string]any{
			{"name": "Read", "description": "read a file", "input_schema": map[string]any{"type": "object"},
				"cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	pctx := inferencePctx("/v1/messages", body)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if !pctx.BodyMutated() {
		t.Fatal("expected the tools breakpoint to be relabelled (SetBody)")
	}
	if got := string(pctx.Body); !strings.Contains(got, `"ttl":"1h"`) {
		t.Fatalf("tools cache_control ttl not relabelled to 1h: %s", got)
	}
}

func TestOnRequest_CompactsLargeToolOutput(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/v1/chat/completions", body)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if !pctx.BodyMutated() {
		t.Fatal("large tool output should have been compacted (SetBody)")
	}
	if len(pctx.Body) >= len(body) {
		t.Fatalf("compacted body should be smaller: before=%d after=%d", len(body), len(pctx.Body))
	}
	if !strings.Contains(string(pctx.Body), "lines omitted") {
		t.Fatalf("expected a collapse note in the rewritten body, got: %s", pctx.Body)
	}
}

// TestOnRequest_SentinelHeader_ShortCircuits covers the one safety-critical
// branch: the plugin's own extract:code LLM call carries the sentinel, and if it
// ever transits this forward proxy OnRequest must bail before touching the body —
// otherwise the compaction call recurses into itself. The sentinel is the
// plugin's own per-process secret: outbound headers are the agent's own
// request, and a caller setting the header to a guessable value must NOT skip.
func TestOnRequest_SentinelHeader_ShortCircuits(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/v1/chat/completions", body)
	pctx.Headers.Set(sentinelHeader, p.sentinel)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if pctx.BodyMutated() {
		t.Fatal("sentinel header must short-circuit before any compaction")
	}

	// A forged constant value — the pre-secret wire value — must be compacted
	// like any other request.
	forged := inferencePctx("/v1/chat/completions", body)
	forged.Headers.Set(sentinelHeader, "1")
	if act := invokeReq(p, forged); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if !forged.BodyMutated() {
		t.Fatal("forged sentinel value \"1\" skipped compaction; only the plugin's own secret may")
	}
}

// TestOnRequest_NoSession_Skips verifies that a request with neither session nor
// identity is passed through untouched, so sessionless callers can't share one
// empty engine-store key.
func TestOnRequest_NoSession_Skips(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/v1/chat/completions", body)
	pctx.Session, pctx.SessionID = nil, "" // and Identity is nil → sessionID == ""
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if pctx.BodyMutated() {
		t.Fatal("no session identity must skip compaction (store-key isolation)")
	}
}

func TestConfigure_RejectsBadCacheMode(t *testing.T) {
	if err := New().Configure(json.RawMessage(`{"cache_mode":"always"}`)); err == nil {
		t.Fatal("expected error for cache_mode \"always\"")
	}
	for _, mode := range []string{"", "auto", "on", "off"} {
		p := New()
		if err := p.Configure(json.RawMessage(`{"cache_mode":"` + mode + `"}`)); err != nil {
			t.Fatalf("cache_mode %q: unexpected error: %v", mode, err)
		}
		// Plumbing only: apply.Opts.CacheMode's own resolution (resolveCacheAware) is
		// context-guru's behavior, not this plugin's, so this pins that the value reaches
		// p.cfg (and so OnRequest's apply.Opts.CacheMode), not what the engine then does with it.
		if p.cfg.CacheMode != mode {
			t.Fatalf("cache_mode %q: p.cfg.CacheMode = %q", mode, p.cfg.CacheMode)
		}
	}
}

func TestConfigure_RejectsPartialModel(t *testing.T) {
	if err := New().Configure(json.RawMessage(`{"cheap_model":{"base_url":"http://x"}}`)); err == nil {
		t.Fatal("expected error when model.model is unset but base_url is set")
	}
	if err := New().Configure(json.RawMessage(`{"cheap_model":{"model":"gpt-4o-mini"}}`)); err == nil {
		t.Fatal("expected error when model.base_url is unset but model is set")
	}
}

func TestConfigure_RejectsNegativeMaxConcurrent(t *testing.T) {
	cfg := `{"cheap_model":{"base_url":"http://x","model":"m","max_concurrent":-1}}`
	if err := New().Configure(json.RawMessage(cfg)); err == nil {
		t.Fatal("expected error for a negative model.max_concurrent")
	}
}

func TestOnRequest_EmptyBody_PassThrough(t *testing.T) {
	p := configured(t, collapseEngine)
	pctx := inferencePctx("/v1/chat/completions", nil)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if pctx.BodyMutated() {
		t.Fatal("nil body must be a pass-through")
	}
}

func TestOnResponse_PassThrough(t *testing.T) {
	p := configured(t, collapseEngine)
	pctx := inferencePctx("/v1/chat/completions", chatBody(bigToolOutput()))
	if act := p.OnResponse(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("v1 OnResponse must be a pass-through, got %v", act.Type)
	}
}

// The log says what happened to the request, from the plugin's own SetBody result:
// under on_error: observe the body goes upstream as the client sent it, so a line
// saying it was rewritten would send an operator looking for a change that was
// never made.
func TestOnRequest_LogsWhetherItsOwnRewriteApplied(t *testing.T) {
	for _, tc := range []struct {
		policy      pipeline.ErrorPolicy
		want, never string
	}{
		{pipeline.ErrorPolicyEnforce, "context-guru rewrote request body", "would rewrite"},
		{pipeline.ErrorPolicyObserve, "context-guru would rewrite request body", "context-guru rewrote"},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			p, err := pipeline.New([]pipeline.Plugin{configured(t, collapseEngine)}, pipeline.WithPolicies(tc.policy))
			if err != nil {
				t.Fatalf("pipeline.New: %v", err)
			}
			p.Run(context.Background(), inferencePctx("/v1/chat/completions", chatBody(bigToolOutput())))

			out := logs.String()
			if !strings.Contains(out, tc.want) || strings.Contains(out, tc.never) {
				t.Errorf("logs want %q and never %q:\n%s", tc.want, tc.never, out)
			}
		})
	}
}

// A request the engine compacts reports its context operations to the accountant, each still
// carrying the "context-guru.<component>" operator the library itself stamps (so the framework's
// ReportOps does not overwrite it with the plugin's own name). The collapse engine on this fixture
// produces exactly one rewrite op; see TestReportResult_ForwardsOpsUnchanged for the general proof
// that reportResult forwards Trace.Ops unchanged (this test only pins the end-to-end shape).
func TestPlugin_ReportsEngineOps(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/v1/chat/completions", body)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if !pctx.BodyMutated() {
		t.Fatal("expected the engine to compact this body")
	}
	ops := pctx.ReportedOps()
	if len(ops) != 1 {
		t.Fatalf("expected exactly one reported op for this fixture, got %d: %+v", len(ops), ops)
	}
	op := ops[0]
	if !strings.HasPrefix(op.Operator, "context-guru.") {
		t.Errorf("op %+v: want Operator prefixed %q", op, "context-guru.")
	}
	if op.Kind == accountant.KindRewrite && !(op.SizeBefore.Bytes > op.SizeAfter.Bytes) {
		t.Errorf("rewrite op %+v: want SizeBefore.Bytes (%d) > SizeAfter.Bytes (%d)",
			op, op.SizeBefore.Bytes, op.SizeAfter.Bytes)
	}
}

// reportResult must forward the engine's ops to the accountant unchanged — same Kind, ChangeID,
// At, Spans, SizeBefore, SizeAfter — not just "something with the right Operator prefix" per op.
// Built against a hand-built apply.Result rather than a second apply.BodyOpts call: the engine's
// store/session state is already mutated by the first call, so a second call would observe
// different state rather than reproduce the same ops.
func TestReportResult_ForwardsOpsUnchanged(t *testing.T) {
	pctx := inferencePctx("/v1/chat/completions", nil)
	pctx.SetCurrentPlugin("context-guru", pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()

	wantOps := []accountant.Operation{
		{
			Kind:     accountant.KindRewrite,
			Operator: "context-guru.collapse",
			ChangeID: "context-guru:collapse:aaaaaa",
			At:       accountant.Position{Section: accountant.SectionMessages, Index: 1},
			Spans: []accountant.Span{{
				Section: accountant.SectionMessages, From: 1, To: 1,
				SizeBefore: accountant.Size{Bytes: 500}, SizeAfter: accountant.Size{Bytes: 50},
			}},
			SizeBefore: accountant.Size{Bytes: 500},
			SizeAfter:  accountant.Size{Bytes: 50},
		},
		{
			Kind:       accountant.KindSetting,
			Operator:   "context-guru.offload",
			ChangeID:   "context-guru:offload:bbbbbb",
			At:         accountant.Position{Section: accountant.SectionMessages, Index: 2},
			SizeBefore: accountant.Size{Bytes: 300},
			SizeAfter:  accountant.Size{Bytes: 0},
		},
	}
	res := apply.Result{
		Body:    []byte(`{"ok":true}`),
		Changed: true,
		Trace:   apply.Trace{Ops: append([]accountant.Operation(nil), wantOps...)},
	}

	out, changed, ops := reportResult(pctx, res, nil)
	if !changed || string(out) != `{"ok":true}` {
		t.Fatalf("reportResult = (%q, %v), want (%q, true)", out, changed, `{"ok":true}`)
	}
	applied := pctx.SetBody(out)
	stampDryRun(ops, !applied)
	pctx.ReportOps(ops...)

	gotOps := pctx.ReportedOps()
	if len(gotOps) != 2 {
		t.Fatalf("got %d ops, want 2: %+v", len(gotOps), gotOps)
	}
	if !reflect.DeepEqual(gotOps, wantOps) {
		t.Fatalf("ReportedOps() = %+v, want %+v (forwarded unchanged)", gotOps, wantOps)
	}
	for _, op := range gotOps {
		if op.Kind == accountant.KindRewrite && !(op.SizeBefore.Bytes > op.SizeAfter.Bytes) {
			t.Errorf("rewrite op %+v: want SizeBefore.Bytes > SizeAfter.Bytes", op)
		}
	}
}

// With no Opts.ThreadOf wired (unchanged in this task), the engine resolves no thread, so
// res.Trace.Thread.ID is empty and SetAccountingThread's empty-id no-op leaves AccountingThread
// unset. core/accounting.ThreadOf's own default rule then names the primary agent "main".
func TestPlugin_SetsThread(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/v1/chat/completions", body)
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if _, _, ok := pctx.AccountingThread(); ok {
		t.Fatal("expected no accounting thread override for the primary agent")
	}
	if got := accounting.ThreadOf(pctx); got.ID != "main" {
		t.Fatalf("accounting.ThreadOf = %+v, want ID \"main\"", got)
	}
}

// cgModel.Complete reports its own in-request model call as a KindSpend op: Input is PromptTokens
// minus the cached part, CacheRead is the cached part, Output is CompletionTokens. The op carries
// no ChangeID (it is not a replayed change) and Detached is false (it finished before the request
// it served was answered).
// A call the server answered is reported even when its cost is not known: a zero CallUsage, which
// the accountant keeps as missing rather than zero. A call that may never have been sent is not
// reported. Off-spec usage never gives a negative Input.
func TestCgModel_SpendOpEdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		down    bool
		wantOps int
		want    accountant.CallUsage
	}{
		{name: "no usage block", status: 200, body: `{"choices":[{"message":{"content":"ok"}}]}`, wantOps: 1},
		{name: "cached above prompt", status: 200,
			body:    `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":30}}}`,
			wantOps: 1, want: accountant.CallUsage{Usage: accountant.Usage{Input: 0, CacheRead: 10}, Output: 5}},
		{name: "HTTP error", status: 500, body: `oops`, wantOps: 1},
		{name: "undecodable body", status: 200, body: `{`, wantOps: 1},
		{name: "server unreachable", down: true, wantOps: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			url := srv.URL
			if tc.down {
				srv.Close()
			} else {
				defer srv.Close()
			}
			pctx := inferencePctx("/v1/chat/completions", nil)
			pctx.SetCurrentPlugin("context-guru", pipeline.InvocationPhaseRequest)
			m := cgModel{
				c:      llmclient.New(llmclient.Options{Endpoint: url, Model: "cheap-model"}),
				model:  "cheap-model",
				host:   "cheap-llm.svc",
				report: pctx.OpReporter(),
			}
			pctx.ClearCurrentPlugin()
			_, _ = m.Complete(context.Background(), "summarize this")
			ops := pctx.ReportedOps()
			if len(ops) != tc.wantOps {
				t.Fatalf("ops = %+v, want %d", ops, tc.wantOps)
			}
			if tc.wantOps == 1 && ops[0].Call.Usage != tc.want {
				t.Errorf("Call.Usage = %+v, want %+v", ops[0].Call.Usage, tc.want)
			}
		})
	}
}

func TestPlugin_ModelCallIsSpendOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}`)
	}))
	defer srv.Close()

	pctx := inferencePctx("/v1/chat/completions", nil)
	pctx.SetCurrentPlugin("context-guru", pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()

	m := cgModel{
		c:         llmclient.New(llmclient.Options{Endpoint: srv.URL, Model: "cheap-model"}),
		maxTokens: 100,
		model:     "cheap-model",
		host:      "cheap-llm.svc",
		report:    pctx.OpReporter(),
	}
	if _, err := m.Complete(context.Background(), "summarize this"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	ops := pctx.ReportedOps()
	if len(ops) != 1 {
		t.Fatalf("expected exactly one reported op, got %d: %+v", len(ops), ops)
	}
	op := ops[0]
	if op.Kind != accountant.KindSpend {
		t.Fatalf("op.Kind = %v, want KindSpend", op.Kind)
	}
	if op.Call == nil {
		t.Fatal("op.Call is nil")
	}
	if op.Call.Model != "cheap-model" {
		t.Errorf("Call.Model = %q, want %q", op.Call.Model, "cheap-model")
	}
	if op.Call.Usage.Input != 60 {
		t.Errorf("Call.Usage.Input = %d, want 60", op.Call.Usage.Input)
	}
	if op.Call.Usage.CacheRead != 40 {
		t.Errorf("Call.Usage.CacheRead = %d, want 40", op.Call.Usage.CacheRead)
	}
	if op.Call.Usage.Output != 20 {
		t.Errorf("Call.Usage.Output = %d, want 20", op.Call.Usage.Output)
	}
	if op.ChangeID != "" {
		t.Errorf("ChangeID = %q, want empty", op.ChangeID)
	}
	if op.Call.Detached {
		t.Error("Detached = true, want false")
	}
	if op.Operator != "context-guru" {
		t.Errorf("Operator = %q, want %q", op.Operator, "context-guru")
	}
}

// Under on_error: observe the plugin's writes are shadow (never sent), and the framework stamps
// DryRun on every op a plugin reports while it runs under that policy — ReportOps, not the
// plugin, is responsible for this, but the plugin must still call it so the accountant sees the
// engine's would-be ops.
func TestPlugin_ObserveModeOpsAreDryRun(t *testing.T) {
	pl, err := pipeline.New([]pipeline.Plugin{configured(t, collapseEngine)}, pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	pctx := inferencePctx("/v1/chat/completions", chatBody(bigToolOutput()))
	pl.Run(context.Background(), pctx)

	ops := pctx.ReportedOps()
	if len(ops) == 0 {
		t.Fatal("expected ops reported under observe mode too")
	}
	for _, op := range ops {
		if !op.DryRun {
			t.Errorf("op %+v: want DryRun under on_error: observe", op)
		}
	}
}

// usageServer answers every chat call with the same usage, the shape cgModel reports a spend op for.
func usageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// context-guru's engine calls Complete from several goroutines at once (its extract stage runs
// six), while the request goroutine is still inside OnRequest. Every call's op must land, named
// context-guru and carrying the dry-run state captured when the model was built. Run with -race.
func TestCgModel_ConcurrentCompleteOnOneRequest(t *testing.T) {
	srv := usageServer(t)
	p := configured(t, `{"cheap_model":{"base_url":"`+srv.URL+`","model":"cheap-model"}}`)
	pctx := inferencePctx("/v1/chat/completions", nil)
	pctx.SetCurrentPlugin("context-guru", pipeline.InvocationPhaseRequest)
	m := p.staticModelFor(pctx)
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Complete(context.Background(), "summarize"); err != nil {
				t.Errorf("Complete: %v", err)
			}
		}()
	}
	// The request goroutine keeps dispatching meanwhile: another plugin's stamp must not leak in.
	pctx.ClearCurrentPlugin()
	pctx.SetCurrentPlugin("someone-else", pipeline.InvocationPhaseRequest)
	wg.Wait()
	pctx.ClearCurrentPlugin()
	ops := pctx.ReportedOps()
	if len(ops) != n {
		t.Fatalf("ops = %d, want %d", len(ops), n)
	}
	for _, op := range ops {
		if op.Operator != "context-guru" || op.DryRun {
			t.Errorf("op = {Operator:%q DryRun:%v}, want {context-guru false}", op.Operator, op.DryRun)
		}
	}
}

// TestCgModel_MaxConcurrentBoundsInFlightCalls proves the cap actually bounds concurrency
// (model.max_concurrent, mirroring context-guru-proxy's --cheap-model-concurrent): with the cap
// at 2 and 6 calls fired at once against a server that blocks until released, no more than 2 are
// ever in flight together, and all 6 still complete once released.
func TestCgModel_MaxConcurrentBoundsInFlightCalls(t *testing.T) {
	const maxConc, n = 2, 6
	release := make(chan struct{})
	var inFlight, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		// Lower the count here, while the call is still inside Complete and so still holds its
		// slot. Lowering it after Complete returns would race: Complete frees the slot in a
		// deferred receive before it returns, so a waiting call can acquire and count itself
		// before this call is uncounted, and a correct cap would report maxConc+1.
		inFlight.Add(-1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(srv.Close)

	p := configured(t, `{"cheap_model":{"base_url":"`+srv.URL+`","model":"cheap-model","max_concurrent":`+
		"2"+`}}`)
	pctx := inferencePctx("/v1/chat/completions", nil)
	pctx.SetCurrentPlugin("context-guru", pipeline.InvocationPhaseRequest)
	model := p.staticModelFor(pctx)
	cm, ok := model.(cgModel)
	if !ok {
		t.Fatalf("staticModelFor returned %T, want cgModel", model)
	}

	// onAcquire fires the instant this call's semaphore slot is taken, in-process, with no
	// network round trip to race against: a broken cap lets every call acquire with no
	// blocking at all, so peak reaches n almost immediately rather than waiting on anything the
	// HTTP server on the other end of Complete could delay. The real call still goes on to that
	// server and blocks on release there, which is what lets this holding pattern exist for the
	// test to observe at all.
	atCap := make(chan struct{})
	var atCapOnce sync.Once
	cm.onAcquire = func() {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		if cur >= maxConc {
			atCapOnce.Do(func() { close(atCap) })
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cm.Complete(context.Background(), "summarize"); err != nil {
				t.Errorf("Complete: %v", err)
			}
		}()
	}
	// Wait for the cap to actually bind before releasing: a genuinely bounded run reaches
	// maxConc and stays there, so this never hangs waiting for a release only a bug would need.
	<-atCap
	close(release)
	wg.Wait()
	pctx.ClearCurrentPlugin()

	if got := peak.Load(); got > maxConc {
		t.Fatalf("peak concurrent calls = %d, want at most %d (max_concurrent)", got, maxConc)
	}
	if ops := pctx.ReportedOps(); len(ops) != n {
		t.Fatalf("ops = %d, want %d -- every call must still complete once released", len(ops), n)
	}
}

// A model built while the plugin runs under on_error: observe reports its calls as DryRun and as
// context-guru's, from what was captured at construction, not from whatever is dispatched when the
// call returns: here the call returns while a later plugin, under enforce, is dispatched.
func TestCgModel_OperatorAndDryRunCapturedAtConstruction(t *testing.T) {
	srv := usageServer(t)
	p := configured(t, `{"cheap_model":{"base_url":"`+srv.URL+`","model":"cheap-model"}}`)
	release, called := make(chan struct{}), make(chan struct{})
	grab := &modelGrabber{p: p, after: func(m cgcomponents.Model) {
		go func() {
			<-release
			_, _ = m.Complete(context.Background(), "summarize")
			close(called)
		}()
	}}
	waiter := &waitPlugin{onReq: func() { close(release); <-called }}
	pl, err := pipeline.New([]pipeline.Plugin{grab, waiter},
		pipeline.WithPolicies(pipeline.ErrorPolicyObserve, pipeline.ErrorPolicyEnforce))
	if err != nil {
		t.Fatal(err)
	}
	pctx := inferencePctx("/v1/chat/completions", nil)
	pl.Run(context.Background(), pctx)
	ops := pctx.ReportedOps()
	if len(ops) != 1 {
		t.Fatalf("ops = %+v, want 1", ops)
	}
	if ops[0].Operator != "context-guru" || !ops[0].DryRun {
		t.Errorf("op = {Operator:%q DryRun:%v}, want {context-guru true}", ops[0].Operator, ops[0].DryRun)
	}
}

type waitPlugin struct{ onReq func() }

func (w *waitPlugin) Name() string                              { return "waiter" }
func (w *waitPlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (w *waitPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	w.onReq()
	return pipeline.Action{Type: pipeline.Continue}
}
func (w *waitPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// A model call an engine detached from its request (summarize_async, cache_aware_async) can
// return after Pipeline.Run handed the op list to the accountant. Its op is dropped and counted,
// in the plugin's metrics and the pipeline's, and reading it races nothing. Run with -race.
func TestCgModel_CompleteAfterRunIsDroppedAndCounted(t *testing.T) {
	srv := usageServer(t)
	p := configured(t, `{"cheap_model":{"base_url":"`+srv.URL+`","model":"cheap-model"}}`)
	grab := &modelGrabber{p: p}
	pl, err := pipeline.New([]pipeline.Plugin{grab})
	if err != nil {
		t.Fatal(err)
	}
	pctx := inferencePctx("/v1/chat/completions", nil)
	pl.Run(context.Background(), pctx)
	if grab.m == nil {
		t.Fatal("the plugin built no model")
	}
	before := pipeline.LateOpsDropped()
	done := make(chan error)
	go func() {
		_, err := grab.m.Complete(context.Background(), "late")
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := len(pctx.ReportedOps()); got != 0 {
		t.Errorf("ops = %d after a late call, want 0", got)
	}
	if got := pipeline.LateOpsDropped() - before; got != 1 {
		t.Errorf("pipeline.LateOpsDropped grew by %d, want 1", got)
	}
	if got := metricValue(p.Metrics(), "late_model_calls_dropped"); got != 1 {
		t.Errorf("late_model_calls_dropped = %v, want 1", got)
	}
}

// modelGrabber builds context-guru's static model inside OnRequest, as OnRequest does, and keeps
// it, so a test can call it after the request.
type modelGrabber struct {
	p     *ContextGuru
	m     cgcomponents.Model
	after func(cgcomponents.Model)
}

func (g *modelGrabber) Name() string { return "context-guru" }
func (g *modelGrabber) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (g *modelGrabber) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	g.m = g.p.staticModelFor(pctx)
	if g.after != nil {
		g.after(g.m)
	}
	return pipeline.Action{Type: pipeline.Continue}
}
func (g *modelGrabber) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// modelsForGrabber calls context-guru's modelsFor inside OnRequest, as OnRequest does, and
// keeps the result, so a test can inspect it after the request — in particular under a policy
// (observe) that invokeReq's bypass of pctx.SetCurrentPlugin cannot set, since
// pipeline.Context.Observing reads the policy the pipeline itself stamped on dispatch.
type modelsForGrabber struct {
	p        *ContextGuru
	provider bschemas.ModelProvider
	api      string
	out      cgcomponents.ModelSpec
}

func (g *modelsForGrabber) Name() string { return "context-guru" }
func (g *modelsForGrabber) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{}
}
func (g *modelsForGrabber) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	g.out = g.p.modelsFor(pctx, g.provider, g.api)
	return pipeline.Action{Type: pipeline.Continue}
}
func (g *modelsForGrabber) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Under on_error: observe, modelsFor must build neither model: a real, billed call on the
// agent's own credential is the opposite of a shadow pass that changes nothing (review
// finding — cgModel.Complete and meteredDispatcher always send the HTTP call regardless of
// dryRun, which only changes how the resulting op is reported). Checked for both dialects:
// Anthropic also takes a credential hold as a side effect of building its incoming model
// (anthropicIncoming), so this doubles as the test that observe takes no hold either.
func TestModelsForBuildsNeitherModelUnderObserve(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider bschemas.ModelProvider
		api      string
		pctx     func(*ContextGuru) *pipeline.Context
	}{
		{"anthropic", bschemas.Anthropic, "", func(*ContextGuru) *pipeline.Context {
			return anthropicPctx("gw", pipeline.AgentRoleMain)
		}},
		{"openai chat completions", bschemas.OpenAI, "", func(*ContextGuru) *pipeline.Context {
			return inferencePctx("/v1/chat/completions", chatBody("hi"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := configured(t, `{`+offline+`}}`)
			grab := &modelsForGrabber{p: p, provider: tc.provider, api: tc.api}
			pl, err := pipeline.New([]pipeline.Plugin{grab}, pipeline.WithPolicies(pipeline.ErrorPolicyObserve))
			if err != nil {
				t.Fatal(err)
			}
			pl.Run(context.Background(), tc.pctx(p))

			if grab.out.Static != nil {
				t.Error("modelsFor built a static model under observe")
			}
			if grab.out.Incoming != nil {
				t.Error("modelsFor built an incoming model under observe")
			}
			if p.holds != nil && len(p.holds.d.held) != 0 {
				t.Errorf("observe took %d credential hold(s), want 0", len(p.holds.d.held))
			}
		})
	}
}

func metricValue(ms []pipeline.Metric, name string) float64 {
	for _, m := range ms {
		if m.Name == name {
			return m.Value
		}
	}
	return -1
}
