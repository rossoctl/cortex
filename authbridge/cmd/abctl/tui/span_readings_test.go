package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// These pin behaviour that spanReadings carries and that was, until now, only asserted through
// spendSummary's dead half — TestSpendSummary_ANegativeWindowTotalIsUnpricedNotARefund and
// friends read WindowUSD and Priced, which no renderer touches. A test exercising a write-only
// field proves the arithmetic and nothing about the screen.

// A NEGATIVE TOTAL IS REFUSED, not clamped and not drawn.
//
// The aggregator sums non-negative per-request figures, so a negative can only come from a broken
// producer — and "-$5.00" on a spend band reads as a refund nobody issued. Treated as unpriced,
// which is the honest reading: we do not know what this span cost.
func TestSpanReadings_ANegativeTotalIsUnpricedNotARefund(t *testing.T) {
	for span := spendSpan(0); span < numSpendSpans; span++ {
		def := spendSpanDefs[span]
		t.Run(def.label, func(t *testing.T) {
			// EVERY OTHER CHAIN ANSWERS HEALTHILY, so the em-dash assertion below can only be
			// satisfied by the refusal under test. With one chain populated the other three have
			// no snapshot and render em dashes of their own, which made that check pass whatever
			// this guard did — true for a reason unrelated to the property.
			m := &model{}
			for other := spendSpan(0); other < numSpendSpans; other++ {
				if other == span {
					continue
				}
				m.spend.chains[other].snap = &usage.Snapshot{
					Window: spendSpanDefs[other].window,
					Totals: usage.Counts{
						Requests: 10, CostMicros: 4_040_000,
						PricedRequests: 10, PriceableRequests: 10,
					},
					Priced: true,
				}
			}
			m.spend.chains[span].snap = &usage.Snapshot{
				Window: def.window,
				Totals: usage.Counts{
					Requests: 10, CostMicros: -5_000_000,
					PricedRequests: 10, PriceableRequests: 10,
				},
				Priced: true,
			}
			got := m.spanReadings()[span]
			if got.Priced {
				t.Errorf("%s: Priced = true on a negative total, so the band would draw it", def.label)
			}
			if got.USD != 0 {
				t.Errorf("%s: USD = %v carried off a negative total", def.label, got.USD)
			}
			// And on screen it is the em dash, never a minus sign.
			//
			// PROBED AS "$-", which is the shape the formatter actually produces: formatUSDCell
			// puts the sigil first, so a refused figure that slipped through renders "$-5.0000"
			// and the "-$" this used to look for could never appear. The clause was dead —
			// dropping the negative guard left it false and only the em-dash count below caught
			// the regression. spend_drawer_test.go had it right.
			line := strings.Join(renderSpendBand(spendSummary{Spans: m.spanReadings()}, 200), "\n")
			for _, shape := range []string{"$-", "-$", "−$", "$−"} {
				if strings.Contains(line, shape) {
					t.Errorf("%s: band drew %q, a negative figure that reads as a refund:\n%s",
						def.label, shape, line)
				}
			}
			if !strings.Contains(line, emptyCell) {
				t.Errorf("%s: band has no %q for a total it refused, so the impossible figure was "+
					"rendered as a number:\n%s", def.label, emptyCell, line)
			}
			// And exactly one cell is withheld: the refusal is this span's, not the band's.
			if n := strings.Count(line, emptyCell); n != 1 {
				t.Errorf("%s: %d cells carry %q, want 1 — one impossible total must not blank its "+
					"neighbours:\n%s", def.label, n, emptyCell, line)
			}
		})
	}
}

