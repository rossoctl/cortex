package usage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// truncatedRespEvent is a response event whose prompt counts landed and whose output
// count never did — a stream that died before message_delta.
//
// PresentKinds carries KindInput|KindOutput with a zero output tally, which is what
// Anthropic really emits, so nothing in these tests can pass by reading the mask.
func truncatedRespEvent(host, model string, input int) *pipeline.SessionEvent {
	return &pipeline.SessionEvent{
		Phase:      pipeline.SessionResponse,
		Host:       host,
		StatusCode: 200,
		Inference: &pipeline.InferenceExtension{
			Model:        model,
			InputTokens:  input,
			PromptTokens: input,
			TotalTokens:  input,
			PresentKinds: KindInput | KindOutput,
		},
	}
}

// withCostRecord attaches a published cost record to an event, the way the parser does.
func withCostRecord(t *testing.T, e *pipeline.SessionEvent, ev event.Event) *pipeline.SessionEvent {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	e.Plugins = map[string]json.RawMessage{event.Key: raw}
	return e
}

// An inexact figure is DISCLOSED, not adjusted. The dollars stay in CostMicros and the
// request stays in PricedRequests — both of those are true — and IncompleteRequests
// carries the caveat alongside.
func TestIncomplete_DisclosedNotDeducted(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	a.Record("s1", withCostRecord(t, truncatedRespEvent("gw.internal", "claude-opus-5", 1000),
		event.Event{
			CostUSD: 0.005, Source: event.SourceUsageFallback, Provenance: "configured",
			Settled: true, Incomplete: true, IncompleteReason: pricing.ReasonOutputUncounted,
		}))

	snap := snapshotOf(a, now)
	if want := int64(5_000); snap.Totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d: the money was really spent and refusing to count it understates spend by more than disclosing the floor does", snap.Totals.CostMicros, want)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1: this request WAS priced, and that counter answers coverage — a different question from exactness", snap.Totals.PricedRequests)
	}
	if snap.Totals.IncompleteRequests != 1 {
		t.Errorf("IncompleteRequests = %d, want 1: without this the total claims an exactness it does not have", snap.Totals.IncompleteRequests)
	}
	// A floor is not a pricing gap: no rate an operator could add would make it go away.
	if len(snap.UnpricedBy) != 0 {
		t.Errorf("UnpricedBy = %v, want empty: naming a floor there would point at a pricing entry that already exists", snap.UnpricedBy)
	}
	// Priced stays true, so a client renders the figure rather than "cost unavailable".
	if !snap.Priced {
		t.Error("Priced = false while CostMicros is non-zero; that breaks the flag's own contract")
	}
}

// THE TRAP this counter exists to avoid: PresentKinds is OR-folded across a bucket, so a
// bucket holding one truncated request and nine complete ones ORs in the other nine's
// Output bit and reads as fully exact. The folded mask answers "did any request here
// expose output", which is NOT "was every figure in this total exact".
//
// So the decision is made per request at settle time and COUNTED. This test is the proof
// that the count survives the fold where the mask cannot.
func TestIncomplete_SurvivesTheFoldWhereTheMaskCannot(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// Nine complete requests, each reporting output.
	for i := 0; i < 9; i++ {
		e := pricedRespEvent("gw.internal", "claude-opus-5", 100, 50)
		e.Inference.PresentKinds = KindInput | KindOutput
		e.Inference.FinishReason = "end_turn"
		a.Record("s1", withCostRecord(t, e, event.Event{
			CostUSD: 0.001, Source: event.SourceUsageFallback,
			Provenance: "configured", Settled: true,
		}))
	}
	// One truncated.
	a.Record("s1", withCostRecord(t, truncatedRespEvent("gw.internal", "claude-opus-5", 1000),
		event.Event{
			CostUSD: 0.005, Source: event.SourceUsageFallback, Provenance: "configured",
			Settled: true, Incomplete: true, IncompleteReason: pricing.ReasonOutputUncounted,
		}))

	snap := snapshotOf(a, now)
	if snap.Totals.Requests != 10 {
		t.Fatalf("Requests = %d, want 10", snap.Totals.Requests)
	}
	// The mask is now useless for this question, and that is the point.
	if snap.Totals.PresentKinds&8 == 0 {
		t.Fatal("fixture no longer ORs the Output bit; it does not reproduce the trap and the assertion below proves nothing")
	}
	if snap.Totals.IncompleteRequests != 1 {
		t.Errorf("IncompleteRequests = %d, want 1: the OR-folded PresentKinds reads as fully exact here, so a counter is the only thing that can carry this", snap.Totals.IncompleteRequests)
	}
	if snap.Totals.PricedRequests != 10 {
		t.Errorf("PricedRequests = %d, want 10", snap.Totals.PricedRequests)
	}
}

