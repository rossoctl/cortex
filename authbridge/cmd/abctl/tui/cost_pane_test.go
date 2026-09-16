package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// newTestModelOnPane builds a model through the package's real constructor, sized
// and laid out, sitting on the given pane.
//
// The real constructor rather than a &model{} literal: these tests drive key
// messages through Update, and a literal leaves the tables, the filter input and
// bodyHeight at their zero values — so a binding could "work" against a model no
// user ever sees. spend_strip_test.go's cases build the same thing inline; this is
// the named version of that, and the height is above spendStripMinHeight so the
// strip row is both reserved and drawn.
func newTestModelOnPane(t *testing.T, pane paneID) *model {
	t.Helper()
	resetSettingsForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, apiclient.New("http://127.0.0.1:1")).(*model)
	m.width, m.height = 120, 40
	m.layout()
	m.pane = pane
	return m
}

// pricedSnapshot is the smallest snapshot that carries a real, exact, fully
// covered dollar total.
func pricedSnapshot(t *testing.T) *usage.Snapshot {
	t.Helper()
	return &usage.Snapshot{
		Window: usage.WindowToday,
		Priced: true,
		Totals: usage.Counts{
			Requests: 10, CostMicros: 1_120_000,
			PricedRequests: 10, PriceableRequests: 10,
		},
	}
}

// TestOpenCostPane_RecordsTheReturnPaneAndStartsOneChain.
func TestOpenCostPane_RecordsTheReturnPaneAndStartsOneChain(t *testing.T) {
	m := &model{width: 120, height: 40}
	m.pane = paneEvents

	_ = m.openCostPane()

	if m.pane != paneCost {
		t.Errorf("pane = %v, want paneCost", m.pane)
	}
	if m.costPane.returnPane != paneEvents {
		t.Errorf("returnPane = %v, want paneEvents", m.costPane.returnPane)
	}
	if m.costPane.tickGen == 0 {
		t.Error("tickGen not advanced; no chain was started")
	}
}

func TestCostPane_StaleReplyIsDropped(t *testing.T) {
	m := &model{}
	m.costPane.reqSeq = 5
	fresh := &usage.Snapshot{Window: "today"}

	m.applyCostLoaded(costLoadedMsg{req: 4, snap: fresh})
	if m.costPane.snap != nil {
		t.Error("a reply from an older request was applied")
	}

	m.applyCostLoaded(costLoadedMsg{req: 5, snap: fresh})
	if m.costPane.snap != fresh {
		t.Error("the current request's reply was dropped")
	}
}

func TestCostPane_StaleTickIsDropped(t *testing.T) {
	m := &model{}
	m.costPane.tickGen = 2
	if m.costTickIsCurrent(1) {
		t.Error("a tick from generation 1 was accepted while 2 is current")
	}
	if !m.costTickIsCurrent(2) {
		t.Error("the current generation's tick was dropped")
	}
}

func TestOpenCostPane_TwiceLeavesOnlyOneLiveChain(t *testing.T) {
	// The documented historical bug, and this is the THIRD copy of the guard
	// pattern in this package — exactly where it comes back. usage_pane.go's
	// comment records what it costs: two live chains each rescheduling the other's
	// successor doubled the request rate for the life of the session.
	m := &model{width: 120, height: 40}
	m.pane = paneEvents

	_ = m.openCostPane()
	first := m.costPane.tickGen
	m.pane = paneEvents
	_ = m.openCostPane()

	if m.costPane.tickGen == first {
		t.Fatal("tickGen unchanged on re-entry; the previous chain was not invalidated")
	}
	if m.costTickIsCurrent(first) {
		t.Error("a tick from the first chain is still accepted; two chains are live")
	}
}

func TestCostPane_CycleWindowWrapsAndVisitsEach(t *testing.T) {
	var s costPaneState
	seen := map[string]bool{}
	for i := 0; i < len(costPaneWindows)*2; i++ {
		seen[s.window()] = true
		s.cycleWindow()
	}
	if len(seen) != len(costPaneWindows) {
		t.Errorf("visited %d of %d windows across two full cycles: %v",
			len(seen), len(costPaneWindows), seen)
	}
}

func TestCostPane_CycleGroupNeverYieldsNone(t *testing.T) {
	// This pane's whole purpose is the breakdown. An ungrouped view of it is the
	// strip, which is already on screen one row above.
	//
	// usage.GroupAgent IS in the want-list, and the comment that used to stand here was
	// wrong on all three of its claims. It read "it does not exist. Client identity is
	// not implemented, so there is no per-agent series to ask for" — true when the pane
	// shipped in ddb93fbd, and false since fc1a8271 added the constant and the per-agent
	// series, and 4cf32e33 put it in costPaneGroups as the fourth position.
	//
	// The test stayed green throughout, which is what made it worth fixing rather than
	// leaving: the closing assertion counts positions instead of naming them — four seen,
	// four in the cycle — so the agent axis was the one position nothing asserted was
	// REACHABLE, with a comment telling the next reader that was deliberate.
	//
	// This test and TestCostPaneGroups_IncludesAgentLast are complementary; neither
	// subsumes the other. That one reads the SLICE: membership and the last position,
	// which is a claim about what a reader is shown first. This one drives cycleGroup and
	// pins that every position is reachable by pressing the key — a slice can be correct
	// while the cycle skips an entry, which is exactly the shape of the bug cycleGroup's
	// own doc records, where matching only GroupNone made the first press a no-op.
	var s costPaneState
	seen := map[usage.Group]bool{}
	for i := 0; i < 12; i++ {
		s.cycleGroup()
		if s.group == usage.GroupNone || s.group == "" {
			t.Fatalf("cycleGroup yielded %q on iteration %d", s.group, i)
		}
		seen[s.group] = true
	}
	// By NAME, not only by count: the count assertion below is satisfied by any four
	// distinct axes, so on its own it cannot notice one being swapped for another.
	for _, want := range []usage.Group{
		usage.GroupModel, usage.GroupEndpoint, usage.GroupSession, usage.GroupAgent,
	} {
		if !seen[want] {
			t.Errorf("cycleGroup never yielded %q; that position of the cycle is unreachable", want)
		}
	}
	if len(seen) != len(costPaneGroups) {
		t.Errorf("cycleGroup visited %d groups, want the %d in costPaneGroups: %v",
			len(seen), len(costPaneGroups), seen)
	}
}

func TestCostPane_DoesNotShareStateWithTheEventsPane(t *testing.T) {
	// #953 asks that the pane update live "without disturbing the events pane".
	// The structural form of that requirement: opening and polling this pane must
	// not touch the events table's cursor, filter, or the spend strip's state.
	m := &model{width: 120, height: 40}
	m.pane = paneEvents
	m.eventsTbl.SetCursor(3)
	// The cursor is read back rather than asserted to be 3: this eventsTbl has no
	// rows, and bubbles' SetCursor clamps to the row range, so pinning the literal
	// would test the table's clamp instead of the Cost pane's isolation.
	cursorBefore := m.eventsTbl.Cursor()
	m.filter = "abc"
	spendBefore := m.spend

	_ = m.openCostPane()
	m.applyCostLoaded(costLoadedMsg{req: m.costPane.reqSeq, snap: &usage.Snapshot{Window: "today"}})

	if got := m.eventsTbl.Cursor(); got != cursorBefore {
		t.Errorf("events cursor moved from %d to %d", cursorBefore, got)
	}
	if m.filter != "abc" {
		t.Errorf("filter changed to %q", m.filter)
	}
	if m.spend != spendBefore {
		t.Error("the spend strip's state was modified by the cost pane")
	}
}

// pricedSnapshotWithSeries builds a snapshot whose breakdown carries one entry per
// map key, with that key's cost in micros.
//
// Deliberately NOT a fully-covered fixture. It carries a handful of pricing gaps
// alongside the priced traffic, because that is the deployment these renderers have
// to be readable on — a rate table that covers most of the traffic and not all of
// it — and because a COVERAGE section with nothing in it would leave the height
// budget with nothing to trade away. Tests that need the clean case build it inline.
//
// Every series entry reports all four token kinds, so WHERE IT WENT has something to
// render; the tests that pin the omit-an-unreported-tier rule set PresentKinds
// themselves.
func pricedSnapshotWithSeries(t *testing.T, costByLabel map[string]int64) *usage.Snapshot {
	t.Helper()
	var totals usage.Counts
	series := map[string]usage.Counts{}
	for label, micros := range costByLabel {
		c := usage.Counts{
			Requests: 1, PriceableRequests: 1, PricedRequests: 1, CostMicros: micros,
			Tokens:      1000,
			InputTokens: 100, CacheReadTokens: 800, CacheWriteTokens: 50, OutputTokens: 50,
			PresentKinds: kindInput | kindCacheRead | kindCacheWrite | kindOutput,
		}
		series[label] = c
		totals.Add(c)
	}
	// The gaps. Named pairs an operator could actually add a rate for, and more of
	// them than one line could hold, which is what the section's cap is for.
	gaps := map[string]int64{
		"gw.example.test model-a": 40,
		"gw.example.test model-b": 32,
		"gw.example.test model-c": 21,
		"gw.example.test model-d": 13,
		"gw.example.test model-e": 8,
		"gw.example.test model-f": 5,
		"gw.example.test model-g": 3,
		"gw.example.test model-h": 2,
	}
	var unpriced int64
	for _, n := range gaps {
		unpriced += n
	}
	totals.Requests += unpriced
	totals.PriceableRequests += unpriced
	return &usage.Snapshot{
		Window:  usage.WindowToday,
		Group:   usage.GroupModel,
		Priced:  true,
		Totals:  totals,
		Buckets: []usage.Bucket{{Counts: totals, Series: series}},
		// The pairs that could not be priced, and the provenance of those that could.
		UnpricedBy: gaps,
		PricedBy:   map[string]int64{"authoritative": int64(len(costByLabel))},
	}
}

// sectionOf returns one section of the rendered pane: its heading line and every
// line up to the next heading.
//
// It exists so a section-scoped assertion cannot accidentally match text from
// another section. WHERE IT WENT is the case that needs it — the rule it enforces is
// "no dollar figure here", and the rest of the pane is full of them, so an
// output-wide assertion would pass or fail on the wrong section's content.
//
// A heading is an unindented non-empty line; every body line the renderers emit is
// indented. That is the same contract the renderers hold to, so a section that
// forgot to indent its rows would surface here rather than silently swallowing the
// next section.
func sectionOf(t *testing.T, rendered, heading string) string {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, heading) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no %q section in:\n%s", heading, rendered)
	}
	out := []string{lines[start]}
	for _, l := range lines[start+1:] {
		if l != "" && !strings.HasPrefix(l, " ") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func TestRenderCostPane_TotalSaysUnavailableRatherThanZero(t *testing.T) {
	snap := &usage.Snapshot{Window: "today", Priced: false,
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10}}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	if !strings.Contains(got, "unavailable") {
		t.Errorf("TOTAL does not say cost is unavailable:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("TOTAL renders $0.00 for an unknown cost:\n%s", got)
	}
}

func TestRenderCostPane_DisclosesAnInexactTotal(t *testing.T) {
	// IncompleteRequests means at least one figure in this total is not exact. A total that
	// hides that reads as exact.
	//
	// IncompleteBy says WHICH WAY, and the fixture carries it because the "higher" assertion
	// below depends on it: output-uncounted is the FLOOR case — a truncated stream priced
	// prompt-only — and it is the only one of the two reasons that has a direction at all.
	// The fixture used to omit it and the assertion still demanded "higher", which pinned a
	// claim the data did not support; see TestRenderCostPane_AnApproximationIsNotCalledAFloor
	// for the case that made it false.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 10, CostMicros: 4_170_000,
			PricedRequests: 10, PriceableRequests: 10, IncompleteRequests: 2},
		IncompleteBy: map[string]int64{"output-uncounted": 2}}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	if !strings.Contains(got, "2") {
		t.Errorf("total does not disclose its 2 inexact figures:\n%s", got)
	}
	// Stronger than the brief's bare "2", which the "$4.1700" figure alone would
	// satisfy: the disclosure has to be a sentence about the two figures, and it has
	// to say which way the total is wrong.
	if !strings.Contains(got, "2 of 10") {
		t.Errorf("total does not count the inexact figures against the priced ones:\n%s", got)
	}
	if !strings.Contains(got, "higher") {
		t.Errorf("total does not say which direction it is wrong in:\n%s", got)
	}
}

func TestRenderCostPane_ExactTotalCarriesNoCaveat(t *testing.T) {
	// The mirror. A permanent caveat with nothing to act on is what teaches an
	// operator to ignore the one that matters.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 10, CostMicros: 4_170_000,
			PricedRequests: 10, PriceableRequests: 10}}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	for _, banned := range []string{"inexact", "floor", "incomplete"} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("exact total carries a %q caveat:\n%s", banned, got)
		}
	}
}

func TestRenderCostPane_BreakdownIsOrderedByCostDescending(t *testing.T) {
	// A table that reshuffles under the cursor is unreadable, and map iteration is
	// nondeterministic. The expensive thing goes first.
	snap := pricedSnapshotWithSeries(t, map[string]int64{
		"cheap": 50, "dear": 3_820_000, "middling": 350_000,
	})
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	iDear, iMid, iCheap := strings.Index(got, "dear"), strings.Index(got, "middling"), strings.Index(got, "cheap")
	if !(iDear < iMid && iMid < iCheap) {
		t.Errorf("rows not ordered by cost descending (dear %d, middling %d, cheap %d):\n%s",
			iDear, iMid, iCheap, got)
	}
}

func TestRenderCostPane_NeverRendersAnEmptySeriesKey(t *testing.T) {
	snap := pricedSnapshotWithSeries(t, map[string]int64{"": 100, "m": 200})
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	lines := strings.Split(got, "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == "$0.0001" {
			t.Errorf("rendered a row for an empty series key:\n%s", got)
		}
	}
	// The brief's assertion above only catches a row that renders as the bare figure.
	// The breakdown is a label column and a figure column, so pin the row count too:
	// one series entry is nameable, the other is not.
	sec := sectionOf(t, got, "BY MODEL")
	if n := strings.Count(sec, "$"); n != 1 {
		t.Errorf("BY MODEL has %d figures, want exactly the one nameable series:\n%s", n, sec)
	}
}

func TestRenderCostPane_WhereItWentIsTokensNotDollars(t *testing.T) {
	// The section deliberately claims tokens, not money. usage.Counts aggregates the
	// four-way TOKEN split and no per-tier dollars at all, and costevent.Event's
	// PromptUSD/OutputUSD are modelled parts that do not reconcile with a gateway's
	// own total — PromptUSD's doc says so in as many words. A dollar figure here
	// would therefore be invented.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1,
			InputTokens: 100, CacheReadTokens: 800, CacheWriteTokens: 50, OutputTokens: 50,
			PresentKinds: 0b1111}}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	sec := sectionOf(t, got, "WHERE IT WENT")
	if strings.Contains(sec, "$") {
		t.Errorf("WHERE IT WENT contains a dollar figure; it reports tokens:\n%s", sec)
	}
	for _, want := range []string{"cache-read", "input", "output"} {
		if !strings.Contains(sec, want) {
			t.Errorf("WHERE IT WENT missing %q:\n%s", want, sec)
		}
	}
}

func TestRenderCostPane_WhereItWentOmitsAnUnreportedTier(t *testing.T) {
	// presentKinds distinguishes "wrote no cache" from "never reports cache". A 0%
	// row for a tier the provider does not expose invents a measurement.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1,
			InputTokens: 100, OutputTokens: 50,
			PresentKinds: 0b1001, // Input|Output only: no cache tiers reported
		}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "WHERE IT WENT")
	if strings.Contains(sec, "cache-read") {
		t.Errorf("rendered a cache-read row for a provider that never reported it:\n%s", sec)
	}
}

func TestRenderCostPane_WhereItWentKeepsAValueWhoseBitIsUnset(t *testing.T) {
	// The mirror of the omit rule, and cmd_cost.go's tokenSplit already holds it: a
	// non-zero count with its bit clear comes from a producer predating PresentKinds,
	// where the value is the only evidence there is. Dropping it would hide a real
	// number in the name of not inventing one.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1,
			InputTokens: 100, CacheReadTokens: 800, OutputTokens: 50,
			PresentKinds: 0, // nothing declared a breakdown
		}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "WHERE IT WENT")
	if !strings.Contains(sec, "cache-read") {
		t.Errorf("dropped a non-zero cache-read count because its bit was clear:\n%s", sec)
	}
	if strings.Contains(sec, "cache-write") {
		t.Errorf("rendered cache-write, which was neither declared nor counted:\n%s", sec)
	}
}

func TestRenderCostPane_WhereItWentSaysSoWhenNothingReportedASplit(t *testing.T) {
	// A gateway reporting only total_tokens: Tokens is non-zero, every split field is
	// zero and PresentKinds is empty. Four 0% rows would assert a measurement that was
	// never made; the section says what happened instead.
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1, Tokens: 4200}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "WHERE IT WENT")
	if strings.Contains(sec, "0.0%") || strings.Contains(sec, "cache-read") {
		t.Errorf("rendered a tier for a provider that reported only a total:\n%s", sec)
	}
	if !strings.Contains(sec, "only a total") {
		t.Errorf("section does not say why it is empty:\n%s", sec)
	}
}

func TestRenderCostPane_CoverageNamesTheGapAndIsSilentWhenThereIsNone(t *testing.T) {
	withGap := &usage.Snapshot{Window: "today", Priced: true,
		Totals:     usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 306, PriceableRequests: 318},
		UnpricedBy: map[string]int64{"api.example.com claude-opus-5": 12}}
	got := renderCostPane(withGap, usage.GroupModel, 120, 40)
	if !strings.Contains(got, "api.example.com") {
		t.Errorf("coverage does not NAME the unpriced pair:\n%s", got)
	}

	clean := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000, PricedRequests: 318, PriceableRequests: 318}}
	if g := renderCostPane(clean, usage.GroupModel, 120, 40); strings.Contains(strings.ToLower(g), "unpriced") {
		t.Errorf("fully priced deployment carries a coverage warning:\n%s", g)
	}
}

func TestRenderCostPane_CoverageSaysWhenThePairsAreUnavailable(t *testing.T) {
	// A ledger-backed window (today, 7d) never carries UnpricedBy: a per-minute row
	// holds only its own labels and cannot tell a priced pair from an unpriced one.
	// The gap is still real and visible in the counters, so the section reports it and
	// says why it cannot name it — rendering nothing would read as "no gaps here".
	snap := &usage.Snapshot{Window: usage.WindowToday, Priced: true,
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 306, PriceableRequests: 318}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "COVERAGE")
	if !strings.Contains(sec, "not reported for this window") {
		t.Errorf("coverage neither names the pairs nor says it cannot:\n%s", sec)
	}
}