// AND A ZERO TOTAL IS CARRIED, which is the other side of the same guard and the easier one to
// "fix" into a bug.
//
// usage.Snapshot.Priced is PricedRequests > 0 and explicitly NOT CostMicros > 0: a window whose
// every request the gateway SETTLED AT ZERO has priced requests and no dollars, and snapshot.go
// requires it to render as a zero figure rather than as "cost unavailable" — those are different
// answers, and only one of them is true here. Tightening this guard to CostMicros > 0 reads like
// defensive hygiene and silently converts a known answer into "not known here".
//
// AT THE READING, not only at the band: the band takes a spanReading, so a test that builds one by
// hand cannot see this guard at all. That is exactly how the first version of this assertion
// passed against the change it was written to reject.
func TestSpanReadings_ASettledFreeWindowIsPricedAtZero(t *testing.T) {
	for span := spendSpan(0); span < numSpendSpans; span++ {
		def := spendSpanDefs[span]
		t.Run(def.label, func(t *testing.T) {
			m := &model{}
			m.spend.chains[span].snap = &usage.Snapshot{
				Window: def.window,
				Totals: usage.Counts{
					Requests: 10, CostMicros: 0,
					PricedRequests: 10, PriceableRequests: 10,
				},
				Priced: true,
			}
			got := m.spanReadings()[span]
			if !got.Priced {
				t.Errorf("%s: Priced = false for a window the gateway settled at zero, so the "+
					"band reports \"cost unavailable\" for traffic whose cost is known to be "+
					"nothing", def.label)
			}
			if got.USD != 0 {
				t.Errorf("%s: USD = %v, want 0", def.label, got.USD)
			}
			// And on screen it is a figure, not the em dash.
			line := strings.Join(renderSpendBand(spendSummary{Spans: m.spanReadings()}, 200), "\n")
			if !strings.Contains(line, "$0.00") {
				t.Errorf("%s: band drew no $0.00 for settled-free traffic:\n%s", def.label, line)
			}
		})
	}
}

// An unpriced answer is not a zero one, which is the distinction every money surface here rests
// on: "nothing could be priced" and "this cost nothing" are different claims and only one of them
// is ever knowable from an empty figure.
func TestSpanReadings_NothingPricedIsNotZero(t *testing.T) {
	m := &model{}
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10},
		Priced: false,
	}
	got := m.spanReadings()[spanToday]
	if got.Priced {
		t.Error("Priced = true on an unpriced answer")
	}
	if got.Unpriced != 10 || got.Priceable != 10 {
		t.Errorf("coverage gap = %d of %d, want 10 of 10", got.Unpriced, got.Priceable)
	}
	line := strings.Join(renderSpendBand(spendSummary{Spans: m.spanReadings()}, 200), "\n")
	if strings.Contains(line, "$0.00") {
		t.Errorf("band rendered $0.00 for an unpriced span, asserting the traffic was free:\n%s", line)
	}
}

// The coverage gap is PRICEABLE minus priced, never Requests minus priced.
//
// Requests counts every proxied response — MCP tool calls, health checks, tunnels — none of which
// can carry a price, so that denominator makes a correctly configured deployment report itself
// permanently incomplete.
func TestSpanReadings_CoverageGapUsesThePriceableDenominator(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 306, PriceableRequests: 318,
		},
		Priced: true,
	}
	got := m.spanReadings()[spanHour]
	if got.Unpriced != 12 {
		t.Errorf("Unpriced = %d, want 12 (318 priceable - 306 priced)", got.Unpriced)
	}
	if got.Priceable != 318 {
		t.Errorf("Priceable = %d, want 318", got.Priceable)
	}
	// A fully priced answer reports no gap rather than a zero one.
	m.spend.chains[spanHour].snap.Totals.PricedRequests = 318
	if got := m.spanReadings()[spanHour]; got.Unpriced != 0 {
		t.Errorf("Unpriced = %d on a fully priced answer, want 0", got.Unpriced)
	}
}

// A failed poll and a snapshot that has not arrived are DIFFERENT STATES, and neither is zero.
// They render alike — there is no room in a seven-column cell to say which — but the reading has
// to keep them apart, because only one of them is worth an operator's attention.
func TestSpanReadings_FailureAndEmptinessAreDistinct(t *testing.T) {
	m := &model{}
	m.spend.chains[spanHour].err = errors.New("dial tcp: connection refused")
	got := m.spanReadings()

	if !got[spanHour].Failed {
		t.Error("Failed = false on a chain whose poll errored")
	}
	if got[spanToday].Failed {
		t.Error("Failed = true on a chain that has simply not answered yet")
	}
	for _, span := range []spendSpan{spanHour, spanToday} {
		if got[span].Priced {
			t.Errorf("%s: Priced = true with no answer", spendSpanDefs[span].label)
		}
	}
}

