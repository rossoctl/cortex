package tui

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
)

// One agent's totals are folded across every bucket in the window.
//
// The snapshot carries per-agent figures PER BUCKET (usage.Bucket.Series), while the pane
// shows one row per agent for the whole window — so the fold is the pane's entire data path,
// and a fold that took only the last bucket would still render a plausible-looking table.
// Two buckets for one agent is the smallest input that tells the difference.
func TestAgentRowsFromBuckets_FoldsOneAgentAcrossBuckets(t *testing.T) {
	buckets := []usage.Bucket{
		{Series: map[string]usage.Counts{
			"bob-shell/2.0.5": {Requests: 3, Tokens: 1_000, CostMicros: 2_000},
		}},
		{Series: map[string]usage.Counts{
			"bob-shell/2.0.5": {Requests: 5, Tokens: 2_500, CostMicros: 5_000},
		}},
	}

	rows := agentRowsFromBuckets(buckets)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 — one agent must fold to one row", len(rows))
	}
	if rows[0].label != "bob-shell/2.0.5" {
		t.Errorf("label = %q, want %q", rows[0].label, "bob-shell/2.0.5")
	}
	if rows[0].Requests != 8 {
		t.Errorf("Requests = %d, want 8 (3+5)", rows[0].Requests)
	}
	if rows[0].Tokens != 3_500 {
		t.Errorf("Tokens = %d, want 3500 (1000+2500)", rows[0].Tokens)
	}
	if rows[0].CostMicros != 7_000 {
		t.Errorf("CostMicros = %d, want 7000 (2000+5000)", rows[0].CostMicros)
	}
}

// The fold puts the most expensive agent first.
//
// Ordering by cost is deterministic even out of a map walk, because the costs differ — so this
// is the half of the contract the fold itself can honestly assert. The tie-break is pinned by
// TestSortAgentRows_OrdersByCostThenLabel, which does not depend on map order at all.
func TestAgentRowsFromBuckets_PutsTheCostliestAgentFirst(t *testing.T) {
	buckets := []usage.Bucket{{Series: map[string]usage.Counts{
		"cheap/1.0":           {Requests: 1, CostMicros: 1},
		"claude-code/2.1.270": {Requests: 1, CostMicros: 500},
		"middle/1.0":          {Requests: 1, CostMicros: 100},
	}}}

	rows := agentRowsFromBuckets(buckets)

	want := []string{"claude-code/2.1.270", "middle/1.0", "cheap/1.0"}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].label != w {
			t.Errorf("row %d = %q, want %q", i, rows[i].label, w)
		}
	}
}

// The AGENTS pane is skipped below two agents.
//
// This is a user-experience requirement with a correctness edge: a picker offering one choice
// is a keystroke that cannot change anything, and every deployment today has exactly one
// agent — so entering the pane unconditionally would put a new mandatory step in front of
// every existing user to no purpose. It is asserted as a RULE OVER DATA rather than through
// the TUI so that it cannot be satisfied by some incidental navigation detail.
//
// Zero is included and is not hypothetical: a proxy that has served no inference yet reports
// no agent series at all, and that must behave like the single-agent case rather than
// entering an empty picker with nothing to select.
func TestAgentsPaneApplies_SkippedBelowTwoAgents(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  []agentRow
		apply bool
	}{
		{"no agents yet", nil, false},
		{"one agent, which is every deployment today", []agentRow{{label: "claude-code/2.1.270"}}, false},
		{"two agents is the case the pane exists for", []agentRow{
			{label: "claude-code/2.1.270"}, {label: "bob-shell/2.0.5"},
		}, true},
		{"three agents", []agentRow{
			{label: "a"}, {label: "b"}, {label: "c"},
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentsPaneApplies(tc.rows); got != tc.apply {
				t.Errorf("agentsPaneApplies(%d rows) = %v, want %v", len(tc.rows), got, tc.apply)
			}
		})
	}
}

