package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
)

// minColumnWidth is the floor a column will not shrink below: enough for a character and the
// truncation ellipsis bubbles appends, so a squeezed column still says something.
const minColumnWidth = 4

// rightAlignHeader right-aligns a column heading inside its own width, so it sits over the
// digits of the right-aligned cells below it rather than over the column's left edge.
//
// THE TITLE STRING IS THE ONLY LEVER a single column has. bubbles' Column is {Title, Width}
// with no alignment field, and table.Styles carries ONE Header style for the whole header row —
// so "right-align this column's heading and leave the text columns alone" cannot be expressed
// as a style. headersView renders runewidth.Truncate(Title, Width, "…") inside a lipgloss box
// of exactly Width, and Truncate spares a string whose display width equals the limit, so a
// title pre-padded to Width arrives intact and lands flush against the right edge — which is
// where padLeft leaves the last digit of every cell beneath it.
//
// Without this the numeric columns labelled themselves from the far side of the column. The
// cells were right-aligned deliberately, so figures could be compared down a column; the
// headers were simply never told, and pointed at nothing:
//
//	EVENTS    TOKENS      COST       SAVED~      ACTIVE
//	     4      629.4k      $0.25       $0.01  ●
//
// Applied against the width the column will RENDER at, which for the sessions table is its
// fitted width and not its declared one: fitTableColumns shrinks columns on a narrow terminal,
// and a title padded to a width the column no longer has is a title bubbles truncates — which
// costs the heading its last letter instead of aligning it. The events table's fitColumns only
// ever DROPS whole columns, and its content sizing (sizedColumns) narrows the eventColumn itself
// before tableColumns sees it, so there eventColumn.width already is the rendered width and
// tableColumns passes it directly.
func rightAlignHeader(title string, width int) string { return padLeft(title, width) }

// headerTitle is a column's NAME, with any alignment padding rightAlignHeader added stripped
// back off.
//
// Every lookup by title goes through this. The sessions table addresses its columns by name in
// half a dozen places — "which width did COST get fitted to", "is TOKENS still legible" — and
// a padded title silently matches none of them: the money columns would read as absent and be
// rendered with a zero budget. Trimming here keeps the name the key and the padding a
// rendering detail, which is what it is.
//
// The events table's sort glyph is NOT stripped: it rides at the name's right edge, so a
// caller comparing "TIME" against a sorted header sees "TIME▲" either way. Nothing addresses
// the events columns by title — they have eventColumnIDs — so there is nothing here to fix.
//
// inexactMarker IS stripped, for exactly the reason the padding is. The sessions table's SAVED
// heading carries it — the saving is an estimate, and that is a property of the column rather
// than of any row, so it is stated once above them (see sessionMoneyCell). Which makes "SAVED~"
// the rendered heading while "SAVED" is still the NAME every lookup uses, and the four callers
// that key on the literal string — sessionsRightAligned, sessionsColumnWidth,
// sessionsShowMoney and sessionsColumnsFor — would otherwise all miss it at once. The failure
// mode is not a wrong heading: a money column that reads as absent is rendered against a zero
// budget, and every charge in it comes out as "—".
//
// A suffix, so it composes with the padding in either order: TrimSpace then TrimSuffix handles
// an aligned "    SAVED~", and a bare "SAVED~" too.
func headerTitle(c table.Column) string {
	return strings.TrimSuffix(strings.TrimSpace(c.Title), inexactMarker)
}

// tableWidth is the width a table with these columns actually renders at: columnsWidth for
// []table.Column instead of []eventColumn, charging the same cellPadding per column.
//
// Zero-width columns are skipped because renderRow skips them, so counting their padding would
// overstate the total and shrink the others for nothing.
func tableWidth(cols []table.Column) int {
	w := 0
	for _, c := range cols {
		if c.Width <= 0 {
			continue
		}
		w += c.Width + cellPadding
	}
	return w
}

// fitTableColumns returns cols narrowed to fit a terminal termWidth columns wide.
//
// This is issue #866's failure mode in the tables that fitColumns does not cover. That fix
// taught the EVENTS table to fit itself; the sessions, pipeline and catalog tables kept the
// fixed widths their constructors declare — the sessions table's comment even promised "widths
// are refined later by layout() based on terminal width", which layout() never did. So the
// sessions pane, the first screen an operator sees, rendered 90 columns wide on every terminal
// including the 80-column default: every row wrapped onto a second screen line, which
// scrambled the columns and broke the height budget at the same time, because the budget
// counts rows while the terminal counts lines.
//
// Shrinks the widest column first, one column at a time, so the space comes out of whichever
// field has the most to give (the session ID, the plugin name) instead of being spread evenly
// across fields that are already tight. Stops when nothing can give without crossing
// minColumnWidth: below that the terminal is simply too narrow, and a table that lies about
// its content is worse than one that overflows visibly.
//
// A copy, never the caller's slice: these come from package-level constructors and mutating
// them in place would make the fit permanent and cumulative across resizes.
func fitTableColumns(cols []table.Column, termWidth int) []table.Column {
	out := make([]table.Column, len(cols))
	copy(out, cols)
	if termWidth <= 0 {
		return out
	}
	for tableWidth(out) > termWidth {
		widest, at := minColumnWidth, -1
		for i, c := range out {
			if c.Width > widest {
				widest, at = c.Width, i
			}
		}
		if at < 0 {
			break
		}
		out[at].Width--
	}
	return out
}
