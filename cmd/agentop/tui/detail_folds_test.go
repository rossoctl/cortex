package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
)

// turnEvent is a request on its third turn: two earlier messages, then this turn's.
func turnEvent() pipeline.SessionEvent {
	return pipeline.SessionEvent{
		Seq: 7, Phase: pipeline.SessionRequest, Direction: pipeline.Outbound, Host: "h",
		Inference: &pipeline.InferenceExtension{
			Model: "m",
			Messages: []pipeline.InferenceMessage{
				{Role: "user", Content: "first question with an old-needle"},
				{Role: "assistant", Content: "first answer"},
				{Role: "assistant", Content: "this turn's reply"},
				{Role: "user", Content: "this turn's input"},
			},
			Tools: []pipeline.InferenceTool{{Name: "Read", Description: "reads a file"}},
		},
	}
}

// openTurnDetail drills from the events pane into turnEvent's detail through the real keys.
func openTurnDetail(t *testing.T) *model {
	t.Helper()
	m := newSearchEventsModel(t, "h")
	m.events["s"] = []pipeline.SessionEvent{turnEvent()}
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 0)
	m.selectedEventKey = keyOf(m.selectedEvent())
	press(m, tea.KeyEnter)
	if m.pane != paneDetail {
		t.Fatalf("setup: Enter left pane %v, want detail", m.pane)
	}
	return m
}

// cursorTo moves the cursor down to the first line holding sub, through the real key.
func cursorTo(t *testing.T, m *model, sub string) {
	t.Helper()
	for range m.detailLines {
		if strings.Contains(stripANSI(m.detailLines[m.detailDoc.cursor].text), sub) {
			return
		}
		m.Update(keyRune('j'))
	}
	t.Fatalf("no line holds %q", sub)
}

func TestDetailFolds_EnterOpensAndClosesTheFoldAtTheCursor(t *testing.T) {
	m := openTurnDetail(t)
	if strings.Contains(detailScreen(m), "old-needle") {
		t.Fatal("setup: the earlier messages are open by default")
	}
	cursorTo(t, m, "earlier message")
	press(m, tea.KeyEnter)
	cursorTo(t, m, "▸ user")
	// A closed message's label shows its first line, so look for its content field.
	body := `"content": "first question`
	press(m, tea.KeyEnter)
	if !strings.Contains(detailScreen(m), body) {
		t.Fatalf("Enter did not open the earlier message:\n%s", detailScreen(m))
	}
	press(m, tea.KeyEnter)
	if strings.Contains(detailScreen(m), body) {
		t.Errorf("a second Enter did not close it:\n%s", detailScreen(m))
	}
	if m.pane != paneDetail {
		t.Errorf("Enter left the pane: %v", m.pane)
	}
}