// The help overlay tells the two "agent" panes apart.
//
// This repo uses the word for two unrelated things: paneNamespaces lists KUBERNETES
// workloads, and its purpose line called them "agents grouped by namespace", while paneAgents
// lists CODING agents — the clients seen on the wire. Side by side in the [?] overlay those
// two descriptions sent a reader to the wrong pane, and the overlay is the one surface that
// renders both at once, so it is where the ambiguity had to be resolved.
//
// Asserted on the distinguishing WORD in each, not on the full sentence, so rewording either
// purpose stays free while dropping the distinction does not.
func TestPaneKeys_TheTwoAgentPanesAreDistinguishable(t *testing.T) {
	ns, ok := paneKeys[paneNamespaces]
	if !ok {
		t.Fatal("paneNamespaces has no paneKeys entry")
	}
	ag, ok := paneKeys[paneAgents]
	if !ok {
		t.Fatal("paneAgents has no paneKeys entry")
	}
	if !strings.Contains(ns.purpose, "Kubernetes") {
		t.Errorf("paneNamespaces purpose %q does not say Kubernetes — it lists workloads, and without that word it reads as the coding-agent pane", ns.purpose)
	}
	if !strings.Contains(ag.purpose, "coding") {
		t.Errorf("paneAgents purpose %q does not say coding — it lists wire clients, and without that word it reads as the namespace pane", ag.purpose)
	}
}

// Money folds through usage.Counts.Add, so it saturates instead of wrapping negative.
//
// Not a figure anybody will reach — it needs ~$9.2T in one label — but the direction of the
// failure is what makes it worth pinning, and the package has already been bitten by it: the
// spend drawer summed with a raw accumulator, and rankSeriesByCost's godoc records that a
// wrapped total ranks BELOW a ten-micro series, which silently drops the window's most
// expensive row out of the table. Here the same wrap would put the busiest agent at the
// bottom of the picker. Add is exported precisely so agentop folds arbitrary Counts the one
// way, and this asserts this pane took it.
func TestAgentRowsFromBuckets_MoneySaturatesRatherThanWrapping(t *testing.T) {
	buckets := []usage.Bucket{
		{Series: map[string]usage.Counts{"a/1": {CostMicros: math.MaxInt64 - 5}}},
		{Series: map[string]usage.Counts{"a/1": {CostMicros: 100}}},
	}

	rows := agentRowsFromBuckets(buckets)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].CostMicros != math.MaxInt64 {
		t.Errorf("CostMicros = %d, want MaxInt64 — a raw += would have wrapped negative here", rows[0].CostMicros)
	}
	if !rows[0].Saturated {
		t.Error("Saturated = false; the row must disclose that its total was clamped")
	}
}

// esc leaves the AGENTS pane and lands where `A` was pressed.
//
// A KEY-OPENED SURFACE MUST RETURN TO ITS CALLER, which is the rule paneCatalog's own esc
// case states and the reason the sessions pane is the only one whose esc costs the
// connection. Without a case of its own a new pane is a DEAD END: esc falls through, the
// reader is stuck, and the only way out is `q`.
//
// The paneNone fallback is Sessions rather than the pane enum's zero value. It is reachable
// rather than defensive — the same way paneCatalog's is — and Sessions is the one pane that is
// always a defensible place to land; falling back to the zero value would drop the reader on
// the Kubernetes namespace picker, tearing down nothing but looking like the connection went
// away.
func TestAgentsPane_EscReturnsToTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name         string
		previousPane paneID
		want         paneID
	}{
		{"opened from sessions", paneSessions, paneSessions},
		{"opened from events", paneEvents, paneEvents},
		{"opened from detail", paneDetail, paneDetail},
		// Usage is the one caller with a tail beyond the pane swap: its polling chain has to be
		// restarted on the way back, so without this row that branch is unexercised.
		{"opened from usage", paneUsage, paneUsage},
		{"no caller recorded falls back to sessions", paneNone, paneSessions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{
				pane:               paneAgents,
				previousPane:       tc.previousPane,
				client:             deadClient(),
				pipelineReturnPane: paneNone,
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			if m.pane != tc.want {
				t.Errorf("esc from AGENTS left pane = %v, want %v", m.pane, tc.want)
			}
		})
	}
}

