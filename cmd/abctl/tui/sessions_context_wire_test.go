package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/session"

	"github.com/rossoctl/cortex/core/pipeline"
)

// projected models what the TIMELINE delivers: sessionapi.summarizeEvent's inference half — the
// slices nilled and their LENGTHS recorded in MessageCount / ToolCount first.
//
// MIRRORED RATHER THAN CALLED — summarizeEvent is unexported and in another module, and core's
// own tests pin both halves of it (TestSummarizeEvent_CountsTheConversationItDrops for the counts).
//
// EVERY OTHER FIXTURE IN THIS PACKAGE BUILDS Tools BY HAND, and that is exactly how a whole suite
// stayed green while the column was blank for every row the server sent: the tool-manifest filter
// and the projection strip the same two fields, so a hand-built fixture models the SSE stream and
// nothing else. Anything asserting on the gauge against server-delivered events has to come
// through here.
func projected(events []pipeline.SessionEvent) []pipeline.SessionEvent {
	return project(events, true)
}

// projectedNoCounts is the SAME projection from a proxy that predates the counts: it strips the
// slices and states nothing in their place.
//
// That window is real — abctl and the proxy install separately, so a build between the CONTEXT
// column and MessageCount/ToolCount projects blind — and it is the shape every rebase test below
// needs, because it is the only one where the timeline genuinely cannot answer and the remembered
// figure is the sole source left.
func projectedNoCounts(events []pipeline.SessionEvent) []pipeline.SessionEvent {
	return project(events, false)
}

func project(events []pipeline.SessionEvent, counts bool) []pipeline.SessionEvent {
	out := make([]pipeline.SessionEvent, 0, len(events))
	for _, e := range events {
		c := e
		if e.Inference != nil {
			inf := *e.Inference
			if counts {
				inf.MessageCount, inf.ToolCount = len(inf.Messages), len(inf.Tools)
			}
			inf.Messages = nil
			inf.Tools = nil
			inf.ToolCalls = nil
			c.Inference = &inf
		}
		out = append(out, c)
	}
	return out
}

// THE TIMELINE ANSWERS THROUGH THE COUNTS, and could not answer at all before them.
//
// Measured against a live proxy on the same 200-event window, 41 of 62 inference responses carry a
// manifest unprojected and 0 of 62 do with view=summary — which abctl asks for on every timeline
// fetch. So the rule was evaluable on streamed events and blank on delivered ones, and the fix is
// two ints the projection records before dropping the slices.
func TestSessionContext_ReadsAProjectedTimelineThroughTheCounts(t *testing.T) {
	full := conversation("c1", time.Now(), 600, 500_000)
	if got, want := pipeline.PromptContextOf(full), 500_000; got != want {
		t.Fatalf("unprojected fixture = %d, want %d", got, want)
	}
	if got, want := pipeline.PromptContextOf(projected(full)), 500_000; got != want {
		t.Errorf("projected = %d, want %d — the counts are what make a delivered row readable",
			got, want)
	}
	// And a one-shot stays a one-shot through the projection: a manifest of zero is STATED as
	// zero, which is the same answer the slice gave.
	if got := pipeline.PromptContextOf(projected(oneShot("o1", time.Now(), 282_000))); got != 0 {
		t.Errorf("a projected one-shot = %d, want 0", got)
	}
	// A proxy that projects without stating the counts cannot be read, and must not be guessed
	// at: this is the case contextRun's remembered figure exists for.
	if got := pipeline.PromptContextOf(projectedNoCounts(full)); got != 0 {
		t.Errorf("projected with no counts = %d, want 0", got)
	}
}

// AN OLD PROXY RETURNS FULL EVENTS, so the len() arm has to stay. eventProjection treats an
// unrecognised `view` as "no projection", which is the skew a new abctl against an old proxy hits —
// and there the slices are populated while the counts are absent.
func TestSessionContext_StillReadsAnOldProxysSlices(t *testing.T) {
	evs := conversation("c1", time.Now(), 600, 500_000)
	for i := range evs {
		if evs[i].Inference != nil {
			if evs[i].Inference.MessageCount != 0 || evs[i].Inference.ToolCount != 0 {
				t.Fatal("the fixture states counts; this test is about a proxy that does not")
			}
		}
	}
	if got, want := pipeline.PromptContextOf(evs), 500_000; got != want {
		t.Errorf("pipeline.PromptContextOf = %d, want %d — counts-only reading would blank every row "+
			"served by a proxy that predates them", got, want)
	}
}

