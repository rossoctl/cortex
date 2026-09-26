package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/usage"
)

// The cent arithmetic, at the boundaries that made #1042 write it out by hand rather than reach
// for %.2f.
func TestFormatUSDTotal_RoundsHalfUpOnTheInteger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		micros int64
		want   string
	}{
		// THE CASE %.2f GETS WRONG. 1_005_000 micros is exactly $1.005, the nearest float64 is
		// 1.00499999…, and %.2f on it prints $1.00. Half-up on the integer gets $1.01.
		{"an exact half rounds up", 1_005_000, "$1.01"},
		{"just under a half rounds down", 1_004_999, "$1.00"},
		{"a whole cent is exact", 1_010_000, "$1.01"},
		{"zero is zero, not a floor", 0, "$0.00"},
		// Positive but under half a cent: never "$0.00", which would claim the traffic was free.
		{"half a cent is the floor", 4_999, "<$0.01"},
		{"one micro is the floor", 1, "<$0.01"},
		{"exactly half a cent rounds up instead", 5_000, "$0.01"},
		{"dollars carry", 159_548_300, "$159.55"},
		// NEGATIVES, which the table did not reach and the float path cannot: MicrosFromUSD
		// rejects a negative before the arithmetic sees it, so formatUSDTotal(-5) tested the
		// guard and not this. Bare, the integer arithmetic truncates toward zero and produced
		// "$0.-15" and "$-12.-34" — and anything above -5_000 rounded to "$0.00", claiming
		// traffic was free. Four decimals here, matching what formatUSDTotal answers, so the
		// two entry points cannot disagree about the same input.
		{"a small negative is not rounded to free", -1, "$-0.0000"},
		{"a negative under half a cent is not free either", -4_999, "$-0.0050"},
		{"a negative does not truncate into $0.-15", -150_000, "$-0.1500"},
		{"a negative dollar figure keeps one sign", -12_345_678, "$-12.3457"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatUSDTotalMicros(tc.micros); got != tc.want {
				t.Errorf("formatUSDTotalMicros(%d) = %q, want %q", tc.micros, got, tc.want)
			}
			// The float64 entry point must agree, which is the whole reason it converts through
			// MicrosFromUSD rather than multiplying by 100: that conversion rounds, so it
			// recovers the integer the float was derived from.
			if got := formatUSDTotal(float64(tc.micros) / 1e6); got != tc.want {
				t.Errorf("formatUSDTotal(%v) = %q, want %q", float64(tc.micros)/1e6, got, tc.want)
			}
		})
	}
}

// An unrepresentable figure is not this formatter's to name. Every surface showing a total has
// its own word for one — "unavailable", a clamp marker — and a third spelling here would hide
// theirs, so it falls through to the four-decimal form and shows whatever the figure is.
func TestFormatUSDTotal_LeavesTheImpossibleToItsCaller(t *testing.T) {
	if got := formatUSDTotal(-5); !strings.Contains(got, "-5") {
		t.Errorf("formatUSDTotal(-5) = %q; a negative must not be rounded into a cent form", got)
	}
}

// THE BOUNDARY ITSELF, because this is the regression the rule is for. Both precisions still come
// out of one marking path, so a change to either formatter can move a surface that did not ask to
// be moved — which is how the drawer's PER-MODEL column got rounded once already, silently,
// since the drawer's own tests assert whole rendered rows and a narrower figure matches most of
// them.
//
// WHERE THE LINE NOW SITS: cents wherever a reader scans and compares, four decimals for ONE
// REQUEST and only there. It used to sit at "span versus one thing", which put the sessions table
// and this drawer on the four-decimal side; both have since moved, and the carve-out is what is
// left. See the rule beside formatUSDTotal.
func TestPrecisionRule_CentsWhereScannedFourDecimalsForOneRequest(t *testing.T) {
	// One figure, both sides of the boundary: $1.0601, which is what the model column showed
	// when this was found.
	const usd = 1.0601

	total := moneyTotal(usd, 0, 0, 0, nil, false)
	if total != "$1.06" {
		t.Errorf("a span total rendered %q, want cents", total)
	}

	// The four-decimal path still exists and still formats four decimals — it is what the events
	// table's per-request COST reads through. Asserted so the carve-out cannot quietly vanish:
	// with every other surface moved to cents, nothing else would notice if it did.
	item := moneyAmount(usd, 0, 0, 0, nil, false)
	if item != "$1.0601" {
		t.Errorf("a per-request figure rendered %q, want four decimals", item)
	}

	// And through the drawer's own row builder, the live caller that moved. It reads in cents now,
	// so the assertion is inverted from what it was: this is the one line in the file that says
	// which side the drawer is on.
	figs := drawerFigures(drawerRow{
		label:  "claude-opus-5",
		counts: usage.Counts{CostMicros: 1_060_100, PricedRequests: 11, PriceableRequests: 11},
	})
	var joined string
	for _, f := range figs {
		joined += f.full + " "
	}
	if !strings.Contains(joined, "$1.06") {
		t.Errorf("the drawer's model row is not in cents: %q", joined)
	}
	if strings.Contains(joined, "$1.0601") {
		t.Errorf("the drawer's model row kept four decimals: %q", joined)
	}
}

// The sessions table reads in cents too, through its own ladder rather than through moneyTotal.
//
// A SEPARATE ASSERTION because it is a separate code path: sessionMoneyCell formats from micros
// directly, so nothing above would catch it drifting back to four decimals. The half-cent case is
// the one that proves it goes through formatUSDTotalMicros rather than a "%.2f" of its own —
// 1_005_000 micros is exactly $1.005 and the float form prints "$1.00".
func TestPrecisionRule_TheSessionsTableReadsInCents(t *testing.T) {
	for _, tc := range []struct {
		micros int64
		want   string
	}{
		{1_060_100, "$1.06"},
		{1_005_000, "$1.01"},
		{20, "<$0.01"},
	} {
		if got := sessionMoneyCell(tc.micros, false, sessionsMoneyWidth); got != tc.want {
			t.Errorf("sessionMoneyCell(%d) = %q, want %q", tc.micros, got, tc.want)
		}
	}
}

// The markers ride on either precision, since they are a claim about the figure rather than a
// part of it. Splitting formatting out of moneyAmount must not have dropped them.
func TestMarkMoney_WrapsEitherPrecision(t *testing.T) {
	deg := &usage.Degraded{}
	if got := moneyTotal(1.0601, 1, 4, 1, deg, true); got != damagedMarker+inexactMarker+"$1.06"+partialMarker {
		t.Errorf("a marked span total = %q", got)
	}
	if got := moneyAmount(1.0601, 1, 4, 1, deg, true); got != damagedMarker+inexactMarker+"$1.0601"+partialMarker {
		t.Errorf("a marked per-request figure = %q", got)
	}
}