// The caller `esc` returns to is the pane `A` was pressed on, not the pane the reply lands on.
//
// THE FETCH IS A ROUND TRIP, so these are two different panes whenever the reader navigates
// while it is in flight — and `A` pressed ON the pane is the case that bit: it recorded
// paneAgents as its own caller, so the esc arm set pane to the pane it was already on and the
// key read as broken. agentRowsLoadedMsg.from carries the press-time answer across, which is the
// same reasoning the struct's `open` field already documents.
//
// Driven through handleKey and Update rather than by calling enterAgentsOrRefuse directly: the
// defect was in WHEN the caller was read, so a test that passes `from` itself cannot see it.
func TestAgentsPane_RecordsTheCallerAtPressTimeNotAtReplyTime(t *testing.T) {
	rows := []agentRow{
		{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 1049, PricedRequests: 1048}},
		{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
	}
	for _, tc := range []struct {
		name string
		// pressedOn is where `A` is pressed; movedTo is where the reader has navigated to by
		// the time the reply lands. wantBack is where esc must then go.
		pressedOn, movedTo, wantBack paneID
		// previousPane seeds the model, so the refetch case can prove it is PRESERVED rather
		// than merely not overwritten with paneAgents.
		previousPane paneID
	}{
		// NOT paneSessions AS THE CALLER in either of the first two rows, and that is the
		// difference between a guard and a decoration. The esc arm falls back to paneSessions
		// when no caller was recorded, so a row that presses `A` FROM Sessions gets the right
		// answer out of the fallback as well — deleting the assignment outright left all three
		// rows green (mutant `enter-no-previouspane` SURVIVED) until these two moved off it. A
		// caller the fallback cannot coincide with is what makes the row able to fail.
		{
			name:      "reader stays put",
			pressedOn: paneDetail, movedTo: paneDetail, wantBack: paneDetail,
			previousPane: paneNone,
		},
		{
			// The reply-time read returned paneEvents here — the later pane, which never
			// asked for anything.
			name:      "reader navigates while the fetch is in flight",
			pressedOn: paneDetail, movedTo: paneEvents, wantBack: paneDetail,
			previousPane: paneNone,
		},
		{
			// A refresh. The reply-time read made the pane its own caller and esc went nowhere.
			name:      "A pressed again while already on the pane keeps the original caller",
			pressedOn: paneAgents, movedTo: paneAgents, wantBack: paneUsage,
			previousPane: paneUsage,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{
				pane:               tc.pressedOn,
				previousPane:       tc.previousPane,
				agents:             rows,
				agentsTbl:          newAgentsTable(),
				client:             deadClient(),
				pipelineReturnPane: paneNone,
			}
			// The press, and then its command is RUN, so the `from` under test is the one
			// keys.go resolved rather than one this test recomputed. deadClient points at a
			// refused port, so the reply carries an error — and `from` alongside it, which is
			// the only field wanted here. Recomputing the press-time rule locally would make
			// this pass no matter what keys.go decided.
			cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
			if cmd == nil {
				t.Fatalf("`A` on %v returned no fetch command", tc.pressedOn)
			}
			failed, ok := cmd().(agentRowsLoadedMsg)
			if !ok {
				t.Fatalf("`A`'s command did not produce an agentRowsLoadedMsg")
			}
			// The reader moves before the reply arrives. A reply-time read of m.pane sees this
			// pane; the press never did.
			m.pane = tc.movedTo
			updated, _ := m.Update(agentRowsLoadedMsg{rows: rows, open: failed.open, from: failed.from})
			m = updated.(*model)
			if m.pane != paneAgents {
				t.Fatalf("reply did not open the pane: pane = %v", m.pane)
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			if m.pane != tc.wantBack {
				t.Errorf("esc after `A` from %v (reader moved to %v) landed on %v, want %v",
					tc.pressedOn, tc.movedTo, m.pane, tc.wantBack)
			}
		})
	}
}

// The COST cell says "—" for an agent nothing priced and a real figure for one priced at zero.
//
// THE ZERO-RATE ROW IS THE WHOLE TEST. "unpriced renders —" alone is satisfied by keying on
// CostMicros, which is the wrong field and the mistake this cell was written to avoid: a request
// CAN be priced at a rate of zero, and collapsing that into "not priced" throws away the one
// distinction the column exists to make. Only a row with PricedRequests > 0 AND CostMicros == 0
// separates the two readings, so without it both a CostMicros key and no guard at all pass.
//
// Bob is the live instance of the unpriced row — it bills in credits, which the cost model
// cannot represent — and `agentop cost --agent` makes the same call through snapshot.Priced, which
// is why TestRunCost_AgentReportsThatAgentOnly asserts "cost unavailable" on the same shape.
func TestAgentCostCell_UnpricedIsADashAndAZeroRateIsAFigure(t *testing.T) {
	for _, tc := range []struct {
		name string
		usage.Counts
		want string
	}{
		{
			// Bob: priceable traffic, nothing priced it.
			"nothing priced it",
			usage.Counts{Requests: 8, PriceableRequests: 7},
			emptyCell,
		},
		{
			// The case that fails a CostMicros key: priced, and it genuinely cost nothing.
			"priced at a rate of zero",
			usage.Counts{Requests: 4, PricedRequests: 4, PriceableRequests: 4},
			"$0.00",
		},
		{
			"priced with a cost",
			usage.Counts{Requests: 1049, PricedRequests: 1048, CostMicros: 146_361_600},
			"$146.36",
		},
		{
			// Sub-half-cent: the floor formatUSDTotalMicros carries, so a known charge never
			// prints as free. A dash here would be the same lie from the other direction.
			"priced below half a cent",
			usage.Counts{Requests: 1, PricedRequests: 1, CostMicros: 400},
			"<$0.01",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentCostCell(tc.Counts); got != tc.want {
				t.Errorf("agentCostCell(%+v) = %q, want %q", tc.Counts, got, tc.want)
			}
		})
	}
}

