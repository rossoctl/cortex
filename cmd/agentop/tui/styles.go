// Package tui implements the agentop Bubble Tea interactive terminal UI.
package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/rossoctl/cortex/cmd/agentop/tui/table"
)

// Palette keeps all colors in one place so recoloring the TUI is a single
// file edit. Colors are chosen to render legibly on both light and dark
// terminals (Bubble Tea's ANSI adaptive palette) — avoid 24-bit colors here.
var (
	colorAccent   = lipgloss.AdaptiveColor{Light: "#4F46E5", Dark: "#A5B4FC"}
	colorOK       = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#6EE7B7"}
	colorWarn     = lipgloss.AdaptiveColor{Light: "#92400E", Dark: "#FCD34D"}
	colorError    = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"}
	colorMuted    = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	colorInbound  = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#93C5FD"}
	colorOutbound = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"}

	// colorOnSeries is the foreground for a letter drawn on a series background
	// (the usage pane's stacked bars). Inverted relative to the palette: those
	// backgrounds are mid-tone in both themes, so the mark needs the opposite end
	// of the ramp to stay legible — white on the darker light-theme grounds, near
	// black on the lighter dark-theme ones.
	colorOnSeries = lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#111827"}

	// colorSelectedBg is the row a table's cursor is on — see tableStyles for why selection is
	// a tint rather than a reverse.
	//
	// A BACKGROUND, so the cells' own ink is left alone: the sessions table's context gauge
	// encodes its value as ink, and anything that swaps ink for background reads it backwards.
	// Mid-tone in both themes for the same reason colorOnSeries is inverted relative to the
	// palette — it has to sit under default-foreground text without hiding it.
	colorSelectedBg = lipgloss.AdaptiveColor{Light: "#D7D7FF", Dark: "#303050"}

	colorDim     = lipgloss.AdaptiveColor{Light: "#9CA3AF", Dark: "#4B5563"} // dividers, " · ", "—"
	colorZebraBg = lipgloss.AdaptiveColor{Light: "#F3F4F6", Dark: "#15151C"} // every second table row
	colorChipBg  = lipgloss.AdaptiveColor{Light: "#E5E7EB", Dark: "#374151"} // key chip ground
	colorChipFg  = lipgloss.AdaptiveColor{Light: "#111827", Dark: "#F3F4F6"} // key chip text
	colorTool    = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#F59E0B"} // agent tool rows
)

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleHint   = lipgloss.NewStyle().Foreground(colorMuted)
	styleOK     = lipgloss.NewStyle().Foreground(colorOK)
	styleWarn   = lipgloss.NewStyle().Foreground(colorWarn)
	styleError  = lipgloss.NewStyle().Foreground(colorError)
	styleMuted  = lipgloss.NewStyle().Foreground(colorMuted)
	styleBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorAccent)

	styleNone       = lipgloss.NewStyle()
	styleDim        = lipgloss.NewStyle().Foreground(colorDim)
	styleBold       = lipgloss.NewStyle().Bold(true)
	styleChip       = lipgloss.NewStyle().Foreground(colorChipFg).Background(colorChipBg)
	styleSeg        = lipgloss.NewStyle().Foreground(colorOnSeries).Background(colorAccent).Bold(true)
	styleTool       = lipgloss.NewStyle().Foreground(colorTool)
	styleAccent     = lipgloss.NewStyle().Foreground(colorAccent)
	styleAccentBold = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleMutedBold  = lipgloss.NewStyle().Foreground(colorMuted).Bold(true)
	styleInbound    = lipgloss.NewStyle().Foreground(colorInbound)
	styleOutbound   = lipgloss.NewStyle().Foreground(colorOutbound)

	// styleMatch and styleCurrentMatch draw the characters a search matched: a background,
	// so a match reads the same whatever ink the text under it has (the JSON colorizer's,
	// a table cell's), and colorOnSeries on it for the reason that colour exists — the warn
	// and accent grounds are mid-tone in both themes. The current one, the row or line `n`
	// moved to, is the accent, so it stands out from the rest.
	styleMatch        = lipgloss.NewStyle().Foreground(colorOnSeries).Background(colorWarn)
	styleCurrentMatch = lipgloss.NewStyle().Foreground(colorOnSeries).Background(colorAccent).Bold(true)

	// PER-PROTOCOL CELL COLOURING USED TO LIVE HERE — styleProtoA2A / MCP / Inference / Blocked
	// and a protoStyle() dispatcher — and is deleted rather than kept for later. It had no
	// callers anywhere outside this file (events_pane.go records the change that removed the
	// last one), and while it sat here unused it was cited as the reason the selected row had
	// to be reverse video, which is the bug below. A palette nothing renders cannot justify a
	// constraint on what everything renders; restore it from git with its caller if the events
	// pane wants colour again.
)

