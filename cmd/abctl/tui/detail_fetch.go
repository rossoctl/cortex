package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
)

// detailEventLoadedMsg carries the full event the detail pane asked for.
type detailEventLoadedMsg struct {
	sessionID string
	seq       uint64
	event     *pipeline.SessionEvent
	err       error
}

// needsFullEvent reports whether the detail pane has to fetch before it can show
// everything.
//
// Three conditions. The server must actually be projecting — against a proxy that
// predates view=summary the timeline already carries whole events, and re-fetching
// would be a round trip for bytes abctl is holding. The event must have a protocol
// extension at all: a CONNECT tunnel or a bare denial has no message body to be
// missing. And this event must not already have been fetched.
//
// THE LAST CONDITION IS TRACKED, NOT INFERRED, and that is not fussiness. The
// obvious alternative — "does it carry any field the projection drops?" — cannot
// work for response events. Messages and Tools are populated on the REQUEST
// extension only (inferenceparser/plugin.go), ToolCalls only when the model called
// a tool, and Completion is KEPT by the projection because the events filter
// searches it. So a plain text response has none of the dropped fields populated
// even after a successful fetch, and a presence check would refetch it on every
// open and permanently tell the operator its bodies had not arrived.
func (m *model) needsFullEvent(e *pipeline.SessionEvent) bool {
	if !m.serverProjects || e == nil {
		return false
	}
	if e.Inference == nil && e.A2A == nil && e.MCP == nil {
		return false
	}
	return !m.fullFetched[m.selectedSess][e.Seq]
}

// markFullFetched records that this event's bodies have been retrieved.
//
// Keyed by session and Seq, and reset with m.events, so it cannot outlive the rows
// it describes.
func (m *model) markFullFetched(sessionID string, seq uint64) {
	if seq == 0 {
		return
	}
	if m.fullFetched == nil {
		m.fullFetched = map[string]map[uint64]bool{}
	}
	if m.fullFetched[sessionID] == nil {
		m.fullFetched[sessionID] = map[uint64]bool{}
	}
	m.fullFetched[sessionID][seq] = true
}

// fetchDetailEventCmd fetches one event in full for the detail pane.
//
// The row is rendered from the summary FIRST and this fills the bodies in when it
// arrives — deliberately, rather than blocking on a spinner. Everything the
// summary carries is worth reading immediately (status, duration, tokens, every
// plugin invocation), the fetch is milliseconds against a local proxy, and a
// timeline that opens instantly only to show "loading…" would have traded one
// wait for another.
//
// Carries the session id and seq so the handler can drop a reply for a row the
// operator has already navigated away from.
func fetchDetailEventCmd(m *model, sessionID string, seq uint64) tea.Cmd {
	ctx, client := m.ctx, m.client
	return func() tea.Msg {
		ev, err := client.GetEvent(ctx, sessionID, seq)
		return detailEventLoadedMsg{sessionID: sessionID, seq: seq, event: ev, err: err}
	}
}

// applyDetailEvent installs a fetched full event and re-renders, or reports why it
// could not.
//
// Ignores a reply that no longer matches what is on screen: the operator can move
// the cursor or leave the pane while the fetch is in flight, and a late reply must
// not redraw the detail of a row they are no longer looking at.
func (m *model) applyDetailEvent(msg detailEventLoadedMsg) {
	if m.pane != paneDetail || m.detailEvent == nil {
		return
	}
	if m.selectedSess != msg.sessionID || m.detailEvent.Seq != msg.seq {
		return
	}
	if msg.err != nil {
		// A footer flash rather than replacing the pane: what is already rendered is
		// correct and useful, it is only missing the bodies, so destroying it to
		// report the failure would be a net loss of information.
		m.setFlash("could not load the full event: " + msg.err.Error())
		return
	}
	// Two records, and they do different jobs. The write-back puts the bodies in the
	// slice abctl holds, so re-opening the row PAINTS complete instead of
	// body-less; the mark stops needsFullEvent asking for them again, so the second
	// open costs nothing. Without the mark the first paint would still be right and
	// the round trip would still be paid every time.
	//
	// Matched on Seq rather than position: the slice is rebuilt from the stream and
	// snapshots, so an index taken when the fetch was issued may not be the same row
	// by the time it lands.
	m.replaceHeldEvent(msg.sessionID, msg.event)
	m.markFullFetched(msg.sessionID, msg.seq)

	// AND THE SESSIONS ROW, because replaceHeldEvent rebased the CONTEXT(1M) gauge and that
	// table holds BAKED cells — see the snapshot arm in Update for the full reason, and for why
	// the repaint is not guarded on the focused pane.
	//
	// It matters most here of the three rebasing sites. Against a proxy that projects without
	// stating the counts this fetch is the ONLY path that ever puts a manifest back, so it is the
	// only thing that can give such a session a gauge at all; leaving the row baked as a dash
	// spends the round trip on a figure the operator cannot see.
	m.rebuildSessionsTable()

	// Re-render through showDetail so the tunnel/TLS headers, wrapping and scroll
	// position are all rebuilt exactly as the first render built them.
	m.detailRow.event = msg.event
	m.showDetail(m.detailRow, false)
}

// replaceHeldEvent swaps the stored event with the same Seq for the full one.
//
// A no-op when the session is gone or the Seq is not held — both happen normally
// (a pod switch, FIFO eviction) and neither is worth reporting: the detail pane
// already has what it fetched.
func (m *model) replaceHeldEvent(sessionID string, full *pipeline.SessionEvent) {
	if full == nil || full.Seq == 0 {
		return
	}
	held := m.events[sessionID]
	for i := range held {
		if held[i].Seq == full.Seq {
			held[i] = *full
			// AND THE GAUGE IS RE-FOLDED, because this write changes the slice's CONTENT at
			// the same length and sessionContextFor's length check cannot see that.
			//
			// Not a hypothetical staleness: this is the one path that puts a manifest and a
			// message count back into a projected timeline, so it is the only thing that can
			// give the CONTEXT(1M) column an answer for a session abctl never streamed. Held
			// behind a cache hit, the operator would open the very event that established the
			// figure and watch the column go on showing a dash.
			m.rebaseSessionContext(sessionID, held)
			return
		}
	}
}

// detailIsProjected reports that the event on screen is still a summary — the
// bodies have not arrived, because the fetch is in flight or it failed.
//
// Exactly needsFullEvent: "we would still ask for this event's bodies" and "the
// bodies are not here" are the same statement, so they must not be two lists that
// can disagree. They were, and the second one was wrong — it tested Messages and
// Tools but not ToolCalls, so a tool-only response turn stayed "projected" forever
// and `y` told the operator to re-yank an event that was already complete.
//
// Used to warn on yank: `y` writes the event's JSON out for debugging, and handing
// somebody a body-less event that looks complete wastes an afternoon.
func (m *model) detailIsProjected() bool {
	return m.needsFullEvent(m.detailEvent)
}
