package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/pipeline"
)

// sessionContextFor is the gauge's figure for one session: abctl's own remembered fold merged
// with whatever the server published for that row.
//
// NEITHER SOURCE DOMINATES, which is why this merges rather than preferring one. The server has
// seen everything since the PROXY started; abctl only since IT attached, which is usually less —
// but abctl's copy survives a proxy restart, and destroying a figure it still holds because the
// server forgot is #870's shape. pipeline.MergePromptContext resolves it by the rule rather than by
// size: a stated figure beats an unstated one at any magnitude.
//
// AND MERGING MOVES THE FIGURE EITHER WAY, which "neither source dominates" must not be read as
// denying: this is a max over a ranking, not an improvement. For an UNSTATED session the ranking
// leads on message count and the server's fold usually has the longer memory, so a turn abctl has
// already moved past can come back:
//
//	local   unstated, fresh, msgs   952, 400,249   had correctly followed a compaction
//	server  unstated, stale, msgs 2,468, 999,623   still holds the pre-compaction turn
//	merged                          999,623
//
// That obeys the documented ordering rather than defeating it — it is the known cost of the
// message-count fallback (see pipeline.PromptContextOf: a compaction leaves the longer
// pre-compaction request retained and the gauge keeps showing the old context), and abctl had the
// better answer only by the accident of having attached later, which is not something the rule can
// prefer. TestSessionContextFor_AStaleServerFigureCanTakeTheColumnFromAFresherLocalOne pins it, as
// the mirror of the stated-beats-unstated case.
//
// server is nil for a proxy older than the field and for a session with no conversation to
// measure. Both mean "nothing known", both are the merge's identity, and that is what lets this
// need no version detection at all.
//
// THROUGH TokensMergedWith RATHER THAN pipeline.MergePromptContext, and the reason is the row loop
// that calls this: publishing the local fold just to merge it put one 48-byte *PromptContext on the
// heap per row per rebuild — 356 ns/op and 10 allocs/op on
// BenchmarkSessionContextPerEvent/folded/10000, against 184 before a server figure existed and 253
// with no allocation as shipped. Same total order, same answer; see
// pipeline.PromptContextFold.TokensMergedWith for the rest of the figures, for why the escape cannot
// be optimised away instead, and for what the equivalence rests on.
func (m *model) sessionContextFor(id string, server *pipeline.PromptContext) int {
	return m.localContextFor(id).TokensMergedWith(server)
}

// localContextFor is the fold abctl maintains itself, unchanged from before the server published
// anything — see the retention inventory on model.events for why it remembers the winning turn
// rather than caching the slice.
//
// FOLDED RATHER THAN RESCANNED. Appending is the only growth path that preserves the prefix, so a
// longer slice folds just its tail. Every path that does something else to m.events owes this
// function an action — see the inventory on model.events — because a replacement of the SAME length
// is invisible to the length check below.
//
// RETURNS THE FOLD BY VALUE, not a published figure: this runs once per visible row per rebuild, and
// a fold copy is registers where a Publish() is a heap allocation.
func (m *model) localContextFor(id string) pipeline.PromptContextFold {
	events := m.events[id]
	run, ok := m.contextRun[id]
	switch {
	case ok && run.Folded() == len(events):
		return run
	case ok && run.Folded() < len(events):
		run.AddAll(events[run.Folded():])
		m.contextRun[id] = run
	default:
		// FEWER EVENTS THAN THE RUN FOLDED, or no run at all: the prefix cannot be trusted,
		// so the slice is re-folded — but from the figure already established, not from zero.
		// The picker's release drops a live session's events to reclaim memory
		// (keys.go), which is a decision about storage and not new information about the
		// session, so zeroing here would turn the gauge into a dash for a session still
		// sending traffic.
		m.rebaseSessionContext(id, events)
		return m.contextRun[id]
	}
	return run
}