// The aggregator's OWN fallback figure gets the same test. There are two sources of cost
// in this package, and disclosing on one side of the fork only would leave the bug live
// in whichever way a figure happened to arrive.
func TestIncomplete_FallbackFigureIsDisclosedToo(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// No cost record at all: the aggregator prices it from its own table.
	a.Record("s1", truncatedRespEvent("gw.internal", "claude-opus-5", 1000))

	snap := snapshotOf(a, now)
	if want := int64(5_000); snap.Totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d", snap.Totals.CostMicros, want)
	}
	if snap.Totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	if snap.Totals.IncompleteRequests != 1 {
		t.Error("IncompleteRequests = 0; the fallback priced a prompt-only figure and presented it as exact")
	}
	// And the WAY it is inexact, on this arm too. pricing.IncompleteReason returns which of
	// the two it is, so dropping it here would leave a fallback-priced window unable to say
	// what a producer-priced one can — the same one-side-of-the-fork gap the counter itself
	// had.
	if got := snap.IncompleteBy[pricing.ReasonOutputUncounted]; got != 1 {
		t.Errorf("IncompleteBy[%s] = %d, want 1 (IncompleteBy = %v)", pricing.ReasonOutputUncounted, got, snap.IncompleteBy)
	}
}

// A complete figure must carry no caveat. A permanent warning with nothing to act on is
// what teaches an operator to ignore the one signal that matters.
func TestIncomplete_CompleteFigureCarriesNoCaveat(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	e := pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)
	e.Inference.PresentKinds = KindInput | KindOutput
	e.Inference.FinishReason = "end_turn"
	a.Record("s1", e)

	snap := snapshotOf(a, now)
	if snap.Totals.IncompleteRequests != 0 {
		t.Errorf("IncompleteRequests = %d, want 0", snap.Totals.IncompleteRequests)
	}
}

// Counts.Add must carry the new field. A previous copy of this summation in another
// module silently missed PricedRequests when it was added, under a comment explaining
// that every field had to be carried — so the field-by-field sum gets an assertion.
func TestIncomplete_AddCarriesTheCounter(t *testing.T) {
	c := Counts{Requests: 1, CostMicros: 10, PricedRequests: 1, IncompleteRequests: 1}
	c.Add(Counts{Requests: 2, CostMicros: 20, PricedRequests: 2, IncompleteRequests: 1})
	if c.IncompleteRequests != 2 {
		t.Errorf("IncompleteRequests = %d, want 2", c.IncompleteRequests)
	}
	// Summed alongside, never out of, PricedRequests: it is a subset disclosure.
	if c.PricedRequests != 3 {
		t.Errorf("PricedRequests = %d, want 3", c.PricedRequests)
	}
	if c.IncompleteRequests > c.PricedRequests {
		t.Error("IncompleteRequests exceeds PricedRequests; it is a SUBSET of it, and a client subtracting the two would go negative")
	}
}

