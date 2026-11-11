package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// trExchange is exchange n of the session: a request at `at` seconds (seq 2n-1) and its
// response two seconds later (seq 2n), from subagent sub ("" for the main agent).
func trExchange(n int, at float64, sub string) []pipeline.SessionEvent {
	id := "r" + string(rune('a'+n))
	return []pipeline.SessionEvent{
		{At: tlAt(at), Seq: uint64(2*n - 1), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			Host: "api.test", RequestID: id, Subagent: sub, Inference: &pipeline.InferenceExtension{Model: "claude-opus-5-5"}},
		{At: tlAt(at + 2), Seq: uint64(2 * n), Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
			Host: "api.test", RequestID: id, Subagent: sub, StatusCode: 200, Duration: 2 * time.Second,
			Inference: &pipeline.InferenceExtension{Model: "claude-opus-5-5"}},
	}
}

// trTool is a tool that the response with seq asked for, from start to end seconds (end < 0:
// still running).
func trTool(seq uint64, id, name string, start, end float64, preview string) timeline.Tool {
	t := timeline.Tool{Agent: timeline.MainAgent, ID: id, Name: name, Start: tlAt(start), Preview: preview, Seq: seq}
	if end >= 0 {
		t.End = tlPtr(end)
	}
	return t
}

// toolRowsModel is the events pane on session "sess-1" with tl loaded for it, at 100s.
func toolRowsModel(t *testing.T, events []pipeline.SessionEvent, tl *timeline.Timeline) *model {
	t.Helper()
	m := fitModel(t, paneEvents, 200, 40, events)
	m.now = func() time.Time { return tlAt(100) }
	m.timeline = timelineState{session: m.selectedSess, cursor: -1, tl: tl, tools: indexTools(tl)}
	m.rebuildEventsTable()
	return m
}

// colPos is the position of column id in the rows just built, -1 when not shown.
func colPos(m *model, id eventColumnID) int {
	cols, _ := layoutColumns(m.eventColumns, m.eventColWidths, m.width)
	return slices.IndexFunc(cols, func(c eventColumn) bool { return c.id == id })
}

// toolRowAt is the rendered row of the tool with id, and its index.
func toolRowAt(t *testing.T, m *model, id string) (int, []string) {
	t.Helper()
	i := slices.IndexFunc(m.visibleRows, func(r eventRow) bool { return r.tool != nil && r.tool.ID == id })
	if i < 0 {
		t.Fatalf("no row for tool %q", id)
	}
	return i, m.eventsTbl.Rows()[i]
}

func toolCell(m *model, row []string, id eventColumnID) string {
	return strings.TrimSpace(row[colPos(m, id)])
}

func trailingOf(row []string) string { return row[len(row)-1] }

func TestToolRows_OneTool(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 30, ""))
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{
		trTool(2, "t1", "Bash", 12, 16.21, "git -C /tmp/wt status"),
	}})
	i, row := toolRowAt(t, m, "t1")
	if m.visibleRows[i-1].resp == nil || m.visibleRows[i-1].resp.Seq != 2 {
		t.Fatalf("the tool row is not right after its exchange: %v", m.visibleRows[i-1])
	}
	for id, want := range map[eventColumnID]string{
		colIndex: "┗⚒", colDir: "", colTime: "12:00:12.00", colAction: "", colPlugin: "",
		colMethod: "Bash", colStatus: "ok", colDuration: "4.21s",
	} {
		if got := toolCell(m, row, id); got != want {
			t.Errorf("%s = %q, want %q", id, got, want)
		}
	}
	if got := trailingOf(row); got != "" {
		t.Errorf("trailing = %q, want no preview", got)
	}
	if want := colPos(m, colDuration) + 2; len(row) != want {
		t.Errorf("cells = %d, want %d", len(row), want)
	}
}

