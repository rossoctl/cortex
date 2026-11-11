package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// eventsTimelineModel is a model on the sessions pane with sessions "a" and "b" listed and
// none open.
func eventsTimelineModel(t *testing.T) *model {
	t.Helper()
	m := fitModel(t, paneSessions, 120, 40, nil)
	m.client = deadClient()
	m.selectedSess = ""
	m.sessionsData = nil
	m.sessions = []session.SessionSummary{{ID: "a", UpdatedAt: tlAt(2)}, {ID: "b", UpdatedAt: tlAt(1)}}
	m.timeline = timelineState{cursor: -1}
	m.rebuildSessionsTable()
	return m
}

// openSession presses Enter on the sessions-pane row of id.
func openSession(t *testing.T, m *model, id string) tea.Cmd {
	t.Helper()
	m.pane = paneSessions
	m.rebuildSessionsTable()
	for i, rid := range m.sessionRowIDs {
		if rid == id {
			m.sessionsTbl.SetCursor(i)
			return m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		}
	}
	t.Fatalf("session %q is not in the sessions table: %v", id, m.sessionRowIDs)
	return nil
}

func TestEventsTimeline_ChainStartsOnEventsPane(t *testing.T) {
	m := eventsTimelineModel(t)
	if cmd := openSession(t, m, "a"); cmd == nil {
		t.Fatalf("Enter on a session returned no cmd")
	}
	if m.pane != paneEvents || m.timeline.session != m.selectedSess || m.selectedSess != "a" {
		t.Fatalf("pane %v timeline session %q selected %q", m.pane, m.timeline.session, m.selectedSess)
	}
	if cmd := m.handleTimelineTick(timelineTickMsg{gen: m.timeline.tickGen}); cmd == nil {
		t.Errorf("a tick on the events pane did not schedule the next one")
	}
}

func TestEventsTimeline_StreamMarksStale(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	m.timeline.stale = false
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{SessionID: "b", At: tlAt(3)}})
	if m.timeline.stale {
		t.Errorf("an event of another session marked the timeline stale")
	}
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{SessionID: "a", At: tlAt(3)}})
	if !m.timeline.stale {
		t.Errorf("an event of the open session did not mark the timeline stale")
	}
}

func TestEventsTimeline_NotFoundGivesNoError(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	m.handleTimelineLoaded(timelineLoadedMsg{req: m.timeline.reqSeq, err: apiclient.ErrNotFound})
	if m.timeline.tl != nil {
		t.Errorf("a 404 left a timeline")
	}
	if out := strings.ToLower(stripANSI(m.View())); strings.Contains(out, "timeline") {
		t.Errorf("the events view shows the timeline failure:\n%s", out)
	}
}

func TestEventsTimeline_TickStopsOffPane(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	m.pane = paneSessions
	if cmd := m.handleTimelineTick(timelineTickMsg{gen: m.timeline.tickGen}); cmd != nil {
		t.Errorf("a tick on the sessions pane scheduled another")
	}
}

func TestEventsTimeline_EscFromTimelineResumes(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	m.openTimeline(m.selectedSess)
	if m.pane != paneTimeline {
		t.Fatalf("pane %v, want the timeline", m.pane)
	}
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pane != paneEvents || m.timeline.session != m.selectedSess || cmd == nil {
		t.Errorf("pane %v timeline session %q selected %q cmd %v", m.pane, m.timeline.session, m.selectedSess, cmd)
	}
}

func TestEventsTimeline_SessionChangeDropsTools(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	m.handleTimelineLoaded(timelineLoadedMsg{req: m.timeline.reqSeq, tl: sampleTimeline()})
	if m.timeline.tl == nil {
		t.Fatalf("the timeline of a did not load")
	}
	openSession(t, m, "b")
	if m.timeline.tl != nil || m.timeline.session != "b" {
		t.Errorf("opening b kept the timeline of a: session %q", m.timeline.session)
	}
}

// Esc from a pane opened over the events pane starts the stopped tick again.
func TestEventsTimeline_EscFromUsageResumes(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	gen := m.timeline.tickGen
	m.pane, m.usage.returnPane = paneUsage, paneEvents
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); m.pane != paneEvents || cmd == nil || m.timeline.tickGen == gen {
		t.Errorf("pane %v cmd %v tickGen %d (was %d)", m.pane, cmd, m.timeline.tickGen, gen)
	}
}

