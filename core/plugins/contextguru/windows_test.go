package contextguru

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"

	cgstore "github.com/rossoctl/context-guru/store"
)

// offline keeps the LiteLLM map out of these tests: no network, so every answer comes from the
// operator table or the built-in one.
const offline = `"context_window": {"model_info": "off"`

func modelPctx(model string) *pipeline.Context {
	p := inferencePctx("/v1/messages", []byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	p.Extensions.Inference = &pipeline.InferenceExtension{Model: model}
	return p
}

func TestWindowFor_OperatorTableIsExact(t *testing.T) {
	p := configured(t, `{`+offline+`, "windows": {"AWS/Claude-Sonnet-5": 1000000}}}`)
	w, exact := p.windowFor(context.Background(), modelPctx("aws/claude-sonnet-5"))
	if w != 1_000_000 || !exact {
		t.Fatalf("operator table: got window=%d exact=%v, want 1000000 exact", w, exact)
	}
}

// The built-in table is the engine's last resort and is never exact, so a fraction trigger must
// still decline on it. This is the case every request was in before the plugin passed a window.
func TestWindowFor_BuiltInTableIsNotExact(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	w, exact := p.windowFor(context.Background(), modelPctx("claude-sonnet-5"))
	if w <= 0 || exact {
		t.Fatalf("built-in table: got window=%d exact=%v, want a window that is not exact", w, exact)
	}
}

func TestWindowFor_NoModelNoWindow(t *testing.T) {
	p := configured(t, `{`+offline+`}}`)
	if w, exact := p.windowFor(context.Background(), inferencePctx("/v1/messages", nil)); w != 0 || exact {
		t.Fatalf("no inference parse: got window=%d exact=%v, want none", w, exact)
	}
}

// billedFor runs one request and its finish through the plugin and returns what the engine's
// store holds for the session afterwards.
func billedFor(t *testing.T, fin *pipeline.Context) (string, bool) {
	t.Helper()
	p := configured(t, `{`+offline+`}}`)
	invokeReq(p, modelPctx("claude-sonnet-5"))
	key, ok := p.cgSessions.Load("s1")
	if !ok {
		t.Fatal("OnRequest did not record the engine's session key")
	}
	p.OnFinish(context.Background(), fin)
	b, ok := p.store.Get(cgstore.BilledPrefix + key.(string))
	return string(b), ok
}

// The next request's Ctx.PrevBilledInput is read from this key, so a fraction trigger can compare
// the session's fill against the window. Billed input is fresh + cache read + cache write.
func TestOnFinish_RecordsBilledInput(t *testing.T) {
	fin := finishedPctx(200, pipeline.AgentRoleMain)
	fin.Extensions.Inference.CacheWriteTokens = 1_000
	got, ok := billedFor(t, fin)
	if !ok || got != "49003" {
		t.Fatalf("billed input: got %q (present=%v), want 49003", got, ok)
	}
}

func TestOnFinish_SkipsSubagentAndFailedRequests(t *testing.T) {
	for name, fin := range map[string]*pipeline.Context{
		"subagent": finishedPctx(200, pipeline.AgentRoleSubagent),
		"failed":   finishedPctx(500, pipeline.AgentRoleMain),
	} {
		if got, ok := billedFor(t, fin); ok {
			t.Errorf("%s: recorded billed input %q, want nothing", name, got)
		}
	}
}
