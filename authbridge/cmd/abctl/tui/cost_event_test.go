package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// costWire is the exact JSON the proxy publishes as a cost record.
//
// It carries what this UI now READS rather than computes: the exchange total, the
// prompt-only figure a request row shows, and the saving attributed to tool-prune. The
// figures are the ones abctl used to derive itself — prompt 1,300 x 3.8e-6 + 680,000 x
// 3.8e-7 = 0.26334, saving 9,899 tokens at the cache-read rate = 0.0038 — so the rendered
// cells below are unchanged by the move. That is the point: same output, one owner.
const costWire = `{"cost_usd":0.2824,"source":"gateway-header",
  "daily_total_usd":1.4207,"daily_max_usd":5,"prompt_usd":0.26334,
  "avoided":[{"component":"tool-prune","tokensAvoided":9899,"usd":0.0038,
  "tier":"cache_read","estimated":true}]}`

// bigPromptWire is a tool-prune event sized for the cache-heavy agent turn this
// split exists for: a ~2.4MB body (≈3.5 bytes/token against a 681k-token prompt)
// with a 34KB tool block removed. The package-level `wire` fixture describes a
// 90KB body, whose byte ratio against a 681k prompt would imply a 31% saving —
// arithmetically valid but not a shape that occurs.
const bigPromptWire = `{"bytesRemoved":34645,"bodyBytesAfter":2384550,
  "model":"claude-opus-5","rateInput":3.8e-06,"rateCacheWrite":4.75e-06,
  "rateCacheRead":3.8e-07,"rateSource":"default"}`

// agentTurn is the usage a long-running agent reports: almost entirely cache
// reads, a small uncached delta, and a modest completion. Sums to TotalTokens so
// the split invariant is checkable.
func agentTurn() *pipeline.InferenceExtension {
	return &pipeline.InferenceExtension{
		InputTokens: 1_300, CacheReadTokens: 680_000,
		OutputTokens: 1_850, TotalTokens: 683_150,
	}
}

func respEvent(raw string, inf *pipeline.InferenceExtension) *pipeline.SessionEvent {
	e := &pipeline.SessionEvent{Phase: pipeline.SessionResponse, Inference: inf}
	if raw != "" {
		e.Plugins = map[string]json.RawMessage{"litellm-budget-track": json.RawMessage(raw)}
	}
	return e
}

// TestDecodeCostEvent guards the tags against drift with the plugin's struct. A
// silent decode failure would leave the COST column blank, which is
// indistinguishable from an unpriced call — so the tags are the only thing
// standing between "free" and "not reported".
func TestDecodeCostEvent(t *testing.T) {
	ce, ok := decodeCostEvent(respEvent(costWire, nil))
	if !ok {
		t.Fatal("failed to decode the published event")
	}
	if ce.CostUSD != 0.2824 {
		t.Errorf("CostUSD = %v, want 0.2824", ce.CostUSD)
	}
	if ce.Source != "gateway-header" {
		t.Errorf("Source = %q, want gateway-header", ce.Source)
	}
	if ce.DailyTotalUSD != 1.4207 || ce.DailyMaxUSD != 5 {
		t.Errorf("daily fields did not decode: %+v", ce)
	}
	// Absent, malformed, and unpriced all decline rather than render $0.0000,
	// which would read as a call that cost nothing.
	for _, bad := range []*pipeline.SessionEvent{
		nil,
		{Phase: pipeline.SessionResponse},
		respEvent(`{"cost_usd":0,"source":"gateway-header"}`, nil),
		respEvent(`not json`, nil),
	} {
		if _, ok := decodeCostEvent(bad); ok {
			t.Errorf("should not decode: %+v", bad)
		}
	}
}

// TestExchangeCellsSumToTotal is the invariant behind splitting the column: the
// request row's prompt count plus the response row's generated count equal the
// billed total. Before the split the response row showed TotalTokens, so the
// prompt was counted once on each row and the generated tokens were invisible.
func TestExchangeCellsSumToTotal(t *testing.T) {
	inf := agentTurn()
	req := reqEvent(t, bigPromptWire)
	req.RequestID = "aaa"
	resp := respEvent(costWire, inf)
	resp.RequestID = "aaa"
	rows := []eventRow{{event: req}, {event: resp}}
	partner := map[int]int{0: 1, 1: 0}
	m := &model{}

	// 1,300 + 680,000 = 681,300 prompt tokens, with the saving in parens:
	// 34,645 bytes × 681,300 tokens / 2,384,550 bytes ≈ 9.9k tokens.
	gotReq := m.tokensCell(rows, partner, 0, req)
	if gotReq != "681,300(−9.9k)" {
		t.Errorf("request TOKENS = %q, want %q", gotReq, "681,300(−9.9k)")
	}
	gotResp := m.tokensCell(rows, partner, 1, resp)
	if gotResp != "1,850" {
		t.Errorf("response TOKENS = %q, want %q", gotResp, "1,850")
	}
	if want := promptTokens(inf) + inf.OutputTokens; want != inf.TotalTokens {
		t.Errorf("the two rows sum to %d but the provider billed %d", want, inf.TotalTokens)
	}
}

