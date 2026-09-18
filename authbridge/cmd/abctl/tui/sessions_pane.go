package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// sessionsColumns is the table's full-width column set, before any terminal-fitting.
//
// A function rather than a package var so layout() can re-fit from the originals on every
// resize: fitting the LIVE columns would be cumulative, and a terminal that got narrower once
// would keep its narrowed columns after being widened again.
//
// These widths sum to 116 rendered columns (104 declared plus bubbles' two per cell), which is
// why they are fitted rather than used as-is — see fitTableColumns.
//
// TITLE is the widest thing here after the id, and fitTableColumns shrinks the widest column
// rather than dropping any, so on a narrow terminal the two of them pay for each other:
// measured, an 80-column terminal lands on ID=14 TITLE=14, which truncates a UUID to
// "4d0d159b-3c04…". That is the accepted cost of naming the rows — a 14-character id prefix
// still distinguishes them in practice, and an unnamed session is the thing this column exists
// to fix. At 100 columns and up the id is 24 or wider.
//
// A terminal WIDER than 116 is handled by sessionsColumnsFor rather than here: fitTableColumns
// only ever shrinks, so these declared widths are simultaneously the narrow-terminal budget and
// the wide-terminal ceiling, and titles are mostly directory paths with nothing to gain from a
// ceiling of 24 when there are 80 spare columns on the screen.
func sessionsColumns() []table.Column {
	return []table.Column{
		{Title: "ID", Width: 40},
		// After ID, never before it: rebuildSessionsTable and selectedSessionID both read
		// row[0] as the session id, so a column at index 0 would silently make the cursor
		// restore and every Enter act on a title instead.
		{Title: "TITLE", Width: sessionsTitleWidth},
		{Title: "UPDATED", Width: 14},
		{Title: "EVENTS", Width: 8},
		{Title: "TOKENS", Width: 10},
		{Title: "ACTIVE", Width: 8},
	}
}

// sessionsTitleWidth is TITLE's width at the declared layout, and its floor on any terminal
// wide enough not to be squeezed.
const sessionsTitleWidth = 24

// sessionsColumnsFor returns the sessions columns laid out for a terminal termWidth wide.
//
// fitTableColumns only shrinks, which is the right rule for every other table here — their
// columns hold bounded things (a count, a status, a plugin name) that gain nothing from extra
// room. TITLE is the exception: it holds a title that is usually a directory path, and
// measured on this machine 107 of 109 of them are longer than 24 characters. So the declared
// 24 was acting as a ceiling on a 200-column terminal with 84 columns going spare.
//
// Only TITLE grows, and only into slack that already exists — the id keeps its own 40, so
// widening never costs another column anything. Left at 24 when there is no slack, which
// hands the narrow case straight back to fitTableColumns unchanged.
func sessionsColumnsFor(termWidth int) []table.Column {
	cols := sessionsColumns()
	if slack := termWidth - tableWidth(cols); slack > 0 {
		for i := range cols {
			if cols[i].Title == "TITLE" {
				cols[i].Width += slack
				break
			}
		}
	}
	return fitTableColumns(cols, termWidth)
}

