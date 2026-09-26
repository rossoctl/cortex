package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"time"

	"testing"

	"github.com/rossoctl/cortex/core/usage"
)

// A reply from a superseded request must be dropped. Two rapid `w` presses leave
// two requests in flight; without an id check an out-of-order response repaints a
// stale window under the current heading.
func TestUsage_StaleReplyIsDiscarded(t *testing.T) {
	m := &model{pane: paneUsage}
	m.usage.reqSeq = 2 // two requests issued; newest is 2

	stale := usageLoadedMsg{req: 1, snap: &usage.Snapshot{
		Totals: usage.Counts{Tokens: 111},
	}}
	u, _ := m.Update(stale)
	mm := u.(*model)
	if mm.usage.snap != nil {
		t.Errorf("stale reply (req=1 vs seq=2) was applied: %+v", mm.usage.snap.Totals)
	}

	fresh := usageLoadedMsg{req: 2, snap: &usage.Snapshot{
		Totals: usage.Counts{Tokens: 222},
	}}
	u, _ = mm.Update(fresh)
	mm = u.(*model)
	if mm.usage.snap == nil || mm.usage.snap.Totals.Tokens != 222 {
		t.Error("current reply (req=2) was not applied")
	}
	if mm.usage.loading {
		t.Error("loading should clear once the current reply lands")
	}
}

// Changing a view option must clear the old snapshot, not just set loading.
// Leaving it in place renders the previous window's bars under the new heading
// until the reply arrives — the same wrong-heading failure the discard guard
// exists to prevent, from the other direction.
func TestUsage_BeginFetchClearsStaleData(t *testing.T) {
	m := &model{pane: paneUsage}
	m.usage.snap = &usage.Snapshot{Totals: usage.Counts{Tokens: 999}}
	m.usage.err = errUsageUnsupported
	before := m.usage.reqSeq

	m.beginFetch() // client is nil, so no command; state changes are what matter

	if m.usage.snap != nil {
		t.Error("beginFetch left the previous snapshot on screen")
	}
	if m.usage.err != nil {
		t.Error("beginFetch left a stale error on screen")
	}
	if !m.usage.loading {
		t.Error("beginFetch did not mark the pane loading")
	}
	if m.usage.reqSeq != before+1 {
		t.Errorf("reqSeq = %d, want %d — a new request must invalidate older replies",
			m.usage.reqSeq, before+1)
	}
}

// The poll must stop doing work once the pane loses focus.
func TestUsage_TickIgnoredOffPane(t *testing.T) {
	m := &model{pane: paneEvents}
	m.usage.tickGen = 1
	_, cmd := m.Update(usageTickMsg{gen: 1})
	if cmd != nil {
		t.Error("tick scheduled more work while the pane was not focused")
	}
}

// A tick left over from an earlier visit must not reschedule itself. Without a
// generation check, a quick exit and re-entry leaves two chains alive — each
// rescheduling its own successor — and the request rate doubles for the life of
// the session.
func TestUsage_StaleTickChainDoesNotReschedule(t *testing.T) {
	m := &model{pane: paneUsage}
	m.usage.tickGen = 2 // second visit

	if _, cmd := m.Update(usageTickMsg{gen: 1}); cmd != nil {
		t.Error("tick from the first visit rescheduled itself, doubling the poll rate")
	}
	// The current generation still polls. m.client is nil so fetchUsage yields no
	// command, but the tick half of the batch must still be scheduled.
	if _, cmd := m.Update(usageTickMsg{gen: 2}); cmd == nil {
		t.Error("current-generation tick did not reschedule")
	}
}

// esc must return to the pane Usage was opened from, even after the catalog
// overlay has been opened on top. model.previousPane is shared with the catalog,
// so relying on it sent esc to Sessions instead of Events.
func TestUsage_ReturnPaneSurvivesCatalogOverlay(t *testing.T) {
	m := &model{pane: paneEvents, selectedSess: "s1", previousPane: paneNone}
	m.openUsage("s1")
	if m.usage.returnPane != paneEvents {
		t.Fatalf("returnPane = %v, want paneEvents", m.usage.returnPane)
	}

	// Simulate opening the catalog from Usage and escaping back out of it: the
	// catalog uses previousPane and clears it on the way out.
	m.previousPane = paneUsage
	m.pane = paneCatalog
	m.previousPane = paneNone
	m.pane = paneUsage

	// Usage's own esc must still know where it came from.
	if m.usage.returnPane != paneEvents {
		t.Errorf("returnPane = %v after a catalog round trip, want paneEvents", m.usage.returnPane)
	}
}

