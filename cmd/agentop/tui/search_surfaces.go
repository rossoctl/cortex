package tui

import (
	"slices"

	"github.com/charmbracelet/bubbles/viewport"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// surface is target t's search, or nil for a target with nothing to search.
func (m *model) surface(t paneID) searchSurface {
	switch t {
	case paneSessions:
		return sessionsSurface{m}
	case paneEvents:
		return eventsSurface{m}
	case panePipeline:
		return tableSurface{m, t, &m.pipelineTbl}
	case paneCatalog:
		return tableSurface{m, t, &m.catalogTbl}
	case paneAgents:
		return tableSurface{m, t, &m.agentsTbl}
	case paneNamespaces:
		return tableSurface{m, t, &m.namespacesTbl}
	case panePods:
		return tableSurface{m, t, &m.podsTbl}
	case paneDetail, panePluginDetail:
		return docSurface{m, t, &m.detailVp, &m.detailDoc}
	case targetHelp:
		return docSurface{m, t, &m.helpVp, &m.helpDoc}
	case paneUsage:
		return usageSurface{m}
	case paneTimeline:
		return timelineSurface{m}
	}
	return nil
}

// timelineSurface is the timeline pane, drawn to fit like usage: its place is only which
// matching line is current.
type timelineSurface struct{ m *model }

func (s timelineSurface) matchLines() []int { return s.m.timelineDoc.matches }

func (s timelineSurface) spot() searchSpot {
	r, _ := s.m.timelineDoc.currentRow()
	return searchSpot{row: r}
}

func (s timelineSurface) lineOf(sp searchSpot) int { return sp.row }
func (s timelineSurface) reveal(i int)             { s.m.timelineDoc.setCurrent(i); s.redraw() }
func (s timelineSurface) restore(searchSpot)       { s.m.timelineDoc.clearCurrent(); s.redraw() }
func (s timelineSurface) redraw()                  { s.m.timelineBody() }

// tableSurface is a table pane whose rows already hold their whole values: the table matches
// the rows as they are, and the cursor is the place.
type tableSurface struct {
	m   *model
	t   paneID
	tbl *table.Model
}

func (s tableSurface) matchLines() []int        { return s.tbl.Matches() }
func (s tableSurface) spot() searchSpot         { return searchSpot{row: s.tbl.Cursor()} }
func (s tableSurface) lineOf(sp searchSpot) int { return sp.row }
func (s tableSurface) reveal(i int)             { setCursorVisible(s.tbl, i) }
func (s tableSurface) restore(sp searchSpot)    { setCursorVisible(s.tbl, sp.row) }
func (s tableSurface) redraw()                  { s.tbl.SetSearch(s.m.searchQuery(s.t), nil) }

// sessionsSurface holds its place by session id: the list re-sorts under the cursor.
type sessionsSurface struct{ m *model }

func (s sessionsSurface) matchLines() []int { return s.m.sessionsTbl.Matches() }

func (s sessionsSurface) spot() searchSpot {
	c := s.m.sessionsTbl.Cursor()
	sp := searchSpot{row: c}
	if c >= 0 && c < len(s.m.sessionRowIDs) {
		sp.sess = s.m.sessionRowIDs[c]
	}
	return sp
}

func (s sessionsSurface) lineOf(sp searchSpot) int {
	if sp.sess != "" {
		if i := slices.Index(s.m.sessionRowIDs, sp.sess); i >= 0 {
			return i
		}
	}
	return sp.row
}

func (s sessionsSurface) reveal(i int)          { setCursorVisible(&s.m.sessionsTbl, i) }
func (s sessionsSurface) restore(sp searchSpot) { s.reveal(s.lineOf(sp)) }
func (s sessionsSurface) redraw()               { s.m.rebuildSessionsTable() }

// eventsSurface holds its place by event key, and by "the newest row" while following.
type eventsSurface struct{ m *model }

func (s eventsSurface) matchLines() []int { return s.m.eventsTbl.Matches() }

func (s eventsSurface) spot() searchSpot {
	c := s.m.eventsTbl.Cursor()
	return searchSpot{
		row: c, event: keyOf(s.m.selectedEvent()),
		tail: s.m.sortCol == "" && c >= len(s.m.eventsTbl.Rows())-1,
	}
}

func (s eventsSurface) lineOf(sp searchSpot) int {
	if sp.event != (eventKey{}) {
		if i := findByKey(s.m.visibleRows, sp.event); i >= 0 {
			return i
		}
	}
	return sp.row
}

// reveal pins the event as well, as every other cursor move here does, so the next rebuild
// keeps the cursor on it — through an eviction or a re-sort — instead of on its row number.
func (s eventsSurface) reveal(i int) {
	setCursorVisible(&s.m.eventsTbl, i)
	s.m.selectedEventKey = keyOf(s.m.selectedEvent())
}

// restore puts a following cursor back on the newest row, so it goes on following.
func (s eventsSurface) restore(sp searchSpot) {
	if sp.tail {
		s.reveal(len(s.m.visibleRows) - 1)
		return
	}
	s.reveal(s.lineOf(sp))
}

func (s eventsSurface) redraw() { s.m.rebuildEventsTable() }

// docSurface is a scrolling text view: its place is the current match when that is on
// screen, else the top of the view.
type docSurface struct {
	m   *model
	t   paneID
	vp  *viewport.Model
	doc *searchDoc
}

func (s docSurface) matchLines() []int { return s.doc.matches }

func (s docSurface) spot() searchSpot {
	sp := searchSpot{row: s.vp.YOffset, yOffset: s.vp.YOffset}
	if c, ok := s.doc.currentRow(); ok && c >= s.vp.YOffset && c < s.vp.YOffset+s.vp.Height {
		sp.row = c
	}
	return sp
}

func (s docSurface) lineOf(sp searchSpot) int { return sp.row }

// reveal makes row current and scrolls to it only when it is off screen, putting it a third
// of the way down so what follows the match is in view. SetYOffset clamps.
func (s docSurface) reveal(row int) {
	s.doc.setCurrent(row)
	s.redraw()
	if row < s.vp.YOffset || row >= s.vp.YOffset+s.vp.Height {
		s.vp.SetYOffset(row - s.vp.Height/3)
	}
}

func (s docSurface) restore(sp searchSpot) {
	s.doc.clearCurrent()
	s.redraw()
	s.vp.SetYOffset(sp.yOffset)
}

func (s docSurface) redraw() { s.vp.SetContent(s.doc.render(s.m.searchQuery(s.t))) }

// usageSurface is the usage pane: drawn to fit, so it never scrolls, and its place is only
// which matching line is current.
type usageSurface struct{ m *model }

func (s usageSurface) matchLines() []int { return s.m.usageDoc.matches }

func (s usageSurface) spot() searchSpot {
	r, _ := s.m.usageDoc.currentRow()
	return searchSpot{row: r}
}

func (s usageSurface) lineOf(sp searchSpot) int { return sp.row }
func (s usageSurface) reveal(i int)             { s.m.usageDoc.setCurrent(i); s.redraw() }
func (s usageSurface) restore(searchSpot)       { s.m.usageDoc.clearCurrent(); s.redraw() }
func (s usageSurface) redraw()                  { s.m.usageBody() }
