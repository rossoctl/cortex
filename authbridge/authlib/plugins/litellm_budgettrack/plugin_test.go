package litellm_budgettrack

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// configure builds a BudgetTrack with a temp-dir spend file and the given budget.
func configure(t *testing.T, maxBudget float64) *billing {
	t.Helper()
	p := New()
	cfg := budgetTrackConfig{
		SpendFile: filepath.Join(t.TempDir(), "spend.json"),
		MaxBudget: maxBudget,
	}
	raw, _ := json.Marshal(cfg)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	// No rates: the header path needs none, and a nil resolver is the state a binary
	// without pricing wiring is in.
	return &billing{BudgetTrack: p}
}

// TestOnResponseReadsResponseHeader is the regression guard for the core fix:
// the cost must be read from ResponseHeaders, not the request Headers.
func TestOnResponseReadsResponseHeader(t *testing.T) {
	p := configure(t, 5.00)
	pctx := &pipeline.Context{
		ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.0025"}},
	}

	if action := p.OnResponse(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("OnResponse() = %v, want Continue", action.Type)
	}
	if p.ledger.TotalSpend != 0.0025 {
		t.Errorf("TotalSpend = %v, want 0.0025", p.ledger.TotalSpend)
	}
	if p.ledger.TotalCalls != 1 {
		t.Errorf("TotalCalls = %d, want 1", p.ledger.TotalCalls)
	}
}

// TestOnResponseIgnoresRequestHeader guards against the original bug: the cost
// header on the request side (pctx.Headers) must NOT be accumulated.
func TestOnResponseIgnoresRequestHeader(t *testing.T) {
	p := configure(t, 5.00)
	pctx := &pipeline.Context{
		Headers:         http.Header{costing.ResponseCostHeader: {"0.0025"}}, // wrong place; must be ignored
		ResponseHeaders: http.Header{},
	}

	p.OnResponse(context.Background(), pctx)
	if p.ledger.TotalSpend != 0 {
		t.Errorf("TotalSpend = %v, want 0 (request-header cost must be ignored)", p.ledger.TotalSpend)
	}
}

// TestOnResponseFallsBackToOriginal covers the Anthropic /v1/messages case where
// only the pre-discount "-original" header is present.
func TestOnResponseFallsBackToOriginal(t *testing.T) {
	p := configure(t, 5.00)
	pctx := &pipeline.Context{
		ResponseHeaders: http.Header{costing.ResponseCostOriginalHeader: {"2.204e-05"}},
	}

	p.OnResponse(context.Background(), pctx)
	if p.ledger.TotalSpend != 2.204e-05 {
		t.Errorf("TotalSpend = %v, want 2.204e-05 (fallback header)", p.ledger.TotalSpend)
	}
}

// TestOnResponseBareHeaderWins verifies the effective (post-discount) header
// takes precedence over "-original" when both are present.
func TestOnResponseBareHeaderWins(t *testing.T) {
	p := configure(t, 5.00)
	pctx := &pipeline.Context{
		ResponseHeaders: http.Header{
			costing.ResponseCostHeader:         {"0.001"},
			costing.ResponseCostOriginalHeader: {"0.002"},
		},
	}

	p.OnResponse(context.Background(), pctx)
	if p.ledger.TotalSpend != 0.001 {
		t.Errorf("TotalSpend = %v, want 0.001 (bare header must win)", p.ledger.TotalSpend)
	}
}

// TestOnResponseIgnoresMissingOrInvalid verifies absent / non-positive / unparseable
// costs are skipped rather than corrupting the ledger.
func TestOnResponseIgnoresMissingOrInvalid(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
	}{
		{"missing", http.Header{}},
		{"zero", http.Header{costing.ResponseCostHeader: {"0"}}},
		{"negative", http.Header{costing.ResponseCostHeader: {"-1"}}},
		{"unparseable", http.Header{costing.ResponseCostHeader: {"abc"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := configure(t, 5.00)
			pctx := &pipeline.Context{ResponseHeaders: tc.headers}
			if action := p.OnResponse(context.Background(), pctx); action.Type != pipeline.Continue {
				t.Fatalf("OnResponse() = %v, want Continue", action.Type)
			}
			if p.ledger.TotalSpend != 0 || p.ledger.TotalCalls != 0 {
				t.Errorf("ledger mutated: spend=%v calls=%d, want 0/0", p.ledger.TotalSpend, p.ledger.TotalCalls)
			}
		})
	}
}

// TestOnRequestEnforcesBudget verifies OnRequest denies with 429 once the
// accumulated spend reaches the daily budget, and allows before that.
func TestOnRequestEnforcesBudget(t *testing.T) {
	p := configure(t, 0.001)

	// Under budget: allowed.
	if action := p.OnRequest(context.Background(), &pipeline.Context{}); action.Type != pipeline.Continue {
		t.Fatalf("OnRequest() under budget = %v, want Continue", action.Type)
	}

	// Accumulate past the budget via a response.
	p.OnResponse(context.Background(), &pipeline.Context{
		ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.002"}},
	})

	// Over budget: rejected with 429 / budget.exceeded.
	action := p.OnRequest(context.Background(), &pipeline.Context{})
	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() over budget = %v, want Reject", action.Type)
	}
	// Stop before dereferencing: a nil Violation must not panic the next lines.
	if action.Violation == nil {
		t.Fatal("Violation is nil, want 429 budget.exceeded")
	}
	if action.Violation.Status != http.StatusTooManyRequests {
		t.Errorf("Violation.Status = %d, want 429", action.Violation.Status)
	}
	if action.Violation.Code != "budget.exceeded" {
		t.Errorf("Violation.Code = %q, want budget.exceeded", action.Violation.Code)
	}
}

