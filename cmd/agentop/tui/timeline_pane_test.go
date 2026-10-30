package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/rossoctl/cortex/cmd/agentop/tui/table"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/timeline"
)

var tlBase = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func tlAt(sec float64) time.Time { return tlBase.Add(time.Duration(sec * float64(time.Second))) }
func tlPtr(sec float64) *time.Time {
	t := tlAt(sec)
	return &t
}

// sampleTimeline is a 30-minute session. The main agent works (model, then Bash), spawns a
// subagent and waits on it until its result wakes main at 600s, fails a call at 900s and retries
// it at 960s with the same conversation, finishes a reply at 1200s and waits on the user until
// 1500s. The subagent runs 300-580s: model, then three Reads in one burst. One LLM host carries
// two overlapping calls, a tunnel goes to github, and one request was refused.
func sampleTimeline() *timeline.Timeline {
	return &timeline.Timeline{
		ID: "sess-1", LastSeq: 60, Start: tlAt(0), End: tlAt(1800),
		Agents: []timeline.Agent{
			{ID: timeline.MainAgent, Label: "main agent"},
			{ID: "a1", Parent: timeline.MainAgent, Label: "Survey the code", Type: "Explore",
				SpawnTool: "spawn", SpawnedAt: tlPtr(300), ReturnedAt: tlPtr(600)},
		},
		Turns: []timeline.Turn{
			{Agent: timeline.MainAgent, Start: tlAt(0), End: tlPtr(200), Finish: "tool_use", Seq: 1},
			{Agent: timeline.MainAgent, Start: tlAt(290), End: tlPtr(300), Finish: "end_turn", Seq: 5},
			{Agent: timeline.MainAgent, Start: tlAt(600), End: tlPtr(900), Error: true, Seq: 20},
			{Agent: timeline.MainAgent, Start: tlAt(960), End: tlPtr(1200), Finish: "end_turn", Resent: true, Seq: 30},
			{Agent: timeline.MainAgent, Start: tlAt(1500), End: tlPtr(1800), Finish: "end_turn", Seq: 40},
			{Agent: "a1", Start: tlAt(300), End: tlPtr(500), Finish: "tool_use", Seq: 7},
			{Agent: "a1", Start: tlAt(560), End: tlPtr(580), Finish: "end_turn", Seq: 15},
		},
		Tools: []timeline.Tool{
			{Agent: timeline.MainAgent, ID: "b1", Name: "Bash", Start: tlAt(200), End: tlPtr(290), Preview: "go test ./...", Seq: 2},
			{Agent: timeline.MainAgent, ID: "spawn", Name: "Agent", Start: tlAt(300), End: tlPtr(302), Preview: "Survey the code", Seq: 6},
			{Agent: "a1", ID: "r1", Name: "Read", Start: tlAt(500), End: tlPtr(520), Preview: "/a.go", Seq: 8},
			{Agent: "a1", ID: "r2", Name: "Read", Start: tlAt(505), End: tlPtr(540), Preview: "/b.go", Seq: 9},
			{Agent: "a1", ID: "g1", Name: "Grep", Start: tlAt(510), End: tlPtr(560), Preview: "TODO", Seq: 10},
		},
		Returns: []timeline.Return{{Agent: "a1", Into: timeline.MainAgent, At: tlAt(600), Seq: 20}},
		Hosts: []timeline.Host{
			{Host: "llm.example", Kind: timeline.KindLLM, Start: tlAt(0), End: tlPtr(200), Status: 200, Seq: 1},
			{Host: "llm.example", Kind: timeline.KindLLM, Start: tlAt(300), End: tlPtr(500), Status: 200, Seq: 7},
			{Host: "llm.example", Kind: timeline.KindLLM, Start: tlAt(310), End: tlPtr(490), Status: 200, Seq: 11},
			{Host: "github.com", Kind: timeline.KindTunnel, Start: tlAt(210), End: tlPtr(211), Status: 200, Seq: 3},
			{Host: "evil.example", Kind: timeline.KindHTTP, Start: tlAt(220), End: tlPtr(220), Status: 403, Error: true, Seq: 4},
			{Host: "127.0.0.1:47600", Kind: timeline.KindHTTP, Start: tlAt(230), End: tlPtr(231), Status: 200, Seq: 4},
		},
	}
}

func timelineModel(t *testing.T, w, h int) *model {
	t.Helper()
	m := fitModel(t, paneTimeline, w, h, nil)
	end := tlAt(7200) // an hour and a half after the last event: ended
	m.now = func() time.Time { return end }
	m.timeline = timelineState{session: "sess-1", tl: sampleTimeline(), cursor: -1, fetchedAt: end}
	m.sessions = []session.SessionSummary{{ID: "sess-1"}}
	return m
}

// cellOf is the layout cell holding instant sec.
func cellOf(t *testing.T, lay tlLayout, sec float64) int {
	t.Helper()
	c, ok := lay.axis.cellAt(tlAt(sec))
	if !ok {
		t.Fatalf("%vs is not on the axis", sec)
	}
	return c
}

func near(a, b float64) bool { return a > b-0.02 && a < b+0.02 }

// Each cell of the main row divides its time among the five states, and the shares match the
// sample exactly at a 30s cell width.
func TestTimelineLayout_MainRowSharesEachState(t *testing.T) {
	lay := buildTimelineLayout(sampleTimeline(), tlAt(0), tlAt(1800), 60, tlAt(3600))
	main := lay.agents[0]
	if main.agent.ID != timeline.MainAgent {
		t.Fatalf("first row %q", main.agent.ID)
	}
	for _, tc := range []struct {
		name string
		sec  float64
		want share
	}{
		{"model", 100, share{model: 1}},
		{"tool", 230, share{tool: 1}},
		{"waiting on its subagent", 450, share{subs: 1}},
		{"retrying after a failed call", 930, share{retry: 1}},
		{"waiting on the user", 1350, share{user: 1}},
	} {
		got := main.cells[cellOf(t, lay, tc.sec)]
		if !near(got.tool, tc.want.tool) || !near(got.model, tc.want.model) || !near(got.retry, tc.want.retry) ||
			!near(got.subs, tc.want.subs) || !near(got.user, tc.want.user) {
			t.Errorf("%s at %vs: %+v, want %+v", tc.name, tc.sec, got, tc.want)
		}
	}
}

// A failed call followed by a new message is not a retry: a refusal the user answered by
// rephrasing re-sends nothing.
func TestTimelineLayout_FailedCallThenNewMessageIsNotRetry(t *testing.T) {
	tl := sampleTimeline()
	for i := range tl.Turns {
		tl.Turns[i].Resent = false
	}
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 60, tlAt(3600))
	if got := lay.agents[0].cells[cellOf(t, lay, 930)]; got.retry != 0 {
		t.Errorf("gap after a failed call with no re-send: %+v, want no retry share", got)
	}
}

// A subagent is measured against the part of the cell it was alive for: a cell it only half
// lived in, busy all that half, is a full bar.
func TestTimelineLayout_SubagentIsScaledToItsLifetime(t *testing.T) {
	tl := sampleTimeline()
	// 300s cells: the subagent lives 300-580s, so the 300-600 cell is 93% alive.
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 6, tlAt(3600))
	sub := lay.agents[1]
	c := cellOf(t, lay, 400)
	if !sub.alive[c] || !near(sub.cells[c].total(), 1) {
		t.Errorf("subagent cell = %+v alive %v, want full", sub.cells[c], sub.alive[c])
	}
	if sub.alive[cellOf(t, lay, 100)] || sub.alive[cellOf(t, lay, 700)] {
		t.Errorf("subagent alive outside its lifetime")
	}
	if sub.cells[c].user != 0 {
		t.Errorf("a subagent has no user to wait on: %+v", sub.cells[c])
	}
}

func TestTimelineLayout_GutterCountsTheWindow(t *testing.T) {
	lay := buildTimelineLayout(sampleTimeline(), tlAt(0), tlAt(1800), 60, tlAt(3600))
	main := lay.agents[0]
	// Model time is the five turns (200+10+300+240+300); tool time is Bash only, not the Agent
	// call's async ack.
	if main.modelTime != 1050*time.Second || main.toolTime != 90*time.Second || main.calls != 2 {
		t.Errorf("main model %v tool %v calls %d, want 17m30s, 1m30s, 2", main.modelTime, main.toolTime, main.calls)
	}
}

