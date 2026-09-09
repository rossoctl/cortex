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

	// Flash message (e.g. "yanked → /tmp/...").
	if m.flash != "" && time.Now().Before(m.flashUntil) {
		status.WriteString(styleTitle.Render("   " + m.flash))
	}

	hint := fitHintLine(m.helpView(), m.width)

	return status.String() + "\n" + styleHint.Render(hint)
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