// The Agents overlay (A) returns from its own Esc arm, before the check above: it must also
// start the fetch again, or the tool rows stop updating.
func TestEventsTimeline_EscFromAgentsResumes(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	gen := m.timeline.tickGen
	m.pane, m.previousPane = paneAgents, paneEvents
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); m.pane != paneEvents || cmd == nil || m.timeline.tickGen == gen {
		t.Errorf("pane %v cmd %v tickGen %d (was %d)", m.pane, cmd, m.timeline.tickGen, gen)
	}
}

// A cached-only session has no timeline on the server: opening one fetches nothing, but the
// timeline drops the previous session, so that session is not kept.
func TestEventsTimeline_CachedOnlySessionFetchesNothing(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	m.events["gone"] = []pipeline.SessionEvent{{SessionID: "gone", At: tlAt(1)}}
	m.sessions = m.sessions[:1]
	m.sessionsData = map[string]SessionMetadata{}
	m.pane = paneSessions
	m.rebuildSessionsTable()
	if !slices.Contains(m.sessionRowIDs, "gone") {
		t.Fatalf("cached-only session not listed: %v", m.sessionRowIDs)
	}
	if cmd := openSession(t, m, "gone"); cmd != nil || m.timeline.session != "gone" {
		t.Errorf("cmd %v timeline session %q", cmd, m.timeline.session)
	}
}

// Esc back to the events pane of a cached-only session must not fetch: the 404 would retry on
// its backoff for as long as the pane is open.
func TestEventsTimeline_EscOnCachedOnlySessionFetchesNothing(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	m.events["gone"] = []pipeline.SessionEvent{{SessionID: "gone", At: tlAt(1)}}
	m.sessions = m.sessions[:1]
	m.sessionsData = map[string]SessionMetadata{}
	openSession(t, m, "gone")
	m.pane, m.usage.returnPane = paneUsage, paneEvents
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); m.pane != paneEvents || cmd != nil || m.timeline.loading {
		t.Errorf("Esc from Usage: pane %v cmd %v loading %v", m.pane, cmd, m.timeline.loading)
	}
	m.pane, m.previousPane = paneAgents, paneEvents
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); m.pane != paneEvents || cmd != nil || m.timeline.loading {
		t.Errorf("Esc from Agents: pane %v cmd %v loading %v", m.pane, cmd, m.timeline.loading)
	}
}

// The Timeline pane of a cached-only session fetches nothing and says why, and Esc from it
// back to the events pane fetches nothing too.
func TestEventsTimeline_TimelineOfCachedOnlySessionFetchesNothing(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	m.events["gone"] = []pipeline.SessionEvent{{SessionID: "gone", At: tlAt(1)}}
	m.sessions = m.sessions[:1]
	m.sessionsData = map[string]SessionMetadata{}
	openSession(t, m, "gone")
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")}); m.pane != paneTimeline || cmd != nil || m.timeline.loading {
		t.Fatalf("T: pane %v cmd %v loading %v", m.pane, cmd, m.timeline.loading)
	}
	if out := stripANSI(m.renderTimeline()); !strings.Contains(out, "no longer has this session") {
		t.Errorf("the Timeline pane does not say why it is empty:\n%s", out)
	}
	if cmd := m.handleTimelineTick(timelineTickMsg{gen: m.timeline.tickGen}); cmd != nil {
		t.Errorf("a tick on the Timeline pane of a cached-only session scheduled another")
	}
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); m.pane != paneEvents || cmd != nil || m.timeline.loading {
		t.Errorf("Esc from the Timeline: pane %v cmd %v loading %v", m.pane, cmd, m.timeline.loading)
	}
}

// A timeline reply for the previous session, still in flight when a cached-only session opens,
// must not land on the new session, and the previous tick chain must stop.
func TestEventsTimeline_CachedOnlyDropsInFlightReply(t *testing.T) {
	m := eventsTimelineModel(t)
	openSession(t, m, "a")
	inFlight, oldGen := m.timeline.reqSeq, m.timeline.tickGen
	m.events["gone"] = []pipeline.SessionEvent{{SessionID: "gone", At: tlAt(1)}}
	m.sessions = m.sessions[:1]
	m.sessionsData = map[string]SessionMetadata{}
	openSession(t, m, "gone")
	m.handleTimelineLoaded(timelineLoadedMsg{req: inFlight, tl: sampleTimeline()})
	if m.timeline.tl != nil || m.timeline.loading {
		t.Errorf("the reply for a landed on the cached-only session: tl %v loading %v", m.timeline.tl != nil, m.timeline.loading)
	}
	m.timeline.stale = true
	if cmd := m.handleTimelineTick(timelineTickMsg{gen: oldGen}); cmd != nil {
		t.Errorf("the tick chain of a kept running on the cached-only session")
	}
}