func TestRenderCostPane_NoBreakdownForAWindowThatCarriesNone(t *testing.T) {
	// group=session on a ledger-backed window: costledger.Fold returns no series for
	// it, because a ledger row carries no session id. An empty section under a "BY
	// SESSION" heading reads as "nothing cost anything"; say what happened instead.
	snap := &usage.Snapshot{Window: usage.WindowToday, Priced: true, Group: usage.GroupSession,
		Totals:  usage.Counts{Requests: 10, CostMicros: 1_000_000, PricedRequests: 10, PriceableRequests: 10},
		Buckets: []usage.Bucket{{Counts: usage.Counts{Requests: 10}}},
	}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupSession, 120, 40), "BY SESSION")
	if !strings.Contains(sec, "no session breakdown") {
		t.Errorf("an empty breakdown does not say it is empty:\n%s", sec)
	}
}

func TestRenderCostPane_UnpricedSeriesRowSaysSoRatherThanZero(t *testing.T) {
	// A series entry with priceable traffic and no priced request has an UNKNOWN cost,
	// not a zero one. $0.0000 against it would assert that model was free.
	snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 2, CostMicros: 1_000_000, PricedRequests: 1, PriceableRequests: 2},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"priced-model":   {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000},
			"unpriced-model": {Requests: 1, PriceableRequests: 1},
		}}},
	}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "BY MODEL")
	if !strings.Contains(sec, "unpriced-model") {
		t.Errorf("the series entry with no figure was dropped entirely:\n%s", sec)
	}
	if strings.Contains(sec, "$0.0000") {
		t.Errorf("rendered a settled zero for a cost nobody knows:\n%s", sec)
	}
}

func TestRenderCostPane_FitsEveryWidthAndHeight(t *testing.T) {
	// The floor is costWidthFloors, which reaches 16. It used to stop at 64, and the
	// pane's own headings are up to 35 cells — so nothing here could see a heading
	// overflow, and nothing could see the truncCells budget violation either.
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 100, "b": 200, "c": 300})
	for _, w := range costWidthFloors {
		for _, h := range []int{40, 24, 20} {
			got := renderCostPane(snap, usage.GroupModel, w, h)
			for i, line := range strings.Split(got, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("w=%d h=%d line %d is %d columns: %q", w, h, i, lw, line)
				}
			}
		}
	}
}

func TestRenderCostPane_FitsAWideCharacterLabel(t *testing.T) {
	// A model name is workload-chosen and arrives verbatim from the request body, so
	// it can be full-width CJK — two display cells per rune. footer.go records what a
	// rune-indexed budget costs on that input: 55 columns rendered for a 40-column
	// budget. trunc and truncStr are both cell-unaware, which is why neither is used
	// in this file.
	snap := pricedSnapshotWithSeries(t, map[string]int64{
		strings.Repeat("日本語", 20): 3_820_000,
	})
	for _, w := range costWidthFloors {
		got := renderCostPane(snap, usage.GroupModel, w, 40)
		for i, line := range strings.Split(got, "\n") {
			if lw := lipgloss.Width(line); lw > w {
				t.Errorf("w=%d line %d is %d columns: %q", w, i, lw, line)
			}
		}
	}
}

func TestRenderCostPane_SmallTerminalDropsWholeSectionsNotDigits(t *testing.T) {
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000})
	tall := renderCostPane(snap, usage.GroupModel, 120, 40)
	short := renderCostPane(snap, usage.GroupModel, 120, 20)
	if len(short) >= len(tall) {
		t.Fatal("a 20-row terminal rendered no less than a 40-row one; test premise is wrong")
	}
	// Whatever survives, no figure is half-rendered.
	if strings.Contains(short, "$12.5") && !strings.Contains(short, "$12.5000") {
		t.Errorf("a dollar figure was clipped mid-digits:\n%s", short)
	}
	// And the budget is actually respected, not merely smaller.
	if n := len(strings.Split(short, "\n")); n > 20 {
		t.Errorf("a 20-row terminal got %d lines:\n%s", n, short)
	}
	// The total is the one thing that must never be the section that goes. It is the
	// answer to the pane's own question, and the caveats that qualify it ride with it.
	if !strings.Contains(short, "$12.5000") {
		t.Errorf("the total was dropped before the sections below it:\n%s", short)
	}
}

func TestRenderCostPane_TinyHeightStillShowsTheTotal(t *testing.T) {
	// bodyHeight can be small enough that not even the first section fits — a 24-row
	// terminal with the filter open and the strip drawn. Rendering nothing there would
	// make the pane look broken; rendering a clipped "$12.5" would be worse still.
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000})
	for _, h := range []int{1, 2, 3, 4} {
		got := renderCostPane(snap, usage.GroupModel, 120, h)
		if !strings.Contains(got, "$12.5000") {
			t.Errorf("h=%d rendered no total:\n%s", h, got)
		}
		if n := len(strings.Split(got, "\n")); n > h {
			t.Errorf("h=%d rendered %d lines:\n%s", h, n, got)
		}
	}
}

func TestRenderCostPane_NoSnapshotSaysSoWithoutAFigure(t *testing.T) {
	// Before the first reply lands there is no answer, and the pane must not imply one.
	got := renderCostPane(nil, usage.GroupModel, 120, 40)
	if strings.Contains(got, "$") {
		t.Errorf("rendered a dollar figure with no snapshot:\n%s", got)
	}
	if !strings.Contains(got, "COST") {
		t.Errorf("rendered no heading at all:\n%s", got)
	}
}

func TestKey_DollarOpensTheCostPane(t *testing.T) {
	for _, key := range []string{"$", "C"} {
		t.Run(key, func(t *testing.T) {
			m := newTestModelOnPane(t, paneEvents)
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
			if m.pane != paneCost {
				t.Errorf("pane = %v after %q, want paneCost", m.pane, key)
			}
		})
	}
}

func TestKey_EscReturnsToTheOpeningPane(t *testing.T) {
	m := newTestModelOnPane(t, paneEvents)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("$")})
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pane != paneEvents {
		t.Errorf("pane = %v after esc, want the pane it was opened from", m.pane)
	}
}

func TestKey_EscOutOfTheCostPaneEndsItsPollChain(t *testing.T) {
	// The Usage pane's esc arm bumps tickGen on the way out for this reason: a chain
	// left running against a backgrounded pane keeps issuing a request every 20s for
	// the life of the session, and nothing on screen shows the cost of it.
	m := newTestModelOnPane(t, paneEvents)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("$")})
	live := m.costPane.tickGen
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.costTickIsCurrent(live) {
		t.Error("the chain that was live in the pane is still accepted after esc")
	}
}

func TestKey_DollarInsideTheFilterIsLiteralText(t *testing.T) {
	// The events pane's / filter accepts arbitrary text. A bare key check would
	// swallow a literal "$" the user is typing into it and yank them to another
	// pane mid-word. keys.go's column-picker guard is the precedent for scoping a
	// letter key on !m.filtering.
	for _, key := range []string{"$", "C"} {
		t.Run(key, func(t *testing.T) {
			m := newTestModelOnPane(t, paneEvents)
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			if !m.filtering {
				t.Fatal("pressing / did not enter filter mode; test premise is wrong")
			}

			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})

			if m.pane == paneCost {
				t.Errorf("%q opened the Cost pane while the filter was active", key)
			}
			if !strings.Contains(m.filterInput.Value(), key) {
				t.Errorf("filter value = %q, want it to contain the literal %s", m.filterInput.Value(), key)
			}
		})
	}
}

func TestPaneKeys_DocumentsTheCostPane(t *testing.T) {
	// TestPaneKeysCoverAllPanes already fails until paneKeys has an entry, because
	// lastPaneID grows. This asserts the entry says something useful rather than
	// existing to silence that test.
	g, ok := paneKeys[paneCost]
	if !ok {
		t.Fatal("paneKeys has no entry for paneCost")
	}
	var found bool
	for _, k := range g.bindings {
		if strings.Contains(k.keys, "w") || strings.Contains(k.keys, "g") {
			found = true
		}
	}
	if !found {
		t.Error("the Cost pane's help entry documents neither [w]indow nor [g]roup")
	}
}

func TestHelpView_MentionsTheCostPaneKeys(t *testing.T) {
	m := newTestModelOnPane(t, paneCost)
	got := m.helpView()
	for _, want := range []string{"w", "g", "esc"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer for paneCost missing %q: %s", want, got)
		}
	}
	// The bare letters above would match almost any prose, so pin the bracketed forms
	// the rest of this footer uses. A footer that names a key nobody can find in it is
	// the failure TestPaneKeysCoverAllPanes exists for, one layer down.
	for _, want := range []string{"[w]", "[g]", "[esc]"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer for paneCost missing %q: %s", want, got)
		}
	}
}

func TestFooterHints_AdvertiseTheCostKeyWhereItIsPressed(t *testing.T) {
	// A key nobody can discover is a key nobody uses — the same rule
	// TestFooterHintsMentionUsageKey holds `u` to. $ works from every session-view
	// pane, so every one of them has to say so.
	m := newTestModelOnPane(t, paneEvents)
	for _, p := range []paneID{paneSessions, paneEvents, paneDetail} {
		m.pane = p
		if got := m.helpView(); !strings.Contains(got, "[$] cost") {
			t.Errorf("pane %v footer omits the cost key:\n  %s", p, got)
		}
	}
}

func TestPaneView_RendersTheCostPaneWithTheStripAboveIt(t *testing.T) {
	// The strip is on every data pane and the Cost pane is one. This is also the
	// mutation target for Step 5.
	m := newTestModelOnPane(t, paneCost)
	m.spend.snap = pricedSnapshot(t)
	m.costPane.snap = pricedSnapshot(t)

	got := m.paneView()

	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("paneView produced %d lines:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[1], stripLabel) {
		t.Errorf("line 1 is not the spend strip:\n%s", got)
	}
	if !strings.Contains(got, "COST") {
		t.Errorf("the Cost pane body did not render:\n%s", got)
	}
}

func TestPaneView_CostPaneSaysWhenThePollFailed(t *testing.T) {
	// spend.go records what the alternative costs: a broken or absent /v1/usage that
	// renders "" produces no figure, no explanation and no diagnostic on every poll
	// forever, while the row stays reserved. renderCostPane takes only the answer, so
	// the error has to be rendered by the arm that owns the model state.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.err = errors.New("connection refused")

	got := m.paneView()
	if !strings.Contains(got, "connection refused") {
		t.Errorf("a failed poll rendered no diagnostic:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("a failed poll rendered a zero figure:\n%s", got)
	}
}

func TestCostPane_RestoresThePersistedView(t *testing.T) {
	// Both a non-default value and a default-matching one. With only the latter the
	// test passes against a pane that ignores the settings entirely, because the
	// assertion would be satisfied by the default it never read.
	for _, tc := range []struct {
		name   string
		in     CostSettings
		window string
		group  usage.Group
	}{
		{"non-default window", CostSettings{Window: "7d", Group: "endpoint"}, "7d", usage.GroupEndpoint},
		{"default window", CostSettings{Window: "today", Group: "session"}, "today", usage.GroupSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModelOnPane(t, paneEvents)
			Settings.Cost = tc.in

			_ = m.openCostPane()

			if m.costPane.window() != tc.window {
				t.Errorf("window = %q, want the persisted %q", m.costPane.window(), tc.window)
			}
			if m.costPane.group != tc.group {
				t.Errorf("group = %q, want the persisted %q", m.costPane.group, tc.group)
			}
		})
	}
}

func TestCostPane_EmptySettingsUseTheDefaults(t *testing.T) {
	// The deviations-only convention: an empty section is not an empty window. It is
	// what makes a build that changes the default move existing users with it, instead
	// of pinning them to a choice they never made.
	m := newTestModelOnPane(t, paneEvents)
	_ = m.openCostPane()
	if m.costPane.window() == "" {
		t.Error("empty settings produced an empty window rather than the default")
	}
	if m.costPane.group == "" || m.costPane.group == usage.GroupNone {
		t.Errorf("empty settings produced group %q rather than the default", m.costPane.group)
	}
}

func TestCostPane_ADiscardedSettingFallsBackToTheDefault(t *testing.T) {
	// A hand-edited file naming an axis this pane has no series for must not leave the
	// pane on it. costView drops the field; this pins that the pane then uses its
	// default rather than an empty group.
	m := newTestModelOnPane(t, paneEvents)
	Settings.Cost = CostSettings{Window: "6h", Group: "status"}

	_ = m.openCostPane()

	if m.costPane.group != costPaneGroups[0] {
		t.Errorf("group = %q, want the default %q", m.costPane.group, costPaneGroups[0])
	}
	if m.costPane.window() != costPaneWindows[0] {
		t.Errorf("window = %q, want the default %q", m.costPane.window(), costPaneWindows[0])
	}
}

func TestCostPane_CyclingUpdatesTheSettings(t *testing.T) {
	m := newTestModelOnPane(t, paneEvents)
	_ = m.openCostPane()
	before := m.costPane.group
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if m.costPane.group == before {
		t.Fatal("g did not change the group; test premise is wrong")
	}
	if Settings.Cost.Group != string(m.costPane.group) {
		t.Errorf("Settings.Cost.Group = %q, want %q", Settings.Cost.Group, m.costPane.group)
	}

	wBefore := m.costPane.window()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	if m.costPane.window() == wBefore {
		t.Fatal("w did not change the window; test premise is wrong")
	}
	if Settings.Cost.Window != m.costPane.window() {
		t.Errorf("Settings.Cost.Window = %q, want %q", Settings.Cost.Window, m.costPane.window())
	}
}

func TestCostPane_LeavingThePaneWritesTheSettingsOnce(t *testing.T) {
	// Persist on the way OUT, not per keypress, mirroring the column picker: a user
	// cycling three windows to reach the one they want must not produce three writes
	// describing states they rejected. Pinned because it is a POLICY, and the obvious
	// "save where the value changes" refactor would silently reverse it.
	m := newTestModelOnPane(t, paneEvents)
	saved := recordSaves(t, m)
	_ = m.openCostPane()

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if len(*saved) != 0 {
		t.Errorf("cycling wrote %d times before the pane was left", len(*saved))
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(*saved) != 1 {
		t.Fatalf("leaving the pane wrote %d times, want exactly 1", len(*saved))
	}
	if got := (*saved)[0].Cost; got.Window != m.costPane.window() || got.Group != string(m.costPane.group) {
		t.Errorf("wrote %+v, want the view the pane was left on (%q/%q)",
			got, m.costPane.window(), m.costPane.group)
	}
}

func TestRenderCostPane_SumsEveryBucketNotJustTheFirst(t *testing.T) {
	// A RING-backed window (1h) returns one bucket per resolution slice, so a series
	// entry's window total is spread across all of them. Reading Buckets[0] would report
	// the first slice of the window as the whole of it — the bug spend.go's sessionCost
	// comment records, arriving one pane over. Every fixture elsewhere in this file has a
	// single bucket, which is exactly why that mistake would go unnoticed.
	snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 3, PricedRequests: 3, PriceableRequests: 3, CostMicros: 3_000_000},
		Buckets: []usage.Bucket{
			{Series: map[string]usage.Counts{"m": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000}}},
			{Series: map[string]usage.Counts{"m": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000}}},
			{Series: map[string]usage.Counts{"m": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000}}},
		},
	}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "BY MODEL")
	if !strings.Contains(sec, "$3.0000") {
		t.Errorf("the series row does not sum every bucket (want $3.0000):\n%s", sec)
	}
}

func TestRenderCostPane_SharesAreOfThePublishedTotal(t *testing.T) {
	// A percentage of two PUBLISHED figures is presentation. Pinned to the digit,
	// because a share is the one number here a reader will act on without re-deriving,
	// and nothing else in this file would notice a wrong divisor.
	snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 4, PricedRequests: 4, PriceableRequests: 4, CostMicros: 4_000_000,
			InputTokens: 250, CacheReadTokens: 750,
			PresentKinds: kindInput | kindCacheRead},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"dear":  {Requests: 3, PricedRequests: 3, PriceableRequests: 3, CostMicros: 3_000_000},
			"cheap": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000},
		}}},
	}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	by := sectionOf(t, got, "BY MODEL")
	for _, want := range []string{"75.0%", "25.0%"} {
		if !strings.Contains(by, want) {
			t.Errorf("BY MODEL missing the share %q:\n%s", want, by)
		}
	}
	// And the token shares are of the SHOWN TIERS, not of Counts.Tokens — which is not
	// always the sum of the split, so normalising against it would draw shares that do
	// not add up. Tokens is left at zero here, which would make that divisor a no-op.
	tok := sectionOf(t, got, "WHERE IT WENT")
	for _, want := range []string{"75.0%", "25.0%"} {
		if !strings.Contains(tok, want) {
			t.Errorf("WHERE IT WENT missing the share %q:\n%s", want, tok)
		}
	}
}

func TestRenderCostPane_ASurvivingSectionIsWhole(t *testing.T) {
	// The height budget drops sections; it never CLIPS one. A truncated COVERAGE reads
	// as a complete list of gaps, and a truncated breakdown as a complete breakdown —
	// both are wrong answers rather than short ones.
	//
	// Asserted by comparing each surviving section against its unbounded rendering, so
	// any per-line trimming anywhere in the assembler surfaces here rather than looking
	// like a shorter pane.
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000, "b": 900_000, "c": 4})
	tall := renderCostPane(snap, usage.GroupModel, 120, 0)
	headings := []string{"TOTAL", "BY MODEL", costTierHeading, "COVERAGE"}
	for _, h := range []int{16, 18, 20, 24, 30} {
		short := renderCostPane(snap, usage.GroupModel, 120, h)
		var kept int
		for _, heading := range headings {
			if !strings.Contains(short, heading) {
				continue
			}
			kept++
			// Blank lines dropped from both sides: the separators between sections are the
			// FIRST thing the budget gives up, and a section that lost its trailing blank has
			// lost decoration rather than content.
			got, want := withoutBlankLines(sectionOf(t, short, heading)), withoutBlankLines(sectionOf(t, tall, heading))
			if got != want {
				t.Errorf("h=%d section %q was clipped rather than dropped:\ngot:\n%s\nwant:\n%s",
					h, heading, got, want)
			}
		}
		if kept == 0 {
			t.Errorf("h=%d kept no section at all:\n%s", h, short)
		}
	}
}