func TestToolRows_Parallel(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{
		trTool(2, "t1", "Bash", 12, 25.21, "a"),
		trTool(2, "t2", "Read", 12, 25.21, "b"),
	}})
	_, r1 := toolRowAt(t, m, "t1")
	_, r2 := toolRowAt(t, m, "t2")
	if toolCell(m, r1, colIndex) != "┣⚒" || toolCell(m, r2, colIndex) != "┗⚒" {
		t.Errorf("# cells = %q, %q", toolCell(m, r1, colIndex), toolCell(m, r2, colIndex))
	}
	for _, r := range [][]string{r1, r2} {
		if d := toolCell(m, r, colDuration); !strings.HasPrefix(d, "∥") {
			t.Errorf("DURATION = %q, want a ∥ prefix", d)
		}
	}
}

func TestToolRows_States(t *testing.T) {
	running := trTool(2, "run", "Bash", 12, -1, "")
	failed := trTool(2, "err", "Bash", 12, 13, "")
	failed.Error = true
	lost := trTool(2, "lost", "Bash", 12, 14, "")
	lost.Lost = true
	end := trTool(2, "end", "StructuredOutput", 12, 15, "done")
	end.Lost = true
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{running, failed, lost, end}})
	for _, c := range []struct{ id, status, duration, trailing string }{
		{"run", "running", "⋯ 1m28s", ""},
		{"err", "error", "1.00s", ""},
		{"lost", "lost", "", ""},
		{"end", "", "", "ends run"},
	} {
		_, row := toolRowAt(t, m, c.id)
		if got := toolCell(m, row, colStatus); got != c.status {
			t.Errorf("%s: STATUS = %q, want %q", c.id, got, c.status)
		}
		if got := toolCell(m, row, colDuration); got != c.duration {
			t.Errorf("%s: DURATION = %q, want %q", c.id, got, c.duration)
		}
		if got := trailingOf(row); got != c.trailing {
			t.Errorf("%s: trailing = %q, want %q", c.id, got, c.trailing)
		}
	}
}

func TestToolRows_RunAndBg(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{
		Tools: []timeline.Tool{
			trTool(2, "spawn", "Agent", 12, 13, "explore"),
			trTool(2, "bash", "Bash", 12, 14, "go test"),
		},
		Agents: []timeline.Agent{{ID: "a1", Parent: timeline.MainAgent, SpawnTool: "spawn", SpawnedAt: tlPtr(12), ReturnedAt: tlPtr(60)}},
		Jobs:   []timeline.Job{{Agent: timeline.MainAgent, Start: tlAt(14), End: tlPtr(20)}},
	})
	if _, row := toolRowAt(t, m, "spawn"); !strings.HasPrefix(trailingOf(row), "run ") {
		t.Errorf("Agent trailing = %q", trailingOf(row))
	}
	if _, row := toolRowAt(t, m, "bash"); !strings.HasPrefix(trailingOf(row), "bg ") {
		t.Errorf("Bash trailing = %q", trailingOf(row))
	}
}

func TestToolRows_InExpandedBlock(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, "a1"), trExchange(3, 30, ""))
	tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(4, "sub", "Read", 22, 23, "x")}}
	tl.Tools[0].Agent = "a1"
	m := toolRowsModel(t, events, tl)
	if slices.ContainsFunc(m.visibleRows, func(r eventRow) bool { return r.tool != nil }) {
		t.Fatalf("a collapsed block shows a tool row")
	}
	m.subagentOpen = map[string]bool{"a1": true}
	m.rebuildEventsTable()
	i, row := toolRowAt(t, m, "sub")
	if r := m.visibleRows[i-1]; r.resp == nil || r.resp.Seq != 4 {
		t.Errorf("the tool row is not right after its member exchange")
	}
	if got := toolCell(m, row, colDir); got != "│" {
		t.Errorf("DIR = %q, want the block gutter", got)
	}
}