// THE REGRESSION BOTH REVIEWS CAUGHT: opening a session must not blank its gauge.
//
// The stream establishes the figure, the operator presses Enter, and the snapshot replaces every
// held event with a projected copy carrying no candidate. Dropping the running answer there — the
// obvious invalidation — turned the column into a dash for exactly the session being looked at.
func TestSessionContextFor_AProjectedSnapshotKeepsTheStreamsFigure(t *testing.T) {
	base := time.Now()
	const id = "s"
	full := conversation("c1", base, 600, 500_000)
	m := &model{events: map[string][]pipeline.SessionEvent{id: full}}
	m.sessionsTbl = newSessionsTable()

	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Fatalf("from the stream: %d, want %d", got, want)
	}

	// The real handler, and the real shape: same events, same count, projected — and WITHOUT the
	// counts, because a proxy that states them makes this a question the slice can answer and
	// stops testing the rebase. The counts-less window is the case the memory exists for.
	m.Update(snapshotLoadedMsg{id: id, events: projectedNoCounts(full), projected: true})

	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Errorf("after a view=summary snapshot: %d, want %d — the column blanked on drill-in",
			got, want)
	}
}

// An older page is projected too, and it arrives in FRONT of the folded events — so the run cannot
// be extended and must not be discarded either.
//
// THE HELD EVENTS HAVE TO BE PROJECTED for this to test anything, which the first version of it got
// wrong. With an unprojected conversation still in the slice, dropping the run and rescanning finds
// that conversation again and reports the same figure, so the assertion passed with the fix
// reverted. The production sequence is the one built here: stream, [t] (snapshot, projected), [o].
func TestSessionContextFor_AnOlderPageKeepsTheFigure(t *testing.T) {
	base := time.Now()
	full := conversation("c1", base, 600, 500_000)
	m := pagedModel(t, full)

	if got, want := m.sessionContextFor("sess-1", nil), 500_000; got != want {
		t.Fatalf("from the stream: %d, want %d", got, want)
	}
	// The snapshot leaves the slice projected — and clears the paging state, so [o] rebuilds it.
	m.Update(snapshotLoadedMsg{id: "sess-1", events: projectedNoCounts(full), projected: true})
	m.paging = map[string]*pagingState{"sess-1": {pageSizes: []int{len(full)}}}

	// Older in wall-clock terms, or applyOlderPage refuses it as a restarted session.
	older := projectedNoCounts(conversation("c0", base.Add(-time.Hour), 40, 62_000))
	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", events: older, serverOldest: 1})

	if got, want := m.sessionContextFor("sess-1", nil), 500_000; got != want {
		t.Errorf("after an older page: %d, want %d — nothing in the slice can answer this "+
			"question any more, so the remembered figure is the only source left", got, want)
	}
}

// THE DETAIL FETCH IS THE ONLY PATH THAT PUTS A MANIFEST BACK, so it is the only way a session
// abctl never streamed can ever show a gauge — and it changes an event's CONTENT at the same
// length, which the fold's length check cannot see.
func TestSessionContextFor_ADetailFetchFillsTheGauge(t *testing.T) {
	base := time.Now()
	const id = "s"
	full := conversation("c1", base, 600, 500_000)
	for i := range full {
		full[i].Seq = uint64(i + 1)
	}
	// A session abctl attached to after its traffic, served by a proxy that projects without
	// stating the counts: nothing in the timeline can answer, so the detail fetch is the only
	// source of a figure at all. Against a current proxy the snapshot answers on its own — see
	// TestSessionsTable_AnIdleSessionsSnapshotFillsTheGauge.
	m := &model{events: map[string][]pipeline.SessionEvent{id: projectedNoCounts(full)}}
	if got := m.sessionContextFor(id, nil); got != 0 {
		t.Fatalf("a projected timeline: %d, want 0 (the dash)", got)
	}

	// The operator opens the response row; the fetched event is unprojected.
	resp := full[1]
	m.replaceHeldEvent(id, &resp)

	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Errorf("after the detail fetch: %d, want %d — the write-back was invisible to the "+
			"length check", got, want)
	}
}

// A DIFFERENT POD IS A DIFFERENT WORKLOAD, and it is the one case where a remembered figure is
// void rather than merely unsupported: the same session id now names someone else's conversation.
// Matching event counts make it a cache HIT, so the wrong figure would render rather than leak.
func TestSessionContextFor_APodSwitchVoidsTheFigure(t *testing.T) {
	base := time.Now()
	m := fitModel(t, paneEvents, 200, 40, conversation("c1", base, 600, 500_000))
	m.parentCtx, m.ctx = context.Background(), context.Background()
	m.cancel = func() {}

	if got, want := m.sessionContextFor("sess-1", nil), 500_000; got != want {
		t.Fatalf("on the first pod: %d, want %d", got, want)
	}

	m.backToPodsPane()
	// The next pod happens to have a session with the same id and the same event count.
	m.events["sess-1"] = conversation("other", base.Add(time.Hour), 40, 62_000)

	if got, want := m.sessionContextFor("sess-1", nil), 62_000; got != want {
		t.Errorf("on the second pod: %d, want %d — the previous pod's context carried over",
			got, want)
	}
}

