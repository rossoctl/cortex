package event

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// eventWith builds a SessionEvent carrying raw JSON under the cost-event key.
func eventWith(t *testing.T, key, raw string) *pipeline.SessionEvent {
	t.Helper()
	return &pipeline.SessionEvent{
		Plugins: map[string]json.RawMessage{key: json.RawMessage(raw)},
	}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name     string
		event    *pipeline.SessionEvent
		wantOK   bool
		wantCost float64
		wantSrc  string
	}{
		{
			name:     "gateway header cost",
			event:    eventWith(t, PluginName, `{"cost_usd":0.0421,"source":"gateway-header","daily_total_usd":1.5,"daily_max_usd":10}`),
			wantOK:   true,
			wantCost: 0.0421,
			wantSrc:  SourceGatewayHeader,
		},
		{
			name:     "usage fallback cost",
			event:    eventWith(t, PluginName, `{"cost_usd":0.01,"source":"usage-fallback"}`),
			wantOK:   true,
			wantCost: 0.01,
			wantSrc:  SourceUsageFallback,
		},
		{
			// A cache hit or error charges nothing and the plugin emits nothing;
			// a zero that does arrive must not read as a priced free request.
			name:   "zero cost is not priced",
			event:  eventWith(t, PluginName, `{"cost_usd":0,"source":"gateway-header"}`),
			wantOK: false,
		},
		{
			name:   "negative cost rejected",
			event:  eventWith(t, PluginName, `{"cost_usd":-1,"source":"gateway-header"}`),
			wantOK: false,
		},
		{
			name:   "malformed JSON rejected",
			event:  eventWith(t, PluginName, `{"cost_usd":`),
			wantOK: false,
		},
		{
			name:   "different plugin key ignored",
			event:  eventWith(t, "tool-prune", `{"cost_usd":5}`),
			wantOK: false,
		},
		{
			name:   "no plugins map",
			event:  &pipeline.SessionEvent{},
			wantOK: false,
		},
		{
			name:   "nil event",
			event:  nil,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Decode(tc.event)
			if ok != tc.wantOK {
				t.Fatalf("Decode ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.CostUSD != tc.wantCost {
				t.Errorf("CostUSD = %v, want %v", got.CostUSD, tc.wantCost)
			}
			if got.Source != tc.wantSrc {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSrc)
			}
		})
	}
}

