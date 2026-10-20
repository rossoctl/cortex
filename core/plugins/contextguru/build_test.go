package contextguru

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"

	// Register inference-parser so context-guru's RequiresAny is satisfiable.
	_ "github.com/rossoctl/cortex/core/plugins/inferenceparser"
	// Register tool-prune, the other request-body writer context-guru now chains with.
	_ "github.com/rossoctl/cortex/core/plugins/toolprune"
)

// unwrapConfigured finds the *ContextGuru among a built pipeline's plugins.
// plugins.Build wraps every Configurable plugin (context-guru included) in
// pipeline's unexported configuredPlugin, which embeds the inner Plugin under
// an exported field named "Plugin" — reflect can still reach it from outside
// the pipeline package because the field itself is exported, even though the
// wrapper type is not. This is a test-only hook; no production API changes.
func unwrapConfigured(pl pipeline.Plugin) pipeline.Plugin {
	if cg, ok := pl.(*ContextGuru); ok {
		return cg
	}
	v := reflect.ValueOf(pl)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return pl
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return pl
	}
	f := v.FieldByName("Plugin")
	if !f.IsValid() || !f.CanInterface() {
		return pl
	}
	inner, ok := f.Interface().(pipeline.Plugin)
	if !ok {
		return pl
	}
	return inner
}

// TestBuild_InChainAfterInferenceParser confirms the plugin assembles on the
// outbound chain when a parser precedes it.
func TestBuild_InChainAfterInferenceParser(t *testing.T) {
	p, err := plugins.Build([]config.PluginEntry{
		{Name: "inference-parser"},
		{Name: "context-guru", Config: json.RawMessage(collapseEngine)},
	})
	if err != nil {
		t.Fatalf("pipeline should build with [inference-parser, context-guru]: %v", err)
	}
	if got := len(p.Plugins()); got != 2 {
		t.Fatalf("expected 2 plugins, got %d", got)
	}
}

// TestBuild_FailsWhenBeforeParser confirms RequiresAny ordering is enforced:
// context-guru must appear after the parser it depends on.
func TestBuild_FailsWhenBeforeParser(t *testing.T) {
	if _, err := plugins.Build([]config.PluginEntry{
		{Name: "context-guru", Config: json.RawMessage(`{}`)},
		{Name: "inference-parser"},
	}); err == nil {
		t.Fatal("expected build failure: context-guru requires inference-parser earlier in the chain")
	}
}

// Request-body writers chain, so context-guru shares the outbound chain with
// tool-prune: tool-prune prunes the manifest, then context-guru compacts what is
// left. Both follow the parser, which must see the bytes the client sent.
func TestBuild_ChainsAfterToolPrune(t *testing.T) {
	p, err := plugins.Build([]config.PluginEntry{
		{Name: "inference-parser"},
		{Name: "tool-prune", Config: json.RawMessage(`{"remove": []}`)},
		{Name: "context-guru", Config: json.RawMessage(collapseEngine)},
	})
	if err != nil {
		t.Fatalf("pipeline should build with [inference-parser, tool-prune, context-guru]: %v", err)
	}
	if got := len(p.Plugins()); got != 3 {
		t.Fatalf("expected 3 plugins, got %d", got)
	}
}

// anthropicMessagesRequest is a minimal, valid Anthropic Messages API request
// body: enough for inference-parser's parseAnthropicRequest to return a
// non-nil extension (it requires a non-null "messages" array).
const anthropicMessagesRequest = `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`

// anthropicMessagesResponse is a minimal, valid Anthropic Messages API
// response carrying a cache-read usage figure, so inference-parser's
// toNeutral() sets the cache-read PresentKinds bit that context-guru's
// keep-alive gates on (UsageOK).
const anthropicMessagesResponse = `{"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":48000}}`

