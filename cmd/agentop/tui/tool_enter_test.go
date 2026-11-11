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