// TestExchangeCellsSurviveAReportedTotalThatDisagrees: TokenUsage.Fill prefers the
// provider's own total_tokens over the sum of the parts, so the two are not
// obliged to agree. Each row must keep reporting its own measured half rather than
// back-deriving anything from the total — otherwise a gateway that rounds, or
// counts a tier this display does not, would silently corrupt both cells.
func TestExchangeCellsSurviveAReportedTotalThatDisagrees(t *testing.T) {
	inf := agentTurn()
	inf.TotalTokens = 999_999 // a reported total that is not prompt+output
	req := reqEvent(t, bigPromptWire)
	req.RequestID = "aaa"
	resp := respEvent(costWire, inf)
	resp.RequestID = "aaa"
	rows := []eventRow{{event: req}, {event: resp}}
	partner := map[int]int{0: 1, 1: 0}
	m := &model{}

	if got := m.tokensCell(rows, partner, 0, req); got != "681,300(−9.9k)" {
		t.Errorf("request TOKENS = %q, want the measured prompt regardless of the reported total", got)
	}
	if got := m.tokensCell(rows, partner, 1, resp); got != "1,850" {
		t.Errorf("response TOKENS = %q, want the measured output regardless of the reported total", got)
	}
	if strings.Contains(m.tokensCell(rows, partner, 1, resp), "999") {
		t.Error("the reported total leaked into a cell")
	}
}

// TestCostCellPhases: the request row is a modelled prompt figure, the response
// row the exchange total as budget-track reported it. The response figure
// INCLUDES the request figure — it is not a generated-token cost, because no
// plugin publishes an output rate.
func TestCostCellPhases(t *testing.T) {
	inf := agentTurn()
	req := reqEvent(t, bigPromptWire)
	req.RequestID = "aaa"
	resp := respEvent(costWire, inf)
	resp.RequestID = "aaa"
	rows := []eventRow{{event: req}, {event: resp}}
	partner := map[int]int{0: 1, 1: 0}
	m := &model{}

	// Prompt: 1,300 × 3.8e-6 + 680,000 × 3.8e-7 = 0.00494 + 0.2584 = 0.26334.
	// Saving: 9.9k tokens at the cache-read rate = 0.0038.
	if got := m.costCell(rows, partner, 0, req); got != "$0.2633(−$0.0038)" {
		t.Errorf("request COST = %q, want %q", got, "$0.2633(−$0.0038)")
	}
	if got := m.costCell(rows, partner, 1, resp); got != "$0.2824" {
		t.Errorf("response COST = %q, want %q", got, "$0.2824")
	}
	// Without the budget-track event nobody reported a cost, so the response cell
	// is blank rather than modelled from the request's rates.
	if got := m.costCell(rows, partner, 1, respEvent("", inf)); got != "" {
		t.Errorf("unpriced response COST = %q, want empty", got)
	}
}

// TestPromptCostIsTierWeighted: pricing the whole prompt at the uncached input
// rate overstates a cache-heavy turn by close to an order of magnitude, which is
// the common shape for a long-running agent. The weighted figure must be well
// below the flat one.
// The tier weighting now happens in the proxy (authlib/costing computes the prompt-only
// figure; authlib/pricing weights the tiers), and is tested there against the real rate
// table. What abctl must still get right is reading the published figure and declining to
// invent one — a $0.00 in this column reads as a free prompt.
func TestPromptCost_ReadsThePublishedFigure(t *testing.T) {
	inf := &pipeline.InferenceExtension{InputTokens: 1_000, CacheReadTokens: 99_000}
	// A tier-weighted figure, well below the 0.38 a flat input rate would produce for
	// the same 100k prompt: that gap is why the proxy weights it rather than the UI.
	e := recordEvent(t, costevent.Event{CostUSD: 0.05, Settled: true, PromptUSD: 0.0414})
	e.Inference = inf
	got, ok := promptCost(e)
	if !ok {
		t.Fatal("no cost figure")
	}
	if got != 0.0414 {
		t.Errorf("promptCost = %v, want the published 0.0414", got)
	}
	if flat := float64(promptTokens(inf)) * 3.8e-6; got >= flat {
		t.Errorf("published figure %v is not below the flat %v; the weighting was lost", got, flat)
	}
	// No record, and a record with no prompt figure, both decline rather than showing 0.
	if _, ok := promptCost(respEvent("", inf)); ok {
		t.Error("a response with no record produced a cost figure")
	}
	if _, ok := promptCost(recordEvent(t, costevent.Event{CostUSD: 0.05, Settled: true})); ok {
		t.Error("a record with no prompt figure produced one")
	}
}