// RELEASING EVENTS FOR MEMORY IS NOT NEWS ABOUT THE SESSION. The picker drops cached events for
// sessions the server still lists; the figure is one int and stays, or a live session's gauge would
// fall back to a dash for having been economical.
func TestSessionContextFor_AReleaseOfItsEventsKeepsTheFigure(t *testing.T) {
	const id = "s"
	m := &model{events: map[string][]pipeline.SessionEvent{
		id: conversation("c1", time.Now(), 600, 500_000),
	}}
	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Fatalf("before the release: %d, want %d", got, want)
	}

	delete(m.events, id) // what the picker's prune does
	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Errorf("after the release: %d, want %d", got, want)
	}
	// And a later streamed turn still wins — on the message count here, since these fixtures
	// state no role.
	m.events[id] = conversation("c2", time.Now().Add(time.Minute), 900, 700_000)
	if got, want := m.sessionContextFor(id, nil), 700_000; got != want {
		t.Errorf("after a new turn: %d, want %d", got, want)
	}
}

// A TIE MUST STILL GO TO THE LATEST TURN ACROSS A REBASE, which is the one rule the remembered
// figure could break.
//
// Two turns of equal length, the later one winning. The snapshot projects both away, so the figure
// survives only in the run — and then the operator opens the OLDER turn, whose full event comes
// back unprojected and folds as a candidate. It ties on message count, and a fold that reads "later
// in this fold" as "later in the session" hands it the column: reproduced at 445k against a true
// 500k before contextRun carried a timestamp.
func TestSessionContextFor_ATieAcrossARebaseKeepsTheLaterTurn(t *testing.T) {
	base := time.Now()
	const id = "s"
	older := conversation("early", base, 700, 445_000)
	newer := conversation("late", base.Add(time.Hour), 700, 500_000)
	for i := range older {
		older[i].Seq = uint64(i + 1)
	}
	for i := range newer {
		newer[i].Seq = uint64(i + 3)
	}
	all := append(append([]pipeline.SessionEvent{}, older...), newer...)

	m := &model{events: map[string][]pipeline.SessionEvent{id: all}}
	m.sessionsTbl = newSessionsTable()
	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Fatalf("from the stream: %d, want %d", got, want)
	}
	m.Update(snapshotLoadedMsg{id: id, events: projectedNoCounts(all), projected: true})
	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Fatalf("after the snapshot: %d, want %d", got, want)
	}

	resp := older[1] // the detail pane fetched the older turn's response
	m.replaceHeldEvent(id, &resp)

	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Errorf("after opening the older turn: %d, want %d — an older tie took the column",
			got, want)
	}
}

// AN IDLE SESSION'S SNAPSHOT NOW FILLS THE GAUGE, with no stream history and no detail fetch.
//
// This is the regression the third review measured against main, where the rule read only
// promptTokens — which the projection keeps — so drilling into an idle row produced a figure. On
// this branch it produced a dash until the counts landed. Main's figure was the LATEST request's,
// right at 63% of drill-in moments across six live sessions and off by more than 10% at 37% of them,
// worst case 6,331 against a true 246,919; the counts make the same rows correct instead.
func TestSessionsTable_AnIdleSessionsSnapshotFillsTheGauge(t *testing.T) {
	base := time.Now()
	const id = "idle"
	// A one-shot speaks LAST, which is what made main's latest-request reading wrong here.
	evs := append(conversation("c1", base, 1509, 851_000), oneShot("o1", base.Add(time.Minute), 7_000)...)

	m := &model{events: map[string][]pipeline.SessionEvent{}}
	m.sessionsTbl = newSessionsTable()
	if got := m.sessionContextFor(id, nil); got != 0 {
		t.Fatalf("before the snapshot: %d, want 0 — abctl holds nothing for this session", got)
	}

	m.Update(snapshotLoadedMsg{id: id, events: projected(evs), projected: true})

	if got, want := m.sessionContextFor(id, nil), 851_000; got != want {
		t.Errorf("after the snapshot: %d, want %d — the conversation's figure, not the "+
			"one-shot's 7,000 and not a dash", got, want)
	}
}

// AND WITH NO ROLE STATED THE MESSAGE COUNT STILL PICKS THE MAIN THREAD through the projection,
// which the test above does not prove: there, every projected candidate loses its count equally and
// the tie-break falls through to time, so the right answer comes out for the wrong reason.
// Mutation-checked — breaking messageCount's counts arm leaves that test green and fails this one.
//
// A tool-carrying SUBAGENT is the case that needs it. It is a legitimate candidate (its own
// manifest, its own conversation) and it speaks LAST, so with nothing declaring what it is, only
// its length keeps it from taking the column from a 1509-message main thread.
func TestSessionsTable_AProjectedUnstatedTimelinePicksTheMainThread(t *testing.T) {
	base := time.Now()
	const id = "idle"
	evs := append(conversation("main", base, 1509, 851_000),
		conversation("sub", base.Add(time.Minute), 12, 40_000)...)

	m := &model{events: map[string][]pipeline.SessionEvent{}}
	m.sessionsTbl = newSessionsTable()
	m.Update(snapshotLoadedMsg{id: id, events: projected(evs), projected: true})

	if got, want := m.sessionContextFor(id, nil), 851_000; got != want {
		t.Errorf("after the snapshot: %d, want %d — the subagent spoke last and is a candidate; "+
			"with no role stated only the message count keeps the column on the main thread",
			got, want)
	}
}

