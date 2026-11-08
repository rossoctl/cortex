package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
)

// turnExchange is turnEvent's request with a response that has enough lines to fill a short
// viewport, so the RESPONSE title can scroll to the top.
func turnExchange(at time.Time) (req, resp pipeline.SessionEvent) {
	req = turnEvent()
	req.At, req.SessionID, req.RequestID = at, "s", "x"
	resp = pipeline.SessionEvent{
		Seq: 8, At: at.Add(2 * time.Second), SessionID: "s", Direction: pipeline.Outbound,
		Phase: pipeline.SessionResponse, Host: "h", RequestID: "x", StatusCode: 200,
		Duration:  2 * time.Second,
		Inference: &pipeline.InferenceExtension{Model: "m", Completion: "the-answer", InputTokens: 70_600, OutputTokens: 9},
		Plugins:   map[string]json.RawMessage{},
	}
	for i := range 12 {
		resp.Plugins[fmt.Sprintf("p%02d", i)] = json.RawMessage("1")
	}
	return req, resp
}

// openDetail opens the detail of the events pane's first row, with the clock at now, on a
// terminal short enough that the page scrolls.
func openDetail(t *testing.T, now time.Time, events ...pipeline.SessionEvent) *model {
	t.Helper()
	m := newSearchEventsModel(t)
	m.height = 16
	m.layout()
	m.now = func() time.Time { return now }
	m.events["s"] = events
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 0)
	m.selectedEventKey = keyOf(m.selectedEvent())
	press(m, tea.KeyEnter)
	if m.pane != paneDetail {
		t.Fatalf("setup: Enter left pane %v, want detail", m.pane)
	}
	return m
}

func exchangeDetail(t *testing.T) *model {
	t.Helper()
	at := time.Now().Add(-time.Minute)
	req, resp := turnExchange(at)
	return openDetail(t, at.Add(time.Minute), req, resp)
}

func detailText(m *model) string {
	var b strings.Builder
	for _, l := range m.detailLines {
		b.WriteString(stripANSI(l.text) + "\n")
	}
	return b.String()
}

func cursorPath(m *model) string { return m.detailLines[m.detailDoc.cursor].path }

func TestDetail_TwoSectionsInOrder(t *testing.T) {
	text := detailText(exchangeDetail(t))
	r, s := strings.Index(text, "━━ REQUEST"), strings.Index(text, "━━ RESPONSE")
	if r < 0 || s < 0 || r > s {
		t.Fatalf("want REQUEST then RESPONSE titles:\n%s", text)
	}
	if !strings.Contains(text, "the-answer") {
		t.Errorf("the response's completion is missing:\n%s", text)
	}
}

func TestDetail_ResponseLeavesOutSharedFields(t *testing.T) {
	m := exchangeDetail(t)
	var reqHost, respHost bool
	for _, l := range m.detailLines {
		reqHost = reqHost || l.path == "request.host"
		respHost = respHost || l.path == "response.host"
	}
	if !reqHost || respHost {
		t.Errorf("request.host shown %v, response.host shown %v: want the host once, in the request", reqHost, respHost)
	}
}

