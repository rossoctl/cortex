package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// footerView renders the bottom two lines: status (connection + rate + drops
// + optional transient flash) and a context-sensitive keybinding hint. No
// lipgloss borders; parent view handles the frame.
func (m *model) footerView() string {
	var status strings.Builder

	// A sticky flash gets the whole line, starting at column 0.
	//
	// Yank is the case this exists for: the path is the longest thing the footer
	// ever carries, and appending it after the ~32 columns of connection state,
	// rate and drops pushed it off the right edge on a narrow terminal — the user
	// saw "yanked → /Users/you/.cortex/abctl-" and could not read the filename,
	// which is the whole point of showing it. Dropping the prefix while the notice
	// is up buys those columns back; the prefix returns on the next keypress, and
	// a sticky flash is by definition something the user just asked for and is
	// reading right now.
	if m.flash != "" && m.flashSticky {
		return styleTitle.Render(fitFlashLine(m.flash, m.width)) + "\n" +
			styleHint.Render(fitHintLine(m.helpView(), m.width))
	}

	// Connection state dot.
	switch m.connState.phase {
	case connOpen:
		status.WriteString(styleOK.Render("● connected"))
	case connReconnecting:
		status.WriteString(styleWarn.Render(
			fmt.Sprintf("◐ reconnecting (attempt %d, next in %ds)",
				m.connState.attempt,
				int(time.Until(m.connState.nextRetry).Round(time.Second).Seconds()))))
	case connFailed:
		msg := "✗ failed"
		if m.connState.err != nil {
			msg += ": " + m.connState.err.Error()
		}
		status.WriteString(styleError.Render(msg))
	default:
		status.WriteString(styleMuted.Render("… connecting"))
	}

	status.WriteString(styleMuted.Render("  "))

	// Rate + drops.
	status.WriteString(styleMuted.Render(fmt.Sprintf("%.1f ev/s", m.rate)))
	status.WriteString(styleMuted.Render("   "))
	if m.drops > 0 {
		status.WriteString(styleWarn.Render(fmt.Sprintf("drops: %d", m.drops)))
	} else {
		status.WriteString(styleMuted.Render("drops: 0"))
	}
	if m.paused {
		status.WriteString(styleWarn.Render("   [paused]"))
	}

	// Flash message (e.g. "yanked → ~/.cortex/abctl-events/...").
	if m.flash != "" && (m.flashSticky || time.Now().Before(m.flashUntil)) {
		status.WriteString(styleTitle.Render("   " + m.flash))
	}

	hint := fitHintLine(m.helpView(), m.width)

	return status.String() + "\n" + styleHint.Render(hint)
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
