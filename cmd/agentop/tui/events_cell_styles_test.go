package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// fgSGR is the SGR parameter text that sets c as the foreground under the current
// profile and theme.
func fgSGR(c lipgloss.AdaptiveColor) string {
	hex := c.Light
	if lipgloss.HasDarkBackground() {
		hex = c.Dark
	}
	return lipgloss.ColorProfile().Color(hex).Sequence(false)
}

// inkBefore is the SGR sequence directly in front of the first text in view, after the
// cell's alignment spaces. It fails the test when text is missing or no SGR is there.
func inkBefore(t *testing.T, view, text string) string {
	t.Helper()
	i := strings.Index(view, text)
	if i < 0 {
		t.Fatalf("%q is not in the view:\n%s", text, ansi.Strip(view))
	}
	before := strings.TrimRight(view[:i], " ")
	j := strings.LastIndex(before, "\x1b[")
	if j < 0 || !strings.HasSuffix(before, "m") || strings.Contains(before[j:], " ") {
		t.Fatalf("no SGR directly before %q in %q", text, before[max(0, len(before)-40):])
	}
	return before[j:]
}

func assertInk(t *testing.T, view, text string, c lipgloss.AdaptiveColor, name string) {
	t.Helper()
	if sgr := inkBefore(t, view, text); !strings.Contains(sgr, fgSGR(c)) {
		t.Errorf("%q: SGR %q has no %s ink (%s)", text, sgr, name, fgSGR(c))
	}
}

// assertLastInk checks the last SGR before the first text in s. Unlike assertInk it allows
// text between them, for cells of one ink that the table draws as one run.
func assertLastInk(t *testing.T, s, text string, c lipgloss.AdaptiveColor, name string) {
	t.Helper()
	i := strings.Index(s, text)
	if i < 0 {
		t.Fatalf("%q is not in %q", text, ansi.Strip(s))
	}
	j := strings.LastIndex(s[:i], "\x1b[")
	if j < 0 {
		t.Fatalf("no SGR before %q", text)
	}
	if sgr := s[j:i]; !strings.Contains(sgr, fgSGR(c)) {
		t.Errorf("%q: last SGR %q has no %s ink (%s)", text, sgr, name, fgSGR(c))
	}
}

// cellStylesEvents is a modify exchange answered 200, an exchange answered 503 and an
// unbridged CONNECT, so the ACTION and STATUS cells hold one value of each group.
func cellStylesEvents() []pipeline.SessionEvent {
	ok := trExchange(1, 10, "")
	ok[0].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{
		{Plugin: "token-exchange", Action: pipeline.ActionModify},
	}}
	failed := trExchange(2, 20, "")
	failed[1].StatusCode = 503
	tunnel := pipeline.SessionEvent{At: tlAt(30), Seq: 5, Direction: pipeline.Outbound,
		Phase: pipeline.SessionRequest, Host: "tun.test:443", Tunnel: true, RequestID: "rt"}
	return slices.Concat(ok, failed, []pipeline.SessionEvent{tunnel})
}

// Column styles follow the column id: with DIR and PLUGIN hidden and STATUS sorted (its
// header reads STATUS▼), every cell still takes its own column's ink.
func TestEventsCellStyles_ByColumnID(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := fitModel(t, paneEvents, 200, 40, cellStylesEvents())
	m.eventColumns[colDir], m.eventColumns[colPlugin] = false, false
	m.sortCol, m.sortDesc = colStatus, true
	m.rebuildEventsTable()
	view := m.eventsTbl.View()
	if !strings.Contains(view, "STATUS▼") {
		t.Fatalf("no sorted STATUS header in:\n%s", ansi.Strip(view))
	}
	assertInk(t, view, "503", colorError, "error")
	assertInk(t, view, "200", colorOK, "ok")
	assertInk(t, view, "modify", colorAccent, "accent")
	// The unbridged CONNECT is quiet (#2019 change 6), so its ACTION is muted, not dim.
	assertInk(t, view, "tunnel", colorMuted, "muted")
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		if slices.Contains(strings.Fields(line), "out") {
			t.Errorf("DIR is hidden, but a row shows %q: %q", "out", line)
		}
	}
}