// AND THE ROLE ARRIVES THROUGH THE PROJECTION, which is what lets the rule above be retired.
//
// Same shape as the test before it, inverted: the subagent speaks last AND out-messages the main
// thread — 186 against 108, which the live sessions do reach — so the message count would hand it
// the column. summarizeEvent copies the extension struct whole, so agentRole needs no counts arm
// to survive, and a projected row answers the same as a streamed one.
func TestSessionsTable_AProjectedTimelineReadsTheRole(t *testing.T) {
	base := time.Now()
	const id = "idle"
	evs := append(mainAgent("main", base, 108, 217_121),
		subagent("sub", base.Add(time.Minute), 186, 198_899)...)

	m := &model{events: map[string][]pipeline.SessionEvent{}}
	m.sessionsTbl = newSessionsTable()
	m.Update(snapshotLoadedMsg{id: id, events: projected(evs), projected: true})

	if got, want := m.sessionContextFor(id, nil), 217_121; got != want {
		t.Errorf("after the snapshot: %d, want %d — a longer, later subagent took the column, so "+
			"the projected row states no role", got, want)
	}
}

// THE FOLD MUST NOT RESCAN WHAT IT ALREADY FOLDED, which is the whole point of contextRun and was
// pinned by nothing: either branch could regress to a whole-slice scan and every other test in this
// package would stay green, because a rescan reaches the same ANSWER — 6.1ms and 3.49MB more slowly,
// per session, per arriving event, on the pane abctl opens on.
//
// So this poisons the prefix: the events already folded are overwritten with a turn that would win a
// rescan outright. A fold that trusts its prefix cannot see it; anything that re-reads the prefix
// reports 999,000. Artificial by construction — production only rewrites a held event through
// replaceHeldEvent, which rebases — and that is what makes it a clean probe of this one property.
//
// Both branches need it, and they fail to different mutations. The equal-length HIT is what the row
// loop takes on every rebuild for every session with no new events; the delta fold is what one
// arriving event costs.
func TestSessionContextFor_DoesNotRescanTheFoldedPrefix(t *testing.T) {
	base := time.Now()
	const id = "s"
	poisoned := func(t *testing.T) *model {
		t.Helper()
		m := &model{events: map[string][]pipeline.SessionEvent{
			id: conversation("c1", base, 600, 500_000),
		}}
		if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
			t.Fatalf("the first fold: %d, want %d", got, want)
		}
		// Same length, so only a re-read of the prefix can notice.
		copy(m.events[id], conversation("poison", base.Add(time.Minute), 9_000, 999_000))
		return m
	}

	t.Run("a repeat call with nothing appended", func(t *testing.T) {
		m := poisoned(t)
		if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
			t.Errorf("repeat call = %d, want %d — 999000 means the length check stopped "+
				"short-circuiting, so every row rescans on every rebuild", got, want)
		}
	})

	t.Run("a call after one appended turn", func(t *testing.T) {
		m := poisoned(t)
		m.events[id] = append(m.events[id],
			conversation("c2", base.Add(time.Hour), 700, 600_000)...)
		if got, want := m.sessionContextFor(id, nil), 600_000; got != want {
			t.Errorf("after one turn = %d, want %d — 999000 means the delta fold became a "+
				"whole-slice fold", got, want)
		}
	})
}

// THE PICKER'S RELEASE IS THE ONE m.events WRITER WITH NO TEST, and the one whose correct action is
// to do NOTHING to contextRun. A `delete(m.contextRun, cached)` there — the obvious lockstep, and
// what its neighbours do for the two maps beside it — turns a live session's gauge into a dash for
// having been economical with memory. Driven through handleKey, since the comment forbidding that
// deletion is only worth as much as the test behind it.
func TestSessionContextFor_ThePickerReleaseKeepsTheFigure(t *testing.T) {
	base := time.Now()
	m := fitModel(t, paneEvents, 200, 40, conversation("c1", base, 600, 500_000))
	m.events["other"] = conversation("c2", base, 900, 700_000)
	if got, want := m.sessionContextFor("other", nil), 700_000; got != want {
		t.Fatalf("before the release: %d, want %d", got, want)
	}

	// Selecting one session prunes the OTHER cached-but-live entries, which is the path under
	// test; both must still be listed by the server for the prune to consider them live.
	m.sessions = []session.SessionSummary{{ID: "sess-1"}, {ID: "other"}}
	m.rebuildSessionsTable()
	setCursorVisible(&m.sessionsTbl, 0)
	m.pane = paneSessions
	m.selectedSess = ""
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	released := false
	for _, id := range []string{"sess-1", "other"} {
		if _, held := m.events[id]; !held {
			released = true
			if got := m.sessionContextFor(id, nil); got == 0 {
				t.Errorf("%q had its events released and its gauge went to a dash", id)
			}
		}
	}
	if !released {
		t.Fatal("the picker released nothing, so this test is not exercising the prune")
	}
}