// withoutBlankLines drops empty lines, so a comparison is about content rather than
// spacing.
func withoutBlankLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestCostPane_InvalidateDisownsAReplyAlreadyInFlight(t *testing.T) {
	// Bumping tickGen alone stops the old chain from SCHEDULING, not the reply already
	// in the air — that reply would still pass the reqSeq guard and land with a fresh
	// timestamp, presenting the previous window's figure as the current one's. spend.go
	// records learning exactly this the hard way, which is why invalidate moves both
	// counters.
	//
	// m.client is nil, so fetchCost issues nothing and bumps nothing: invalidate's own
	// increment is the only thing standing between the old reply and the screen.
	m := &model{}
	inFlight := m.costPane.reqSeq

	_ = m.resumeCostPolling()
	m.applyCostLoaded(costLoadedMsg{req: inFlight, snap: &usage.Snapshot{Window: "1h"}})

	if m.costPane.snap != nil {
		t.Error("a reply issued before the view changed was applied to the new view")
	}
	if !m.costPane.lastFetch.IsZero() {
		t.Error("a discarded reply stamped lastFetch; the pane would report the age of data it threw away")
	}
}

func TestRenderCostPane_FullCoverageRendersNoCoverageSectionAtAll(t *testing.T) {
	// Not merely "no warning text" — no SECTION. A heading with a 0-request line under
	// it is still a permanent block about a problem the deployment does not have, and a
	// reader who learns to skip it will skip the real one too.
	clean := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 318, PriceableRequests: 318}}
	got := renderCostPane(clean, usage.GroupModel, 120, 40)
	if strings.Contains(got, "COVERAGE") {
		t.Errorf("fully priced deployment rendered a COVERAGE section:\n%s", got)
	}
}

func TestRenderCostPane_SanitizesWireDerivedLabels(t *testing.T) {
	// A series key and an UnpricedBy key are both "<endpoint> <model>", and the model
	// half comes from the request body's `model` field — chosen by the workload and
	// recorded verbatim by the parser. Written raw to a TTY, an escape sequence can
	// reposition the cursor, recolour the pane, or erase the very row being reported; a
	// newline alone breaks the table apart. CWE-150, the same rule renderCostSummary
	// already holds to.
	for name, hostile := range map[string]string{
		"ANSI colour":     "gw \x1b[31mclaude-opus-5",
		"cursor move":     "gw \x1b[2Aclaude",
		"newline":         "gw claude\nFAKE TOTAL: $9.9999",
		"carriage return": "gw claude\rerased",
		"NUL":             "gw claude\x00",
		"DEL":             "gw claude\x7f",
	} {
		t.Run(name, func(t *testing.T) {
			snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
				Totals: usage.Counts{Requests: 10, PriceableRequests: 10, PricedRequests: 4, CostMicros: 1_000_000},
				Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
					hostile: {Requests: 4, PricedRequests: 4, PriceableRequests: 4, CostMicros: 1_000_000},
				}}},
				UnpricedBy: map[string]int64{hostile: 6},
			}
			got := renderCostPane(snap, usage.GroupModel, 200, 40)
			for _, bad := range []string{"\x1b", "\r", "\x00", "\x7f"} {
				if strings.Contains(got, bad) {
					t.Errorf("rendered output still carries %q:\n%q", bad, got)
				}
			}
			// A newline cannot be caught by scanning for the byte — the pane is multi-line by
			// design — so the assertion is the ROW COUNT against a benign control of the same
			// length. A smuggled newline would split one row into two and shift everything
			// below it; a control character rendered as U+FFFD leaves the count alone.
			benign := strings.Map(func(r rune) rune {
				if r < 0x20 || r == 0x7f {
					return 'x'
				}
				return r
			}, hostile)
			ctrl := renderCostPane(&usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
				Totals: usage.Counts{Requests: 10, PriceableRequests: 10, PricedRequests: 4, CostMicros: 1_000_000},
				Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
					benign: {Requests: 4, PricedRequests: 4, PriceableRequests: 4, CostMicros: 1_000_000},
				}}},
				UnpricedBy: map[string]int64{benign: 6},
			}, usage.GroupModel, 200, 40)
			if g, w := len(strings.Split(got, "\n")), len(strings.Split(ctrl, "\n")); g != w {
				t.Errorf("hostile label rendered %d rows, benign control %d — a control character moved the layout:\n%s", g, w, got)
			}
			// The label is still recognisable, so a real gap remains actionable.
			if !strings.Contains(got, "claude") {
				t.Errorf("sanitizing destroyed the label:\n%s", got)
			}
		})
	}
}

func TestRenderCostPane_FitsEveryWidthWithEveryCaveatOn(t *testing.T) {
	// The caveat prose is the longest text the pane emits, and the fixtures elsewhere in
	// this file switch most of it off — so a renderer that never wrapped would still fit
	// them. This one turns every line on at once, and gives snap.Window a long value.
	//
	// A long window label is not contrived: the field is free-form on the wire, a
	// ring-answered request comes back as time.Duration.String() ("6h0m0s"), and a proxy
	// is entitled to name a span rather than measure it. The header carries no figure, so
	// it may be truncated — but it may not overflow.
	snap := &usage.Snapshot{
		Window: "since-the-last-deploy-rollout-window-label", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{
			Requests: 900, PriceableRequests: 318, PricedRequests: 300,
			IncompleteRequests: 27, CostMicros: 4_170_000, Tokens: 216_208_900,
			InputTokens: 308_900, CacheReadTokens: 215_900_000, CacheWriteTokens: 1_200_000,
			OutputTokens: 3_100_000, ReasoningTokens: 410_000,
			PresentKinds: kindInput | kindCacheRead | kindCacheWrite | kindOutput | kindReasoning,
		},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"a-model-with-a-genuinely-long-name": {Requests: 300, PricedRequests: 300, PriceableRequests: 300, CostMicros: 4_170_000},
		}}},
		UnpricedBy: map[string]int64{"gateway.internal.example.test a-model-with-a-genuinely-long-name": 18},
		PricedBy:   map[string]int64{"authoritative": 200, "bundled": 60, "configured": 40},
	}
	for _, w := range costWidthFloors {
		for _, h := range []int{60, 40, 24, 20, 16} {
			got := renderCostPane(snap, usage.GroupModel, w, h)
			for i, line := range strings.Split(got, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("w=%d h=%d line %d is %d columns: %q", w, h, i, lw, line)
				}
			}
		}
	}
}

func TestRenderCostPane_RowsInASectionShareTheirColumns(t *testing.T) {
	// Every row of a list section is padded to the same column layout, so the figures sit
	// under one another. That is the whole reason a share column exists: a reader compares
	// them by eye, and a ragged column defeats it. Wide characters are the case that
	// breaks a byte- or rune-counted pad, which is why one label here is full-width.
	//
	// The WIDEST label is deliberately the ASCII one. With the wide label widest, its byte
	// length and its cell count both exceed the column and a byte-counted pad happens to
	// produce the same zero — the two spellings agree and the bug hides. It only shows
	// when a wide label has to be padded OUT to a wider column.
	snap := pricedSnapshotWithSeries(t, map[string]int64{
		"a-really-long-ascii-model-name": 3_820_000,
		"日本語モデル":                         350_000,
		"another-name":                   50,
	})
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 60), "BY MODEL")
	var width = -1
	for _, l := range strings.Split(sec, "\n") {
		// Rows only: the heading and the "+N more" prose are not part of the column grid.
		if !strings.HasPrefix(l, costIndent) || !strings.Contains(l, "$") {
			continue
		}
		if width < 0 {
			width = lipgloss.Width(l)
			continue
		}
		if got := lipgloss.Width(l); got != width {
			t.Errorf("row is %d columns wide, the first row was %d — the columns do not line up:\n%s",
				got, width, sec)
		}
	}
	if width < 0 {
		t.Fatalf("no rows matched; test premise is wrong:\n%s", sec)
	}
}

func TestPaneView_CostPaneDistinguishesLoadingFromNoData(t *testing.T) {
	// Two states with an identical (nil) snapshot and opposite meanings: one resolves on
	// the reply already in flight, the other never will. renderUsage draws the same
	// distinction, and collapsing it here would make a dead poll chain look like a quiet
	// window.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.loading = true
	if got := m.paneView(); !strings.Contains(got, "loading") {
		t.Errorf("a pane with a request in flight does not say so:\n%s", got)
	}

	m.costPane.loading = false
	got := m.paneView()
	if strings.Contains(got, "loading") {
		t.Errorf("a pane with nothing in flight claims to be loading:\n%s", got)
	}
	if !strings.Contains(got, "no data") {
		t.Errorf("a pane with no answer and nothing in flight says nothing:\n%s", got)
	}
}

func TestPaneView_CostPaneReportsTheAgeOfItsFigure(t *testing.T) {
	// The pane refreshes on a timer with no other sign of life, so a figure carrying no
	// age cannot be told from a frozen one — a chain that silently died looks exactly like
	// a quiet hour.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.snap = pricedSnapshot(t)
	m.costPane.lastFetch = time.Now().Add(-4 * time.Second)

	got := m.paneView()
	if !strings.Contains(got, "updated 4s ago") {
		t.Errorf("the pane does not report how old its figure is:\n%s", got)
	}
	if !strings.Contains(got, "every 20s") {
		t.Errorf("the pane does not say how often it refreshes:\n%s", got)
	}
}

func TestPaneView_CostPaneDropsTheAgeLineBeforeASection(t *testing.T) {
	// The age is worth strictly less than any section, so it takes a row only when one is
	// spare — the same priority the height budget gives the separators. A terminal that
	// cannot fit both must show the money.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.snap = pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000})
	m.costPane.lastFetch = time.Now().Add(-4 * time.Second)

	m.width, m.height = 120, 40
	m.layout()
	tall := m.paneView()
	if !strings.Contains(tall, "updated 4s ago") {
		t.Fatalf("the age line is absent with room to spare; test premise is wrong:\n%s", tall)
	}

	// Squeeze the body to exactly what the sections need, leaving no spare row.
	m.costPane.lastFetch = time.Now().Add(-4 * time.Second)
	m.bodyHeight = len(strings.Split(renderCostPane(m.costPane.snap, m.costPane.group, m.width, 0), "\n"))
	if got := m.paneView(); strings.Contains(got, "updated") {
		t.Errorf("the age line took a row the sections needed:\n%s", got)
	}
}

func TestCostPane_AnAcceptedReplyStampsItsAgeAndClearsLoading(t *testing.T) {
	// The other half of "lastFetch moves only on an ACCEPTED reply": the stale-reply tests
	// pin that a discarded one leaves it alone, and without this one a renderer could
	// report an age nothing ever set — which reads as a figure that never refreshes.
	m := newTestModelOnPane(t, paneCost)
	m.costPane.loading = true

	m.applyCostLoaded(costLoadedMsg{req: m.costPane.reqSeq, snap: pricedSnapshot(t)})

	if m.costPane.lastFetch.IsZero() {
		t.Error("an accepted reply did not stamp lastFetch")
	}
	if m.costPane.loading {
		t.Error("an accepted reply left the pane claiming a request is still in flight")
	}
	if got := m.paneView(); !strings.Contains(got, "updated") {
		t.Errorf("the pane reports no age after a reply landed:\n%s", got)
	}
}

// TestCostPaneGroups_IncludesAgentLast pins both the membership and the position.
//
// Membership, because the agent axis is the one this pane was asked for. Position,
// because agent is the only axis whose rows are SPOOFABLE — a User-Agent is
// self-reported — so it must not be what a reader is shown first.
func TestCostPaneGroups_IncludesAgentLast(t *testing.T) {
	if len(costPaneGroups) == 0 || costPaneGroups[len(costPaneGroups)-1] != usage.GroupAgent {
		t.Errorf("costPaneGroups = %v, want GroupAgent last", costPaneGroups)
	}
	for _, g := range costPaneGroups {
		if g == usage.GroupNone {
			t.Error("GroupNone is in the cycle; an ungrouped view is the spend strip one row above")
		}
	}
}

// TestCostSeriesLabel_ReservedAgentBucketIsNotRenderedAsAName is the consumer half of
// pipeline.UnknownClientLabel's contract, which says in terms that a consumer must not
// present it as an agent name. This pane IS that consumer, so the rule is only real if
// something here enforces it.
func TestCostSeriesLabel_ReservedAgentBucketIsNotRenderedAsAName(t *testing.T) {
	got := costSeriesLabel(pipeline.UnknownClientLabel, usage.GroupAgent)
	if got == pipeline.UnknownClientLabel {
		t.Errorf("group=agent rendered the reserved bucket verbatim as %q; it would sit in the same "+
			"column as claude-code/2.1.14 and read as a program by that name that spent real money", got)
	}
	if got != costUnattributedLabel {
		t.Errorf("costSeriesLabel = %q, want %q", got, costUnattributedLabel)
	}
	// Only that axis. "unknown" is a legitimate value on the others — a model or an
	// endpoint could be named it — and rewriting it there would hide a real key.
	for _, g := range []usage.Group{usage.GroupModel, usage.GroupEndpoint, usage.GroupSession} {
		if got := costSeriesLabel(pipeline.UnknownClientLabel, g); got != pipeline.UnknownClientLabel {
			t.Errorf("group=%s rewrote a real key %q to %q", g, pipeline.UnknownClientLabel, got)
		}
	}
}

// costWidthFloors are the widths every layout test sweeps.
//
// Down to 16, not 40. The bottom of this list used to be 40, and 40 is wider than
// costTierHeading's 35 display cells — so the headings, which fitCostSections appended
// RAW, could overflow at every width below 35 with no test able to see it. A pane whose
// narrowest tested terminal is wider than its longest fixed string is not width-tested.
var costWidthFloors = []int{200, 120, 100, 80, 64, 48, 40, 34, 32, 30, 24, 20, 16}

// costHostileWidths are the widths the emoji cases panicked at, plus the sweep.
//
// 21-26 for "⚠️-model", 28-35 for "gpt-4o ▶️ preview", 21-42 for twelve "❤️" — each is a
// window where truncCells' per-rune sum under-charged a cluster by exactly enough to
// push renderCostRows' pad negative. Enumerated rather than sampled, because the bug was
// invisible at 40 and at 120 and lived only in between.
func costHostileWidths() []int {
	var out []int
	for w := 16; w <= 48; w++ {
		out = append(out, w)
	}
	return out
}

// TestTruncCells_NeverExceedsItsBudget is the unit-level half of the crash.
//
// The caller measures the RESULT with lipgloss.Width. So must this: truncCells used to
// measure the string it was BUILDING one rune at a time, and for any cluster wider than
// the sum of its runes — Width("⚠️") is 2, Width("⚠")+Width("️") is 1+0 — that sum
// under-charged and the result came back up to twice its budget. Twelve "❤️" truncated
// to 5 returned 9 cells.
func TestTruncCells_NeverExceedsItsBudget(t *testing.T) {
	for name, s := range map[string]string{
		"variation selector 16":  "⚠️-model",
		"play button":            "gpt-4o ▶️ preview",
		"repeated heart":         strings.Repeat("❤️", 12),
		"combining acute accent": "mode\u0301l-cafe\u0301-nai\u0308ve",
		"CJK":                    strings.Repeat("日本語", 12),
		"CJK and emoji":          "日本語-⚠️-モデル-❤️",
		"ZWJ family":             "family-👨‍👩‍👧-model",
		"plain ASCII":            "a-perfectly-ordinary-model-name",
	} {
		t.Run(name, func(t *testing.T) {
			for n := 1; n <= 48; n++ {
				got := truncCells(s, n)
				if w := lipgloss.Width(got); w > n {
					t.Errorf("truncCells(%q, %d) is %d cells: %q", s, n, w, got)
				}
			}
			if got := truncCells(s, 0); got != "" {
				t.Errorf("truncCells(%q, 0) = %q, want the empty string", s, got)
			}
		})
	}
}

// TestTruncCells_KeepsWhatFits is the mirror: the self-correcting loop must not shrink a
// label that already fitted, or every column in the pane silently loses a cell.
func TestTruncCells_KeepsWhatFits(t *testing.T) {
	for _, s := range []string{"input", "日本語", "⚠️", strings.Repeat("❤️", 3)} {
		w := lipgloss.Width(s)
		if got := truncCells(s, w); got != s {
			t.Errorf("truncCells(%q, %d) = %q, want it untouched", s, w, got)
		}
		if got := truncCells(s, w+10); got != s {
			t.Errorf("truncCells(%q, %d) = %q, want it untouched", s, w+10, got)
		}
	}
}

// TestRenderCostPane_DoesNotPanicOnAnEmojiModelName is the reachable crash, driven
// through the whole pane on wire-derived labels.
//
// A model name is workload-chosen and recorded verbatim, so "⚠️-model" is a name a
// workload can simply pick. renderCostRows padded with
// strings.Repeat(" ", labelW-Width(lbl)); an over-budget label made that count negative
// and strings.Repeat panicked — inside View(), which is fatal in bubbletea and leaves the
// terminal in alt-screen. The same call serves UnpricedBy, so a coverage-gap pair reached
// it too, which is why both are populated here.
func TestRenderCostPane_DoesNotPanicOnAnEmojiModelName(t *testing.T) {
	for name, label := range map[string]string{
		"variation selector 16": "⚠️-model",
		"play button":           "gpt-4o ▶️ preview",
		"repeated heart":        strings.Repeat("❤️", 12),
		"combining accent":      "mode\u0301l-cafe\u0301",
		"CJK":                   strings.Repeat("日本語", 12),
	} {
		t.Run(name, func(t *testing.T) {
			snap := &usage.Snapshot{Window: "1h", Priced: true, Group: usage.GroupModel,
				Totals: usage.Counts{Requests: 10, PriceableRequests: 10, PricedRequests: 4,
					CostMicros: 1_000_000, InputTokens: 100, OutputTokens: 50,
					PresentKinds: kindInput | kindOutput},
				Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
					label: {Requests: 4, PricedRequests: 4, PriceableRequests: 4, CostMicros: 1_000_000},
				}}},
				UnpricedBy: map[string]int64{"gw.example.test " + label: 6},
			}
			for _, w := range costHostileWidths() {
				for _, h := range []int{40, 20, 0} {
					// A panic here is the defect: View() dying takes abctl with it.
					got := renderCostPane(snap, usage.GroupModel, w, h)
					for i, line := range strings.Split(got, "\n") {
						if lw := lipgloss.Width(line); lw > w {
							t.Fatalf("w=%d h=%d line %d is %d columns: %q", w, h, i, lw, line)
						}
					}
				}
			}
		})
	}
}

