package contextguru

import (
	"context"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"

	"github.com/rossoctl/context-guru/models"
	"github.com/rossoctl/context-guru/models/litellm"
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

// billedFor runs one request and its finish through the plugin, on one pctx as the pipeline does,
// and returns what the engine's store holds afterwards: the session figure and the thread figure.
// The request's usage is finishedPctx's, with status and role as given.
func billedFor(t *testing.T, status int, role pipeline.AgentRole, cacheWrite int) (session, thread string, ok bool) {
	t.Helper()
	p := configured(t, `{`+offline+`}}`)
	pctx := modelPctx("claude-sonnet-5")
	invokeReq(p, pctx)
	th := pipeline.GetState[cgThread](pctx, cgThreadStateKey)
	if th == nil {
		t.Fatal("OnRequest did not record the engine's session and thread")
	}
	fin := finishedPctx(status, role)
	pctx.StatusCode, pctx.Headers, pctx.StartedAt = fin.StatusCode, fin.Headers, fin.StartedAt
	pctx.Extensions.Inference = fin.Extensions.Inference
	pctx.Extensions.Inference.CacheWriteTokens = cacheWrite
	p.OnFinish(context.Background(), pctx)
	s, sok := p.store.Get(cgstore.BilledPrefix + th.session)
	tb, tok := p.store.Get(cgstore.BilledThreadPrefix + th.session) // the primary thread's key is the session
	if sok != tok {
		t.Fatalf("session figure present=%v but thread figure present=%v: lifecycle.Finish writes both", sok, tok)
	}
	return string(s), string(tb), sok
}

// The next request's Ctx.PrevBilledInputThread is read from the thread key, so the size trigger
// can compare the thread's fill against the window (#2130: without it the summarizer never
// fires). Billed input is fresh + cache read + cache write.
func TestOnFinish_RecordsBilledInput(t *testing.T) {
	session, thread, ok := billedFor(t, 200, pipeline.AgentRoleMain, 1_000)
	if !ok || session != "49003" || thread != "49003" {
		t.Fatalf("billed input: session %q, thread %q (present=%v), want 49003 for both", session, thread, ok)
	}
}

// A failed request has no usage worth keeping, so the previous turn's figure stays.
func TestOnFinish_SkipsAFailedRequest(t *testing.T) {
	if session, _, ok := billedFor(t, 500, pipeline.AgentRoleMain, 0); ok {
		t.Errorf("failed request: recorded billed input %q, want nothing", session)
	}
}

func hasLiteLLM(r models.Resolver) bool {
	chain, ok := r.(models.Chain)
	if !ok {
		return false
	}
	for _, c := range chain {
		if _, ok := c.(*litellm.LiteLLM); ok {
			return true
		}
	}
	return false
}

// The LiteLLM map is fetched from the public internet, so it is the default only on a local
// install; a pod does not reach out unless its config says so. An explicit model_info always
// wins, and so does a model_info_url: an operator who pointed the plugin at a map asked for it.
func TestBuildWindows_ModelInfoDefaultFollowsLocalInstall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   windowsConfig
		local bool
		want  bool
	}{
		{"local default", windowsConfig{}, true, true},
		{"cluster default", windowsConfig{}, false, false},
		{"cluster explicit litellm", windowsConfig{ModelInfo: "litellm"}, false, true},
		{"cluster explicit url", windowsConfig{ModelInfoURL: "https://maps.example/model_prices.json"}, false, true},
		{"local explicit off", windowsConfig{ModelInfo: "off"}, true, false},
		{"local explicit OFF", windowsConfig{ModelInfo: " OFF "}, true, false},
	} {
		if got := hasLiteLLM(buildWindows(tc.cfg, tc.local)); got != tc.want {
			t.Errorf("%s: LiteLLM in chain = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The plugin's own default follows what BuildWithDeps told it.
func TestPlugin_ModelInfoDefaultFollowsLocalInstall(t *testing.T) {
	for _, local := range []bool{true, false} {
		p := New()
		p.SetLocalInstall(local)
		if err := p.Configure([]byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if got := hasLiteLLM(p.windows); got != local {
			t.Errorf("local=%v: LiteLLM in chain = %v, want %v", local, got, local)
		}
	}
}
