package tui

import (
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// titleScroll scrolls the TITLE cell of the selected sessions row when the title does not fit,
// so that the operator can read all of it. Only the selected row scrolls, and only while the
// sessions pane shows.
type titleScroll struct {
	id      string // the row that scrolls
	step    int    // ticks since the cursor came to the row; 0 shows the static cell
	gen     uint64 // drops the ticks of an earlier row's chain
	running bool
}

type titleScrollMsg struct{ gen uint64 }

const (
	titleScrollEvery = 200 * time.Millisecond
	titleScrollHold  = 5 // ticks that the cell stays at each end
)

func titleScrollTick(gen uint64) tea.Cmd {
	return tick(titleScrollEvery, func(time.Time) tea.Msg { return titleScrollMsg{gen: gen} })
}

// titleScrollTarget returns the TITLE column, its width after the mark, the mark and the full
// title of the selected row. ok is false when there is nothing to scroll.
func (m *model) titleScrollTarget() (col, w int, mark, title string, ok bool) {
	id := m.selectedSessionID()
	if m.pane != paneSessions || id == "" {
		return
	}
	col = -1
	for i, c := range m.sessionsTbl.Columns() {
		if headerTitle(c) == "TITLE" {
			col, w = i, c.Width
		}
	}
	if col < 0 {
		return
	}
	served := noServedTitle
	for _, s := range m.sessions {
		if s.ID == id {
			// The mark that the static cell has: the short one when the full one leaves no room.
			served, mark = s.Title, titleCellMark(s, w)
			break
		}
	}
	title = m.sessionTitleFor(id, served)
	w -= lipgloss.Width(mark)
	// A path keeps its tail in the static cell (sessionTitleCell). The head is the part that all
	// sessions share, so a scroll to the head hides the part that identifies the row.
	return col, w, mark, title, w > 0 && lipgloss.Width(title) > w && !looksLikePath(title)
}

// scrollWindow returns the part of title that fits in w columns at this step. It holds at the
// head, moves one rune for each step, holds at the tail, and then starts again.
func scrollWindow(title string, w, step int) string {
	r := []rune(title)
	end := 0 // the first offset where the rest fits
	for end < len(r) && lipgloss.Width(string(r[end:])) > w {
		end++
	}
	off := min(max(step%(end+2*titleScrollHold)-titleScrollHold, 0), end)
	return truncRight(string(r[off:]), w)
}

// applyTitleScroll writes the scrolled cell into the selected row. rebuildSessionsTable calls it
// too, so that a poll does not put the static cell back between two ticks.
func (m *model) applyTitleScroll() {
	ts := &m.titleScroll
	if !ts.running || ts.step == 0 || ts.id != m.selectedSessionID() {
		return
	}
	col, w, mark, title, ok := m.titleScrollTarget()
	if !ok {
		return
	}
	rows := m.sessionsTbl.Rows()
	c := m.sessionsTbl.Cursor()
	row := slices.Clone(rows[c])
	row[col] = mark + scrollWindow(title, w, ts.step)
	// In place, not through SetRows: SetRows drops the search's full rows.
	rows[c] = row
	m.sessionsTbl.UpdateViewport()
}

// armTitleScroll runs after each message. It resets the scroll when the selected row changes,
// and starts the tick chain when the new row's title does not fit.
func (m *model) armTitleScroll() tea.Cmd {
	ts := &m.titleScroll
	id := ""
	if m.pane == paneSessions {
		id = m.selectedSessionID()
	}
	if id != ts.id {
		scrolled := ts.step > 0
		*ts = titleScroll{id: id, gen: ts.gen + 1}
		if scrolled {
			m.rebuildSessionsTable() // puts the static cell back on the row that the cursor left
		}
	}
	if ts.running || id == "" {
		return nil
	}
	if _, _, _, _, ok := m.titleScrollTarget(); !ok {
		return nil
	}
	ts.running = true
	return titleScrollTick(ts.gen)
}

func (m *model) handleTitleScroll(msg titleScrollMsg) tea.Cmd {
	ts := &m.titleScroll
	if msg.gen != ts.gen || !ts.running {
		return nil
	}
	if _, _, _, _, ok := m.titleScrollTarget(); !ok || ts.id != m.selectedSessionID() {
		ts.running = false // armTitleScroll starts a new chain if a title needs one
		return nil
	}
	ts.step++
	m.applyTitleScroll()
	return titleScrollTick(msg.gen)
}