// TestLedgerPersistsAcrossInstances verifies the spend file is reloaded, so a
// restart on the same day resumes the accumulated total.
func TestLedgerPersistsAcrossInstances(t *testing.T) {
	spendFile := filepath.Join(t.TempDir(), "spend.json")
	raw, _ := json.Marshal(budgetTrackConfig{SpendFile: spendFile, MaxBudget: 5.00})

	// Wrapped, because the plugin no longer prices: something has to settle the cost
	// before there is anything to persist.
	p1 := &billing{BudgetTrack: New()}
	if err := p1.Configure(raw); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	p1.OnResponse(context.Background(), &pipeline.Context{
		ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.01"}},
	})

	p2 := New()
	if err := p2.Configure(raw); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if p2.ledger.TotalSpend != 0.01 {
		t.Errorf("reloaded TotalSpend = %v, want 0.01", p2.ledger.TotalSpend)
	}
}

// TestConfigureRejectsBadConfig verifies required-field and JSON validation.
func TestConfigureRejectsBadConfig(t *testing.T) {
	spend := filepath.Join(t.TempDir(), "spend.json")
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty spend_file", `{"max_budget": 5.0}`},
		{"zero max_budget", fmt.Sprintf(`{"spend_file": %q, "max_budget": 0}`, spend)},
		{"negative max_budget", fmt.Sprintf(`{"spend_file": %q, "max_budget": -1}`, spend)},
		{"invalid json", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := New().Configure(json.RawMessage(tc.raw)); err == nil {
				t.Errorf("Configure(%s) = nil, want error", tc.raw)
			}
		})
	}
}

// TestLoadLedgerResetsStaleDay verifies a spend file left over from a previous
// day is discarded on Configure rather than counted against today's budget.
func TestLoadLedgerResetsStaleDay(t *testing.T) {
	spend := filepath.Join(t.TempDir(), "spend.json")
	stale := `{"date":"2000-01-01","total_spend":9.99,"total_calls":42}`
	if err := os.WriteFile(spend, []byte(stale), 0o644); err != nil {
		t.Fatalf("seed spend file: %v", err)
	}

	p := New()
	raw, _ := json.Marshal(budgetTrackConfig{SpendFile: spend, MaxBudget: 5.00})
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	if p.ledger.Date != today {
		t.Errorf("ledger.Date = %q, want %q", p.ledger.Date, today)
	}
	if p.ledger.TotalSpend != 0 || p.ledger.TotalCalls != 0 {
		t.Errorf("stale ledger not reset: spend=%v calls=%d", p.ledger.TotalSpend, p.ledger.TotalCalls)
	}

	// A same-day ledger, by contrast, is preserved.
	sameDay := fmt.Sprintf(`{"date":%q,"total_spend":1.25,"total_calls":3}`, today)
	if err := os.WriteFile(spend, []byte(sameDay), 0o644); err != nil {
		t.Fatalf("seed same-day file: %v", err)
	}
	p2 := New()
	if err := p2.Configure(raw); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if p2.ledger.TotalSpend != 1.25 || p2.ledger.TotalCalls != 3 {
		t.Errorf("same-day ledger not preserved: spend=%v calls=%d", p2.ledger.TotalSpend, p2.ledger.TotalCalls)
	}
}

// TestConcurrentOnResponse exercises the mutex under concurrent responses.
// Run with -race to catch data races on the ledger.
func TestConcurrentOnResponse(t *testing.T) {
	p := configure(t, 1000.0) // high budget so nothing is rejected
	const goroutines = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			p.OnResponse(context.Background(), &pipeline.Context{
				ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.01"}},
			})
		}()
	}
	wg.Wait()

	if p.ledger.TotalCalls != goroutines {
		t.Errorf("TotalCalls = %d, want %d", p.ledger.TotalCalls, goroutines)
	}
	// 50 × 0.01 = 0.50, within float tolerance.
	if got := p.ledger.TotalSpend; got < 0.4999 || got > 0.5001 {
		t.Errorf("TotalSpend = %v, want ~0.50", got)
	}
}

// --- streaming (SSE usage) tests ---

// configurePriced builds a plugin whose rates reach it by injection, as
// plugins.BuildWithDeps does in production. Rates left at 0 are UNSET, not free:
// pricing.Cost refuses to price a tier that carried tokens without a rate.
// billing is a BudgetTrack plus the rate table the COST OWNER holds in production.
//
// The plugin no longer prices anything, so a test that wants a bill has to do what the
// pipeline does: let the cost owner settle first. settle runs the same authlib/costing
// entry points inference-parser calls, which keeps these tests about billing — the
// end-to-end proof that the parser drives them lives in sse_equivalence_test.go and
// forwardproxy_integration_test.go, both of which run the real parser.
type billing struct {
	*BudgetTrack
	rates pricing.Resolver
}

