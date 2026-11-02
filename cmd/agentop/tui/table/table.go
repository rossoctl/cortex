// Package table provides a simple table component for Bubble Tea applications.
//
// A copy of github.com/charmbracelet/bubbles v1.0.0 table/table.go (MIT; see LICENSE in
// this directory), kept because its scrolling cannot be fixed from outside the package.
// Everything but the scrolling and marked rows is the upstream code unchanged, so the
// upstream API still works and a caller switches by import path alone.
//
// WHAT CHANGED: the screen. Upstream renders a window of up to twice the height around the
// cursor — start = cursor − height — and shows `height` lines of it from a viewport offset
// that MoveUp and MoveDown nudge by hand. The two only stay in step while start moves with
// the cursor, and it stops at 0. So on a table between one and two screens long, arrowing
// down after scrolling back up from the tail lowered the offset while start stayed put:
// each press scrolled the rows down a line and moved the highlight down two. SetCursor and
// SetHeight never touched the offset at all, which left the highlight off screen. Bubbles'
// main branch (v2) has the same arithmetic.
//
// Here start IS the first row on screen. UpdateViewport keeps it where it is unless that
// would leave the cursor off screen, and then moves it only as far as it takes to bring the
// cursor back. Every way of moving the cursor goes through UpdateViewport, so every one
// keeps the cursor on screen and scrolls only when the cursor crosses an edge.
//
// WHAT WAS ADDED: search (SetSearch, Matches, Styles.Match, CurrentMatch and Marked), which
// agentop's `/` is drawn with. Cells are plain text and are truncated before they are
// styled, so a highlight is applied to the truncated text — and a match the truncation cut
// off is shown by highlighting the "…" that replaced it. Every piece of a row carries the
// row's style itself, rather than the row being wrapped in it once: a highlight ends in a
// reset, and a reset inside a wrapped row ends the row's style with it.
package table

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"

	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
)

// Model defines a state for the table widget.
type Model struct {
	KeyMap KeyMap
	Help   help.Model

	cols   []Column
	rows   []Row
	cursor int
	focus  bool
	styles Styles

	// query is the search the rows are matched against, full the pane's full cell values
	// for it (nil when the rows hold them already), and hits the rows that matched, by
	// row. Recomputed when the rows or the search change, never on a cursor move.
	query   string
	full    []SearchRow
	matches []int
	hits    []bool

	// The viewport renders exactly the rows on screen and never scrolls: its offset stays
	// 0, and start is the scroll position. Rows [start, end) are what the screen shows.
	viewport viewport.Model
	start    int
	end      int
}

// Row represents one line in the table.
type Row []string

// SearchRow is one row's full text for a search. Cells is parallel to the row's cells: an
// entry is the cell's value as it would read with unlimited room, for a cell the pane cut
// before handing it over, and "" for a cell that is already whole. Dropped is the full
// values of columns the pane had no room to show.
type SearchRow struct {
	Cells   []string
	Dropped []string
}

// Column defines the table structure.
type Column struct {
	Title string
	Width int
}

// KeyMap defines keybindings. It satisfies to the help.KeyMap interface, which
// is used to render the help menu.
type KeyMap struct {
	LineUp       key.Binding
	LineDown     key.Binding
	PageUp       key.Binding
	PageDown     key.Binding
	HalfPageUp   key.Binding
	HalfPageDown key.Binding
	GotoTop      key.Binding
	GotoBottom   key.Binding
}

// ShortHelp implements the KeyMap interface.
func (km KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{km.LineUp, km.LineDown}
}

// FullHelp implements the KeyMap interface.
func (km KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{km.LineUp, km.LineDown, km.GotoTop, km.GotoBottom},
		{km.PageUp, km.PageDown, km.HalfPageUp, km.HalfPageDown},
	}
}

