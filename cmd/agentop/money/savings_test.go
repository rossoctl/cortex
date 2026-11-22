package money

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// Net is saved minus penalty and is not floored.
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
// another: usage-limit units with their API equivalent labelled, unknown apart, and plugin spend
// on its own.
func TestAccountantLines(t *testing.T) {
	c := usage.Counts{
		AvoidedMicros: 10_000, PenaltyMicros: 30_000,
		AvoidedUnits: 4_500, PenaltyUnits: 500, AvoidedAPIEquivalentMicros: 1_000_000,
		AvoidedUnknownMicros: 200_000,
		PluginSpendMicros:    4_000, PluginSpendUnits: 2_000, PluginSpendUnknownMicros: 50_000,
	}
	want := []Line{
		{Label: "saved", Figure: "$0.01   penalty $0.03   net -$0.02"},
		{Label: "usage-limit units", Figure: "4.5 saved   penalty 0.5   net 4.0   API equivalent net $1.00"},
		{Label: "unknown billing", Figure: "$0.20 net, not in any total"},
		{Label: "plugin model calls", Figure: "<$0.01   2.0 units   $0.05 unknown billing"},
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