// tableStyles returns the standard agentop table palette — layered on top of
// bubbles' DefaultStyles so cell padding, borders, and other layout rules
// come through unchanged.
//
// SELECTION IS A BACKGROUND TINT, NOT A REVERSE, and the reason is the sessions table's
// CTX(1M) gauge. bubbles wraps the whole row in Selected.Render, so reverse video swapped
// ink and background for every cell in it — and a gauge encodes its value AS ink. The filled
// blocks rendered in the background colour (reading as empty) while the blank track rendered in
// the foreground (reading as filled), so the highlighted row showed 44% as ~56%, from the wrong
// side. Any density-encoded figure has the same problem; only position and pattern survive an
// inversion.
//
// The previous comment here said Reverse was deliberate, to keep per-cell protocol colouring from
// being clobbered by the row style. That colouring did not exist — the palette it named had no
// callers — so nothing was being protected, and the palette is now deleted rather than left to make
// the same claim again.
//
// Colouring the GAUGE instead was the first thing tried and is not possible in this table: a
// styled cell carries escape bytes, runewidth.StringWidth counts them (an 11-column gauge
// measures 24), and renderRow truncates with runewidth BEFORE styling — so the cell comes back
// as a lone "…". Same mechanism as the mangled marker that
// TestSessionsPicker_CachedMarkerRendersIntact pins. bubbles v1.0.0 has no per-column style, so
// the row style is the only lever there is.
//
// What this costs: a terminal with no colour (the Ascii profile) drops the background and the
// selected row is left bold only, where reverse video would have survived. Accepted knowingly —
// a bold row is still marked, and a gauge that lies on the selected row is worse than a
// selection that is merely quieter in monochrome.
//
// ONE STYLE FOR EVERY TABLE. Scoping this to the sessions table would have left one pane
// tinting its selection and five inverting theirs.
//
// Per-column color now exists, and it keeps the argument above: the table applies a pane's
// cell style after it cuts the cell to width, and a cell style sets only ink and bold, never
// Reverse or a background. So the gauge's filled blocks keep their ink (accent) on the tinted
// row, and its brackets are dim through Glyphs. Zebra rows (Alt) are a background on rows the
// cursor is not on, and the bar (Bar) replaces the cursor row's blank left pad.
func tableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		Foreground(colorMuted).
		BorderForeground(colorMuted).
		Bold(true)
	s.Selected = lipgloss.NewStyle().Bold(true).Background(colorSelectedBg)
	// A search's matched characters, and a row whose only match is somewhere no cell shows.
	// The mark is ink rather than a background, so a marked row never reads as the cursor,
	// in the warn colour the footer's search status is drawn in. Recolouring ink is safe for
	// the context gauge, unlike inverting it: the gauge's value is which cells are filled.
	s.Match = styleMatch
	s.CurrentMatch = styleCurrentMatch
	s.Marked = lipgloss.NewStyle().Foreground(colorWarn)
	s.Alt = lipgloss.NewStyle().Background(colorZebraBg)
	s.Bar = lipgloss.NewStyle().Foreground(colorAccent)
	s.Glyphs = tableGlyphs
	return s
}

// tableGlyphs are the runes drawn in their own ink inside a styled cell: the CTX gauge's
// brackets and the BYTES arrows dim, the agents pane's open marks accent.
//
// shortcut: the gauge's one-eighth block is the same rune as its closing bracket (▏), so a fill
// under one eighth of a cell draws dim; give the gauge its own bracket rune if that matters.
var tableGlyphs = map[rune]lipgloss.Style{
	'▕': styleDim, '▏': styleDim, '↑': styleDim, '↓': styleDim,
	'▸': styleAccent, '▾': styleAccent,
}

// cellKind is the style a pane gives one of its columns.
type cellKind int

const (
	cellPlain cellKind = iota
	cellMuted
	cellBold
	cellOK
	cellAccent
)

var cellKindStyles = [...]lipgloss.Style{
	cellPlain: styleNone, cellMuted: styleMuted, cellBold: styleBold, cellOK: styleOK, cellAccent: styleAccent,
}

// kindsCellStyle styles each cell by its column's kind, kinds parallel to the table's columns.
func kindsCellStyle(kinds []cellKind) table.CellStyleFunc {
	return dashDim(func(_ table.Row, col int) lipgloss.Style {
		if col < len(kinds) {
			return cellKindStyles[kinds[col]]
		}
		return styleNone
	})
}

// pickerCellStyle is the Namespaces and Pods tables' style: the name bold, the rest muted.
var pickerCellStyle = dashDim(func(_ table.Row, col int) lipgloss.Style {
	if col == 0 {
		return styleBold
	}
	return styleMuted
})