func TestTimelineLayout_HostRows(t *testing.T) {
	lay := buildTimelineLayout(sampleTimeline(), tlAt(0), tlAt(1800), 60, tlAt(3600))
	var llm, refused *tlHostRow
	for i := range lay.hosts {
		switch lay.hosts[i].host {
		case "llm.example":
			llm = &lay.hosts[i]
		case "evil.example":
			refused = &lay.hosts[i]
		case "127.0.0.1:47600":
			t.Errorf("a loopback host is drawn: it is the proxy itself")
		}
	}
	if llm == nil || refused == nil {
		t.Fatalf("hosts = %+v", lay.hosts)
	}
	if lay.hosts[0].host != "llm.example" {
		t.Errorf("busiest host is %q, want it first", lay.hosts[0].host)
	}
	if got := llm.peak[cellOf(t, lay, 400)]; got != 2 {
		t.Errorf("llm in flight at 400s = %d, want 2", got)
	}
	if c := cellOf(t, lay, 220); !refused.err[c] || refused.peak[c] < 1 {
		t.Errorf("the refused request draws no error cell")
	}
}

// One sweep finds the true peak, including a zero-length request and intervals that only touch.
func TestTimelineSweepPeaks(t *testing.T) {
	ax := tlLinearAxis(tlAt(0), tlAt(10), 2) // [0,5) [5,10)
	pts := []tlPt{
		{at: tlAt(1), d: 1}, {at: tlAt(3), d: -1}, // 1-3
		{at: tlAt(2), d: 1}, {at: tlAt(4), d: -1}, // 2-4: overlaps the first
		{at: tlAt(4), d: 1}, {at: tlAt(6), d: -1}, // touches the second at 4: not concurrent with it
		{at: tlAt(7), d: 1}, {at: tlAt(7), d: -1, zero: true}, // zero length
	}
	peak := make([]int, 2)
	tlSweepPeaks(pts, ax, peak)
	if peak[0] != 2 || peak[1] != 1 {
		t.Errorf("peaks = %v, want [2 1]", peak)
	}
	// A zero-length request alone in a cell is one in flight, even when the cell's other
	// request closes at that instant.
	zero := []tlPt{{at: tlAt(6), d: 1}, {at: tlAt(7), d: -1}, {at: tlAt(7), d: 1}, {at: tlAt(7), d: -1, zero: true}}
	peak = make([]int, 2)
	tlSweepPeaks(zero, ax, peak)
	if peak[1] != 1 {
		t.Errorf("zero-length request: peak %d, want 1", peak[1])
	}
	// Built from host spans: a zero-length request that did not fail is counted. (A failed one
	// is floored to 1 by its error mark, so it would not show the sweep's handling.)
	tl := sampleTimeline()
	tl.Hosts = append(tl.Hosts, timeline.Host{Host: "blip.example", Kind: timeline.KindHTTP, Start: tlAt(700), End: tlPtr(700), Status: 204})
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 60, tlAt(3600))
	found := false
	for _, h := range lay.hosts {
		if h.host == "blip.example" {
			found = true
			if p := h.peak[cellOf(t, lay, 700)]; p != 1 {
				t.Errorf("a zero-length request: peak %d, want 1", p)
			}
		}
	}
	if !found {
		t.Fatalf("blip.example not drawn")
	}
}

// Idle stretches over 30 minutes collapse; every busy period keeps at least one cell however
// many there are, and the widths fill the axis exactly.
func TestTimelineCollapsedAxis(t *testing.T) {
	var busy [][2]time.Time
	for i := 0; i < 10; i++ {
		s := tlAt(float64(i) * 7200)
		busy = append(busy, [2]time.Time{s, s.Add(time.Minute)})
	}
	for _, cells := range []int{150, 40, 12} {
		ax := tlCollapsedAxis(busy[0][0], busy[9][1], busy, cells)
		if ax.width() != cells {
			t.Fatalf("%d cells: axis is %d wide", cells, ax.width())
		}
		seen := map[int64]bool{}
		for _, c := range ax.cells {
			if !c.gap {
				seen[c.from.Truncate(time.Hour).Unix()] = true
			}
		}
		// Gaps merge when there is no room, but no busy period may vanish.
		for _, b := range busy {
			if _, ok := ax.cellAt(b[0].Add(30 * time.Second)); !ok {
				t.Errorf("%d cells: busy period at %v has no cell", cells, b[0])
			}
		}
	}
	ax := tlCollapsedAxis(busy[0][0], busy[9][1], busy, 150)
	gaps := 0
	for _, c := range ax.cells {
		if c.marker {
			gaps++
		}
	}
	if gaps != 9 {
		t.Errorf("gaps = %d at 150 cells, want 9", gaps)
	}
}

// An instant at a busy period's end belongs to the period, not the gap after it, so a span that
// ends there is drawn.
func TestTimelineAxis_PeriodEndBelongsToThePeriod(t *testing.T) {
	busy := [][2]time.Time{{tlAt(0), tlAt(60)}, {tlAt(7200), tlAt(7260)}}
	ax := tlCollapsedAxis(tlAt(0), tlAt(7260), busy, 20)
	c, ok := ax.cellAt(tlAt(60))
	if !ok || ax.cells[c].gap {
		t.Errorf("the first period's end lands in a gap: %d %v", c, ok)
	}
	if _, ok := ax.cellAt(tlAt(3600)); ok {
		t.Errorf("an instant inside the gap has a cell")
	}
}

func TestTimelineAxisLabels_NameTheGap(t *testing.T) {
	tl := sampleTimeline()
	day := 26 * time.Hour
	late := tlAt(1800).Add(day)
	lateEnd := late.Add(5 * time.Minute)
	tl.Turns = append(tl.Turns, timeline.Turn{Agent: timeline.MainAgent, Start: late, End: &lateEnd, Finish: "end_turn", Seq: 50})
	tl.End = lateEnd
	lay := buildTimelineLayout(tl, time.Time{}, time.Time{}, 80, lateEnd)
	if labels := axisLabels(lay); !strings.Contains(labels, "26.0h") {
		t.Errorf("axis does not label the gap: %q", labels)
	}
}