// TestRenderCostPane_HeadingsAreWidthFitted pins the heading budget on PURE ASCII, so it
// cannot be mistaken for another spelling of the emoji case.
//
// costTierHeading is 35 display cells. fitCostSections appended it raw, so it overflowed
// at every width below 35 — 20, 24, 30, 32 and 34 all reproduced. Every other line in the
// pane goes through renderCostRows or wrapCells; the heading was the one that did not.
func TestRenderCostPane_HeadingsAreWidthFitted(t *testing.T) {
	if lipgloss.Width(costTierHeading) <= 34 {
		t.Fatalf("costTierHeading is %d cells; this test needs it wider than the widths it sweeps",
			lipgloss.Width(costTierHeading))
	}
	snap := pricedSnapshotWithSeries(t, map[string]int64{"a": 12_500_000, "b": 900_000})
	for _, w := range []int{16, 20, 24, 30, 32, 34} {
		got := renderCostPane(snap, usage.GroupModel, w, 0)
		for i, line := range strings.Split(got, "\n") {
			if lw := lipgloss.Width(line); lw > w {
				t.Errorf("w=%d line %d is %d columns: %q", w, i, lw, line)
			}
		}
		// And a heading that had to be cut still says it was cut, rather than reading as a
		// shorter heading that means something else.
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "WHERE IT WENT") && lipgloss.Width(line) < lipgloss.Width(costTierHeading) &&
				!strings.HasSuffix(line, "…") {
				t.Errorf("w=%d the tier heading was shortened without saying so: %q", w, line)
			}
		}
	}
}

// TestPaneView_CostPaneErrorRespectsTheHeightBudget: the error branch was the one body
// path in this pane with no height cap.
//
// A server-authored body — a 502 HTML page, an upstream-connect-error chain — wrapped to
// 42 lines in a 20-row body at w=40. paneView joins without clipping, so the frame grew
// past the terminal, the footer went off the bottom and the terminal scrolled. Every
// other path here respects m.bodyHeight.
func TestPaneView_CostPaneErrorRespectsTheHeightBudget(t *testing.T) {
	long := "upstream connect error or disconnect/reset before headers: " +
		strings.Repeat("reset reason: connection failure, transport failure reason: "+
			"delayed connect error: 111; ", 12)
	for _, h := range []int{20, 8, 4, 2, 1} {
		m := newTestModelOnPane(t, paneCost)
		m.width, m.height = 40, h+3
		m.layout()
		m.bodyHeight = h
		m.costPane.err = errors.New(long)

		got := m.renderCostBody()
		lines := strings.Split(got, "\n")
		if len(lines) > h {
			t.Errorf("h=%d: the error body is %d lines:\n%s", h, len(lines), got)
		}
		// It must still SAY what went wrong — a cap that swallowed the diagnostic would be
		// the failure spend.go records, arriving from the other direction.
		if !strings.Contains(got, "COST") {
			t.Errorf("h=%d: the capped error body no longer says the pane is unavailable:\n%s", h, got)
		}
		// And a cut body must admit it was cut, or a clipped diagnostic reads as a complete
		// one. One row buys the first line rather than the admission.
		if h > 1 && !strings.Contains(got, costTruncated) {
			t.Errorf("h=%d: the error body was cut without saying so:\n%s", h, got)
		}
		for i, l := range lines {
			if lw := lipgloss.Width(l); lw > m.width {
				t.Errorf("h=%d line %d is %d columns, terminal is %d: %q", h, i, lw, m.width, l)
			}
		}
	}
}

// TestRenderCostPane_ASectionWithNoRowsIsDroppedWhole.
//
// renderCostRows returns nil when even a one-cell label cannot sit beside the figures,
// and the "+N more" note was appended regardless. At w=20 with ten series that produced
// "BY MODEL" followed only by "+2 more, each smaller than the last row" — naming a last
// row that is not on screen, and reporting 2 elided when all 10 were lost. COVERAGE had
// the same shape.
func TestRenderCostPane_ASectionWithNoRowsIsDroppedWhole(t *testing.T) {
	costs := map[string]int64{}
	for i := 0; i < 10; i++ {
		costs[fmt.Sprintf("model-%d-with-a-long-name", i)] = int64(10_000 * (i + 1))
	}
	snap := pricedSnapshotWithSeries(t, costs)
	// The SECTION builders directly, not the assembled pane. At a width this narrow
	// wrapCells breaks the elision note across three lines, so scanning the rendered pane
	// for the whole sentence finds nothing whether the bug is present or not — the test
	// would pass for the wrong reason. Each section's own lines are unambiguous.
	//
	// A ROW is identified by its RIGHT column, which is the one thing prose never carries
	// and the one thing renderCostRows never truncates: costMoney's "$" for the breakdown,
	// the "x<count>" request tally for COVERAGE. The LABEL is no good as a marker — it is
	// truncated to a bare "…" at these widths, which is precisely the state under test.
	rowMarker := regexp.MustCompile(`\$|x[0-9]`)
	for _, w := range []int{1, 4, 8, 12, 16, 18, 20} {
		for _, tc := range []struct {
			name string
			sec  costSection
		}{
			{"BY MODEL", costBreakdownSection(snap, usage.GroupModel, w)},
			{"COVERAGE", costCoverageSection(snap, w)},
		} {
			if len(tc.sec.lines) == 0 {
				continue // dropped whole, which is the correct answer
			}
			var rows int
			for _, l := range tc.sec.lines {
				if rowMarker.MatchString(l) {
					rows++
				}
			}
			if rows == 0 {
				t.Errorf("w=%d: %s kept its heading and %d lines of prose with no data row at all: %q",
					w, tc.name, len(tc.sec.lines), tc.sec.lines)
			}
		}
	}
	// And at a width where rows DO fit, the note is still disclosed — the fix must not have
	// bought its correctness by dropping the elision note outright.
	wide := costBreakdownSection(snap, usage.GroupModel, 120)
	if !strings.Contains(strings.Join(wide.lines, "\n"), "more, each smaller than the last row") {
		t.Errorf("ten series over a cap of %d disclosed no elision at all:\n%s",
			costMaxSeriesRows, strings.Join(wide.lines, "\n"))
	}
}

// TestCostBar_NaNDrawsNothingRatherThanPanicking.
//
// NaN fails BOTH of the range comparisons, so it used to reach int(frac*n+0.5) intact.
// That conversion is implementation-defined: 0 on arm64, -2^63 on amd64 — where
// strings.Repeat panics. An architecture-dependent crash is the worst kind to leave in,
// because it does not reproduce on the machine it was written on.
func TestCostBar_NaNDrawsNothingRatherThanPanicking(t *testing.T) {
	// costFrac first, and it is the assertion that actually PINS the guard. Checked
	// through the returned number rather than through costBar's string, because on arm64
	// int(NaN) lands on 0 and the string is identical with or without the guard — so a
	// test that only read costBar's output could not fail on this machine no matter what
	// was deleted, and the crash lives on amd64.
	for name, in := range map[string]float64{
		"NaN":  math.NaN(),
		"-Inf": math.Inf(-1),
		"+Inf": math.Inf(1),
		"-0.5": -0.5,
		"1.5":  1.5,
	} {
		got := costFrac(in)
		if got != got {
			t.Errorf("costFrac(%s) returned NaN; int() of it is 0 on arm64 and -2^63 on amd64, "+
				"where strings.Repeat panics inside View()", name)
		}
		if got < 0 || got > 1 {
			t.Errorf("costFrac(%s) = %v, want it clamped into [0,1]", name, got)
		}
	}
	if got := costFrac(0.25); got != 0.25 {
		t.Errorf("costFrac(0.25) = %v, want it untouched", got)
	}
	for _, n := range []int{1, 6, 28} {
		got := costBar(math.NaN(), true, n)
		if lipgloss.Width(got) != n {
			t.Errorf("costBar(NaN, true, %d) is %d cells, want exactly %d: %q",
				n, lipgloss.Width(got), n, got)
		}
		if strings.Contains(got, "█") {
			t.Errorf("costBar(NaN, true, %d) drew a share for a figure that is not a number: %q", n, got)
		}
	}
	if got := costBar(math.Inf(-1), true, 10); lipgloss.Width(got) != 10 {
		t.Errorf("costBar(-Inf, true, 10) is %d cells: %q", lipgloss.Width(got), got)
	}
	if got := costBar(math.Inf(1), true, 10); lipgloss.Width(got) != 10 {
		t.Errorf("costBar(+Inf, true, 10) is %d cells: %q", lipgloss.Width(got), got)
	}
}

// TestRenderCostPane_SanitizesTheProvenanceKeys.
//
// snap.PricedBy was the one wire-derived string the pane wrote out raw: snap.Window, the
// series labels, the UnpricedBy keys and the error text all went through sanitizeLabel.
// A key carrying ESC recolours the pane (CWE-150) and a key carrying a newline both blows
// the height budget and emits an UNINDENTED body line, which breaks the "every heading is
// unindented" contract the section logic reads.
func TestRenderCostPane_SanitizesTheProvenanceKeys(t *testing.T) {
	hostile := &usage.Snapshot{Window: "1h", Priced: true,
		Totals:   usage.Counts{Requests: 10, PricedRequests: 10, PriceableRequests: 10, CostMicros: 4_170_000},
		PricedBy: map[string]int64{"bun\ndled\x1b[31m": 6, "authoritative": 4}}
	benign := &usage.Snapshot{Window: "1h", Priced: true,
		Totals:   usage.Counts{Requests: 10, PricedRequests: 10, PriceableRequests: 10, CostMicros: 4_170_000},
		PricedBy: map[string]int64{"bunxdledxx[31m": 6, "authoritative": 4}}

	got := renderCostPane(hostile, usage.GroupModel, 120, 40)
	for _, bad := range []string{"\x1b", "\r", "\x00", "\x7f"} {
		if strings.Contains(got, bad) {
			t.Errorf("the provenance note still carries %q:\n%q", bad, got)
		}
	}
	ctrl := renderCostPane(benign, usage.GroupModel, 120, 40)
	if g, w := len(strings.Split(got, "\n")), len(strings.Split(ctrl, "\n")); g != w {
		t.Errorf("a hostile provenance key rendered %d rows against the benign control's %d — "+
			"a smuggled newline moved the layout:\n%s", g, w, got)
	}
	// Every body line stays indented, which is the contract the section split depends on:
	// sectionOf reads exactly this, so an unindented line no renderer authored would
	// silently swallow the section below it.
	headings := map[string]bool{"TOTAL": true, "BY MODEL": true, costTierHeading: true, "COVERAGE": true}
	for i, l := range strings.Split(got, "\n") {
		if i == 0 || l == "" || strings.HasPrefix(l, costIndent) || headings[l] {
			continue
		}
		t.Errorf("line %d is unindented and is not one of this pane's headings — a newline in "+
			"the provenance note broke the section contract: %q", i, l)
	}
}

// TestKey_ReturningFromTheCatalogRestartsTheCostChain is `$` `P` `esc`.
//
// The catalog-return arm resumed ONLY paneUsage while resumeCostPolling's comment claimed
// the two entry points could not drift. They had: the tick in flight when the catalog
// opened was dropped by costTickMsg's `m.pane != paneCost` guard, nothing rescheduled, and
// the pane never refreshed again while its freshness line went on counting up.
func TestKey_ReturningFromTheCatalogRestartsTheCostChain(t *testing.T) {
	m := newTestModelOnPane(t, paneEvents)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("$")})
	if m.pane != paneCost {
		t.Fatalf("pane = %v after $; test premise is wrong", m.pane)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("P")})
	if m.pane != paneCatalog {
		t.Fatalf("pane = %v after P; test premise is wrong", m.pane)
	}
	dead := m.costPane.tickGen

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if m.pane != paneCost {
		t.Fatalf("pane = %v after esc, want paneCost", m.pane)
	}
	if cmd == nil {
		t.Fatal("returning to the Cost pane scheduled nothing; its 20s refresh is dead")
	}
	if m.costPane.tickGen == dead {
		t.Error("tickGen unchanged on the way back in; no new chain was started")
	}
	if m.costTickIsCurrent(dead) {
		t.Error("the dropped generation is still accepted; the guard and the resume disagree")
	}
}

// TestKey_LeavingTheCostPaneRestartsTheUsageChain is `u` `$` `esc`, and it is a
// REGRESSION to an existing pane rather than a gap in a new one.
//
// Adding paneCost to the `$` opener list made paneUsage one of the panes it can be opened
// FROM. The Usage tick in flight then fell to usageTickMsg's own `m.pane != paneUsage`
// guard, and esc landed back on a Usage pane whose 20s refresh was dead — bit for bit the
// failure the catalog-return arm exists to prevent, arriving through a different door.
func TestKey_LeavingTheCostPaneRestartsTheUsageChain(t *testing.T) {
	m := newTestModelOnPane(t, paneSessions)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	if m.pane != paneUsage {
		t.Fatalf("pane = %v after u; test premise is wrong", m.pane)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("$")})
	if m.pane != paneCost {
		t.Fatalf("pane = %v after $; test premise is wrong", m.pane)
	}
	dead := m.usage.tickGen

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if m.pane != paneUsage {
		t.Fatalf("pane = %v after esc, want paneUsage", m.pane)
	}
	if cmd == nil {
		t.Fatal("returning to the Usage pane scheduled nothing; its 20s refresh is dead")
	}
	if m.usage.tickGen == dead {
		t.Error("usage tickGen unchanged on the way back in; no new chain was started")
	}
	// And the Cost pane's own chain still ends on the way out, which is the rule the esc
	// arm already held: a chain against a backgrounded pane polls forever for nothing.
	if m.costTickIsCurrent(m.costPane.tickGen - 1) {
		t.Error("a Cost generation from inside the pane is still accepted after esc")
	}
}

// TestKey_ReturningFromTheCatalogStillRestartsTheUsageChain guards the arm that already
// worked, because the fix rewrote it into a switch.
func TestKey_ReturningFromTheCatalogStillRestartsTheUsageChain(t *testing.T) {
	m := newTestModelOnPane(t, paneSessions)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("P")})
	if m.pane != paneCatalog {
		t.Fatalf("pane = %v after P; test premise is wrong", m.pane)
	}
	dead := m.usage.tickGen

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if m.pane != paneUsage {
		t.Fatalf("pane = %v after esc, want paneUsage", m.pane)
	}
	if cmd == nil || m.usage.tickGen == dead {
		t.Error("returning to the Usage pane from the catalog started no chain")
	}
}

// TestCostMoney_ASubFloorCostIsNotRenderedAsFree.
//
// Four decimals only MOVE the threshold — from $0.005 to $0.00005 — they do not remove
// it, and costMoney's own doc argued the four places exist so a real cost is never shown
// as free. Every figure under 50 micros printed "$0.0000". 30 micros is an ordinary
// cache-read-only turn: 100 cache-read tokens at $0.30/MTok.
//
// The exact zero keeps "$0.0000", and that is the point of the split rather than a
// leftover: a settled zero is LEGITIMATELY priced — the producer means "this call was
// free" — so conflating it with "too small to state" is the one thing this whole surface
// exists to refuse.
func TestCostMoney_ASubFloorCostIsNotRenderedAsFree(t *testing.T) {
	for _, tc := range []struct {
		micros int64
		want   string
	}{
		{0, "$0.0000"},
		{1, "<$0.0001"},
		{30, "<$0.0001"},
		{49, "<$0.0001"},
		{50, "$0.0001"},
		{100, "$0.0001"},
		{1_120_000, "$1.1200"},
	} {
		if got := costMoney(tc.micros); got != tc.want {
			t.Errorf("costMoney(%d) = %q, want %q", tc.micros, got, tc.want)
		}
	}
	// The spelling matches the sibling formatter rather than inventing a second one.
	if got, want := costMoney(30), formatUSDCell(0.00003); got != want {
		t.Errorf("costMoney says %q where formatUSDCell says %q; two spellings for one fact", got, want)
	}
}

// TestRenderCostPane_ASubFloorTotalIsNotRenderedAsFree drives the same figure through the
// pane, because the reviewer saw it in TWO places at once: the TOTAL read "$0.0000" and
// the breakdown row read "$0.0000" beside a FULL bar and 100.0%.
func TestRenderCostPane_ASubFloorTotalIsNotRenderedAsFree(t *testing.T) {
	for _, micros := range []int64{1, 30, 49} {
		snap := &usage.Snapshot{Window: "today", Priced: true, Group: usage.GroupModel,
			Totals: usage.Counts{Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: micros},
			Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
				"claude-opus-5": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: micros},
			}}},
		}
		got := renderCostPane(snap, usage.GroupModel, 120, 40)
		if strings.Contains(got, "$0.0000") {
			t.Errorf("%d micros rendered as a settled zero:\n%s", micros, got)
		}
		if !strings.Contains(got, "<$0.0001") {
			t.Errorf("%d micros did not say it is below the smallest figure four places can state:\n%s",
				micros, got)
		}
		// And the row still says 100.0% of the total, which it genuinely is — the share was
		// never the wrong part.
		if !strings.Contains(sectionOf(t, got, "BY MODEL"), "100.0%") {
			t.Errorf("%d micros lost its share:\n%s", micros, got)
		}
	}
	// A settled zero is still a settled zero.
	zero := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 0}}
	if got := renderCostPane(zero, usage.GroupModel, 120, 40); !strings.Contains(got, "$0.0000") {
		t.Errorf("an exact zero no longer renders as one; free and too-small now read alike:\n%s", got)
	}
}

// TestCostShare_RefusesAPairThatCannotBothBeRight.
//
// The zero-denominator guard had no test at all: weakening `whole <= 0` to `whole < 0`
// survived the entire suite and put "NaN%" in the breakdown, the token tiers and a caveat
// line at once. The other two refusals are the pane restating a guarantee the server
// makes and this side never checked.
func TestCostShare_RefusesAPairThatCannotBothBeRight(t *testing.T) {
	for _, tc := range []struct {
		part, whole int64
		want        string
	}{
		{1, 0, ""},     // divide by zero: "NaN%" / "+Inf%"
		{0, 0, ""},     //
		{1, -5, ""},    // a negative whole is not a whole
		{-1, 10, ""},   // a negative share
		{200, 100, ""}, // above 100%, which the bar has already clamped
		{25, 100, "25.0%"},
		{100, 100, "100.0%"},
	} {
		if got := costShare(tc.part, tc.whole); got != tc.want {
			t.Errorf("costShare(%d, %d) = %q, want %q", tc.part, tc.whole, got, tc.want)
		}
	}
}

// TestRenderCostPane_NeverRendersNaNOrInf is the pane-level half: a priced snapshot whose
// total is zero makes every denominator in the pane zero at once.
func TestRenderCostPane_NeverRendersNaNOrInf(t *testing.T) {
	snap := &usage.Snapshot{Window: "today", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 2, PricedRequests: 2, PriceableRequests: 2, CostMicros: 0},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"a": {Requests: 1, PricedRequests: 1, PriceableRequests: 1},
			"b": {Requests: 1, PricedRequests: 1, PriceableRequests: 1},
		}}},
	}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	for _, bad := range []string{"NaN", "Inf"} {
		if strings.Contains(got, bad) {
			t.Errorf("rendered %q:\n%s", bad, got)
		}
	}
}

