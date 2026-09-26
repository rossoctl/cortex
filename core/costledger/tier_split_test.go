package costledger

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/pipeline"
)

// setTiers attaches a modelled per-tier split to an event's cost record, the way
// costing.NewRecord publishes one whenever the rate table resolved.
//
// A MUTATOR OVER costedEvent RATHER THAN A SECOND FIXTURE, matching setProvenance and
// markIncomplete: the split is one more field on the same record, and a parallel builder
// would drift from costedEvent the first time the base shape changed.
func setTiers(t *testing.T, e *pipeline.SessionEvent, input, cacheWrite, cacheRead, output float64) {
	t.Helper()
	ev, ok := costevent.Record(e)
	if !ok {
		t.Fatal("event carries no cost record")
	}
	ev.Tiers = &costevent.TierCost{
		Input: input, CacheWrite: cacheWrite, CacheRead: cacheRead, Output: output,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e.Plugins[costevent.Key] = raw
}

// TestRecord_PersistsThePublishedTierSplit is the defect this file exists for.
//
// Record settled CostMicros off the published record and dropped its Tiers on the floor, so
// every row on disk carried a total with no breakdown. The consumer is
// usage.Counts.ApportionTiers, which returns ok=false when the mix is empty — and the four
// surfaces that read it (abctl's spend drawer, `abctl cost`, its JSON, the /v1/usage
// response) all render "not known here" rather than a breakdown. Measured before the fix:
// window=1h served the ring's split while window=today, 7d and month served none, which is
// the split being populated on one of two paths that the mix's own doc warns about.
func TestRecord_PersistsThePublishedTierSplit(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	setTiers(t, e, 0.01, 0.04, 0.06, 0.14)
	w.Record("s1", e)

	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	// IN MICROS ON DISK, because usage.Counts keeps money as an integer for exact addition
	// across buckets — see its CostMicros doc. The record carries dollars, so this asserts
	// the conversion as well as the copy.
	for _, c := range []struct {
		name string
		got  int64
		want int64
	}{
		{"InputCostMicros", r.InputCostMicros, 10_000},
		{"CacheWriteCostMicros", r.CacheWriteCostMicros, 40_000},
		{"CacheReadCostMicros", r.CacheReadCostMicros, 60_000},
		{"OutputCostMicros", r.OutputCostMicros, 140_000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	// THE WHOLE POINT, asserted through the consumer rather than only on the fields: four
	// populated columns that ApportionTiers still refuses would leave every display exactly
	// as blank as before.
	if _, ok := r.ApportionTiers(); !ok {
		t.Error("ApportionTiers refused the persisted split; the display stays blank")
	}
}

// TestRecord_ARowAccumulatesTheTierSplit checks the split sums like every other figure.
//
// Rows are per-minute aggregates, so a second request in the same minute must add to the
// four columns rather than replace them. Cheap to get wrong: the assignment sits beside
// r.Counts' wholesale literal, and placing it BEFORE that literal would silently zero the
// split for every inference row — the trap r.AvoidedMicros already documents.
func TestRecord_ARowAccumulatesTheTierSplit(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	for range 3 {
		e := costedEvent(t, "gw", "m", 0.25, 100, 50)
		setTiers(t, e, 0.01, 0, 0, 0.14)
		w.Record("s1", e)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 accumulated row", len(rows))
	}
	if got := rows[0].InputCostMicros; got != 30_000 {
		t.Errorf("InputCostMicros = %d, want 30000 (3 x 10000)", got)
	}
	if got := rows[0].OutputCostMicros; got != 420_000 {
		t.Errorf("OutputCostMicros = %d, want 420000 (3 x 140000)", got)
	}
	// Absent from the mix stays absent under accumulation: three requests that wrote no
	// cache must not acquire a cache-write charge between them.
	if got := rows[0].CacheWriteCostMicros; got != 0 {
		t.Errorf("CacheWriteCostMicros = %d, want 0 for traffic that wrote no cache", got)
	}
}

// TestRecord_NoPublishedTiersLeavesTheSplitEmpty pins the state that means "not known".
//
// A gateway-priced response on a model with no rates has a settled total and no breakdown,
// and costevent.Event.Tiers is nil for it. Four zeros is how that is said: ApportionTiers
// reads them as "no mix reached this row" and every display renders its not-known cell,
// which is a different claim from a tier having been FREE. Writing a zeroed split as though
// it were a real one would put "$0.00" under three tiers that certainly cost money.
func TestRecord_NoPublishedTiersLeavesTheSplitEmpty(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	// costedEvent publishes no Tiers, which is exactly the shape under test.
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.CostMicros != 250_000 {
		t.Fatalf("CostMicros = %d, want 250000: the total must survive an absent split", r.CostMicros)
	}
	if r.InputCostMicros|r.CacheWriteCostMicros|r.CacheReadCostMicros|r.OutputCostMicros != 0 {
		t.Errorf("split = %d/%d/%d/%d, want all zero for a record carrying no Tiers",
			r.InputCostMicros, r.CacheWriteCostMicros, r.CacheReadCostMicros, r.OutputCostMicros)
	}
	// OMITTED FROM THE FILE, not written as four zeros. usage.Counts tags these omitempty
	// precisely so absence costs no bytes and reads as absence; a `"inputCostMicros":0` on
	// disk would be a durable assertion that the tier was free.
	if b := readAllBytes(t, dir); bytesContains(b, "inputCostMicros") {
		t.Error("an absent split reached the file as a zero column; it must be omitted")
	}
}

// TestRecord_ARefusedFigureCarriesNoTierSplit keeps the refusal's own rule.
//
// A refused figure is not a figure, so it contributes no dollars and no mix. The row still
// exists — it is a coverage gap worth naming, see
// TestRecord_ARefusedCostFigureIsRecordedAsACoverageGap — and the split must stay empty on
// it even if a future producer sends Tiers alongside a rejection, because a ratio carries no
// magnitude and a discredited one looks exactly like a sound one on screen.
func TestRecord_ARefusedFigureCarriesNoTierSplit(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := refusedEvent(t, "gw")
	setTiers(t, e, 0.01, 0.04, 0.06, 0.14)
	w.Record("s1", e)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (the coverage gap)", len(rows))
	}
	r := rows[0]
	if r.PricedRequests != 0 || r.CostMicros != 0 {
		t.Fatalf("PricedRequests = %d, CostMicros = %d; want 0 and 0 for a refusal",
			r.PricedRequests, r.CostMicros)
	}
	if r.InputCostMicros|r.CacheWriteCostMicros|r.CacheReadCostMicros|r.OutputCostMicros != 0 {
		t.Errorf("split = %d/%d/%d/%d, want all zero: a refused figure publishes no mix",
			r.InputCostMicros, r.CacheWriteCostMicros, r.CacheReadCostMicros, r.OutputCostMicros)
	}
}
