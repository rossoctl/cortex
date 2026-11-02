package tui

import (
	"fmt"
	"slices"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// `/` SEARCHES WHAT A PANE SHOWS, ON EVERY PANE (#1339, and the report that followed it): every
// row stays on screen, the matched characters are highlighted, and the cursor jumps between
// them. It replaced a filter that hid every row it did not match, and that filter's matching
// outlived it here for a while: each searchable pane had its own matcher, written apart from
// the code that drew the pane, over a hand-kept list of fields. The events pane's read hidden
// fields and not PHASE, so `/req` marked rows for a "request" buried in a plugin's details and
// skipped the rows whose PHASE said req. Every other pane had no matcher, and so no search.
//
// So a pane no longer says what matches; it says what it draws. Tables hand the table copy
// each row's full cell values, built by the same cell functions that draw them (see
// rowBuilder), and the table matches and highlights. Text views and usage hand a searchDoc
// their lines. This file is what is common: the prompt, each target's query, where `/` was
// pressed, Esc, n and N, and the footer's count. It reaches a pane only through
// searchSurface.
//
// A search is neither saved nor shared. Each pane keeps its own, as does the help overlay
// (targetHelp). The events pane's and message detail's are about one session, so opening
// another session's drops both (see the drill-in in keys.go); message detail keeps its query
// from one message to the next, so the operator can look for one string message by message.

// targetHelp is the help overlay's search, kept beside the panes'. Outside 0..lastPaneID, as
// paneNone is, so nothing that walks the panes takes it for one.
const targetHelp paneID = -2

// newSearchInput is the `/` prompt both constructors build.
func newSearchInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "/ "
	ti.Placeholder = "search…"
	return ti
}

// searchSpot is a place in a pane: a row number, or a scroll offset, plus the identity a row
// had where rows move under a held position. The sessions list re-sorts every second and the
// events pane evicts its oldest rows, so a bare row number would put Esc somewhere other than
// where `/` was pressed.
//
// tail is an events cursor that was following new events: on the newest row of a
// chronological table, the predicate rebuildEventsTable tail-follows on. That cursor's place
// is "the newest row" rather than the event that was newest at `/` — events keep streaming
// while the prompt is open, and putting it back on that event would stop it following them.
//
// path is the event detail pane's cursor node.
type searchSpot struct {
	row     int
	sess    string
	event   eventKey
	tail    bool
	yOffset int
	path    string
}

// searchSurface is how the search reaches a pane. A line is a table row, or a drawn line of
// a text view; row numbers in, row numbers out.
type searchSurface interface {
	// matchLines is the lines the last redraw matched, ascending.
	matchLines() []int
	// spot is where the pane is now: its cursor, or its current match and scroll offset.
	spot() searchSpot
	// lineOf is the line a spot is on after a redraw.
	lineOf(searchSpot) int
	// reveal puts the cursor on line i, or scrolls a text view to it.
	reveal(i int)
	// restore puts the pane back where a spot was.
	restore(searchSpot)
	// redraw rebuilds the pane against its current query.
	redraw()
}

// searchTarget is what `/`, n and N act on: the help overlay while it is up, else the pane.
func (m *model) searchTarget() paneID {
	if m.helpVisible {
		return targetHelp
	}
	return m.pane
}

// searchQuery is the query target t is matched against: what is in the prompt while it is
// open on t, which is what makes the search incremental, and the committed one otherwise.
func (m *model) searchQuery(t paneID) string {
	if m.searching && t == m.searchPane {
		return m.searchInput.Value()
	}
	return m.search[t]
}

// openSearch is `/` on target t. The prompt opens empty, and where t is now is remembered:
// it is the place typing searches from, and the place Esc returns to.
func (m *model) openSearch(t paneID) {
	s := m.surface(t)
	if s == nil {
		return
	}
	m.searching = true
	m.searchPane = t
	m.searchOrigin = s.spot()
	m.searchInput.SetValue("")
	m.searchInput.Focus()
	if t == targetHelp {
		// Drawn on the overlay's bottom line, so sized to it; layout() puts the width back
		// for a pane's prompt the next time one opens.
		m.searchInput.Width = max(m.helpVp.Width-4, 1)
	}
	m.searchLayout()
}