func TestDetail_FixedLineFollowsTheScroll(t *testing.T) {
	m := exchangeDetail(t)
	if m.detailVp.Height != m.bodyHeight-1 {
		t.Errorf("viewport height %d, want %d: the fixed line takes one row", m.detailVp.Height, m.bodyHeight-1)
	}
	line := stripANSI(m.detailFixedLine())
	if !strings.HasPrefix(line, "REQUEST │") || !strings.Contains(line, "#1") || !strings.Contains(line, "200") {
		t.Errorf("fixed line = %q, want REQUEST, the row's # and its status", line)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if line := stripANSI(m.detailFixedLine()); !strings.HasPrefix(line, "RESPONSE │") {
		t.Errorf("after Tab: fixed line = %q, want RESPONSE", line)
	}
}

func TestDetail_TabJumpsBetweenTitles(t *testing.T) {
	m := exchangeDetail(t)
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if l := m.detailLines[m.detailDoc.cursor]; l.path != "response" || !l.head {
		t.Errorf("after Tab the cursor is on %q, want the RESPONSE title", stripANSI(l.text))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if l := m.detailLines[m.detailDoc.cursor]; l.path != "request" || !l.head {
		t.Errorf("after a second Tab the cursor is on %q, want the REQUEST title", stripANSI(l.text))
	}
}

func TestDetail_EnterFoldsSection(t *testing.T) {
	m := exchangeDetail(t)
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	press(m, tea.KeyEnter)
	for _, l := range m.detailLines {
		if strings.HasPrefix(l.path, "response.") {
			t.Fatalf("line %q still shows: the RESPONSE section did not fold", stripANSI(l.text))
		}
	}
}

func TestDetail_SearchCrossesSections(t *testing.T) {
	m := exchangeDetail(t)
	typeKeys(m, "/the-answer")
	press(m, tea.KeyEnter)
	m.Update(keyRune('n'))
	if got := sectionOf(cursorPath(m)); got != "response" {
		t.Errorf("the search left the cursor in %s, want the match in the response", got)
	}
}

func TestDetail_MissingHalfNotes(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	req, resp := turnExchange(at)
	denied := req
	denied.Phase = pipeline.SessionDenied
	denied.Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "guard", Action: pipeline.ActionDeny}}}
	tunnel := pipeline.SessionEvent{
		At: at, SessionID: "s", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Tunnel: true, TunnelReason: "passthrough-host", Host: "api.test:443", RequestID: "t",
	}
	for name, tc := range map[string]struct {
		now    time.Time
		events []pipeline.SessionEvent
		want   string
	}{
		"in flight":     {at.Add(time.Minute), []pipeline.SessionEvent{req}, "waiting for response…"},
		"lost":          {at.Add(20 * time.Minute), []pipeline.SessionEvent{req}, "no response recorded"},
		"denied":        {at.Add(time.Minute), []pipeline.SessionEvent{denied}, "denied by guard: no upstream call"},
		"response only": {at.Add(time.Minute), []pipeline.SessionEvent{resp}, "request not loaded: load older events in events"},
		"open tunnel":   {at.Add(6 * time.Hour), []pipeline.SessionEvent{tunnel}, "no close recorded yet"},
	} {
		if text := detailText(openDetail(t, tc.now, tc.events...)); !strings.Contains(text, tc.want) {
			t.Errorf("%s: want %q in:\n%s", name, tc.want, text)
		}
	}
}

// A denial that follows its request is the RESPONSE half, not a missing one.
func TestDetail_DenialIsTheResponseHalf(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	req, _ := turnExchange(at)
	denied := pipeline.SessionEvent{
		Seq: 8, At: at.Add(time.Millisecond), SessionID: "s", Direction: pipeline.Outbound,
		Phase: pipeline.SessionDenied, Host: "h", RequestID: "x", StatusCode: 403,
		Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "egress-allowlist", Action: pipeline.ActionDeny}}},
	}
	text := detailText(openDetail(t, at.Add(20*time.Minute), req, denied))
	if !strings.Contains(text, "━━ RESPONSE · "+denied.At.Format("15:04:05.00")+" · deny egress-allowlist") {
		t.Errorf("want the denial as the RESPONSE section:\n%s", text)
	}
	for _, note := range []string{"no response recorded", "waiting for response…", "lost"} {
		if strings.Contains(text, note) {
			t.Errorf("found %q, want no missing-half note:\n%s", note, text)
		}
	}
}

