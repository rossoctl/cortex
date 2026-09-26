package costevent

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
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

// TestEventJSONTagsArePinned guards the wire format. abctl decodes these exact
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
// abctl decodes this struct out of process, so a tag that changes — or a new field whose tag
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
		Settled:          true,
		Incomplete:       true,
		IncompleteReason: "output-uncounted",
		// A cache-blind header that lost to the table, and the figure it had reported. These
		// two ride together by construction — see costing.gatewayFigureOf.
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
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"cost_usd":0.25,"source":"gateway-header","daily_total_usd":3.5,` +
		`"daily_max_usd":10,"provenance":"authoritative","header_omitted_cache":true,` +
		`"gateway_usd":0.000205,"settled":true,"incomplete":true,` +
		`"incomplete_reason":"output-uncounted","prompt_usd":0.2,"output_usd":0.05,` +
		`"tiers":{"input":0.007,"cache_write":0.022,"cache_read":0.3,"output":0.46},` +
		`"usage_refused":true,"rejected_reason":"implausible",` +
		`"avoided":[{"component":"tool-prune",` +
		`"tokensAvoided":1200,"usd":0.01,"provenance":"configured"}]}`
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
	if got, want := ev.TotalAvoidedMicros(), int64(3_880); got != want {
		t.Errorf("TotalAvoidedMicros = %d, want %d", got, want)
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
	const pinned = 16
	if got := reflect.TypeOf(Event{}).NumField(); got != pinned {
		t.Fatalf("Event has %d fields, %d are pinned on the wire.\n"+
			"Add the new field to TestEventJSONTagsArePinned_EveryField's marshal AND its "+
			"expected string, or abctl will never see it.", got, pinned)
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
// (costing.implausibleUnparsedCost). The record is published so the coverage gap is
// nameable, which means it travels to every consumer that reads a cost — so the predicate
// they all ask has to answer no.
//
// The second row is the one worth having: a reason set ALONGSIDE a figure. No producer
// writes that today (costing.Settle leaves CostUSD zero when it rejects), and the check is
// here precisely so a future one that forgets to drop the number cannot turn a refusal into
// a charge. Fail-closed on money.
func TestPriced_RefusedFigureIsNeverSpend(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   Event
	}{
		{"the shape costing publishes", Event{RejectedReason: RejectedImplausible}},
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