// DefaultKeyMap returns a default set of keybindings.
func DefaultKeyMap() KeyMap {
	const spacebar = " "
	return KeyMap{
		LineUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		LineDown: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("b", "pgup"),
			key.WithHelp("b/pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("f", "pgdown", spacebar),
			key.WithHelp("f/pgdn", "page down"),
		),
		HalfPageUp: key.NewBinding(
			key.WithKeys("u", "ctrl+u"),
			key.WithHelp("u", "½ page up"),
		),
		HalfPageDown: key.NewBinding(
			key.WithKeys("d", "ctrl+d"),
			key.WithHelp("d", "½ page down"),
		),
		GotoTop: key.NewBinding(
			key.WithKeys("home", "g"),
			key.WithHelp("g/home", "go to start"),
		),
		GotoBottom: key.NewBinding(
			key.WithKeys("end", "G"),
			key.WithHelp("G/end", "go to end"),
		),
	}
}

// Styles contains style definitions for this list component. By default, these
// values are generated by DefaultStyles.
type Styles struct {
	Header   lipgloss.Style
	Cell     lipgloss.Style
	Selected lipgloss.Style
	// Match draws the characters a search matched, CurrentMatch those on the cursor's row.
	// Marked draws a row whose only match is somewhere no cell on screen shows it. Not
	// upstream: see the package doc.
	Match        lipgloss.Style
	CurrentMatch lipgloss.Style
	Marked       lipgloss.Style
}

// DefaultStyles returns a set of default style definitions for this table.
func DefaultStyles() Styles {
	return Styles{
		Selected:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")),
		Match:        lipgloss.NewStyle().Reverse(true),
		CurrentMatch: lipgloss.NewStyle().Reverse(true).Bold(true),
		Header:       lipgloss.NewStyle().Bold(true).Padding(0, 1),
		Cell:         lipgloss.NewStyle().Padding(0, 1),
	}
}

// SetStyles sets the table styles.
func (m *Model) SetStyles(s Styles) {
	m.styles = s
	m.UpdateViewport()
}

// Option is used to set options in New. For example:
//
//	table := New(WithColumns([]Column{{Title: "ID", Width: 10}}))
type Option func(*Model)

// New creates a new model for the table widget.
func New(opts ...Option) Model {
	m := Model{
		cursor:   0,
		viewport: viewport.New(0, 20), //nolint:mnd

		KeyMap: DefaultKeyMap(),
		Help:   help.New(),
		styles: DefaultStyles(),
	}

	for _, opt := range opts {
		opt(&m)
	}

	m.UpdateViewport()

	return m
}

// WithColumns sets the table columns (headers).
func WithColumns(cols []Column) Option {
	return func(m *Model) {
		m.cols = cols
	}
}

// WithRows sets the table rows (data).
func WithRows(rows []Row) Option {
	return func(m *Model) {
		m.rows = rows
	}
}

// WithHeight sets the height of the table.
func WithHeight(h int) Option {
	return func(m *Model) {
		m.viewport.Height = h - lipgloss.Height(m.headersView())
	}
}

// WithWidth sets the width of the table.
func WithWidth(w int) Option {
	return func(m *Model) {
		m.viewport.Width = w
	}
}

// WithFocused sets the focus state of the table.
func WithFocused(f bool) Option {
	return func(m *Model) {
		m.focus = f
	}
}

// WithStyles sets the table styles.
func WithStyles(s Styles) Option {
	return func(m *Model) {
		m.styles = s
	}
}

// WithKeyMap sets the key map.
func WithKeyMap(km KeyMap) Option {
	return func(m *Model) {
		m.KeyMap = km
	}
}

