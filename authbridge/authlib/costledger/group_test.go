package costledger

import (
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// allGroups is every axis /v1/usage accepts, so a new one cannot be added to usage
// without a test here deciding whether a ledger row can carry it.
var allGroups = []usage.Group{
	usage.GroupNone, usage.GroupModel, usage.GroupMethod, usage.GroupEndpoint,
	usage.GroupAgent, usage.GroupSession, usage.GroupStatus, usage.GroupPlugin,
}

// fullRow is a row with every label a ledger row HAS populated, and a dollar on it.
//
// Fully populated deliberately: a group that produces no label for THIS row produces
// none for any row, because there is no further field to carry one. That is what makes
// Groupable a statement about the SOURCE rather than about one row's contents.
func fullRow() Row {
	return Row{
		Endpoint: "gw.example", Model: "m", Agent: "claude-code/1.0", Provenance: "authoritative",
		Counts: usage.Counts{Requests: 1, Tokens: 100, CostMicros: 1_000_000, PricedRequests: 1, PriceableRequests: 1},
	}
}

// TestGroupable_MatchesWhatLabelForCanActuallyProduce is the drift guard between the
// predicate and the loop it describes.
//
// Groupable exists because usage.Group.Reconcilable answers a question about the RING,
// and the ledger cannot answer for three of the axes the ring can. A predicate that
// disagreed with labelFor would move the defect rather than fix it: a group Groupable
// admits but labelFor cannot label produces the 100% residual again, and one it refuses
// while labelFor CAN label silently drops a real reconciliation.
func TestGroupable_MatchesWhatLabelForCanActuallyProduce(t *testing.T) {
	for _, g := range allGroups {
		_, ok := labelFor(fullRow(), g)
		if got := Groupable(g); got != ok {
			t.Errorf("Groupable(%s) = %v but labelFor on a fully populated row returned ok=%v; "+
				"the predicate and the loop must agree or Fold decides the residual from the "+
				"wrong one", g, got, ok)
		}
	}
}

// TestFold_AnAxisNoLedgerRowCarriesReportsNoResidualAtAll is the defect, priced.
//
// Group.Reconcilable is false only for GroupNone and GroupPlugin, but a ledger row
// carries no session id and no status code, so labelFor returns ok=false for
// group=session and group=status as well — and Fold then added EVERY row's dollars to
// the residual. GET /v1/usage?window=today&group=status answered 200 with series: null
// and ungroupedCostMicros equal to the entire total: a response asserting that none of
// the money it reports can be accounted for, which is exactly the "reads as a fault"
// outcome the GroupNone exclusion exists to prevent, reached through a documented query
// that answers correctly over a ring window.
//
// A residual is a statement about a breakdown that ALMOST accounts for the total. Where
// there is no breakdown at all there is nothing for it to be a residual OF, and the
// honest number is not 100% — it is no number, with the grouping the source could apply
// reported instead. See Groupable and sessionapi's ledgerSnapshot.
func TestFold_AnAxisNoLedgerRowCarriesReportsNoResidualAtAll(t *testing.T) {
	rows := []Row{fullRow(), fullRow()}
	for _, g := range []usage.Group{usage.GroupSession, usage.GroupStatus, usage.GroupPlugin} {
		t.Run(string(g), func(t *testing.T) {
			totals, series, ungrouped := Fold(rows, g)
			if totals.CostMicros != 2_000_000 {
				t.Errorf("totals.CostMicros = %d, want 2000000; the rows count toward the total "+
					"whatever axis was asked for", totals.CostMicros)
			}
			if series != nil {
				t.Errorf("series = %v, want nil: a ledger row carries no value for %s", series, g)
			}
			if ungrouped.Micros != 0 {
				t.Errorf("residual = %d over a total of %d for group=%s — a response disclosing "+
					"100%% of its own spend as unaccounted for; the ledger cannot group by this "+
					"axis at all, which is a different answer from a breakdown that fell short",
					ungrouped.Micros, totals.CostMicros, g)
			}
		})
	}
}

// TestFold_AnAxisTheLedgerCanLabelStillDisclosesItsResidual is the other half, and the
// one that stops the fix from being "never disclose a residual".
//
// group=model over a gateway-priced row with no model is the case
// Snapshot.UngroupedCostMicros exists for: the row counts toward the total, cannot be a
// series key, and the difference has to reach the wire. That must keep working, or the
// arithmetic promise in usage.Snapshot.UngroupedCostMicros breaks for the axis it was
// written about.
func TestFold_AnAxisTheLedgerCanLabelStillDisclosesItsResidual(t *testing.T) {
	priced := fullRow()
	noModel := fullRow()
	noModel.Model = ""
	totals, series, ungrouped := Fold([]Row{priced, noModel}, usage.GroupModel)
	if len(series) != 1 {
		t.Fatalf("series = %v, want one entry keyed by the model that IS present", series)
	}
	if ungrouped.Micros != 1_000_000 {
		t.Errorf("residual = %d, want 1000000 — the modelless row's dollars", ungrouped.Micros)
	}
	if got := series["m"].CostMicros + ungrouped.Micros; got != totals.CostMicros {
		t.Errorf("series + residual = %d, want %d: sum(series) + residual == total is the "+
			"promise usage.Snapshot.UngroupedCostMicros makes", got, totals.CostMicros)
	}
}
