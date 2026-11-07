package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/money"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/session"
)

// moneySweep is a spread of figures across every rung of the money ladders: sub-cent, cents,
// dollars, hundreds, thousands and a million, so a relabelling that only handled one magnitude
// fails somewhere.
var moneySweep = []int64{1, 4_999, 30_000, 7_800_000, 123_450_000, 9_876_000_000, 1_234_567_000_000}

// A session with no Currencies — every dollars-only deployment, and every older server — renders
// exactly what it rendered before units existed, at every width the pane uses.
func TestSessionMoneyCellIn_DollarsAreByteIdentical(t *testing.T) {
	for _, micros := range moneySweep {
		for _, budget := range []int{6, 7, 8, 10, 14} {
			for _, sat := range []bool{false, true} {
				want := sessionMoneyCell(micros, sat, budget)
				for _, cur := range [][]string{nil, {"USD"}} {
					if got := sessionMoneyCellIn(micros, sat, budget, cur); got != want {
						t.Errorf("micros %d budget %d sat %v cur %v: got %q, want %q",
							micros, budget, sat, cur, got, want)
					}
				}
			}
		}
	}
}

// A session priced in one foreign unit shows its figure in that unit, never with "$", and never
// wider than the column.
func TestSessionMoneyCellIn_AForeignUnitNeverWearsADollarSign(t *testing.T) {
	for _, micros := range moneySweep {
		for _, budget := range []int{8, 10, 14} {
			for _, sat := range []bool{false, true} {
				got := sessionMoneyCellIn(micros, sat, budget, []string{"Bobcoins"})
				if strings.Contains(got, "$") {
					t.Errorf("micros %d budget %d: %q is a Bobcoins figure printed as dollars", micros, budget, got)
				}
				if w := lipgloss.Width(got); w > budget {
					t.Errorf("micros %d budget %d: %q is %d wide", micros, budget, got, w)
				}
				if got == emptyCell {
					t.Errorf("micros %d budget %d: a real charge rendered as no figure", micros, budget)
				}
			}
		}
	}
	if got := sessionMoneyCellIn(30_000, false, 14, []string{"Bobcoins"}); got != "0.03 Bobcoins" {
		t.Errorf("wide cell = %q, want 0.03 Bobcoins", got)
	}
}

// A session whose figure spans two units has no amount to show.
func TestSessionMoneyCellIn_AMixedSessionShowsNoAmount(t *testing.T) {
	if got := sessionMoneyCellIn(7_800_000, false, 10, []string{"Bobcoins", "USD"}); got != "(mixed)" {
		t.Errorf("mixed session = %q, want (mixed)", got)
	}
}

// An agent's COST cell is in its own units: dollars as before, Bob's credits labelled as credits,
// and an agent that itself spans units withheld.
func TestAgentCostCellIn_UsesTheAgentsOwnUnits(t *testing.T) {
	c := usage.Counts{PricedRequests: 2, CostMicros: 7_800}
	if got, want := agentCostCellIn(c, nil, agentsCostWidth), agentCostCell(c); got != want {
		t.Errorf("dollars = %q, want %q unchanged", got, want)
	}
	if got := agentCostCellIn(c, []string{"Bobcoins"}, agentsCostWidth); got != "0.01 Bobcoi…" {
		t.Errorf("Bobcoins = %q, want 0.01 Bobcoi…", got)
	}
	if got := agentCostCellIn(c, []string{"Bobcoins", "USD"}, agentsCostWidth); got != "(mixed)" {
		t.Errorf("mixed agent = %q, want (mixed)", got)
	}
	if got := agentCostCellIn(usage.Counts{}, []string{"Bobcoins"}, agentsCostWidth); got != emptyCell {
		t.Errorf("unpriced = %q, want the empty cell whatever the unit", got)
	}
}