// TestCostBar_APresentButTinyShareGetsACell.
//
// Rounding 0.4% of 28 cells to nothing draws a present-but-tiny series identically to an
// absent one, and those mean opposite things — an absent share is UNKNOWN. The guard was
// there; disabling it survived the whole suite.
func TestCostBar_APresentButTinyShareGetsACell(t *testing.T) {
	for _, frac := range []float64{0.004, 0.0001, 1e-9} {
		got := costBar(frac, true, 28)
		if !strings.Contains(got, "█") {
			t.Errorf("costBar(%g, true, 28) drew a blank bar, which is what an ABSENT share draws: %q",
				frac, got)
		}
		if lipgloss.Width(got) != 28 {
			t.Errorf("costBar(%g, true, 28) is %d cells: %q", frac, lipgloss.Width(got), got)
		}
	}
	// And an exact zero is still empty: 0% is a share, and it is not one cell of spend.
	if got := costBar(0, true, 28); strings.Contains(got, "█") {
		t.Errorf("costBar(0, true, 28) drew a cell for no spend at all: %q", got)
	}
	// An ABSENT share stays blank, which is the distinction the guard protects.
	if got := costBar(0.5, false, 28); strings.Contains(got, "█") {
		t.Errorf("costBar drew a bar for an unknown share: %q", got)
	}
}

// TestRenderCostPane_WhereItWentKeepsADeclaredButZeroTier is the missing half of the
// PresentKinds rule.
//
// TestRenderCostPane_WhereItWentOmitsAnUnreportedTier pins undeclared-and-zero out, and
// KeepsAValueWhoseBitIsUnset pins counted-but-undeclared in. Declared-AND-zero — "this
// traffic wrote no cache", which is a real measurement — had nothing holding it, and
// dropping the PresentKinds half of the condition survived the suite.
func TestRenderCostPane_WhereItWentKeepsADeclaredButZeroTier(t *testing.T) {
	snap := &usage.Snapshot{Window: "today", Priced: true,
		Totals: usage.Counts{Requests: 1, CostMicros: 1_000_000,
			PricedRequests: 1, PriceableRequests: 1,
			InputTokens: 100, OutputTokens: 50,
			// Declared: all four. Counted: two. The two zeros are an ANSWER.
			CacheReadTokens: 0, CacheWriteTokens: 0,
			PresentKinds: kindInput | kindCacheRead | kindCacheWrite | kindOutput,
		}}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "WHERE IT WENT")
	for _, want := range []string{"cache-read", "cache-write"} {
		if !strings.Contains(sec, want) {
			t.Errorf("dropped %q although PresentKinds declared it — reported-as-zero and "+
				"never-reported are different answers, and that distinction is what PresentKinds "+
				"is for:\n%s", want, sec)
		}
	}
}

// TestRenderCostPane_ANegativeFigureIsUnpricedNotACredit.
//
// The server refuses a negative cost, so the pane was inheriting a guarantee it never
// restated — and "$-5.0000" in a column of costs reads as a refund nobody issued.
func TestRenderCostPane_ANegativeFigureIsUnpricedNotACredit(t *testing.T) {
	snap := &usage.Snapshot{Window: "today", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: -5_000_000},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: -5_000_000},
		}}},
	}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	if strings.Contains(got, "$-") {
		t.Errorf("rendered a negative dollar figure:\n%s", got)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("an impossible figure was neither shown nor declined:\n%s", got)
	}
}

// TestRenderCostPane_AShareAboveOneHundredIsOmittedRatherThanContradictingItsBar.
//
// costBar clamps to 100%, so a part exceeding the whole put "200.0%" beside a full bar and
// left a reader no way to tell which of the two was the lie. The dollar figure stays; only
// the comparison goes, which is the priority order the pane holds to everywhere.
func TestRenderCostPane_AShareAboveOneHundredIsOmittedRatherThanContradictingItsBar(t *testing.T) {
	snap := &usage.Snapshot{Window: "today", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 1_000_000},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 1, PricedRequests: 1, PriceableRequests: 1, CostMicros: 2_000_000},
		}}},
	}
	sec := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "BY MODEL")
	if strings.Contains(sec, "200.0%") {
		t.Errorf("a share above 100%% is rendered beside a bar clamped to 100%%:\n%s", sec)
	}
	// The figure is still there: an inconsistent pair costs the comparison, not the money.
	if !strings.Contains(sec, "$2.0000") {
		t.Errorf("the row's own figure went with the share:\n%s", sec)
	}
}

// TestCostPane_CyclingTheAxisDropsTheSnapshotItWasFetchedFor is the first of two named
// historical bugs on this branch that had no regression test — removing
// `m.costPane.snap = nil` from beginCostFetch survived the whole suite.
//
// invalidate()'s comment names the failure: the previous axis's series rendering under the
// NEW heading, a group=model payload sitting under "BY ENDPOINT". It records it as
// something another pane learned the hard way, which is exactly the kind of claim that
// needs a test rather than a paragraph.
func TestCostPane_CyclingTheAxisDropsTheSnapshotItWasFetchedFor(t *testing.T) {
	m := newTestModelOnPane(t, paneEvents)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("$")})
	// A snapshot in hand for the CURRENT axis, exactly as a landed reply leaves it.
	m.costPane.snap = pricedSnapshotWithSeries(t, map[string]int64{"only-on-the-model-axis": 3_820_000})
	m.costPane.lastFetch = time.Now()
	m.costPane.loading = false
	before := m.costPane.group
	if !strings.Contains(m.paneView(), "only-on-the-model-axis") {
		t.Fatalf("the series is not on screen before the cycle; test premise is wrong:\n%s", m.paneView())
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})

	if m.costPane.group == before {
		t.Fatalf("g did not change the axis; test premise is wrong")
	}
	if m.costPane.snap != nil {
		t.Fatal("cycling the axis kept the snapshot fetched for the previous one")
	}
	got := m.paneView()
	if strings.Contains(got, "only-on-the-model-axis") {
		t.Errorf("the previous axis's series is still drawn under the new %q heading:\n%s",
			"BY "+strings.ToUpper(string(m.costPane.group)), got)
	}
	if !strings.Contains(got, "loading") {
		t.Errorf("the pane neither shows the new axis nor says it is fetching it:\n%s", got)
	}
}

// TestRenderCostPane_CoverageIsMeasuredAgainstPriceableRequests is the second: swapping
// the denominator from PriceableRequests to Requests survived the suite, because no
// fixture in this file ever set the two to different values.
//
// The adjacent comment names this as the bug that "left a correctly configured deployment
// reading a permanent warning with nothing to act on" — Requests counts every proxied
// response, MCP tool calls and health checks included, while only inference can ever be
// priced.
func TestRenderCostPane_CoverageIsMeasuredAgainstPriceableRequests(t *testing.T) {
	// 900 proxied responses, 10 of them priceable, all 10 priced: FULLY covered.
	snap := &usage.Snapshot{Window: "today", Priced: true, Group: usage.GroupModel,
		Totals: usage.Counts{Requests: 900, PriceableRequests: 10, PricedRequests: 10,
			CostMicros: 4_170_000},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 900, PriceableRequests: 10, PricedRequests: 10, CostMicros: 4_170_000},
		}}},
	}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	if strings.Contains(got, "of 900") {
		t.Errorf("coverage is measured against every proxied response rather than the priceable ones:\n%s", got)
	}
	if strings.Contains(got, "covers") {
		t.Errorf("a fully priced deployment carries a coverage caveat:\n%s", got)
	}
	if strings.Contains(got, "COVERAGE") {
		t.Errorf("a fully priced deployment rendered a COVERAGE section:\n%s", got)
	}
	// The mirror, so the test cannot pass by suppressing the warning outright: a REAL gap
	// against the same 900 still reports, and reports the priceable denominator.
	snap.Totals.PricedRequests = 4
	gap := renderCostPane(snap, usage.GroupModel, 120, 40)
	if !strings.Contains(gap, "covers 4 of 10 priceable requests") {
		t.Errorf("a real gap is not reported against the priceable denominator:\n%s", gap)
	}
}

// TestCostPane_ARejectedPersistedValueIsNormalisedOnOpen.
//
// costView drops a field the pane cannot honour and the pane falls back to its default —
// but Settings kept the rejected value, so esc wrote it straight back and the file never
// healed. Every future open re-read a value the pane had already refused.
func TestCostPane_ARejectedPersistedValueIsNormalisedOnOpen(t *testing.T) {
	m := newTestModelOnPane(t, paneEvents)
	saved := recordSaves(t, m)
	// "6h" is a Usage-pane window this pane never cycles; "status" is a real axis with no
	// series here. Both are what costView exists to reject.
	Settings.Cost = CostSettings{Window: "6h", Group: "status"}

	_ = m.openCostPane()

	if Settings.Cost.Window != "" {
		t.Errorf("Settings.Cost.Window = %q after open, want it cleared — the pane rejected it",
			Settings.Cost.Window)
	}
	if Settings.Cost.Group != "" {
		t.Errorf("Settings.Cost.Group = %q after open, want it cleared", Settings.Cost.Group)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(*saved) != 1 {
		t.Fatalf("leaving the pane wrote %d times, want exactly 1", len(*saved))
	}
	if got := (*saved)[0].Cost; got.Window == "6h" || got.Group == "status" {
		t.Errorf("esc wrote the rejected value straight back to disk: %+v", got)
	}
	// A value the pane DOES honour survives untouched, so the healing is not a reset.
	Settings.Cost = CostSettings{Window: "7d", Group: "endpoint"}
	_ = m.openCostPane()
	if Settings.Cost.Window != "7d" || Settings.Cost.Group != "endpoint" {
		t.Errorf("a valid persisted view was cleared: %+v", Settings.Cost)
	}
}

// TestPaneKeys_CostPaneNamesEveryAxisAndSaysItWinsOverTheGlobalG.
//
// The overlay contradicted itself: the GLOBAL group advertised "g / G — jump to top /
// bottom" while the Cost group advertised "g — cycle breakdown", with nothing saying which
// wins. Cost wins (keys.go claims it before the global dispatch) and G is genuinely inert
// there. The axis list had also gone stale — the agent axis was added to costPaneGroups and
// not to this line.
func TestPaneKeys_CostPaneNamesEveryAxisAndSaysItWinsOverTheGlobalG(t *testing.T) {
	g, ok := paneKeys[paneCost]
	if !ok {
		t.Fatal("paneKeys has no entry for paneCost")
	}
	var gDesc string
	for _, kb := range g.bindings {
		if kb.keys == "g" {
			gDesc = kb.desc
		}
	}
	if gDesc == "" {
		t.Fatal("the Cost pane's help entry has no [g] binding")
	}
	// Every axis the key actually cycles, by name.
	for _, axis := range costPaneGroups {
		if !strings.Contains(gDesc, string(axis)) {
			t.Errorf("[g] is described as %q, which does not name the %q axis it cycles", gDesc, axis)
		}
	}
	// And the contradiction with the global list is resolved somewhere a reader will see.
	body := helpBodyLines(paneCost)
	var globalG string
	for _, kb := range globalKeys.bindings {
		if strings.Contains(kb.keys, "g") && strings.Contains(kb.desc, "jump") {
			globalG = kb.desc
		}
	}
	if globalG == "" {
		t.Fatal("the global group no longer advertises g/G; this test's premise is wrong")
	}
	if !strings.Contains(strings.ToLower(globalG), "cost") && !strings.Contains(strings.ToLower(gDesc), "global") {
		t.Errorf("the overlay lists both %q (global) and %q (Cost) with nothing saying which wins:\n%s",
			globalG, gDesc, body)
	}
}

// damagedTodaySnapshot is a LEDGER-BACKED "today" answer that lost rows: the fixture whose
// absence hid the defect. Only a ledger window can populate usage.Snapshot.Degraded, and
// "today" is this pane's DEFAULT window (costPaneWindows[0]), so this is the ordinary case
// rather than an exotic one.
//
// Shaped like the real thing: ONE bucket spanning the window, which is what ledgerSnapshot
// returns, and no UnpricedBy — a per-minute ledger row cannot distinguish the unpriced pairs
// from the priced ones, so the ledger never sends it.
func damagedTodaySnapshot(d *usage.Degraded) *usage.Snapshot {
	return &usage.Snapshot{
		Window:        usage.WindowToday,
		BucketSeconds: 86_400,
		Group:         usage.GroupModel,
		Totals: usage.Counts{
			Requests: 400, Tokens: 120_000,
			CostMicros: 12_500_000, PricedRequests: 300, PriceableRequests: 400,
			IncompleteRequests: 7,
		},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"claude-opus-5": {Requests: 400, CostMicros: 12_500_000,
				PricedRequests: 300, PriceableRequests: 400},
		}}},
		Priced:   true,
		Degraded: d,
	}
}

// TestRenderCostPane_DisclosesADamagedLedgerRead.
//
// usage.Snapshot.Degraded says the answer is MISSING ROWS. The server populates it and logs
// a warning, and it reached no client in cmd/abctl at all — so a damaged read rendered a
// figure byte-identical to a clean one, which is the failure the field's own doc says it
// exists to prevent, on this pane's default window.
func TestRenderCostPane_DisclosesADamagedLedgerRead(t *testing.T) {
	snap := damagedTodaySnapshot(&usage.Degraded{SkippedLines: 3, TruncatedDays: 1})
	got := renderCostPane(snap, usage.GroupModel, 120, 40)

	// The figure stays: it is short, not unknown, and withholding it would report a day of
	// known spend as unavailable.
	if !strings.Contains(got, "$12.5000") {
		t.Errorf("the pane withheld a figure that is short rather than unknown:\n%s", got)
	}
	for _, want := range []string{"SHORT", "3 unreadable lines", "1 day file"} {
		if !strings.Contains(got, want) {
			t.Errorf("pane missing %q — the damage is not disclosed:\n%s", want, got)
		}
	}
}

// TestRenderCostPane_ACleanReadCarriesNoDamageCaveat is the mirror, and the half that keeps
// the disclosure worth reading: a permanent caveat with nothing to act on is what teaches an
// operator to ignore the one that matters.
func TestRenderCostPane_ACleanReadCarriesNoDamageCaveat(t *testing.T) {
	got := renderCostPane(damagedTodaySnapshot(nil), usage.GroupModel, 120, 40)
	for _, banned := range []string{"SHORT", "ledger", damagedMarker} {
		if strings.Contains(got, banned) {
			t.Errorf("a clean read renders %q:\n%s", banned, got)
		}
	}
}

// TestRenderCostPane_DamageIsNotMergedWithInexactness.
//
// usage.Snapshot.Degraded's doc forbids merging the two claims or showing them under one
// marker: IncompleteRequests says a figure the total CARRIES is a floor, Degraded says rows
// are missing from the sum. The fixture has both, and each has to be separately legible.
func TestRenderCostPane_DamageIsNotMergedWithInexactness(t *testing.T) {
	snap := damagedTodaySnapshot(&usage.Degraded{SkippedLines: 3})
	got := renderCostPane(snap, usage.GroupModel, 120, 40)

	// The inexactness caveat keeps its own words and its own numbers. Direction-free,
	// because this fixture is a ledger-shaped window and carries no IncompleteBy — see
	// costInexactNote and the two tests below it.
	if !strings.Contains(got, "7 of 300 priced figures are inexact") {
		t.Errorf("the inexactness caveat was displaced by the damage one:\n%s", got)
	}
	// And the damage caveat does not borrow them: no "lower bound" wording, and it says the
	// shortfall cannot be stated, which is the whole difference.
	var dmg string
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "SHORT") {
			dmg = l
		}
	}
	if dmg == "" {
		t.Fatalf("no damage caveat at all:\n%s", got)
	}
	if strings.Contains(dmg, "lower bound") {
		t.Errorf("the damage caveat is worded as an inexactness one: %q", dmg)
	}
	// It LEADS the caveats: it is the only one of the three that says the sum itself is
	// incomplete, where the others qualify a figure the sum contains.
	iDmg := strings.Index(got, "SHORT")
	iCov := strings.Index(got, "covers 300 of 400")
	iInc := strings.Index(got, "priced figures are inexact")
	if iDmg > iCov || iCov > iInc {
		t.Errorf("caveats out of order (damaged %d, coverage %d, inexact %d):\n%s",
			iDmg, iCov, iInc, got)
	}
}

// TestRenderCostPane_ADamagedUnpricedReadStillSaysSo.
//
// "Nothing in this window carried a cost" is a claim about the rows that were READ, and a
// damaged read may have lost the priced ones. Without the disclosure on this path a corrupt
// day file renders as a quiet day — the most reassuring possible spelling of a data loss.
func TestRenderCostPane_ADamagedUnpricedReadStillSaysSo(t *testing.T) {
	snap := &usage.Snapshot{
		Window: usage.WindowToday, Priced: false, Group: usage.GroupModel,
		Totals:   usage.Counts{Requests: 400, PriceableRequests: 400},
		Degraded: &usage.Degraded{TruncatedDays: 1},
	}
	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	if !strings.Contains(got, "unavailable") {
		t.Errorf("an unpriced window stopped saying so:\n%s", got)
	}
	if !strings.Contains(got, "SHORT") {
		t.Errorf("a damaged read of an unpriced day reads as a quiet day:\n%s", got)
	}
}

// TestRenderCostPane_FitsEveryWidthWithADamagedRead.
//
// The damage sentence is now the longest prose the pane emits, and the pane's own headings
// reach 35 cells, so a caveat that never wrapped would still have fitted the fixtures that
// switch it off. costWidthFloors reaches 16.
func TestRenderCostPane_FitsEveryWidthWithADamagedRead(t *testing.T) {
	snap := damagedTodaySnapshot(&usage.Degraded{SkippedLines: 1_234_567, TruncatedDays: 89})
	snap.PricedBy = map[string]int64{"authoritative": 200, "bundled": 60, "configured": 40}
	for _, w := range costWidthFloors {
		for _, h := range []int{60, 40, 24, 20, 16, 8, 4} {
			got := renderCostPane(snap, usage.GroupModel, w, h)
			for i, line := range strings.Split(got, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("w=%d h=%d line %d is %d columns: %q", w, h, i, lw, line)
				}
			}
			if n := len(strings.Split(got, "\n")); h > 0 && n > h {
				t.Errorf("w=%d h=%d produced %d lines:\n%s", w, h, n, got)
			}
		}
	}
}