func TestEventsCellStyles_ToolRows(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 30, ""))
	tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Grep", 98.8, -1, "")}}
	m := toolRowsModel(t, events, tl)
	m.now = func() time.Time { return tlAt(100) }
	m.rebuildEventsTable()
	_, row := toolRowAt(t, m, "t1")
	dur := toolCell(m, row, colDuration)
	if !strings.HasPrefix(dur, "⋯") {
		t.Fatalf("DURATION = %q, want a running tool's ⋯ cell", dur)
	}
	view := m.eventsTbl.View()
	assertLastInk(t, view, "┗", colorDim, "dim")
	assertLastInk(t, view, "⚒", colorDim, "dim")
	assertLastInk(t, view, "Grep", colorMuted, "muted")
	assertLastInk(t, view, dur, colorWarn, "warn")
	assertInk(t, view, "running", colorWarn, "warn")
}

func TestEventsCellStyles_BlockRowAccent(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := newBlockModel(t, blockEvents())
	view := m.eventsTbl.View()
	// The glyph is a run of its own (tableGlyphs), so each part is checked alone.
	assertInk(t, view, "▸", colorAccent, "accent")
	assertInk(t, view, "─ a1", colorAccent, "accent")
}

func TestEventsCellStyles_PlainInvariant(t *testing.T) {
	for _, w := range []int{80, 100, 140} {
		events := slices.Concat(cellStylesEvents(), trExchange(3, 40, ""))
		tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(6, "t1", "Bash", 42, 43, "ls")}}
		m := toolRowsModel(t, events, tl)
		m.width = w
		assertPlainInvariant(t, func() string {
			m.rebuildEventsTable()
			return m.eventsTbl.View()
		})
	}
}

// A tool row keeps its ink with # hidden: no cell then shows the ┣⚒ mark, so the hook must
// know the row is a tool row from the row itself, not from a visible cell.
func TestEventsCellStyles_ToolRowsWithIndexHidden(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 30, ""))
	tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(2, "t1", "Grep", 98.8, -1, "")}}
	m := toolRowsModel(t, events, tl)
	m.now = func() time.Time { return tlAt(100) }
	m.eventColumns = defaultColumnSelection()
	m.eventColumns[colIndex] = false
	m.rebuildEventsTable()
	_, row := toolRowAt(t, m, "t1")
	dur := toolCell(m, row, colDuration)
	view := m.eventsTbl.View()
	if strings.Contains(ansi.Strip(view), "⚒") {
		t.Fatalf("# is hidden, but the view shows the tool mark:\n%s", ansi.Strip(view))
	}
	assertLastInk(t, view, "Grep", colorMuted, "muted")
	assertLastInk(t, view, dur, colorWarn, "warn")
}

// A block header is accent when # is the only column: one visible column makes every row one
// cell wide, so the header is known by its row, not by its width.
func TestEventsCellStyles_BlockRowAccentOneColumn(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := newBlockModel(t, blockEvents())
	m.eventColumns = map[eventColumnID]bool{colIndex: true}
	m.rebuildEventsTable()
	view := m.eventsTbl.View()
	// The # column is narrow, so the header's label is cut to "─ …".
	assertInk(t, view, "─ …", colorAccent, "accent")
}

// rowFg is the foreground the events hook gives each cell of row r, keyed by column id.
func rowFg(m *model, r int) map[eventColumnID]lipgloss.TerminalColor {
	cols, _ := layoutColumns(m.eventColumns, m.eventColWidths, m.width)
	rows := m.eventsTbl.Rows()
	hook := eventCellStyle(cols, m.eventRowKinds(len(rows)), rows)
	row := rows[r]
	fg := map[eventColumnID]lipgloss.TerminalColor{}
	for i, c := range cols {
		if i < len(row) && strings.TrimSpace(row[i]) != "" {
			fg[c.id] = hook(r, row, i).GetForeground()
		}
	}
	return fg
}