// Rows take their units from the server's per-agent cross-tabulation, so Claude Code's dollars are
// not withheld because Bob billed in credits in the same window.
func TestAgentRowsFromSnapshot_LabelsEachAgentWithItsOwnUnits(t *testing.T) {
	snap := &usage.Snapshot{
		Currencies: []string{"Bobcoins", "USD"},
		SeriesCurrencies: map[string][]string{
			"bob-shell/2.0.5": {"Bobcoins"}, "claude-code/2.1.284": {"USD"},
		},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"bob-shell/2.0.5":     {Requests: 3, PricedRequests: 3, CostMicros: 7_800},
			"claude-code/2.1.284": {Requests: 9, PricedRequests: 9, CostMicros: 6_200_000},
		}}},
	}
	for _, r := range agentRowsFromSnapshot(snap) {
		cell := agentCostCellIn(r.Counts, r.units, agentsCostWidth)
		switch r.label {
		case "claude-code/2.1.284":
			if cell != "$6.20" {
				t.Errorf("claude-code = %q, want $6.20", cell)
			}
		case "bob-shell/2.0.5":
			if strings.Contains(cell, "$") || cell == money.Mixed {
				t.Errorf("bob-shell = %q, want a Bobcoins figure", cell)
			}
		}
	}
	// An older server sends no cross-tabulation: a mixed window withholds every row.
	snap.SeriesCurrencies = nil
	for _, r := range agentRowsFromSnapshot(snap) {
		if cell := agentCostCellIn(r.Counts, r.units, agentsCostWidth); cell != money.Mixed {
			t.Errorf("%s from an older server = %q, want (mixed)", r.label, cell)
		}
	}
}

// The band's dollars are what markMoneyTotal printed before units existed, caveats and all.
func TestBandValue_DollarsAreMarkMoneyTotal(t *testing.T) {
	for _, micros := range moneySweep {
		r := spanReading{USD: float64(micros) / 1e6, Priced: true, Unpriced: 1, Priceable: 3, Incomplete: 1}
		want := markMoneyTotal(r.USD, r.Unpriced, r.Priceable, r.Incomplete, r.Degraded, r.Clamped, false)
		for _, units := range [][]string{nil, {"USD"}} {
			r.Units = units
			if got := bandValue(r); got != want {
				t.Errorf("micros %d units %v: %q, want %q", micros, units, got, want)
			}
		}
	}
}

// A mixed span prints each unit's figure, dollars first, and never their sum; without a split to
// print it says it is mixed.
func TestBandValue_AMixedSpanShowsEachUnitAndNoSum(t *testing.T) {
	r := spanReading{USD: 6.2078, Priced: true, Units: []string{"Bobcoins", "USD"},
		ByUnit: map[string]int64{"Bobcoins": 7_800, "USD": 6_200_000}}
	if got := bandValue(r); got != "$6.20 + 0.01 Bobcoins" {
		t.Errorf("mixed span = %q, want $6.20 + 0.01 Bobcoins", got)
	}
	r.ByUnit = nil
	if got := bandValue(r); got != money.Mixed {
		t.Errorf("mixed span without a split = %q, want %s", got, money.Mixed)
	}
	one := spanReading{USD: 0.0078, Priced: true, Units: []string{"Bobcoins"}}
	if got := bandValue(one); got != "0.01 Bobcoins" {
		t.Errorf("Bobcoins-only span = %q, want 0.01 Bobcoins", got)
	}
}

// The split is asked for only when the poll names two or more units, and a downgraded or
// incomplete split is not used.
func TestFetchSpendSpan_AsksForAUnitSplitOnlyForAMixedWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		poll       string
		split      string
		wantSplits int
		wantByUnit map[string]int64
	}{
		{"dollars only", `{"window":"today","currencies":["USD"],"buckets":[],"totals":{}}`, "", 0, nil},
		{"older server", `{"window":"today","buckets":[],"totals":{}}`, "", 0, nil},
		{"mixed", `{"window":"today","currencies":["Bobcoins","USD"],"buckets":[],"totals":{}}`,
			`{"window":"today","group":"currency","currencies":["Bobcoins","USD"],"buckets":[{"series":{` +
				`"Bobcoins":{"costMicros":7800,"pricedRequests":1},"USD":{"costMicros":6200000,"pricedRequests":1}}}],"totals":{}}`,
			1, map[string]int64{"Bobcoins": 7_800, "USD": 6_200_000}},
		{"downgraded split", `{"window":"today","currencies":["Bobcoins","USD"],"buckets":[],"totals":{}}`,
			// Currencies present and the grouping downgraded: read as a split, this would print a
			// zero figure for every unit.
			`{"window":"today","group":"none","currencies":["Bobcoins","USD"],"buckets":[],"totals":{}}`, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			splits := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("group") == string(usage.GroupCurrency) {
					splits++
					_, _ = w.Write([]byte(tc.split))
					return
				}
				_, _ = w.Write([]byte(tc.poll))
			}))
			defer ts.Close()
			m := &model{client: apiclient.New(ts.URL)}
			msg := m.fetchSpendSpan(spanToday)().(spendLoadedMsg)
			if splits != tc.wantSplits {
				t.Errorf("split requests = %d, want %d", splits, tc.wantSplits)
			}
			if (msg.byUnit == nil) != (tc.wantByUnit == nil) || len(msg.byUnit) != len(tc.wantByUnit) {
				t.Fatalf("byUnit = %v, want %v", msg.byUnit, tc.wantByUnit)
			}
			for u, v := range tc.wantByUnit {
				if msg.byUnit[u] != v {
					t.Errorf("byUnit[%s] = %d, want %d", u, msg.byUnit[u], v)
				}
			}
		})
	}
}

