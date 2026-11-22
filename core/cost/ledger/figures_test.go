package ledger

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
)

// The accountant's figures reach the durable row (design §5.9, §5.7a): penalty beside the gross
// saving, the usage-limit units of a subscription saving, an unknown one apart, and each plugin
// spend by basis — and they come back from the file as written.
func TestRow_PenaltyMicrosRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })
	e := costedEvent(t, "api.anthropic.com", "claude-opus-5", 0.25, 100, 10)
	rec, err := json.Marshal(event.Event{
		CostUSD: 0.25, Settled: true, Provenance: "configured", Source: event.SourceUsageFallback,
		Avoided: []event.Saving{
			{Component: "context-guru.mask", Basis: event.BasisUSD, USD: 0.01, PenaltyUSD: 0.03},
			{Component: "tool-prune", Basis: event.BasisSubscriptionEquivalent, USD: 1, Units: 4.5, PenaltyUnits: 0.5},
			{Component: "tool-prune", Basis: event.BasisUnknown, USD: 0.2},
		},
		Spends: []event.SpendRecord{{Basis: event.BasisUSD, USD: 0.004}, {Basis: event.BasisSubscriptionEquivalent, Units: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Plugins[event.Key] = rec
	w.Record("s1", e)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.AvoidedMicros != 10_000 || r.PenaltyMicros != 30_000 {
		t.Errorf("avoided/penalty = %d/%d, want 10000/30000: gross, a net of -20000 kept as two figures",
			r.AvoidedMicros, r.PenaltyMicros)
	}
	if r.AvoidedUnits != 4_500 || r.PenaltyUnits != 500 || r.AvoidedUnknownMicros != 200_000 {
		t.Errorf("units %d/%d, unknown %d; want 4500/500 milli-units and 200000", r.AvoidedUnits, r.PenaltyUnits, r.AvoidedUnknownMicros)
	}
	if r.PluginSpendMicros != 4_000 || r.PluginSpendUnits != 2_000 || r.CostMicros != 250_000 {
		t.Errorf("plugin spend %d micros / %d units, cost %d; want 4000, 2000 and the request's own 250000",
			r.PluginSpendMicros, r.PluginSpendUnits, r.CostMicros)
	}
	if r.Requests != 1 {
		t.Errorf("Requests = %d, want 1", r.Requests)
	}
}

// A row written before these fields existed reads them as 0.
func TestRow_AnOldRowReadsTheNewFieldsAsZero(t *testing.T) {
	const old = `{"at":"2026-11-01T10:00:00Z","endpoint":"e","model":"m","requests":3,"costMicros":900,"avoidedMicros":120}`
	var r Row
	if err := json.Unmarshal([]byte(old), &r); err != nil {
		t.Fatal(err)
	}
	if r.AvoidedMicros != 120 || r.PenaltyMicros != 0 || r.AvoidedUnits != 0 || r.PenaltyUnits != 0 ||
		r.AvoidedUnknownMicros != 0 || r.PluginSpendMicros != 0 {
		t.Errorf("old row = %+v, want the new fields zero and the old ones as written", r.Counts)
	}
}
