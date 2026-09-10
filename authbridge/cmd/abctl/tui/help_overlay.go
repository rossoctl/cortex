package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

// keyBinding is one row in the help overlay: the key(s) and what they do.
type keyBinding struct {
	keys string
	desc string
}

// keyGroup is a titled block of bindings in the help overlay.
type keyGroup struct {
	title    string
	bindings []keyBinding
}

// globalKeys are the bindings that work in (nearly) every pane. `P` lives
// here because it's the discoverability problem this overlay exists to
// solve: it works from every session-view pane but was never shown in any
// footer. The picker panes are the documented exception — noted in the
// group title rather than duplicated per-pane.
var globalKeys = keyGroup{
	title: "GLOBAL",
	bindings: []keyBinding{
		{"?", "this help"},
		{"↑↓ / jk", "scroll this help"},
		{"P", "plugin catalog (session views)"},
		{"p", "pause/resume stream"},
		{"g / G", "jump to top / bottom"},
		{"b / f", "page up / down"},
		{"q · ctrl+c", "quit"},
	},
}

// paneKeys maps each pane to its own bindings, rendered first (and
// emphasized) when the overlay opens over that pane. Panes absent from
// this map fall back to the global group alone.
var paneKeys = map[paneID]keyGroup{
	paneNamespaces: {
		title: "NAMESPACES (this pane)",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵", "open namespace"},
			{"l", "connect to the local session API"},
			{"r", "reload agent list"},
			{"q · esc", "quit"},
		},
	},
	panePods: {
		title: "PODS (this pane)",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵", "port-forward + connect"},
			{"esc", "back to namespaces"},
			{"r", "reload agent list"},
		},
	},
	paneSessions: {
		title: "SESSIONS (this pane)",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵ / → / l", "drill into session"},
			{"tab", "switch to pipeline"},
			{"u", "usage charts (all sessions)"},
			{"/", "filter"},
			{"esc", "back to pods picker"},
		},
	},
	paneEvents: {
		title: "EVENTS (this pane)",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵ / → / l", "event detail"},
			{"/", "filter"},
			{"s", "toggle passthru/skip rows"},
			{"c", "column picker (checkboxes + descriptions)"},
			{"u", "usage charts (this session)"},
			{"esc / ← / h", "back to sessions"},
		},
	},
	paneDetail: {
		title: "EVENT DETAIL (this pane)",
		bindings: []keyBinding{
			{"↑↓", "scroll"},
			{"y", "yank event JSON to ~/.cortex/abctl-events"},
			{"u", "usage charts (this session)"},
			{"esc / ← / h", "back to events"},
		},
	},
	panePipeline: {
		title: "PIPELINE (this pane)",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵ / → / l", "plugin detail"},
			{"e", "edit pipeline in $EDITOR"},
			{"tab", "switch to sessions"},
			{"esc", "back to pods picker"},
		},
	},
	panePluginDetail: {
		title: "PLUGIN DETAIL (this pane)",
		bindings: []keyBinding{
			{"↑↓", "scroll"},
			{"esc / ← / h", "back"},
		},
	},
	paneUsage: {
		title: "USAGE (this pane)",
		bindings: []keyBinding{
			{"m", "cycle metric (tokens/requests/errors/latency)"},
			{"w", "cycle window (10m/1h/6h)"},
			{"b", "cycle breakdown (none/status/method/plugin; not for latency)"},
			{"s", "toggle session / all sessions"},
			{"esc", "back"},
		},
	},
	paneCatalog: {
		title: "PLUGIN CATALOG (this pane)",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵ / → / l", "plugin detail"},
			{"r", "refresh from /v1/plugins"},
			{"esc / ← / h", "back"},
		},
	},
}

// otherPaneOrder fixes the render order of the "OTHER PANES" section so
// the overlay is stable across openings (Go map iteration is random).
var otherPaneOrder = []paneID{
	paneNamespaces, panePods, paneSessions, paneEvents,
	paneDetail, paneUsage, panePipeline, panePluginDetail, paneCatalog,
}

// helpKeyColWidth is the fixed width of the key column so descriptions
// align into a readable second column across every group.
const helpKeyColWidth = 12

