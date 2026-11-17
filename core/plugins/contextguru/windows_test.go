package contextguru

import (
	"context"
	"fmt"
	"testing"
	"time"

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
	key, ok := p.cgSessions.get("s1")
	if !ok {
		t.Fatal("OnRequest did not record the engine's session key")
	}
	p.OnFinish(context.Background(), fin)
	b, ok := p.store.Get(cgstore.BilledPrefix + key)
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

// cgSessions holds one entry per session for the life of the process; past sessionKeysCap the
// sessions seen longest ago are dropped down to sessionKeysTrimTo, and the newest survive.
func TestSessionKeysIsBounded(t *testing.T) {
	var s sessionKeys
	at := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return at }
	for i := 0; i <= sessionKeysCap; i++ {
		at = at.Add(time.Second)
		s.set(fmt.Sprintf("s%d", i), fmt.Sprintf("k%d", i))
	}
	if n := s.len(); n != sessionKeysTrimTo {
		t.Fatalf("sessionKeys holds %d after crossing the cap, want %d", n, sessionKeysTrimTo)
	}
	if _, ok := s.get("s0"); ok {
		t.Error("the oldest session survived the trim")
	}
	newest := fmt.Sprintf("s%d", sessionKeysCap)
	if k, ok := s.get(newest); !ok || k != fmt.Sprintf("k%d", sessionKeysCap) {
		t.Errorf("the newest session = %q, %v; want kept", k, ok)
	}
}