// runFinishedAnthropicRequest pushes one Anthropic /v1/messages request
// through the full request -> response -> finish lifecycle, mirroring the
// call sequence forwardproxy/server.go uses around its RunFinish calls.
func runFinishedAnthropicRequest(t *testing.T, pipe *pipeline.Pipeline, sessionID string) {
	t.Helper()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound, Method: "POST", Scheme: "https",
		Host: "api.anthropic.com", Path: "/v1/messages", Headers: http.Header{},
		Body:    []byte(anthropicMessagesRequest),
		Session: &pipeline.SessionView{ID: sessionID},
		// StartedAt is normally stamped by the listener (forwardproxy/server.go,
		// extproc) when it constructs the context; keepalive.Keeper.Record bails
		// out on a zero StartedAt (it is the anchor for the idle-timer math), so a
		// hand-built pctx must set it explicitly.
		StartedAt: time.Now(),
	}
	if action := pipe.Run(context.Background(), pctx); action.Type == pipeline.Reject {
		t.Fatalf("request pass rejected: %+v", action)
	}
	pctx.StatusCode = 200
	pctx.ResponseHeaders = http.Header{}
	pctx.ResponseBody = []byte(anthropicMessagesResponse)
	if action := pipe.RunResponse(context.Background(), pctx); action.Type == pipeline.Reject {
		t.Fatalf("response pass rejected: %+v", action)
	}
	// inference-parser implements StreamingResponder, so RunResponse (above) skips it by
	// design (pipeline.go: "Plugins implementing StreamingResponder are skipped here") and
	// RunResponseFrame is the only path that actually feeds it the response body — exactly
	// what every real listener does (see forwardproxy/server.go's calls around line 662).
	// Without this call pctx.Extensions.Inference never gains cache-read usage, and
	// context-guru's OnFinish silently records nothing.
	if action := pipe.RunResponseFrame(context.Background(), pctx, pctx.ResponseBody, true); action.Type == pipeline.Reject {
		t.Fatalf("response frame pass rejected: %+v", action)
	}
	pipe.RunFinish(context.Background(), pctx, pipeline.Outcome{FinalAction: pipeline.OutcomeAllow, StatusCode: 200})
}

// TestBuild_KeepAliveThroughPipeline exercises context-guru's keep-alive end
// to end through a built pipeline, with inference-parser doing the real
// Anthropic response parsing (no hand-set pctx.Extensions.Inference): two
// finished requests on one session should leave exactly one live keep-alive
// entry, and Stop should release it.
func TestBuild_KeepAliveThroughPipeline(t *testing.T) {
	pipe, err := plugins.Build([]config.PluginEntry{
		{Name: "inference-parser"},
		{Name: "context-guru", Config: json.RawMessage(`{"keep_alive":{"enabled":true,"min_prefix_tokens":1}}`)},
	})
	if err != nil {
		t.Fatalf("pipeline should build with [inference-parser, context-guru]: %v", err)
	}

	var cg *ContextGuru
	for _, pl := range pipe.Plugins() {
		if g, ok := unwrapConfigured(pl).(*ContextGuru); ok {
			cg = g
		}
	}
	if cg == nil {
		t.Fatal("context-guru instance not found in built pipeline")
	}
	if cg.keeper == nil {
		t.Fatal("keeper should be built when keep_alive.enabled is true")
	}

	ctx := context.Background()
	if err := pipe.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	runFinishedAnthropicRequest(t, pipe, "s-keepalive")
	runFinishedAnthropicRequest(t, pipe, "s-keepalive")

	if keys := cg.keeper.LiveSessionKeys(); len(keys) != 1 {
		t.Fatalf("expected 1 live keep-alive entry after two finished requests, got %d (%v)", len(keys), keys)
	}

	pipe.Stop(ctx)

	if keys := cg.keeper.LiveSessionKeys(); len(keys) != 0 {
		t.Fatalf("expected 0 live keep-alive entries after Stop, got %d (%v)", len(keys), keys)
	}
}