// And the cell the table actually renders is that function's answer, not a second spelling.
//
// agentCostCell could be correct while rebuildAgentsTable formatted the money itself — the
// mutation that deleted the guard would then still show $0.00 on screen with the unit test
// green. This walks the built rows so the assertion covers the path a reader sees.
func TestRebuildAgentsTable_RendersTheUnpricedAgentAsADash(t *testing.T) {
	m := &model{
		agentsTbl: newAgentsTable(),
		agents: []agentRow{
			{label: "claude-code/2.1.270", Counts: usage.Counts{
				Requests: 1049, Tokens: 297_961_318, PricedRequests: 1048, CostMicros: 146_361_600}},
			{label: "bob-shell/2.0.5", Counts: usage.Counts{
				Requests: 8, Tokens: 38_682, PriceableRequests: 7}},
		},
	}
	m.rebuildAgentsTable()
	rows := m.agentsTbl.Rows()
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want All agents and 2", len(rows))
	}
	// Cost is the last column; see newAgentsTable for why it is the widest.
	const costCol = 3
	if got := rows[2][costCol]; got != emptyCell {
		t.Errorf("unpriced agent's COST cell = %q, want %q — $0.00 would read as free", got, emptyCell)
	}
	if got := rows[1][costCol]; got != "$146.36" {
		t.Errorf("priced agent's COST cell = %q, want %q", got, "$146.36")
	}
}