// newSessionsTable builds an empty sessions table. Columns are fitted to the terminal by
// layout(), which is called on every WindowSizeMsg.
func newSessionsTable() table.Model {
	t := table.New(
		table.WithColumns(sessionsColumns()),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// rebuildSessionsTable updates the rows from m.sessions, applies the current
// filter, and keeps the cursor on the previously-selected session if still
// present.
func (m *model) rebuildSessionsTable() {
	prev := ""
	if rows := m.sessionsTbl.Rows(); len(rows) > 0 {
		prev = rows[m.sessionsTbl.Cursor()][0]
	}
	now := time.Now()
	rows := make([]table.Row, 0, len(m.sessions))
	for _, s := range m.sessions {
		if m.filter != "" && !strings.Contains(s.ID, m.filter) {
			continue
		}
		active := ""
		if s.Active {
			active = "●"
		}
		rows = append(rows, table.Row{
			s.ID,
			m.sessionTitleCell(s.ID),
			relTime(now, s.UpdatedAt),
			// The server's count, and only ever the server's: it is the complete one.
			// abctl's own cache holds what it snapshotted plus what it has streamed
			// since attaching, which for a session older than the connection is a
			// smaller number — and when handleStreamEvent also wrote this field, the
			// cell flipped between the two on live traffic. The cached-only rows below
			// use len(cached) because the server does not list those at all.
			fmt.Sprintf("%d", s.EventCount),
			sessionTokens(s.TotalTokens, m.events[s.ID]),
			active,
		})
	}
	// Sessions whose events abctl still holds but the server no longer lists.
	// Retaining the events (#870) is only half a fix if there is no row to
	// select them from: after a proxy restart the server lists nothing, so
	// without this the picker is empty and the retained history is unreachable.
	for _, id := range m.cachedOnlySessionIDs() {
		if m.filter != "" && !strings.Contains(id, m.filter) {
			continue
		}
		cached := m.events[id]
		rows = append(rows, table.Row{
			id,
			m.sessionTitleCell(id),
			"—",
			fmt.Sprintf("%d", len(cached)),
			sessionTokens(0, cached),
			"cached",
		})
	}
	m.sessionsTbl.SetRows(rows)

	// Restore cursor position if possible. Through setCursorVisible: a restored row
	// past the first screenful would otherwise land one line below the rendered
	// window, leaving the pane with no highlight — see setCursorVisible.
	if prev != "" {
		for i, r := range rows {
			if r[0] == prev {
				setCursorVisible(&m.sessionsTbl, i)
				return
			}
		}
	}
	setCursorVisible(&m.sessionsTbl, 0)
}

// cachedOnlySessionIDs lists sessions abctl has events for that the server's
// current list omits, sorted so the picker does not reshuffle under the cursor
// on each refresh. Empty caches are skipped: a row advertising zero events
// helps nobody, and snapshotLoadedMsg can create the key with an empty slice.
func (m *model) cachedOnlySessionIDs() []string {
	live := make(map[string]bool, len(m.sessions))
	for _, s := range m.sessions {
		live[s.ID] = true
	}
	out := make([]string, 0, len(m.events))
	for id, evs := range m.events {
		if live[id] || len(evs) == 0 {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// sessionTitle names a session from the harvested metadata, or "" when nothing names it.
//
// Nil-safe by construction rather than by a guard: a read on a nil map yields the zero
// SessionMetadata, so a session nobody harvested, an id not in the file, and the file being
// absent altogether all render the same empty cell. That is the right answer for all three —
// none of them is a fact about the session, only about whether anyone has run the harvester.
func (m *model) sessionTitle(id string) string {
	return m.sessionsData[id].Title
}

// sessionTitleCell is sessionTitle fitted to the TITLE column, truncated from the LEFT.
//
// Truncating from the left because the titles are mostly paths. bubbles truncates every cell
// from the right, which on "/Users/snible/src/cortex/.worktrees/claudesessions" keeps
// "/Users/snible/src/cor…" — the half every session on the machine shares, and none of the
// half that says which one this is. Keeping the tail instead gives "…es/claudesessions".
//
// Pre-truncated here rather than left to bubbles because bubbles offers no choice of side, so
// the cell has to arrive already short enough. That means reading the LIVE column width, not
// the declared one: layout() fits the columns and this reads back what it decided, which is
// why layout() must also rebuild these rows — see its call to rebuildSessionsTable.
//
// Left alone when it is not a path: a title is prose, and prose reads from the left.
func (m *model) sessionTitleCell(id string) string {
	title := m.sessionTitle(id)
	if title == "" || !strings.HasPrefix(title, "/") {
		return title
	}
	for _, c := range m.sessionsTbl.Columns() {
		if c.Title == "TITLE" {
			return truncLeft(title, c.Width)
		}
	}
	return title
}

// truncLeft clips s to n runes keeping the RIGHT end, marking the cut with a leading ellipsis.
//
// The mirror of trunc, for values whose distinguishing end is the last one: a path, where every
// sibling shares the prefix. n < 1 yields "" and n == 1 yields just the ellipsis, so the result
// never exceeds the cell it was measured for.
func truncLeft(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return "…" + string(r[len(r)-(n-1):])
}

// relTime renders "Ns", "Nm", "Nh" for small deltas; absolute time otherwise.
func relTime(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2 15:04")
	}
}

// sessionTokens reports the total tokens for a session, compactly: 137,156,234 renders
// as "137.2M".
//
// Grouped digits overflowed the 10-column cell at 100M and were truncated to
// "137,156,2…", which is worse than a rounded figure in every way — it is longer, less
// readable, and its last digits are the ones that got cut. A session total is a
// magnitude, not something anyone reconciles: the exact per-event counts are in the
// events pane, and the exact total is one curl away on /v1/sessions.
//
// Prefers the server-computed count from SessionSummary (authoritative, covers the
// full event backlog even before we've streamed anything for this
// session). Falls back to a client-side sum over the cached events when
// the server returned zero (older authbridge server without token
// aggregation). Returns "—" when neither source has data.
func sessionTokens(serverTotal int, cached []pipeline.SessionEvent) string {
	if serverTotal > 0 {
		return formatCompact(float64(serverTotal))
	}
	var total int
	for i := range cached {
		if cached[i].Phase != pipeline.SessionResponse {
			continue
		}
		if cached[i].Inference == nil {
			continue
		}
		total += cached[i].Inference.TotalTokens
	}
	if total == 0 {
		return "—"
	}
	return formatCompact(float64(total))
}

// selectedSessionID returns the cursor row's session ID, or "".
func (m *model) selectedSessionID() string {
	rows := m.sessionsTbl.Rows()
	if len(rows) == 0 {
		return ""
	}
	return rows[m.sessionsTbl.Cursor()][0]
}