// Update is the Bubble Tea update loop.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if !m.focus {
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.KeyMap.LineUp):
			m.MoveUp(1)
		case key.Matches(msg, m.KeyMap.LineDown):
			m.MoveDown(1)
		case key.Matches(msg, m.KeyMap.PageUp):
			m.MoveUp(m.viewport.Height)
		case key.Matches(msg, m.KeyMap.PageDown):
			m.MoveDown(m.viewport.Height)
		case key.Matches(msg, m.KeyMap.HalfPageUp):
			m.MoveUp(m.viewport.Height / 2) //nolint:mnd
		case key.Matches(msg, m.KeyMap.HalfPageDown):
			m.MoveDown(m.viewport.Height / 2) //nolint:mnd
		case key.Matches(msg, m.KeyMap.GotoTop):
			m.GotoTop()
		case key.Matches(msg, m.KeyMap.GotoBottom):
			m.GotoBottom()
		}
	}

	return m, nil
}

// Focused returns the focus state of the table.
func (m Model) Focused() bool {
	return m.focus
}

// Focus focuses the table, allowing the user to move around the rows and
// interact.
func (m *Model) Focus() {
	m.focus = true
	m.UpdateViewport()
}

// Blur blurs the table, preventing selection or movement.
func (m *Model) Blur() {
	m.focus = false
	m.UpdateViewport()
}

// View renders the component.
func (m Model) View() string {
	return m.headersView() + "\n" + m.viewport.View()
}

// HelpView is a helper method for rendering the help menu from the keymap.
// Note that this view is not rendered by default and you must call it
// manually in your application, where applicable.
func (m Model) HelpView() string {
	return m.Help.View(m.KeyMap)
}

// UpdateViewport updates the list content based on the previously defined
// columns and rows.
//
// It also settles the scroll position, which is why every cursor move, resize and row
// change ends here. start stays where it is unless the cursor is off screen, and then
// moves just far enough that the cursor is on the nearest edge: the top line if it went
// above the screen, the bottom line if it went below. It never scrolls past the last row,
// so a table that shrank under its screen pulls its rows back down to fill it.
func (m *Model) UpdateViewport() {
	h := max(m.viewport.Height, 0)
	if m.cursor >= 0 {
		m.start = clamp(m.start, m.cursor-h+1, m.cursor)
	}
	m.start = clamp(m.start, 0, max(len(m.rows)-h, 0))
	m.end = min(m.start+h, len(m.rows))

	// Only the rows on screen are rendered: constant runtime, independent of the number
	// of rows in the table.
	renderedRows := make([]string, 0, m.end-m.start)
	for i := m.start; i < m.end; i++ {
		renderedRows = append(renderedRows, m.renderRow(i))
	}

	m.viewport.SetContent(
		lipgloss.JoinVertical(lipgloss.Left, renderedRows...),
	)
}

// SelectedRow returns the selected row.
// You can cast it to your own implementation.
func (m Model) SelectedRow() Row {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil
	}

	return m.rows[m.cursor]
}

// Rows returns the current rows.
func (m Model) Rows() []Row {
	return m.rows
}

// Columns returns the current columns.
func (m Model) Columns() []Column {
	return m.cols
}

// SetRows sets a new rows state. The search is kept and matched against the new rows; full
// values set for the old rows are dropped, since they describe rows that are gone.
func (m *Model) SetRows(r []Row) {
	m.rows = r
	m.full = nil
	m.match()

	if m.cursor > len(m.rows)-1 {
		m.cursor = len(m.rows) - 1
	}

	m.UpdateViewport()
}

// SetSearch matches the rows against q, reading each cell's full value from full where the
// pane supplied one. full is parallel to the rows; one of another length is ignored. An
// empty q clears the search. Not upstream: see the package doc.
func (m *Model) SetSearch(q string, full []SearchRow) {
	m.query = q
	m.full = nil
	if len(full) == len(m.rows) {
		m.full = full
	}
	m.match()
	m.UpdateViewport()
}

// Matches is the rows the search matched, ascending; nil without a search.
func (m Model) Matches() []int { return m.matches }

func (m *Model) match() {
	m.matches, m.hits = nil, nil
	if m.query == "" {
		return
	}
	m.hits = make([]bool, len(m.rows))
	for r := range m.rows {
		if m.rowMatches(r) {
			m.hits[r] = true
			m.matches = append(m.matches, r)
		}
	}
}

