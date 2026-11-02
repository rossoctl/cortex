package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
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
	m.detailFolds = map[string]foldChoice{"inference.messages[<19]": {open: true}, "inference.messages[<19][1]": {open: true}, "inference.messages[<19][1].content": {open: true}}
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
	m.detailFolds = map[string]foldChoice{"inference.messages[<19]": {open: true}, "inference.messages[<19][1]": {open: true}, "inference.messages[<19][1].content": {open: true}}
	m.renderDetail()
	if n := len(m.detailLines); n < 1595 {
		t.Fatalf("the big message did not open: %d lines", n)
	}
}

// The plugin detail pane draws into the event detail pane's document. `P` from the event
// detail, a plugin, then esc twice lands back on the event, which must draw itself again.
func TestDetailFolds_BackFromAPluginRedrawsTheEvent(t *testing.T) {
	m := openTurnDetail(t)
	m.client = deadClient()
	m.detailVp.Height = 4
	cursorTo(t, m, "this turn's input")
	m.pipeline = &apiclient.PipelineView{Outbound: []apiclient.PipelinePlugin{*prunePluginWithTools(0)}}
	m.handleKey(keyRune('P'))
	rows := m.pipelineTbl.Rows()
	for i := range rows {
		if !isDividerRow(rows, i) {
			setCursorVisible(&m.pipelineTbl, i)
			break
		}
	}
	press(m, tea.KeyEnter)
	if m.pane != panePluginDetail {
		t.Fatalf("setup: Enter on the pipeline left pane %v", m.pane)
	}
	press(m, tea.KeyEsc)
	press(m, tea.KeyEsc)
	if m.pane != paneDetail {
		t.Fatalf("esc from the pipeline left pane %v, want detail", m.pane)
	}
	if strings.Contains(detailScreen(m), "Plugin:") {
		t.Errorf("the event detail pane shows the plugin:\n%s", detailScreen(m))
	}
	if !strings.Contains(detailScreen(m), "▌         \"content\": \"this turn's input\"") {
		t.Errorf("the cursor line is not on screen:\n%s", detailScreen(m))
	}
	for _, k := range []tea.KeyMsg{keyRune('G'), keyRune('f'), keyRune('b'), {Type: tea.KeyPgDown}} {
		m.Update(k)
	}
}

// Esc on the search prompt puts the cursor back where it was at `/`, as it does the scroll.
func TestDetailFolds_EscOnTheSearchPutsTheCursorBack(t *testing.T) {
	for _, q := range []string{"/old-needle", "/no-such-text"} {
		m := openTurnDetail(t)
		cursorTo(t, m, `"model"`)
		path := m.detailCursorPath
		typeKeys(m, q)
		press(m, tea.KeyEsc)
		if m.detailCursorPath != path {
			t.Errorf("%s then esc: cursor on %q, want %q", q, m.detailCursorPath, path)
		}
		if l := stripANSI(m.detailLines[m.detailDoc.cursor].text); !strings.Contains(l, `"model"`) {
			t.Errorf("%s then esc: cursor line %q", q, l)
		}
	}
}

// The keys the viewport scrolled the pane with before it had a cursor move the cursor now.
func TestDetailFolds_ViewportScrollKeysMoveTheCursor(t *testing.T) {
	m := openTurnDetail(t)
	cursorTo(t, m, "earlier message")
	press(m, tea.KeyEnter)
	cursorTo(t, m, "▸ user")
	press(m, tea.KeyEnter)
	m.detailVp.Height = 4
	for _, c := range []struct {
		key  tea.KeyMsg
		down bool
	}{
		{tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, true},
		{keyRune('d'), true},
		{tea.KeyMsg{Type: tea.KeyCtrlD}, true},
		{tea.KeyMsg{Type: tea.KeyCtrlU}, false},
	} {
		before := m.detailDoc.cursor
		m.Update(c.key)
		if moved := m.detailDoc.cursor - before; (c.down && moved <= 0) || (!c.down && moved >= 0) {
			t.Errorf("%q moved the cursor by %d", c.key.String(), moved)
		}
	}
}