// THE TWO DISCLOSURES THAT TRAVEL FROM THE LEDGER REACH THE CELL, asserted through the live path.
//
// Deleting both `r.DaysOutsideRetention = snap.DaysOutsideRetention` and `r.Degraded =
// snap.Degraded` from spanReadings left the whole package green: no fixture fed either field
// through spanReadings into renderSpendBand, so the retention-coverage marker — the disclosure this
// branch argues for at length — was carried by nothing but its own assignment. The one Degraded
// fixture that existed sat on the hour-and-day path that the four spans replaced.
//
// The two are DIFFERENT CLAIMS and wear different glyphs, which is why both are here:
//
//	DaysOutsideRetention  a floor      the window asked for days the ledger cannot reach
//	Degraded              short by an unstatable amount, from a damaged read
//
// A month against a ten-day ledger is the ordinary case for the first, and it is exactly the case
// where a clean-looking total is most misleading.
func TestSpanReadings_CarryTheLedgersDisclosuresToTheCell(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*usage.Snapshot)
		want   string
	}{
		{"days outside retention", func(s *usage.Snapshot) { s.DaysOutsideRetention = 21 }, partialMarker},
		{"a damaged read", func(s *usage.Snapshot) {
			s.Degraded = &usage.Degraded{UnreadableDays: 1}
		}, damagedMarker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{}
			snap := &usage.Snapshot{
				Window: spendSpanDefs[spanMonth].window,
				Totals: usage.Counts{
					Requests: 400, CostMicros: 703_180_000,
					PricedRequests: 400, PriceableRequests: 400,
				},
				Priced: true,
			}
			tc.mutate(snap)
			m.spend.chains[spanMonth].snap = snap

			got := m.spanReadings()[spanMonth]
			if !got.Priced {
				t.Fatalf("the reading is unpriced, so the marker has no figure to ride on")
			}
			line := renderSpendBand(spendSummary{Spans: m.spanReadings()}, 200)[1]
			if !strings.Contains(line, tc.want) {
				t.Errorf("the month's cell carries no %q for %s, so the total is published as "+
					"though it were whole:\n%s", tc.want, tc.name, line)
			}
			// The figure is still shown — this qualifies the number, it does not withhold it.
			if !strings.Contains(line, "$703.18") {
				t.Errorf("the disclosure took the figure with it:\n%s", line)
			}
		})
	}
}

// A FAILED CHAIN MUST NOT BLANK THE OTHERS, which is the whole reason the chains were split and
// the whole reason spendSummary stopped being a wrapper.
//
// The derivation this replaced returned early on a failed hour poll, so the other three spans
// were never read; a wrapper existed purely to fill them outside it. spanReadings walks every
// chain unconditionally.
//
// WITH REAL MODEL STATE, which is the gap this closes. The comment on spendSummary claimed the
// bug was fixed and nothing asserted it: reintroducing the early return left the suite green,
// because TestSpanReadings_FailureAndEmptinessAreDistinct only asserts a LATER span is not
// Failed — which a zero-value entry satisfies — and the band's OneChainsFailureLeavesTheOthers
// builds its summary by hand and never calls spanReadings at all. The one test that pinned it
// through the model was deleted with the dead path.
func TestSpanReadings_AFailedChainDoesNotBlankTheOthers(t *testing.T) {
	m := &model{}
	// The FIRST chain fails, which is the one an early return would exit on.
	m.spend.chains[spanHour].err = errors.New("dial tcp: connection refused")
	// Every later span answered, and with distinct figures so a reading cannot pass by
	// accidentally holding its neighbour's.
	for span, usd := range map[spendSpan]int64{
		spanToday: 18_800_000, span7d: 216_440_000, spanMonth: 703_180_000,
	} {
		def := spendSpanDefs[span]
		m.spend.chains[span].snap = &usage.Snapshot{
			Window: def.window,
			Totals: usage.Counts{Requests: 10, CostMicros: usd, PricedRequests: 10, PriceableRequests: 10},
			Priced: true,
		}
	}

	got := m.spanReadings()

	if !got[spanHour].Failed {
		t.Error("the failed chain is not marked Failed")
	}
	for span, want := range map[spendSpan]float64{
		spanToday: 18.8, span7d: 216.44, spanMonth: 703.18,
	} {
		r := got[span]
		if !r.Priced {
			t.Errorf("%s: Priced = false — the hour's failure took this span's answer with it, "+
				"which is the early return spanReadings exists to have removed",
				spendSpanDefs[span].label)
		}
		if r.USD != want {
			t.Errorf("%s: USD = %v, want %v", spendSpanDefs[span].label, r.USD, want)
		}
	}
	// And on screen: three figures beside one em dash, not four em dashes.
	line := strings.Join(renderSpendBand(m.spendSummary(), 200), "\n")
	for _, want := range []string{"$18.80", "$216.44", "$703.18"} {
		if !strings.Contains(line, want) {
			t.Errorf("band lost %s over the hour's failure:\n%s", want, line)
		}
	}
}

