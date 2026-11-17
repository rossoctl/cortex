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
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
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

func TestConfigure_DefaultsBuildBalanced(t *testing.T) {
	p := configured(t, `{}`)
	if p.pipe == nil || p.store == nil {
		t.Fatal("empty config should default to the balanced preset and build a pipeline + store")
	}
	if len(p.cfg.Paths) == 0 {
		t.Fatal("paths should default to the inference paths")
	}
}

func TestConfigure_RejectsBadEngine(t *testing.T) {
	if err := New().Configure(json.RawMessage(`{"engine":{"preset":"does-not-exist"}}`)); err == nil {
		t.Fatal("expected error for unknown preset")
	}
}

func TestProviderFor(t *testing.T) {
	if providerFor("/v1/messages") != bschemas.Anthropic {
		t.Fatal("/v1/messages should map to anthropic")
	}
	if providerFor("/v1/chat/completions") != bschemas.OpenAI {
		t.Fatal("/v1/chat/completions should map to openai")
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

func TestConfigure_RejectsPartialModel(t *testing.T) {
	if err := New().Configure(json.RawMessage(`{"model":{"base_url":"http://x"}}`)); err == nil {
		t.Fatal("expected error when model.model is unset but base_url is set")
	}
	if err := New().Configure(json.RawMessage(`{"model":{"model":"gpt-4o-mini"}}`)); err == nil {
		t.Fatal("expected error when model.base_url is unset but model is set")
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

	out, changed := reportResult(pctx, res)
	if !changed || string(out) != `{"ok":true}` {
		t.Fatalf("reportResult = (%q, %v), want (%q, true)", out, changed, `{"ok":true}`)
	}

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
		pctx:      pctx,
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
