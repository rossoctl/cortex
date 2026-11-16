package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// actOnFirst gives event seq 1 an acting invocation, so hideInactive keeps exchange 1 only.
func actOnFirst(events []pipeline.SessionEvent) {
	events[0].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "p", Action: pipeline.ActionAllow}}}
}

func TestToolTarget(t *testing.T) {
	lost := trTool(2, "t1", "Bash", 12, 20, "")
	lost.Lost = true
	handback := trTool(2, "t1", "SubagentHandback", 12, -1, "")
	handback.Lost = true
	for _, c := range []struct {
		name      string
		tools     []timeline.Tool
		hide      bool
		wantSeq   uint64
		inRequest bool
		ok        bool
	}{
		{"ended", []timeline.Tool{trTool(2, "t1", "Bash", 12, 20, "")}, false, 3, true, true},
		{"running", []timeline.Tool{trTool(2, "t1", "Bash", 12, -1, "")}, false, 1, false, true},
		{"lost", []timeline.Tool{lost}, false, 1, false, true},
		{"SubagentHandback", []timeline.Tool{handback}, false, 1, false, true},
		{"clamped end", []timeline.Tool{trTool(2, "t1", "Bash", 12, 13, "")}, false, 1, false, true},
		{"hidden by filter", []timeline.Tool{trTool(2, "t1", "Bash", 12, 20, "")}, true, 3, true, true},
		{"not held", []timeline.Tool{trTool(99, "t1", "Bash", 12, 50, "")}, false, 0, false, false},
		{"parallel", []timeline.Tool{trTool(2, "t1", "Bash", 12, 20, ""), trTool(2, "t2", "Read", 12, 20, "")}, false, 3, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
			if c.hide {
				actOnFirst(events)
			}
			m := toolRowsModel(t, events, &timeline.Timeline{Tools: c.tools})
			if c.hide {
				m.hideInactive = true
				m.rebuildEventsTable()
				if slices.ContainsFunc(m.visibleRows, func(r eventRow) bool { return r.event != nil && r.event.Seq == 3 }) {
					t.Fatal("seq 3 still shows")
				}
			}
			for _, tool := range c.tools {
				row, _, inRequest, ok := m.toolTarget(&toolRow{Tool: tool})
				if ok != c.ok {
					t.Fatalf("%s: ok = %v, want %v", tool.ID, ok, c.ok)
				}
				if !ok {
					continue
				}
				if row.event.Seq != c.wantSeq || inRequest != c.inRequest {
					t.Errorf("%s: seq %d inRequest %v, want %d %v", tool.ID, row.event.Seq, inRequest, c.wantSeq, c.inRequest)
				}
			}
		})
	}
}

// enterOnTool puts the cursor on tool t1 and presses Enter. It returns the tool row's index.
func enterOnTool(t *testing.T, m *model) int {
	t.Helper()
	i, _ := toolRowAt(t, m, "t1")
	setCursorVisible(&m.eventsTbl, i)
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	return i
}

func endedModel(t *testing.T, hide bool) *model {
	t.Helper()
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	if hide {
		actOnFirst(events)
	}
	m := toolRowsModel(t, events, &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Bash", 12, 20, "")}})
	if hide {
		m.hideInactive = true
		m.rebuildEventsTable()
	}
	return m
}

func TestToolEnter_OpensTarget(t *testing.T) {
	m := endedModel(t, false)
	j := slices.IndexFunc(m.visibleRows, func(r eventRow) bool { return r.event != nil && r.event.Seq == 3 })
	want := toolCell(m, m.eventsTbl.Rows()[j], colIndex)
	enterOnTool(t, m)
	if m.pane != paneDetail || m.detailRow.event == nil || m.detailRow.event.Seq != 3 {
		t.Fatalf("pane %v, detail row %v", m.pane, m.detailRow.event)
	}
	if m.detailNum != want {
		t.Errorf("detailNum = %q, want %q", m.detailNum, want)
	}
	if strings.ContainsAny(m.detailNum, "⚒↳") {
		t.Errorf("detailNum = %q holds a tool mark", m.detailNum)
	}
}

func TestToolEnter_HeaderWhenHidden(t *testing.T) {
	m := endedModel(t, true)
	enterOnTool(t, m)
	if m.pane != paneDetail || m.detailNum != "2" {
		t.Errorf("pane %v, detailNum = %q, want 2", m.pane, m.detailNum)
	}
}

func TestToolEnter_EscBack(t *testing.T) {
	m := endedModel(t, false)
	i, _ := toolRowAt(t, m, "t1")
	setCursorVisible(&m.eventsTbl, i)
	key := m.selectedPinKey()
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pane != paneEvents || m.eventsTbl.Cursor() != i || m.selectedPinKey() != key {
		t.Errorf("pane %v, cursor %d (want %d), key %v (want %v)", m.pane, m.eventsTbl.Cursor(), i, m.selectedPinKey(), key)
	}
}

// The held events lost the tool's exchange after the table was built: Enter opens nothing.
func TestToolEnter_NoTarget(t *testing.T) {
	m := endedModel(t, false)
	m.events[m.selectedSess] = trExchange(5, 50, "")
	enterOnTool(t, m)
	if m.pane != paneEvents {
		t.Errorf("pane %v, want the events pane", m.pane)
	}
}

// callMsgs is a turn where the assistant calls tool id and the user sends its result back.
func callMsgs(id string) []pipeline.InferenceMessage {
	return []pipeline.InferenceMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCallNames: "Bash", ToolUseIDs: id},
		{Role: "user", ToolUseIDs: id},
	}
}