// A terminal cell holds two colours: a cell is drawn with its two largest states, and a cell of
// one state is one block of its height.
func TestShareGlyph(t *testing.T) {
	forceColor(t)
	for _, tc := range []struct {
		name string
		s    share
		want string // the glyph, colours stripped
	}{
		{"idle", share{}, " "},
		{"full model", share{model: 1}, "█"},
		{"half tool", share{tool: 0.5}, "▄"},
		{"a sliver still shows", share{tool: 0.01}, "▁"},
		{"tool under model, full", share{tool: 0.25, model: 0.75}, "▂"},
	} {
		if got := stripANSI(shareGlyph(tc.s)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	lo, loF, hi, hiF := share{tool: 0.1, model: 0.5, user: 0.4}.top2()
	if lo != stModel || hi != stUser || !near(loF+hiF, 1) {
		t.Errorf("top2 = %v %.2f %v %.2f, want model and user filling the cell", lo, loF, hi, hiF)
	}
	if !strings.Contains(shareGlyph(share{tool: 0.25, model: 0.75}), "48;") {
		t.Errorf("a full two-state cell has no background colour")
	}
}

// The rendered rows: main above its subagent, then the hosts, each with a blank line under it.
func TestTimelineRender_RowsAndSpacing(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	lines := strings.Split(stripANSI(m.timelineBody()), "\n")
	idx := func(prefix string) int {
		for i, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		t.Fatalf("no line starting %q in:\n%s", prefix, strings.Join(lines, "\n"))
		return -1
	}
	mainIdx, subIdx, sepIdx, hostIdx := idx("main agent"), idx("  Survey the code"), idx("╌"), idx("llm.example")
	if mainIdx >= subIdx || subIdx >= sepIdx || sepIdx >= hostIdx {
		t.Errorf("rows out of order: main %d, sub %d, separator %d, host %d", mainIdx, subIdx, sepIdx, hostIdx)
	}
	for _, i := range []int{mainIdx, subIdx, hostIdx} {
		if strings.TrimSpace(lines[i+1]) != "" {
			t.Errorf("row %d is not followed by a blank line: %q", i, lines[i+1])
		}
	}
	if !strings.Contains(lines[mainIdx], "█") {
		t.Errorf("main row draws no bars: %q", lines[mainIdx])
	}
	if !strings.Contains(lines[mainIdx-1], "model  tools  calls") {
		t.Errorf("the line above the first row is not the column headers: %q", lines[mainIdx-1])
	}
}

// w offers only the windows shorter than the session, then "all"; a saved choice too long for
// this session shows "all" without being overwritten.
func TestTimelineWindows_OfferedBySessionLength(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want []string
	}{
		{4 * time.Minute, []string{"all"}},
		{30 * time.Minute, []string{"5m", "all"}},
		{5 * time.Hour, []string{"5m", "1h", "all"}},
		{50 * time.Hour, []string{"5m", "1h", "24h", "all"}},
	} {
		var got []string
		for _, i := range offeredWindows(tc.d) {
			got = append(got, timelineWindows[i].label)
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%v: %v, want %v", tc.d, got, tc.want)
		}
	}
	resetSettingsForTest(t)
	m := timelineModel(t, 120, 40) // a 30-minute session
	m.timeline.windowIdx = 2       // 24h, saved from a longer session
	if got := timelineWindows[m.timeline.window(m.clock())].label; got != "all" {
		t.Errorf("24h on a 30-minute session draws %q, want all", got)
	}
	if m.timeline.windowIdx != 2 {
		t.Errorf("drawing the fallback overwrote the saved choice")
	}
	if got := stripANSI(m.renderTimeline()); !strings.Contains(got, "window 5m [all]") {
		t.Errorf("tabs are not the offered windows: %q", strings.SplitN(got, "\n", 2)[0])
	}
}

func TestTimelineKeys_WindowCyclesTheOfferedAndPersists(t *testing.T) {
	resetSettingsForTest(t)
	m := timelineModel(t, 120, 40)
	m.timeline.windowIdx = 0
	m.handleKey(keyRune('w'))
	if got := timelineWindows[m.timeline.windowIdx].label; got != "all" || Settings.Timeline.Window != "all" {
		t.Errorf("w from 5m on a 30-minute session: %q, saved %q, want all", got, Settings.Timeline.Window)
	}
	m.handleKey(keyRune('w'))
	if got := timelineWindows[m.timeline.windowIdx].label; got != "5m" {
		t.Errorf("w wrapped to %q, want 5m", got)
	}
	if Settings.Timeline.Window != "" || Settings.timelineWindow() != 0 {
		t.Errorf("the default is saved as %q", Settings.Timeline.Window)
	}
}

func TestTimelineKeys_CursorSkipsGapsAndListsTools(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
	lay, _ := m.timelineLayoutNow()
	if c := m.timeline.cursor; c != lay.axis.width()-1 {
		t.Errorf("first ← put the cursor at %d, want the newest cell", c)
	}
	for i := 0; i < lay.axis.width()+5; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
		if lay.axis.cells[m.timeline.cursor].gap {
			t.Fatalf("cursor landed on a gap cell %d", m.timeline.cursor)
		}
	}
	m.timeline.cursor = cellOf(t, lay, 507)
	strip := stripANSI(m.cursorStrip(lay))
	if !strings.Contains(strip, "Read") || !strings.Contains(strip, "/a.go") || !strings.Contains(strip, "Survey the co") {
		t.Errorf("strip = %q, want the subagent's tools with previews", strip)
	}
	if seq, ok := m.timelineCursorSeq(); !ok || seq < 8 || seq > 10 {
		t.Errorf("cursor seq = %d %v, want one of the burst's tools", seq, ok)
	}
}

// The agent label is the model-written Agent description and the tool name is the model's too:
// neither may carry a control sequence to the terminal.
func TestTimelineCursorStrip_SanitizesModelText(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	m.timeline.tl.Agents[1].Label = "Sur\x1b]52;c;aGk=\x07vey"
	m.timeline.tl.Tools[2].Name = "Re\x1b[2Jad"
	lay, _ := m.timelineLayoutNow()
	m.timeline.cursor = cellOf(t, lay, 500)
	if strip := m.cursorStrip(lay); strings.Contains(strip, "\x1b]") || strings.Contains(strip, "\x1b[2J") || strings.Contains(strip, "\x07") {
		t.Errorf("strip = %q, carries a control sequence", strip)
	}
}

func TestTimelineKeys_EscReturnsAndArrowsStay(t *testing.T) {
	m := timelineModel(t, 120, 40)
	m.timeline.returnPane = paneEvents
	m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
	m.handleKey(keyRune('h'))
	if m.pane != paneTimeline {
		t.Fatalf("← / h left the pane: %v", m.pane)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pane != paneEvents {
		t.Errorf("esc went to %v, want events", m.pane)
	}
}

func TestTimelineOpen_FromEventsAndFromSessions(t *testing.T) {
	m := fitModel(t, paneEvents, 120, 40, nil)
	m.client = deadClient()
	m.handleKey(keyRune('T'))
	if m.pane != paneTimeline || m.timeline.session != "sess-1" || m.timeline.returnPane != paneEvents || !m.timeline.loading {
		t.Fatalf("T from events: pane %v session %q return %v loading %v", m.pane, m.timeline.session, m.timeline.returnPane, m.timeline.loading)
	}

	m = fitModel(t, paneSessions, 120, 40, nil)
	m.client = deadClient()
	m.sessionRowIDs = []string{"row-a", "row-b"}
	m.sessionsTbl.SetRows([]table.Row{make(table.Row, len(m.sessionsTbl.Columns())), make(table.Row, len(m.sessionsTbl.Columns()))})
	m.sessionsTbl.SetCursor(1)
	m.handleKey(keyRune('T'))
	if m.pane != paneTimeline || m.timeline.session != "row-b" || !m.timeline.loading {
		t.Errorf("T from sessions: pane %v session %q loading %v, want the highlighted row", m.pane, m.timeline.session, m.timeline.loading)
	}
}

// 404 means an old proxy only for a listed session that never loaded; a session the proxy does
// not list, or a refresh after a good load, keeps what it had and reports the error.
func TestTimelineLoaded_NotFound(t *testing.T) {
	m := timelineModel(t, 120, 40)
	m.timeline.tl = nil
	m.timeline.reqSeq = 3
	m.handleTimelineLoaded(timelineLoadedMsg{req: 3, err: apiclient.ErrNotFound})
	if !errors.Is(m.timeline.err, errTimelineUnsupported) || !strings.Contains(stripANSI(m.renderTimeline()), "upgrade cortex") {
		t.Errorf("old proxy: err %v", m.timeline.err)
	}

	m = timelineModel(t, 120, 40)
	m.timeline.reqSeq = 3
	m.handleTimelineLoaded(timelineLoadedMsg{req: 3, err: apiclient.ErrNotFound})
	if errors.Is(m.timeline.err, errTimelineUnsupported) || m.timeline.tl == nil {
		t.Errorf("a 404 after a good load says upgrade or drops the chart: err %v", m.timeline.err)
	}

	m = timelineModel(t, 120, 40)
	m.timeline.tl, m.sessions = nil, nil
	m.timeline.reqSeq = 3
	m.handleTimelineLoaded(timelineLoadedMsg{req: 3, err: apiclient.ErrNotFound})
	if errors.Is(m.timeline.err, errTimelineUnsupported) {
		t.Errorf("an unlisted session's 404 says upgrade")
	}

	m.handleTimelineLoaded(timelineLoadedMsg{req: 2, tl: sampleTimeline()})
	if m.timeline.tl != nil {
		t.Errorf("a stale reply was applied")
	}
}

// The stream marks the pane stale for its own session only; the next tick refetches. A live
// session with no events refetches every timelineIdleRefresh so spans in flight keep growing.
func TestTimelineRefresh(t *testing.T) {
	m := timelineModel(t, 120, 40)
	m.client = deadClient()
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{SessionID: "other", At: tlAt(1)}})
	if m.timeline.stale {
		t.Errorf("another session's event marked the pane stale")
	}
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{SessionID: "sess-1", At: tlAt(1)}})
	if !m.timeline.stale {
		t.Fatalf("this session's event did not mark the pane stale")
	}
	m.timeline.tickGen = 7
	m.handleTimelineTick(timelineTickMsg{gen: 7})
	if !m.timeline.loading || m.timeline.stale {
		t.Errorf("a stale tick did not fetch: loading %v stale %v", m.timeline.loading, m.timeline.stale)
	}
	if cmd := m.handleTimelineTick(timelineTickMsg{gen: 6}); cmd != nil {
		t.Errorf("a tick from an earlier visit rescheduled")
	}

	// Live: last activity is now. No events, but the idle interval has passed.
	m = timelineModel(t, 120, 40)
	m.client = deadClient()
	live := tlAt(1800).Add(30 * time.Second)
	m.now = func() time.Time { return live }
	m.timeline.fetchedAt = live.Add(-timelineIdleRefresh)
	m.timeline.tickGen = 1
	m.handleTimelineTick(timelineTickMsg{gen: 1})
	if !m.timeline.loading {
		t.Errorf("a live session idle for %v was not refetched", timelineIdleRefresh)
	}
	// Ended: no refetch without events.
	m = timelineModel(t, 120, 40)
	m.client = deadClient()
	m.timeline.fetchedAt = tlAt(7200).Add(-time.Minute)
	m.timeline.tickGen = 1
	m.handleTimelineTick(timelineTickMsg{gen: 1})
	if m.timeline.loading {
		t.Errorf("an ended session was refetched with nothing new")
	}
}