// TestEventJSONTagsArePinned guards the wire format. agentop decodes these exact
// tags from a separate module that this phase does not rebuild, so a rename here
// would silently blank its COST column.
func TestEventJSONTagsArePinned(t *testing.T) {
	b, err := json.Marshal(Event{
		CostUSD:       0.25,
		Source:        SourceGatewayHeader,
		DailyTotalUSD: 3.5,
		DailyMaxUSD:   10,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"cost_usd":0.25,"source":"gateway-header","daily_total_usd":3.5,"daily_max_usd":10}`
	if string(b) != want {
		t.Errorf("wire format changed:\n got %s\nwant %s", b, want)
	}
}

// EVERY FIELD, INCLUDING THE omitempty ONES, because the marshal above cannot see them.
//
// agentop decodes this struct out of process, so a tag that changes — or a new field whose tag
// nobody pinned — is a field that silently stops arriving. The four fields set above are the
// ones with no omitempty; every other tag is absent from that expectation precisely because
// its field was zero, which is how the three this change adds — and three that predate it —
// went unpinned.
func TestEventJSONTagsArePinned_EveryField(t *testing.T) {
	b, err := json.Marshal(Event{
		CostUSD:          0.25,
		Source:           SourceGatewayHeader,
		DailyTotalUSD:    3.5,
		DailyMaxUSD:      10,
		Provenance:       "authoritative",
		Currency:         "credits",
		Settled:          true,
		Incomplete:       true,
		IncompleteReason: "output-uncounted",
		// A cache-blind header that lost to the table, and the figure it had reported. These
		// two ride together by construction — see settle.gatewayFigureOf.
		HeaderOmittedCache: true,
		GatewayUSD:         0.000205,
		PromptUSD:          0.2,
		OutputUSD:          0.05,
		Tiers: &TierCost{
			Input: 0.007, CacheWrite: 0.022, CacheRead: 0.3, Output: 0.46,
		},
		RejectedReason: "implausible",
		UsageRefused:   true,
		Avoided: []Saving{{
			Component:     "tool-prune",
			TokensAvoided: 1200,
			USD:           0.01,
			Provenance:    "configured",
			PenaltyUSD:    0.002,
			Operator:      "tool-prune",
			OpsVersion:    1,
			Basis:         "subscriptionEquivalent",
			Units:         120,
			PenaltyUnits:  4.5,
		}},
		Purpose:              PurposeKeepAlive,
		Basis:                BasisSubscriptionEquivalent,
		Units:                1.5,
		Estimated:            true,
		UnattributedBreakUSD: 0.003,
		InteractionUSD:       0.0041,
		Thread:               "main",
		SavingsWithheld:      "check_sum",
		Spends: []SpendRecord{{
			ChangeID: "s:1", Operator: "context-guru.extract_llm", Thread: "main", Model: "m", Host: "h",
			Input: 8000, CacheWrite5m: 1, CacheWrite1h: 2, CacheRead: 3, Output: 1000,
			USD: 0.04, Basis: "usd", Units: 7, Estimated: true, Projected: true, Refused: true,
		}},
		Compaction: &CompactionRecord{Trigger: "unknown", Ended: []string{"tool-prune:Big"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"cost_usd":0.25,"source":"gateway-header","daily_total_usd":3.5,` +
		`"daily_max_usd":10,"provenance":"authoritative","currency":"credits","header_omitted_cache":true,` +
		`"gateway_usd":0.000205,"settled":true,"incomplete":true,` +
		`"incomplete_reason":"output-uncounted","prompt_usd":0.2,"output_usd":0.05,` +
		`"tiers":{"input":0.007,"cache_write":0.022,"cache_read":0.3,"output":0.46},` +
		`"usage_refused":true,"rejected_reason":"implausible",` +
		`"avoided":[{"component":"tool-prune",` +
		`"tokensAvoided":1200,"usd":0.01,"provenance":"configured","penaltyUsd":0.002,` +
		`"operator":"tool-prune","opsVersion":1,"basis":"subscriptionEquivalent","units":120,` +
		`"penaltyUnits":4.5}],"purpose":"keep-alive","basis":"subscriptionEquivalent","units":1.5,"estimated":true,"unattributed_break_usd":0.003,"interaction_usd":0.0041,"thread":"main",` +
		`"savings_withheld":"check_sum",` +
		`"spends":[{"changeId":"s:1","operator":"context-guru.extract_llm","thread":"main","model":"m",` +
		`"host":"h","input":8000,"cacheWrite5m":1,"cacheWrite1h":2,"cacheRead":3,"output":1000,` +
		`"usd":0.04,"basis":"usd","units":7,"estimated":true,"projected":true,"refused":true}],` +
		`"compaction":{"trigger":"unknown","ended":["tool-prune:Big"]}}`
	if string(b) != want {
		t.Errorf("wire format changed:\n got %s\nwant %s", b, want)
	}
}

// TestTotalAvoidedMicros_ReadsWhatTheProducerActuallyWrites decodes bytes taken verbatim off
// a running proxy — tool-prune in enforce mode behind Claude Code — and asserts the aggregate
// reads them.
//
// The test above pins the ENCODE direction from a struct literal, which cannot fail if a tag
// and the literal are wrong in the same way. This one starts from production bytes instead, so
// it is the only assertion here that would catch the aggregate summing a field the producer
// spells differently. Note `usd` and `tokensAvoided` in one record: the mixed casing is real,
// and a "tidy-up" that regularised either would silently zero every saving.
//
// cache_read is not incidental. The tool manifest sits inside the cached prefix, so the
// recurring saving comes out of the ~0.1x tier — the tier that makes the figure small and
// worth reporting anyway, since it recurs on every turn.
func TestTotalAvoidedMicros_ReadsWhatTheProducerActuallyWrites(t *testing.T) {
	const wire = `{"cost_usd":0.0526,"source":"usage-fallback","daily_total_usd":0,` +
		`"daily_max_usd":0,"provenance":"bundled","settled":true,` +
		`"avoided":[{"component":"tool-prune","tokensAvoided":10210,"usd":0.00388,` +
		`"provenance":"bundled","tier":"cache_read","estimated":true}]}`

	var ev Event
	if err := json.Unmarshal([]byte(wire), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// THAT RECORD PREDATES THE ACCOUNTANT and names no basis. settle.Avoided wrote it, and it
	// priced only dollars, so it counts as "usd" (Saving.EffectiveBasis) — as the ledger rows
	// written from it already do. The accountant's own bytes, with their basis, count the same.
	if got, want := ev.TotalAvoidedMicros(), int64(3_880); got != want {
		t.Errorf("TotalAvoidedMicros = %d for a legacy saving with no basis, want %d", got, want)
	}
	const withBasis = `{"cost_usd":0.0526,"source":"usage-fallback","daily_total_usd":0,` +
		`"daily_max_usd":0,"provenance":"bundled","settled":true,` +
		`"avoided":[{"component":"tool-prune","tokensAvoided":10210,"usd":0.00388,` +
		`"provenance":"bundled","tier":"cache_read","penaltyUsd":0.0005,"operator":"tool-prune",` +
		`"opsVersion":1,"basis":"usd","estimated":true}]}`
	if err := json.Unmarshal([]byte(withBasis), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := ev.TotalAvoidedMicros(), int64(3_880); got != want {
		t.Errorf("TotalAvoidedMicros = %d, want %d", got, want)
	}
	if got, want := ev.TotalPenaltyMicros(), int64(500); got != want {
		t.Errorf("TotalPenaltyMicros = %d, want %d", got, want)
	}
	// The record's own cost is untouched by the saving beside it, which is the invariant the
	// aggregate rests on, asserted here at the record level where it starts.
	if got, want := ev.Micros(), int64(52_600); got != want {
		t.Errorf("Micros = %d, want %d — a saving must not move the incurred figure", got, want)
	}
	if !ev.Priced() {
		t.Error("Priced = false: this is a settled figure and both totals depend on it being one")
	}
}

// A FIELD ADDED WITHOUT A PINNED TAG FAILS HERE.
//
// The two tests above assert the tags of the fields they happen to set; this one asserts that
// the set is complete, so adding a field to Event without extending the expectation above is
// a failure rather than a silent gap. Same instrument as
// pipeline.TestSessionEventWireCoversEveryField, for the same reason: the consumer is another
// process.
func TestEventWireCoversEveryField(t *testing.T) {
	// Keep in step with the marshal in TestEventJSONTagsArePinned_EveryField.
	const pinned = 27
	if got := reflect.TypeOf(Event{}).NumField(); got != pinned {
		t.Fatalf("Event has %d fields, %d are pinned on the wire.\n"+
			"Add the new field to TestEventJSONTagsArePinned_EveryField's marshal AND its "+
			"expected string, or agentop will never see it.", got, pinned)
	}
}

func TestMicros(t *testing.T) {
	tests := []struct {
		name string
		usd  float64
		want int64
	}{
		{"whole dollar", 1, 1_000_000},
		{"typical request", 0.0421, 42_100},
		{"rounds to nearest", 0.0000005, 1},
		{"sub-micro rounds to zero", 0.0000001, 0},
		{"large", 1234.5678, 1_234_567_800},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Event{CostUSD: tc.usd}).Micros(); got != tc.want {
				t.Errorf("Micros() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A figure too large for the micros unit is UNPRICED, not saturated.
//
// Micros() was `int64(math.Round(CostUSD * 1e6))` with no bound at all, while
// pricing.Cost guarded the identical conversion and called the unguarded form "a
// garbage ledger figure". The producer's header path accepts any finite non-negative
// float, so a gateway reporting 1e13 saturated to MaxInt64 and TWO such requests
// wrapped usage.Counts.Add to −2 micros. The ring forgets that in six hours; the
// durable ledger keeps it thirty days with no repair path.
//
// The bound stopped the SATURATION and did not stop the wrap: 1024 figures at the bound
// still wrap that sum. See pricing.TestNoPerRequestBoundClosesTheAccumulationWrap.
//
// Not clamped to the bound either. A clamped figure is a wrong number wearing a right
// label, and Priced() has to agree with Micros() or a consumer adds a zero to its total
// while counting the request as covered.
func TestMicros_OutOfRangeIsUnpricedNotSaturated(t *testing.T) {
	// THE BOUND IS EXCLUSIVE, walked from both sides — one micro under is a figure and
	// the edge itself is not.
	//
	// This assertion is the reverse of what it said before, and the reversal is the fix:
	// the check was `> MaxCostMicros`, so exactly 2^53 micros was accepted while the doc
	// called anything above the bound out of range. 2^53 is the first integer float64
	// cannot follow — 2^53+1 rounds back onto it — so the edge is the one value in the
	// range that cannot be told apart from a figure past it. Admitting it admitted that
	// ambiguity; `>=` makes every accepted figure exactly representable and distinct from
	// its neighbours.
	// Two micros under, not one: at 2^53 micros the float64 spacing of the USD figure is
	// wider than a micro, so 2^53-1 rounds back onto the edge and goes out of range with
	// it. pricing.TestMicrosFromUSD_BoundIsExclusive pins that arithmetic.
	underBound := Event{CostUSD: float64(pricing.MaxCostMicros-2) / 1e6, Settled: true}
	if !underBound.Priced() {
		t.Error("Priced() = false under MaxCostMicros; the bound excludes only its own edge, and this figure is exactly representable")
	}
	if got := underBound.Micros(); got != pricing.MaxCostMicros-2 {
		t.Errorf("Micros() = %d two micros under the bound, want %d", got, pricing.MaxCostMicros-2)
	}

	// The edge, and one dollar past it. (One MICRO past is unrepresentable: 2^53+1 rounds
	// back to 2^53 in a float64 — which is exactly why the edge is excluded.)
	for _, tc := range []struct {
		name string
		usd  float64
	}{
		{"exactly at the bound", float64(pricing.MaxCostMicros) / 1e6},
		{"a dollar past the bound", float64(pricing.MaxCostMicros)/1e6 + 1},
		{"the header figure that saturated MaxInt64", 1e13},
		{"an infinity", math.Inf(1)},
		{"not a number", math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Event{CostUSD: tc.usd, Settled: true}
			if e.Priced() {
				t.Error("Priced() = true for a figure the micros unit cannot hold; it must read as unpriced, not as money")
			}
			if got := e.Micros(); got != 0 {
				t.Errorf("Micros() = %d, want 0 — neither saturated nor clamped to the bound", got)
			}
		})
	}
}

// The key names the CONCERN, not the producer.
//
// It used to be the producing plugin's name, pinned by a test to
// litellm-budget-track.Name(). That made moving the producer — which cortex #972 does,
// to inference-parser — a breaking wire change for every live consumer AND for every
// event already sitting in a session store. The framework's own body-mutation event
// established the alternative and said why: "a switch of plugin names in a future
// refactor shouldn't break operators' dashboards" (pipeline/context.go).
func TestKey_NamesTheConcernNotTheProducer(t *testing.T) {
	if Key != "cost" {
		t.Errorf("Key = %q, want %q — a producer name here couples every consumer to one plugin", Key, "cost")
	}
	if Key == PluginName {
		t.Error("Key equals the legacy producer name; the point is that they differ")
	}
}

// A record written under the new key decodes. A record written under the legacy key still
// decodes, because session stores are full of them and a consumer reading history must not
// see a gap where the rename happened.
func TestDecode_ReadsBothKeys(t *testing.T) {
	raw, err := json.Marshal(Event{CostUSD: 0.25, Source: SourceGatewayHeader})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{Key, PluginName} {
		t.Run(key, func(t *testing.T) {
			e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{key: raw}}
			got, ok := Decode(e)
			if !ok {
				t.Fatalf("no cost decoded under %q", key)
			}
			if got.CostUSD != 0.25 {
				t.Errorf("CostUSD = %v, want 0.25", got.CostUSD)
			}
		})
	}
}

// The new key wins when both are present. A transitional deployment can have an old
// producer and a new one in the same pipeline; taking the legacy value there would report
// the figure from the component being retired.
func TestDecode_NewKeyWinsOverLegacy(t *testing.T) {
	newRaw, err := json.Marshal(Event{CostUSD: 0.25, Source: SourceGatewayHeader})
	if err != nil {
		t.Fatal(err)
	}
	oldRaw, err := json.Marshal(Event{CostUSD: 9.99, Source: SourceUsageFallback})
	if err != nil {
		t.Fatal(err)
	}
	e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{
		Key:        newRaw,
		PluginName: oldRaw,
	}}
	got, ok := Decode(e)
	if !ok {
		t.Fatal("nothing decoded")
	}
	if got.CostUSD != 0.25 {
		t.Errorf("CostUSD = %v, want 0.25 from the new key", got.CostUSD)
	}
}

// Presence and PRICEDNESS are different questions, and Decode only ever answered the
// second: it returns false for a record whose cost is an unsettled zero, meaning a caller
// cannot tell "nobody priced this" from "no record at all".
//
// That distinction is load-bearing for #972: the same record is about to carry avoided
// cost, and a request that could not be priced is exactly the one whose savings figure is
// most interesting. Under Decode's rule that record would vanish entirely.
func TestRecord_PresenceIsNotPricedness(t *testing.T) {
	unpriced, err := json.Marshal(Event{CostUSD: 0, Settled: false, Source: SourceUsageFallback})
	if err != nil {
		t.Fatal(err)
	}
	e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{Key: unpriced}}

	if _, ok := Decode(e); ok {
		t.Error("Decode reported a priced figure for an unsettled zero")
	}
	got, ok := Record(e)
	if !ok {
		t.Fatal("Record did not report the record as PRESENT; a consumer cannot see its non-cost fields")
	}
	if got.Source != SourceUsageFallback {
		t.Errorf("Source = %q, want the record's own value", got.Source)
	}
	if got.Priced() {
		t.Error("Priced() true for an unsettled zero")
	}
}

// Priced() is the rule Decode applies, named and reusable, so a consumer that took the
// record via Record can ask the same question without reimplementing the predicate.
func TestPriced_MatchesDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   Event
		want bool
	}{
		{"positive", Event{CostUSD: 0.25}, true},
		{"settled zero is an answer", Event{CostUSD: 0, Settled: true}, true},
		{"unsettled zero is not", Event{CostUSD: 0}, false},
		{"negative is never", Event{CostUSD: -1, Settled: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.Priced(); got != tc.want {
				t.Errorf("Priced() = %v, want %v", got, tc.want)
			}
			raw, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatal(err)
			}
			e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{Key: raw}}
			if _, ok := Decode(e); ok != tc.want {
				t.Errorf("Decode ok = %v, but Priced() = %v; the two must agree", ok, tc.want)
			}
		})
	}
}

