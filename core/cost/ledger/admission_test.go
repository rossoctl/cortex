package ledger

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// refusedEvent builds the response event settle.implausibleUnparsedCost publishes: a
// cost record carrying a REFUSAL and no figure, on a response with no inference
// extension.
//
// NO EXTENSION IS NOT A CHOICE THIS FIXTURE MAKES — it is the only shape the refusal
// has. implausibleUnparsedCost is gated on a nil extension ("GATED ON A NIL EXTENSION,
// which is exactly the set of responses whose spend was newly admitted"), so every
// refusal the pipeline can produce looks like this. That is what made the ledger's
// admission rule drop all of them rather than some.
func refusedEvent(t *testing.T, host string) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(event.Event{
		// CostUSD zero and Settled false: a refused figure is not a figure. settle.Settle
		// drops the number when it sets the reason, and event.Priced would return false
		// on the reason alone even if a future producer forgot to.
		Source:         event.SourceGatewayHeader,
		Provenance:     "authoritative",
		RejectedReason: event.RejectedImplausible,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: host,
		Plugins: map[string]json.RawMessage{event.Key: rec},
	}
}

// TestRecord_ARefusedCostFigureIsRecordedAsACoverageGap is the third defect.
//
// The admission rule was `Inference != nil || (record && Priced())`. A refusal has no
// extension by construction and is unpriced by definition, so it satisfied neither and
// no row was written — while the ring counted the response. The refusal exists to keep a
// coverage gap NAMEABLE ("the refusal is published instead ... so the coverage gap stays
// nameable"), and the durable surface, which is the one an operator reads to answer
// "what did today cost", dropped every one of them: a response from an unparsed endpoint
// claiming $50,000 left no trace at all in a file retained for thirty days.
//
// RECORDED AS PRICEABLE-BUT-UNPRICED, which is the disclosure the existing arithmetic
// already carries. A cost figure was on the wire, so the request COULD have been priced
// — that is what PriceableRequests counts — and nothing priced it, so priced-versus-
// priceable is short by one and every consumer of that ratio already renders the gap.
// The dollars stay zero: the whole point of the refusal is that there is no figure.
func TestRecord_ARefusedCostFigureIsRecordedAsACoverageGap(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	w.Record("s1", refusedEvent(t, "unparsed.example"))
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("ledger holds %d rows, want 1: a response whose cost figure was REFUSED is "+
			"traffic that happened and could not be priced, and the ring counts it — dropping it "+
			"here is the ring-versus-window=today divergence, and it hides the only evidence a "+
			"refusal ever occurred", len(rows))
	}
	got := rows[0]
	if got.Endpoint != "unparsed.example" {
		t.Errorf("Endpoint = %q, want the host the figure came from", got.Endpoint)
	}
	if got.CostMicros != 0 || got.PricedRequests != 0 {
		t.Errorf("CostMicros = %d, PricedRequests = %d; want both zero — a refused figure must "+
			"not reach a total under any label", got.CostMicros, got.PricedRequests)
	}
	if got.Requests != 1 {
		t.Errorf("Requests = %d, want 1", got.Requests)
	}
	if got.PriceableRequests != 1 {
		t.Errorf("PriceableRequests = %d, want 1: a figure was on the wire and was declined, so "+
			"this is a request that could have been priced and was not. Zero here would leave "+
			"priced-versus-priceable at parity and report complete coverage over a gap",
			got.PriceableRequests)
	}
}