// An ended session's windows end at its agents' last activity, not at background traffic after
// it, and not at now.
func TestTimelineRange_EndedSessionEndsAtItsLastActivity(t *testing.T) {
	m := timelineModel(t, 120, 40)
	m.timeline.tl.Hosts = append(m.timeline.tl.Hosts, timeline.Host{Host: "fw.example", Kind: timeline.KindTunnel, Start: tlAt(3000), End: tlPtr(3001)})
	m.timeline.tl.End = tlAt(3001)
	m.timeline.windowIdx = 0
	_, to, _, _ := m.timelineRange()
	if !to.Equal(tlAt(1800)) {
		t.Errorf("window ends %v, want the last turn's end %v", to, tlAt(1800))
	}
}

// enter lands on the event in its own session, and a pending target is dropped when that
// session's snapshot fails, so it cannot jump the cursor in another session later.
func TestTimelineEnter_RevealIsTiedToItsSession(t *testing.T) {
	m := timelineModel(t, 120, 40)
	m.client = deadClient()
	m.openEventsAtSeq("sess-1", 8)
	if m.pane != paneEvents || m.revealSeq != 8 || m.revealSess != "sess-1" {
		t.Fatalf("pane %v reveal %d/%q", m.pane, m.revealSeq, m.revealSess)
	}
	m.Update(errMsg{where: "snapshot sess-1", err: errors.New("down")})
	if m.revealSeq != 0 || m.revealSess != "" {
		t.Errorf("a failed snapshot left the reveal pending: %d/%q", m.revealSeq, m.revealSess)
	}
	m.revealSeq, m.revealSess = 8, "sess-1"
	m.selectedSess = "other"
	if m.revealPendingSeq() {
		t.Errorf("revealed sess-1's seq in another session")
	}
}

// Fits at every size, many rows included: the row caps follow the body height.
func TestTimelinePane_FitsEverySize(t *testing.T) {
	forceColor(t)
	many := sampleTimeline()
	for i := 0; i < 12; i++ {
		id := string(rune('b' + i))
		many.Agents = append(many.Agents, timeline.Agent{ID: id, Parent: timeline.MainAgent, Label: "subagent " + id})
		many.Turns = append(many.Turns, timeline.Turn{Agent: id, Start: tlAt(float64(100 * i)), End: tlPtr(float64(100*i + 50)), Seq: uint64(100 + i)})
		many.Hosts = append(many.Hosts, timeline.Host{Host: "h" + id + ".example", Kind: timeline.KindHTTP, Start: tlAt(float64(100 * i)), End: tlPtr(float64(100*i + 1))})
	}
	for _, tl := range []*timeline.Timeline{sampleTimeline(), many} {
		for _, d := range fitSizes {
			m := timelineModel(t, d[0], d[1])
			m.timeline.tl = tl
			m.timeline.windowIdx = 3
			m.layout()
			assertFits(t, m, "timeline")
			m.timeline.cursor = 5
			assertFits(t, m, "timeline with cursor")
		}
	}
	m := timelineModel(t, 80, 24)
	m.timeline.tl = many
	m.layout()
	if !strings.Contains(stripANSI(m.View()), "agents below") {
		t.Errorf("at 80x24 the hidden agents are not counted")
	}
}

// Every host row's gutter lines up, whatever its kind tag.
func TestTimelineRender_HostGuttersAlign(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	lay, _ := m.timelineLayoutNow()
	col := -1
	for _, r := range lay.hosts {
		line := stripANSI(m.hostLine(r, lay))
		at := lipgloss.Width(line[:strings.Index(line, "│")])
		if col >= 0 && at != col {
			t.Errorf("%s gutter at %d, others at %d", r.host, at, col)
		}
		col = at
	}
}

// A gap that a subagent's result ended is waiting on subagents, even after the subagent itself
// finished: here the subagent stops at 580s and its result wakes main at 600s.
func TestTimelineWaits_ResultEndsTheWaitAfterTheSubagentFinished(t *testing.T) {
	tl := sampleTimeline()
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 180, tlAt(3600)) // 10s cells
	got := lay.agents[0].cells[cellOf(t, lay, 590)]
	if !near(got.subs, 1) || got.user != 0 {
		t.Errorf("580-600s, after the subagent stopped and before its result: %+v, want waiting on subagents", got)
	}
}

// Requests touching at an instant are not concurrent: a close at t sorts before an open at t.
func TestTimelineSweepPeaks_TouchingIsNotConcurrent(t *testing.T) {
	ax := tlLinearAxis(tlAt(0), tlAt(10), 1)
	pts := []tlPt{{at: tlAt(4), d: 1}, {at: tlAt(6), d: -1}, {at: tlAt(1), d: 1}, {at: tlAt(4), d: -1}}
	peak := make([]int, 1)
	tlSweepPeaks(pts, ax, peak)
	if peak[0] != 1 {
		t.Errorf("peak = %d for two requests that only touch, want 1", peak[0])
	}
}

// The cursor never rests on a collapsed gap, stepping in either direction.
func TestTimelineKeys_CursorNeverRestsOnAGap(t *testing.T) {
	m := timelineModel(t, 120, 40)
	day := 26 * time.Hour
	late := tlAt(1800).Add(day)
	lateEnd := late.Add(5 * time.Minute)
	m.timeline.tl.Turns = append(m.timeline.tl.Turns, timeline.Turn{Agent: timeline.MainAgent, Start: late, End: &lateEnd, Finish: "end_turn", Seq: 50})
	m.timeline.tl.End = lateEnd
	m.now = func() time.Time { return lateEnd.Add(time.Hour) }
	m.timeline.fetchedAt = lateEnd.Add(time.Hour)
	m.timeline.windowIdx = 3
	lay, _ := m.timelineLayoutNow()
	gapAt := -1
	for c, cell := range lay.axis.cells {
		if cell.gap {
			gapAt = c
			break
		}
	}
	if gapAt < 0 {
		t.Fatal("no gap on the axis")
	}
	m.timeline.cursor = gapAt + timelineGapCells
	m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
	if m.timeline.cursor != gapAt-1 {
		t.Errorf("← from after the gap landed on %d, want %d, the cell before it", m.timeline.cursor, gapAt-1)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRight})
	if m.timeline.cursor != gapAt+timelineGapCells {
		t.Errorf("→ from before the gap landed on %d, want %d", m.timeline.cursor, gapAt+timelineGapCells)
	}
}

// A seq waiting to be revealed in one session is never applied to another's rows, even when an
// event there has the same seq.
func TestTimelineEnter_RevealIgnoresAnotherSessionsRows(t *testing.T) {
	m := fitModel(t, paneEvents, 120, 40, []pipeline.SessionEvent{{Seq: 8, At: tlAt(1), Direction: pipeline.Outbound, Host: "h"}})
	m.rebuildEventsTable()
	m.revealSeq, m.revealSess = 8, "another-session"
	if m.revealPendingSeq() {
		t.Errorf("revealed another session's seq 8 on sess-1's rows")
	}
	m.revealSess = "sess-1"
	if !m.revealPendingSeq() || m.revealSeq != 0 {
		t.Errorf("did not reveal sess-1's own seq 8")
	}
}

// Dropping a third state scales the other two up to the cell's filled share, so a busy cell does
// not draw as part idle; and a sliver of a single state still draws.
func TestShareGlyph_ThreeStatesKeepTheFill(t *testing.T) {
	forceColor(t)
	// Not full: model 0.30, user 0.20, tool 0.05 fill 55%. The two largest scale up to 55%,
	// drawn as the larger one alone at ▄ (4/8); unscaled they would be 50%, also ▄, so use a
	// case where the scale crosses an eighth: 0.30 + 0.14 = 44% unscaled (▄), 54% scaled.
	s := share{model: 0.30, user: 0.14, tool: 0.10}
	_, loF, _, hiF := s.top2()
	if !near(loF+hiF, 0.54) {
		t.Errorf("two largest of a 54%% cell sum to %.2f, want 0.54", loF+hiF)
	}
	if got := stripANSI(shareGlyph(s)); got != "▄" {
		t.Errorf("a 54%% three-state cell drew %q, want ▄", got)
	}
	if got := stripANSI(shareGlyph(share{model: 0.04})); got != "▁" {
		t.Errorf("a 4%% sliver drew %q, want ▁", got)
	}
}