func TestToolRows_HideInactiveHidesTools(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	events[2].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "p", Action: pipeline.ActionAllow}}}
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{
		trTool(2, "hidden", "Bash", 12, 13, ""),
		trTool(4, "shown", "Bash", 22, 23, ""),
	}})
	m.hideInactive = true
	m.rebuildEventsTable()
	toolRowAt(t, m, "shown")
	if slices.ContainsFunc(m.visibleRows, func(r eventRow) bool { return r.tool != nil && r.tool.ID == "hidden" }) {
		t.Errorf("the tool of a hidden exchange still shows")
	}
}

func TestToolRows_SortByDuration(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	events[1].Duration = 4400 * time.Millisecond
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{trTool(4, "long", "Bash", 62, -1, "")}})
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	i, row := toolRowAt(t, m, "long")
	if i != 0 {
		t.Errorf("the 38s tool is row %d, want 0 above the 4.4s exchange", i)
	}
	if got := toolCell(m, row, colIndex); got != "↳2" {
		t.Errorf("# = %q, want ↳2", got)
	}
}

func TestToolRows_SortOtherColumnIsStable(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{
		trTool(2, "a", "Bash", 12, 13, ""),
		trTool(4, "b", "Bash", 22, 23, ""),
	}})
	m.sortCol = colCost
	m.rebuildEventsTable()
	var got []string
	for _, r := range m.visibleRows {
		switch {
		case r.tool != nil:
			got = append(got, r.tool.ID)
		case r.resp != nil:
			got = append(got, r.resp.RequestID)
		}
	}
	if want := []string{"rb", "a", "rc", "b"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestToolRows_SearchFindsPreview(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{
		trTool(2, "t1", "Bash", 12, 13, "git -C /tmp/wt status"),
	}})
	m.search = map[paneID]string{paneEvents: "git -C"}
	m.rebuildEventsTable()
	i, _ := toolRowAt(t, m, "t1")
	if !slices.Contains(m.eventsTbl.Matches(), i) {
		t.Errorf("matches = %v, want row %d", m.eventsTbl.Matches(), i)
	}
}

// The preview is in no cell, and with no METHOD shown the tool name is in none either, so
// search must still find both in the row's dropped text.
func TestToolRows_SearchFindsHiddenText(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{
		trTool(2, "t1", "Bash", 12, 13, "git -C /tmp/wt status"),
	}})
	for _, id := range []eventColumnID{colBytes, colTokens, colCost, colHost, colMethod} {
		m.eventColumns[id] = false
	}
	for _, q := range []string{"git -C", "Bash"} {
		m.search = map[paneID]string{paneEvents: q}
		m.rebuildEventsTable()
		if p := colPos(m, colMethod); p >= 0 {
			t.Fatalf("METHOD still shown at %d", p)
		}
		i, row := toolRowAt(t, m, "t1")
		if strings.Contains(strings.Join(row, " "), q) {
			t.Fatalf("query %q is in a shown cell %q; the test needs it hidden", q, row)
		}
		if !slices.Contains(m.eventsTbl.Matches(), i) {
			t.Errorf("query %q: matches = %v, want row %d", q, m.eventsTbl.Matches(), i)
		}
	}
}

func TestToolRows_CursorStaysOnTool(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}})
	i, _ := toolRowAt(t, m, "t1")
	setCursorVisible(&m.eventsTbl, i)
	m.selectedEventKey = m.selectedPinKey()
	want := m.selectedEventKey
	m.events[m.selectedSess] = append(m.events[m.selectedSess], trExchange(3, 30, "")...)
	m.rebuildEventsTable()
	if got := m.selectedPinKey(); got != want {
		t.Fatalf("after a new event the cursor is on %v, want %v", got, want)
	}
	m.sortCol, m.sortDesc = colTime, true
	m.rebuildEventsTable()
	if got := m.selectedPinKey(); got != want {
		t.Errorf("after a sort the cursor is on %v, want %v", got, want)
	}
}