// A totals-only gateway is approximate rather than partial, and the reason string is what
// lets a client tell a standing property of the gateway from a transient incident.
func TestIncomplete_TotalOnlyGatewayIsDisclosedAsApproximate(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// total_tokens only: no split, no presence bits.
	a.Record("s1", &pipeline.SessionEvent{
		Phase: pipeline.SessionResponse, Host: "gw.internal", StatusCode: 200,
		Inference: &pipeline.InferenceExtension{
			Model: "claude-opus-5", TotalTokens: 1000, FinishReason: "end_turn",
		},
	})

	snap := snapshotOf(a, now)
	if snap.Totals.PricedRequests != 1 {
		t.Fatalf("PricedRequests = %d, want 1: a bare total is still priced, approximately", snap.Totals.PricedRequests)
	}
	if snap.Totals.IncompleteRequests != 1 {
		t.Error("IncompleteRequests = 0; a figure modelled by attributing a bare total wholly to uncached input is not exact")
	}
	// The counter alone cannot say APPROXIMATE, which is the claim this test's name makes:
	// the same 1 appears for a truncated stream, whose figure is a floor. The reason is
	// what carries it.
	if got := snap.IncompleteBy[pricing.ReasonSplitUnreported]; got != 1 {
		t.Errorf("IncompleteBy[%s] = %d, want 1 (IncompleteBy = %v); a bare total is APPROXIMATE, and a client that cannot tell that from a floor has to render a standing property of the gateway as an incident",
			pricing.ReasonSplitUnreported, got, snap.IncompleteBy)
	}
}