// No record at all stays a clean negative on both paths.
func TestRecord_AbsentIsNotAnError(t *testing.T) {
	for _, e := range []*pipeline.SessionEvent{
		nil,
		{},
		{Plugins: map[string]json.RawMessage{"tool-prune": []byte(`{"bytesRemoved":1}`)}},
	} {
		if _, ok := Record(e); ok {
			t.Errorf("Record found a record in %+v", e)
		}
		if _, ok := Decode(e); ok {
			t.Errorf("Decode found a cost in %+v", e)
		}
	}
}

// A REFUSED figure is never spend, whatever else the record says.
//
// RejectedReason is set by a producer that declined a cost header it could not corroborate
// (settle.implausibleUnparsedCost). The record is published so the coverage gap is
// nameable, which means it travels to every consumer that reads a cost — so the predicate
// they all ask has to answer no.
//
// The second row is the one worth having: a reason set ALONGSIDE a figure. No producer
// writes that today (settle.Settle leaves CostUSD zero when it rejects), and the check is
// here precisely so a future one that forgets to drop the number cannot turn a refusal into
// a charge. Fail-closed on money.
func TestPriced_RefusedFigureIsNeverSpend(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   Event
	}{
		{"the shape cost/settle publishes", Event{RejectedReason: RejectedImplausible}},
		// IN RANGE for the micros unit deliberately — $1 billion converts cleanly — so what
		// zeroes it below is the REFUSAL and not the representability bound, which is a
		// different check with its own rows above.
		{"a reason left beside a figure", Event{CostUSD: 1e9, Settled: true, Source: SourceGatewayHeader, RejectedReason: RejectedImplausible}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ev.Priced() {
				t.Error("Priced() = true for a record whose figure was REFUSED; every consumer's admission guard goes through this predicate, so a true here puts a forged figure in the ledger and the budget")
			}
			if got := tc.ev.Micros(); got != 0 {
				t.Errorf("Micros() = %d, want 0 — a refused figure is not clamped to a bound, it is not a figure", got)
			}
			e := &pipeline.SessionEvent{Plugins: map[string]json.RawMessage{
				Key: mustJSON(t, tc.ev),
			}}
			if _, ok := Decode(e); ok {
				t.Error("Decode returned a cost for a refused figure; the aggregator would add it to a dollar total")
			}
			// PRESENT, though. That is the point of publishing it: the gap has to be
			// visible, or a response that reported $9,000,000,000 is the same event as one
			// that reported nothing.
			rec, ok := Record(e)
			if !ok {
				t.Fatal("Record found nothing; a refusal that reaches no record is a coverage gap nobody can see")
			}
			if rec.RejectedReason != RejectedImplausible {
				t.Errorf("RejectedReason = %q, want %q — the reason must survive the wire or the record cannot say why it carries no figure", rec.RejectedReason, RejectedImplausible)
			}
		})
	}
}

