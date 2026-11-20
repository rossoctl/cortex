package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// sgrAt is the SGR parameters in force where needle first starts on line, outside escapes:
// the last escape before it. "" when needle is not on the line or no escape precedes it.
func sgrAt(line, needle string) string {
	last := ""
	for i := 0; i < len(line); {
		if strings.HasPrefix(line[i:], "\x1b[") {
			end := strings.IndexByte(line[i:], 'm')
			if end < 0 {
				return ""
			}
			last = line[i+2 : i+end]
			i += end + 1
			continue
		}
		if strings.HasPrefix(line[i:], needle) {
			return last
		}
		i++
	}
	return ""
}

// fgOf is the foreground parameters style s emits, such as "38;2;110;231;183".
func fgOf(s lipgloss.Style) string {
	r := s.Render("x")
	return strings.TrimSuffix(strings.TrimPrefix(r[:strings.Index(r, "x")], "\x1b["), "m")
}

// boldIn reports whether SGR parameters p set bold, skipping the parts of a color.
func boldIn(p string) bool {
	parts := strings.Split(p, ";")
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "1":
			return true
		case "38", "48":
			i += 4 // ;2;r;g;b
		}
	}
	return false
}

// tableLines is tbl's rendered lines, and the index of the first row line (the one that
// shows first, a cell of row 0).
func tableLines(t *testing.T, tbl table.Model, first string) ([]string, int) {
	t.Helper()
	lines := strings.Split(tbl.View(), "\n")
	for i, l := range lines {
		if strings.Contains(ansi.Strip(l), first) {
			return lines, i
		}
	}
	t.Fatalf("no line shows %q in\n%s", first, ansi.Strip(tbl.View()))
	return nil, 0
}

func titledCol(cols []table.Column, title string) int {
	return slices.IndexFunc(cols, func(c table.Column) bool { return headerTitle(c) == title })
}

func TestSessionsCellStyles_FollowColumnIdentity(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	for _, w := range []int{140, 100} {
		var m *model
		if w == 140 {
			m = sessionsServerModel(routerRaw)
			m.width = 140
		} else {
			m = fitModel(t, paneSessions, w, 24, nil)
		}
		for i := range m.sessions {
			m.sessions[i].CostMicros, m.sessions[i].AvoidedMicros = 1_230_000, 450_000
		}
		m.rebuildSessionsTable()
		cols := m.sessionsTbl.Columns()
		if got, want := titledCol(cols, "AGENT") >= 0 && titledCol(cols, "SERVER") >= 0, w == 140; got != want {
			t.Fatalf("width %d: AGENT and SERVER shown = %v, want %v", w, got, want)
		}
		rows := m.sessionsTbl.Rows()
		const r = 2 // not the cursor row, not a zebra row
		if m.sessionsTbl.Cursor() == r {
			t.Fatalf("width %d: the cursor is on row %d", w, r)
		}
		lines, first := tableLines(t, m.sessionsTbl, strings.TrimSpace(rows[0][0]))
		line := lines[first+r]
		cell := func(title string) string { return strings.TrimSpace(rows[r][titledCol(cols, title)]) }

		if p := sgrAt(line, cell("COST")); !boldIn(p) {
			t.Errorf("width %d: COST %q has SGR %q, want bold", w, cell("COST"), p)
		}
		if p := sgrAt(line, cell("SAVED")); !strings.Contains(p, fgOf(styleOK)) {
			t.Errorf("width %d: SAVED %q has SGR %q, want the ok ink %q", w, cell("SAVED"), p, fgOf(styleOK))
		}
		if p := sgrAt(line, cell("SESSION")); !strings.Contains(p, fgOf(styleMuted)) {
			t.Errorf("width %d: SESSION %q has SGR %q, want the muted ink", w, cell("SESSION"), p)
		}
		if p := sgrAt(line, cell("EVENTS")); boldIn(p) || strings.Contains(p, "38;") {
			t.Errorf("width %d: EVENTS %q has SGR %q, want none", w, cell("EVENTS"), p)
		}

		// An unpriced session: its COST is "—", which is dim.
		for i := range m.sessions {
			m.sessions[i].CostMicros = 0
		}
		m.rebuildSessionsTable()
		rows = m.sessionsTbl.Rows()
		if cell("COST") != "—" {
			t.Fatalf("width %d: unpriced COST is %q, want —", w, cell("COST"))
		}
		lines, first = tableLines(t, m.sessionsTbl, strings.TrimSpace(rows[0][0]))
		// The first "—" on the line: COST is the first column that can hold one here.
		if p := sgrAt(lines[first+r], "—"); !strings.Contains(p, fgOf(styleDim)) {
			t.Errorf("width %d: unpriced COST has SGR %q, want the dim ink", w, p)
		}
	}
}