// TestRenderCostPane_ADisclosureWithNoCountersIsStillADisclosure.
//
// The pane half of the presence rule. usage.Snapshot.Degraded is a pointer so a clean read
// serialises NOTHING; a present object with zero counters therefore still reports damage,
// and the sentence says so without inventing a number.
func TestRenderCostPane_ADisclosureWithNoCountersIsStillADisclosure(t *testing.T) {
	got := renderCostPane(damagedTodaySnapshot(&usage.Degraded{}), usage.GroupModel, 120, 40)
	if !strings.Contains(got, "SHORT") {
		t.Errorf("a counterless disclosure reads as a clean read:\n%s", got)
	}
	if !strings.Contains(got, "without saying how much") {
		t.Errorf("the pane invented or omitted a count it was not given:\n%s", got)
	}
}

// qualifiedTotalSnapshot is a total that is BOTH partial and inexact — the fixture whose
// absence hid the tiny-height defect. Every test above either switched the caveats off or
// rendered tall enough that they all fitted, so nothing could see the fallback path drop
// them.
//
// Ledger-shaped, like the window this pane defaults to: one bucket spanning the whole span,
// no UnpricedBy (a per-minute ledger row cannot distinguish the unpriced pairs from the
// priced ones).
func qualifiedTotalSnapshot(d *usage.Degraded) *usage.Snapshot {
	snap := damagedTodaySnapshot(d)
	// Explicit about the premise this test rests on, so a fixture edit cannot quietly make
	// the assertions vacuous.
	if snap.Totals.PricedRequests >= snap.Totals.PriceableRequests {
		panic("fixture is not partial")
	}
	if snap.Totals.IncompleteRequests == 0 {
		panic("fixture is not inexact")
	}
	return snap
}

// costCaveatSentences maps the leading words of each caveat the TOTAL section can emit to the
// closing words of the same sentence. A rendered caveat must carry both: half a sentence
// about a missing day reads as a complete one about something else.
var costCaveatSentences = map[string]string{
	"this total is SHORT":        "an amount nothing here can state",
	"covers 300 of 400":          "the rest carry no figure",
	"priced figures are inexact": "does not record which way",
	// The clamp disclosure's own closing, which is deliberately long: its tail shares
	// "an amount nothing here can state" with the damaged-read sentence above, so a short key
	// would be satisfied by the WRONG caveat's ending whenever both are on screen.
	"every figure in this window is a FLOOR": "the real requests, tokens and cost are all " +
		"larger, by an amount nothing here can state",
}

// TestRenderCostPane_ATinyHeightNeverShowsABareConfidentFigure.
//
// The defect: renderCostPane's last-resort path cut the TOTAL section's flat line list at
// out[:height], and what it cut was the coverage ratio and the lower-bounds caveat — so at
// small heights the pane kept the dollar amount and discarded the two things saying the
// amount was partial and inexact. Its comment claimed "whole lines only… this path cannot
// cut it", true of the FIGURE and of nothing else.
//
// Heights 4 through 8 because layout()'s own floor is 4, so this is reachable rather than
// theoretical; 1 through 12 as well, because the interesting failures are at the seams where
// a unit stops fitting.
func TestRenderCostPane_ATinyHeightNeverShowsABareConfidentFigure(t *testing.T) {
	snap := qualifiedTotalSnapshot(&usage.Degraded{SkippedLines: 3})
	for _, h := range append([]int{4, 5, 6, 7, 8}, 1, 2, 3, 9, 10, 11, 12) {
		got := renderCostPane(snap, usage.GroupModel, 60, h)
		if !strings.Contains(got, "$12.5000") {
			t.Errorf("h=%d dropped the answer entirely:\n%s", h, got)
			continue
		}
		// The figure wears all three claims, whatever the budget left of the words.
		want := damagedMarker + inexactMarker + "$12.5000" + partialMarker
		if !strings.Contains(got, want) {
			t.Errorf("h=%d publishes a qualified total as a bare confident number:\n%s", h, got)
		}
		// The budget is respected, not merely smaller.
		if n := len(strings.Split(got, "\n")); n > h {
			t.Errorf("h=%d produced %d lines:\n%s", h, n, got)
		}
	}
}

// unwrapCostBody rejoins a rendered pane into one line so a WRAPPED sentence can be matched
// whole. Trimming each line and joining with a single space reconstructs the prose exactly as
// wrapCells split it.
//
// Needed because the assertion below is about SENTENCES and the pane's own wrapping puts
// newlines inside them: matching the raw output reports every wrapped caveat as a fragment.
func unwrapCostBody(s string) string {
	parts := strings.Split(s, "\n")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, " ")
}

// TestRenderCostPane_ATinyHeightDropsWholeCaveatsNotSentences.
//
// The other half of "whole lines only": the caveats are WRAPPED PROSE, so a cut at a line
// boundary leaves a fragment. Whatever survives the budget must be a complete sentence.
//
// Every width and every height up to where all four sections fit, because a caveat's line
// count depends on both, and a fragment appears exactly at the seam where one more line would
// have carried the whole of it.
func TestRenderCostPane_ATinyHeightDropsWholeCaveatsNotSentences(t *testing.T) {
	snap := qualifiedTotalSnapshot(&usage.Degraded{SkippedLines: 3, TruncatedDays: 1})
	for _, w := range []int{40, 60, 80, 120} {
		for h := 1; h <= 24; h++ {
			flat := unwrapCostBody(renderCostPane(snap, usage.GroupModel, w, h))
			for opening, closing := range costCaveatSentences {
				if strings.Contains(flat, opening) && !strings.Contains(flat, closing) {
					t.Errorf("w=%d h=%d: caveat %q is rendered without its ending %q:\n%s",
						w, h, opening, closing, flat)
				}
			}
		}
	}
}

// TestRenderCostPane_TheFigureWearsTheSameMarkersAsEveryOtherMoneySurface.
//
// One spelling everywhere. The Cost pane was the only money surface stating its caveats in
// words alone — which is exactly the surface a short terminal cuts the words off — while the
// strip, the sessions COST cell and the Usage pane cell all marked theirs. Pinned against
// the strip's own figure for the same three facts, so the two cannot drift.
func TestRenderCostPane_TheFigureWearsTheSameMarkersAsEveryOtherMoneySurface(t *testing.T) {
	snap := qualifiedTotalSnapshot(&usage.Degraded{SkippedLines: 3})
	paneFigure := costTotalFigure(snap)

	stripFig := moneyFigure(12.5, "today",
		snap.Totals.PriceableRequests-snap.Totals.PricedRequests,
		snap.Totals.PriceableRequests, snap.Totals.IncompleteRequests, snap.Degraded,
		snap.Totals.Saturated)
	// Same markers in the same places; only the amount's own formatter differs (costMoney
	// states four places, formatUSDCell adds a "<" floor), so compare the marker frame.
	paneFrame := strings.ReplaceAll(paneFigure, "$12.5000", "AMOUNT")
	stripFrame := strings.ReplaceAll(strings.Fields(stripFig.compact)[0], "$12.5000", "AMOUNT")
	if paneFrame != stripFrame {
		t.Errorf("pane figure frame %q, strip figure frame %q — two spellings for three facts",
			paneFrame, stripFrame)
	}
}

// TestRenderCostPane_AnUnqualifiedTotalCarriesNoMarkers is the mirror, and the half that
// keeps the markers worth reading. A figure permanently wearing "~" says nothing.
func TestRenderCostPane_AnUnqualifiedTotalCarriesNoMarkers(t *testing.T) {
	snap := damagedTodaySnapshot(nil)
	snap.Totals.PricedRequests = snap.Totals.PriceableRequests
	snap.Totals.IncompleteRequests = 0
	for _, h := range []int{1, 4, 8, 40} {
		got := renderCostPane(snap, usage.GroupModel, 60, h)
		if !strings.Contains(got, "$12.5000") {
			t.Fatalf("h=%d lost the figure; test premise is wrong:\n%s", h, got)
		}
		for _, m := range []string{damagedMarker, inexactMarker, partialMarker} {
			if strings.Contains(got, m+"$12.5000") || strings.Contains(got, "$12.5000"+m) {
				t.Errorf("h=%d marks a clean, complete, exact total with %q:\n%s", h, m, got)
			}
		}
	}
}

// TestRenderCostPane_EachQualificationMarksTheFigureOnItsOwn.
//
// Each of the three independently, because a fixture with all three on cannot tell "all
// three markers are applied" from "one marker is applied three times", and the fallback path
// only has to lose ONE of them to publish a misleading figure.
func TestRenderCostPane_EachQualificationMarksTheFigureOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(*usage.Snapshot)
		want  string
	}{
		{"damaged only", func(s *usage.Snapshot) {
			s.Degraded = &usage.Degraded{SkippedLines: 3}
		}, damagedMarker + "$12.5000"},
		{"inexact only", func(s *usage.Snapshot) {
			s.Totals.IncompleteRequests = 7
		}, inexactMarker + "$12.5000"},
		{"partial only", func(s *usage.Snapshot) {
			s.Totals.PricedRequests = 300
		}, "$12.5000" + partialMarker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := damagedTodaySnapshot(nil)
			snap.Totals.PricedRequests = snap.Totals.PriceableRequests
			snap.Totals.IncompleteRequests = 0
			tc.shape(snap)
			// Every height including the fallback path's own, because the marker is what makes
			// that path safe.
			for _, h := range []int{4, 5, 6, 7, 8, 40} {
				got := renderCostPane(snap, usage.GroupModel, 60, h)
				if !strings.Contains(got, tc.want) {
					t.Errorf("h=%d: figure does not carry %q:\n%s", h, tc.want, got)
				}
			}
		})
	}
}

// inexactBySnapshot is a ledger-shaped today window whose inexact figures carry REASONS —
// usage.Snapshot.IncompleteBy — so the pane's caveat can be asked which way it claims the
// total is wrong.
//
// Built on damagedTodaySnapshot with the damage removed: same 7-of-300 inexact figures, no
// missing rows, so the only caveat under test is the inexactness one.
func inexactBySnapshot(by map[string]int64) *usage.Snapshot {
	snap := damagedTodaySnapshot(nil)
	snap.IncompleteBy = by
	return snap
}

// TestRenderCostPane_AFloorIsCalledAFloor keeps the strong wording where it is TRUE. The
// direction is the actionable half of the disclosure — a total that will only go up is a
// different thing to chase than one that is merely fuzzy — so a fix for the false case must
// not cost the true one its words.
func TestRenderCostPane_AFloorIsCalledAFloor(t *testing.T) {
	got := unwrapCostBody(renderCostPane(
		inexactBySnapshot(map[string]int64{"output-uncounted": 7}), usage.GroupModel, 120, 40))

	if !strings.Contains(got, "7 are lower bounds") {
		t.Errorf("a window whose figures are all floors does not say so:\n%s", got)
	}
	if !strings.Contains(got, "real total is higher") {
		t.Errorf("the floor is named without its direction, which is the half an operator "+
			"acts on:\n%s", got)
	}
}

// TestRenderCostPane_AnApproximationIsNotCalledAFloor is the defect this reading fixes.
//
// pricing.ReasonSplitUnreported means a gateway reported only a total, so the figure is off in
// NO KNOWN DIRECTION — over-priced for a cache-heavy request, under-priced for a
// generation-heavy one. The pane asserted "lower bounds … so the real total is higher" over
// every inexact figure, because Totals.IncompleteRequests is one number that cannot tell the
// two apart. Claiming a direction that does not exist is a false statement about money, and it
// also mis-frames a STANDING property of a gateway as an incident.
func TestRenderCostPane_AnApproximationIsNotCalledAFloor(t *testing.T) {
	got := unwrapCostBody(renderCostPane(
		inexactBySnapshot(map[string]int64{"split-unreported": 7}), usage.GroupModel, 120, 40))

	if !strings.Contains(got, "7 are approximations") {
		t.Errorf("the pane does not name the figures as approximations:\n%s", got)
	}
	if !strings.Contains(got, "no known direction") {
		t.Errorf("the pane states an approximation without saying it has no direction:\n%s", got)
	}
	for _, banned := range []string{"lower bound", "real total is higher"} {
		if strings.Contains(got, banned) {
			t.Errorf("the pane says %q over figures that are approximations, which claims a "+
				"direction the data does not have:\n%s", banned, got)
		}
	}
}

// TestRenderCostPane_MixedReasonsAreBothStated. Both kinds in one window, so neither is
// rounded up into the other's wording — and the count line above still says how many in total.
func TestRenderCostPane_MixedReasonsAreBothStated(t *testing.T) {
	got := unwrapCostBody(renderCostPane(
		inexactBySnapshot(map[string]int64{"output-uncounted": 6, "split-unreported": 1}),
		usage.GroupModel, 120, 40))

	for _, want := range []string{"7 of 300 priced figures are inexact", "6 are lower bounds",
		"1 is an approximation"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q — a mixed window must state both claims:\n%s", want, got)
		}
	}
}

// TestRenderCostPane_NoReasonsClaimsNoDirection is the pane's DEFAULT window, and the reason
// the direction had to become conditional rather than merely correct sometimes: today and 7d
// are ledger-backed, and a persisted per-minute row has no reason column, so IncompleteBy is
// never populated for them and the old sentence's direction was a guess on every real run.
//
// The caveat itself still prints. usage.Snapshot.IncompleteBy's doc is explicit that its
// absence is not a claim of exactness.
func TestRenderCostPane_NoReasonsClaimsNoDirection(t *testing.T) {
	got := unwrapCostBody(renderCostPane(inexactBySnapshot(nil), usage.GroupModel, 120, 40))

	if !strings.Contains(got, "7 of 300 priced figures are inexact") {
		t.Errorf("a window that does not record the reasons dropped the inexactness caveat "+
			"entirely, which reads as an exact total:\n%s", got)
	}
	if !strings.Contains(got, "does not record which way") {
		t.Errorf("the pane neither names a direction nor says the window has none:\n%s", got)
	}
	for _, banned := range []string{"lower bound", "real total is higher", "approximation"} {
		if strings.Contains(got, banned) {
			t.Errorf("the pane invented %q for a window that reported no reasons:\n%s", banned, got)
		}
	}
	// The figure keeps its marker either way: what is unknown is the direction, not the
	// inexactness.
	if !strings.Contains(got, inexactMarker+"$12.5000") {
		t.Errorf("the figure lost its inexactness marker:\n%s", got)
	}
}

// TestCostInexactClauses_OrderAndUnknownKeys pins the two rules the caveat's prose depends on
// that are awkward to see through a rendered pane: a FIXED order (a map's iteration order is
// randomised, so a substring check can pass on a lucky shuffle) and an unrecognised key
// PRINTED rather than dropped.
//
// Dropping it would be the worse bug of the two: IncompleteBy's counts sum to
// IncompleteRequests by contract, so a reader subtracting the clauses from the count would
// conclude the remainder were exact figures. Sanitized on the way out, because a reason string
// is a wire value and an ESC in one reaches the TTY — the hazard costPricedBy exists for.
func TestCostInexactClauses_OrderAndUnknownKeys(t *testing.T) {
	if got := costInexactClauses(nil); got != nil {
		t.Errorf("costInexactClauses(nil) = %v, want nothing at all", got)
	}
	got := costInexactClauses(map[string]int64{
		"split-unreported": 2,
		"output-uncounted": 5,
		"unlabelled":       1,
		"zz-new":           3,
		"aa-\x1b[31mnew":   4,
	})
	if len(got) != 5 {
		t.Fatalf("got %d clauses, want 5: %v", len(got), got)
	}
	wantPrefix := []string{
		"5 are lower bounds",
		"2 are approximations",
		"1 carries a caveat",
		"4 under \"aa-�[31mnew\"",
		"3 under \"zz-new\"",
	}
	for i, want := range wantPrefix {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("clause %d = %q, want it to start %q", i, got[i], want)
		}
	}
	if strings.Contains(strings.Join(got, " "), "\x1b") {
		t.Errorf("a raw ESC from a wire key reached the rendered caveat: %v", got)
	}
}

// ungroupedSnapshot is a window whose SERIES IS SHORT OF ITS TOTAL, with
// usage.Snapshot.UngroupedCostMicros carrying the difference.
//
// The shape a gateway-priced /v1/embeddings response produces: the gateway settled a cost,
// the inference parser cannot read that path so the event carries no model, and the dollars
// therefore count toward Totals while belonging to no group=model key. It is priced traffic
// — the ungrouped part is added to PricedRequests too — and what it lacks is a label on
// this axis, not a figure.
//
// THE IDENTITY IS ASSERTED HERE, once, so no case below can pass against a fixture that
// never held it. sum(series CostMicros) + UngroupedCostMicros == Totals.CostMicros is what
// the field promises for a reconcilable group; a test that only matched rendered strings
// would look identical whether or not the numbers reconciled, which is the failure mode of
// asserting output instead of arithmetic.
//
// Through usage.SetUngroupedCost rather than by taking the address of a local, so the
// absent-not-zero rule under test is the producer's own: a residual of zero must leave the
// field nil.
func ungroupedSnapshot(t *testing.T, seriesCost map[string]int64, ungrouped int64) *usage.Snapshot {
	t.Helper()
	var totals usage.Counts
	series := map[string]usage.Counts{}
	for label, micros := range seriesCost {
		c := usage.Counts{Requests: 1, PriceableRequests: 1, PricedRequests: 1, CostMicros: micros}
		series[label] = c
		totals.Add(c)
	}
	totals.Add(usage.Counts{Requests: 1, PriceableRequests: 1, PricedRequests: 1, CostMicros: ungrouped})
	snap := &usage.Snapshot{
		Window: usage.WindowToday, Group: usage.GroupModel, Priced: true,
		Totals:  totals,
		Buckets: []usage.Bucket{{Counts: totals, Series: series}},
	}
	// A usage.CostSum, not an int64: the setter takes the saturation flag alongside the
	// figure so a clamped residual cannot arrive looking exact. These fixtures state a
	// residual directly, so Saturated stays false.
	snap.SetUngroupedCost(usage.CostSum{Micros: ungrouped})
	var sum int64
	for _, c := range series {
		sum += c.CostMicros
	}
	if sum+ungrouped != snap.Totals.CostMicros {
		t.Fatalf("fixture does not reconcile: series %d + ungrouped %d = %d, totals %d",
			sum, ungrouped, sum+ungrouped, snap.Totals.CostMicros)
	}
	return snap
}

// costLineIndex reports which rendered line first contains want, or -1.
//
// Used for the ORDER assertions: two disclosures that must not be mistaken for each other
// have to appear in a fixed order, and "both strings are somewhere in the output" cannot
// see an ordering defect.
func costLineIndex(rendered, want string) int {
	for i, l := range strings.Split(rendered, "\n") {
		if strings.Contains(l, want) {
			return i
		}
	}
	return -1
}