// A wait for the user that a background subagent briefly overlapped is still waiting on the
// user outside the overlap, not blank.
func TestTimelineWaits_UserWaitSurvivesABriefSubagent(t *testing.T) {
	tl := sampleTimeline()
	// Main finishes a reply at 1200s and the user answers at 1500s; a subagent runs 1210-1260s.
	tl.Agents = append(tl.Agents, timeline.Agent{ID: "bg", Parent: timeline.MainAgent, Label: "background"})
	tl.Turns = append(tl.Turns, timeline.Turn{Agent: "bg", Start: tlAt(1210), End: tlPtr(1260), Finish: "end_turn", Seq: 35})
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 180, tlAt(3600)) // 10s cells
	main := lay.agents[0]
	if c := main.cells[cellOf(t, lay, 1235)]; !near(c.subs, 1) {
		t.Errorf("during the subagent: %+v, want waiting on subagents", c)
	}
	if c := main.cells[cellOf(t, lay, 1400)]; !near(c.user, 1) {
		t.Errorf("after the subagent, before the user's answer: %+v, want waiting on the user", c)
	}
}

// A live main agent that has finished its reply is waiting on the user now, not idle.
func TestTimelineWaits_LiveSessionIsWaitingOnTheUserNow(t *testing.T) {
	tl := sampleTimeline() // the last main turn ends at 1800s with end_turn
	lay := buildTimelineLayout(tl, tlAt(1500), tlAt(2100), 60, tlAt(2100))
	if c := lay.agents[0].cells[cellOf(t, lay, 2000)]; !near(c.user, 1) {
		t.Errorf("after the last reply, live: %+v, want waiting on the user", c)
	}
}

// The layout is cached across redraws of a live, windowed session within one refresh interval.
func TestTimelineLayout_CachedWithinARefresh(t *testing.T) {
	m := timelineModel(t, 120, 40)
	at := tlAt(1800).Add(30 * time.Second) // live: last activity 30s ago
	m.now = func() time.Time { return at }
	m.timeline.fetchedAt = at
	m.timeline.windowIdx = 0
	m.timelineLayoutNow()
	first := m.timeline.layoutKey
	at = at.Add(300 * time.Millisecond)
	m.timelineLayoutNow()
	if m.timeline.layoutKey != first {
		t.Errorf("a redraw 300ms later rebuilt the layout: %+v vs %+v", m.timeline.layoutKey, first)
	}
}

// A cursor left past the end by a narrower terminal still moves, and the strip never reads a
// gap.
func TestTimelineKeys_CursorAfterAShrink(t *testing.T) {
	m := timelineModel(t, 200, 40)
	m.timeline.windowIdx = 3
	m.timeline.cursor = 140
	m.width = 120
	m.layout()
	m.timelineLayoutNow()
	if m.timeline.cursor >= m.timelineCells() {
		t.Errorf("cursor %d left past a %d-cell axis", m.timeline.cursor, m.timelineCells())
	}
	m.timeline.cursor = 999 // as if set before a rebuild that did not run
	m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
	if c := m.timeline.cursor; c < 0 || c >= m.timelineCells() {
		t.Errorf("← from past the end left the cursor at %d", c)
	}
}

// A session with hosts and no agents draws its host rows at "all".
func TestTimelineLayout_HostOnlySessionDrawsAtAll(t *testing.T) {
	tl := &timeline.Timeline{ID: "s", Start: tlAt(0), End: tlAt(20000), Agents: []timeline.Agent{}}
	for i := 0; i < 4; i++ {
		s := float64(i) * 6000
		tl.Hosts = append(tl.Hosts, timeline.Host{Host: "h.example", Kind: timeline.KindHTTP, Start: tlAt(s), End: tlPtr(s + 30)})
	}
	lay := buildTimelineLayout(tl, time.Time{}, time.Time{}, 60, tlAt(20000))
	lit := 0
	for _, p := range lay.hosts[0].peak {
		if p > 0 {
			lit++
		}
	}
	if lit < 4 {
		t.Errorf("host-only session at all lit %d cells, want every request drawn", lit)
	}
	for _, c := range lay.axis.cells {
		if !c.gap && !c.to.After(c.from) {
			t.Fatalf("zero-width cell %+v", c)
		}
	}
}

// w on a session with only "all" to offer changes nothing and keeps a saved window.
func TestTimelineKeys_WOnAShortSessionKeepsTheSavedWindow(t *testing.T) {
	resetSettingsForTest(t)
	Settings.Timeline.Window = "1h"
	m := timelineModel(t, 120, 40)
	short := sampleTimeline()
	short.Turns = short.Turns[:1] // one 200s turn: under 5 minutes
	short.Tools = nil
	m.timeline.tl = short
	m.timeline.windowIdx = 1
	m.handleKey(keyRune('w'))
	if m.timeline.windowIdx != 1 || Settings.Timeline.Window != "1h" {
		t.Errorf("w on a 3-minute session changed the window to %d, saved %q", m.timeline.windowIdx, Settings.Timeline.Window)
	}
}

// Moving the events cursor, or picking another session, cancels a pending reveal.
func TestTimelineEnter_ManualMovesCancelTheReveal(t *testing.T) {
	m := fitModel(t, paneEvents, 120, 40, []pipeline.SessionEvent{
		{Seq: 1, At: tlAt(1), Direction: pipeline.Outbound, Host: "h"},
		{Seq: 2, At: tlAt(2), Direction: pipeline.Outbound, Host: "h"},
	})
	m.rebuildEventsTable()
	m.revealSeq, m.revealSess = 99, "sess-1"
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.revealSeq != 0 {
		t.Errorf("an events cursor move left the reveal pending")
	}
	m = fitModel(t, paneSessions, 120, 40, nil)
	m.revealSeq, m.revealSess = 99, "sess-1"
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.revealSeq != 0 {
		t.Errorf("a new pick from Sessions left the reveal pending")
	}
}

// Below the sizes the fit test sweeps, the pane still never runs past the terminal.
func TestTimelinePane_FitsShortTerminals(t *testing.T) {
	forceColor(t)
	for _, h := range []int{10, 12, 14, 16, 18} {
		m := timelineModel(t, 100, h)
		m.timeline.windowIdx = 3
		m.layout()
		assertFits(t, m, "short timeline")
	}
}

// A session that stopped mid-tool, with the tool's result never recorded, does not draw that
// tool running on to now: once the session is no longer live its open spans end at its last
// recorded event. While it is live, an open tool grows with the clock.
func TestTimelineOpenSpans_EndAtTheLastEventOnceTheSessionStopped(t *testing.T) {
	tl := sampleTimeline()
	tl.Turns = tl.Turns[:4]                                                                                                    // main's last turn ends at 1200s with end_turn...
	tl.Tools = append(tl.Tools, timeline.Tool{Agent: timeline.MainAgent, ID: "cut", Name: "Bash", Start: tlAt(1210), Seq: 50}) // ...then a Bash that never answered
	tl.End = tlAt(1215)

	m := timelineModel(t, 120, 40) // its clock is hours past 1215s
	m.timeline.tl = tl
	m.timeline.windowIdx = 3
	lay, _ := m.timelineLayoutNow()
	// The sample's own Bash is 90s; the cut-off one may add at most the 5s to the last event.
	if got := lay.agents[0].toolTime; got > 95*time.Second {
		t.Errorf("an ended session's cut-off tool brings tool time to %v, want at most 1m35s", got)
	}
	if lay.to.After(tlAt(1215).Add(time.Second)) {
		t.Errorf("\"all\" runs to %v, past the session's last event", lay.to)
	}

	// w measures the session as the pane draws it: the open Bash does not stretch it to the
	// clock, so from 5m the next window is all, as the tab row offers.
	m.timeline.windowIdx = 0
	m.handleKey(keyRune('w'))
	if got := timelineWindows[m.timeline.windowIdx].label; got != "all" {
		t.Errorf("w on an ended session with an open call chose %q, want all", got)
	}

	live := tlAt(1215).Add(5 * time.Minute)
	m.now = func() time.Time { return live }
	m.timeline.layoutKey = tlLayoutKey{}
	lay, _ = m.timelineLayoutNow()
	if got := lay.agents[0].toolTime; got < 90*time.Second+4*time.Minute {
		t.Errorf("a live session's running tool brings tool time to %v, want it growing to now", got)
	}
}

