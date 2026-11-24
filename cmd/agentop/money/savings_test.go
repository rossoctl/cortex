package money

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// Net is saved minus penalty (and keep-alive spend) and is not floored.
func TestNetMicros_CanBeNegative(t *testing.T) {
	if got := NetMicros(usage.Counts{AvoidedMicros: 10_000, PenaltyMicros: 30_000}); got != -20_000 {
		t.Errorf("NetMicros = %d, want -20000", got)
	}
}

func TestSigned(t *testing.T) {
	for _, tc := range []struct {
		micros int64
		unit   string
		want   string
	}{{120_000, "", "$0.12"}, {-120_000, "", "-$0.12"}, {-1_000, "", "-<$0.01"}, {-2_500_000, "credits", "-2.50 credits"}} {
		if got := Signed(tc.micros, tc.unit); got != tc.want {
			t.Errorf("Signed(%d, %q) = %q, want %q", tc.micros, tc.unit, got, tc.want)
		}
	}
}

// Every accountant figure beside the money totals gets its own line, and none is added into
// another: usage-limit units with their API equivalent labelled, unknown apart, plugin spend and
// keep-alive spend each on their own.
func TestAccountantLines(t *testing.T) {
	c := usage.Counts{
		AvoidedMicros: 10_000, PenaltyMicros: 30_000,
		AvoidedUnits: 4_500, PenaltyUnits: 500, AvoidedAPIEquivalentMicros: 1_000_000,
		AvoidedUnknownMicros: 200_000,
		PluginSpendMicros:    4_000, PluginSpendUnits: 2_000, PluginSpendUnknownMicros: 50_000,
		KeepAliveMicros: 20_000, KeepAlivePings: 3,
	}
	want := []Line{
		// Net of the keep-alive spend too: 0.01 - 0.03 - 0.02.
		{Label: "saved", Figure: "$0.01   penalty $0.03   keep-alive $0.02   net -$0.04"},
		{Label: "usage-limit units", Figure: "4.5 saved   penalty 0.5   net 4.0   API equivalent net $1.00"},
		{Label: "unknown billing", Figure: "$0.20 net, not in any total"},
		{Label: "plugin model calls", Figure: "<$0.01   2.0 units   $0.05 unknown billing"},
		{Label: "keep-alive", Figure: "$0.02   3 pings"},
	}
	if got := AccountantLines(c, ""); !reflect.DeepEqual(got, want) {
		t.Errorf("AccountantLines =\n%+v\nwant\n%+v", got, want)
	}
	// Every figure in the window's unit, the API equivalent and the unknown ones included.
	for _, l := range AccountantLines(c, "credits") {
		if strings.Contains(l.Figure, "$") {
			t.Errorf("%s = %q in a credits window carries a $", l.Label, l.Figure)
		}
	}
	if got := AccountantLines(usage.Counts{CostMicros: 5}, ""); got != nil {
		t.Errorf("no accountant figure, lines = %+v", got)
	}
}

// The keeper's rescue is in AvoidedMicros, so its pings' cost comes off the net: a $0.10 rescue
// that took $0.04 of pings saved $0.06.
func TestNetMicros_SubtractsKeepAliveSpend(t *testing.T) {
	c := usage.Counts{AvoidedMicros: 100_000, PenaltyMicros: 0, KeepAliveMicros: 40_000}
	if got := NetMicros(c); got != 60_000 {
		t.Errorf("NetMicros = %d, want 60000", got)
	}
	// Subscription and unknown-billing pings are not money and leave the usd net alone.
	c = usage.Counts{AvoidedMicros: 100_000, KeepAliveAPIEquivalentMicros: 40_000, KeepAliveUnknownMicros: 9}
	if got := NetMicros(c); got != 100_000 {
		t.Errorf("NetMicros = %d, want 100000", got)
	}
}

// Spend is the requests plus the pings on an API key; the pings that are not money stay out.
func TestSpendMicros(t *testing.T) {
	c := usage.Counts{CostMicros: 1_000_000, KeepAliveMicros: 25_000, KeepAliveAPIEquivalentMicros: 7,
		KeepAliveUnknownMicros: 8, PluginSpendMicros: 9}
	if got := SpendMicros(c); got != 1_025_000 {
		t.Errorf("SpendMicros = %d, want 1025000", got)
	}
}