// TestRejectedFiguresCannotWrapAnAggregate is the accumulation half, at the record level.
//
// mustJSON marshals ev for a session-event fixture.
func mustJSON(t *testing.T, ev Event) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Penalty sums beside the saving, applied usd savings only, and is never netted or floored: a
// penalty larger than the saving is two figures, not a clamped zero.
func TestTotalPenaltyMicros(t *testing.T) {
	ev := Event{Avoided: []Saving{
		{Component: "context-guru.mask", Basis: BasisUSD, USD: 0.01, PenaltyUSD: 0.03},
		{Component: "tool-prune", Basis: BasisUSD, USD: 0.002, PenaltyUSD: 0.001},
		{Component: "tool-prune", Basis: BasisUSD, USD: 5, PenaltyUSD: 7, Projected: true},
		{Component: "context-guru.mask", Basis: BasisSubscriptionEquivalent, USD: 1, PenaltyUSD: 2},
		{Component: "context-guru.mask", Basis: BasisUnknown, USD: 1, PenaltyUSD: 2},
	}}
	if got, want := ev.TotalPenaltyMicros(), int64(31_000); got != want {
		t.Errorf("TotalPenaltyMicros = %d, want %d", got, want)
	}
	if got, want := ev.TotalAvoidedMicros(), int64(12_000); got != want {
		t.Errorf("TotalAvoidedMicros = %d, want %d: gross, not net, not floored", got, want)
	}
	if (Event{}).TotalPenaltyMicros() != 0 {
		t.Error("no saving, nonzero penalty")
	}
}