// rebaseSessionContext re-folds a session whose events were REPLACED rather than appended to,
// keeping the figure it had already established.
//
// Called wherever the length check cannot interpret what happened: the snapshot load (a projected
// copy of the same window, often the same length), the older-page merge (older events land before
// the folded ones, and the page cap can drop newer ones off the end), the detail pane's write-back
// (one event swapped in place for its full self, length unchanged), and localContextFor's own
// fallback for a slice shorter than the run.
//
// KEEPS tokens, msgs AND at, and re-folds the whole new slice on top of them. Keeping the figure is
// what makes the column survive a snapshot from a proxy whose projection states no counts — see
// pipeline.PromptContextOf for what the timeline can and cannot say. Re-folding rather than just re-basing n
// is what lets the new slice WIN: nothing here assumes the replacement is poorer, so a detail fetch that puts a longer
// conversation back in place beats the remembered one on message count exactly as a streamed turn
// would.
//
// Costs one whole-slice fold per replacement, which is a keystroke rather than an arriving event:
// 0.51ms at 100k events, no allocation.
//
// THERE IS DELIBERATELY NO forgetSessionContext. Dropping the entry is never the right answer to a
// session's events changing: against a projection that states no counts, this figure is the only
// source there is. The one thing that voids it is a DIFFERENT WORKLOAD behind the same session id,
// which backToPodsPane handles by nilling the whole map beside m.events.
func (m *model) rebaseSessionContext(id string, events []pipeline.SessionEvent) {
	// The previous run with its COUNTER reset, rather than a field-by-field copy of it. Naming
	// the fields to carry is how the At tie-break went missing from exactly this literal: a
	// rebase seeded tokens and msgs, dropped at, and an older turn of equal length then took the
	// column. Reset-what-changes carries the next field added to pipeline.PromptContextFold by
	// default.
	prev := m.contextRun[id]
	prev.ResetFolded()
	prev.AddAll(events)
	if m.contextRun == nil {
		m.contextRun = map[string]pipeline.PromptContextFold{}
	}
	m.contextRun[id] = prev
}

// contextGauge draws prompt tokens against contextWindowTokens, in exactly width columns.
//
// A BRACKETED TRACK, because the brackets are what make the scale visible on every row. The
// three designs considered were this, a dotted remainder ("███▎····") and a single eighth-block
// cell; the brackets earn their two columns twice over. A session at 0.8% draws "▕▏       ▏",
// which reads as nearly-empty-out-of-something, where a bare "▏" floating in whitespace reads as
// a rendering fault — and the em dash for an unknown context stays unmistakably different from
// both, which it cannot be when an almost-empty bar is itself almost nothing.
//
// No ░ for the remainder. usage_glyphs.go rejected shaded blocks after measuring them: "█
// against ▓ is nearly indistinguishable at a glance in most terminal fonts", and a track a
// reader cannot separate from the fill is worse than no track.
//
// Fill arithmetic is tierBar's, reused rather than restated, which brings its rule with it: a
// non-zero value never renders as an empty bar. That rule is the whole reason a 8,200-token
// context still shows a sliver.
//
// Exactly width columns, so every row's gauge starts and ends in the same place and the fills can
// be compared down the column by eye. That is also what makes the column's alignment moot — see
// sessionsRightAligned, which lists it for the em dash's sake.
func contextGauge(promptTokens, width int) string {
	// Three is brackets plus one cell of track. Below that there is nothing to draw, and a
	// bracket pair alone would claim a scale it cannot show.
	if width < 3 {
		return ""
	}
	if promptTokens <= 0 {
		return emptyCell
	}
	budget := width - 2
	bar := tierBar(int64(promptTokens), contextWindowTokens, budget)
	return "▕" + bar + strings.Repeat(" ", budget-lipgloss.Width(bar)) + "▏"
}

// cachedMarker names a row whose events abctl holds and the server no longer lists (#870).
//
// A word rather than a glyph: it has to survive rendering under a colour profile, and bubbles
// truncates each cell with runewidth.Truncate BEFORE styling — see
// TestSessionsPicker_CachedMarkerRendersIntact, which exists because a styled cell had its
// escape bytes measured against the column width and came out mangled.
const cachedMarker = "cached"