// redrawSearches redraws every target against its query, for a change to more than one of
// them at once.
func (m *model) redrawSearches() {
	for t := paneID(0); t <= lastPaneID; t++ {
		if s := m.surface(t); s != nil {
			s.redraw()
		}
	}
	if s := m.surface(targetHelp); s != nil {
		s.redraw()
	}
}

// closeSearch closes the prompt; the target is redrawn for the committed search.
func (m *model) closeSearch() {
	m.searching = false
	m.searchInput.Blur()
	m.searchLayout()
}

// searchLayout lays the screen out again for the prompt opening or closing — it takes a body
// line — and redraws the target, so its highlights are for the query now in force. The help
// overlay draws its prompt on its own bottom line, so the body does not change for it.
func (m *model) searchLayout() {
	if m.searchPane != targetHelp {
		m.layout()
	}
	if s := m.surface(m.searchPane); s != nil {
		s.redraw()
	}
}

// searchKey handles a key while the prompt is open. Enter commits the query, and an empty one
// clears the search. Esc puts the place and the search back as they were at `/`. Anything
// else edits the query, and the target moves to the first match at or after where `/` was
// pressed, wrapping past the end — or back there when nothing matches.
func (m *model) searchKey(msg tea.KeyMsg) tea.Cmd {
	t := m.searchPane
	switch msg.String() {
	case "enter":
		if m.search == nil {
			m.search = make(map[paneID]string)
		}
		m.search[t] = m.searchInput.Value()
		m.closeSearch()
		return nil
	case "esc":
		m.closeSearch()
		if s := m.surface(t); s != nil {
			s.restore(m.searchOrigin)
		}
		return nil
	}
	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	s := m.surface(t)
	if s == nil {
		return cmd
	}
	s.redraw()
	if m.searchInput.Value() != "" {
		// Counted from where `/` was pressed even when following: what streamed in since is
		// after it, and is searched first.
		if i, ok := nextMatch(s.matchLines(), s.lineOf(m.searchOrigin), true, true); ok {
			s.reveal(i)
			return cmd
		}
	}
	s.restore(m.searchOrigin)
	return cmd
}

// searchStep is n (forward) and N on target t: the next match past where it is, wrapping.
// Nothing happens with no match, which includes having no search.
func (m *model) searchStep(t paneID, forward bool) {
	s := m.surface(t)
	if s == nil {
		return
	}
	if i, ok := nextMatch(s.matchLines(), s.spot().row, forward, false); ok {
		s.reveal(i)
	}
}

// nextMatch is the first of matches (ascending rows) after from going forward, or before it going
// back, wrapping past either end. inclusive lets from itself count.
func nextMatch(matches []int, from int, forward, inclusive bool) (int, bool) {
	if len(matches) == 0 {
		return 0, false
	}
	if forward {
		for _, i := range matches {
			if i > from || (inclusive && i == from) {
				return i, true
			}
		}
		return matches[0], true
	}
	for k := len(matches) - 1; k >= 0; k-- {
		if i := matches[k]; i < from || (inclusive && i == from) {
			return i, true
		}
	}
	return matches[len(matches)-1], true
}

// searchStatus is the account of target t's search: which match it is on, how many there
// are when it is on none, or that there are none. Live while the prompt is open, so a query
// that matches nothing says so as it is typed. Empty without a search.
func (m *model) searchStatus(t paneID) string {
	s, q := m.surface(t), m.searchQuery(t)
	if s == nil || q == "" {
		return ""
	}
	matches := s.matchLines()
	if len(matches) == 0 {
		return fmt.Sprintf("[/%s: no match]", q)
	}
	if k, ok := slices.BinarySearch(matches, s.spot().row); ok {
		return fmt.Sprintf("[/%s %d/%d]", q, k+1, len(matches))
	}
	if len(matches) == 1 {
		return fmt.Sprintf("[/%s 1 match]", q)
	}
	return fmt.Sprintf("[/%s %d matches]", q, len(matches))
}

// searchHints is a hint line's `/`, with n/N beside it only while t has a committed search:
// they do nothing without one, and hint lines are short of room.
func (m *model) searchHints(t paneID) string {
	if m.search[t] != "" {
		return "[/] search  [n/N] next/prev"
	}
	return "[/] search"
}