// OnResponse and OnResponseFrame settle first, then bill — the order the pipeline produces,
// since the parser sits later in the chain and the response pass runs in reverse.
//
// Overriding them on the wrapper rather than editing every call site keeps these tests
// reading as they did, which matters: their recorded costs are the equivalence proof that
// this refactor did not change anybody's bill.
func (b *billing) OnResponse(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	b.settle(pctx)
	return b.BudgetTrack.OnResponse(ctx, pctx)
}

func (b *billing) OnResponseFrame(ctx context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if last {
		b.settle(pctx)
	}
	return b.BudgetTrack.OnResponseFrame(ctx, pctx, frame, last)
}

// settle stands in for the cost owner: price the response, stash the outcome, publish the
// record.
func (b *billing) settle(pctx *pipeline.Context) {
	s := costing.Settle(pctx, b.rates)
	costing.Store(pctx, s)
	if s.Priced {
		costing.Publish(pctx, costing.Record(s, costing.Avoided(pctx, b.rates)))
	}
}

func configurePriced(t *testing.T, maxBudget float64, perTier map[pricing.Tier]float64) *billing {
	t.Helper()
	p := New()
	var r pricing.Rates
	for tier, v := range perTier {
		if v > 0 {
			r.Base[tier], r.Set[tier] = v, true
		}
	}
	tab, err := pricing.NewTable([]pricing.Entry{
		{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	rates := pricing.NewRegistry(tab)
	raw, _ := json.Marshal(budgetTrackConfig{
		SpendFile: filepath.Join(t.TempDir(), "spend.json"),
		MaxBudget: maxBudget,
	})
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	return &billing{BudgetTrack: p, rates: rates}
}

// pricedInference seeds the per-tier counts inference-parser would have published,
// which is now this plugin's only source for them.
func pricedInference(pctx *pipeline.Context, input, cacheWrite, cacheRead, output int) {
	pctx.Extensions.Inference = &pipeline.InferenceExtension{
		Model:            "claude-opus-5",
		InputTokens:      input,
		CacheWriteTokens: cacheWrite,
		CacheReadTokens:  cacheRead,
		OutputTokens:     output,
	}
}

// Anthropic-style streamed /v1/messages frames: input in message_start,
// cumulative output in message_delta, both in message_stop.
const (
	frameMessageStart = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}` + "\n"
	frameContentDelta = "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n"
	frameMessageDelta = "event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":0,"output_tokens":40}}` + "\n"
)

// TestStreamingPricesFromUsage: header cost is absent/0 (streaming), so cost is
// computed from the parsed usage and the configured per-token rates.
func TestStreamingPricesFromUsage(t *testing.T) {
	p := configurePriced(t, 5.00, map[pricing.Tier]float64{pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6}) // $1/1M in, $5/1M out
	ctx := context.Background()
	pctx := &pipeline.Context{ResponseHeaders: http.Header{}} // streamed: no cost header
	pricedInference(pctx, 100, 0, 0, 40)                      // what inference-parser publishes

	p.OnResponseFrame(ctx, pctx, []byte(frameMessageStart), false)
	p.OnResponseFrame(ctx, pctx, []byte(frameContentDelta), false)
	p.OnResponseFrame(ctx, pctx, []byte(frameMessageDelta), false)
	p.OnResponseFrame(ctx, pctx, nil, true) // terminal frame settles cost

	want := 100*1e-6 + 40*5e-6 // 0.0001 + 0.0002 = 0.0003
	if got := p.ledger.TotalSpend; got < want-1e-12 || got > want+1e-12 {
		t.Errorf("TotalSpend = %v, want %v", got, want)
	}
	if p.ledger.TotalCalls != 1 {
		t.Errorf("TotalCalls = %d, want 1", p.ledger.TotalCalls)
	}
}

// TestStreamingWithoutPricesRecordsZero: no per-token rates configured -> a
// streamed response cannot be priced and must not corrupt the ledger.
func TestStreamingWithoutPricesRecordsZero(t *testing.T) {
	p := configure(t, 5.00) // no prices
	ctx := context.Background()
	pctx := &pipeline.Context{ResponseHeaders: http.Header{}}
	p.OnResponseFrame(ctx, pctx, []byte(frameMessageStart), false)
	p.OnResponseFrame(ctx, pctx, []byte(frameMessageDelta), false)
	p.OnResponseFrame(ctx, pctx, nil, true)
	if p.ledger.TotalSpend != 0 || p.ledger.TotalCalls != 0 {
		t.Errorf("ledger mutated without prices: spend=%v calls=%d", p.ledger.TotalSpend, p.ledger.TotalCalls)
	}
}

// TestHeaderCostWinsOverUsage: when the terminal frame has a real header cost
// (non-streaming buffered path delivered as a single frame), it is used and the
// per-token pricing is ignored.
func TestHeaderCostWinsOverUsage(t *testing.T) {
	p := configurePriced(t, 5.00, map[pricing.Tier]float64{pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6})
	pctx := &pipeline.Context{ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.02"}}}
	// single-frame buffered json also carries usage, which must be ignored
	body := []byte(`data: {"usage":{"prompt_tokens":100,"completion_tokens":40}}`)
	p.OnResponseFrame(context.Background(), pctx, body, true)
	if got := p.ledger.TotalSpend; got != 0.02 {
		t.Errorf("TotalSpend = %v, want 0.02 (header cost must win)", got)
	}
}

// TestOnResponseFrameOriginalFallback: streamed header 0 but non-streaming
// -original present on the terminal frame is still honored.
func TestOnResponseFrameOriginalFallback(t *testing.T) {
	p := configurePriced(t, 5.00, map[pricing.Tier]float64{pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6})
	pctx := &pipeline.Context{ResponseHeaders: http.Header{costing.ResponseCostOriginalHeader: {"6.688e-05"}}}
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	if got := p.ledger.TotalSpend; got < 6.687e-05 || got > 6.689e-05 {
		t.Errorf("TotalSpend = %v, want 6.688e-05", got)
	}
}

// TestStreamingBareFrames reflects reality: the sseframe reader strips the
// "data:" prefix, so OnResponseFrame receives bare JSON payloads.

// TestCacheTierPricing is the PR #816 must-fix: cache tiers must be priced
// separately, not flat at input_cost_per_token. Uses the real Claude Code turn
// from cortex#811 (input 9, cache_creation 3755, cache_read 30008).
func TestCacheTierPricing(t *testing.T) {
	p := configurePriced(t, 100, map[pricing.Tier]float64{
		pricing.TierInput:      1e-6,
		pricing.TierCacheWrite: 1.25e-6, // write premium
		pricing.TierCacheRead:  0.1e-6,  // read discount
		pricing.TierOutput:     5e-6,
	})
	pctx := &pipeline.Context{ResponseHeaders: http.Header{"Content-Type": {"text/event-stream"}}}
	pricedInference(pctx, 9, 3755, 30008, 100)
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	// Quantized to micros, which is a deliberate change: pricing.Cost returns
	// integer millionths of a dollar so bucket addition stays exact, and the ledger
	// now agrees with /v1/usage to the last digit instead of diverging in the 7th
	// decimal. The lost precision is a millionth of a dollar per request.
	exact := 9*1e-6 + 3755*1.25e-6 + 30008*0.1e-6 + 100*5e-6
	want := math.Round(exact*1e6) / 1e6
	if got := p.ledger.TotalSpend; got < want-1e-12 || got > want+1e-12 {
		t.Errorf("TotalSpend = %v, want %v (per-tier pricing, micro-quantized from %v)", got, want, exact)
	}
	// Guard against a regression to flat pricing: flat would be far higher.
	flat := (9+3755+30008)*1e-6 + 100*5e-6
	if p.ledger.TotalSpend >= flat {
		t.Errorf("priced flat (%v) — cache tiers not applied", flat)
	}
}

// TestCacheTiersWithoutRatesAreUnpriced records a deliberate behaviour change.
//
// This plugin used to default an unset cache rate to the uncached input rate. That
// silently OVERSTATED a cache read by 10x, and the request still counted as priced,
// so the error was invisible. pricing.Cost's per-tier invariant replaces it: a tier
// that carried tokens with no rate makes the whole request unpriced, which is a
// visible gap instead of a plausible wrong number.
func TestCacheTiersWithoutRatesAreUnpriced(t *testing.T) {
	p := configurePriced(t, 100, map[pricing.Tier]float64{pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6}) // no cache rates
	pctx := &pipeline.Context{ResponseHeaders: http.Header{"Content-Type": {"text/event-stream"}}}
	pricedInference(pctx, 10, 20, 30, 40) // cache tiers carry tokens
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	if p.ledger.TotalSpend != 0 {
		t.Errorf("TotalSpend = %v, want 0 — cache tiers carried tokens with no rate", p.ledger.TotalSpend)
	}
	if p.ledger.TotalCalls != 0 {
		t.Errorf("TotalCalls = %d, want 0 — an unpriced request must not count as priced", p.ledger.TotalCalls)
	}
	if _, ok := pctx.Extensions.Custom[p.Name()+pipeline.PluginEventSuffix]; ok {
		t.Error("an unpriced request published a cost event")
	}
}

// TestNonFiniteCostRejected guards the data-integrity fix from PR #815 review:
// strconv.ParseFloat accepts "NaN"/"Inf", both slip past a bare `cost <= 0`
// check, poison TotalSpend, and break the JSON marshal. The ledger must stay
// clean and its file must not be overwritten with garbage.
func TestNonFiniteCostRejected(t *testing.T) {
	for _, hdr := range []string{"NaN", "Inf", "+Inf", "-Inf"} {
		t.Run(hdr, func(t *testing.T) {
			p := configure(t, 5.00)
			p.OnResponse(context.Background(), &pipeline.Context{
				ResponseHeaders: http.Header{costing.ResponseCostHeader: {hdr}},
			})
			if p.ledger.TotalSpend != 0 || p.ledger.TotalCalls != 0 {
				t.Errorf("%s: ledger mutated: spend=%v calls=%d", hdr, p.ledger.TotalSpend, p.ledger.TotalCalls)
			}
			// The spend file must remain valid JSON (not overwritten with garbage).
			if data, err := os.ReadFile(p.cfg.SpendFile); err == nil && len(data) > 0 {
				var l spendLedger
				if json.Unmarshal(data, &l) != nil {
					t.Errorf("%s: spend file corrupted: %s", hdr, data)
				}
			}
		})
	}
}

// TestCapabilitiesDeclaresReadsBody is the must-fix from PR #816 review: the
// plugin parses the response body, so it must declare ReadsBody or the extproc
// listener won't buffer the body (streamed accounting records nothing).
func TestCapabilitiesDeclaresReadsBody(t *testing.T) {
	if !New().Capabilities().ReadsBody {
		t.Error("Capabilities().ReadsBody = false; extproc will not buffer the body and streamed cost is lost")
	}
}

// TestOnResponseFrameSettlesOnce guards the exactly-once contract: a second
// terminal dispatch (e.g. extproc header + buffered-body phases) must not
// double-charge the ledger.
func TestOnResponseFrameSettlesOnce(t *testing.T) {
	p := configurePriced(t, 5.00, map[pricing.Tier]float64{pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6})
	ctx := context.Background()
	pctx := &pipeline.Context{ResponseHeaders: http.Header{}}
	pricedInference(pctx, 100, 0, 0, 40)
	p.OnResponseFrame(ctx, pctx, nil, true) // first terminal — charges
	p.OnResponseFrame(ctx, pctx, nil, true) // second terminal — must be a no-op
	want := 100*1e-6 + 40*5e-6
	if p.ledger.TotalCalls != 1 {
		t.Errorf("TotalCalls = %d, want 1 (double terminal dispatch must not double-charge)", p.ledger.TotalCalls)
	}
	if got := p.ledger.TotalSpend; got < want-1e-12 || got > want+1e-12 {
		t.Errorf("TotalSpend = %v, want %v", got, want)
	}
}

// TestZeroCostHeaderNonStreamedNotRepriced: a genuine free call (cost header
// "0", non-streamed) must be charged 0, not re-priced from its usage block.
func TestZeroCostHeaderNonStreamedNotRepriced(t *testing.T) {
	p := configurePriced(t, 5.00, map[pricing.Tier]float64{pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6})
	pctx := &pipeline.Context{ResponseHeaders: http.Header{
		costing.ResponseCostHeader: {"0"},
		"Content-Type":             {"application/json"},
	}}
	p.OnResponseFrame(context.Background(), pctx, []byte(`{"usage":{"input_tokens":100,"output_tokens":40}}`), true)
	if p.ledger.TotalSpend != 0 || p.ledger.TotalCalls != 0 {
		t.Errorf("free non-streamed call re-priced from usage: spend=%v calls=%d", p.ledger.TotalSpend, p.ledger.TotalCalls)
	}
}

// TestZeroCostHeaderStreamedPricesFromUsage: streamed responses always report a
// 0 cost header, so the usage fallback must still apply for text/event-stream.
func TestZeroCostHeaderStreamedPricesFromUsage(t *testing.T) {
	p := configurePriced(t, 5.00, map[pricing.Tier]float64{pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6})
	pctx := &pipeline.Context{ResponseHeaders: http.Header{
		costing.ResponseCostHeader: {"0"},
		"Content-Type":             {"text/event-stream; charset=utf-8"},
	}}
	pricedInference(pctx, 100, 0, 0, 40)
	p.OnResponseFrame(context.Background(), pctx, nil, true)
	want := 100*1e-6 + 40*5e-6
	if got := p.ledger.TotalSpend; got < want-1e-12 || got > want+1e-12 {
		t.Errorf("streamed zero-header call not priced from usage: got %v want %v", got, want)
	}
}

// getCostEvent pulls the emitted cost event out of pctx.Extensions.Custom
// under the same key the listener would read. Nil when nothing was emitted.
func getCostEvent(t *testing.T, pctx *pipeline.Context) *costevent.Event {
	t.Helper()
	if pctx.Extensions.Custom == nil {
		return nil
	}
	v, ok := pctx.Extensions.Custom["litellm-budget-track"+pipeline.PluginEventSuffix].(costevent.Event)
	if !ok {
		return nil
	}
	return &v
}

// TestPluginNameMatchesCostEventKey pins the plugin name to the constant the
// aggregator and abctl look the event up by. A rename on one side only would
// make every consumer silently stop seeing costs.
func TestPluginNameMatchesCostEventKey(t *testing.T) {
	if got := New().Name(); got != costevent.PluginName {
		t.Errorf("Name() = %q, costevent.PluginName = %q", got, costevent.PluginName)
	}
}

// TestEmitCostWireFormatIsAdditive proves the wire stayed backward compatible when
// provenance was added.
//
// The four original tags must serialize with the same names and values, and a
// consumer that knows only those four must still decode correctly. abctl decodes
// these tags from a separate module that this change does not rebuild, so a
// breaking drift here would silently blank its COST column rather than fail a
// build.
func TestEmitCostWireFormatIsAdditive(t *testing.T) {
	p := configure(t, 10)
	pctx := &pipeline.Context{ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.25"}}}
	p.OnResponse(context.Background(), pctx)

	ev := getCostEvent(t, pctx)
	if ev == nil {
		t.Fatal("no cost event emitted")
	}
	b, err := json.Marshal(*ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// The full shape, with provenance appended last.
	const want = `{"cost_usd":0.25,"source":"gateway-header","daily_total_usd":0.25,"daily_max_usd":10,"provenance":"authoritative","settled":true}`
	if string(b) != want {
		t.Errorf("wire format changed:\n got %s\nwant %s", b, want)
	}

	// The compatibility claim itself: a consumer written against only the original
	// four fields decodes this unchanged. This is what "additive" has to mean.
	var legacy struct {
		CostUSD       float64 `json:"cost_usd"`
		Source        string  `json:"source"`
		DailyTotalUSD float64 `json:"daily_total_usd"`
		DailyMaxUSD   float64 `json:"daily_max_usd"`
	}
	if err := json.Unmarshal(b, &legacy); err != nil {
		t.Fatalf("a four-field consumer failed to decode: %v", err)
	}
	if legacy.CostUSD != 0.25 || legacy.Source != costevent.SourceGatewayHeader ||
		legacy.DailyTotalUSD != 0.25 || legacy.DailyMaxUSD != 10 {
		t.Errorf("four-field decode = %+v, want the original values intact", legacy)
	}
}

// TestEmitCost_ProvenanceOmittedWhenAbsent pins that an event carrying no
// provenance omits the key entirely, so an older producer's bytes stay byte-identical
// to what they were before the field existed.
func TestEmitCost_ProvenanceOmittedWhenAbsent(t *testing.T) {
	b, err := json.Marshal(costevent.Event{CostUSD: 0.25, Source: costevent.SourceGatewayHeader})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "provenance") {
		t.Errorf("provenance was emitted for an event that has none: %s", b)
	}
}

// TestEmitCost_HeaderPath: OnResponse (buffered) prices from the header
// and emits a costEvent tagged gateway-header, with DailyTotalUSD matching
// the ledger's post-add state and DailyMaxUSD from config.
func TestEmitCost_HeaderPath(t *testing.T) {
	p := configure(t, 5.00)
	pctx := &pipeline.Context{ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.0025"}}}
	p.OnResponse(context.Background(), pctx)

	ev := getCostEvent(t, pctx)
	if ev == nil {
		t.Fatal("no costEvent emitted")
	}
	if ev.CostUSD != 0.0025 {
		t.Errorf("CostUSD = %v, want 0.0025", ev.CostUSD)
	}
	if ev.Source != costevent.SourceGatewayHeader {
		t.Errorf("Source = %q, want %s", ev.Source, costevent.SourceGatewayHeader)
	}
	if ev.DailyTotalUSD != 0.0025 {
		t.Errorf("DailyTotalUSD = %v, want 0.0025", ev.DailyTotalUSD)
	}
	if ev.DailyMaxUSD != 5.00 {
		t.Errorf("DailyMaxUSD = %v, want 5.00", ev.DailyMaxUSD)
	}

	// A second priced response on the same plugin must show
	// DailyTotalUSD accumulating while CostUSD stays per-response.
	// Without this a bug that emitted per-call cost as the daily
	// total would pass every other assertion here.
	pctx2 := &pipeline.Context{ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.0025"}}}
	p.OnResponse(context.Background(), pctx2)
	ev2 := getCostEvent(t, pctx2)
	if ev2 == nil {
		t.Fatal("no costEvent emitted on second response")
	}
	if ev2.CostUSD != 0.0025 {
		t.Errorf("second CostUSD = %v, want 0.0025 (per-response, not cumulative)", ev2.CostUSD)
	}
	if ev2.DailyTotalUSD != 0.0050 {
		t.Errorf("second DailyTotalUSD = %v, want 0.0050 (accumulated across both responses)", ev2.DailyTotalUSD)
	}
}

// TestEmitCost_UsageFallback: streamed responses (0-cost header) get priced
// from the token counters and the emitted event names the fallback source.
func TestEmitCost_UsageFallback(t *testing.T) {
	p := configurePriced(t, 5.00, map[pricing.Tier]float64{pricing.TierInput: 1e-6, pricing.TierOutput: 5e-6})
	pctx := &pipeline.Context{ResponseHeaders: http.Header{
		costing.ResponseCostHeader: {"0"},
		"Content-Type":             {"text/event-stream; charset=utf-8"},
	}}
	pricedInference(pctx, 100, 0, 0, 40)
	p.OnResponseFrame(context.Background(), pctx, nil, true)

	ev := getCostEvent(t, pctx)
	if ev == nil {
		t.Fatal("no costEvent emitted")
	}
	if ev.Source != costevent.SourceUsageFallback {
		t.Errorf("Source = %q, want %s", ev.Source, costevent.SourceUsageFallback)
	}
	want := 100*1e-6 + 40*5e-6
	if ev.CostUSD < want-1e-12 || ev.CostUSD > want+1e-12 {
		t.Errorf("CostUSD = %v, want %v", ev.CostUSD, want)
	}
	// Daily fields populated on the streaming path too.
	if ev.DailyTotalUSD < want-1e-12 || ev.DailyTotalUSD > want+1e-12 {
		t.Errorf("DailyTotalUSD = %v, want %v (matches first-response cost)", ev.DailyTotalUSD, want)
	}
	if ev.DailyMaxUSD != 5.00 {
		t.Errorf("DailyMaxUSD = %v, want 5.00", ev.DailyMaxUSD)
	}
}

// TestEmitCost_NoEmitWhenUnpriced: a response whose header is missing or invalid must NOT
// emit a cost event (matching the ledger-untouched invariant asserted by
// TestOnResponseIgnoresMissingOrInvalid).
//
// A header of exactly "0" is deliberately NOT in that set. On a non-streamed response it is
// the gateway saying the call was free — an answer, not an absence — and publishing it as a
// settled zero is what stops the usage aggregator from re-pricing a free call from its own
// rate table. See TestSettledZero_OnlyForADeclaredFreeCall.
//
// Before cortex #972 this asymmetry existed by accident: OnResponse handled only a positive
// header, so the settled zero was published on the frame path and silently dropped on the
// buffered one. The two now share one decision (costing.Settle), so a listener choosing
// OnResponse no longer reports a different cost from one choosing OnResponseFrame.
func TestEmitCost_NoEmitWhenUnpriced(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
	}{
		{"missing", http.Header{}},
		{"unparseable", http.Header{costing.ResponseCostHeader: {"abc"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := configure(t, 5.00)
			pctx := &pipeline.Context{ResponseHeaders: tc.headers}
			p.OnResponse(context.Background(), pctx)
			if ev := getCostEvent(t, pctx); ev != nil {
				t.Errorf("costEvent emitted on unpriced response: %+v", *ev)
			}
		})
	}
}

// A declared-free call publishes a settled zero on BOTH response paths, and adds nothing to
// the ledger. The two halves matter for different reasons: the event stops the aggregator
// inventing a cost, and the untouched ledger keeps a free call out of the budget.
func TestEmitCost_DeclaredZeroIsPublishedOnEitherPath(t *testing.T) {
	for _, name := range []string{"OnResponse", "OnResponseFrame"} {
		t.Run(name, func(t *testing.T) {
			p := configure(t, 5.00)
			pctx := &pipeline.Context{ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0"}}}
			if name == "OnResponse" {
				p.OnResponse(context.Background(), pctx)
			} else {
				p.OnResponseFrame(context.Background(), pctx, nil, true)
			}
			ev := getCostEvent(t, pctx)
			if ev == nil {
				t.Fatal("no event for a gateway-declared free call; the aggregator will re-price it")
			}
			if !ev.Settled || ev.CostUSD != 0 {
				t.Errorf("event = %+v, want a settled zero", *ev)
			}
			if p.ledger.TotalSpend != 0 || p.ledger.TotalCalls != 0 {
				t.Errorf("ledger moved on a free call: spend=%v calls=%d", p.ledger.TotalSpend, p.ledger.TotalCalls)
			}
		})
	}
}

// TestOnRequestRecordsDenyInvocation: the 429 path must record a deny
// Invocation with the spend snapshot, or phase:"denied" never surfaces.
func TestOnRequestRecordsDenyInvocation(t *testing.T) {
	p := configure(t, 0.001)

	// Push past the budget so the next OnRequest denies.
	p.OnResponse(context.Background(), &pipeline.Context{
		ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.002"}},
	})

	// Stamp Plugin+Phase like Pipeline.Run does — without it Record leaves
	// Phase:"" and the listener's FilteredByPhase drops the invocation.
	pctx := &pipeline.Context{}
	pctx.SetCurrentPlugin("litellm-budget-track", pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()

	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Reject {
		t.Fatalf("OnRequest() over budget = %v, want Reject", action.Type)
	}

	if pctx.Extensions.Invocations == nil {
		t.Fatal("no Invocations recorded on deny path")
	}
	inv := pctx.Extensions.Invocations.Inbound
	if len(inv) != 1 {
		t.Fatalf("Inbound invocations = %d, want 1", len(inv))
	}
	got := inv[0]
	if got.Plugin != "litellm-budget-track" {
		t.Errorf("Plugin = %q, want litellm-budget-track", got.Plugin)
	}
	if got.Phase != pipeline.InvocationPhaseRequest {
		t.Errorf("Phase = %q, want request (else listener FilteredByPhase drops it)", got.Phase)
	}
	if got.Action != pipeline.ActionDeny {
		t.Errorf("Action = %q, want deny", got.Action)
	}
	if got.Reason != "budget.exceeded" {
		t.Errorf("Reason = %q, want budget.exceeded", got.Reason)
	}
	// Snapshot must match the ledger the plugin actually checked — not a
	// stale zero from before the OnResponse above. Values match the 429
	// wire message's precision.
	if got.Details["daily_total_usd"] != "0.0020" {
		t.Errorf("Details[daily_total_usd] = %q, want 0.0020", got.Details["daily_total_usd"])
	}
	if got.Details["daily_max_usd"] != "0.00" {
		t.Errorf("Details[daily_max_usd] = %q, want 0.00", got.Details["daily_max_usd"])
	}
	if got.Details["total_calls"] != "1" {
		t.Errorf("Details[total_calls] = %q, want 1", got.Details["total_calls"])
	}
}

// TestOnRequestUnderBudgetRecordsNothing: allow-path stays clean; an
// always-record plugin would clutter every timeline.
func TestOnRequestUnderBudgetRecordsNothing(t *testing.T) {
	p := configure(t, 5.00)
	pctx := &pipeline.Context{}
	pctx.SetCurrentPlugin("litellm-budget-track", pipeline.InvocationPhaseRequest)
	defer pctx.ClearCurrentPlugin()
	if action := p.OnRequest(context.Background(), pctx); action.Type != pipeline.Continue {
		t.Fatalf("OnRequest() under budget = %v, want Continue", action.Type)
	}
	if pctx.Extensions.Invocations != nil && len(pctx.Extensions.Invocations.Inbound) > 0 {
		t.Errorf("under-budget OnRequest recorded %d invocations, want 0",
			len(pctx.Extensions.Invocations.Inbound))
	}
}

// TestEndToEnd_CostReachesUsageAggregator closes the loop this plugin's cost
// event depends on but no unit test covers: the plugin writes to
// pctx.Extensions.Custom, a listener promotes that to SessionEvent.Plugins via
// pipeline.SnapshotPlugins, and session.Store.Append fans the event out to the
// usage Aggregator as a Recorder.
//
// Every step there is a separate package, and the ordering matters — if the
// listener appended before snapshotting the plugin map, or SnapshotPlugins
// dropped the key, /v1/usage would silently report no cost while every unit test
// still passed. This asserts the composed path, not the pieces.
func TestEndToEnd_CostReachesUsageAggregator(t *testing.T) {
	p := configure(t, 10)
	agg := usage.New()
	store := session.New(time.Hour, 100, 10)
	store.AddRecorder(agg)

	// The plugin prices a response, exactly as OnResponse would in a pipeline.
	pctx := &pipeline.Context{ResponseHeaders: http.Header{costing.ResponseCostHeader: {"0.0421"}}}
	p.OnResponse(context.Background(), pctx)

	// The listener's promotion step, verbatim from forwardproxy/server.go.
	store.Append("sess-e2e", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Outbound,
		Phase:     pipeline.SessionResponse,
		RequestID: "req-e2e",
		Host:      "litellm.corp",
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5", TotalTokens: 1000},
		Plugins:   pipeline.SnapshotPlugins(pctx.Extensions.Custom),
	})

	snap := agg.Snapshot(usage.BucketWidth, usage.BucketWidth, "", usage.GroupNone)
	if !snap.Priced {
		t.Error("Priced = false: the cost never reached the aggregator")
	}
	if snap.Totals.CostMicros != 42_100 {
		t.Errorf("CostMicros = %d, want 42100", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
}

// TestEndToEnd_UnpricedResponseReachesAggregatorUnpriced is the negative control
// for the test above. Same composed path, same assertions, only the plugin does
// not price the response (no cost header, no configured rates). Without it, a
// wiring bug that made everything look priced — or an assertion that could not
// fail — would pass unnoticed.
func TestEndToEnd_UnpricedResponseReachesAggregatorUnpriced(t *testing.T) {
	p := configure(t, 10)
	agg := usage.New()
	store := session.New(time.Hour, 100, 10)
	store.AddRecorder(agg)

	pctx := &pipeline.Context{ResponseHeaders: http.Header{}} // nothing to price
	p.OnResponse(context.Background(), pctx)

	store.Append("sess-e2e-unpriced", pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Outbound,
		Phase:     pipeline.SessionResponse,
		RequestID: "req-e2e-unpriced",
		Host:      "litellm.corp",
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5", TotalTokens: 1000},
		Plugins:   pipeline.SnapshotPlugins(pctx.Extensions.Custom),
	})

	snap := agg.Snapshot(usage.BucketWidth, usage.BucketWidth, "", usage.GroupNone)
	if snap.Priced {
		t.Error("Priced = true for a response the plugin never priced")
	}
	if snap.Totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", snap.Totals.CostMicros)
	}
	if snap.Totals.PricedRequests != 0 {
		t.Errorf("PricedRequests = %d, want 0", snap.Totals.PricedRequests)
	}
	// The request still counted as traffic — unpriced is not invisible.
	if snap.Totals.Requests != 1 {
		t.Errorf("Requests = %d, want 1", snap.Totals.Requests)
	}
	if snap.Totals.Tokens != 1000 {
		t.Errorf("Tokens = %d, want 1000", snap.Totals.Tokens)
	}
}