// fullCell is cell i of row r as it would read with unlimited room.
func (m *Model) fullCell(r, i int) string {
	if r < len(m.full) && i < len(m.full[r].Cells) && m.full[r].Cells[i] != "" {
		return m.full[r].Cells[i]
	}
	return m.rows[r][i]
}

func (m *Model) rowMatches(r int) bool {
	for i := range m.rows[r] {
		if find.Contains(m.fullCell(r, i), m.query) {
			return true
		}
	}
	if r < len(m.full) {
		for _, v := range m.full[r].Dropped {
			if find.Contains(v, m.query) {
				return true
			}
		}
	}
	return false
}

// cellSpans is where to highlight cell i of row r, drawn as text: each occurrence of the
// query in it, or, when only the full value holds one, the "…" that stands for the rest.
func (m *Model) cellSpans(r, i int, text string) []find.Range {
	if spans := find.Ranges(text, m.query); len(spans) > 0 {
		return spans
	}
	if find.Contains(m.fullCell(r, i), m.query) {
		return find.Ranges(text, "…")
	}
	return nil
}

// paint draws text in style, with each span in match.
func paint(text string, spans []find.Range, style, match lipgloss.Style) string {
	if len(spans) == 0 {
		return style.Render(text)
	}
	var b strings.Builder
	at := 0
	for _, sp := range spans {
		if sp.Start > at {
			b.WriteString(style.Render(ansi.Cut(text, at, sp.Start)))
		}
		b.WriteString(match.Render(ansi.Cut(text, sp.Start, sp.End)))
		at = sp.End
	}
	if w := ansi.StringWidth(text); at < w {
		b.WriteString(style.Render(ansi.Cut(text, at, w)))
	}
	return b.String()
}

// SetColumns sets a new columns state.
func (m *Model) SetColumns(c []Column) {
	m.cols = c
	m.UpdateViewport()
}

// SetWidth sets the width of the viewport of the table.
func (m *Model) SetWidth(w int) {
	m.viewport.Width = w
	m.UpdateViewport()
}

// SetHeight sets the height of the viewport of the table.
func (m *Model) SetHeight(h int) {
	m.viewport.Height = h - lipgloss.Height(m.headersView())
	m.UpdateViewport()
}

// Height returns the viewport height of the table.
func (m Model) Height() int {
	return m.viewport.Height
}

// Width returns the viewport width of the table.
func (m Model) Width() int {
	return m.viewport.Width
}

// Cursor returns the index of the selected row.
func (m Model) Cursor() int {
	return m.cursor
}

// SetCursor sets the cursor position in the table.
func (m *Model) SetCursor(n int) {
	m.cursor = clamp(n, 0, len(m.rows)-1)
	m.UpdateViewport()
}

// MoveUp moves the selection up by any number of rows.
// It can not go above the first row.
func (m *Model) MoveUp(n int) {
	m.cursor = clamp(m.cursor-n, 0, len(m.rows)-1)
	m.UpdateViewport()
}

// MoveDown moves the selection down by any number of rows.
// It can not go below the last row.
func (m *Model) MoveDown(n int) {
	m.cursor = clamp(m.cursor+n, 0, len(m.rows)-1)
	m.UpdateViewport()
}

// GotoTop moves the selection to the first row.
func (m *Model) GotoTop() {
	m.MoveUp(m.cursor)
}

// GotoBottom moves the selection to the last row.
func (m *Model) GotoBottom() {
	m.MoveDown(len(m.rows))
}

// FromValues create the table rows from a simple string. It uses `\n` by
// default for getting all the rows and the given separator for the fields on
// each row.
func (m *Model) FromValues(value, separator string) {
	rows := []Row{}
	for _, line := range strings.Split(value, "\n") {
		r := Row{}
		for _, field := range strings.Split(line, separator) {
			r = append(r, field)
		}
		rows = append(rows, r)
	}

	m.SetRows(rows)
}