// Only a "usd" saving is money (design §5.7a): a subscriptionEquivalent saving's dollars are
// what the API would have charged, an unknown one's are unknown, and a saving with no basis
// predates the accountant and is dollars.
func TestTotalAvoidedUSD_CountsOnlyUSDBasis(t *testing.T) {
	ev := Event{Avoided: []Saving{
		{Component: "tool-prune", Basis: BasisUSD, USD: 0.25},
		{Component: "tool-prune", Basis: BasisSubscriptionEquivalent, USD: 1, Units: 40},
		{Component: "tool-prune", Basis: BasisUnknown, USD: 2},
		{Component: "tool-prune", USD: 4},
	}}
	// 0.25 + the legacy no-basis 4, which is dollars (EffectiveBasis).
	if got := ev.TotalAvoidedUSD(); got != 4.25 {
		t.Errorf("TotalAvoidedUSD = %v, want 4.25 from the usd and the legacy saving", got)
	}
	if (Saving{}).EffectiveBasis() != BasisUSD || (Saving{Basis: BasisUnknown}).EffectiveBasis() != BasisUnknown {
		t.Error("EffectiveBasis does not read an empty basis as usd, or rewrites a set one")
	}
}

// Figures splits a record's savings and spends by basis, each into its own field, and only
// applied savings count.
func TestFigures_SplitsByBasis(t *testing.T) {
	ev := Event{
		Avoided: []Saving{
			{Basis: BasisUSD, USD: 0.25, PenaltyUSD: 0.05},
			{Basis: BasisSubscriptionEquivalent, USD: 1, Units: 40.5, PenaltyUnits: 2.25},
			{Basis: BasisSubscriptionEquivalent, USD: 9, Units: 900, Projected: true},
			{Basis: BasisUnknown, USD: 2, PenaltyUSD: 1},
		},
		Spends: []SpendRecord{
			{Basis: BasisUSD, USD: 0.01},
			{Basis: BasisSubscriptionEquivalent, USD: 0.5, Units: 7},
			{Basis: BasisUnknown, USD: 0.03},
			{USD: 99},
			// A dry run's call and a call the library refused to price stay visible as records,
			// and enter no total (accountant R12; Cortex design, fix round 2).
			{Basis: BasisUSD, USD: 5, Projected: true},
			{Basis: BasisSubscriptionEquivalent, USD: 5, Units: 500, Projected: true},
			{Basis: BasisUnknown, USD: 5, Projected: true},
			{Basis: BasisUSD, USD: 6, Refused: true},
			{Basis: BasisSubscriptionEquivalent, USD: 6, Units: 600, Refused: true},
			{Basis: BasisUnknown, USD: 6, Refused: true},
		},
	}
	want := Figures{
		AvoidedMicros: 250_000, PenaltyMicros: 50_000,
		AvoidedUnits: 40_500, PenaltyUnits: 2_250, AvoidedAPIEquivalentMicros: 1_000_000,
		// Net of its penalty, 2 - 1: an unknown-basis penalty went nowhere when this was gross.
		AvoidedUnknownMicros: 1_000_000,
		PluginSpendMicros:    10_000, PluginSpendUnits: 7_000, PluginSpendUnknownMicros: 30_000,
	}
	if got := ev.Figures(); got != want {
		t.Errorf("Figures = %+v\nwant %+v", got, want)
	}
	if !(Event{}).Figures().IsZero() {
		t.Error("an empty record has figures")
	}
}