// A SAVING on a record with no inference extension must still reach the ledger.
//
// The guard's sibling test below establishes that an unpriced, unrefused record over
// non-inference traffic stays out. This is the one exception, and it exists because
// event.Record's own doc names "a saving on a request that could not be priced" as the
// case it was split out from Decode to serve. Without it the durable file dropped exactly
// that subset — silently, and only from disk, while session.sumCost counted it, so the two
// money surfaces disagreed on a case neither documented.
//
// THE COVERAGE COUNTERS MUST NOT MOVE, which is what the guard is actually protecting.
// Asserted here rather than assumed: a row admitted on the saving alone carries no model and
// no tokens, so PriceableRequests stays zero and priced-versus-priceable is untouched. If
// this row ever set it, a deployment would read a permanent coverage gap it cannot close,
// which is the "1/10 priced forever" failure the guard was written against.
func TestRecord_ASavingWithNoInferenceExtensionIsStillARow(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	e := refusedEvent(t, "mcp.example")
	// Present, unpriced, nothing declined — and a real applied saving.
	rec, err := json.Marshal(event.Event{
		Source: event.SourceUsageFallback, Provenance: "bundled",
		Avoided: []event.Saving{{
			Component: "tool-prune", TokensAvoided: 1000, USD: 0.10, Provenance: "bundled", Tier: "input",
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e.Plugins[event.Key] = rec
	w.Record("s1", e)
	if ferr := w.Flush(); ferr != nil {
		t.Fatalf("Flush: %v", ferr)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("ledger holds %d rows, want 1: a measured saving is money-not-spent and this "+
			"file is its durable record", len(rows))
	}
	if rows[0].AvoidedMicros != 100_000 {
		t.Errorf("AvoidedMicros = %d, want 100000", rows[0].AvoidedMicros)
	}
	if rows[0].CostMicros != 0 || rows[0].PricedRequests != 0 {
		t.Errorf("CostMicros = %d, PricedRequests = %d, want both zero: a saving is not spend",
			rows[0].CostMicros, rows[0].PricedRequests)
	}
	if rows[0].PriceableRequests != 0 {
		t.Errorf("PriceableRequests = %d, want 0 — this row carries no model and no tokens, so "+
			"counting it as priceable would open a coverage gap nothing can close",
			rows[0].PriceableRequests)
	}
}

// TestRecord_AnUnpricedRecordWithNoRefusalIsStillNotARow keeps the fix narrow.
//
// The admission guard's "PRICED, not merely present" rule is there because a record that
// exists and priced nothing adds no dollars, so admitting it would inflate the request
// count without moving the money — the denominator mistake that made a correct
// deployment read "1/10 priced". A refusal is admitted because a figure WAS on the wire;
// a record with no figure and no refusal is not evidence of anything and must stay out.
func TestRecord_AnUnpricedRecordWithNoRefusalIsStillNotARow(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	e := refusedEvent(t, "mcp.example")
	// Same record, refusal removed: present, unpriced, nothing declined.
	rec, err := json.Marshal(event.Event{Source: event.SourceGatewayHeader, Provenance: "authoritative"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e.Plugins[event.Key] = rec
	w.Record("s1", e)
	if ferr := w.Flush(); ferr != nil {
		t.Fatalf("Flush: %v", ferr)
	}

	if rows := readAllRows(t, dir); len(rows) != 0 {
		t.Errorf("ledger holds %d rows, want 0: %+v — an unpriced record over non-inference "+
			"traffic is what the priceable denominator must not absorb", len(rows), rows)
	}
}

// TestRecord_APromptOnlyOrAvoidedOnlyRecordIsARowWithNoDollars pins the other half of
// what a per-minute row can and cannot say, because it was reported as a defect and is
// not one.
//
// Both records DO get a row — through the extension, not through the cost — and NEITHER
// PUTS A DOLLAR IN CostMicros. Where they now differ is whether the figure is persisted
// at all:
//
//   - PromptUSD is the modelled PROMPT half of one call's cost, and still has no column.
//     It is a component of a figure, published so a REQUEST row can show what that row
//     cost; the response row carries the call's total in CostMicros, which is what this
//     file accumulates. There is nothing missing from the ledger's total.
//   - Avoided IS persisted now, in AvoidedMicros, which is #972's decision and arrived
//     after this test did. It is a column of its own, never a contribution to CostMicros:
//     costevent is explicit that no consumer may add money-not-spent to spend. Row embeds
//     usage.Counts, so the ring carries the same field under the same name across a
//     window and the two surfaces still AGREE — which is what that requirement was
//     really about, not the absence of the column.
//
// So the honest statement is: a ledger row's DOLLARS are spend, and a saving rides beside
// them under a name that cannot be mistaken for one. This test is what makes that a pinned
// claim rather than an omission a reader has to infer — and the avoided case asserts the
// figure LANDS as well as staying out of spend, because a column silently dropped on the
// write path would satisfy every other assertion here.
func TestRecord_APromptOnlyOrAvoidedOnlyRecordIsARowWithNoDollars(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  event.Event
		// wantAvoided is the row's AvoidedMicros. Stated per case rather than derived, so
		// the prompt-only case pins that a prompt component reaches NEITHER dollar column.
		wantAvoided int64
	}{
		{name: "prompt-only", rec: event.Event{
			Source: event.SourceUsageFallback, Provenance: "bundled",
			PromptUSD: 0.25,
		}},
		{name: "avoided-only", rec: event.Event{
			Source: event.SourceUsageFallback, Provenance: "bundled",
			Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 1000, USD: 0.10, Provenance: "bundled", Tier: "input"}},
		}, wantAvoided: 100_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w := newTestWriter(t, dir, func() time.Time { return at })
			e := costedEvent(t, "gw", "m", 0, 100, 50)
			raw, err := json.Marshal(tc.rec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			e.Plugins[event.Key] = raw
			w.Record("s1", e)
			if ferr := w.Flush(); ferr != nil {
				t.Fatalf("Flush: %v", ferr)
			}

			rows := readAllRows(t, dir)
			if len(rows) != 1 {
				t.Fatalf("ledger holds %d rows, want 1: the inference extension admits the row", len(rows))
			}
			if rows[0].CostMicros != 0 || rows[0].PricedRequests != 0 {
				t.Errorf("CostMicros = %d, PricedRequests = %d, want both zero: neither a prompt "+
					"component nor a saving is spend", rows[0].CostMicros, rows[0].PricedRequests)
			}
			// The saving's own column: persisted for the avoided case, untouched by the
			// prompt-only one. Both directions matter — the first is the feature, the second
			// is the guarantee that a component of a real cost is not filed as a saving.
			if rows[0].AvoidedMicros != tc.wantAvoided {
				t.Errorf("AvoidedMicros = %d, want %d", rows[0].AvoidedMicros, tc.wantAvoided)
			}
			// Priceable through the ordinary model-and-tokens test, so the coverage gap is
			// already visible without either figure.
			if rows[0].PriceableRequests != 1 {
				t.Errorf("PriceableRequests = %d, want 1", rows[0].PriceableRequests)
			}
		})
	}
}