// A dollars-only drawer renders exactly as it did: units nil and units [USD] are the same drawer.
func TestRenderSpendDrawer_DollarsAreUnchanged(t *testing.T) {
	snap := &usage.Snapshot{Group: usage.GroupModel, Buckets: []usage.Bucket{{
		Counts: usage.Counts{Requests: 5, PricedRequests: 5, CostMicros: 6_200_000, InputCostMicros: 6_200_000},
		Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 5, PricedRequests: 5, CostMicros: 6_200_000, AvoidedMicros: 70_000},
		},
	}}}
	want := renderSpendDrawer(snap, nil, usage.GroupModel, "1h", 140)
	snap.Currencies = []string{"USD"}
	snap.SeriesCurrencies = map[string][]string{"claude-opus-5": {"USD"}}
	got := renderSpendDrawer(snap, nil, usage.GroupModel, "1h", 140)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("a USD-labelled drawer differs from an unlabelled one:\n got %q\nwant %q", got, want)
	}
}

// In a Bob + Claude window each row is in its own unit, and the tier column — which would split a
// cross-unit sum — is withheld.
func TestRenderSpendDrawer_EachRowInItsOwnUnitAndNoMixedTiers(t *testing.T) {
	snap := &usage.Snapshot{
		Group:      usage.GroupEndpoint,
		Currencies: []string{"Bobcoins", "USD"},
		SeriesCurrencies: map[string][]string{
			"api.us-east.bob.ibm.com": {"Bobcoins"}, "gw.internal": {"USD"},
		},
		Buckets: []usage.Bucket{{
			Counts: usage.Counts{Requests: 8, PricedRequests: 8, CostMicros: 6_207_800, InputCostMicros: 6_207_800},
			Series: map[string]usage.Counts{
				"api.us-east.bob.ibm.com": {Requests: 3, PricedRequests: 3, CostMicros: 7_800},
				"gw.internal":             {Requests: 5, PricedRequests: 5, CostMicros: 6_200_000},
			},
		}},
	}
	out := strings.Join(renderSpendDrawer(snap, nil, usage.GroupEndpoint, "1h", 140), "\n")
	if !strings.Contains(out, "$6.20") {
		t.Errorf("Claude's endpoint lost its dollars:\n%s", out)
	}
	if !strings.Contains(out, "0.01 Bobcoins") {
		t.Errorf("Bob's endpoint is not in Bobcoins:\n%s", out)
	}
	if strings.Contains(out, "$6.21") || strings.Contains(out, "$0.01") {
		t.Errorf("a cross-unit sum or a Bobcoins figure reached the drawer as dollars:\n%s", out)
	}
	if !strings.Contains(out, "tiers") || !strings.Contains(out, money.Mixed) {
		t.Errorf("the tier column split a cross-unit total instead of withholding it:\n%s", out)
	}
}

func costChartSnapshot(currencies []string) *usage.Snapshot {
	snap := &usage.Snapshot{Priced: true, Currencies: currencies}
	for i, c := range []int64{30_000, 7_800_000, 123_450_000} {
		b := usage.Bucket{Counts: usage.Counts{Requests: int64(i + 1), PricedRequests: int64(i + 1), CostMicros: c}}
		snap.Buckets = append(snap.Buckets, b)
		snap.Totals.Add(b.Counts)
	}
	return snap
}