// The inexact count reaches the cell. Re-pinned per span: the deleted
// TestSpendSummary_CarriesTheInexactCountFromBothSnapshots asserted the summary-level field, so
// it never covered this propagation either — forcing r.Incomplete = 0 survived the whole suite.
func TestSpanReadings_CarryTheInexactCount(t *testing.T) {
	for span := spendSpan(0); span < numSpendSpans; span++ {
		def := spendSpanDefs[span]
		t.Run(def.label, func(t *testing.T) {
			m := &model{}
			m.spend.chains[span].snap = &usage.Snapshot{
				Window: def.window,
				Totals: usage.Counts{
					Requests: 10, CostMicros: 4_040_000,
					PricedRequests: 10, PriceableRequests: 10,
					IncompleteRequests: 7,
				},
				Priced: true,
			}
			if got := m.spanReadings()[span]; got.Incomplete != 7 {
				t.Errorf("%s: Incomplete = %d, want 7", def.label, got.Incomplete)
			}
			// And it earns the marker that says the figure is a lower bound.
			line := strings.Join(renderSpendBand(m.spendSummary(), 200), "\n")
			if !strings.Contains(line, inexactMarker) {
				t.Errorf("%s: a figure with 7 inexact requests drew no %q marker:\n%s",
					def.label, inexactMarker, line)
			}
		})
	}
}

// A CHAIN THAT HAS NEVER ANSWERED IS NOT STALE — it is empty, which the band says differently.
//
// Re-pinned for the same reason: the deleted TestSpendSummary_NoFetchYetIsNotStale asserted the
// summary field, so replacing the `!c.lastFetch.IsZero()` guard with `true` survived the suite.
// Without the guard a zero lastFetch makes the age the time since the epoch.
func TestSpanReadings_AChainThatNeverAnsweredIsNotStale(t *testing.T) {
	m := &model{}
	// A snapshot with no lastFetch: the state between a fetch being issued and its reply.
	m.spend.chains[spanToday].snap = &usage.Snapshot{
		Window: usage.WindowToday,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}
	got := m.spanReadings()
	for span := spendSpan(0); span < numSpendSpans; span++ {
		r := got[span]
		if r.Stale {
			t.Errorf("%s: Stale = true with a zero lastFetch — the age would be measured from "+
				"the epoch", spendSpanDefs[span].label)
		}
		if r.Age != 0 {
			t.Errorf("%s: Age = %v with a zero lastFetch", spendSpanDefs[span].label, r.Age)
		}
	}
	// And no cell carries a timestamp, which is what a reader would see.
	line := strings.Join(renderSpendBand(spendSummary{Spans: got}, 200), "\n")
	for _, span := range []spendSpan{spanHour, spanToday, span7d, spanMonth} {
		if strings.Contains(line, spendSpanDefs[span].label+" ") &&
			!strings.Contains(line, spendSpanDefs[span].label+"  ") {
			t.Errorf("%s appears with a suffix on a band that has never polled:\n%s",
				spendSpanDefs[span].label, line)
		}
	}
}