// Switching pods must invalidate in-flight work: a reply describing the old pod
// must not land as if it described the new one, and the old polling chain must
// stop. backToPodsPane needs a fully wired model (contexts, port-forward), so
// this asserts the reset a stale reply would have to get past — that a bumped
// sequence makes the previous request's reply undeliverable.
func TestUsage_PodSwitchInvalidatesInFlight(t *testing.T) {
	m := &model{pane: paneUsage}
	m.usage.reqSeq = 5
	m.usage.tickGen = 3
	m.usage.snap = &usage.Snapshot{Totals: usage.Counts{Tokens: 42}}

	// The reset backToPodsPane performs on the usage fields.
	m.usage.snap = nil
	m.usage.err = nil
	m.usage.reqSeq++
	m.usage.tickGen++

	// A reply issued against the old pod must now be rejected.
	u, _ := m.Update(usageLoadedMsg{req: 5, snap: &usage.Snapshot{
		Totals: usage.Counts{Tokens: 999},
	}})
	if got := u.(*model); got.usage.snap != nil {
		t.Errorf("a reply from the previous pod was applied: %+v", got.usage.snap.Totals)
	}
	// And the old polling chain must not reschedule.
	if _, cmd := m.Update(usageTickMsg{gen: 3}); cmd != nil {
		t.Error("the previous pod's polling chain is still alive")
	}
}

// Cycling window and metric must stay in range rather than growing an index.
func TestUsageState_CyclesWrap(t *testing.T) {
	var u usageState
	for i := 0; i < len(usageWindows)*3; i++ {
		u.cycleWindow()
		if w, r := u.window(); w <= 0 || r <= 0 {
			t.Fatalf("cycle %d produced window=%v resolution=%v", i, w, r)
		}
	}
	seen := map[usageMetric]bool{}
	for i := 0; i < usageMetricCount*2; i++ {
		u.cycleMetric()
		seen[u.metric] = true
		if u.metric < 0 || u.metric >= usageMetricCount {
			t.Fatalf("metric %d out of range after %d cycles", u.metric, i+1)
		}
	}
	if len(seen) != usageMetricCount {
		t.Errorf("metric cycle covered %d of %d metrics", len(seen), usageMetricCount)
	}
}

// [g] must visit every grouping and return to ungrouped, so an operator can
// always get back to the sub-row-precision view without leaving the pane.
func TestUsageState_GroupCycleVisitsAllAndReturns(t *testing.T) {
	var u usageState // zero value is GroupNone ("")

	want := []usage.Group{usage.GroupStatus, usage.GroupMethod, usage.GroupPlugin, usage.GroupHost, usage.GroupNone}
	for i, w := range want {
		u.cycleGroup()
		if u.group != w {
			t.Fatalf("cycle %d: group = %q, want %q", i+1, u.group, w)
		}
	}
	// And it keeps cycling rather than sticking.
	u.cycleGroup()
	if u.group != usage.GroupStatus {
		t.Errorf("group = %q after a full lap, want status", u.group)
	}
}