func TestToolRows_TailFollowOnToolRow(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}})
	setCursorVisible(&m.eventsTbl, len(m.eventsTbl.Rows())-1)
	m.events[m.selectedSess] = append(m.events[m.selectedSess], trExchange(2, 20, "")...)
	tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, ""), trTool(4, "t2", "Bash", 22, 23, "")}}
	m.timeline.tl, m.timeline.tools = tl, indexTools(tl)
	m.rebuildEventsTable()
	if c, n := m.eventsTbl.Cursor(), len(m.eventsTbl.Rows()); c != n-1 {
		t.Errorf("cursor %d, want the last row %d", c, n-1)
	}
	if m.selectedEvent() != nil {
		t.Errorf("a tool row has an event")
	}
}

func TestToolRows_TimelineBeforeEvents(t *testing.T) {
	m := toolRowsModel(t, nil, &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}})
	if len(m.visibleRows) != 0 {
		t.Fatalf("rows with no events: %d", len(m.visibleRows))
	}
	m.events[m.selectedSess] = trExchange(1, 10, "")
	m.rebuildEventsTable()
	toolRowAt(t, m, "t1")
}

func TestToolRows_OrphanToolNotShown(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{trTool(99, "t1", "Bash", 12, 13, "")}})
	if slices.ContainsFunc(m.visibleRows, func(r eventRow) bool { return r.tool != nil }) {
		t.Errorf("a tool with no exchange shows")
	}
}

func TestToolRows_NarrowTerminalCellCount(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "x")}})
	for _, w := range []int{40, 60, 80} {
		m.width = w
		m.rebuildEventsTable()
		toolRowAt(t, m, "t1")
		for i, row := range m.eventsTbl.Rows() {
			if len(row) > len(m.eventsTbl.Columns()) {
				t.Errorf("width %d row %d: %d cells, %d columns", w, i, len(row), len(m.eventsTbl.Columns()))
			}
		}
		_ = m.eventsTbl.View()
	}
}

func TestToolRows_DetailSearchSkipsToolRows(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}})
	ti, _ := toolRowAt(t, m, "t1")
	m.detailSearch = &detailSearch{
		hits: map[eventKey]bool{keyOf(m.visibleRows[0].event): true},
		last: map[string]uint64{m.selectedSess: 1}, oldest: map[string]uint64{m.selectedSess: 1},
		newest: map[string]uint64{m.selectedSess: 0},
	}
	_ = m.detailSearchStale()
	for _, rows := range [][]int{m.detailMatchRows(), m.detailNewRows()} {
		if slices.Contains(rows, ti) {
			t.Errorf("rows %v hold the tool row %d", rows, ti)
		}
	}
}

func TestToolRows_LinkedEventNoTools(t *testing.T) {
	events := trExchange(1, 10, "")
	events[0].SessionID, events[1].SessionID = "bucket", "bucket"
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}})
	if slices.ContainsFunc(m.visibleRows, func(r eventRow) bool { return r.tool != nil }) {
		t.Errorf("a linked exchange got the session's tool")
	}
}

// The model writes a tool's name and preview: a control rune in either must not reach the
// terminal, where an OSC 52 sequence would write the clipboard.
func TestToolRows_ControlRunesRemoved(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 30, ""))
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{
		trTool(2, "t1", "Bash\x1b[2J", 12, 16, "echo \x1b]52;c;aGk=\x07"),
	}})
	_, row := toolRowAt(t, m, "t1")
	for _, cell := range []string{toolCell(m, row, colMethod), trailingOf(row)} {
		if strings.ContainsAny(cell, "\x1b\x07") {
			t.Errorf("control rune in cell %q", cell)
		}
	}
}

// Under a sort, a tool row of exchange 1000 or more shows its parent's number whole: the
// # column is 4 wide, so ↳ gives way to the number.
func TestToolRows_SortedParentNumberFits(t *testing.T) {
	var events []pipeline.SessionEvent
	for n := 1; n <= 1000; n++ {
		events = append(events, trExchange(n, float64(n*3), "")...)
	}
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{
		trTool(2000, "big", "Bash", 3002, 3003, ""),
		trTool(1998, "small", "Bash", 2999, 3000, ""),
	}})
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	if _, row := toolRowAt(t, m, "big"); toolCell(m, row, colIndex) != "1000" {
		t.Errorf("exchange 1000: # = %q, want 1000", toolCell(m, row, colIndex))
	}
	if _, row := toolRowAt(t, m, "small"); toolCell(m, row, colIndex) != "↳999" {
		t.Errorf("exchange 999: # = %q, want ↳999", toolCell(m, row, colIndex))
	}
}