// A basis-usd spend whose own Currency differs from the record's must not enter
// PluginSpendMicros: its call priced at a different rate table than the primary request's (a
// credits-billed gateway beside a USD-endpoint operator call), so adding its figure in would mix
// units into one total (review finding). A spend with no Currency (the default, same as the
// record's own empty Currency) still counts, matching every other test's fixtures above.
func TestFigures_SkipsASpendInACurrencyNotTheRecordsOwn(t *testing.T) {
	ev := Event{
		Spends: []SpendRecord{
			{Basis: BasisUSD, USD: 0.01},              // same currency (both default/USD): counted
			{Basis: BasisUSD, USD: 5, Currency: "CR"}, // a different currency: skipped
		},
	}
	if got := ev.Figures().PluginSpendMicros; got != 10_000 {
		t.Errorf("PluginSpendMicros = %d, want 10000 (only the same-currency spend)", got)
	}

	credits := Event{
		Currency: "CR",
		Spends: []SpendRecord{
			{Basis: BasisUSD, USD: 5, Currency: "CR"}, // same currency as the record: counted
			{Basis: BasisUSD, USD: 0.01},              // USD (the default): skipped here
		},
	}
	if got := credits.Figures().PluginSpendMicros; got != 5_000_000 {
		t.Errorf("PluginSpendMicros = %d, want 5000000 (only the credits-currency spend)", got)
	}
}

