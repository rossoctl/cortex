package litellm_budgettrack

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/inferenceparser"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// TestSSEEquivalence is the regression guard for the riskiest change in the
// pricing consolidation: this plugin dropping its own SSE token parser in favour
// of the counts inference-parser already publishes.
//
// The expected costs below were RECORDED from the plugin's own parser before that
// change, and must not be edited by it. Two parsers agreeing is the whole claim;
// a silent divergence in streamed pricing is invisible in production, because the
// only figure anyone sees is the one this plugin produces. If a case here moves,
// the two parsers disagree and the number people bill against changed.
//
// The `?beta=true` shape is the one that matters most. Claude Code posts there,
// and that path defers the cache counts from message_start to message_delta — the
// exact shape that once made a 33.8k-token turn record as 9 tokens.
//
// Rates are 10/12.5/1/50 per million, chosen so each tier's contribution is
// separable by eye in the total.
const (
	rateInput      = 10.0 / 1e6
	rateCacheWrite = 12.5 / 1e6
	rateCacheRead  = 1.0 / 1e6
	rateOutput     = 50.0 / 1e6
)

// sseCase is one canned streamed response and the cost it must price to.
type sseCase struct {
	name   string
	frames []string
	// wantMicros is the settled cost in millionths of a dollar. Integer so the
	// assertion cannot drift on float formatting.
	wantMicros int64
}

func sseCases() []sseCase {
	return []sseCase{{
		// Non-beta: message_start carries the full prompt split, message_delta
		// carries cumulative output.
		name: "non-beta prompt split on message_start",
		frames: []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":1000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":30000,"output_tokens":1}}}`,
			`{"type":"content_block_delta","delta":{"text":"hi"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":500}}`,
		},
		// 1000*10 + 2000*12.5 + 30000*1 + 500*50 = 10000+25000+30000+25000 micros
		wantMicros: 90_000,
	}, {
		// ?beta=true: message_start carries ONLY input_tokens; the cache counts
		// arrive on message_delta. A parser that reads usage from message_start
		// alone records almost nothing here.
		name: "beta defers cache counts to message_delta",
		frames: []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":1000,"output_tokens":1}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":30000,"output_tokens":500}}`,
		},
		wantMicros: 90_000,
	}, {
		// A later frame reporting zero must not clobber a real earlier count.
		name: "zero in a later frame does not clobber",
		frames: []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":1000,"cache_read_input_tokens":30000,"output_tokens":1}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"cache_read_input_tokens":0,"output_tokens":500}}`,
		},
		// 1000*10 + 30000*1 + 500*50 = 10000+30000+25000
		wantMicros: 65_000,
	}, {
		name: "cache-read-only turn, the steady state for a long agent session",
		frames: []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":169511,"output_tokens":1}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		},
		// 12*10 + 169511*1 + 9*50 = 120 + 169511 + 450
		wantMicros: 170_081,
	}, {
		name: "output only, no prompt counts reported at all",
		frames: []string{
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":400}}`,
		},
		wantMicros: 20_000,
	}}
}

// runSSE drives one case through the plugin's frame path and returns the settled
// cost event, if any.
func runSSE(t *testing.T, p *billing, c sseCase) (costevent.Event, bool) {
	t.Helper()
	pctx := &pipeline.Context{
		Path:            "/v1/messages?beta=true",
		Host:            "gw.internal:4000",
		ResponseHeaders: http.Header{},
	}
	pctx.ResponseHeaders.Set("Content-Type", "text/event-stream")
	// No cost header: streamed responses always report 0, which is what makes the
	// usage path load-bearing rather than a fallback nobody hits.
	// The parser carries the rate table now, and settles the cost on the terminal
	// frame. Running it with the resolver is what makes this an end-to-end proof of
	// the production path rather than a test of a stub.
	primeInference(t, pctx, c.frames, p.rates)

	for i, f := range c.frames {
		last := i == len(c.frames)-1
		if act := p.OnResponseFrame(context.Background(), pctx, []byte(f), last); act.Type != pipeline.Continue {
			t.Fatalf("frame %d: action = %v, want Continue", i, act.Type)
		}
	}

	raw, ok := pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix]
	if !ok {
		return costevent.Event{}, false
	}
	// Round-trip through JSON exactly as the listener does, so the test exercises
	// the wire shape rather than the in-memory struct.
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var ev costevent.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	return ev, true
}