// TestRenderCostPane_DisclosesTheSpendNoRowCarries.
//
// The defect: usage.Snapshot.UngroupedCostMicros was populated by both producers and read
// by nothing, so the pane drew a breakdown that summed to LESS than the total two sections
// above it with nothing on screen to explain the difference. A reader adding the column up
// found $4.0000 under a $4.2500 headline.
func TestRenderCostPane_DisclosesTheSpendNoRowCarries(t *testing.T) {
	snap := ungroupedSnapshot(t, map[string]int64{"m-a": 3_000_000, "m-b": 1_000_000}, 250_000)

	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	by := sectionOf(t, got, "BY MODEL")
	if !strings.Contains(by, costUngroupedLabel) {
		t.Errorf("the breakdown does not disclose the spend no row carries:\n%s", by)
	}
	if !strings.Contains(by, "$0.2500") {
		t.Errorf("the residual band carries no figure (want $0.2500):\n%s", by)
	}
	// Its share is of the same published total the rows are measured against, so the band
	// is comparable with them: 250000/4250000.
	if !strings.Contains(by, "5.9%") {
		t.Errorf("the residual band carries no share of the published total:\n%s", by)
	}
	// And the headline is still the SERVER'S total, never the sum of the rows. The residual
	// exists so the breakdown can be reconciled, not so the total can be re-derived.
	if tot := sectionOf(t, got, "TOTAL"); !strings.Contains(tot, "$4.2500") {
		t.Errorf("TOTAL is not the published figure:\n%s", tot)
	}
	if strings.Contains(got, "$4.0000") {
		t.Errorf("the series sum is presented as a total somewhere in the pane:\n%s", got)
	}
}

// TestRenderCostPane_AReconciledBreakdownRendersNoBand is the mirror, and the half that
// keeps the band worth reading.
//
// Absence of the field means the breakdown accounts for every dollar — a nil pointer, not a
// zero — so a band must not appear. A permanent "(unattributed) $0.0000" row on every
// correctly attributed window is the same failure as a coverage warning that never clears:
// it trains a reader to skip the one row that matters.
func TestRenderCostPane_AReconciledBreakdownRendersNoBand(t *testing.T) {
	snap := ungroupedSnapshot(t, map[string]int64{"m-a": 3_000_000, "m-b": 1_000_000}, 0)
	if snap.UngroupedCostMicros != nil {
		t.Fatalf("fixture premise is wrong: a zero residual was published as %d",
			*snap.UngroupedCostMicros)
	}

	got := renderCostPane(snap, usage.GroupModel, 120, 40)
	if strings.Contains(got, costUngroupedLabel) {
		t.Errorf("a complete breakdown still drew a residual band:\n%s", got)
	}
	if strings.Contains(got, "$0.0000") {
		t.Errorf("a zero band was rendered for a breakdown with no residual:\n%s", got)
	}
}

// TestCostUngroupedRow_OnlyAPublishedPositiveResidualBecomesABand is the unit-level half.
//
// Three refusals, and each would put a claim on screen that the data does not support: an
// absent field means the breakdown reconciles, a zero is that same case wearing a value,
// and a negative residual is not spend. usage.SetUngroupedCost already refuses the last
// two, which is exactly why this restates them — a guarantee inherited silently is a
// guarantee that stops holding without anything noticing.
func TestCostUngroupedRow_OnlyAPublishedPositiveResidualBecomesABand(t *testing.T) {
	base := func(micros *int64) *usage.Snapshot {
		return &usage.Snapshot{
			Priced:              true,
			Totals:              usage.Counts{CostMicros: 4_250_000, PricedRequests: 3, PriceableRequests: 3},
			UngroupedCostMicros: micros,
		}
	}
	for _, tc := range []struct {
		name  string
		value *int64
		want  bool
	}{
		{"absent", nil, false},
		{"zero", ptrInt64(0), false},
		{"negative", ptrInt64(-5_000_000), false},
		{"positive", ptrInt64(250_000), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := costUngroupedRow(base(tc.value))
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v (row %+v)", ok, tc.want, row)
			}
			if !ok {
				return
			}
			if row.label != costUngroupedLabel {
				t.Errorf("label = %q, want %q", row.label, costUngroupedLabel)
			}
			if !strings.Contains(row.right, "$0.2500") || !row.hasBar {
				t.Errorf("row = %+v, want the figure and a share bar", row)
			}
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }

// TestRenderCostPane_TheResidualBandIsNotTheTruncationNote.
//
// TWO DIFFERENT CLAIMS, and merging them would be worse than either alone. "+N more, each
// smaller than the last row" is rows omitted FOR SPACE — a taller terminal shows them. The
// band is spend with no row on this axis AT ALL — no terminal will ever produce one. A
// reader who read them as one disclosure would go looking for a row that cannot exist.
//
// The ORDER is asserted, not just the presence of both: the note's own wording names "the
// last row", so a band sitting above it would make that phrase point at the residual and
// turn the count into a claim about entries cheaper than it.
func TestRenderCostPane_TheResidualBandIsNotTheTruncationNote(t *testing.T) {
	series := map[string]int64{}
	for i := 0; i < costMaxSeriesRows+2; i++ {
		series[fmt.Sprintf("model-%02d", i)] = int64(1_000_000 - i*1_000)
	}
	snap := ungroupedSnapshot(t, series, 250_000)

	by := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 60), "BY MODEL")
	note := costLineIndex(by, "+2 more")
	band := costLineIndex(by, costUngroupedLabel)
	if note < 0 {
		t.Fatalf("the truncation note is missing; test premise is wrong:\n%s", by)
	}
	if band < 0 {
		t.Fatalf("the residual band is missing:\n%s", by)
	}
	if band < note {
		t.Errorf("the band is drawn above the +N note, so \"the last row\" names the residual:\n%s", by)
	}
	// The note counts ELIDED SERIES ROWS ONLY. The band is not one of them, and the cap
	// applies to label rows, so it must not be counted against either.
	if strings.Contains(by, "+3 more") {
		t.Errorf("the residual band was counted as an elided series row:\n%s", by)
	}
	// The two claims never share a line, which is what keeps them two claims.
	if note == band {
		t.Errorf("the band and the truncation note are on one line:\n%s", by)
	}
}

// TestRenderCostPane_TheResidualBandIsAStateNotAName.
//
// The band is not a key and must not be readable as one. Its spelling follows
// costUnattributedLabel's convention — parenthesised, lowercase — rather than inventing a
// second way of saying "this is a state".
//
// On group=agent BOTH labels can appear at once, and the case is not contrived: the agent
// axis reserves a bucket for traffic that named no client. They are different facts —
// "(no user-agent)" is a row of real spend whose requests carried no User-Agent,
// "(unattributed)" is spend with no row at all — so the pane must keep them distinguishable
// rather than collapsing them into one "unknown".
func TestRenderCostPane_TheResidualBandIsAStateNotAName(t *testing.T) {
	if !strings.HasPrefix(costUngroupedLabel, "(") || !strings.HasSuffix(costUngroupedLabel, ")") {
		t.Errorf("costUngroupedLabel = %q, which does not follow costUnattributedLabel's convention",
			costUngroupedLabel)
	}
	if costUngroupedLabel == costUnattributedLabel {
		t.Fatal("the residual band and the reserved agent bucket share one spelling for two facts")
	}
	snap := ungroupedSnapshot(t, map[string]int64{
		"claude-code/2.1.14":        3_000_000,
		pipeline.UnknownClientLabel: 1_000_000,
	}, 250_000)

	by := sectionOf(t, renderCostPane(snap, usage.GroupAgent, 120, 40), "BY AGENT")
	for _, want := range []string{costUnattributedLabel, costUngroupedLabel} {
		if !strings.Contains(by, want) {
			t.Errorf("BY AGENT is missing %q:\n%s", want, by)
		}
	}
	if strings.Contains(by, pipeline.UnknownClientLabel) {
		t.Errorf("the reserved bucket reached the screen as a name:\n%s", by)
	}
}

// TestRenderCostPane_TheResidualBandTravelsWithItsRows.
//
// Where the band ranks against the height budget. It qualifies the ROWS, not the figure —
// Totals.CostMicros already includes every ungrouped dollar — so it belongs to the
// breakdown section and is dropped WITH it. Two things follow, and both are asserted:
// a band never appears without the rows it is residual of, and the total is never left
// short by the section's loss.
//
// This is why the band is not a TOTAL caveat unit. A caveat's loss would leave a qualified
// figure looking exact, which is what costTotalFigure's markers exist to prevent; losing
// the band leaves a complete total and no column to add up, which misstates nothing.
func TestRenderCostPane_TheResidualBandTravelsWithItsRows(t *testing.T) {
	snap := ungroupedSnapshot(t, map[string]int64{"m-a": 3_000_000, "m-b": 1_000_000}, 250_000)
	for h := 1; h <= 24; h++ {
		got := renderCostPane(snap, usage.GroupModel, 80, h)
		// The answer survives every budget, as it must on every path in this pane.
		if !strings.Contains(got, "$4.2500") {
			t.Errorf("h=%d dropped the total:\n%s", h, got)
		}
		if strings.Contains(got, costUngroupedLabel) && !strings.Contains(got, "m-a") {
			t.Errorf("h=%d shows a residual with none of the rows it is residual of:\n%s", h, got)
		}
		if n := len(strings.Split(got, "\n")); n > h {
			t.Errorf("h=%d rendered %d lines:\n%s", h, n, got)
		}
	}
}

// TestRenderCostPane_FitsEveryWidthWithTheResidualBand.
//
// The band adds a label ("(unattributed)", 15 cells) and a figure to the shared column
// grid, so it moves the width arithmetic for every row in the section. The pane's contract
// is unchanged: never exceed the budget, and drop whole figures rather than clip them.
func TestRenderCostPane_FitsEveryWidthWithTheResidualBand(t *testing.T) {
	snap := ungroupedSnapshot(t, map[string]int64{
		"a-really-long-ascii-model-name": 3_820_000,
		"日本語モデル":                         350_000,
	}, 250_000)
	for _, w := range append(costWidthFloors, costHostileWidths()...) {
		for _, h := range []int{60, 40, 24, 20, 16} {
			got := renderCostPane(snap, usage.GroupModel, w, h)
			for i, line := range strings.Split(got, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("w=%d h=%d line %d is %d columns: %q", w, h, i, lw, line)
				}
			}
			// A clipped figure is a different, smaller number. The band's own figure is held to
			// the same rule as every other one in this pane.
			if strings.Contains(got, "$0.25") && !strings.Contains(got, "$0.2500") {
				t.Errorf("w=%d h=%d clipped the residual figure mid-digits:\n%s", w, h, got)
			}
		}
	}
}

// TestRenderCostPane_AnEmptyBreakdownDrawsNoResidualBand.
//
// The path where the field is LARGEST and a band would say least. group=session on a
// ledger-backed window carries no series at all — a per-minute row holds no session id — so
// costledger.Fold finds no label for any row and publishes the WHOLE total as ungrouped. A
// "(unattributed) 100.0%" band under "no session breakdown for this window" restates that
// sentence in a form that reads as a mystery spender, and a residual is the part a
// breakdown leaves out: with no breakdown there is nothing for it to be residual of.
func TestRenderCostPane_AnEmptyBreakdownDrawsNoResidualBand(t *testing.T) {
	whole := int64(12_500_000)
	snap := &usage.Snapshot{
		Window: usage.WindowToday, Group: usage.GroupSession, Priced: true,
		Totals:  usage.Counts{Requests: 400, CostMicros: whole, PricedRequests: 400, PriceableRequests: 400},
		Buckets: []usage.Bucket{{Counts: usage.Counts{Requests: 400, CostMicros: whole}}},
	}
	snap.SetUngroupedCost(usage.CostSum{Micros: whole})
	if snap.UngroupedCostMicros == nil {
		t.Fatal("fixture premise is wrong: the whole total was not published as ungrouped")
	}

	by := sectionOf(t, renderCostPane(snap, usage.GroupSession, 120, 40), "BY SESSION")
	if strings.Contains(by, costUngroupedLabel) {
		t.Errorf("a 100%% residual band was drawn where there is no breakdown to reconcile:\n%s", by)
	}
	if !strings.Contains(by, "no session breakdown") {
		t.Errorf("the empty breakdown stopped saying it is empty:\n%s", by)
	}
	// And nothing is understated: the total still carries every dollar.
	if tot := sectionOf(t, renderCostPane(snap, usage.GroupSession, 120, 40), "TOTAL"); !strings.Contains(tot, "$12.5000") {
		t.Errorf("TOTAL does not carry the whole figure:\n%s", tot)
	}
}

// TestRenderCostPane_TheResidualBandSharesTheRowsColumnGrid.
//
// Rendered through the same renderCostRows call as the label rows, so its figure sits in
// the same column as theirs. A band a reader cannot align with the rows is a band they
// cannot compare against them, which is the whole point of giving it a share and a bar.
func TestRenderCostPane_TheResidualBandSharesTheRowsColumnGrid(t *testing.T) {
	snap := ungroupedSnapshot(t, map[string]int64{
		"a-really-long-ascii-model-name": 3_000_000,
		"short":                          1_000_000,
	}, 250_000)

	by := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 60), "BY MODEL")
	width := -1
	var bandSeen bool
	for _, l := range strings.Split(by, "\n") {
		if !strings.HasPrefix(l, costIndent) || !strings.Contains(l, "$") {
			continue
		}
		if strings.Contains(l, costUngroupedLabel) {
			bandSeen = true
		}
		if width < 0 {
			width = lipgloss.Width(l)
			continue
		}
		if got := lipgloss.Width(l); got != width {
			t.Errorf("row is %d columns, the first row was %d — the band does not share the grid:\n%s",
				got, width, by)
		}
	}
	if !bandSeen {
		t.Fatalf("no band matched; test premise is wrong:\n%s", by)
	}
}

// overshotSnapshot is a breakdown whose series sums to MORE than the published total, which is
// the state usage.Snapshot.SeriesOvershootMicros exists to report.
//
// Built through SetUngroupedCost with a NEGATIVE residual, because that is the only way the
// field is ever populated — the producers pass a signed number and the setter turns a negative
// one into this unsigned magnitude. A fixture assigning the pointer directly would pass while
// the real path was broken, which is the shape of defect this whole file is about.
func overshotSnapshot(t *testing.T, seriesCost map[string]int64, overshoot int64) *usage.Snapshot {
	t.Helper()
	if overshoot <= 0 {
		t.Fatalf("fixture premise is wrong: an overshoot of %d is not an overshoot", overshoot)
	}
	var series = map[string]usage.Counts{}
	var seriesTotal int64
	for label, micros := range seriesCost {
		series[label] = usage.Counts{Requests: 1, PriceableRequests: 1, PricedRequests: 1,
			CostMicros: micros}
		seriesTotal += micros
	}
	// The total is SHORT of the series by exactly the overshoot, which is what makes the
	// residual negative and the setter publish the magnitude.
	total := seriesTotal - overshoot
	snap := &usage.Snapshot{
		Window: usage.WindowToday, Group: usage.GroupModel, Priced: true,
		Totals: usage.Counts{Requests: int64(len(series)), PriceableRequests: int64(len(series)),
			PricedRequests: int64(len(series)), CostMicros: total},
		Buckets: []usage.Bucket{{Series: series}},
	}
	snap.SetUngroupedCost(usage.CostSum{Micros: total - seriesTotal})
	if snap.SeriesOvershootMicros == nil {
		t.Fatalf("fixture premise is wrong: series %d over a total of %d published no overshoot",
			seriesTotal, total)
	}
	if snap.UngroupedCostMicros != nil {
		t.Fatalf("fixture premise is wrong: an overshoot also published a residual of %d",
			*snap.UngroupedCostMicros)
	}
	return snap
}

// TestRenderCostPane_AnOvershotBreakdownSaysNotToTrustIt.
//
// The defect this closes: usage.Snapshot.SeriesOvershootMicros was populated by both producers
// and read by nothing, so a response whose breakdown summed to MORE than its own total rendered
// byte-identically to a correct one — a reader adding the column up got a bigger number than
// the total two sections above, with nothing on screen saying the rows were wrong.
//
// The field's doc is explicit that this cannot happen to correct code: every event lands in at
// most one entry of a reconcilable group's map, so the entries sum to the total or to less. So
// the rendering has to be a DEFECT REPORT — refuse the rows, say the total is still good, and
// say where the bug is — rather than one more number in the money column.
func TestRenderCostPane_AnOvershotBreakdownSaysNotToTrustIt(t *testing.T) {
	snap := overshotSnapshot(t, map[string]int64{"m-a": 3_000_000, "m-b": 1_500_000}, 250_000)

	by := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "BY MODEL")
	flat := unwrapCostBody(by)
	// It refuses the rows out loud. "Inconsistent" on its own leaves a reader deciding for
	// themselves whether to believe a table; this tells them not to.
	if !strings.Contains(flat, "DO NOT TRUST THESE ROWS") {
		t.Errorf("the breakdown does not refuse rows that sum to more than the total:\n%s", by)
	}
	// The magnitude, because it is what makes the bug report actionable: a quarter of a dollar
	// over says one row is doubled, a figure the size of the total says the whole series is.
	if !strings.Contains(flat, "$0.2500") {
		t.Errorf("the defect report states no magnitude (want $0.2500):\n%s", by)
	}
	// And it names TOTAL as still trustworthy, which is the whole difference from a damaged
	// read. Totals is summed from the raw buckets before any grouping, so an overshoot indicts
	// the series and leaves the headline exactly as good as it was. A reader told only "this
	// answer is broken" would stop believing the one figure that is still right.
	if !strings.Contains(flat, "total itself is unaffected") {
		t.Errorf("the defect report does not say the total is still good:\n%s", by)
	}
	// It says whose bug it is. A coverage gap is the operator's to close with a rate-table
	// entry; this one is not theirs at all, and a caveat that reads like a configuration
	// warning sends them to edit a file that has nothing wrong with it.
	if !strings.Contains(flat, "defect in Cortex") || !strings.Contains(flat, "report it") {
		t.Errorf("the defect report does not say it is a bug to report:\n%s", by)
	}
	if tot := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "TOTAL"); !strings.Contains(tot, "$4.2500") {
		t.Errorf("TOTAL is not the published figure:\n%s", tot)
	}
}