// ↑↓ and j/k move the cursor, because two surfaces promise they do.
//
// AN ADVERTISED KEY THAT DOES NOTHING is the defect agentsPaneRefusal's doc names from the
// other direction — "`A` doing nothing looks like a broken binding" — and this pane shipped
// with exactly that from the opposite end: handleKey's fall-through dispatch switch carried
// arms for sessions, events, detail, pipeline and catalog and none for paneAgents, so every
// navigation key fell off the end of it and returned nil. The footer printed "[↑↓] nav" and
// the help overlay listed "↑↓ / jk  navigate" over a cursor that could not move, and the table
// is built WithFocused(true), so it rendered a selection highlight the whole time.
//
// DRIVEN THROUGH handleKey, never through agentsTbl.Update: the missing code WAS the dispatch,
// so a test calling the table's own Update would have been green against the bug it is here to
// catch. That is the same reason TestAgentsPane_RecordsTheCallerAtPressTimeNotAtReplyTime goes
// through handleKey rather than calling enterAgentsOrRefuse.
//
// BOTH SPELLINGS OF EACH DIRECTION, because they are two separate claims: bubbles' table binds
// arrows and jk independently, and the help overlay advertises the pair as one binding.
func TestAgentsPane_NavigationKeysMoveTheCursor(t *testing.T) {
	// Three agents, so a cursor that moves can also be seen to stop: a two-row fixture cannot
	// tell "moved one row" from "jumped to the end". All agents above them makes four rows.
	rows := []agentRow{
		{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 1049, PricedRequests: 1048, CostMicros: 146_361_600}},
		{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 118}},
		{label: "cursor/1.2.3", Counts: usage.Counts{Requests: 12, PricedRequests: 12, CostMicros: 4_000}},
	}
	key := func(s string) tea.KeyMsg {
		switch s {
		case "up":
			return tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			return tea.KeyMsg{Type: tea.KeyDown}
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	for _, tc := range []struct {
		name string
		keys []string
		want int
	}{
		{"down moves to the second row", []string{"down"}, 1},
		{"j moves to the second row", []string{"j"}, 1},
		{"down twice reaches the third", []string{"down", "down"}, 2},
		{"up comes back", []string{"down", "down", "up"}, 1},
		{"k comes back", []string{"j", "j", "k"}, 1},
		// The clamps, which are bubbles' own behavior and are asserted so a future dispatch
		// arm that reimplemented the movement by hand could not quietly run off either end.
		// The two are not equally strong, and it is worth knowing which: the bottom clamp also
		// fails outright when the arm is missing (want 3, no dispatch leaves the cursor at 0),
		// but the top clamp's want IS the no-dispatch value, so it can only catch a hand-rolled
		// reimplementation — never a missing arm. The five rows above are what cover dispatch.
		{"up on the first row stays put", []string{"up"}, 0},
		{"down past the last row stays on it", []string{"down", "down", "down", "down", "down"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{
				pane:               paneAgents,
				previousPane:       paneSessions,
				agents:             rows,
				agentsTbl:          newAgentsTable(),
				client:             deadClient(),
				pipelineReturnPane: paneNone,
			}
			m.rebuildAgentsTable()
			for _, k := range tc.keys {
				m.handleKey(key(k))
			}
			if got := m.agentsTbl.Cursor(); got != tc.want {
				t.Errorf("after %v the cursor is on row %d, want %d", tc.keys, got, tc.want)
			}
		})
	}
}

// errStartupProbe stands in for whatever the endpoint answered. The message is never rendered
// by the cases that use it — that is the assertion — so its text only has to be identifiable
// in a failure.
var errStartupProbe = errors.New("dial tcp 127.0.0.1:1: connect: connection refused")

// Startup lands on AGENTS when two or more agents are on the wire, and nowhere else otherwise.
//
// THE GATE IS agentsPaneApplies, which until now had no caller outside its own tests: the rule
// "fewer than two agents skips the pane" was written, asserted and never consulted. A picker
// offering one row is a keystroke that cannot change what is displayed, and one agent is still
// every deployment that has not adopted a second — so entering unconditionally would put a new
// mandatory step in front of those users to no purpose.
//
// SILENT WHEN IT DECLINES. enterAgentsOrRefuse's refusal is written for someone who pressed `A`
// and is owed an answer; nobody asked for this one, so flashing "only claude-code has been seen"
// at every startup would be an unsolicited complaint about a normal deployment.
func TestAgentsPane_StartupEntersOnlyWhereTheGateApplies(t *testing.T) {
	row := func(label string) agentRow {
		return agentRow{label: label, Counts: usage.Counts{Requests: 10, PricedRequests: 10}}
	}
	for _, tc := range []struct {
		name string
		rows []agentRow
		want paneID
	}{
		// The case this feature exists for: a laptop running Claude Code and Bob at once.
		{"two agents opens the picker", []agentRow{row("claude-code/2.1.270"), row("bob-shell/2.0.5")}, paneAgents},
		{"three agents opens the picker", []agentRow{row("a/1"), row("b/2"), row("c/3")}, paneAgents},
		// Every deployment with one coding agent, which is most of them.
		{"one agent goes straight to sessions", []agentRow{row("claude-code/2.1.270")}, paneSessions},
		// Reachable rather than theoretical: a proxy that has served no inference yet reports
		// no agent series at all, and an empty picker is worse than the view behind it.
		{"no agents goes straight to sessions", nil, paneSessions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{pane: paneSessions, previousPane: paneNone, agentsTbl: newAgentsTable(), client: deadClient()}
			updated, _ := m.Update(agentRowsLoadedMsg{rows: tc.rows, open: agentsOpenAtStartup})
			m = updated.(*model)
			if m.pane != tc.want {
				t.Errorf("startup with %d agents landed on %v, want %v", len(tc.rows), m.pane, tc.want)
			}
			if m.flash != "" {
				t.Errorf("startup flashed %q; nobody asked for the agents pane, so a refusal "+
					"is an unsolicited complaint", m.flash)
			}
		})
	}
}