func TestSSEEquivalence(t *testing.T) {
	for _, c := range sseCases() {
		t.Run(c.name, func(t *testing.T) {
			p := newPricedBudgetTrack(t)
			ev, ok := runSSE(t, p, c)
			if !ok {
				t.Fatal("no cost event was published")
			}
			if got := ev.Micros(); got != c.wantMicros {
				t.Errorf("cost = %d micros, want %d (delta %d)", got, c.wantMicros, got-c.wantMicros)
			}
		})
	}
}

// TestSSEEquivalence_HeaderCostStillWins pins that an authoritative gateway figure
// is preferred over any modelled one, streamed or not.
func TestSSEEquivalence_HeaderCostStillWins(t *testing.T) {
	p := newPricedBudgetTrack(t)
	pctx := &pipeline.Context{Path: "/v1/messages", Host: "gw.internal", ResponseHeaders: http.Header{}}
	pctx.ResponseHeaders.Set("Content-Type", "application/json")
	pctx.ResponseHeaders.Set(costing.ResponseCostHeader, "0.25")
	frames := []string{`{"usage":{"input_tokens":1000,"output_tokens":500}}`}
	primeInference(t, pctx, frames, p.rates)

	p.OnResponseFrame(context.Background(), pctx, []byte(frames[0]), true)

	raw, ok := pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix]
	if !ok {
		t.Fatal("no cost event")
	}
	b, _ := json.Marshal(raw)
	var ev costevent.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.CostUSD != 0.25 {
		t.Errorf("cost = %v, want the header's 0.25", ev.CostUSD)
	}
	if ev.Source != costevent.SourceGatewayHeader {
		t.Errorf("source = %q, want %q", ev.Source, costevent.SourceGatewayHeader)
	}
}

// spendFile gives each plugin instance its own ledger, so cases cannot contaminate
// each other through the daily total.
func spendFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "spend.json")
}

// primeInference runs the REAL inference-parser over the same frames, populating
// pctx.Extensions.Inference exactly as it would in a live pipeline.
//
// This runs on both sides of the migration, deliberately. Before it, the counts it
// publishes are unused and this only proves the two parsers see the same bytes;
// after, they are the plugin's only source. Keeping the shim identical across the
// change is what makes the recorded costs a real invariant rather than two
// unrelated measurements.
func primeInference(t *testing.T, pctx *pipeline.Context, frames []string, rates pricing.Resolver) {
	t.Helper()
	ip := inferenceparser.NewInferenceParser()
	ip.SetPricingResolver(rates)
	// The parser needs a request-side extension to fold into, and Stream must be
	// true or it treats a single last=true frame as a buffered JSON body.
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model:  "claude-opus-5",
		Stream: costing.IsEventStream(pctx),
	}
	for i, f := range frames {
		ip.OnResponseFrame(context.Background(), pctx, []byte(f), i == len(frames)-1)
	}
}

// newPricedBudgetTrack builds a configured plugin with the test rates.
//
// POST-MIGRATION: rates arrive by resolver injection, as plugins.BuildWithDeps
// does in production. This shim is the only thing in this file that changed with
// the migration — the recorded costs above were not touched, which is what makes
// them an equivalence proof rather than a fresh expectation.
func newPricedBudgetTrack(t *testing.T) *billing {
	t.Helper()
	p := New()
	raw, err := json.Marshal(map[string]any{
		"spend_file": spendFile(t),
		"max_budget": 1000.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	var r pricing.Rates
	for tier, v := range map[pricing.Tier]float64{
		pricing.TierInput:      rateInput,
		pricing.TierCacheWrite: rateCacheWrite,
		pricing.TierCacheRead:  rateCacheRead,
		pricing.TierOutput:     rateOutput,
	} {
		r.Base[tier], r.Set[tier] = v, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{
		{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return &billing{BudgetTrack: p, rates: pricing.NewRegistry(tab)}
}