// yanked reads the file the footer names.
func yanked(t *testing.T, m *model) string {
	t.Helper()
	path, ok := strings.CutPrefix(m.flash, "yanked → ")
	if !ok {
		t.Fatalf("flash = %q, want a yank", m.flash)
	}
	path, _, _ = strings.Cut(path, "  (")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDetail_YankWritesCursorHalf(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := exchangeDetail(t)
	m.Update(keyRune('y'))
	if got := yanked(t, m); !strings.Contains(got, `"phase": "request"`) {
		t.Errorf("y in REQUEST wrote:\n%s", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(keyRune('y'))
	if got := yanked(t, m); !strings.Contains(got, `"phase": "response"`) || !strings.Contains(got, `"host": "h"`) {
		t.Errorf("y in RESPONSE wrote:\n%s\nwant the full response event, host included", got)
	}
}

// Review focus: the cursor sits in a missing half.
func TestDetail_YankMissingHalfWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	at := time.Now().Add(-time.Minute)
	req, _ := turnExchange(at)
	m := openDetail(t, at.Add(time.Minute), req)
	if hint := m.helpView(); !strings.Contains(hint, "[y] yank request") {
		t.Errorf("hint in REQUEST = %q, want the yank", hint)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if hint := m.helpView(); strings.Contains(hint, "[y]") {
		t.Errorf("hint in the missing RESPONSE = %q, want no yank", hint)
	}
	m.Update(keyRune('y'))
	if want := "nothing to yank: waiting for response…"; m.flash != want {
		t.Errorf("flash = %q, want %q: the section's own note", m.flash, want)
	}
	if files, _ := filepath.Glob(filepath.Join(home, ".cortex", "agentop-events", "*")); len(files) > 0 {
		t.Errorf("y wrote %v for a missing half", files)
	}
}

// Review focus: a row whose request is not held. The page opens on the REQUEST note, Tab
// goes to the response, and a second Tab comes back to the note.
func TestDetail_TabWithOneHalf(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	_, resp := turnExchange(at)
	m := openDetail(t, at.Add(time.Minute), resp)
	for _, want := range []string{"response", "request"} {
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
		if l := m.detailLines[m.detailDoc.cursor]; l.path != want || !l.head {
			t.Errorf("Tab put the cursor on %q, want the %s title", stripANSI(l.text), want)
		}
	}
}

// Review focus: the fixed line never wraps.
func TestDetail_FixedLineFitsNarrowTerminal(t *testing.T) {
	m := exchangeDetail(t)
	m.width = 60
	m.layout()
	if w := lipgloss.Width(m.detailFixedLine()); w > 60 {
		t.Errorf("fixed line is %d wide on a 60-column terminal", w)
	}
}

func TestDetail_FetchReplyForEitherHalf(t *testing.T) {
	m := exchangeDetail(t)
	full := *m.detailRow.resp
	full.Inference = &pipeline.InferenceExtension{Model: "m", Completion: "full-body"}
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s", seq: 99, event: &full})
	if strings.Contains(detailText(m), "full-body") {
		t.Fatal("a reply for another seq was applied")
	}
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s", seq: 8, event: &full})
	if !strings.Contains(detailText(m), "full-body") {
		t.Errorf("the response's fetched body did not reach the page:\n%s", detailText(m))
	}
}

// The plugin detail pane shares detailVp, and the event detail's fixed line took a row of it.
// A plugin opened from the event detail gets the whole body back.
func TestDetail_PluginDetailKeepsTheFullBody(t *testing.T) {
	m := exchangeDetail(t)
	m.showPluginDetail(&apiclient.PipelinePlugin{Name: "p"}, true)
	if m.detailVp.Height != m.bodyHeight {
		t.Errorf("plugin detail viewport height %d, want %d", m.detailVp.Height, m.bodyHeight)
	}
}

// The usage pane and the Timeline go back to the detail without a re-render. A resize while
// one of them is up must still leave room for the detail's fixed line.
func TestDetail_FitsAfterAResizeInAnotherPane(t *testing.T) {
	for _, key := range []rune{'u', 'T'} {
		m := exchangeDetail(t)
		m.Update(keyRune(key))
		if m.pane == paneDetail {
			t.Fatalf("%c did not leave the detail", key)
		}
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		press(m, tea.KeyEsc)
		if m.pane != paneDetail {
			t.Fatalf("esc after %c went to pane %v, want detail", key, m.pane)
		}
		assertFits(t, m, fmt.Sprintf("detail after %c and a resize", key))
	}
}

func TestDetail_ResponseFillsOpenDetail(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	req, resp := turnExchange(at)
	m := openDetail(t, at.Add(time.Minute), req)
	if !strings.Contains(detailText(m), "waiting for response…") {
		t.Fatalf("setup: want the waiting note:\n%s", detailText(m))
	}
	m.Update(keyRune('j'))
	m.Update(keyRune('j'))
	before := cursorPath(m)

	m.Update(streamMsg(apiclient.StreamEvent{Event: &resp}))
	if !strings.Contains(detailText(m), "the-answer") {
		t.Errorf("the response did not fill the page:\n%s", detailText(m))
	}
	if got := cursorPath(m); got != before {
		t.Errorf("cursor moved from %q to %q: the fill must keep the reader's place", before, got)
	}
}

func TestDetail_PausedStreamLeavesOpenDetail(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	req, resp := turnExchange(at)
	m := openDetail(t, at.Add(time.Minute), req)
	m.Update(keyRune('p'))
	if !m.paused {
		t.Fatal("setup: p did not pause the stream")
	}

	m.Update(streamMsg(apiclient.StreamEvent{Event: &resp}))
	text := detailText(m)
	if !strings.Contains(text, "waiting for response…") || strings.Contains(text, "the-answer") {
		t.Errorf("a paused stream filled the page:\n%s", text)
	}
}