// The startup picker is the level above Sessions, so esc on it backs out of the connection the way
// esc on Sessions does: to the pods picker, and nowhere in --endpoint mode.
func TestAgentsPane_StartupPickerEscBacksOutOfTheConnection(t *testing.T) {
	rows := []agentRow{
		{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10}},
		{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
	}
	for _, tc := range []struct {
		name     string
		picker   bool
		want     paneID
		wantHint string
	}{
		{"pods picker mode", true, panePods, "[esc] pods"},
		{"--endpoint mode", false, paneAgents, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// previousPane is seeded with a pane the startup arm must overwrite.
			m := &model{pane: paneSessions, previousPane: panePipeline, agentsTbl: newAgentsTable(), client: deadClient()}
			if tc.picker {
				m.parentCtx = context.Background()
				m.ctx, m.cancel = context.WithCancel(m.parentCtx)
				defer func() { m.cancel() }()
			}
			updated, _ := m.Update(agentRowsLoadedMsg{rows: rows, open: agentsOpenAtStartup})
			m = updated.(*model)
			if m.pane != paneAgents || m.previousPane != paneNone || !m.agentsAboveSessions {
				t.Fatalf("startup: pane %v previousPane %v above %v, want the picker above Sessions",
					m.pane, m.previousPane, m.agentsAboveSessions)
			}
			footer := m.helpView()
			if tc.wantHint != "" && !strings.Contains(footer, tc.wantHint) {
				t.Errorf("footer %q omits %q", footer, tc.wantHint)
			}
			if tc.wantHint == "" && strings.Contains(footer, "[esc]") {
				t.Errorf("footer %q offers esc, which goes nowhere here", footer)
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			if m.pane != tc.want {
				t.Errorf("esc from the startup picker landed on %v, want %v", m.pane, tc.want)
			}
		})
	}
}

// The gate interrupts Sessions and nothing else. Its reply lands a round trip after the view
// opened, so an operator can already have moved; one who has is left where they are, with the
// return pane they had, rather than pulled into the picker with an esc that lands on Sessions.
func TestAgentsPane_StartupLeavesAnOperatorWhoHasMoved(t *testing.T) {
	rows := []agentRow{
		{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10}},
		{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
	}
	visited := 0
	for start := paneID(0); start <= lastPaneID; start++ {
		if start == paneSessions {
			continue
		}
		visited++
		m := &model{pane: start, previousPane: panePipeline, agentsTbl: newAgentsTable(), client: deadClient()}
		updated, _ := m.Update(agentRowsLoadedMsg{rows: rows, open: agentsOpenAtStartup})
		m = updated.(*model)
		if m.pane != start || m.previousPane != panePipeline {
			t.Errorf("a startup reply arriving on %v moved the operator to %v (previousPane %v)",
				start, m.pane, m.previousPane)
		}
	}
	if visited != int(lastPaneID) {
		t.Fatalf("visited %d panes, want %d", visited, lastPaneID)
	}
}

// A failed startup fetch is silent too, and leaves the reader on the sessions pane.
//
// The operator did not ask for this fetch, and the pane they ARE looking at reports its own
// connection trouble — so a second error line about a breakdown nobody requested would be noise
// on top of the message that matters. The error is still recorded, so pressing `A` later says
// what happened rather than showing an empty grid.
func TestAgentsPane_StartupFetchFailureIsSilent(t *testing.T) {
	m := &model{pane: paneSessions, previousPane: paneNone, agentsTbl: newAgentsTable(), client: deadClient(),
		// TWO ROWS, so the error is the only thing that can hold the pane back. With none, the
		// gate's OTHER conjunct (agentsPaneApplies) is already false and the assertion passes
		// whatever the error handling does — measured: deleting `msg.err == nil` from the gate
		// left the whole package green. These rows stand for a previous pod's, which is how the
		// state is reachable: m.agents survives a failed fetch and the gate re-runs per
		// connection, so without the guard the picker opens showing another proxy's agents.
		agents: []agentRow{
			{label: "claude-code/2.1.270", Counts: usage.Counts{Requests: 10}},
			{label: "bob-shell/2.0.5", Counts: usage.Counts{Requests: 8}},
		}}
	updated, _ := m.Update(agentRowsLoadedMsg{err: errStartupProbe, open: agentsOpenAtStartup})
	m = updated.(*model)
	if m.pane != paneSessions {
		t.Errorf("a failed startup fetch moved the reader to %v", m.pane)
	}
	if m.flash != "" {
		t.Errorf("a failed startup fetch flashed %q", m.flash)
	}
	if m.agentsErr == nil {
		t.Error("agentsErr was not recorded; a later `A` press would show an empty pane instead of the reason")
	}
}