// aimModel is exchanges 1 and 2 with tool t1 asked for by seq 2 and ended at seq 3, whose
// request holds msgs, the call and its result.
func aimModel(t *testing.T, msgs []pipeline.InferenceMessage, tools ...timeline.Tool) *model {
	t.Helper()
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""))
	events[1].Inference.ToolCalls = []pipeline.InferenceToolCall{{ID: "t1", Name: "Bash"}}
	events[2].Inference.Messages = msgs
	events[2].Inference.HistoryToolCalls = []pipeline.InferenceToolCall{{ID: "t1", Name: "Bash", Arguments: `{"command":"ls"}`}}
	events[2].Inference.ToolResults = []pipeline.InferenceToolResult{{ToolUseID: "t1", Content: "out"}}
	if len(tools) == 0 {
		tools = []timeline.Tool{trTool(2, "t1", "Bash", 12, 20, "")}
	}
	return toolRowsModel(t, events, &timeline.Timeline{Tools: tools})
}

// enterOnRow puts the cursor on the first tool row that pick accepts and presses Enter.
func enterOnRow(t *testing.T, m *model, pick func(*toolRow) bool) {
	t.Helper()
	i := slices.IndexFunc(m.visibleRows, func(r eventRow) bool { return r.tool != nil && pick(r.tool) })
	if i < 0 {
		t.Fatal("no such tool row")
	}
	setCursorVisible(&m.eventsTbl, i)
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
}

// assertCursorAt checks that the cursor and its line are on the node at path.
func assertCursorAt(t *testing.T, m *model, path string) {
	t.Helper()
	if m.detailCursorPath != path {
		t.Fatalf("cursor path = %q, want %q", m.detailCursorPath, path)
	}
	if l := m.detailLines[m.detailDoc.cursor]; l.path != path {
		t.Errorf("cursor line path = %q, want %q", l.path, path)
	}
}

func assertOpen(t *testing.T, m *model, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if !m.detailFolds[p].open {
			t.Errorf("fold %q is not open", p)
		}
	}
}

func TestToolEnter_AimOnCall(t *testing.T) {
	m := aimModel(t, callMsgs("t1"))
	enterOnTool(t, m)
	assertCursorAt(t, m, "request.inference.messages[1].toolCalls[0]")
	assertOpen(t, m, "request.inference.messages[1].toolCalls", "request.inference.messages[1].toolCalls[0]",
		"request.inference.messages[2].toolResults", "request.inference.messages[2].toolResults[0]")
	if m.detailAim != nil {
		t.Error("the aim is still pending after it applied")
	}
}

func TestToolEnter_AimAfterFetch(t *testing.T) {
	m := aimModel(t, nil)
	m.serverProjects = true
	full := m.events[m.selectedSess][2]
	full.Inference = &pipeline.InferenceExtension{Model: "claude-opus-5-5", Messages: callMsgs("t1"),
		HistoryToolCalls: []pipeline.InferenceToolCall{{ID: "t1", Name: "Bash", Arguments: `{"command":"ls"}`}},
		ToolResults:      []pipeline.InferenceToolResult{{ToolUseID: "t1", Content: "out"}}}
	enterOnTool(t, m)
	if m.detailCursorPath != "" || m.detailAim == nil {
		t.Fatalf("before the fetch: cursor %q, aim %v", m.detailCursorPath, m.detailAim)
	}
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: m.selectedSess, seq: 3, event: &full})
	assertCursorAt(t, m, "request.inference.messages[1].toolCalls[0]")
}