// TestGeneratedTokensCellReadsOutputTokens: the response row shows what was
// generated, and shows nothing rather than "0" when no usage was reported — a
// blank cell reads as "not measured", a 0 as "generated nothing".
//
// Asserts OutputTokens only. The CompletionTokens fallback in the cell is not
// exercised because it cannot fire: TokenUsage.Fill sets CompletionTokens from
// the same Output value, so a test feeding one without the other would pin a
// shape no parser produces.
func TestGeneratedTokensCellReadsOutputTokens(t *testing.T) {
	if got := generatedTokensCell(respEvent("", &pipeline.InferenceExtension{OutputTokens: 1_850})); got != "1,850" {
		t.Errorf("cell = %q, want 1,850", got)
	}
	for _, inf := range []*pipeline.InferenceExtension{nil, {}} {
		if got := generatedTokensCell(respEvent("", inf)); got != "" {
			t.Errorf("cell = %q, want empty", got)
		}
	}
}

// TestPromptTokensSumsTheSplitTiers: the prompt total is the sum of the three
// prompt-side tiers, which is the only shape parsercommon.TokenUsage.Fill
// produces. Summing is what lets a request row show a total at all.
//
// The PromptTokens fallback is not asserted for a value of its own: Fill sets it
// to the identical sum, so there is no input where the two disagree.
func TestPromptTokensSumsTheSplitTiers(t *testing.T) {
	split := &pipeline.InferenceExtension{
		InputTokens: 100, CacheReadTokens: 400, CacheWriteTokens: 25, PromptTokens: 525,
	}
	if got := promptTokens(split); got != 525 {
		t.Errorf("promptTokens = %d, want 525", got)
	}
	// Cache-write tokens are prompt-side and must not be dropped: on a cold cache
	// they are the bulk of the prompt, and they bill at ~1.25x the input rate.
	noWrite := &pipeline.InferenceExtension{InputTokens: 100, CacheReadTokens: 400, PromptTokens: 500}
	if got := promptTokens(noWrite); got != 500 {
		t.Errorf("promptTokens = %d, want 500", got)
	}
	if got := promptTokens(nil); got != 0 {
		t.Errorf("nil promptTokens = %d, want 0", got)
	}
	if got := promptTokens(&pipeline.InferenceExtension{}); got != 0 {
		t.Errorf("empty promptTokens = %d, want 0", got)
	}
}

// TestTokensCellFitsASevenDigitPrompt pins the TOKENS column width against the
// worst realistic case. Million-token contexts are in service, and bubbles
// truncates each cell at the column width with an ellipsis — so at 15 this
// rendered "1,048,576(−1…", silently dropping the saving, which is the half of
// the cell that appears nowhere else in the UI.
//
// Asserts against the column definition rather than a transcribed number, so
// narrowing the column fails here instead of quietly clipping in production.
func TestTokensCellFitsASevenDigitPrompt(t *testing.T) {
	inf := &pipeline.InferenceExtension{
		InputTokens: 48_576, CacheReadTokens: 1_000_000,
		OutputTokens: 3_650, TotalTokens: 1_052_226,
	}
	req := reqEvent(t, bigPromptWire)
	req.RequestID = "big"
	resp := respEvent(costWire, inf)
	resp.RequestID = "big"
	rows := []eventRow{{event: req}, {event: resp}}
	m := &model{}

	cell := m.tokensCell(rows, map[int]int{0: 1, 1: 0}, 0, req)
	if !strings.HasPrefix(cell, "1,048,576(−") {
		t.Fatalf("TOKENS cell = %q, want a 7-digit prompt with its saving", cell)
	}
	width := columnWidth(t, "TOKENS")
	if n := len([]rune(cell)); n > width {
		t.Errorf("TOKENS cell %q is %d cols but the column is %d — bubbles will clip the saving",
			cell, n, width)
	}
	// The same guard for COST, whose widest realistic value is a sub-cent saving
	// against a dollar total.
	costStr := m.costCell(rows, map[int]int{0: 1, 1: 0}, 0, req)
	if n := len([]rune(costStr)); n > columnWidth(t, "COST") {
		t.Errorf("COST cell %q is %d cols but the column is %d", costStr, n, columnWidth(t, "COST"))
	}
}

// columnWidth reads a width off the real events-table definition, so a test
// cannot drift from the column it is meant to be guarding.
func columnWidth(t *testing.T, title string) int {
	t.Helper()
	for _, c := range newEventsTable().Columns() {
		if c.Title == title {
			return c.Width
		}
	}
	t.Fatalf("no %q column", title)
	return 0
}

