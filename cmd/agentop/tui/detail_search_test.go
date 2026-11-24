package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
)

// detailEvents is an events pane whose rows are responses with long completions, so their
// detail is long enough to scroll. needle sits near the end of row 0's.
func newDetailSearchModel(t *testing.T) *model {
	t.Helper()
	m := newSearchEventsModel(t, "r0.test", "r1.test")
	for i := range m.events["s"] {
		e := &m.events["s"][i]
		e.Phase = pipeline.SessionResponse
		e.Inference = &pipeline.InferenceExtension{Model: "m",
			Completion: strings.Repeat("filler words here ", 2000) + "needle-" + e.Host + " " + strings.Repeat("filler words here ", 1000)}
	}
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 0)
	m.selectedEventKey = keyOf(m.selectedEvent())
	press(m, tea.KeyEnter)
	if m.pane != paneDetail {
		t.Fatalf("setup: Enter left pane %v, want detail", m.pane)
	}
	return m
}

func detailScreen(m *model) string { return stripANSI(m.detailVp.View()) }

func TestSearch_InMessageDetail(t *testing.T) {
	m := newDetailSearchModel(t)
	typeKeys(m, "/needle-r0")
	if !m.searching || m.searchPane != paneDetail {
		t.Fatalf("searching=%v pane=%v, want the prompt open on detail", m.searching, m.searchPane)
	}
	if !strings.Contains(detailScreen(m), "needle-r0") {
		t.Errorf("the match is not on screen:\n%s", detailScreen(m))
	}
	press(m, tea.KeyEnter)
	if st := stripANSI(m.footerView()); !strings.Contains(st, "[/needle-r0 1/1]") {
		t.Errorf("status row = %q, want [/needle-r0 1/1]", st)
	}
}

// TestSearch_DetailRevealsAnOffScreenMatchAThirdDown, and leaves one already on screen.
func TestSearch_DetailRevealsAnOffScreenMatchAThirdDown(t *testing.T) {
	m := newDetailSearchModel(t)
	typeKeys(m, "/needle-r0")
	press(m, tea.KeyEnter)
	row := m.detailDoc.matches[0]
	// Typing reveals the match too, but each keystroke may already have scrolled to a partial
	// query's match nearby. Scroll away, so n has an off-screen match to reveal.
	m.detailVp.GotoTop()
	m.Update(keyRune('n'))
	// A third of the way down, clamped as the viewport clamps: no further than its last screen.
	want := min(max(row-m.detailVp.Height/3, 0), m.detailVp.TotalLineCount()-m.detailVp.Height)
	if m.detailVp.YOffset != want {
		t.Errorf("YOffset = %d, want %d (match on row %d, height %d, %d lines)",
			m.detailVp.YOffset, want, row, m.detailVp.Height, m.detailVp.TotalLineCount())
	}
	before := m.detailVp.YOffset
	m.Update(keyRune('n')) // the only match: already on screen
	if m.detailVp.YOffset != before {
		t.Errorf("n to a match on screen scrolled from %d to %d", before, m.detailVp.YOffset)
	}
}

// TestSearch_DetailQueryStaysWithTheSession: kept from one message to the next, dropped when
// another session's events are opened — driven through the drill-in, as
// TestSearch_EventsSearchStaysWithItsSession drives the events search.
func TestSearch_DetailQueryStaysWithTheSession(t *testing.T) {
	m := newSearchSessionsModel(t, searchIDs...)
	long := func(host string) []pipeline.SessionEvent {
		evs := hostEvents(host+"-0.test", host+"-1.test")
		for i := range evs {
			evs[i].Phase = pipeline.SessionResponse
			evs[i].Inference = &pipeline.InferenceExtension{Model: "m", Completion: "a needle here"}
		}
		return evs
	}
	m.events["x-alpha"], m.events["y-beta"] = long("xa"), long("yb")

	press(m, tea.KeyEnter) // x-alpha's events
	press(m, tea.KeyEnter) // a message
	typeKeys(m, "/needle")
	press(m, tea.KeyEnter)
	press(m, tea.KeyEsc) // back to events
	m.Update(keyRune('k'))
	press(m, tea.KeyEnter) // the other message
	if m.pane != paneDetail || m.searchQuery(paneDetail) != "needle" {
		t.Fatalf("the next message: pane %v, query %q, want detail with needle", m.pane, m.searchQuery(paneDetail))
	}
	if len(m.detailDoc.matches) == 0 {
		t.Error("the next message opened with no match highlighted")
	}

	press(m, tea.KeyEsc) // events
	press(m, tea.KeyEsc) // sessions
	m.Update(keyRune('j'))
	press(m, tea.KeyEnter) // y-beta's events
	if m.selectedSess != "y-beta" {
		t.Fatalf("setup: entered %q, want y-beta", m.selectedSess)
	}
	press(m, tea.KeyEnter)
	if got := m.searchQuery(paneDetail); got != "" {
		t.Errorf("another session's message opened with the detail search %q", got)
	}
}

// TestSearch_DetailResizeWithACommittedSearch: the highlights are redrawn at the new wrap,
// the current row is dropped with the old one, and the offset stays in range.
func TestSearch_DetailResizeWithACommittedSearch(t *testing.T) {
	m := newDetailSearchModel(t)
	typeKeys(m, "/filler")
	press(m, tea.KeyEnter)
	m.Update(keyRune('n'))
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	if _, ok := m.detailDoc.currentRow(); ok {
		t.Error("the current row survived a re-wrap")
	}
	if len(m.detailDoc.matches) == 0 {
		t.Fatal("no matches after the resize")
	}
	if max := m.detailVp.TotalLineCount() - m.detailVp.Height; m.detailVp.YOffset > max && max >= 0 {
		t.Errorf("YOffset %d past the bottom (%d)", m.detailVp.YOffset, max)
	}
}

// TestSearch_DetailBodyArrivesUnderTheSearch: the detail opens from the summary and fills in
// when the full event arrives; the search follows the new text.
func TestSearch_DetailBodyArrivesUnderTheSearch(t *testing.T) {
	m := newDetailSearchModel(t)
	typeKeys(m, "/late-arrival")
	if n := len(m.detailDoc.matches); n != 0 {
		t.Fatalf("setup: %d matches before the body arrived", n)
	}
	full := *m.detailRow.event
	inf := *full.Inference
	inf.Completion += " late-arrival"
	full.Inference = &inf
	r := m.detailRow
	r.event = &full
	m.showDetail(r, false)
	if n := len(m.detailDoc.matches); n != 1 {
		t.Errorf("after the body arrived: %d matches, want 1", n)
	}
}

func TestSearch_InPluginDetail(t *testing.T) {
	m := newPanesModel(t)
	m.pane = panePipeline
	press(m, tea.KeyEnter) // jwt-validation's detail
	if m.pane != panePluginDetail {
		t.Fatalf("setup: Enter left pane %v, want plugin detail", m.pane)
	}
	typeKeys(m, "/jwt-validation")
	press(m, tea.KeyEnter)
	if len(m.detailDoc.matches) == 0 {
		t.Error("plugin detail: no match for the plugin's own name")
	}
}