// drawnLine is the drawn line of view whose plain text contains text.
func drawnLine(t *testing.T, view, text string) string {
	t.Helper()
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(ansi.Strip(l), text) {
			return l
		}
	}
	t.Fatalf("no line with %q in:\n%s", text, ansi.Strip(view))
	return ""
}

// A tool row steps back (#2019 change 3): every cell is muted, only live state (running, ⋯)
// keeps the warn ink and a failed tool the error ink, and its ┗⚒/┣⚒ mark and a subagent │
// are dim. The same holds in search results and inside an open subagent block.
func TestEventCellStyle_ToolRowsMuted(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	okTool := trTool(2, "ok", "Grep", 12, 13, "x")
	running := trTool(2, "run", "Bash", 12, -1, "")
	failed := trTool(2, "err", "Edit", 12, 13, "")
	failed.Error = true
	lost := trTool(2, "lost", "Read", 12, 14, "")
	lost.Lost = true
	m := toolRowsModel(t, trExchange(1, 10, ""), &timeline.Timeline{Tools: []timeline.Tool{okTool, running, failed, lost}})
	want := map[string]map[eventColumnID]lipgloss.AdaptiveColor{
		"ok":   {colStatus: colorMuted},
		"run":  {colStatus: colorWarn, colDuration: colorWarn},
		"err":  {colStatus: colorError},
		"lost": {colStatus: colorError},
	}
	for id, special := range want {
		r, row := toolRowAt(t, m, id)
		for col, fg := range rowFg(m, r) {
			c, ok := special[col]
			if !ok {
				c = colorMuted
			}
			if strings.TrimSpace(row[colPos(m, col)]) == "—" {
				c = colorDim
			}
			if fg != c {
				t.Errorf("%s: column %v = %v, want %v", id, col, fg, c)
			}
		}
	}
	view := m.eventsTbl.View()
	assertLastInk(t, view, "┣", colorDim, "dim")
	assertLastInk(t, view, "┗", colorDim, "dim")
	assertLastInk(t, view, "⚒", colorDim, "dim")
	assertLastInk(t, drawnLine(t, view, "Grep"), "ok", colorMuted, "muted")

	m.search = map[paneID]string{paneEvents: "Grep"}
	m.rebuildEventsTable()
	view = m.eventsTbl.View()
	assertLastInk(t, drawnLine(t, view, "Grep"), "┣", colorDim, "dim")
	assertLastInk(t, drawnLine(t, view, "Grep"), "ok", colorMuted, "muted")
	if fg := rowFg(m, slices.IndexFunc(m.visibleRows, func(r eventRow) bool { return r.tool != nil && r.tool.ID == "ok" })); fg[colStatus] != colorMuted {
		t.Errorf("search: STATUS ok = %v, want muted", fg[colStatus])
	}
}

func TestEventCellStyle_ToolRowsMutedInSubagentBlock(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, "a1"), trExchange(3, 30, ""))
	tl := &timeline.Timeline{Tools: []timeline.Tool{trTool(4, "sub", "Read", 22, 23, "x")}}
	tl.Tools[0].Agent = "a1"
	m := toolRowsModel(t, events, tl)
	m.subagentOpen = map[string]bool{"a1": true}
	m.rebuildEventsTable()
	r, _ := toolRowAt(t, m, "sub")
	for col, fg := range rowFg(m, r) {
		if fg != colorMuted && fg != colorDim {
			t.Errorf("column %v = %v, want muted", col, fg)
		}
	}
	line := drawnLine(t, m.eventsTbl.View(), "Read")
	assertLastInk(t, line, "│", colorDim, "dim")
	assertLastInk(t, line, "Read", colorMuted, "muted")
}