// heldContextCell is the CONTEXT(1M) cell of one session's row AS THE TABLE HOLDS IT.
//
// Read off Rows() rather than recomputed, because that is the whole distinction the three tests
// below exist for: bubbles/table stores the strings rebuildSessionsTable baked and View() reprints
// them, so this is what the operator is looking at and sessionContextFor is not.
func heldContextCell(t *testing.T, m *model, id string) string {
	t.Helper()
	for i, rowID := range m.sessionRowIDs {
		if rowID != id {
			continue
		}
		row := m.sessionsTbl.Rows()[i]
		return strings.TrimSpace(row[len(row)-1])
	}
	t.Fatalf("no sessions row for %q; the table holds %v", id, m.sessionRowIDs)
	return ""
}

// assertGaugeFilled fails unless the cell is a bracketed track with something drawn IN it.
//
// `!= emptyCell` is too weak, which a review caught: an EMPTY track is not the em dash, so it
// would pass, and this package calls it a fault in its own right — "an empty track beside a live
// session reads as a rendering fault" (sessions_context_test.go), the same rule tierBar states as
// a non-zero figure never drawing as an empty bar.
//
// NOT the sibling assertion's strings.Contains(cell, "█") though, which holds only for a large
// figure. Measured at this column's width: 500,000 of 1M draws "▕████▌    ▏", but 62,000 draws
// "▕▌        ▏" and 8,200 draws "▕▏        ▏" — no full block in either. Requiring one would fail
// the small figures the tests below deliberately use. Ink between the brackets is the property
// that holds for every non-zero figure.
//
// Sliced by byte offset rather than strings.Trim, and that is not pedantry: the one-eighth fill ▏
// is THE SAME RUNE as the closing bracket, so Trim(cell, "▕▏") eats an 8,200-token sliver whole
// and then reports the empty track it was written to catch.
func assertGaugeFilled(t *testing.T, cell, when string) {
	t.Helper()
	const openBracket, closeBracket = "▕", "▏"
	if !strings.HasPrefix(cell, openBracket) || !strings.HasSuffix(cell, closeBracket) {
		t.Errorf("%s the row holds %q, which is not a gauge at all", when, cell)
		return
	}
	if strings.TrimSpace(cell[len(openBracket):len(cell)-len(closeBracket)]) == "" {
		t.Errorf("%s the row holds an EMPTY track %q — the figure reached contextRun and not "+
			"the cell", when, cell)
	}
}

// assertGaugeShows fails unless the cell is EXACTLY the gauge promptTokens draws at the width the
// table built the row against — a different question from assertGaugeFilled's, and the one the
// tests below actually mean.
//
// INK BETWEEN THE BRACKETS WAS NOT ENOUGH, measured rather than argued — and the measurement is the
// DOUBLED figure. Drawing contextGauge(m.sessionContextFor(...)*2, contextW) in rebuildSessionsTable
// left the whole tui suite green before this helper existed, and fails all five row tests below with
// it: a gauge at twice the figure still has ink between its brackets, so "the row repainted" was
// pinned as "something was drawn". The figure and the cell have to be pinned TOGETHER.
//
// AND NOT THE CONSTANT GAUGE, which is the mutation the review that found this named first and which
// does not in fact isolate anything: contextGauge(1, contextW) fails
// TestSessionsTable_ContextColumnReplacesActive, which requires a full block at 500k, and
// TestSessionsTable_UnknownContextIsADash, which requires the dash — so it never reaches the tests
// below on its own account. Recorded because a reader reaching for it would conclude the weakness had
// already been closed.
//
// assertGaugeFilled IS STILL CALLED FIRST, for the diagnostic rather than for the coverage: a %q
// gauge against another %q gauge is hard to read, so the shape failures — "not a gauge at all", "an
// EMPTY track" — get to speak before the byte comparison does. Its deliberate weakness is right for
// that purpose (a small figure legitimately draws less than a full block) and wrong as the only
// assertion in a test whose name promises a figure.
//
// THE WIDTH IS READ BACK OFF THE INSTALLED HEADER rather than recomputed from m.width. The fitter
// shrinks columns on a narrow terminal and rebuildSessionsTable draws every cell to the FITTED
// width, so asking m.sessionsTbl.Columns() is asking the table what it built the row against —
// the same object heldContextCell reads the row from, which is what keeps the two in step.
func assertGaugeShows(t *testing.T, m *model, id string, promptTokens int, when string) {
	t.Helper()
	cell := heldContextCell(t, m, id)
	assertGaugeFilled(t, cell, when)
	w := sessionsColumnWidth(m.sessionsTbl.Columns(), contextColumnTitle)
	want := contextGauge(promptTokens, w)
	// A dash or an empty string would make the comparison below assert the ABSENCE of a figure,
	// which is a claim no caller of this helper is making.
	if want == "" || want == emptyCell {
		t.Fatalf("%s the gauge of %d at the fitted width %d is %q — this helper asserts a FIGURE, "+
			"so the fixture or the width is wrong", when, promptTokens, w, want)
	}
	if cell != want {
		t.Errorf("%s the row holds %q, want %q — the gauge of %d prompt tokens at width %d",
			when, cell, want, promptTokens, w)
	}
}

