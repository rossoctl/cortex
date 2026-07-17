package contextguru

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	bschemas "github.com/maximhq/bifrost/core/schemas"
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