func TestToolEnter_ReaderMoveHolds(t *testing.T) {
	m := aimModel(t, callMsgs("t1"))
	enterOnTool(t, m)
	m.handleKey(keyRune('j'))
	moved := m.detailCursorPath
	m.showDetail(m.detailRow, false)
	if m.detailCursorPath != moved {
		t.Errorf("re-render moved the cursor from %q to %q", moved, m.detailCursorPath)
	}

	// A move before the fetch holds too.
	m = aimModel(t, nil)
	m.serverProjects = true
	full := m.events[m.selectedSess][2]
	full.Inference = &pipeline.InferenceExtension{Model: "claude-opus-5-5", Messages: callMsgs("t1")}
	enterOnTool(t, m)
	m.handleKey(keyRune('j'))
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: m.selectedSess, seq: 3, event: &full})
	if m.detailCursorPath == "request.inference.messages[1].toolCalls[0]" {
		t.Error("the fetch moved the cursor to the call after the reader moved it")
	}
}

func TestToolEnter_AimInEarlierGroup(t *testing.T) {
	m := aimModel(t, append(callMsgs("t1"), pipeline.InferenceMessage{Role: "assistant", Content: "done"}))
	enterOnTool(t, m)
	if !strings.HasPrefix(m.detailCursorPath, "request.inference.messages[<") {
		t.Fatalf("cursor path = %q, want one in the earlier group", m.detailCursorPath)
	}
	assertCursorAt(t, m, "request.inference.messages[<3][1].toolCalls[0]")
	assertOpen(t, m, "request.inference.messages[<3]")
}

func TestToolEnter_ReusedID(t *testing.T) {
	msgs := append(callMsgs("call_0"), callMsgs("call_0")[1:]...)
	m := aimModel(t, msgs, trTool(2, "call_0", "Bash", 12, 20, ""))
	enterOnRow(t, m, func(r *toolRow) bool { return r.ID == "call_0" })
	// nestToolBlocks gives the one call to the first message that names it, so the last
	// one holds no entry to aim at and the message is the target.
	assertCursorAt(t, m, "request.inference.messages[3]")
}

func TestToolEnter_EmptyID(t *testing.T) {
	m := aimModel(t, callMsgs("t1"), trTool(2, "", "Bash", 12, 20, ""))
	enterOnRow(t, m, func(r *toolRow) bool { return r.ID == "" })
	if m.pane != paneDetail || m.detailAim != nil || m.detailCursorPath != "" {
		t.Errorf("pane %v, aim %v, cursor %q", m.pane, m.detailAim, m.detailCursorPath)
	}
}

func TestToolEnter_RunningAimsResponse(t *testing.T) {
	m := aimModel(t, nil, trTool(2, "t1", "Bash", 12, -1, ""))
	enterOnTool(t, m)
	assertCursorAt(t, m, "response.inference.toolCalls[0]")

	m = aimModel(t, nil, trTool(2, "call_0", "Bash", 12, -1, ""), trTool(2, "call_0", "Read", 12, -1, ""))
	m.events[m.selectedSess][1].Inference.ToolCalls = []pipeline.InferenceToolCall{{ID: "call_0", Name: "Bash"}, {ID: "call_0", Name: "Read"}}
	enterOnRow(t, m, func(r *toolRow) bool { return r.Pos == 1 })
	assertCursorAt(t, m, "response.inference.toolCalls[1]")
}

// A Responses API request names no tool on its messages: the cursor stays at the top.
func TestToolEnter_NoNesting(t *testing.T) {
	m := aimModel(t, []pipeline.InferenceMessage{{Role: "user", Content: "go"}, {Role: "assistant", Content: "ok"}})
	enterOnTool(t, m)
	if m.pane != paneDetail || m.detailCursorPath != "" {
		t.Errorf("pane %v, cursor %q", m.pane, m.detailCursorPath)
	}
}

func TestToolEnter_SearchKeepsQuery(t *testing.T) {
	m := aimModel(t, callMsgs("t1"))
	m.searchDetails = true
	m.search = map[paneID]string{paneEvents: "ls"}
	enterOnTool(t, m)
	if m.pane != paneDetail || m.search[paneDetail] != "ls" {
		t.Errorf("pane %v, detail search %q", m.pane, m.search[paneDetail])
	}
}
