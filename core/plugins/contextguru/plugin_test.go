package contextguru

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
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
		Body:    body,
		Session: &pipeline.SessionView{ID: "s1"},
	}
}

func TestConfigure_DefaultsBuildXHigh(t *testing.T) {
	p := configured(t, `{}`)
	if p.pipe == nil || p.store == nil {
		t.Fatal("empty config should default to the xhigh preset and build a pipeline + store")
	}
	if len(p.cfg.Paths) == 0 {
		t.Fatal("paths should default to the inference paths")
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
// otherwise the compaction call recurses into itself.
func TestOnRequest_SentinelHeader_ShortCircuits(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/v1/chat/completions", body)
	pctx.Headers.Set(sentinelHeader, "1")
	if act := invokeReq(p, pctx); act.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", act.Type)
	}
	if pctx.BodyMutated() {
		t.Fatal("sentinel header must short-circuit before any compaction")
	}
}

// TestOnRequest_NoSession_Skips verifies that a request with neither session nor
// identity is passed through untouched, so sessionless callers can't share one
// empty engine-store key.
func TestOnRequest_NoSession_Skips(t *testing.T) {
	p := configured(t, collapseEngine)
	body := chatBody(bigToolOutput())
	pctx := inferencePctx("/v1/chat/completions", body)
	pctx.Session = nil // and Identity is nil → sessionID == ""
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