// TestEventMethodSharesTheColumnWidth: eventMethod truncated to a hardcoded 22
// after the column shrank to 14. bubbles re-truncates and hid it, but that is the
// drift actionColWidth exists to prevent, so lock the two together.
func TestEventMethodSharesTheColumnWidth(t *testing.T) {
	if methodColWidth != columnWidth(t, "METHOD") {
		t.Errorf("methodColWidth = %d but the METHOD column is %d",
			methodColWidth, columnWidth(t, "METHOD"))
	}
	long := &pipeline.SessionEvent{
		Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-4-5-20250929"},
	}
	if n := len([]rune(eventMethod(*long))); n > methodColWidth {
		t.Errorf("eventMethod returned %d cols, want <= %d", n, methodColWidth)
	}
	// A name that fits must not be truncated.
	fits := &pipeline.SessionEvent{
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5"},
	}
	if got := eventMethod(*fits); got != "claude-opus-5" {
		t.Errorf("eventMethod = %q, want it untouched", got)
	}
}

// TestFormatUSDCellNeverRendersAFreeCall: %.4f turns anything under $0.00005 into
// "$0.0000", which reads as a call that cost nothing. decodeCostEvent declines a
// zero cost and promptCost declines an unpriced model precisely so that reading
// never appears, and rounding at the formatting layer would undo both.
//
// Reachable: 100 cache-read tokens at a typical rate is $0.000038.
func TestFormatUSDCellNeverRendersAFreeCall(t *testing.T) {
	tiny := 100 * 3.8e-7 // $0.000038
	if got := formatUSDCell(tiny); got != "<$0.0001" {
		t.Errorf("formatUSDCell(%v) = %q, want %q", tiny, got, "<$0.0001")
	}
	// Zero is genuinely zero and keeps its plain rendering: callers already
	// decline to show a cell at all in that case.
	if got := formatUSDCell(0); got != "$0.0000" {
		t.Errorf("formatUSDCell(0) = %q, want %q", got, "$0.0000")
	}
	// At and above the floor, exact figures are unchanged.
	for v, want := range map[float64]string{0.0001: "$0.0001", 0.2633: "$0.2633", 12.5: "$12.5000"} {
		if got := formatUSDCell(v); got != want {
			t.Errorf("formatUSDCell(%v) = %q, want %q", v, got, want)
		}
	}
	// A sub-floor saving beside a normal total must not read as "saved nothing".
	cell := formatUSDWithSaving(0.2633, tiny, false)
	if !strings.Contains(cell, "(−<$0.0001)") {
		t.Errorf("cell = %q, want the saving marked as below the floor", cell)
	}
	if strings.Contains(cell, "$0.0000") {
		t.Errorf("cell = %q still rounds a real amount to zero", cell)
	}
	// The widest cell the formatter can produce must fit the column.
	widest := formatUSDWithSaving(tiny, tiny/2, false)
	if n := len([]rune(widest)); n > columnWidth(t, "COST") {
		t.Errorf("widest COST cell %q is %d cols but the column is %d",
			widest, n, columnWidth(t, "COST"))
	}
}

// TestMethodColumnDistinguishesDatedModelIDs: the COST column reports a per-row
// cost, so METHOD has to be able to say which model it belongs to. At 14 the two
// dated Sonnet IDs both rendered "claude-sonnet…", making the pair unreadable.
func TestMethodColumnDistinguishesDatedModelIDs(t *testing.T) {
	a := &pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-4-5-20250929"}}
	b := &pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-4-20250514"}}
	if eventMethod(*a) == eventMethod(*b) {
		t.Errorf("both models render as %q; METHOD cannot say which the COST cell is for", eventMethod(*a))
	}
	// A long MCP method name still truncates rather than overflowing.
	mcp := &pipeline.SessionEvent{MCP: &pipeline.MCPExtension{Method: "notifications/initialized"}}
	if n := len([]rune(eventMethod(*mcp))); n > methodColWidth {
		t.Errorf("eventMethod = %d cols, want <= %d", n, methodColWidth)
	}
}

// Precision IS the estimate marker: an estimated saving renders compact because trailing
// digits would be false precision, and a counted one renders exact. No extra glyph — "~"
// already means projected, and every saving published today is estimated, so a marker on
// every row would distinguish nothing.
func TestFormatTokensWithSaving_PrecisionMarksTheEstimate(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		projected, estimated bool
		want                 string
	}{
		{"estimated and applied", false, true, "681,300(−9.9k)"},
		{"estimated and projected", true, true, "681,300(~9.9k)"},
		// A saving counted by a tokenizer, which nothing publishes yet: exact digits,
		// because they would be real.
		{"counted and applied", false, false, "681,300(−9,899)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTokensWithSaving(681_300, 9_899, tc.projected, tc.estimated)
			if got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}