// renderKeyGroup renders one titled group. emphasize bolds the title and
// the key column — used for the pane the overlay was opened over.
func renderKeyGroup(g keyGroup, emphasize bool) string {
	var b strings.Builder
	titleStyle := styleHint
	keyStyle := styleMuted
	if emphasize {
		titleStyle = styleTitle
		keyStyle = styleOK
	}
	b.WriteString(titleStyle.Render(g.title))
	for _, kb := range g.bindings {
		keys := kb.keys
		if w := lipgloss.Width(keys); w < helpKeyColWidth {
			keys += strings.Repeat(" ", helpKeyColWidth-w)
		}
		b.WriteString("\n  " + keyStyle.Render(keys) + " " + styleHint.Render(kb.desc))
	}
	return b.String()
}

// helpBodyLines builds the scrollable body of the key-help overlay: the
// active pane's group (emphasized) first, then the global keys, then a
// one-line summary of every other pane. Returned as a single string so a
// viewport can page through it.
//
// The close hint is deliberately NOT included — it lives in the overlay's
// fixed footer so it can't be scrolled out of reach.
func helpBodyLines(pane paneID) string {
	var sections []string

	if g, ok := paneKeys[pane]; ok {
		sections = append(sections, renderKeyGroup(g, true))
	}
	sections = append(sections, renderKeyGroup(globalKeys, false))

	// Remaining panes, compacted to one line each so the overlay stays
	// scannable. The active pane is already rendered in full above.
	var others []string
	for _, p := range otherPaneOrder {
		if p == pane {
			continue
		}
		g, ok := paneKeys[p]
		if !ok {
			continue
		}
		keys := make([]string, 0, len(g.bindings))
		for _, kb := range g.bindings {
			keys = append(keys, kb.keys)
		}
		// Strip the "(this pane)" suffix the active-pane title carries.
		name := strings.TrimSuffix(g.title, " (this pane)")
		others = append(others, "  "+styleMuted.Render(padRight(name, 16))+" "+
			styleHint.Render(strings.Join(keys, "  ")))
	}
	if len(others) > 0 {
		sections = append(sections,
			styleHint.Render("OTHER PANES")+"\n"+strings.Join(others, "\n"))
	}

	return strings.Join(sections, "\n\n")
}

// helpBodyWidth is the natural (unwrapped) width of the help body, used
// to size the overlay before the terminal cap is applied.
func helpBodyWidth(body string) int {
	w := 0
	for _, ln := range strings.Split(body, "\n") {
		if lw := lipgloss.Width(ln); lw > w {
			w = lw
		}
	}
	return w
}

// helpOverlayFrameH is the number of rows the overlay frame costs beyond
// the scrollable body: top border, footer hint, bottom border.
const helpOverlayFrameH = 3

// helpViewportSize returns the width and height the help body's viewport
// should occupy for the given terminal size. Height leaves room for the
// frame; both are floored at 1 so a pathologically small terminal still
// renders something rather than panicking inside lipgloss.
func helpViewportSize(width, height, bodyW int) (int, int) {
	frameW := styleBorder.GetHorizontalBorderSize() + helpPadX*2
	w := bodyW
	if max := width - frameW; max > 0 && w > max {
		w = max
	}
	if w < 1 {
		w = 1
	}
	h := height - helpOverlayFrameH
	if h < 1 {
		h = 1
	}
	return w, h
}

// helpPadX is the overlay's horizontal padding inside its border.
const helpPadX = 2