// The same currency gate applies to basis unknown, not just usd — PluginSpendUnknownMicros is
// "not money" (design §5.7a), but it is still one number, and a USD figure and a Bobcoin figure
// summed into it would be just as mixed (review finding; probe matched here: a $0.02 USD spend
// and a 5-Bobcoin spend, both basis unknown, on a record whose own Currency is Bobcoins).
func TestFigures_SkipsAnUnknownBasisSpendInACurrencyNotTheRecordsOwn(t *testing.T) {
	ev := Event{
		Currency: "Bobcoins",
		Spends: []SpendRecord{
			{Basis: BasisUnknown, USD: 5, Currency: "Bobcoins"}, // same currency: counted
			{Basis: BasisUnknown, USD: 0.02},                    // USD (the default): skipped
		},
	}
	if got := ev.Figures().PluginSpendUnknownMicros; got != 5_000_000 {
		t.Errorf("PluginSpendUnknownMicros = %d, want 5000000 (only the Bobcoins-currency spend)", got)
	}
}

// A penalty bigger than the saving is a real net cost: AvoidedAPIEquivalentMicros and
// AvoidedUnknownMicros must go negative, the same way PenaltyUnits already can exceed
// AvoidedUnits on the same line (review finding: both used to floor at 0 via MicrosOrZero,
// which reported a net cost as a net gain of nothing and disagreed in sign with the units
// figure beside it).
func TestFigures_NegativeNetIsNegative(t *testing.T) {
	ev := Event{
		Avoided: []Saving{
			{Basis: BasisSubscriptionEquivalent, USD: 0.10, PenaltyUSD: 0.40, Units: 1, PenaltyUnits: 5},
			{Basis: BasisUnknown, USD: 0.10, PenaltyUSD: 0.40},
		},
	}
	got := ev.Figures()
	if got.AvoidedAPIEquivalentMicros != -300_000 {
		t.Errorf("AvoidedAPIEquivalentMicros = %d, want -300000", got.AvoidedAPIEquivalentMicros)
	}
	if got.AvoidedUnknownMicros != -300_000 {
		t.Errorf("AvoidedUnknownMicros = %d, want -300000", got.AvoidedUnknownMicros)
	}
	// The units figure on the same line was already allowed to go negative; the two must agree
	// in sign, not just both be "small".
	if got.PenaltyUnits-got.AvoidedUnits <= 0 {
		t.Errorf("units net is not negative: avoided=%d penalty=%d", got.AvoidedUnits, got.PenaltyUnits)
	}
}