func TestDetailFolds_OnlyEnterFolds(t *testing.T) {
	m := openTurnDetail(t)
	cursorTo(t, m, `"tools"`)
	for _, k := range []tea.KeyMsg{{Type: tea.KeyRight}, keyRune('l')} {
		m.Update(k)
		if strings.Contains(detailScreen(m), "reads a file") {
			t.Errorf("%v opened a fold; Enter is the only fold key", k)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if m.pane != paneEvents {
		t.Errorf("← left pane %v, want events as before", m.pane)
	}
}

func TestDetailFolds_EnterOnAPlainLineDoesNothing(t *testing.T) {
	m := openTurnDetail(t)
	cursorTo(t, m, `"model"`)
	before := detailScreen(m)
	press(m, tea.KeyEnter)
	if detailScreen(m) != before || len(m.detailFolds) != 0 {
		t.Errorf("Enter on a line that is not a fold changed the pane")
	}
}

func TestDetailFolds_EnterOnATunnelRowDoesNothing(t *testing.T) {
	m := newSearchEventsModel(t, "h")
	m.events["s"] = []pipeline.SessionEvent{{Seq: 1, Phase: pipeline.SessionRequest, Host: "h:443", Tunnel: true, HTTPMethod: "CONNECT"}}
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 0)
	m.selectedEventKey = keyOf(m.selectedEvent())
	press(m, tea.KeyEnter)
	press(m, tea.KeyEnter)
	if m.pane != paneDetail || !strings.Contains(detailScreen(m), "h:443") {
		t.Errorf("pane %v, screen:\n%s", m.pane, detailScreen(m))
	}
}

// The projected timeline renders from the summary, then the full event arrives. A fold the
// reader opened, and the cursor, survive that.
func TestDetailFolds_KeptWhenTheFullEventArrives(t *testing.T) {
	m := openTurnDetail(t)
	cursorTo(t, m, `"tools"`)
	press(m, tea.KeyEnter)
	path := m.detailCursorPath
	full := turnEvent()
	full.Inference.Completion = "arrived"
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s", seq: 7, event: &full})
	if !strings.Contains(detailScreen(m), "Read ·") {
		t.Errorf("the opened tools fold closed when the full event arrived:\n%s", detailScreen(m))
	}
	if m.detailCursorPath != path {
		t.Errorf("cursor moved from %q to %q", path, m.detailCursorPath)
	}
}

func TestDetailFolds_ResizeKeepsFoldsAndANewEventClearsThem(t *testing.T) {
	m := openTurnDetail(t)
	cursorTo(t, m, `"tools"`)
	press(m, tea.KeyEnter)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if !strings.Contains(detailScreen(m), "Read ·") {
		t.Errorf("a resize closed the fold:\n%s", detailScreen(m))
	}
	press(m, tea.KeyEsc)
	press(m, tea.KeyEnter)
	if strings.Contains(detailScreen(m), "Read ·") || len(m.detailFolds) != 0 {
		t.Errorf("opening the event again kept the old folds:\n%s", detailScreen(m))
	}
}

// A query opens the closed fold that holds its match, n puts the cursor on it, and clearing
// the query closes the fold again.
func TestDetailFolds_SearchFindsAMatchInAClosedFold(t *testing.T) {
	m := openTurnDetail(t)
	typeKeys(m, "/old-needle")
	press(m, tea.KeyEnter)
	if !strings.Contains(detailScreen(m), `"content": "first question`) {
		t.Fatalf("the match in a closed fold did not open it:\n%s", detailScreen(m))
	}
	m.Update(keyRune('n'))
	if l := stripANSI(m.detailLines[m.detailDoc.cursor].text); !strings.Contains(l, "old-needle") {
		t.Errorf("n left the cursor on %q", l)
	}
	typeKeys(m, "/")
	press(m, tea.KeyEnter) // an empty query clears the search
	if strings.Contains(detailScreen(m), `"content": "first question`) {
		t.Errorf("clearing the query left the fold open:\n%s", detailScreen(m))
	}
}

func TestDetailFolds_PluginDetailHasNoGutter(t *testing.T) {
	m := pluginDetailModel(t)
	if m.detailDoc.gutter || strings.Contains(m.detailVp.View(), "▌") {
		t.Error("the plugin detail pane draws a cursor")
	}
}

// fatTurn is shaped like the measured event: 21 messages, one of them 147KB over 1,595 lines,
// and 17 tool schemas.
func fatTurn() pipeline.SessionEvent {
	e := turnEvent()
	e.Inference.Messages = nil
	for i := 0; i < 21; i++ {
		role, content := "user", fmt.Sprintf("message %d", i)
		if i%2 == 1 {
			role = "assistant"
		}
		if i == 1 {
			role, content = "user", strings.Repeat(strings.Repeat("x", 91)+"\n", 1595)
		}
		e.Inference.Messages = append(e.Inference.Messages, pipeline.InferenceMessage{Role: role, Content: content})
	}
	for i := 0; i < 17; i++ {
		e.Inference.Tools = append(e.Inference.Tools, pipeline.InferenceTool{
			Name: fmt.Sprintf("tool%d", i), Description: strings.Repeat("d", 1800)})
	}
	return e
}

// The spec's target is under 20ms per render of a 216KB event.
func BenchmarkRenderDetail(b *testing.B) {
	e := fatTurn()
	m := &model{detailVp: newDetailViewportForBench()}
	m.showDetail(eventRow{event: &e}, true)
	b.ResetTimer()
	for b.Loop() {
		m.renderDetail()
	}
}

func newDetailViewportForBench() viewport.Model { v := viewport.New(120, 40); return v }

// The worst case: the 147KB message open, and a cursor move over it.
func BenchmarkRenderDetailAllOpen(b *testing.B) {
	e := fatTurn()
	m := &model{detailVp: newDetailViewportForBench()}
	m.showDetail(eventRow{event: &e}, true)
	m.detailFolds = map[string]bool{"inference.messages[<19]": true, "inference.messages[<19][1]": true, "inference.messages[<19][1].content": true}
	m.renderDetail()
	b.Run("render", func(b *testing.B) {
		for b.Loop() {
			m.renderDetail()
		}
	})
	b.Run("cursor", func(b *testing.B) {
		for b.Loop() {
			m.moveDetailCursor(m.detailDoc.cursor + 1)
		}
	})
}

func TestFatTurn_AllOpenShowsTheBigMessage(t *testing.T) {
	e := fatTurn()
	m := &model{detailVp: newDetailViewportForBench()}
	m.showDetail(eventRow{event: &e}, true)
	m.detailFolds = map[string]bool{"inference.messages[<19]": true, "inference.messages[<19][1]": true, "inference.messages[<19][1].content": true}
	m.renderDetail()
	if n := len(m.detailLines); n < 1595 {
		t.Fatalf("the big message did not open: %d lines", n)
	}
}
