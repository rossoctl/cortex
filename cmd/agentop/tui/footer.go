package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// feedbackURL is where a user sends feedback or reports a problem (#975). It is the last
// section of the [?] help (helpBodyLines), not on the screen: the bottom bar is one line and
// has no room for it.
//
// The /issues/new/choose form drops the user on the template picker — the
// Laptop feedback form — one click from filing, rather than on the issue list.
// This is the same destination the docs point at (see rossoctl#977).
const feedbackURL = "https://github.com/rossoctl/cortex/issues/new/choose"

// footerLines is the height of footerView, and the number of rows layout() reserves for it.
const footerLines = 1

// barEssentialHints ends every helpView line (TestFitHintLine_KeepsQuitAndHelpAtEveryWidth).
const barEssentialHints = "[?] keys  [q] quit"

// barInk is st drawn on the bar: the status runs keep their ink and get the bar's ground.
func barInk(st lipgloss.Style) lipgloss.Style { return st.Background(colorBarBg) }

// footerView is the one bottom bar: the key hints on the left and the status on the right
// (see bottomBar). The status is ordered by what a cut drops last: the connection state,
// then the other notes, then a timed flash.
func (m *model) footerView() string {
	conn := m.connStatus()

	// A sticky flash takes the place of the other notes and of every hint but [?] and [q].
	//
	// Yank is the case this exists for: the path is the longest thing the bar ever
	// carries, and the filename at its end is the whole point of showing it. The other
	// notes and hints return on the next keypress, and a sticky flash is by definition
	// something the user just asked for and is reading right now.
	if m.flash != "" && m.flashSticky {
		statusRoom := m.width - lipgloss.Width("… "+barEssentialHints) - 2
		flash := barInk(styleTitle)
		status := ""
		if room := statusRoom - lipgloss.Width(conn) - 3; room >= 2 {
			status = conn + barInk(styleMuted).Render("   ") + flash.Render(fitFlashLine(m.flash, room))
		} else if statusRoom >= 1 {
			status = flash.Render(fitFlashLine(m.flash, statusRoom))
		}
		return bottomBar(barEssentialHints, status, m.width)
	}

	var status strings.Builder
	status.WriteString(conn)
	status.WriteString(barInk(styleMuted).Render("  "))

	// Rate. Spelled out rather than "ev/s", which reads as a unit the operator has to
	// decode before it tells them anything.
	//
	// A "drops: N" counter used to sit beside it. Nothing ever incremented it, so it
	// displayed a hardcoded zero — which is worse than showing nothing, because it
	// teaches that no event is ever dropped. Removed with its field; see #1060.
	status.WriteString(barInk(styleMuted).Render(fmt.Sprintf("%.1f events/sec", m.rate)))
	warn := barInk(styleWarn)
	if m.paused {
		status.WriteString(warn.Render("   [paused]"))
	}
	// A server switch S started, until its one flash replaces this. State, not a key, like
	// [paused]: the write waits for the proxy's reload, which takes about a second.
	if m.serverSwitch != nil {
		status.WriteString(warn.Render("   [" + m.serverSwitch.text() + "]"))
	}
	// The active pane's search: which match the cursor is on, how many there are, or that
	// there are none — see searchStatus. In the status rather than in the hints because it is
	// state, not a keybinding — the same reason [paused] sits above.
	if s := m.searchStatus(m.pane); s != "" {
		status.WriteString(warn.Render("   " + s))
	}
	if m.pane == paneSessions && m.agentScope != "" && m.sessionsScope() == "" {
		status.WriteString(warn.Render("   [no session names its agent: list not scoped]"))
	}
	if m.pane == paneSessions && m.projectScope != "" && m.sessionsProjectScope() == "" {
		status.WriteString(warn.Render("   [no session names its project: list not scoped]"))
	}
	// A non-chronological sort, for the same reason as the search above: it is
	// state the operator chose, and a table in an order the eye does not expect
	// reads as a bug when nothing on screen names the ordering.
	//
	// Here as well as in the column header, because the sorted column may be one
	// fitColumns dropped on a narrow terminal — and then there is no header on
	// screen to carry the glyph, which is precisely when the reordering is most
	// confusing.
	//
	// Gated on the pane as well as the column, because sortCol is model-global and
	// restored from settings at startup: it survives into panes that hold no sortable
	// table, where "[sort: COST▼]" names an ordering nothing on screen has. The Usage
	// pane is a chart in time order and that is the case #1060 reports.
	//
	// paneEvents alone, not a list of panes: the events table is the only sortable one
	// in the TUI — sessions_pane.go never reads sortCol, and the [s]/[d] keys that set
	// it are handled under paneEvents only.
	if m.sortCol != "" && m.pane == paneEvents {
		glyph := sortGlyphAsc
		if m.sortDesc {
			glyph = sortGlyphDesc
		}
		status.WriteString(warn.Render("   [sort: " + string(m.sortCol) + glyph + "]"))
	}

	// Timed flash message (e.g. "copied"), last: a cut drops it first.
	if m.flash != "" && time.Now().Before(m.flashUntil) {
		status.WriteString(barInk(styleTitle).Render("   " + m.flash))
	}
	return bottomBar(m.helpView(), status.String(), m.width)
}