// THE FIGURE IS NOT THE ROW, and every test above this one stops at the figure.
//
// Which is how the reported bug survived a suite this size. TestSessionsTable_AnIdleSessionsSnapshotFillsTheGauge
// builds a sessions table, names the sessions table, and then asserts on sessionContextFor — so the
// rebase was pinned and the repaint was pinned by nothing. All three handlers that reach
// rebaseSessionContext (the inventory on model.events lists them) ended without one, while the
// fourth writer — the streamed append — has called rebuildSessionsTable all along. That asymmetry is
// the bug: a session abctl streamed shows a gauge, a session it did not shows a dash that opening
// the session corrects in contextRun and nowhere the operator can see.
//
// What that looks like from the outside, and how it was reported: on a row idle since before abctl
// attached, press Enter, come straight back out, and the gauge appears about a second later. The
// wait is the /v1/sessions poll at refreshInterval, which is simply the next thing that happens to
// rebuild the table.
//
// AND THE REPAINT MUST NOT BE GUARDED ON THE FOCUSED PANE, which is the tempting one-liner next to
// each arm's existing `if m.pane == paneEvents`. The snapshot normally lands while the operator is
// still in the timeline they just opened — milliseconds against a local proxy — and esc back to the
// sessions pane rebuilds nothing (keys.go). So `if m.pane == paneSessions` would repaint in the race
// and skip the ordinary case. The sessions table is a RETAINED component: what matters is what its
// rows say when it is next painted, not which pane is on screen when they are built.
func TestSessionsTable_ASnapshotRepaintsTheGaugeItFilled(t *testing.T) {
	base := time.Now()
	const id = "idle"
	// PARKED ON THE TIMELINE, not on the sessions pane, and that is what makes this test pin the
	// paragraph above rather than merely exercise the arm. Enter switches to paneEvents and the
	// snapshot lands there — the ordinary case — so a `m.pane == paneSessions` guard fails here.
	// Set the other way it passed with that guard in place, and so did the other two tests, which
	// each cover a different arm: the forbidden one-liner was caught by nothing at all.
	//
	// selectedSess is left empty so the arm's OWN pane check still skips rebuildEventsTable; this
	// model has no events table to rebuild.
	m := &model{width: 200, pane: paneEvents, events: map[string][]pipeline.SessionEvent{}}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{ID: id, UpdatedAt: base, EventCount: 9}}
	m.rebuildSessionsTable()

	if got := heldContextCell(t, m, id); got != emptyCell {
		t.Fatalf("before the snapshot the row holds %q, want %q — abctl has no events for a "+
			"session idle since before it attached", got, emptyCell)
	}

	// The snapshot Enter issued. A current proxy states the counts, so the projected timeline can
	// answer on its own and no detail fetch is involved.
	m.Update(snapshotLoadedMsg{
		id: id, events: projected(conversation("c1", base, 600, 500_000)), projected: true})

	if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
		t.Fatalf("the figure is %d, want %d — this test is about the ROW, which cannot be "+
			"right until the figure is", got, want)
	}
	assertGaugeShows(t, m, id, 500_000, "after the snapshot")
}

// AN OLDER PAGE IS THE SAME DEFECT AT THE SECOND SITE. [o] merges a projected page, rebases, and
// changes the gauge — for a session whose held events cannot answer, it is the first thing that can.
func TestSessionsTable_AnOlderPageRepaintsTheGauge(t *testing.T) {
	base := time.Now()
	// A proxy that projects without stating the counts, so nothing held can be read and the
	// page is the only candidate — see projectedNoCounts.
	m := pagedModel(t, projectedNoCounts(conversation("c1", base, 600, 500_000)))
	m.width = 200
	m.sessions = []session.SessionSummary{{ID: "sess-1", UpdatedAt: base, EventCount: 600}}
	m.rebuildSessionsTable()

	if got := heldContextCell(t, m, "sess-1"); got != emptyCell {
		t.Fatalf("before the page the row holds %q, want %q", got, emptyCell)
	}

	// Older in wall-clock terms, or applyOlderPage refuses it as a restarted session.
	m.Update(olderPageLoadedMsg{id: "sess-1",
		events:       projected(conversation("c0", base.Add(-time.Hour), 40, 62_000)),
		serverOldest: 1})

	if got, want := m.sessionContextFor("sess-1", nil), 62_000; got != want {
		t.Fatalf("the figure is %d, want %d", got, want)
	}
	// 62,000 of 1M draws a half-block sliver and no full block — see assertGaugeFilled, which is
	// why the exact comparison is against contextGauge's own output rather than against a literal.
	assertGaugeShows(t, m, "sess-1", 62_000, "after the older page")
}