// A session with no agents, or no hosts, draws a note in that side's place; the other side's
// rows make room for it rather than push the pane past the terminal.
func TestTimelinePane_FitsWithOneSideEmpty(t *testing.T) {
	forceColor(t)
	noHosts := sampleTimeline()
	noHosts.Hosts = nil
	for i := 0; i < 20; i++ {
		id := string(rune('b' + i))
		noHosts.Agents = append(noHosts.Agents, timeline.Agent{ID: id, Parent: timeline.MainAgent, Label: "subagent " + id})
		noHosts.Turns = append(noHosts.Turns, timeline.Turn{Agent: id, Start: tlAt(float64(100 * i)), End: tlPtr(float64(100*i + 50))})
	}
	noAgents := &timeline.Timeline{ID: "s", Start: tlAt(0), End: tlAt(600), Agents: []timeline.Agent{}}
	for i := 0; i < 20; i++ {
		noAgents.Hosts = append(noAgents.Hosts, timeline.Host{Host: "h" + string(rune('a'+i)) + ".example", Kind: timeline.KindHTTP, Start: tlAt(float64(i * 10)), End: tlPtr(float64(i*10 + 5))})
	}
	sizes := append([][2]int{{100, 10}, {100, 12}, {100, 14}, {100, 16}, {100, 18}}, fitSizes...)
	for name, tl := range map[string]*timeline.Timeline{"no hosts": noHosts, "no agents": noAgents} {
		for _, d := range sizes {
			m := timelineModel(t, d[0], d[1])
			m.timeline.tl = tl
			m.timeline.windowIdx = 3
			m.layout()
			assertFits(t, m, name)
		}
	}
}

// A linked bucket's events sit in the session's rows numbered in their own bucket, so a seq
// to reveal lands on the session's own event of that seq, never on a bucket's.
func TestTimelineEnter_RevealSkipsALinkedBucketsEventOfTheSameSeq(t *testing.T) {
	m := fitModel(t, paneEvents, 120, 40, []pipeline.SessionEvent{
		{Seq: 8, At: tlAt(1), SessionID: "pending:claude-code@100.5", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "bucket"},
		{Seq: 8, At: tlAt(2), SessionID: "sess-1", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "own"},
	})
	m.rebuildEventsTable()
	m.revealSeq, m.revealSess = 8, "sess-1"
	if !m.revealPendingSeq() {
		t.Fatal("did not reveal sess-1's seq 8")
	}
	if m.selectedEventKey.at != tlAt(2).UnixNano() {
		t.Errorf("revealed the linked bucket's seq 8, want the session's own")
	}
}

// [↑↓] is advertised only when some agent rows do not fit.
func TestTimelineFooter_ScrollHintOnlyWhenRowsAreHidden(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	m.layout()
	if strings.Contains(m.helpView(), "[↑↓]") {
		t.Errorf("every agent fits, yet the footer offers [↑↓]: %q", m.helpView())
	}
	many := sampleTimeline()
	for i := 0; i < 20; i++ {
		id := string(rune('b' + i))
		many.Agents = append(many.Agents, timeline.Agent{ID: id, Parent: timeline.MainAgent, Label: "subagent " + id})
	}
	m.timeline.tl, m.timeline.layoutKey = many, tlLayoutKey{}
	if !strings.Contains(m.helpView(), "[↑↓] rows") {
		t.Errorf("rows are hidden, yet the footer does not offer [↑↓]: %q", m.helpView())
	}
}

// laterSession is sampleTimeline plus a turn a day after it, so "all" collapses the stretch
// between them into a gap.
func laterSession(after timeline.Turn) *timeline.Timeline {
	tl := sampleTimeline()
	late := tlAt(1800).Add(26 * time.Hour)
	lateEnd := late.Add(5 * time.Minute)
	tl.Turns = append(tl.Turns, timeline.Turn{Agent: timeline.MainAgent, Start: late, End: &lateEnd, Finish: "end_turn", Seq: 50})
	tl.End = lateEnd
	return tl
}

func gapMarker(t *testing.T, lay tlLayout) int {
	t.Helper()
	for c, cell := range lay.axis.cells {
		if cell.marker {
			return c
		}
	}
	t.Fatal("no gap on the axis")
	return -1
}

// A collapsed gap's marker carries the state that held the stretch: a day after a finished reply
// is waiting on the user, and the marker is drawn in that colour, not as bare idle.
func TestTimelineGap_MarkerCarriesItsState(t *testing.T) {
	forceColor(t)
	tl := laterSession(timeline.Turn{})
	lay := buildTimelineLayout(tl, time.Time{}, time.Time{}, 120, tl.End)
	c := gapMarker(t, lay)
	if got := lay.agents[0].cells[c]; !near(got.user, 1) {
		t.Fatalf("gap cell = %+v, want the whole stretch waiting on the user", got)
	}
	m := timelineModel(t, 140, 46)
	user, muted := lipgloss.NewStyle().Foreground(colorTLUser).Render("┆"), styleMuted.Render("┆")
	if line := m.agentLine(lay.agents[0], lay); !strings.Contains(line, user) {
		t.Errorf("main row %q does not draw its marker in the waiting-on-you colour", line)
	}
	// The subagent was not alive across the gap, so its marker stays muted.
	if line := m.agentLine(lay.agents[1], lay); !strings.Contains(line, muted) || strings.Contains(line, user) {
		t.Errorf("subagent row %q, want its marker muted", line)
	}
}

// A failed request inside a collapsed gap marks the host's gap marker rather than vanishing.
func TestTimelineGap_HostErrorInTheGapShows(t *testing.T) {
	tl := laterSession(timeline.Turn{})
	at := tlAt(1800).Add(3 * time.Hour)
	tl.Hosts = append(tl.Hosts, timeline.Host{Host: "llm.example", Kind: timeline.KindLLM, Start: at, End: &at, Status: 502, Error: true, Seq: 45})
	lay := buildTimelineLayout(tl, time.Time{}, time.Time{}, 120, tl.End)
	c := gapMarker(t, lay)
	for _, r := range lay.hosts {
		if r.host == "llm.example" && !r.err[c] {
			t.Errorf("the 502 three hours into the gap is not on the gap's marker")
		}
	}
}

// After a call cut mid-stream, the agent is retrying until the last failed request to its model
// host, then waiting on the user, who had to act; not hours of retrying.
func TestTimelineWaits_RetryEndsAtTheLastFailureThenWaitsOnYou(t *testing.T) {
	tl := sampleTimeline()
	// The 900s turn failed; failed requests to its host follow at 910s and 930s, and the next
	// call at 960s is a fresh conversation, not a re-send.
	tl.Turns[3].Resent = false
	tl.Hosts = append(tl.Hosts,
		timeline.Host{Host: "llm.example", Kind: timeline.KindLLM, Start: tlAt(600), End: tlPtr(900), Status: 200, Seq: 20},
		timeline.Host{Host: "llm.example", Kind: timeline.KindTunnel, Start: tlAt(910), End: tlPtr(911), Status: 502, Error: true, Seq: 21},
		timeline.Host{Host: "llm.example", Kind: timeline.KindTunnel, Start: tlAt(930), End: tlPtr(931), Status: 502, Error: true, Seq: 22})
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 180, tlAt(3600)) // 10s cells
	main := lay.agents[0]
	if c := main.cells[cellOf(t, lay, 915)]; !near(c.retry, 1) {
		t.Errorf("between the failures: %+v, want retrying", c)
	}
	if c := main.cells[cellOf(t, lay, 945)]; !near(c.user, 1) {
		t.Errorf("after the last failure: %+v, want waiting on the user", c)
	}
}

// A call that re-sent the conversation unchanged makes the whole gap before it retrying.
func TestTimelineWaits_ResentCallMakesTheGapRetrying(t *testing.T) {
	tl := sampleTimeline() // the 960s call re-sends the 600s call's conversation
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 180, tlAt(3600))
	if c := lay.agents[0].cells[cellOf(t, lay, 950)]; !near(c.retry, 1) {
		t.Errorf("just before the re-send: %+v, want retrying", c)
	}
}