// The subscription line is net of the subscription pings, in units and in API-equivalent dollars,
// and the unknown-billing line of the unknown-billing pings.
func TestAccountantLines_NetsKeepAliveByBasis(t *testing.T) {
	c := usage.Counts{
		AvoidedUnits: 4_500, PenaltyUnits: 500, AvoidedAPIEquivalentMicros: 1_000_000,
		AvoidedUnknownMicros: 200_000,
		KeepAliveUnits:       1_000, KeepAliveAPIEquivalentMicros: 300_000, KeepAliveUnknownMicros: 50_000, KeepAlivePings: 2,
	}
	want := []Line{
		{Label: "usage-limit units", Figure: "4.5 saved   penalty 0.5   keep-alive 1.0   net 3.0   API equivalent net $0.70"},
		{Label: "unknown billing", Figure: "$0.15 net, not in any total"},
		{Label: "keep-alive", Figure: "1.0 units (API equivalent $0.30)   $0.05 unknown billing   2 pings"},
	}
	if got := AccountantLines(c, ""); !reflect.DeepEqual(got, want) {
		t.Errorf("AccountantLines =\n%+v\nwant\n%+v", got, want)
	}
}

// Keep-alive spend is split like plugin spend: dollars only for pings on an API key, a
// subscription's pings in usage-limit units with their API equivalent labelled, unknown billing
// apart, and the count of every ping.
func TestAccountantLines_KeepAliveByBasis(t *testing.T) {
	c := usage.Counts{
		KeepAliveMicros: 20_000, KeepAliveUnits: 186_000, KeepAliveAPIEquivalentMicros: 560_000,
		KeepAliveUnknownMicros: 30_000, KeepAlivePings: 5,
	}
	want := []Line{{Label: "keep-alive",
		Figure: "$0.02   186.0 units (API equivalent $0.56)   $0.03 unknown billing   5 pings"}}
	if got := AccountantLines(c, ""); !reflect.DeepEqual(got, want) {
		t.Errorf("AccountantLines =\n%+v\nwant\n%+v", got, want)
	}
	// Only subscription pings: no dollar figure is shown as money.
	sub := usage.Counts{KeepAliveUnits: 1_500, KeepAliveAPIEquivalentMicros: 4_000, KeepAlivePings: 1}
	want = []Line{{Label: "keep-alive", Figure: "1.5 units (API equivalent <$0.01)   1 ping"}}
	if got := AccountantLines(sub, ""); !reflect.DeepEqual(got, want) {
		t.Errorf("AccountantLines =\n%+v\nwant\n%+v", got, want)
	}
	// A subscription ping whose units round to zero still shows its API equivalent.
	tiny := usage.Counts{KeepAliveAPIEquivalentMicros: 4_000, KeepAlivePings: 1}
	got := AccountantLines(tiny, "")
	if len(got) != 1 || !strings.Contains(got[0].Figure, "API equivalent") {
		t.Errorf("AccountantLines = %+v, want the API equivalent shown", got)
	}
}

// A priced request or an API-key ping is a spend figure; subscription and unknown-billing pings
// alone are not money.
func TestHasSpend(t *testing.T) {
	for _, tc := range []struct {
		c    usage.Counts
		want bool
	}{
		{usage.Counts{}, false},
		{usage.Counts{PricedRequests: 1}, true},
		{usage.Counts{KeepAliveMicros: 1}, true},
		{usage.Counts{KeepAliveUnits: 5, KeepAliveAPIEquivalentMicros: 9, KeepAliveUnknownMicros: 9}, false},
	} {
		if got := HasSpend(tc.c); got != tc.want {
			t.Errorf("HasSpend(%+v) = %v, want %v", tc.c, got, tc.want)
		}
		if got := SpendPriced(&usage.Snapshot{Totals: tc.c, Priced: tc.c.PricedRequests > 0}); got != tc.want {
			t.Errorf("SpendPriced(%+v) = %v, want %v", tc.c, got, tc.want)
		}
	}
	if SpendPriced(nil) {
		t.Error("SpendPriced(nil) = true")
	}
}