// TestRenderCostPane_TheOvershootIsNotTheResidualBand.
//
// The confusion that would be worse than rendering neither. usage.Snapshot.UngroupedCostMicros
// is legitimate unattributed spend and gets a row with a figure, a share and a bar; the
// overshoot is the claim that the rows are WRONG and no chart should draw it. The field is a
// separate unsigned magnitude rather than a signed residual precisely so a consumer cannot
// render the second as the first — see usage.SetUngroupedCost — and putting it in the money
// column would put the sign back.
//
// Both directions are asserted, because either mix-up is the same defect: an overshoot must not
// draw a band, and a real residual must not read as a defect report.
func TestRenderCostPane_TheOvershootIsNotTheResidualBand(t *testing.T) {
	over := overshotSnapshot(t, map[string]int64{"m-a": 3_000_000, "m-b": 1_500_000}, 250_000)
	by := sectionOf(t, renderCostPane(over, usage.GroupModel, 120, 40), "BY MODEL")
	if strings.Contains(by, costUngroupedLabel) {
		t.Errorf("an overshoot was rendered as the (unattributed) residual band:\n%s", by)
	}
	// No share and no bar for the magnitude: those are what make the band comparable with the
	// rows, and a defect report is not comparable with anything. The bar glyph is the tell.
	for _, l := range strings.Split(by, "\n") {
		if strings.Contains(l, "$0.2500") && strings.Contains(l, "█") {
			t.Errorf("the overshoot magnitude was drawn with a share bar:\n%s", l)
		}
	}

	// And the mirror: a genuine residual is not a defect.
	band := ungroupedSnapshot(t, map[string]int64{"m-a": 3_000_000, "m-b": 1_000_000}, 250_000)
	if band.SeriesOvershootMicros != nil {
		t.Fatalf("fixture premise is wrong: a positive residual published an overshoot of %d",
			*band.SeriesOvershootMicros)
	}
	bandBy := sectionOf(t, renderCostPane(band, usage.GroupModel, 120, 40), "BY MODEL")
	if strings.Contains(bandBy, "DO NOT TRUST") {
		t.Errorf("a legitimate residual was rendered as a defect report:\n%s", bandBy)
	}
}

// TestRenderCostPane_TheOvershootLeadsTheRowsItIndicts.
//
// ORDER, not merely presence. Every other disclosure in this section follows the rows because
// it qualifies how COMPLETE they are; this one says they are WRONG, and a reader who scans the
// table and stops has read numbers they were told not to believe. There is no single figure for
// the claim to ride on as a marker — it indicts the whole series — so leading the section is the
// only form available.
func TestRenderCostPane_TheOvershootLeadsTheRowsItIndicts(t *testing.T) {
	snap := overshotSnapshot(t, map[string]int64{"m-a": 3_000_000, "m-b": 1_500_000}, 250_000)
	by := sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 40), "BY MODEL")
	note := costLineIndex(by, "DO NOT TRUST")
	firstRow := costLineIndex(by, "m-a")
	if note < 0 || firstRow < 0 {
		t.Fatalf("premise is wrong: note=%d firstRow=%d:\n%s", note, firstRow, by)
	}
	if note > firstRow {
		t.Errorf("the defect report is drawn below the rows it refuses:\n%s", by)
	}
	// Below the heading, though: the heading is what says which axis these rows are.
	if heading := costLineIndex(by, "BY MODEL"); note <= heading {
		t.Errorf("the defect report displaced the section heading:\n%s", by)
	}
}

// TestCostOvershootNote_OnlyAPublishedPositiveMagnitudeIsRendered.
//
// The absent-not-zero rule, restated on this side of the wire. A zero would have to mean both
// "checked, the series adds up" and "not checked", which is why the field is a pointer; and a
// non-positive value is not an overshoot at all. usage.SetUngroupedCost already refuses to
// publish either, and this is the same defence in depth costUngroupedRow applies to its own
// field — a guarantee inherited silently is a guarantee that stops holding without anything
// noticing.
func TestCostOvershootNote_OnlyAPublishedPositiveMagnitudeIsRendered(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *int64
		want  bool
	}{
		{"absent", nil, false},
		{"zero", ptrInt64(0), false},
		{"negative", ptrInt64(-250_000), false},
		{"positive", ptrInt64(250_000), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &usage.Snapshot{Priced: true,
				Totals:                usage.Counts{CostMicros: 4_250_000, PricedRequests: 3, PriceableRequests: 3},
				SeriesOvershootMicros: tc.value}
			got := costOvershootNote(snap)
			if (got != "") != tc.want {
				t.Fatalf("costOvershootNote = %q, want non-empty = %v", got, tc.want)
			}
		})
	}
}

// TestRenderCostPane_AnOvershootTravelsWithTheRowsAndFitsEveryWidth.
//
// Two contracts at once, because the note is prose inside the breakdown section and both apply
// to it. The height budget drops a section WHOLE — so the report never appears without the rows
// it refuses, and losing it costs nothing, since Totals still carries every dollar exactly. The
// width budget is absolute: never exceed it, and never clip a figure into a different, smaller
// number.
func TestRenderCostPane_AnOvershootTravelsWithTheRowsAndFitsEveryWidth(t *testing.T) {
	snap := overshotSnapshot(t, map[string]int64{
		"a-really-long-ascii-model-name": 3_000_000,
		"日本語モデル":                         1_500_000,
	}, 250_000)
	for _, w := range append(costWidthFloors, costHostileWidths()...) {
		for _, h := range []int{60, 40, 24, 20, 16, 8, 4, 1} {
			got := renderCostPane(snap, usage.GroupModel, w, h)
			for i, line := range strings.Split(got, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("w=%d h=%d line %d is %d columns: %q", w, h, i, lw, line)
				}
			}
			if n := len(strings.Split(got, "\n")); h > 0 && n > h {
				t.Errorf("w=%d h=%d rendered %d lines:\n%s", w, h, n, got)
			}
			// The answer survives every budget, as it must on every path in this pane.
			if !strings.Contains(got, "$4.2500") {
				t.Errorf("w=%d h=%d dropped the total:\n%s", w, h, got)
			}
			// The largest row's own figure rather than its label: a narrow terminal truncates
			// the label to "a-…" while the figure column is never clipped, so the figure is
			// what says a row is really on screen.
			if strings.Contains(got, "DO NOT TRUST") && !strings.Contains(got, "$3.0000") {
				t.Errorf("w=%d h=%d refuses rows that are not on screen:\n%s", w, h, got)
			}
			// A clipped magnitude is a different, smaller number. Held to the same rule as
			// every other figure in this pane.
			if strings.Contains(got, "$0.25") && !strings.Contains(got, "$0.2500") {
				t.Errorf("w=%d h=%d clipped the overshoot magnitude mid-digits:\n%s", w, h, got)
			}
		}
	}
}

// refusedTokenSnapshot is a window whose token figures are SHORT because reports were refused.
//
// Deliberately a HEALTHY answer in every other respect — fully priced, wholly exact, clean read
// — because that is the state the disclosure has to survive: cost is settled and bounded
// separately from the token report, so trustworthy dollars beside short tokens is the normal
// shape of this field rather than an edge of it.
func refusedTokenSnapshot(refused int64, split usage.Counts) *usage.Snapshot {
	totals := split
	totals.Requests = 400
	totals.PricedRequests = 400
	totals.PriceableRequests = 400
	totals.CostMicros = 12_500_000
	totals.RefusedTokenRequests = refused
	return &usage.Snapshot{
		Window: usage.WindowToday, BucketSeconds: 86_400, Group: usage.GroupModel, Priced: true,
		Totals:  totals,
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{"claude-opus-5": totals}}},
	}
}

// TestRenderCostPane_ARefusedTokenReportSaysTheTokensAreShort.
//
// The defect this closes: usage.Counts.RefusedTokenRequests was aggregated by the server and
// read by nothing, so a window that threw away token reports rendered a tier table
// byte-identical to a complete one. The counter exists because capping cost while leaving
// tokens unbounded is not a position that survives being stated — and a bound whose refusals
// are invisible is the same thing again one step later.
//
// It is disclosed in WHERE IT WENT rather than on TOTAL, and the wording has to carry the
// asymmetry: the figures in this section are short, the dollar figure is not.
func TestRenderCostPane_ARefusedTokenReportSaysTheTokensAreShort(t *testing.T) {
	snap := refusedTokenSnapshot(3, usage.Counts{
		Tokens: 120_000, InputTokens: 20_000, CacheReadTokens: 90_000, OutputTokens: 10_000,
		PresentKinds: kindInput | kindCacheRead | kindOutput,
	})
	got := renderCostPane(snap, usage.GroupModel, 120, 60)
	tier := unwrapCostBody(sectionOf(t, got, costTierHeading))

	if !strings.Contains(tier, "3 token reports REFUSED") {
		t.Errorf("the token section does not say reports were refused:\n%s", tier)
	}
	// SHORT, not "inexact". A refused report contributed nothing at all, so these counts are
	// missing whatever those requests really used — a different claim from a figure that is
	// present and imprecise, and the branch keeps those two apart everywhere else.
	if !strings.Contains(tier, "SHORT by an amount nothing can state") {
		t.Errorf("the caveat does not say the counts are short by an unstatable amount:\n%s", tier)
	}
	// And it says the money is fine, which is the point of putting it here.
	if !strings.Contains(tier, "dollar total is unaffected") {
		t.Errorf("the caveat does not clear the dollar figure:\n%s", tier)
	}
	// The tiers still render: the counts are short, not unknown, and withholding them would
	// report a window of measured traffic as unmeasured.
	if !strings.Contains(tier, "cache-read") {
		t.Errorf("the tier rows were withheld over a refusal:\n%s", tier)
	}
}

// TestRenderCostPane_ARefusedTokenReportDoesNotQualifyTheDollarTotal.
//
// The asymmetry, asserted rather than only documented. Cost is settled by a different producer
// and bounded twice over (pricing.MaxPlausibleRequestCostMicros, pricing.MaxCostMicros), so a
// refused token report removes nothing from Totals.CostMicros. Marking the dollar figure would
// send an operator after the one number in the answer that is right.
func TestRenderCostPane_ARefusedTokenReportDoesNotQualifyTheDollarTotal(t *testing.T) {
	snap := refusedTokenSnapshot(3, usage.Counts{
		Tokens: 120_000, InputTokens: 20_000, PresentKinds: kindInput,
	})
	if figure := costTotalFigure(snap); figure != "$12.5000" {
		t.Errorf("costTotalFigure = %q, want a bare $12.5000: a refused TOKEN report is not a "+
			"claim about the dollars", figure)
	}
	tot := unwrapCostBody(sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 60), "TOTAL"))
	if strings.Contains(tot, "REFUSED") {
		t.Errorf("the refusal caveat reached the TOTAL section:\n%s", tot)
	}
}

// TestRenderCostPane_ARefusedReportIsDisclosedWhereNoTierWasReported.
//
// The path where silence would be a FALSE NEGATIVE rather than a shortfall. With every tier
// absent the section says "no tokens recorded in this window" — and over a refused report that
// is wrong in the worst available direction: reports arrived, were rejected, and the window
// reads as quiet. This is the exit the caveat is easiest to forget on, because it returns early.
func TestRenderCostPane_ARefusedReportIsDisclosedWhereNoTierWasReported(t *testing.T) {
	for _, tc := range []struct {
		name  string
		split usage.Counts
		want  string
	}{
		{"no tokens at all", usage.Counts{}, "no tokens recorded"},
		{"only a total reported", usage.Counts{Tokens: 120_000}, "no breakdown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := refusedTokenSnapshot(2, tc.split)
			tier := unwrapCostBody(sectionOf(t,
				renderCostPane(snap, usage.GroupModel, 120, 60), costTierHeading))
			if !strings.Contains(tier, tc.want) {
				t.Fatalf("premise is wrong: the section does not take the %q path:\n%s", tc.name, tier)
			}
			if !strings.Contains(tier, "2 token reports REFUSED") {
				t.Errorf("a refusal on the %q path is not disclosed, so the section reads as a "+
					"measurement nobody refused:\n%s", tc.name, tier)
			}
		})
	}
}

// TestRenderCostPane_NoRefusalsCarryNoRefusalCaveat is the mirror, and the half that keeps the
// caveat worth reading. Zero renders nothing: a permanent line saying no reports were refused is
// the "checked, fine" claim from a producer that never checked, and it trains a reader to skip
// the one line that matters.
func TestRenderCostPane_NoRefusalsCarryNoRefusalCaveat(t *testing.T) {
	snap := refusedTokenSnapshot(0, usage.Counts{
		Tokens: 120_000, InputTokens: 20_000, CacheReadTokens: 90_000,
		PresentKinds: kindInput | kindCacheRead,
	})
	got := renderCostPane(snap, usage.GroupModel, 120, 60)
	for _, unwanted := range []string{"REFUSED", "0 token report"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a clean window rendered %q:\n%s", unwanted, got)
		}
	}
}

// TestRenderCostPane_AClampedAggregateMarksTheTotalAndSaysEveryFigureIsAFloor.
//
// usage.Counts.Saturated says an addition into these totals reached the int64 ceiling and was
// CLAMPED rather than allowed to wrap, so every figure in the answer is a floor. Its own doc
// argues the clamp is only acceptable BECAUSE the flag travels with it, and that it is
// deliberately not logged because "the disclosure travels on the same response as the number it
// qualifies, which is where an operator reading that number will see it" — a client that drops
// it is what turns the clamp back into a lie.
//
// It takes damagedMarker, not a fourth glyph: the reader's action is identical to a damaged
// read's — the number is less than the money that was spent, by an unstatable amount — and the
// words are what name the cause.
func TestRenderCostPane_AClampedAggregateMarksTheTotalAndSaysEveryFigureIsAFloor(t *testing.T) {
	snap := damagedTodaySnapshot(nil)
	snap.Totals.Saturated = true

	if figure := costTotalFigure(snap); !strings.HasPrefix(figure, damagedMarker) {
		t.Errorf("costTotalFigure = %q, want a leading %q: a clamped total is short of real "+
			"spend by an amount nothing can state", figure, damagedMarker)
	}
	tot := unwrapCostBody(sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 60), "TOTAL"))
	if !strings.Contains(tot, "every figure in this window is a FLOOR") {
		t.Errorf("TOTAL does not disclose the clamp:\n%s", tot)
	}
	// Every figure, not the dollars alone: Add clamps the whole Counts, so Requests and Tokens
	// are floors too — and Requests is the denominator of every ratio on this pane.
	if !strings.Contains(tot, "requests, tokens and cost are all larger") {
		t.Errorf("the clamp caveat names only some of the figures it invalidates:\n%s", tot)
	}
	// It says CLAMPED rather than wrapped, which is the only reason the figure on screen is
	// worth anything: the alternative was a wrapped total, a large negative presented as fact.
	if !strings.Contains(tot, "rather than allowed to wrap") {
		t.Errorf("the clamp caveat does not say the number is a floor rather than a wrap:\n%s", tot)
	}
}

// TestRenderCostPane_ACleanAggregateCarriesNoClampCaveat is the mirror. A permanent "these are
// floors" line over an aggregate that never clamped is the same false signal as a coverage
// warning that never clears.
func TestRenderCostPane_ACleanAggregateCarriesNoClampCaveat(t *testing.T) {
	snap := damagedTodaySnapshot(nil)
	if snap.Totals.Saturated {
		t.Fatal("fixture premise is wrong: the clean snapshot is already saturated")
	}
	got := renderCostPane(snap, usage.GroupModel, 120, 60)
	if strings.Contains(got, "is a FLOOR") {
		t.Errorf("a clean aggregate rendered the clamp caveat:\n%s", got)
	}
	if strings.Contains(costTotalFigure(snap), damagedMarker) {
		t.Errorf("costTotalFigure = %q wears the damaged marker over a clean read and a clean sum",
			costTotalFigure(snap))
	}
}

// TestCostTotalFigure_OneMarkerForBothWaysATotalCanBeShort.
//
// damagedMarker's two causes take ONE cell between them. A glyph per cause would say "!!" for a
// figure that is short twice over, which reads as emphasis rather than as two facts, and it
// would make the strip's vocabulary four marks deep to draw a distinction that changes nothing
// about how the number must be read. The words carry the causes; the marker carries the claim.
func TestCostTotalFigure_OneMarkerForBothWaysATotalCanBeShort(t *testing.T) {
	snap := damagedTodaySnapshot(&usage.Degraded{SkippedLines: 3})
	snap.Totals.Saturated = true
	figure := costTotalFigure(snap)
	if n := strings.Count(figure, damagedMarker); n != 1 {
		t.Errorf("costTotalFigure = %q carries %d %q cells, want exactly 1", figure, n, damagedMarker)
	}
	// And BOTH sets of words, because one sends an operator to a day file and the other to
	// whatever produced 9.2e18 micros of traffic.
	tot := unwrapCostBody(sectionOf(t, renderCostPane(snap, usage.GroupModel, 120, 60), "TOTAL"))
	for _, want := range []string{"every figure in this window is a FLOOR", "this total is SHORT"} {
		if !strings.Contains(tot, want) {
			t.Errorf("TOTAL is missing %q — one marker must not collapse two causes into one "+
				"explanation:\n%s", want, tot)
		}
	}
	// The clamp leads: it is short in every column of the aggregate, where a damaged read is
	// short in the dollars.
	if clamp, damaged := costLineIndex(tot, "is a FLOOR"), costLineIndex(tot, "total is SHORT"); clamp > damaged {
		t.Errorf("the damaged-read caveat outranks the clamp:\n%s", tot)
	}
}

// TestRenderCostPane_AClampedTotalKeepsItsMarkerAtEveryHeight.
//
// The tiny-height rule, extended to the new caveat unit. A budget may drop the WORDS — they are
// wrapped prose and a whole unit goes at a time — but the figure keeps its marker, so a clamped
// total can never be published as a settled one. This is the promise renderCostPane's fallback
// path was written to keep, and a fourth caveat unit is exactly the kind of addition that breaks
// it silently.
func TestRenderCostPane_AClampedTotalKeepsItsMarkerAtEveryHeight(t *testing.T) {
	snap := damagedTodaySnapshot(&usage.Degraded{SkippedLines: 3})
	snap.Totals.Saturated = true
	for _, w := range []int{40, 60, 80, 120} {
		for h := 1; h <= 24; h++ {
			got := renderCostPane(snap, usage.GroupModel, w, h)
			if !strings.Contains(got, damagedMarker+"~$12.5000+") {
				t.Errorf("w=%d h=%d published a figure stripped of its markers:\n%s", w, h, got)
			}
			// Whatever words survive must be whole sentences, the same rule the other caveats
			// are held to.
			flat := unwrapCostBody(got)
			for opening, closing := range costCaveatSentences {
				if strings.Contains(flat, opening) && !strings.Contains(flat, closing) {
					t.Errorf("w=%d h=%d: caveat %q is rendered without its ending %q:\n%s",
						w, h, opening, closing, flat)
				}
			}
		}
	}
}