// connStatus is the connection state, styled for the bar.
func (m *model) connStatus() string {
	switch m.connState.phase {
	case connOpen:
		return barInk(styleOK).Render("● connected")
	case connReconnecting:
		return barInk(styleWarn).Render(fmt.Sprintf("◐ reconnecting (attempt %d, next in %ds)",
			m.connState.attempt,
			int(time.Until(m.connState.nextRetry).Round(time.Second).Seconds())))
	case connFailed:
		msg := "✗ failed"
		if m.connState.err != nil {
			msg += ": " + m.connState.err.Error()
		}
		return barInk(styleError).Render(msg)
	default:
		return barInk(styleMuted).Render("… connecting")
	}
}

// pickerStatus is the status of the Namespaces and Pods bars: the search status, the
// error and a flash. A picker has no connection to the session API, so no connection state.
func (m *model) pickerStatus() string {
	var parts []string
	if st := m.searchStatus(m.pane); st != "" {
		parts = append(parts, barInk(styleWarn).Render(st))
	}
	if m.pickerErr != "" {
		parts = append(parts, barInk(styleError).Render("error: "+m.pickerErr))
	}
	if m.flash != "" && (m.flashSticky || time.Now().Before(m.flashUntil)) {
		parts = append(parts, barInk(styleTitle).Render(m.flash))
	}
	return strings.Join(parts, barInk(styleMuted).Render("   "))
}

// bottomBar lays hints out on the left and status on the right of one line of width columns.
// hints is helpView(); status is the styled status, already in bar ink.
//
// The priority when the line is short, from the first to keep: [?] keys [q] quit, then the
// head of the status (it is cut from the RIGHT, so the connection state goes last), then the
// other hints, which fitHintLine drops from the left. The status gets at most what the line
// has after "… [?] keys  [q] quit" and a gap, so it never pushes those two out.
func bottomBar(hints, status string, width int) string {
	if width <= 0 {
		if status == "" {
			return renderKeyBar(hints, 0)
		}
		return renderKeyBar(hints, 0) + "  " + status
	}
	statusRoom := width - lipgloss.Width("… "+barEssentialHints) - 2
	if statusRoom < 1 {
		status = ""
	} else if lipgloss.Width(status) > statusRoom {
		status = truncToWidth(status, statusRoom)
	}
	if status == "" {
		bar := renderKeyBar(fitHintLine(hints, width), width)
		if colorOn() {
			return bar
		}
		// renderKeyBar does not pad under Ascii; the bar must still be width columns.
		return bar + strings.Repeat(" ", max(width-lipgloss.Width(bar), 0))
	}
	left := renderKeyBar(fitHintLine(hints, width-lipgloss.Width(status)-2), 0)
	gap := strings.Repeat(" ", max(width-lipgloss.Width(left)-lipgloss.Width(status), 0))
	if colorOn() {
		gap = styleBar.Render(gap)
	}
	return left + gap + status
}

// fitFlashLine bounds a full-width flash to the terminal, truncating from the
// LEFT so the tail survives. For a path the tail is the filename, which is what
// the user retypes or completes against; the leading directories are the
// guessable part, and the README states the directory anyway.
func fitFlashLine(flash string, width int) string {
	if width <= 0 || lipgloss.Width(flash) <= width {
		return flash
	}
	const ell = "…"
	budget := width - lipgloss.Width(ell)
	if budget <= 0 {
		return ell
	}
	// Walk backwards accumulating DISPLAY COLUMNS, not runes. An earlier version
	// computed the budget in columns and then sliced by rune index, which
	// overflowed on any wide character — a CJK path asked to fit 40 columns
	// rendered 55, because each rune it kept was two columns wide.
	r := []rune(flash)
	used := 0
	i := len(r)
	for i > 0 {
		w := lipgloss.Width(string(r[i-1]))
		if used+w > budget {
			break
		}
		used += w
		i--
	}
	return ell + string(r[i:])
}

// fitHintLine trims a footer hint line to the terminal width, dropping whole
// hints from the FRONT.
//
// The events footer runs ~135 columns, so an 80-column terminal simply lost the
// tail — and the tail is where "[?] keys" and "[q] quit" live, the pair a stuck
// user reaches for. helpView orders its hints so the essential ones come last;
// this is the half that makes that ordering mean something, by cutting the other
// end.
//
// A leading "…" marks the cut, so a missing hint reads as "there are more" rather
// than as a key that does not exist. The [?] overlay is the complete reference.
func fitHintLine(hint string, width int) string {
	if width <= 0 || lipgloss.Width(hint) <= width {
		return hint
	}
	const sep = "  "
	const ellipsis = "… "

	parts := strings.Split(hint, sep)
	// Drop from the front until it fits, always keeping the last hint: one clipped
	// hint beats a bare ellipsis.
	for i := 0; i < len(parts)-1; i++ {
		candidate := ellipsis + strings.Join(parts[i+1:], sep)
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	// Even the last hint alone is too wide. Truncate it rather than return a line
	// that overflows and wraps, which would push a row of the table off-screen.
	last := parts[len(parts)-1]
	if lipgloss.Width(last) <= width {
		return last
	}
	return truncToWidth(last, width)
}