// AND THE DETAIL FETCH IS THE THIRD, which matters most of the three: against a proxy that projects
// without stating the counts it is the only path that ever puts a manifest back, so it is the only
// thing that can give such a session a gauge at all (see replaceHeldEvent). Filling the figure and
// leaving the row alone spends a round trip on nothing the operator can see.
func TestSessionsTable_ADetailFetchRepaintsTheGauge(t *testing.T) {
	base := time.Now()
	full := conversation("c1", base, 600, 500_000)
	for i := range full {
		full[i].Seq = uint64(i + 1)
	}
	summaries := projectedNoCounts(full)

	// detailModel keys its events under "s" and parks the pane on the response row.
	m := detailModel(t, "s", &summaries[1])
	m.events["s"] = summaries
	m.width = 200
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{ID: "s", UpdatedAt: base, EventCount: len(summaries)}}
	m.rebuildSessionsTable()

	if got := heldContextCell(t, m, "s"); got != emptyCell {
		t.Fatalf("before the fetch the row holds %q, want %q — a counts-less projection is "+
			"unreadable, which is the case this path exists for", got, emptyCell)
	}

	resp := full[1] // what GetEvent returns: the manifest is back
	m.Update(detailEventLoadedMsg{sessionID: "s", seq: resp.Seq, event: &resp})

	if got, want := m.sessionContextFor("s", nil), 500_000; got != want {
		t.Fatalf("the figure is %d, want %d", got, want)
	}
	assertGaugeShows(t, m, "s", 500_000, "after the detail fetch")
}

// THE BUG THIS WHOLE CHANGE EXISTS FOR: a session idle since before abctl attached shows a gauge
// on the first /v1/sessions poll, with no Enter, no snapshot and no wait.
//
// Asserted on the RENDERED ROW rather than on sessionContextFor, because a correct figure that
// nothing paints is the defect PR #1102 fixed and this file's own tests missed.
func TestSessionsTable_AnIdleRowShowsTheServersFigureWithoutBeingOpened(t *testing.T) {
	base := time.Now()
	const id = "idle"
	m := &model{width: 200, pane: paneSessions, events: map[string][]pipeline.SessionEvent{}}
	m.sessionsTbl = newSessionsTable()

	// What the poll delivers: a row abctl holds no events for, carrying the server's figure.
	m.Update(sessionsLoadedMsg([]session.SessionSummary{{
		ID: id, UpdatedAt: base, EventCount: 1509,
		PromptContext: &pipeline.PromptContext{Tokens: 851_000, Stated: true, At: base},
	}}))

	// The SERVER's 851,000 and nothing else: abctl holds no events for this row, so an exact
	// comparison here is also what pins that rebuildSessionsTable passes s.PromptContext into
	// sessionContextFor at all rather than dropping it.
	assertGaugeShows(t, m, id, 851_000, "on the first poll")
}

// AND THE PROXY-UPGRADE REGRESSION, which is why PromptContext carries Stated.
//
// NAMED FOR THE FIGURE, NOT THE ROW, which a review corrected: this asserts on sessionContextFor,
// and a TestSessionsTable_ prefix is a promise about the rendered cell — the very confusion the
// comment above TestSessionsTable_ASnapshotRepaintsTheGaugeItFilled says let the reported bug
// survive this suite. The claim here is about the merge RULE at abctl's call site, so the name says
// so and the sessions table it used to build (and never read) is gone.
//
// The row-level half of this path is covered rather than dropped:
// TestSessionsTable_AnIdleRowShowsTheServersFigureWithoutBeingOpened pins that a server figure
// reaches the cell, and the three repaint tests above pin that a local one does.
func TestSessionContextFor_AStatedServerFigureBeatsAStaleUnstatedLocalOne(t *testing.T) {
	base := time.Now()
	const id = "s"
	// abctl's own figure, folded from a proxy that stated no roles: the documented
	// stale-fallback case, 700k held from before a compaction.
	m := &model{width: 200, pane: paneSessions, events: map[string][]pipeline.SessionEvent{
		id: conversation("pre", base.Add(-time.Hour), 2468, 700_000),
	}}
	if got := m.sessionContextFor(id, nil); got != 700_000 {
		t.Fatalf("local figure is %d, want 700000 — the fixture is not exercising the fallback", got)
	}

	server := &pipeline.PromptContext{Tokens: 200_000, Stated: true, At: base}
	if got := m.sessionContextFor(id, server); got != 200_000 {
		t.Errorf("merged to %d, want 200000 — a stated figure beats an unstated one at any "+
			"size, so max() over token counts is not the rule", got)
	}
}