func TestAgentsCellStyles_TopLevelBold(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := fitModel(t, paneAgents, 140, 30, nil)
	m.sessions[0].Agent, m.sessions[0].Projects = "claude-code/2.1.270", []string{"/src/cortex"}
	m.agentsExpanded = map[string]bool{"claude-code/2.1.270": true}
	m.rebuildAgentsTable()
	rows := m.agentsTbl.Rows()
	lines, first := tableLines(t, m.agentsTbl, strings.TrimSpace(rows[0][0]))
	find := func(label string) string {
		for i, r := range rows {
			if strings.TrimSpace(r[0]) == label {
				if i == m.agentsTbl.Cursor() {
					t.Fatalf("row %q is the cursor row", label)
				}
				return lines[first+i]
			}
		}
		t.Fatalf("no agents row %q in %q", label, rows)
		return ""
	}
	heading := find("▾ claude-code/2.1.270")
	if p := sgrAt(heading, "claude-code/2.1.270"); !boldIn(p) {
		t.Errorf("open agent name has SGR %q, want bold", p)
	}
	if p := sgrAt(heading, "▾"); !strings.Contains(p, fgOf(styleAccent)) {
		t.Errorf("▾ has SGR %q, want the accent ink", p)
	}
	closed := find("▸ bob-shell/2.0.5")
	if p := sgrAt(closed, "bob-shell"); !boldIn(p) {
		t.Errorf("closed agent name has SGR %q, want bold", p)
	}
	if p := sgrAt(closed, "▸"); !strings.Contains(p, fgOf(styleAccent)) {
		t.Errorf("▸ has SGR %q, want the accent ink", p)
	}
	if p := sgrAt(find("All projects"), "All projects"); boldIn(p) {
		t.Errorf("project sub-row has SGR %q, want not bold", p)
	}
}

func TestPipelineCellStyles(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	m := fitModel(t, panePipeline, 140, 30, nil)
	m.pipeline.Inbound[1].Requires = []string{"inbound-plugin-00"}
	m.rebuildPipelineTable()
	rows := m.pipelineTbl.Rows()
	lines, first := tableLines(t, m.pipelineTbl, rows[0][2])
	row := func(name string) string {
		i := slices.IndexFunc(rows, func(r table.Row) bool { return r[2] == name })
		if i < 0 || i == m.pipelineTbl.Cursor() {
			t.Fatalf("row %q is missing or the cursor row", name)
		}
		return lines[first+i]
	}
	l := row("inbound-plugin-01")
	if p := sgrAt(l, "inbound-plugin-01"); !boldIn(p) {
		t.Errorf("PLUGIN has SGR %q, want bold", p)
	}
	if p := sgrAt(l, "✓"); !strings.Contains(p, fgOf(styleOK)) {
		t.Errorf("✓ has SGR %q, want the ok ink", p)
	}
	if p := sgrAt(l, "inbound"); !strings.Contains(p, fgOf(styleInbound)) {
		t.Errorf("DIRECTION has SGR %q, want the inbound ink", p)
	}
	if p := sgrAt(row("outbound-plugin-01"), "outbound"); !strings.Contains(p, fgOf(styleOutbound)) {
		t.Errorf("DIRECTION has SGR %q, want the outbound ink", p)
	}
	if p := sgrAt(row("── (app) ──"), "── (app) ──"); !strings.Contains(p, fgOf(styleDim)) {
		t.Errorf("divider has SGR %q, want the dim ink", p)
	}
}

func TestPickerCellStyles_NameBoldRestMuted(t *testing.T) {
	withProfile(t, termenv.TrueColor)
	tbl := newNamespacesTable()
	tbl.SetRows([]table.Row{{"team1", "3"}, {"team2", "4"}, {"team3", "5"}})
	lines, first := tableLines(t, tbl, "team1")
	if p := sgrAt(lines[first+2], "team3"); !boldIn(p) {
		t.Errorf("namespace name has SGR %q, want bold", p)
	}
	if p := sgrAt(lines[first+2], "5"); !strings.Contains(p, fgOf(styleMuted)) {
		t.Errorf("PODS has SGR %q, want the muted ink", p)
	}
}

// Color changes no text and no width in any of these panes' tables.
func TestCellStyles_PlainInvariant(t *testing.T) {
	for _, w := range []int{80, 100, 140} {
		m := fitModel(t, paneSessions, w, 30, nil)
		for _, c := range []struct {
			name string
			view func() string
		}{
			{"sessions", func() string { m.rebuildSessionsTable(); return m.sessionsTbl.View() }},
			{"agents", func() string { m.rebuildAgentsTable(); return m.agentsTbl.View() }},
			{"pipeline", func() string { m.rebuildPipelineTable(); return m.pipelineTbl.View() }},
		} {
			t.Run(fmt.Sprintf("%s/%d", c.name, w), func(t *testing.T) { assertPlainInvariant(t, c.view) })
		}
	}
}
