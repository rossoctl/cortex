package contextguru

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// Per-token rates for the integration test, distinct per tier so a figure shows which tier priced
// it: input 1x, cache write 1.25x, cache read 0.1x — the Anthropic ratios.
const (
	itInput      = 1e-6
	itCacheWrite = 1.25e-6
	itCacheRead  = 0.1e-6
)

func itRates(t *testing.T) *pricing.Registry {
	t.Helper()
	var r pricing.Rates
	for tier, rate := range map[pricing.Tier]float64{pricing.TierInput: itInput, pricing.TierCacheWrite: itCacheWrite,
		pricing.TierCacheRead: itCacheRead, pricing.TierOutput: 5e-6} {
		r.Base[tier], r.Set[tier] = rate, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

// formatEngine reformats JSON tool output: a Reformat, which the engine keys on the content it
// rewrote (Report.CacheKeys). That key is what lets a repeat be told from a first sight.
const formatEngine = `{"engine":{"pipeline":["format"]}}`

// collapseEngine (plugin_test.go) drops the output without stashing it, as dedup and extract_llm do
// here: this plugin caps marker_mode at summary, so the engine stashes nothing. Such a run had no
// key, and counted as unique on every turn, until the engine keyed it on the content too.

// itToolOutput is indented JSON, which format compacts.
func itToolOutput() string {
	rows := make([]map[string]any, 80)
	for i := range rows {
		rows[i] = map[string]any{"id": fmt.Sprintf("svc-%d", i), "region": []string{"us-east", "eu-de"}[i%2],
			"status": "healthy", "replicas": i%5 + 1}
	}
	b, _ := json.MarshalIndent(rows, "", "    ")
	return string(b)
}

// itConversation is a Claude Code shaped /v1/messages body: one Read whose output the engine
// compacts, then `turns` more question-and-answer pairs. Each turn re-sends the same tool output,
// as an agent does.
func itConversation(turns int) []byte {
	msgs := []any{
		map[string]any{"role": "user", "content": "read the log"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "/x.log"}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "t1", "content": itToolOutput()}}},
	}
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d", i)},
			map[string]any{"role": "user", "content": fmt.Sprintf("question %d", i)})
	}
	b, _ := json.Marshal(map[string]any{"model": "claude-haiku-4-5", "max_tokens": 64, "messages": msgs})
	return b
}

// itTurn runs one request through the full request -> response -> finish lifecycle and returns
// the context-guru saving on the cost record inference-parser published.
func itTurn(t *testing.T, pipe *pipeline.Pipeline, session string, body []byte, usage string) event.Saving {
	t.Helper()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound, Method: "POST", Scheme: "https",
		Host: "api.anthropic.com", Path: "/v1/messages", Headers: http.Header{},
		Body: body, Session: &pipeline.SessionView{ID: session}, StartedAt: time.Now(),
	}
	if a := pipe.Run(context.Background(), pctx); a.Type == pipeline.Reject {
		t.Fatalf("request pass rejected: %+v", a)
	}
	if !pctx.BodyMutated() {
		t.Fatal("the engine did not compact the request, so there is no saving to price")
	}
	pctx.StatusCode = 200
	pctx.ResponseHeaders = http.Header{}
	pctx.ResponseBody = []byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":` + usage + `}`)
	if a := pipe.RunResponse(context.Background(), pctx); a.Type == pipeline.Reject {
		t.Fatalf("response pass rejected: %+v", a)
	}
	// inference-parser is a StreamingResponder: only the frame pass feeds it the body, and its
	// final frame is where it settles cost. See runFinishedAnthropicRequest.
	if a := pipe.RunResponseFrame(context.Background(), pctx, pctx.ResponseBody, true); a.Type == pipeline.Reject {
		t.Fatalf("response frame pass rejected: %+v", a)
	}
	pipe.RunFinish(context.Background(), pctx, pipeline.Outcome{FinalAction: pipeline.OutcomeAllow, StatusCode: 200})

	ev, ok := pctx.Extensions.Custom[event.Key+pipeline.PluginEventSuffix].(event.Event)
	if !ok {
		t.Fatal("no cost record was published")
	}
	for _, s := range ev.Avoided {
		if s.Component == "context-guru" {
			return s
		}
	}
	t.Fatalf("cost record has no context-guru saving: %+v", ev.Avoided)
	return event.Saving{}
}

// TestIntegration_RepeatedCompactionIsPricedAsACacheRead is the over-report scenario end to end,
// through a built [inference-parser, context-guru] pipeline with a real rate table and real
// Anthropic usage parsing:
//
//  1. Turn 1 compacts a tool output for the first time. Its response writes the cache, so the
//     saving is priced at the cache-write rate.
//  2. Turns 2 and 3 re-send the same output, and the engine compacts it again. Without compaction
//     it would now be in the cached prefix, so the saving is priced at the cache-read rate: a tenth
//     of an input token, not 1.25x.
//  3. A new session sending the same output is a first sight again.
//
// Before the fix every turn was priced like turn 1, which is how a long session reported about
// five times the saving it made. Run for a Reformat and for an Offload that stashes nothing.
func TestIntegration_RepeatedCompactionIsPricedAsACacheRead(t *testing.T) {
	for name, engine := range map[string]string{"reformat": formatEngine, "lossy offload": collapseEngine} {
		t.Run(name, func(t *testing.T) { repeatScenario(t, engine) })
	}
}

func repeatScenario(t *testing.T, engine string) {
	pipe, err := plugins.BuildWithDeps([]config.PluginEntry{
		{Name: "inference-parser"},
		{Name: "context-guru", Config: json.RawMessage(engine)},
	}, plugins.Deps{Pricing: itRates(t)})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	const (
		writing = `{"input_tokens":20,"output_tokens":5,"cache_creation_input_tokens":3000}`
		hitting = `{"input_tokens":20,"output_tokens":5,"cache_read_input_tokens":3000,"cache_creation_input_tokens":30}`
	)

	first := itTurn(t, pipe, "s1", itConversation(0), writing)
	if first.Tier != pricing.TierCacheWrite.String() || first.TokensAvoided <= 0 {
		t.Fatalf("turn 1 = %+v, want a cache-write saving", first)
	}
	assertPriced(t, "turn 1", first, itCacheWrite)

	for turn := 2; turn <= 3; turn++ {
		s := itTurn(t, pipe, "s1", itConversation(turn-1), hitting)
		if s.Tier != pricing.TierCacheRead.String() {
			t.Fatalf("turn %d tier = %q, want cache-read: the output was compacted on an earlier turn", turn, s.Tier)
		}
		assertPriced(t, fmt.Sprintf("turn %d", turn), s, itCacheRead)
		if s.USD >= first.USD/5 {
			t.Errorf("turn %d saved $%.8f against turn 1's $%.8f: a repeat must be priced far below a first sight",
				turn, s.USD, first.USD)
		}
	}

	other := itTurn(t, pipe, "s2", itConversation(0), writing)
	if other.Tier != pricing.TierCacheWrite.String() {
		t.Fatalf("new session tier = %q, want cache-write: content seen in s1 is new to s2", other.Tier)
	}
	assertPriced(t, "new session", other, itCacheWrite)
}

// assertPriced checks a saving is all of its tokens at one rate, to the micro-dollar settle rounds to.
func assertPriced(t *testing.T, label string, s event.Saving, rate float64) {
	t.Helper()
	if want := float64(s.TokensAvoided) * rate; math.Abs(s.USD-want) > 1e-6 {
		t.Errorf("%s USD = %.10f, want %d tokens x %g = %.10f", label, s.USD, s.TokensAvoided, rate, want)
	}
}