// A line that wraps to more rows than the view holds: each motion key shows every row of the
// document on the way through, down from the top and up from the bottom.
func TestDetailFolds_MotionKeysReachEveryRowOfATallLine(t *testing.T) {
	words := make([]string, 600)
	for i := range words {
		words[i] = "filler"
	}
	ev := turnEvent()
	ev.Inference.Messages[3].Content = strings.Join(words, " ")
	for _, c := range []struct {
		start string
		key   tea.KeyMsg
	}{
		{"g", keyRune('j')},
		{"g", tea.KeyMsg{Type: tea.KeyPgDown}},
		{"g", keyRune('d')},
		{"G", keyRune('k')},
		{"G", tea.KeyMsg{Type: tea.KeyPgUp}},
		{"G", tea.KeyMsg{Type: tea.KeyCtrlU}},
	} {
		m := openTurnDetail(t)
		m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m.events["s"] = []pipeline.SessionEvent{ev}
		m.showDetail(eventRow{event: &ev}, true)
		cursorTo(t, m, "filler filler")
		press(m, tea.KeyEnter) // open the clipped string
		if m.detailDoc.rows() <= 2*m.detailVp.Height {
			t.Fatalf("setup: %d rows on a view of %d", m.detailDoc.rows(), m.detailVp.Height)
		}
		m.Update(keyRune([]rune(c.start)[0]))
		seen := make([]bool, m.detailDoc.rows())
		for range m.detailDoc.rows() {
			for r := m.detailVp.YOffset; r < min(m.detailVp.YOffset+m.detailVp.Height, len(seen)); r++ {
				seen[r] = true
			}
			m.Update(c.key)
		}
		for r, ok := range seen {
			if !ok {
				t.Errorf("%s then %q: row %d of %d never came on screen", c.start, c.key.String(), r, len(seen))
				break
			}
		}
	}
}

// A fold the reader closed is open for a new query that matches inside it, as a fold nobody
// touched is. A fold the reader closes while that query is set stays closed.
func TestDetailFolds_ANewQueryOpensAFoldTheReaderClosed(t *testing.T) {
	m := openTurnDetail(t)
	toggle := func(sub string) {
		m.Update(keyRune('g'))
		cursorTo(t, m, sub)
		press(m, tea.KeyEnter)
	}
	for _, head := range []string{`"tools"`, "earlier message"} {
		toggle(head) // open
		toggle(head) // close
	}
	for _, q := range []string{"reads a file", "old-needle"} {
		typeKeys(m, "/"+q)
		press(m, tea.KeyEnter)
		if len(m.surface(paneDetail).matchLines()) == 0 {
			t.Errorf("/%s: %s", q, m.searchStatus(paneDetail))
		}
	}
	toggle("earlier message")
	if strings.Contains(detailScreen(m), "old-needle") {
		t.Errorf("Enter did not close a fold the query opened:\n%s", detailScreen(m))
	}
}

// A close made under a search that the reader then cleared holds as a close made with no
// search: the same search again opens the fold, because the fold holds its match. Esc on the
// prompt puts the search back, and the close with it.
func TestDetailFolds_ClearedSearchLetsTheSameSearchOpenAFold(t *testing.T) {
	closeUnder := func(q string) *model {
		m := openTurnDetail(t)
		typeKeys(m, "/"+q)
		press(m, tea.KeyEnter)
		m.Update(keyRune('g'))
		cursorTo(t, m, `"tools"`)
		press(m, tea.KeyEnter)
		return m
	}
	m := closeUnder("reads a file")
	typeKeys(m, "/")
	press(m, tea.KeyEnter) // an empty query clears the search
	typeKeys(m, "/reads a file")
	press(m, tea.KeyEnter)
	if len(m.surface(paneDetail).matchLines()) == 0 {
		t.Errorf("the same search after a clear: %s", m.searchStatus(paneDetail))
	}

	m = closeUnder("reads a file")
	typeKeys(m, "/")
	press(m, tea.KeyEsc)
	if strings.Contains(detailScreen(m), "reads a file") {
		t.Errorf("Esc on the prompt opened a fold the reader closed:\n%s", detailScreen(m))
	}
}