func TestMilliUnits(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want int64
	}{{0, 0}, {1.2345, 1235}, {-1, 0}, {math.NaN(), 0}, {math.Inf(1), 0}, {1e20, 0}} {
		if got := MilliUnits(tc.in); got != tc.want {
			t.Errorf("MilliUnits(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A keep-alive ping's record is told apart by its Purpose.
func TestIsKeepAlive(t *testing.T) {
	if !(Event{Purpose: PurposeKeepAlive}).IsKeepAlive() || (Event{}).IsKeepAlive() {
		t.Error("IsKeepAlive does not follow Purpose")
	}
}

// A keep-alive ping's spend is split by its basis like a plugin's: only a usd ping is money
// (KeepAliveMicros), a subscription ping is its usage-limit units with its API-equivalent dollars
// beside them, and an unknown one is shown apart. A ping with no basis was written before the
// split and reads as usd, as it was booked then. A record that is not a ping books none of these.
func TestFigures_KeepAliveSplitsByBasis(t *testing.T) {
	ping := func(basis string) Event {
		return Event{CostUSD: 0.002, Settled: true, Provenance: "configured", Purpose: PurposeKeepAlive,
			Basis: basis, Units: 12.5}
	}
	for _, tc := range []struct {
		basis string
		want  Figures
	}{
		{"", Figures{KeepAliveMicros: 2000}},
		{BasisUSD, Figures{KeepAliveMicros: 2000}},
		{BasisSubscriptionEquivalent, Figures{KeepAliveUnits: 12500, KeepAliveAPIEquivalentMicros: 2000}},
		{BasisUnknown, Figures{KeepAliveUnknownMicros: 2000}},
	} {
		if got := ping(tc.basis).Figures(); got != tc.want {
			t.Errorf("basis %q: Figures = %+v, want %+v", tc.basis, got, tc.want)
		}
	}
	notPing := ping(BasisUSD)
	notPing.Purpose = ""
	if got := notPing.Figures(); !got.IsZero() {
		t.Errorf("a request's record booked keep-alive figures: %+v", got)
	}
}

// #2137: the compaction record is additive. A record with none has no "compaction" key, and one
// with no ended change writes "ended":[] and not null.
func TestEvent_CompactionWire(t *testing.T) {
	plain, err := json.Marshal(Event{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "compaction") {
		t.Errorf("record with no compaction = %s, want no compaction key", plain)
	}
	b, err := json.Marshal(Event{Compaction: &CompactionRecord{Trigger: "unknown", Ended: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"compaction":{"trigger":"unknown","ended":[]}`) {
		t.Errorf("record = %s, want the compaction record with ended []", b)
	}
}

// The interaction is money on its own figure, signed, and in no operator's AvoidedMicros
// (rossoctl/cortex#2139).
func TestFigures_InteractionIsSignedAndApart(t *testing.T) {
	ev := Event{InteractionUSD: 0.0041, Avoided: []Saving{{Basis: BasisUSD, USD: 0.01}}}
	got := ev.Figures()
	if got.InteractionMicros != 4_100 || got.AvoidedMicros != 10_000 {
		t.Errorf("InteractionMicros = %d, AvoidedMicros = %d, want 4100 and 10000", got.InteractionMicros, got.AvoidedMicros)
	}
	if got := (Event{InteractionUSD: -0.002}).Figures(); got.InteractionMicros != -2_000 || got.IsZero() {
		t.Errorf("a negative interaction = %d, want -2000 and a figure", got.InteractionMicros)
	}
}
