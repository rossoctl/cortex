package usage

import (
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// A Bob response priced in Bobcoins and a Claude Code response priced in dollars, each carrying
// its settled record — the shape the proxy records.
func twoUnitRing(t *testing.T, now time.Time) *Aggregator {
	t.Helper()
	a := New(WithClock(func() time.Time { return now }))
	bob := withCostRecord(t, pricedRespEvent("api.us-east.bob.ibm.com", "premium-ide", 3852, 47),
		event.Event{CostUSD: 0.0078, Settled: true, Provenance: "configured", Currency: "Bobcoins"})
	bob.Client = &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}
	claude := withCostRecord(t, pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500),
		event.Event{CostUSD: 0.25, Settled: true, Provenance: "configured"})
	claude.Client = &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	a.Record("bob-task", bob)
	a.Record("claude-session", claude)
	a.Record("bob-task", plainResponse(now, bob.Client))
	a.Record("bob-task", unpricedInference(bob.Client))
	return a
}

func unpricedInference(c *pipeline.EventClient) *pipeline.SessionEvent {
	e := pricedRespEvent("localhost:11434", "llama3", 10, 10)
	e.Client = c
	return e
}

// plainResponse is traffic that carries no money: bridged HTTPS to a host that is not an
// inference endpoint.
func plainResponse(now time.Time, c *pipeline.EventClient) *pipeline.SessionEvent {
	return &pipeline.SessionEvent{At: now, Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		Host: "github.com", StatusCode: 200, Client: c}
}

// The ring answers group=currency with a real per-unit breakdown, and names both units.
//
// This replaces TestSnapshot_TheRingServesGroupCurrencyAsNone, which pinned the ring's old
// inability. Its concern survives here: echoing the group over an empty series would publish the
// whole cross-unit total as the part the breakdown left out, so the series must SUM to the total
// and nothing may be reported as ungrouped.
func TestSnapshot_TheRingBreaksTotalsDownByUnit(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	snap := twoUnitRing(t, now).Snapshot(10*BucketWidth, BucketWidth, "", GroupCurrency)

	if snap.Group != GroupCurrency {
		t.Fatalf("Group = %q, want %q", snap.Group, GroupCurrency)
	}
	series := FoldSeriesAcrossWindow(snap.Buckets)
	if got := series["Bobcoins"].CostMicros; got != 7_800 {
		t.Errorf("Bobcoins series = %d, want 7800", got)
	}
	if got := series[pricing.CurrencyUSD].CostMicros; got != 250_000 {
		t.Errorf("USD series = %d, want 250000", got)
	}
	if snap.UngroupedCostMicros != nil {
		t.Errorf("UngroupedCostMicros = %d, want absent: every request has a unit", *snap.UngroupedCostMicros)
	}
	if want := []string{"Bobcoins", pricing.CurrencyUSD}; !slices.Equal(snap.Currencies, want) {
		t.Errorf("Currencies = %v, want %v", snap.Currencies, want)
	}
}

// A ring window names its units under every grouping, as a ledger window does — so a client
// refusing a cross-unit sum can refuse it on the LAST 1H figure too.
func TestSnapshot_TheRingNamesItsUnitsUngrouped(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	snap := twoUnitRing(t, now).Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
	if want := []string{"Bobcoins", pricing.CurrencyUSD}; !slices.Equal(snap.Currencies, want) {
		t.Errorf("Currencies = %v, want %v; a mixed ring total must say it is mixed", snap.Currencies, want)
	}
}

// Dollars-only traffic reports USD, exactly as a ledger window over the same rows does
// (ledger.CurrenciesIn), and an idle window names nothing.
func TestSnapshot_TheRingReportsDollarsLikeTheLedger(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	if snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone); snap.Currencies != nil {
		t.Errorf("idle window Currencies = %v, want none: there is no figure to label", snap.Currencies)
	}
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.25))
	snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone)
	if want := []string{pricing.CurrencyUSD}; !slices.Equal(snap.Currencies, want) {
		t.Errorf("Currencies = %v, want %v", snap.Currencies, want)
	}

	bob := New(WithClock(func() time.Time { return now }))
	bob.Record("s1", withCostRecord(t, pricedRespEvent("api.us-east.bob.ibm.com", "premium-ide", 3852, 47),
		event.Event{CostUSD: 0.0078, Settled: true, Currency: "Bobcoins"}))
	bob.Record("s1", plainResponse(now, nil))
	bob.Record("s1", unpricedInference(nil))
	if got := bob.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone).Currencies; !slices.Equal(got, []string{"Bobcoins"}) {
		t.Errorf("Bobcoins-only window Currencies = %v, want [Bobcoins]: nothing else was priced", got)
	}
}

// A request the ring prices itself, from the rate table, is in the endpoint's unit — there is no
// record to carry one, so the ring must ask the same rule the record's producer does.
func TestSnapshot_TheRingsOwnPricingCarriesTheEndpointsUnit(t *testing.T) {
	tbl, err := pricing.Build(&pricing.Config{Endpoints: []pricing.EndpointConfig{{
		Hosts: []string{"api.us-east.bob.ibm.com"}, Unit: "Bobcoins",
		Models: map[string]pricing.ModelConfig{
			"*": {TierRates: pricing.TierRates{InputCostPerMillion: 2, OutputCostPerMillion: 2}},
		},
	}}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }), WithPricing(pricing.NewRegistry(tbl)))
	a.Record("s1", pricedRespEvent("api.us-east.bob.ibm.com", "openai/gpt-oss-20b", 1355, 40))

	snap := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupCurrency)
	if snap.Totals.PricedRequests != 1 {
		t.Fatalf("PricedRequests = %d, want 1; the fixture priced nothing", snap.Totals.PricedRequests)
	}
	series := FoldSeriesAcrossWindow(snap.Buckets)
	if _, ok := series[pricing.CurrencyUSD]; ok {
		t.Errorf("series = %v: a Bobcoins charge was filed as dollars", series)
	}
	if got := series["Bobcoins"].CostMicros; got != snap.Totals.CostMicros {
		t.Errorf("Bobcoins series = %d, want the whole %d", got, snap.Totals.CostMicros)
	}
}