// renderHelpOverlay draws the key-help panel around an already-scrolled
// viewport. The active pane's bindings come first and are emphasized;
// the global group follows; every other pane is summarized underneath so
// the overlay is a complete reference rather than a per-pane cheat sheet.
//
// vp must already hold the body content (see model.syncHelpViewport) —
// this function only frames it, so scroll position is owned by the model
// and survives re-renders.
//
// Returns the panel only — placement over the underlying view is the
// caller's job (see overlayCenter).
func renderHelpOverlay(vp viewport.Model, width, height int) string {
	// Scroll affordance: only shown when the body doesn't fit, so a
	// terminal tall enough for the whole reference stays uncluttered.
	// Degrades to the bare close hint when the viewport is too narrow for
	// the annotated form — a clipped "clos" is worse than no percentage.
	const closeHint = "[?] or [esc] close"
	hint := closeHint
	if vp.TotalLineCount() > vp.VisibleLineCount() {
		annotated := fmt.Sprintf("[↑↓] scroll  %d%%  ·  %s",
			int(vp.ScrollPercent()*100), closeHint)
		if lipgloss.Width(annotated) <= vp.Width {
			hint = annotated
		}
	}
	if lipgloss.Width(hint) > vp.Width {
		hint = truncToWidth(hint, vp.Width)
	}

	inner := vp.View() + "\n" + styleHint.Render(hint)

	// No Width()/MaxHeight() here: the viewport is already sized to the
	// terminal by helpViewportSize (which reserves helpOverlayFrameH rows
	// for this frame), so the block fits by construction. Setting Width
	// would make lipgloss re-wrap the pre-aligned key columns, and
	// MaxHeight would clip the footer hint that reservation exists to
	// protect. MaxWidth stays as a backstop against a terminal narrower
	// than one padded column.
	box := styleBorder.Padding(0, helpPadX)
	if width > styleBorder.GetHorizontalBorderSize() {
		box = box.MaxWidth(width)
	}
	return box.Render(inner)
}

// padRight pads s with spaces to n display columns. Narrower than n is
// left untouched.
func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// overlayCenter draws overlay centered on top of base, line by line,
// preserving the surrounding view. Both are treated as plain rendered
// blocks; the overlay's lines replace the base's at the computed offset.
//
// ANSI-aware only insofar as lipgloss.Width is used for column math —
// good enough because the overlay fully covers the columns it occupies,
// so no partial-escape splicing happens on the overlay's own rows.
func overlayCenter(base, overlay string, width, height int) string {
	baseLines := strings.Split(base, "\n")
	overLines := strings.Split(overlay, "\n")

	// Pad the base up to the terminal height so a short base view still
	// gets a centered overlay rather than one pinned to the top.
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}

	overH := len(overLines)
	overW := 0
	for _, l := range overLines {
		if w := lipgloss.Width(l); w > overW {
			overW = w
		}
	}

	top := (len(baseLines) - overH) / 2
	if top < 0 {
		top = 0
	}
	left := (width - overW) / 2
	if left < 0 {
		left = 0
	}
	// Never let centering push the panel past the right edge: if the panel
	// is wider than the terminal, render it flush-left and let MaxWidth in
	// renderHelpOverlay have already bounded it.
	if left+overW > width {
		left = width - overW
		if left < 0 {
			left = 0
		}
	}

	out := make([]string, len(baseLines))
	copy(out, baseLines)
	for i, ol := range overLines {
		row := top + i
		if row >= len(out) {
			break
		}
		// Rebuild the row as: base prefix (left columns) + overlay line.
		// Anything the overlay covers to the right is dropped — the panel
		// is opaque, and reconstructing a styled tail past an ANSI-laden
		// prefix is not worth the complexity for a modal.
		prefix := truncToWidth(out[row], left)
		if w := lipgloss.Width(prefix); w < left {
			prefix += strings.Repeat(" ", left-w)
		}
		out[row] = prefix + ol
	}
	return strings.Join(out, "\n")
}

// truncToWidth clips a possibly-ANSI-styled line to n display columns,
// closing any open style with a reset so the overlay that follows starts
// clean. Escape sequences are copied through without counting toward the
// width budget.
func truncToWidth(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	var b strings.Builder
	w := 0
	sawEscape := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			// Copy the whole escape sequence verbatim (through the final
			// byte of a CSI sequence, or a single byte otherwise).
			j := i + 1
			for j < len(s) && !isCSITerminator(s[j]) {
				j++
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:j])
			sawEscape = true
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := lipgloss.Width(string(r))
		if w+rw > n {
			break
		}
		b.WriteString(s[i : i+size])
		w += rw
		i += size
	}
	if sawEscape {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// isCSITerminator reports whether b ends an ANSI CSI escape sequence.
func isCSITerminator(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}
