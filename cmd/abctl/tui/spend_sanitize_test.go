package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// esc is the ASCII escape character, built rather than written as a literal so this file carries
// no control byte of its own.
const esc = "\x1b"

// A SERVER-SUPPLIED WINDOW LABEL REACHES THE TERMINAL, so it must not be able to carry escape
// sequences or newlines. CWE-150 — improper neutralization of escape sequences — in a TUI: a
// proxy answering {"window":"<ESC>[2J1h"} could clear the screen, and one answering a newline
// could add a line to a two-row reservation and push the footer off the bottom of the terminal.
//
// THESE TESTS EXISTED AND WERE DELETED AS COLLATERAL.
// TestSpendSummary_SanitisesTheServersWindowLabel and its escape-sequence sibling went out in a
// sweep aimed at TestRenderSpendStrip_*, because they were named TestSpendSummary_*. The
// PROPERTY outlived renderSpendStrip: sanitizeLabel is live on the drawer's caption, on the
// band's per-span served-window comparison, and on the drawer's error text. Nothing else in the
// package feeds a control character into a spend label, so deleting any of those calls breaks no
// other test — which is what made the loss invisible.
//
// Asserted THROUGH THE RENDERED SURFACES rather than on sanitizeLabel directly. The helper is
// covered by construction; what regressed is the CALL, and a test on the helper alone would keep
// passing if a call site dropped it.
func TestSpendLabels_NeutraliseAServerSuppliedControlCharacter(t *testing.T) {
	for _, tc := range []struct{ name, window string }{
		{"escape sequence", esc + "[2J1h"},
		{"newline", "1h\nEXTRA"},
		{"carriage return", "1h\rEXTRA"},
		{"DEL", "1h" + string(rune(0x7f))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// THE AXIS TOO, which is the same exposure through the other field: snap.Group reaches
			// drawerHeaders, which renders "BY " + ToUpper(axis) and clips it — and clipping
			// shortens a row without neutralising anything in it. Measured before the fix: a Group
			// of "model\x1b[2J\x1b[H" came out as "   BY MODEL\x1b[2J\x1b[H" with the
			// clear-screen intact, and a newline returned two rows against a one-row reservation.
			ax := &model{width: 120}
			ax.spend.drawer.snap = drawerSnap()
			ax.spend.drawer.snap.Group = usage.Group("model" + tc.window)
			axAxis, axWindow := ax.drawerLabels()
			assertNoControlChars(t, "drawer axis", string(axAxis))
			for _, line := range renderSpendDrawer(ax.spend.drawer.snap, nil, axAxis, axWindow, 120) {
				assertNoControlChars(t, "drawer line (axis)", line)
			}
			if n := len(renderSpendDrawer(ax.spend.drawer.snap, nil, axAxis, axWindow, 120)); n != spendDrawerLines {
				t.Errorf("a tampered axis returned %d rows, want %d — the reservation is the height",
					n, spendDrawerLines)
			}

			// The drawer's caption prints the window the SERVER said it served.
			m := &model{width: 120}
			m.spend.drawer.snap = drawerSnap()
			m.spend.drawer.snap.Window = tc.window
			axis, window := m.drawerLabels()
			assertNoControlChars(t, "drawer caption", window)

			// And the whole rendered drawer, so a second path cannot reintroduce it.
			for _, line := range renderSpendDrawer(m.spend.drawer.snap, nil, axis, window, 120) {
				assertNoControlChars(t, "drawer line", line)
			}

			// THE BAND NEVER PRINTS THE SERVER'S LABEL, so its side of this is a different
			// assertion — and the previous version of this claimed otherwise ("a control
			// character travels into that comparison and onto the cell") and checked the band
			// lines for control characters, which no band cell can contain whatever the server
			// sends: bandSpanCell builds its label from spendSpanDefs and its value from
			// moneyAmount, and snap.Window reaches neither.
			//
			// What the band DOES with a tampered window is refuse the span: the served label
			// cannot match the requested one, so the reading is Unanswerable and the cell is an
			// em dash. That is the assertion worth making, because the alternative — drawing the
			// figure under the requested label — publishes a number for a window nobody served.
			// ALL FOUR CHAINS ANSWER, and only the hour's label is tampered with. An earlier version
			// populated the hour alone, which left the other three spans nil — already em dashes for
			// want of a snapshot — so "the band contains an em dash" was true before the tampering
			// and the assertion could not fail. Counted instead, so the ONE refusal has to be the
			// tampered span and the other three have to still be figures.
			// The hour's figure is UNIQUE so the check below names one span: with every span at the
			// same amount, "the band does not contain $1.00" would fail on its honest neighbours.
			b := &model{}
			for span := spendSpan(0); span < numSpendSpans; span++ {
				micros := int64(7_770_000)
				if span == spanHour {
					micros = 1_000_000
				}
				b.spend.chains[span].snap = &usage.Snapshot{
					Window: string(spendSpanDefs[span].window),
					Totals: usage.Counts{Requests: 1, CostMicros: micros, PricedRequests: 1, PriceableRequests: 1},
					Priced: true,
				}
			}
			b.spend.chains[spanHour].snap.Window = tc.window
			lines := renderSpendBand(spendSummary{Spans: b.spanReadings()}, 200)
			for _, line := range lines {
				assertNoControlChars(t, "band line", line)
			}
			if n := strings.Count(lines[0], emptyCell); n != 1 {
				t.Errorf("%d cells are %q, want exactly 1 — the tampered span and no other:\n%s",
					n, emptyCell, strings.Join(lines, "\n"))
			}
			if strings.Contains(lines[0], "$1.00") {
				t.Errorf("the band drew the figure under the requested label for a window the "+
					"server did not serve:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// A failed drawer fetch prints the error, and a transport error can carry the server's own bytes
// — the same exposure one layer over.
func TestRenderSpendDrawer_NeutralisesControlCharsInItsError(t *testing.T) {
	err := errors.New("unexpected status 500: " + esc + "[2Jgone\nsecond line")
	for _, line := range renderSpendDrawer(nil, err, usage.GroupModel, "MONTH", 120) {
		assertNoControlChars(t, "drawer error line", line)
	}
}

// assertNoControlChars fails on any C0 control character or DEL — exactly the set sanitizeLabel
// replaces with U+FFFD. One display column per replacement, so the width arithmetic every one of
// these surfaces depends on still holds.
func assertNoControlChars(t *testing.T, where, s string) {
	t.Helper()
	for i, r := range s {
		if r == 0x7f || r < 0x20 {
			t.Errorf("%s: control character %#U at byte %d in %q — a server-supplied label "+
				"reached the terminal unsanitised", where, r, i, s)
			return
		}
	}
}

// A WIDE-RUNE ERROR MESSAGE MUST NOT OVERFLOW THE DRAWER'S RESERVATION, which is the other half of
// "the server's bytes reach this row" — and the half sanitizeLabel cannot cover, because a wide
// rune is not a control character.
//
// clipRow counted RUNES: len([]rune(row)) <= width passes for a string of CJK that renders at twice
// that, and slicing by rune index then produces a line wider than the budget it was clipped to.
// Measured before the fix: 45 display columns against a 40-column budget. A line over budget wraps,
// which costs a row the reservation does not have, so spendDrawerLines stops being the height and
// the footer goes off the bottom of the terminal.
//
// The message is SERVER-SUPPLIED — err.Error() is rendered verbatim — so this is reachable from the
// other end of the connection, not just from a wide locale.
func TestRenderSpendDrawer_AWideErrorMessageStaysInsideTheReservation(t *testing.T) {
	wide := errors.New("unexpected status 500: " + strings.Repeat("過", 40))
	// LITERALS, NOT spendDrawerLinesFor(width). The error path pads to
	// spendDrawerLinesFor(width)-1 and appends one hint line, so comparing against that
	// same function compares the renderer with its own padding rule and cannot fail —
	// the constant this used to name was an independent witness and this restores one.
	//
	// 6 below spendDrawerTwoColumnMin (86) and 7 at or above it: the tier column, and
	// with it the reasoning child's row, only exists in two columns. 120 is the only
	// width here that reaches it.
	for _, tc := range []struct {
		width, wantLines int
	}{{20, 6}, {40, 6}, {72, 6}, {120, 7}} {
		width := tc.width
		lines := renderSpendDrawer(nil, wide, usage.GroupModel, "MONTH", width)
		if len(lines) != tc.wantLines {
			t.Errorf("width %d: %d lines, want %d — the reservation is the height, so an extra "+
				"line pushes the footer off the bottom", width, len(lines), tc.wantLines)
		}
		// And the reservation layout() holds back must agree with what was emitted.
		if got := spendDrawerLinesFor(width); got != tc.wantLines {
			t.Errorf("width %d: reservation is %d but the render is %d lines", width, got, tc.wantLines)
		}
		for i, line := range lines {
			if n := lipgloss.Width(line); n > width {
				t.Errorf("width %d: line %d renders %d display columns: a rune count would have "+
					"passed this and wrapped:\n%q", width, i, n, line)
			}
			if strings.Contains(line, "\n") {
				t.Errorf("width %d: line %d carries a newline", width, i)
			}
		}
	}
}