func (m Model) headersView() string {
	s := make([]string, 0, len(m.cols))
	for _, col := range m.cols {
		if col.Width <= 0 {
			continue
		}
		style := lipgloss.NewStyle().Width(col.Width).MaxWidth(col.Width).Inline(true)
		renderedCell := style.Render(runewidth.Truncate(col.Title, col.Width, "…"))
		s = append(s, m.styles.Header.Render(renderedCell))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, s...)
}

// renderRow draws one row. A row of EXACTLY ONE CELL is a section row: it renders as
// one line spanning every column, not as one cell squeezed into the first column's
// width — the shape the events pane's subagent block headers need, where the label is
// longer than any single column. Fewer cells than columns is already harmless
// elsewhere; this pins the one-cell case to a meaning rather than leaving it a
// truncated first cell. No caller rendered a one-cell row before this (the pipeline
// pane's "── (app) ──" divider carries a cell per column), so the convention changes
// nothing that existed.
func (m *Model) renderRow(r int) string {
	if len(m.rows[r]) == 1 {
		return m.renderSectionRow(r)
	}
	texts := make([]string, len(m.rows[r]))
	var spans [][]find.Range
	if r < len(m.hits) && m.hits[r] {
		spans = make([][]find.Range, len(m.rows[r]))
	}
	lit := false
	for i, value := range m.rows[r] {
		if m.cols[i].Width <= 0 {
			continue
		}
		texts[i] = runewidth.Truncate(value, m.cols[i].Width, "…")
		if spans != nil {
			spans[i] = m.cellSpans(r, i, texts[i])
			lit = lit || len(spans[i]) > 0
		}
	}

	row, match := lipgloss.NewStyle(), m.styles.Match
	switch {
	case r == m.cursor:
		row, match = m.styles.Selected, m.styles.CurrentMatch
	case spans != nil && !lit:
		row = m.styles.Marked
	}
	match = match.Inherit(row)

	s := make([]string, 0, len(m.cols))
	for i := range m.rows[r] {
		if m.cols[i].Width <= 0 {
			continue
		}
		var sp []find.Range
		if spans != nil {
			sp = spans[i]
		}
		box := lipgloss.NewStyle().Width(m.cols[i].Width).MaxWidth(m.cols[i].Width).Inline(true).Inherit(row)
		// Filled to the column before it is painted, so the text and the padding after it are
		// one run in the row's style rather than a run and a pad lipgloss styles apart.
		filled := runewidth.FillRight(texts[i], m.cols[i].Width)
		s = append(s, m.styles.Cell.Inherit(row).Render(box.Render(paint(filled, sp, row, match))))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, s...)
}

// renderSectionRow draws a one-cell row across the whole table width, in the same
// row/cursor/match styles a data row gets. The line is the sum of the column widths
// — without each cell's padding, which a spanning line has no use for, so it sits a
// few columns short of a full data row's right edge and reads as its own kind of row.
func (m *Model) renderSectionRow(r int) string {
	total := 0
	for _, col := range m.cols {
		if col.Width > 0 {
			total += col.Width
		}
	}
	text := runewidth.Truncate(m.rows[r][0], total, "…")
	var spans []find.Range
	lit := false
	if r < len(m.hits) && m.hits[r] {
		spans = m.cellSpans(r, 0, text)
		lit = len(spans) > 0
	}

	row, match := lipgloss.NewStyle(), m.styles.Match
	switch {
	case r == m.cursor:
		row, match = m.styles.Selected, m.styles.CurrentMatch
	case spans != nil && !lit:
		row = m.styles.Marked
	}
	match = match.Inherit(row)

	filled := runewidth.FillRight(text, total)
	return m.styles.Cell.Inherit(row).Render(paint(filled, spans, row, match))
}

func clamp(v, low, high int) int {
	return min(max(v, low), high)
}