// Time on the main row nothing else explains is waiting on the user; time after a tool that lost
// its result is not claimed.
func TestTimelineWaits_MainDefaultsToYouExceptAfterALostTool(t *testing.T) {
	tl := sampleTimeline()
	// A main turn that ends on tool_use, whose Bash was lost, then a gap until 1500s.
	tl.Turns[4] = timeline.Turn{Agent: timeline.MainAgent, Start: tlAt(960), End: tlPtr(1200), Finish: "tool_use", Seq: 30}
	tl.Tools = append(tl.Tools, timeline.Tool{Agent: timeline.MainAgent, ID: "lost", Name: "Bash", Start: tlAt(1200), End: tlPtr(1200), Lost: true, Seq: 31})
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 180, tlAt(3600))
	if c := lay.agents[0].cells[cellOf(t, lay, 1350)]; c.total() != 0 {
		t.Errorf("after a lost tool: %+v, want unclaimed", c)
	}
	tl.Tools = tl.Tools[:len(tl.Tools)-1]
	lay = buildTimelineLayout(tl, tlAt(0), tlAt(1800), 180, tlAt(3600))
	if c := lay.agents[0].cells[cellOf(t, lay, 1350)]; !near(c.user, 1) {
		t.Errorf("after a tool_use turn with nothing running: %+v, want waiting on the user", c)
	}
}

// A background job the main agent left running is waiting on subagents, not on the user.
func TestTimelineWaits_BackgroundJobIsNotTheUsersTime(t *testing.T) {
	tl := sampleTimeline() // main finishes a reply at 1200s; the user answers at 1500s
	tl.Jobs = []timeline.Job{{Agent: timeline.MainAgent, ID: "bk1", Start: tlAt(1190), End: tlPtr(1400), Seq: 29}}
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 180, tlAt(3600))
	if c := lay.agents[0].cells[cellOf(t, lay, 1300)]; !near(c.subs, 1) {
		t.Errorf("while the job ran: %+v, want waiting on subagents", c)
	}
	if c := lay.agents[0].cells[cellOf(t, lay, 1450)]; !near(c.user, 1) {
		t.Errorf("after the job's notification: %+v, want waiting on the user", c)
	}
}

// A first fetch that times out is retried on a backoff and drawn as loading, a refresh never
// hammers a dead proxy, and an old proxy's 404 is never retried.
func TestTimelineRefresh_FailedFetchIsRetriedOnABackoff(t *testing.T) {
	m := timelineModel(t, 120, 40)
	m.client = deadClient()
	m.timeline.tl = nil
	m.timeline.tickGen, m.timeline.reqSeq = 1, 3
	m.handleTimelineLoaded(timelineLoadedMsg{req: 3, err: errors.New("context deadline exceeded")})
	if out := stripANSI(m.renderTimeline()); !strings.Contains(out, "retrying in 1s") {
		t.Errorf("after a failed first fetch: %q, want it shown as retrying", out)
	}
	m.handleTimelineTick(timelineTickMsg{gen: 1})
	if m.timeline.loading {
		t.Errorf("refetched before the backoff ran out")
	}
	at := tlAt(7200).Add(time.Second)
	m.now = func() time.Time { return at }
	m.handleTimelineTick(timelineTickMsg{gen: 1})
	if !m.timeline.loading {
		t.Fatalf("not refetched after the backoff")
	}
	m.handleTimelineLoaded(timelineLoadedMsg{req: m.timeline.reqSeq, err: errors.New("again")})
	if got := m.timeline.retryAt.Sub(at); got != 2*time.Second {
		t.Errorf("second backoff = %v, want 2s", got)
	}
	if got := timelineRetryAfter(20); got != timelineRetryCap {
		t.Errorf("backoff after 20 failures = %v, want the cap", got)
	}
	m.handleTimelineLoaded(timelineLoadedMsg{req: m.timeline.reqSeq, tl: sampleTimeline()})
	if m.timeline.fails != 0 || !m.timeline.retryAt.IsZero() {
		t.Errorf("a good load left the backoff: %d %v", m.timeline.fails, m.timeline.retryAt)
	}

	// An old proxy is not retried.
	m = timelineModel(t, 120, 40)
	m.client = deadClient()
	m.timeline.tl, m.timeline.err, m.timeline.tickGen = nil, errTimelineUnsupported, 1
	m.handleTimelineTick(timelineTickMsg{gen: 1})
	if m.timeline.loading {
		t.Errorf("an old proxy's 404 was retried")
	}

	// A live session whose refresh fails waits out its backoff, though its idle refresh is due.
	m = timelineModel(t, 120, 40)
	m.client = deadClient()
	live := tlAt(1800).Add(30 * time.Second)
	m.now = func() time.Time { return live }
	m.timeline.fetchedAt, m.timeline.tickGen, m.timeline.reqSeq = live.Add(-timelineIdleRefresh), 1, 3
	m.handleTimelineLoaded(timelineLoadedMsg{req: 3, err: errors.New("refused")})
	m.handleTimelineTick(timelineTickMsg{gen: 1})
	if m.timeline.loading {
		t.Errorf("a failing live refresh was retried on the next tick, not after its backoff")
	}
}

// The first fetch of a visit gets the long deadline; a refresh the short one.
func TestTimelineFetch_FirstLoadWaitsLonger(t *testing.T) {
	if timelineFirstReadTimeout <= timelineReadTimeout {
		t.Fatalf("first-load deadline %v is not longer than a refresh's %v", timelineFirstReadTimeout, timelineReadTimeout)
	}
}

// ↓ scrolls the agent rows until the last is in view, then the host rows, so every host can be
// reached; the hints count what is above and below, and ↵ only opens what is on screen.
func TestTimelineKeys_ArrowsReachHiddenHosts(t *testing.T) {
	m := timelineModel(t, 140, 24)
	m.timeline.windowIdx = 3
	tl := sampleTimeline()
	for i := 0; i < 15; i++ {
		h := "h" + string(rune('a'+i)) + ".example"
		tl.Hosts = append(tl.Hosts, timeline.Host{Host: h, Kind: timeline.KindHTTP, Start: tlAt(float64(1000 + i)), End: tlPtr(float64(1000 + i)), Seq: uint64(200 + i)})
	}
	m.timeline.tl, m.timeline.layoutKey = tl, tlLayoutKey{}
	lay, _ := m.timelineLayoutNow()
	if m.timelineScrollMax(lay) == 0 {
		t.Fatal("every host fits; the test needs hidden ones")
	}
	if out := stripANSI(m.View()); !strings.Contains(out, "hosts below") || strings.Contains(out, "ho.example") {
		t.Fatalf("before scrolling: want hidden hosts counted and the last not drawn")
	}
	for i := 0; i < m.timelineScrollMax(lay)+3; i++ {
		m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "ho.example") {
		t.Errorf("after scrolling down, the last host is still hidden")
	}
	if !strings.Contains(out, "hosts above") || strings.Contains(out, "hosts below") {
		t.Errorf("at the bottom, the hint does not say the rest are above")
	}
	if m.timeline.scroll != m.timelineScrollMax(lay) {
		t.Errorf("↓ past the end left scroll at %d, want it held at %d", m.timeline.scroll, m.timelineScrollMax(lay))
	}
	// A shrink (a window change, a resize) can leave scroll past what is hidden now; one ↑ must
	// still move the view.
	m.timeline.scroll = m.timelineScrollMax(lay) + 5
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.timeline.scroll != m.timelineScrollMax(lay)-1 {
		t.Errorf("↑ from past the end moved to %d, want %d", m.timeline.scroll, m.timelineScrollMax(lay)-1)
	}
	// llm.example scrolled out of view: ↵ on its cell does not open it.
	m.timeline.scroll = m.timelineScrollMax(lay)
	m.timeline.cursor = cellOf(t, lay, 0)
	for _, r := range m.visibleHostRows(lay) {
		if r.host == "llm.example" {
			t.Fatal("llm.example is still on screen")
		}
	}
	if seq, ok := m.timelineCursorSeq(); ok && seq == 1 && len(m.visibleAgentRows(lay)) == 0 {
		t.Errorf("↵ opened a hidden host's request")
	}
}