// colorOn reports whether lipgloss emits color. Under Ascii (what go test gets) every
// helper below returns exactly what the code drew before it existed.
func colorOn() bool { return lipgloss.ColorProfile() != termenv.Ascii }

// hintChipRe is a key hint: [x] with x non-empty and no space or bracket in it.
var hintChipRe = regexp.MustCompile(`\[[^\[\]\s]+\]`)

type hintPart struct {
	text string
	chip bool // text is the key, brackets removed
}

// splitHintChips splits a plain hint line into key hints and the text between them.
// An unclosed "[" (a hint fitHintLine cut in half) stays plain text.
func splitHintChips(s string) []hintPart {
	var parts []hintPart
	last := 0
	for _, loc := range hintChipRe.FindAllStringIndex(s, -1) {
		if loc[0] > last {
			parts = append(parts, hintPart{text: s[last:loc[0]]})
		}
		parts = append(parts, hintPart{text: s[loc[0]+1 : loc[1]-1], chip: true})
		last = loc[1]
	}
	if last < len(s) {
		parts = append(parts, hintPart{text: s[last:]})
	}
	return parts
}

// renderHints styles a hint line that is already cut to width. With color, each [x]
// becomes the chip " x " (same width); the rest is drawn in the hint style.
func renderHints(fitted string) string {
	if !colorOn() {
		return styleHint.Render(fitted)
	}
	var b strings.Builder
	for _, p := range splitHintChips(fitted) {
		if p.chip {
			b.WriteString(styleChip.Render(" " + p.text + " "))
		} else {
			b.WriteString(styleHint.Render(p.text))
		}
	}
	return b.String()
}

// titleMarkRe finds the parts of a title that are not bold default ink.
var titleMarkRe = regexp.MustCompile(` · |https?://\S+|\([^()]*\)`)

// renderTitle styles a pane title without changing its text: a leading "agentop" in
// accent bold, " · " dim, URLs and (…) groups muted, the rest bold.
func renderTitle(title string) string {
	if !colorOn() {
		return styleTitle.Render(title)
	}
	var b strings.Builder
	rest := title
	if r, ok := strings.CutPrefix(rest, "agentop"); ok {
		b.WriteString(styleAccentBold.Render("agentop"))
		rest = r
	}
	last := 0
	for _, loc := range titleMarkRe.FindAllStringIndex(rest, -1) {
		if loc[0] > last {
			b.WriteString(styleBold.Render(rest[last:loc[0]]))
		}
		m := rest[loc[0]:loc[1]]
		if m == " · " {
			b.WriteString(styleDim.Render(m))
		} else {
			b.WriteString(styleMuted.Render(m))
		}
		last = loc[1]
	}
	if last < len(rest) {
		b.WriteString(styleBold.Render(rest[last:]))
	}
	return b.String()
}

// colorGroup is the meaning a cell's value carries, which picks its ink.
type colorGroup int

const (
	groupDefault colorGroup = iota
	groupMuted
	groupOK
	groupWarn
	groupError
	groupAccent
	groupDim
)

// statusGroup classifies a STATUS cell. Cells are aligned, so spaces are trimmed first.
func statusGroup(cell string) colorGroup {
	v := strings.TrimSpace(cell)
	switch v {
	case "—":
		return groupDim
	case "ok":
		return groupOK
	case "running":
		return groupWarn
	case "lost", "error":
		return groupError
	}
	if len(v) == 3 && strings.Trim(v, "0123456789") == "" {
		switch v[0] {
		case '2':
			return groupOK
		case '4':
			return groupWarn
		case '5':
			return groupError
		}
	}
	return groupMuted
}

// actionGroup classifies an ACTION cell. A trailing "*" marks a shadow verdict of the
// same verb, so it takes the verb's group: a would-be deny still reads as a deny.
func actionGroup(cell string) colorGroup {
	switch strings.TrimSuffix(strings.TrimSpace(cell), "*") {
	case "modify":
		return groupAccent
	case "deny":
		return groupError
	case "tunnel", "—":
		return groupDim
	}
	return groupMuted
}

// groupStyle returns the package-level style for a group.
func groupStyle(g colorGroup) lipgloss.Style {
	switch g {
	case groupMuted:
		return styleMuted
	case groupOK:
		return styleOK
	case groupWarn:
		return styleWarn
	case groupError:
		return styleError
	case groupAccent:
		return styleAccent
	case groupDim:
		return styleDim
	}
	return styleNone
}

// dashDim draws a cell whose value is "—" (no value) dim, and every other cell with f.
func dashDim(f table.CellStyleFunc) table.CellStyleFunc {
	return func(row table.Row, col int) lipgloss.Style {
		if col < len(row) && strings.TrimSpace(row[col]) == "—" {
			return styleDim
		}
		return f(row, col)
	}
}