// Latency needs the whiskers renderer; everything else uses bars, stacked when a
// grouping is active. Dispatch is what makes the three forms reachable at all.
func TestRenderUsageChart_PicksTheRightForm(t *testing.T) {
	const nl = "\n"
	base := time.Date(2026, 9, 6, 23, 24, 0, 0, time.UTC)
	snap := &usage.Snapshot{Buckets: []usage.Bucket{{
		At:          base,
		Counts:      usage.Counts{Requests: 10, Tokens: 1000},
		LatMeanMs:   2000,
		LatStdDevMs: 400,
		LatSamples:  10,
		Series:      map[string]usage.Counts{"200": {Requests: 10, Tokens: 1000}},
	}}}

	// Latency -> whisker glyphs, never block glyphs.
	latency := strings.Join(renderUsageChart(snap, metricLatency, usage.GroupNone, 80, 0), nl)
	if !strings.ContainsRune(latency, whiskerMean) {
		t.Error("latency did not use the whiskers renderer")
	}
	if strings.ContainsAny(latency, "▁▂▃▄▅▆▇█") {
		t.Error("latency drew bars")
	}
	// Latency ignores grouping: there is no per-label latency to break down.
	grouped := strings.Join(renderUsageChart(snap, metricLatency, usage.GroupStatus, 80, 0), nl)
	if grouped != latency {
		t.Error("grouping changed the latency chart, implying a breakdown that does not exist")
	}

	// Grouped counts -> a legend; ungrouped -> none.
	stacked := strings.Join(renderUsageChart(snap, metricTokens, usage.GroupStatus, 80, 0), nl)
	if !strings.Contains(stripANSI(stacked), "200 (") {
		t.Error("grouped chart has no legend")
	}
	plain := strings.Join(renderUsageChart(snap, metricTokens, usage.GroupNone, 80, 0), nl)
	if strings.Contains(plain, "200 (") {
		t.Error("ungrouped chart rendered a legend")
	}
}

// Returning into Usage from the catalog overlay must restart the poll chain.
// The tick in flight when the catalog opened was dropped by the focus guard, so
// without an explicit restart the 20s auto-refresh is silently dead: the chart
// freezes and only `r` (a one-shot refetch) or backing all the way out and
// re-entering with `u` brings it back.
func TestUsage_CatalogRoundTripRestartsPolling(t *testing.T) {
	m := &model{pane: paneEvents, selectedSess: "s1", previousPane: paneNone}
	m.openUsage("s1")
	genBefore := m.usage.tickGen

	// P opens the catalog from Usage; previousPane records where to return.
	m.previousPane = paneUsage
	m.pane = paneCatalog

	// A tick from the pre-catalog chain arrives while the catalog has focus and
	// is dropped — this is what leaves nothing scheduled.
	if _, cmd := m.Update(usageTickMsg{gen: genBefore}); cmd != nil {
		t.Error("a tick was rescheduled while the catalog had focus")
	}

	// esc out of the catalog, back into Usage.
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pane != paneUsage {
		t.Fatalf("pane = %v after esc from catalog, want paneUsage", m.pane)
	}
	if cmd == nil {
		t.Fatal("returning into Usage scheduled no work — the poll chain is dead")
	}
	if m.usage.tickGen == genBefore {
		t.Error("tickGen unchanged: the stale chain was not invalidated")
	}

	// The new chain must be the one that polls, and the old generation must stay
	// dead so only one chain is alive.
	if _, c := m.Update(usageTickMsg{gen: m.usage.tickGen}); c == nil {
		t.Error("the restarted chain does not reschedule")
	}
	if _, c := m.Update(usageTickMsg{gen: genBefore}); c != nil {
		t.Error("the pre-catalog chain is still alive — two chains would double the poll rate")
	}
}

// The header must not claim a breakdown the chart is not showing. Latency has no
// per-label data, so renderUsageChart ignores the group — a header reading "by
// status" over a bucket-wide mean asserts a breakdown that does not exist.
func TestRenderUsage_HeaderDoesNotClaimLatencyBreakdown(t *testing.T) {
	m := &model{pane: paneUsage, width: 80, bodyHeight: 24}
	m.usage.group = usage.GroupStatus
	m.usage.snap = &usage.Snapshot{Buckets: []usage.Bucket{{
		At: time.Date(2026, 9, 6, 23, 24, 0, 0, time.UTC),
	}}}

	m.usage.metric = metricTokens
	if got := m.renderUsage(80, 24); !strings.Contains(got, "by status") {
		t.Error("count metric header omits the active breakdown")
	}

	m.usage.metric = metricLatency
	got := m.renderUsage(80, 24)
	if strings.Contains(got, "by status") {
		t.Errorf("latency header claims a breakdown the chart ignores:\n%s", firstLine(got))
	}
	if !strings.Contains(got, "no breakdown for latency") {
		t.Errorf("latency header does not say why there is no breakdown:\n%s", firstLine(got))
	}

	// The selection survives, so cycling back to a count metric restores it.
	if m.usage.group != usage.GroupStatus {
		t.Errorf("group = %q, want the selection preserved", m.usage.group)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