func TestRowQuiet(t *testing.T) {
	skip := pipeline.Invocation{Plugin: "p", Action: pipeline.ActionSkip}
	shadowDeny := pipeline.Invocation{Plugin: "p", Action: pipeline.ActionDeny, Shadow: true}
	gateDeny := pipeline.Invocation{Plugin: "egress-gate", Action: pipeline.ActionDeny}
	tunnel := func(invs ...pipeline.Invocation) cellContext {
		ev := &pipeline.SessionEvent{Tunnel: true, Host: "tun.test:443"}
		return cellContext{row: eventRow{event: ev}, invs: invs}
	}
	ctxs := []cellContext{
		{}, // 0: no invocations
		{invs: []pipeline.Invocation{skip, skip}}, // 1: all skip
		{invs: []pipeline.Invocation{shadowDeny}}, // 2: a shadow deny
		{},               // 3: inactive, partner 2 is active
		tunnel(),         // 4: an unbridged tunnel
		tunnel(gateDeny), // 5: a tunnel the gate denied
	}
	partner := map[int]int{3: 2, 2: 3}
	want := []bool{true, true, false, false, true, false}
	for j, w := range want {
		if got := rowQuiet(ctxs, partner, j); got != w {
			t.Errorf("row %d: rowQuiet = %v, want %v", j, got, w)
		}
	}
}

// quietEvents are four exchanges to api.test: 1 and 4 quiet and answered 200, 2 active
// (modify) and answered 200, 3 quiet and answered 502.
func quietEvents() []pipeline.SessionEvent {
	active := trExchange(2, 20, "")
	active[0].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{
		{Plugin: "token-exchange", Action: pipeline.ActionModify},
	}}
	failed := trExchange(3, 30, "")
	failed[1].StatusCode = 502
	return slices.Concat(trExchange(1, 10, ""), active, failed, trExchange(4, 40, ""))
}

// exchangeRow is the rendered row index of the exchange whose request has seq.
func exchangeRow(t *testing.T, m *model, seq uint64) int {
	t.Helper()
	i := slices.IndexFunc(m.visibleRows, func(r eventRow) bool { return r.event != nil && r.event.Seq == seq })
	if i < 0 {
		t.Fatalf("no row for seq %d", seq)
	}
	return i
}

// wantQuietFg checks row r of m is a quiet row: muted, except the columns in special, and a
// "—" cell, which is dim.
func wantQuietFg(t *testing.T, m *model, r int, special map[eventColumnID]lipgloss.AdaptiveColor) {
	t.Helper()
	row := m.eventsTbl.Rows()[r]
	for col, fg := range rowFg(m, r) {
		c, ok := special[col]
		if !ok {
			c = colorMuted
		}
		if strings.TrimSpace(row[colPos(m, col)]) == "—" {
			c = colorDim
		}
		if fg != c {
			t.Errorf("row %d: column %v = %v, want %v", r, col, fg, c)
		}
	}
}

// Background traffic steps back (#2019 change 6): a quiet row is muted, a 5xx keeps its
// error ink, and a run of quiet rows dims what repeats the row above. An active row stops
// the run.
func TestEventCellStyle_QuietRows(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := fitModel(t, paneEvents, 200, 40, quietEvents())
	r1, r2, r3, r4 := exchangeRow(t, m, 1), exchangeRow(t, m, 3), exchangeRow(t, m, 5), exchangeRow(t, m, 7)
	if r2 != r1+1 || r3 != r2+1 || r4 != r3+1 {
		t.Fatalf("rows %d %d %d %d are not adjacent", r1, r2, r3, r4)
	}
	wantQuietFg(t, m, r1, nil)
	wantQuietFg(t, m, r3, map[eventColumnID]lipgloss.AdaptiveColor{
		colStatus: colorError, colHost: colorMuted, colMethod: colorMuted, colDir: colorMuted,
	})
	// The row after r3 repeats it: DIR, METHOD and HOST are dim, TIME stays muted.
	wantQuietFg(t, m, r4, map[eventColumnID]lipgloss.AdaptiveColor{
		colHost: colorDim, colMethod: colorDim, colDir: colorDim,
	})
	if fg := rowFg(m, r2); fg[colStatus] != colorOK || fg[colDir] != colorOutbound {
		t.Errorf("active row: STATUS %v, DIR %v; want ok and outbound ink", fg[colStatus], fg[colDir])
	}
	view := m.eventsTbl.View()
	assertInk(t, view, "502", colorError, "error")
	assertLastInk(t, drawnLine(t, view, "502"), "api.test", colorMuted, "muted")
}