// THE TWO REASONS MUST ARRIVE SEPARATELY, because they are different claims about money:
// a floor says the real figure is HIGHER ("at least $X"), an approximation says it is off
// in NO KNOWN DIRECTION ("roughly $X"). Counts.IncompleteRequests is one number and cannot
// express that — it read 2 here before IncompleteBy existed, and a client had no way to
// learn that one of the two was a truncated stream worth chasing and the other a permanent
// property of the gateway.
//
// Both figures reach the aggregate by the PRODUCER path (a published cost record) and the
// fallback path is covered by the two tests above, so between them each way a figure can
// arrive is asserted to carry its reason.
func TestIncomplete_SnapshotSeparatesTheTwoReasons(t *testing.T) {
	base := time.Now().Truncate(BucketWidth)
	clock := base
	a := New(WithClock(func() time.Time { return clock }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	// A floor: prompt counted, output never arrived.
	a.Record("s1", withCostRecord(t, truncatedRespEvent("gw.internal", "claude-opus-5", 1000),
		event.Event{
			CostUSD: 0.005, Source: event.SourceUsageFallback, Provenance: "configured",
			Settled: true, Incomplete: true, IncompleteReason: pricing.ReasonOutputUncounted,
		}))
	// An approximation, in the NEXT bucket, so this also pins that the breakdown sums
	// across the window rather than reporting only the newest minute — the same property
	// TestPricing_UnpricedByFoldsAcrossBuckets pins for the coverage gaps.
	clock = base.Add(BucketWidth)
	totalOnly := pricedRespEvent("gw.internal", "claude-opus-5", 0, 0)
	totalOnly.Inference.TotalTokens = 2000
	a.Record("s1", withCostRecord(t, totalOnly, event.Event{
		CostUSD: 0.01, Source: event.SourceUsageFallback, Provenance: "configured",
		Settled: true, Incomplete: true, IncompleteReason: pricing.ReasonSplitUnreported,
	}))

	snap := a.Snapshot(10*BucketWidth, 5*BucketWidth, "", GroupNone)
	if snap.Totals.IncompleteRequests != 2 {
		t.Fatalf("IncompleteRequests = %d, want 2; the fixture does not reproduce the case and the assertions below prove nothing", snap.Totals.IncompleteRequests)
	}
	if got := snap.IncompleteBy[pricing.ReasonOutputUncounted]; got != 1 {
		t.Errorf("IncompleteBy[%s] = %d, want 1 (IncompleteBy = %v)", pricing.ReasonOutputUncounted, got, snap.IncompleteBy)
	}
	if got := snap.IncompleteBy[pricing.ReasonSplitUnreported]; got != 1 {
		t.Errorf("IncompleteBy[%s] = %d, want 1 (IncompleteBy = %v)", pricing.ReasonSplitUnreported, got, snap.IncompleteBy)
	}
	// The sum invariant the field's godoc promises: a client subtracting the map from the
	// counter must get zero, or "the rest" reads as requests whose figures are exact.
	var sum int64
	for _, n := range snap.IncompleteBy {
		sum += n
	}
	if sum != snap.Totals.IncompleteRequests {
		t.Errorf("IncompleteBy sums to %d, want IncompleteRequests = %d; the difference would read as exact requests", sum, snap.Totals.IncompleteRequests)
	}
	// Provenance is unaffected: an inexact figure is still a priced one from a named
	// source, and the two maps answer different questions about the same request.
	if got := snap.PricedBy["configured"]; got != 2 {
		t.Errorf("PricedBy[configured] = %d, want 2; disclosing inexactness must not withdraw the provenance", got)
	}
}

// A producer that disclosed the caveat WITHOUT naming its kind — one predating
// event.Event.IncompleteReason — is counted under the reserved "unlabelled" key rather
// than dropped. Dropped, the map would sum to less than IncompleteRequests, and a client
// computing "the rest" from the difference would report inexact requests as exact: the
// exact class of false reassurance this whole disclosure exists to refuse.
func TestIncomplete_UnlabelledReasonIsStillBrokenOut(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }))

	a.Record("s1", withCostRecord(t, truncatedRespEvent("gw.internal", "claude-opus-5", 1000),
		event.Event{
			CostUSD: 0.005, Source: event.SourceUsageFallback, Settled: true,
			Incomplete: true, // no IncompleteReason, as an older producer sends it
		}))

	snap := snapshotOf(a, now)
	if snap.Totals.IncompleteRequests != 1 {
		t.Fatalf("IncompleteRequests = %d, want 1", snap.Totals.IncompleteRequests)
	}
	if got := snap.IncompleteBy["unlabelled"]; got != 1 {
		t.Errorf("IncompleteBy[unlabelled] = %d, want 1 (IncompleteBy = %v)", got, snap.IncompleteBy)
	}
	if len(snap.IncompleteBy) != 1 {
		t.Errorf("IncompleteBy = %v, want exactly one key: an unnamed reason must not be invented as either of the two real ones", snap.IncompleteBy)
	}
}

// Exact traffic emits NO map at all, rather than an empty object. A zeroed breakdown from a
// producer that never checked reads as "checked, all exact" — the same false reassurance
// Snapshot.Degraded's pointer exists to avoid — and the omission is also what keeps a
// client's "is there a caveat" test a single presence check.
func TestIncomplete_ByReasonIsOmittedWhenEveryFigureIsExact(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New(WithClock(func() time.Time { return now }),
		WithPricing(resolverFor(t, "claude-opus-5", 5.0/1e6, 25.0/1e6)))

	e := pricedRespEvent("gw.internal", "claude-opus-5", 1000, 500)
	e.Inference.PresentKinds = KindInput | KindOutput
	e.Inference.FinishReason = "end_turn"
	a.Record("s1", e)

	snap := snapshotOf(a, now)
	if snap.Totals.PricedRequests != 1 {
		t.Fatalf("PricedRequests = %d, want 1", snap.Totals.PricedRequests)
	}
	if snap.IncompleteBy != nil {
		t.Errorf("IncompleteBy = %v, want nil for a window with nothing inexact", snap.IncompleteBy)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "incompleteBy") {
		t.Errorf("the wire carries incompleteBy over exact traffic: %s", raw)
	}
}