// AND MERGING CAN MOVE AN UNSTATED FIGURE THE OTHER WAY, which is the mirror of the test above and
// the reason sessionContextFor's doc no longer reads as though merging can only improve a row.
//
// REPRODUCED THROUGH THE REAL PATH, not constructed: abctl attached mid-session and had followed a
// compaction correctly, while the server's fold — older, and with the longer memory — still held the
// pre-compaction turn. The unstated arm LEADS ON Msgs, so 2,468 retained messages outrank the
// client's post-compaction 952 and the merge hands the column back to the stale figure. That row
// showed 400,249 before this PR published anything.
//
// THE ORDERING IS NOT THE BUG HERE, so this test pins the behaviour rather than forbidding it. It is
// the known cost of the message-count fallback, stated in pipeline.PromptContextOf: with no role to
// read, a compaction leaves the longer pre-compaction request retained and the gauge keeps showing
// the old context. abctl only had the better answer by the accident of having attached later, which
// is not a rule the merge can prefer. If a rule is ever found that does better, it belongs in
// better() for BOTH combines, and this expectation should change there.
func TestSessionContextFor_AStaleServerFigureCanTakeTheColumnFromAFresherLocalOne(t *testing.T) {
	base := time.Now()
	const id = "s"
	// The client's own fold: unstated, and correctly following the compaction.
	m := &model{width: 200, pane: paneSessions, events: map[string][]pipeline.SessionEvent{
		id: conversation("post-compaction", base, 952, 400_249),
	}}
	if got := m.sessionContextFor(id, nil); got != 400_249 {
		t.Fatalf("local figure is %d, want 400249 — the fixture is not exercising the fallback", got)
	}

	// The server's: unstated too, ten hours older, and longer because it never lost the
	// pre-compaction turn.
	server := &pipeline.PromptContext{Tokens: 999_623, Msgs: 2468, At: base.Add(-10 * time.Hour)}
	if got := m.sessionContextFor(id, server); got != 999_623 {
		t.Errorf("merged to %d, want 999623 — with no role stated the merge ranks on message count, "+
			"so a stale server figure CAN take the column from a fresher local one; see the comment "+
			"above before changing this expectation", got)
	}
}

// An old proxy sends nothing, and nothing must not blank a row abctl can answer for itself.
//
// AT THE FIGURE, AND NAMED FOR IT: nil is the merge's identity, which is a claim about
// sessionContextFor and not about a cell. The ROW-level half has its own sibling —
// TestSessionsTable_ACachedOnlyRowDrawsItsOwnFigure passes nil at the other call site, and the three
// repaint tests above all reach rebuildSessionsTable with a summary carrying no figure — so routing
// this one through the table as well would add a fourth copy of a covered path rather than coverage.
func TestSessionContextFor_ANilServerFigureKeepsTheLocalOne(t *testing.T) {
	base := time.Now()
	const id = "s"
	m := &model{width: 200, events: map[string][]pipeline.SessionEvent{
		id: conversation("c1", base, 600, 500_000),
	}}
	if got := m.sessionContextFor(id, nil); got != 500_000 {
		t.Errorf("got %d, want 500000", got)
	}
}

// AND THE CACHED-ONLY ROW STILL DRAWS ITS GAUGE, which is the second call site — the one the server
// does not list at all, so it passes nil and abctl's own fold is the ONLY possible source.
//
// On the RENDERED ROW, for the headline test's reason: the figure is not the row. This is also the
// path a future "the server always sends it now, drop the local fold" simplification breaks, and it
// would break it silently — the server has forgotten these sessions by definition (#870), so there
// is nothing for such a change to fall back to.
//
// ITS OWN FIGURE, ASSERTED AS A FIGURE. This test read `assertGaugeFilled` until a review measured
// what that buys: replacing the cell with a constant contextGauge(1, contextW) left it green, so
// "draws its own figure" was pinned only as "draws something". It also has no `!= emptyCell`
// before-arm to lean on, unlike the three repaint tests above — the row does not exist until
// sessionsLoadedMsg builds it — which made the exactness the whole of the assertion rather than half
// of it.
func TestSessionsTable_ACachedOnlyRowDrawsItsOwnFigure(t *testing.T) {
	base := time.Now()
	const id = "vanished"
	m := &model{width: 200, pane: paneSessions, events: map[string][]pipeline.SessionEvent{
		id: conversation("c1", base, 600, 500_000),
	}}
	m.sessionsTbl = newSessionsTable()

	// The server lists nothing — a proxy restart, or any blip that empties /v1/sessions — so the
	// row comes from cachedOnlySessionIDs and carries no summary to read a figure from.
	m.Update(sessionsLoadedMsg{})

	if len(m.sessions) != 0 {
		t.Fatalf("the server list is %v, want empty — this test is about a row with no summary",
			m.sessions)
	}
	assertGaugeShows(t, m, id, 500_000, "on a cached-only row")
}