// group=agent, model and endpoint carry each series' own units, so one row can be labelled without
// borrowing the window's mixture.
func TestSnapshot_SeriesCurrenciesNamesEachAgentsUnits(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	snap := twoUnitRing(t, now).Snapshot(10*BucketWidth, BucketWidth, "", GroupAgent)

	// Keyed by the folded name: the ring records "bob-shell/2.0.5" (twoUnitRing), the
	// axis folds it to "bob", and the two must not split into a row each.
	if got := snap.SeriesCurrencies["bob"]; !slices.Equal(got, []string{"Bobcoins"}) {
		t.Errorf("bob units = %v, want [Bobcoins]", got)
	}
	if got := snap.SeriesCurrencies["claude-code"]; !slices.Equal(got, []string{pricing.CurrencyUSD}) {
		t.Errorf("claude-code units = %v, want [USD]", got)
	}
	// The drawer's other two axes carry it too, keyed like their own series.
	byModel := twoUnitRing(t, now).Snapshot(10*BucketWidth, BucketWidth, "", GroupModel).SeriesCurrencies
	if got := byModel["premium-ide"]; !slices.Equal(got, []string{"Bobcoins"}) {
		t.Errorf("model premium-ide units = %v, want [Bobcoins]", got)
	}
	byEndpoint := twoUnitRing(t, now).Snapshot(10*BucketWidth, BucketWidth, "", GroupEndpoint).SeriesCurrencies
	if got := byEndpoint["gw.internal"]; !slices.Equal(got, []string{pricing.CurrencyUSD}) {
		t.Errorf("endpoint gw.internal units = %v, want [USD]", got)
	}
	// Every other grouping's response is unchanged.
	if other := twoUnitRing(t, now).Snapshot(10*BucketWidth, BucketWidth, "", GroupStatus); other.SeriesCurrencies != nil {
		t.Errorf("group=status carries SeriesCurrencies %v; it is defined for the drawer's axes only", other.SeriesCurrencies)
	}
}

// ScopeToAgent labels the scoped figure with THAT agent's units when the producer sent them,
// instead of carrying the window's mixture over.
func TestScopeToAgent_NarrowsCurrenciesToTheAgent(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	snap := twoUnitRing(t, now).Snapshot(10*BucketWidth, BucketWidth, "", GroupAgent)
	scoped, err := ScopeToAgent(&snap, "claude-code", KeepBuckets)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{pricing.CurrencyUSD}; !slices.Equal(scoped.Currencies, want) {
		t.Errorf("scoped Currencies = %v, want %v: Claude's dollars must not be withheld for Bob's credits",
			scoped.Currencies, want)
	}
	if !slices.Equal(snap.Currencies, []string{"Bobcoins", pricing.CurrencyUSD}) {
		t.Errorf("the caller's snapshot was mutated: Currencies = %v", snap.Currencies)
	}

	// Without the field — an older producer — the window's list is kept, today's behaviour.
	snap.SeriesCurrencies = nil
	legacy, err := ScopeToAgent(&snap, "claude-code", KeepBuckets)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(legacy.Currencies, []string{"Bobcoins", pricing.CurrencyUSD}) {
		t.Errorf("legacy scoped Currencies = %v, want the window's list carried over", legacy.Currencies)
	}
}

// A gateway-priced response carries its figure on the record alone, with no parsed inference; it
// is priced traffic and names its unit, or its Bobcoins would be summed into the dollar total.
func TestSnapshot_ARecordPricedWithoutInferenceNamesItsUnit(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	gw := plainResponse(now, nil)
	gw.Host = "api.us-east.bob.ibm.com"
	a.Record("s1", withCostRecord(t, gw, event.Event{CostUSD: 3, Settled: true, Currency: "Bobcoins"}))
	a.Record("s1", withCost(t, respEvent(now, 200, time.Second, "claude-opus-5", 1000), 0.25))
	if got := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone).Currencies; !slices.Equal(got, []string{"Bobcoins", pricing.CurrencyUSD}) {
		t.Errorf("Currencies = %v, want [Bobcoins USD]", got)
	}
}

// A saving on a request that could not be priced is still a figure — tool-prune's avoided cost
// lands in Totals.AvoidedMicros — so it names its unit, or a Bobcoins saving prints as dollars.
func TestSnapshot_ASavingOnAnUnpricedRequestNamesItsUnit(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", withCostRecord(t, plainResponse(now, nil), event.Event{Currency: "Bobcoins",
		Avoided: []event.Saving{{Basis: event.BasisUSD, Component: "tool-prune", USD: 0.5}}}))
	if got := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone).Currencies; !slices.Equal(got, []string{"Bobcoins"}) {
		t.Errorf("Currencies = %v, want [Bobcoins]", got)
	}
}