// A tool pin survives a rebuild while the timeline is not loaded yet, for example after a `T`
// look at another session: the tool row comes back when the timeline lands.
func TestToolRows_PinWaitsForTheTimeline(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}}
	m := toolRowsModel(t, events, tl)
	i, _ := toolRowAt(t, m, "t1")
	setCursorVisible(&m.eventsTbl, i)
	m.selectedEventKey = m.selectedPinKey()
	want := m.selectedEventKey
	m.setTimelineSession("other")
	m.setTimelineSession(m.selectedSess)
	m.rebuildEventsTable()
	if m.selectedEventKey != want {
		t.Fatalf("a rebuild with no timeline cleared the tool pin: %v", m.selectedEventKey)
	}
	m.handleTimelineLoaded(timelineLoadedMsg{tl: tl, req: m.timeline.reqSeq})
	if got := m.selectedPinKey(); got != want {
		t.Errorf("after the timeline landed the cursor is on %v, want %v", got, want)
	}
}

// A tool call with no ID (some local models send none) can be pinned.
func TestToolRows_PinToolWithNoID(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "", "Bash", 12, 13, "")}})
	i, _ := toolRowAt(t, m, "")
	setCursorVisible(&m.eventsTbl, i)
	m.selectedEventKey = m.selectedPinKey()
	want := m.selectedEventKey
	m.events[m.selectedSess] = append(m.events[m.selectedSess], trExchange(3, 30, "")...)
	m.rebuildEventsTable()
	if got := m.selectedPinKey(); got != want || m.eventsTbl.Cursor() != i {
		t.Errorf("after a new event the cursor is on row %d %v, want row %d %v", m.eventsTbl.Cursor(), got, i, want)
	}
}

// A tool pin stops waiting when the timeline fetch fails.
func TestToolRows_PinStopsWaitingOnAFailedFetch(t *testing.T) {
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}})
	i, _ := toolRowAt(t, m, "t1")
	setCursorVisible(&m.eventsTbl, i)
	m.selectedEventKey = m.selectedPinKey()
	m.setTimelineSession("other")
	m.setTimelineSession(m.selectedSess)
	m.handleTimelineLoaded(timelineLoadedMsg{err: errors.New("boom"), req: m.timeline.reqSeq})
	m.rebuildEventsTable()
	if m.selectedEventKey != (eventKey{}) {
		t.Errorf("after a failed fetch the tool pin still waits: %v", m.selectedEventKey)
	}
}

// Tail-follow on a tool row survives a look at another session: while the timeline loads
// again, the cursor follows the end, and it stays at the end when the timeline lands.
func TestToolRows_TailFollowWhileTheTimelineLoadsAgain(t *testing.T) {
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}})
	setCursorVisible(&m.eventsTbl, len(m.eventsTbl.Rows())-1)
	m.selectedEventKey = m.selectedPinKey()
	m.timeline.tl, m.timeline.tools = nil, nil // a T look at another session dropped it
	m.rebuildEventsTable()
	m.events[m.selectedSess] = append(m.events[m.selectedSess], trExchange(2, 20, "")...)
	m.rebuildEventsTable()
	tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, ""), trTool(4, "t2", "Bash", 22, 23, "")}}
	m.timeline.tl, m.timeline.tools = tl, indexTools(tl)
	m.rebuildEventsTable()
	if c, n := m.eventsTbl.Cursor(), len(m.eventsTbl.Rows()); c != n-1 {
		t.Errorf("cursor %d, want the last row %d", c, n-1)
	}
}