// A cell a tool was still running through lists that tool as running since its start, and ↵
// opens it; a tool that started in the cell comes first.
func TestTimelineStrip_ListsAToolStillRunning(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	lay, _ := m.timelineLayoutNow()
	m.timeline.cursor = cellOf(t, lay, 250) // main's Bash runs 200-290s
	strip := stripANSI(m.cursorStrip(lay))
	if !strings.Contains(strip, "Bash") || !strings.Contains(strip, "since") {
		t.Errorf("strip = %q, want the running Bash marked since its start", strip)
	}
	if seq, ok := m.timelineCursorSeq(); !ok || seq != 2 {
		t.Errorf("↵ = %d %v, want the running Bash (seq 2)", seq, ok)
	}
	// A Read starting in the same cell, while the Bash runs on, is listed first, and ↵ opens it.
	cell := lay.axis.cells[cellOf(t, lay, 250)]
	m.timeline.tl.Tools = append(m.timeline.tl.Tools, timeline.Tool{Agent: timeline.MainAgent, ID: "rd", Name: "Read",
		Start: cell.from.Add(time.Second), End: tlPtr(260), Preview: "/c.go", Seq: 3})
	m.timeline.layoutKey = tlLayoutKey{}
	lay, _ = m.timelineLayoutNow()
	strip = stripANSI(m.cursorStrip(lay))
	if i, j := strings.Index(strip, "/c.go"), strings.Index(strip, "since"); i < 0 || j < 0 || j < i {
		t.Errorf("strip = %q, want the Read that started here before the Bash still running", strip)
	}
	if seq, ok := m.timelineCursorSeq(); !ok || seq != 3 {
		t.Errorf("↵ = %d %v, want the Read that started here (seq 3)", seq, ok)
	}
}

// A cell with no tool names the request ↵ would open rather than saying nothing happened.
func TestTimelineStrip_NamesTheRequestWhenNoToolRan(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	lay, _ := m.timelineLayoutNow()
	m.timeline.cursor = cellOf(t, lay, 5) // main's first model call: no tool yet, one request
	strip := stripANSI(m.cursorStrip(lay))
	if !strings.Contains(strip, "llm.example") {
		t.Errorf("strip = %q, want the request named", strip)
	}
	if seq, ok := m.timelineCursorSeq(); !ok || seq != 1 {
		t.Errorf("↵ = %d %v, want llm.example's request (seq 1)", seq, ok)
	}
}

// A live session's "all" runs to now, as its other windows do: the wait after its last reply is
// drawn as waiting on the user, not cut off at the reply.
func TestTimelineAll_LiveSessionRunsToNow(t *testing.T) {
	m := timelineModel(t, 140, 46)
	live := tlAt(1800).Add(10 * time.Minute) // the last reply ended at 1800s; live for 30 minutes
	m.now = func() time.Time { return live }
	m.timeline.fetchedAt, m.timeline.windowIdx = live, 3
	lay, _ := m.timelineLayoutNow()
	if !lay.to.Equal(live) {
		t.Fatalf("all ends at %v, want now %v", lay.to.Sub(tlBase), live.Sub(tlBase))
	}
	last := lay.agents[0].cells[lay.axis.width()-1]
	if lay.axis.cells[lay.axis.width()-1].gap || !near(last.user, 1) {
		t.Errorf("the newest cell = %+v, want waiting on the user", last)
	}
}

// A live wait is drawn to scale while short; once it would take more than timelineTailCells, it
// is squeezed into them and labelled with its length, so the work keeps the width.
func TestTimelineAll_LongLiveWaitIsCapped(t *testing.T) {
	for _, tc := range []struct {
		after  time.Duration
		capped bool
	}{{20 * time.Second, false}, {25 * time.Minute, true}} {
		m := timelineModel(t, 140, 46)
		live := tlAt(1800).Add(tc.after)
		m.now = func() time.Time { return live }
		m.timeline.fetchedAt, m.timeline.windowIdx = live, 3
		lay, _ := m.timelineLayoutNow()
		w := lay.axis.width()
		if !lay.axis.cells[w-1].to.Equal(live) || !near(lay.agents[0].cells[w-1].user, 1) {
			t.Errorf("%v on: the newest cell does not end at now waiting on the user", tc.after)
		}
		tailCells := lay.axis.cellsFrom(tlAt(1800))
		if tc.capped && (tailCells != timelineTailCells || lay.axis.tail != timelineTailCells) {
			t.Errorf("%v on: the wait takes %d cells (tail %d), want %d", tc.after, tailCells, lay.axis.tail, timelineTailCells)
		}
		if !tc.capped && (tailCells > timelineTailCells || lay.axis.tail != 0) {
			t.Errorf("%v on: a short wait took %d cells (tail %d), want it drawn to scale", tc.after, tailCells, lay.axis.tail)
		}
		if label := axisLabels(lay); tc.capped != strings.Contains(label, "+"+tlDuration(tc.after)) {
			t.Errorf("%v on: axis %q, want the squeezed wait labelled only when squeezed", tc.after, label)
		}
	}
}

// A question the agent put to the user is the user's time: grey, not amber, and kept out of the
// row's tool total.
func TestTimelineWaits_AskUserQuestionIsWaitingOnYou(t *testing.T) {
	tl := sampleTimeline() // main finishes a reply at 1200s; the user answers at 1500s
	tl.Tools = append(tl.Tools, timeline.Tool{Agent: timeline.MainAgent, ID: "q1", Name: "AskUserQuestion", Start: tlAt(1200), End: tlPtr(1500), Seq: 35})
	lay := buildTimelineLayout(tl, tlAt(0), tlAt(1800), 180, tlAt(3600))
	if c := lay.agents[0].cells[cellOf(t, lay, 1300)]; !near(c.user, 1) || c.tool != 0 {
		t.Errorf("while the question waited: %+v, want waiting on the user", c)
	}
	without := buildTimelineLayout(sampleTimeline(), tlAt(0), tlAt(1800), 180, tlAt(3600))
	if got, want := lay.agents[0].toolTime, without.agents[0].toolTime; got != want {
		t.Errorf("tool time = %s, want %s: the question is not tool time", got, want)
	}

	// At "all", a question left overnight collapses like any other wait on the user, so the work
	// keeps the width; and one still open is the live wait's capped tail.
	overnight := func(ask bool) *timeline.Timeline {
		tl := sampleTimeline()
		if ask {
			tl.Tools = append(tl.Tools, timeline.Tool{Agent: timeline.MainAgent, ID: "q1", Name: "AskUserQuestion", Start: tlAt(1800), End: tlPtr(1800 + 7*3600), Seq: 45})
		}
		tl.Turns = append(tl.Turns, timeline.Turn{Agent: timeline.MainAgent, Start: tlAt(1800 + 7*3600), End: tlPtr(1800 + 7*3600 + 60), Finish: "end_turn", Seq: 50})
		tl.End = tlAt(1800 + 7*3600 + 60)
		return tl
	}
	work := func(tl *timeline.Timeline) int {
		ax := buildTimelineLayout(tl, time.Time{}, time.Time{}, 180, tl.End.Add(time.Hour)).axis
		return ax.width() - ax.cellsFrom(tlAt(1800))
	}
	if got, want := work(overnight(true)), work(overnight(false)); got != want {
		t.Errorf("at all, the work before an overnight question got %d cells, want %d as before a plain wait", got, want)
	}
	open := sampleTimeline()
	open.Tools = append(open.Tools, timeline.Tool{Agent: timeline.MainAgent, ID: "q1", Name: "AskUserQuestion", Start: tlAt(1800), Seq: 45})
	if ax := buildTimelineLayout(open, time.Time{}, tlAt(1800+7*3600), 180, tlAt(1800+7*3600)).axis; ax.tail != timelineTailCells {
		t.Errorf("an open question's wait has tail %d, want the capped %d", ax.tail, timelineTailCells)
	}
}

// A cell inside a turn that carries a model names it, so a client running several models
// side by side (Bob's classifier beside its main turns) reads apart.
func TestTimelineStrip_NamesTheTurnModel(t *testing.T) {
	m := timelineModel(t, 140, 46)
	m.timeline.windowIdx = 3
	lay, _ := m.timelineLayoutNow()
	m.timeline.cursor = cellOf(t, lay, 5) // main's first model call: no tool yet
	for i := range m.timeline.tl.Turns {
		if tu := &m.timeline.tl.Turns[i]; tu.Agent == timeline.MainAgent && tu.Start.Equal(tlAt(0)) {
			tu.Model = "premium-ide"
		}
	}
	m.timeline.layoutKey = tlLayoutKey{}
	lay, _ = m.timelineLayoutNow()
	strip := stripANSI(m.cursorStrip(lay))
	if !strings.Contains(strip, "premium-ide") {
		t.Errorf("strip = %q, want the turn's model named", strip)
	}
}