// initSessionView ARMS the gate. Without this nothing asserted the wiring, only the handler.
//
// THE FOUR TESTS ABOVE INJECT agentRowsLoadedMsg STRAIGHT INTO Update, which verifies what the
// reply does and never that anything sends it. Measured: replacing the `m.startupAgentsGateCmd(),`
// line in initSessionView with a no-op left the whole package green — so the PR's headline
// behaviour, "the picker opens itself at startup", had no test that it is ever reached in
// production. `grep -rn startupAgentsGateCmd --include=*_test.go` returned nothing.
//
// initSessionView IS THE RIGHT SEAM, not Init: it is the one place every entry point converges on
// (--endpoint mode's Init, the pod picker's portForwardReadyMsg, and [l]'s local endpoint), and it
// is where the gate has to live for a second pod's connection to re-run it.
//
// THE BATCH IS FANNED OUT CONCURRENTLY WITH A DEADLINE, because tea.Batch's leaves include a 1s
// tick and an SSE pump that never returns on their own. Running them in sequence would hang on
// the first blocker rather than reaching the gate's fetch.
func TestInitSessionView_ArmsTheStartupAgentsGate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only /v1/usage matters here; the batch's other leaves may 404 harmlessly.
		_, _ = w.Write([]byte(`{"window":"today","group":"agent","buckets":[{"at":"2026-09-29T10:00:00Z",` +
			`"requests":18,"series":{"claude-code/2.1.270":{"requests":10},"bob-shell/2.0.5":{"requests":8}}}],` +
			`"totals":{"requests":18}}`))
	}))
	defer ts.Close()

	m := &model{client: apiclient.New(ts.URL), agentsTbl: newAgentsTable(), pane: paneSessions}
	m.parentCtx = context.Background()
	m.ctx, m.cancel = context.WithCancel(m.parentCtx)
	defer m.cancel()

	cmd := m.initSessionView()
	if cmd == nil {
		t.Fatal("initSessionView returned no command")
	}
	first := cmd()
	batch, ok := first.(tea.BatchMsg)
	if !ok {
		t.Fatalf("initSessionView produced %T, want tea.BatchMsg", first)
	}
	got := make(chan agentRowsLoadedMsg, len(batch))
	for _, leaf := range batch {
		if leaf == nil {
			continue
		}
		go func(c tea.Cmd) {
			if msg, ok := c().(agentRowsLoadedMsg); ok {
				got <- msg
			}
		}(leaf)
	}
	select {
	case msg := <-got:
		// The VALUE, not just the arrival: a gate armed with agentsOpenOnPress would flash a
		// refusal at every single-agent startup, which is the behaviour agentsOpenAtStartup
		// exists to avoid.
		if msg.open != agentsOpenAtStartup {
			t.Errorf("the startup fetch carried open=%v, want agentsOpenAtStartup", msg.open)
		}
		// THE FETCH ACTUALLY SERVED. fetchAgentRowsCmd's error return carries the same `open`, so
		// without these two the server, the payload and the handler are all decoration: blanking
		// the fixture's agents to `{}` left the test green.
		if msg.err != nil {
			t.Errorf("the startup fetch failed: %v — the fixture is not being served", msg.err)
		}
		if len(msg.rows) != 2 {
			t.Errorf("the startup fetch carried %d rows, want the fixture's 2", len(msg.rows))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no agentRowsLoadedMsg came out of initSessionView's batch — the startup gate is not armed")
	}
}

// The open pane refetches its rows every agentsPollInterval, so an agent that starts sending
// traffic while the reader watches appears without an `A` press (#1209).
func TestAgentsPane_PollsWhileOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane paneID
		age  time.Duration
		want bool
	}{
		{"open and due", paneAgents, agentsPollInterval, true},
		{"open and fetched recently", paneAgents, agentsPollInterval / 2, false},
		{"another pane", paneSessions, agentsPollInterval, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now().Add(-tc.age)
			m := &model{pane: tc.pane, previousPane: paneNone, agentsTbl: newAgentsTable(),
				client: deadClient(), agentsFetchedAt: before}
			m.Update(refreshTickMsg(time.Now()))
			if got := m.agentsFetchedAt.After(before); got != tc.want {
				t.Errorf("refresh tick fetched agent rows = %v, want %v", got, tc.want)
			}
		})
	}
}