// The usage pane's cost chart and COST line are unchanged for dollars, carry no "$" for a foreign
// unit, and refuse to chart a window that mixes units.
func TestUsagePane_CostIsInTheWindowsUnit(t *testing.T) {
	want := strings.Join(renderUsageChart(costChartSnapshot(nil), metricCost, "", 60, 12), "\n")
	if got := strings.Join(renderUsageChart(costChartSnapshot([]string{"USD"}), metricCost, "", 60, 12), "\n"); got != want {
		t.Errorf("a USD-labelled chart differs from an unlabelled one:\n got %s\nwant %s", got, want)
	}
	if got, want := renderCostSummary(costChartSnapshot([]string{"USD"})), renderCostSummary(costChartSnapshot(nil)); got != want {
		t.Errorf("USD COST line = %q, want %q", got, want)
	}

	bob := costChartSnapshot([]string{"Bobcoins"})
	if chart := strings.Join(renderUsageChart(bob, metricCost, "", 60, 12), "\n"); strings.Contains(chart, "$") {
		t.Errorf("a Bobcoins chart is labelled in dollars:\n%s", chart)
	}
	if got := renderCostSummary(bob); !strings.HasPrefix(got, "COST 131.28 Bobcoins") {
		t.Errorf("Bobcoins COST line = %q, want it to start COST 131.28 Bobcoins", got)
	}

	mixed := costChartSnapshot([]string{"Bobcoins", "USD"})
	if chart := renderUsageChart(mixed, metricCost, "", 60, 12); len(chart) != 2 || !strings.Contains(chart[0], "withheld") {
		t.Errorf("a mixed cost chart was drawn: %q", chart)
	}
	if got := renderCostSummary(mixed); !strings.HasPrefix(got, "COST (mixed): Bobcoins, USD") {
		t.Errorf("mixed COST line = %q", got)
	}
	// Tokens chart normally over the same mixed window: only money has a unit.
	if chart := renderUsageChart(mixed, metricTokens, "", 60, 12); len(chart) < 2 {
		t.Errorf("the tokens chart was withheld too: %q", chart)
	}
}

// The no-agent note keeps its dollar amount, and drops it for any other unit.
func TestCostUngroupedRow_OnlyDollarsCarryAnAmount(t *testing.T) {
	residual := int64(250_000)
	snap := &usage.Snapshot{UngroupedCostMicros: &residual}
	want := costUngroupedRow(snap, "claude-code/2.1.284", nil)
	if !strings.Contains(want, "$0.25") {
		t.Fatalf("dollar note = %q, want the amount", want)
	}
	if got := costUngroupedRow(snap, "bob-shell/2.0.5", []string{"Bobcoins"}); strings.Contains(got, "$") || got == "" {
		t.Errorf("Bobcoins note = %q, want the note without a dollar amount", got)
	}
}

// An exchange in Bobcoins reads in Bobcoins in the events pane, the saving included, with the unit
// named once; the same exchange without a unit is the dollar cell TestCostCellPhases pins.
func TestCostCell_ARecordInAForeignUnitIsLabelledSo(t *testing.T) {
	wire := strings.Replace(costWire, `"source":"gateway-header",`, `"source":"gateway-header","currency":"Bobcoins",`, 1)
	if wire == costWire {
		t.Fatal("the fixture no longer has the anchor this test rewrites")
	}
	req := reqEvent(t, bigPromptWire)
	req.RequestID = "aaa"
	resp := respEvent(wire, agentTurn())
	resp.RequestID = "aaa"
	if got := exchangeCostCell(eventRow{event: req, resp: resp}); got != "0.2824(−0.0038) Bobcoins" {
		t.Errorf("COST = %q, want 0.2824(−0.0038) Bobcoins", got)
	}
}

// The y-axis caption of a Bobcoins cost chart names Bobcoins, not USD; a dollar chart keeps USD.
func TestUsageChart_CaptionNamesTheUnit(t *testing.T) {
	width, height := axisCaptionWidth+20, 24
	dollars := strings.Join(renderUsageChart(costChartSnapshot(nil), metricCost, "", width, height), "\n")
	if !strings.Contains(dollars, "USD") {
		t.Fatalf("the dollar chart has no USD caption at width %d, so this pins nothing:\n%s", width, dollars)
	}
	bob := strings.Join(renderUsageChart(costChartSnapshot([]string{"Bobcoins"}), metricCost, "", width, height), "\n")
	if strings.Contains(bob, "USD") || !strings.Contains(bob, "Bobc…") {
		t.Errorf("a Bobcoins chart is captioned wrongly:\n%s", bob)
	}
}

// The wiring, end to end through the model: a Bobcoins session's row in the sessions table, and a
// mixed hour's reply through applySpendLoaded into the band reading.
func TestUnits_ReachTheTablesAndTheBandThroughTheModel(t *testing.T) {
	m := &model{width: 200}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{
		ID: "ebc67cf8fa68", UpdatedAt: time.Now(), EventCount: 18,
		TotalTokens: 5_300, CostMicros: 10_600, Currencies: []string{"Bobcoins"},
	}}
	m.rebuildSessionsTable()
	rows := m.sessionsTbl.Rows()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if row := strings.Join(rows[0], " "); strings.Contains(row, "$") || !strings.Contains(row, "Bob") {
		t.Errorf("Bob's session row %q is not in Bobcoins", row)
	}

	m.spend.chains[spanHour].reqSeq = 7
	m.applySpendLoaded(spendLoadedMsg{span: spanHour, req: 7,
		snap: &usage.Snapshot{Window: "1h0m0s", Priced: true, Currencies: []string{"Bobcoins", "USD"},
			Totals: usage.Counts{Requests: 4, PricedRequests: 4, PriceableRequests: 4, CostMicros: 6_207_800}},
		byUnit: map[string]int64{"Bobcoins": 7_800, "USD": 6_200_000}})
	if got := bandValue(m.spanReadings()[spanHour]); got != "$6.20 + 0.01 Bobcoins" {
		t.Errorf("hour cell = %q, want $6.20 + 0.01 Bobcoins", got)
	}
}