// The quiet ink and the dim repeats follow the sorted rows, and key by column id with
// ACTION and PLUGIN hidden (#2019 Review Focus 2, 3).
func TestEventCellStyle_QuietFollowsSortAndColumns(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := fitModel(t, paneEvents, 200, 40, quietEvents())
	m.eventColumns[colAction], m.eventColumns[colPlugin] = false, false
	m.sortCol, m.sortDesc = colStatus, true
	m.rebuildEventsTable()
	if !strings.Contains(ansi.Strip(m.eventsTbl.View()), "STATUS▼") {
		t.Fatal("no sorted STATUS header")
	}
	// Sorted: 502 (seq 5), then the 200s in arrival order (1, 3, 7).
	order := []int{exchangeRow(t, m, 5), exchangeRow(t, m, 1), exchangeRow(t, m, 3), exchangeRow(t, m, 7)}
	for i := 1; i < len(order); i++ {
		if order[i] != order[0]+i {
			t.Fatalf("sorted rows = %v, want adjacent from %d", order, order[0])
		}
	}
	fg := rowFg(m, order[0])
	if _, ok := fg[colAction]; ok {
		t.Fatal("ACTION is hidden, but the hook styles it")
	}
	wantQuietFg(t, m, order[0], map[eventColumnID]lipgloss.AdaptiveColor{colStatus: colorError})
	// Seq 1 sits under the quiet 502 row now, so it repeats it.
	wantQuietFg(t, m, order[1], map[eventColumnID]lipgloss.AdaptiveColor{
		colHost: colorDim, colMethod: colorDim, colDir: colorDim,
	})
	if fg := rowFg(m, order[2]); fg[colStatus] != colorOK {
		t.Errorf("active row STATUS = %v, want ok", fg[colStatus])
	}
	// Seq 7 sits under the active row, so nothing on it is dim, though in arrival order it
	// follows the quiet 502 row.
	wantQuietFg(t, m, order[3], nil)
}

// Search over quiet rows (#2019 Review Focus 1): a match keeps the match style on a muted
// cell and on a dim repeat, a marked row keeps its warn ink, and the selected quiet row
// keeps its tint.
func TestQuietRows_SearchMatchWins(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	esc := func(s lipgloss.Style) string { return strings.SplitN(s.Render("x"), "x", 2)[0] }
	st := tableStyles()
	events := slices.Concat(trExchange(1, 10, ""), trExchange(2, 20, ""), trExchange(3, 30, ""))
	for i := range events {
		events[i].Host = "q.test"
	}
	// The third exchange's host is cut to HOST's width, so its match is off screen: marked.
	events[4].Host, events[5].Host = "aaaaaaaaaaaaaaaaaaaaaaaaaaaa-tail.test", "aaaaaaaaaaaaaaaaaaaaaaaaaaaa-tail.test"
	m := fitModel(t, paneEvents, 200, 40, events)
	m.search = map[paneID]string{paneEvents: "q.test"}
	m.rebuildEventsTable()
	m.eventsTbl.SetCursor(exchangeRow(t, m, 1))
	r2 := exchangeRow(t, m, 3)
	if fg := rowFg(m, r2); fg[colHost] != colorDim {
		t.Fatalf("second row HOST = %v, want the dim repeat", fg[colHost])
	}
	lines := []string{}
	for _, l := range strings.Split(m.eventsTbl.View(), "\n") {
		if strings.Contains(ansi.Strip(l), "q.test") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 lines with q.test, got %d", len(lines))
	}
	if !strings.Contains(lines[0], esc(st.Selected)) {
		t.Errorf("selected quiet row lost its tint: %q", lines[0])
	}
	if !strings.Contains(lines[1], st.Match.Render("q.test")) {
		t.Errorf("dim repeat: match style lost: %q", lines[1])
	}

	m.search = map[paneID]string{paneEvents: "tail"}
	m.rebuildEventsTable()
	if got := rowLook(m.eventsTbl, "aaaaaaaaaaaa"); got != "marked" {
		t.Errorf("quiet row with an off-screen match is %s, want marked", got)
	}
}
