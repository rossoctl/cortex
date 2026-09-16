package sessionapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// TestHandleUsage_ALedgerWindowSaysWhichGroupingItCouldApply is the client-visible half
// of the residual defect.
//
// GET /v1/usage?window=today&group=status is served from the cost ledger, whose rows
// carry no status code. It used to answer 200 with group:"status", series:null and
// ungroupedCostMicros equal to the whole of totals.costMicros — a response disclosing
// 100% of the money in it as unaccounted for, which is the "reads as a fault" outcome
// usage.Group.Reconcilable refuses for group=none, reached through a documented query
// that answers correctly over a duration window.
//
// THREE STATES HAVE TO BE DISTINGUISHABLE FROM THE RESPONSE ALONE, and this pins all
// three against the same one dollar of spend:
//
//	group asked   group served   ungroupedCostMicros   means
//	status        none           absent                this source cannot group by that
//	model         model          absent                the breakdown accounts for everything
//	model         model          present               the breakdown falls short by that much
//
// The third is pinned by TestLedgerSnapshot_UngroupedCostReachesTheWire; this test
// covers the first two, because they were the pair that had become the same response.
func TestHandleUsage_ALedgerWindowSaysWhichGroupingItCouldApply(t *testing.T) {
	at := insideToday(t, 3*time.Hour)
	for _, tc := range []struct {
		asked  string
		served usage.Group
		why    string
	}{
		{asked: "status", served: usage.GroupNone, why: "a ledger row carries no status code"},
		{asked: "session", served: usage.GroupNone, why: "a ledger row carries no session id"},
		{asked: "plugin", served: usage.GroupNone, why: "a ledger row carries no plugin list"},
		{asked: "model", served: usage.GroupModel, why: "model is a column on every row"},
		{asked: "endpoint", served: usage.GroupEndpoint, why: "endpoint is a column on every row"},
		{asked: "agent", served: usage.GroupAgent, why: "agent is a column on every row"},
	} {
		t.Run("group="+tc.asked, func(t *testing.T) {
			led := ledgerWithOneCostedMinute(t, at, "gw.example", "m", 1.0)
			ts, _ := newTestServer(t, WithUsage(usage.New()), WithCostLedger(led))

			status, body := fetchUsage(t, ts.URL, "?window=today&group="+tc.asked)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", status, body)
			}
			var snap usage.Snapshot
			if err := json.Unmarshal([]byte(body), &snap); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if snap.Group != tc.served {
				t.Errorf("group = %q, want %q (%s); the response has to say which grouping it "+
					"applied, or a client cannot tell an unsupported axis from an idle window",
					snap.Group, tc.served, tc.why)
			}
			if snap.Totals.CostMicros != 1_000_000 {
				t.Errorf("totals.costMicros = %d, want 1000000; the dollars stand whatever axis "+
					"was asked for", snap.Totals.CostMicros)
			}
			if snap.UngroupedCostMicros != nil {
				t.Errorf("ungroupedCostMicros = %d over a total of %d for group=%s: every dollar "+
					"here is attributable, so a residual of any size is a claim the rows do not "+
					"support — and one equal to the total reads as a fault",
					*snap.UngroupedCostMicros, snap.Totals.CostMicros, tc.asked)
			}
			// The raw body too, because a nil pointer and an absent field are the same thing
			// in Go and only one of them is the wire contract.
			if strings.Contains(body, "ungroupedCostMicros") {
				t.Errorf("body carries an ungroupedCostMicros field: %s", body)
			}
		})
	}
}