// The AGENTS fetch carries each agent's units from the reply into its rows.
func TestFetchAgentRows_CarriesEachAgentsUnits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"window":"today","group":"agent","currencies":["Bobcoins","USD"],` +
			`"seriesCurrencies":{"bob-shell/2.0.5":["Bobcoins"],"claude-code/2.1.284":["USD"]},` +
			`"buckets":[{"series":{"bob-shell/2.0.5":{"requests":3,"pricedRequests":3,"costMicros":7800},` +
			`"claude-code/2.1.284":{"requests":9,"pricedRequests":9,"costMicros":6200000}}}],"totals":{}}`))
	}))
	defer ts.Close()
	m := &model{client: apiclient.New(ts.URL)}
	msg, ok := m.fetchAgentRowsCmd(agentsOpenOnPress, paneSessions)().(agentRowsLoadedMsg)
	if !ok || msg.err != nil {
		t.Fatalf("fetch = %+v", msg)
	}
	seen := 0
	for _, r := range msg.rows {
		cell := agentCostCellIn(r.Counts, r.units, agentsCostWidth)
		switch r.label {
		case "claude-code/2.1.284":
			seen++
			if cell != "$6.20" {
				t.Errorf("claude-code via the fetch = %q, want $6.20", cell)
			}
		case "bob-shell/2.0.5":
			seen++
			if strings.Contains(cell, "$") || !strings.Contains(cell, "Bob") {
				t.Errorf("bob-shell via the fetch = %q, want a Bobcoins figure", cell)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d of the 2 agents in %+v", seen, msg.rows)
	}
}

// A unit whose requests were all unpriced has no figure, so the split must not print it as $0.00.
func TestBandValue_AUnitNothingPricedIsNotAFigure(t *testing.T) {
	snap := &usage.Snapshot{Group: usage.GroupCurrency, Currencies: []string{"Bobcoins", "USD"},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"Bobcoins": {Requests: 1, PricedRequests: 1, CostMicros: 7_800},
			"USD":      {Requests: 3},
		}}}}
	r := spanReading{USD: 0.0078, Priced: true, Units: snap.Currencies, ByUnit: unitCosts(snap)}
	if got := bandValue(r); strings.Contains(got, "$0.00") {
		t.Errorf("band = %q: USD carried no priced request, so it is no $0.00 figure", got)
	}
}

// A unit the window names but the split shows no spend for — a saving-only unit, whose traffic
// is not priceable and so marks nothing partial — cannot be left out silently, or the band prints
// one unit's figure for a window holding two. It says mixed, like the drawer and AGENTS pane.
// A split that does not cover a unit at all is not used either.
func TestBandValue_AUnitWithNoSpendInTheSplitIsMixed(t *testing.T) {
	split := func(series map[string]usage.Counts) *usage.Snapshot {
		return &usage.Snapshot{Group: usage.GroupCurrency, Currencies: []string{"Bobcoins", "USD"},
			Buckets: []usage.Bucket{{Series: series}}}
	}
	usd := usage.Counts{Requests: 3, PricedRequests: 3, PriceableRequests: 3, CostMicros: 6_200_000}
	totals := usage.Counts{Requests: 4, PricedRequests: 3, PriceableRequests: 3, CostMicros: 6_200_000, AvoidedMicros: 500_000}
	unpriced, priceable := unpricedGap(totals)
	for name, snap := range map[string]*usage.Snapshot{
		"saving-only Bobcoins":   split(map[string]usage.Counts{"Bobcoins": {Requests: 1, AvoidedMicros: 500_000}, "USD": usd}),
		"split missing Bobcoins": split(map[string]usage.Counts{"USD": usd}),
	} {
		r := spanReading{USD: 6.2, Priced: true, Unpriced: unpriced, Priceable: priceable, Units: snap.Currencies, ByUnit: unitCosts(snap)}
		if got := bandValue(r); got != money.Mixed {
			t.Errorf("%s: band = %q, want %s", name, got, money.Mixed)
		}
	}
}
