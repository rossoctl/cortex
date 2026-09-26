package tui

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A COLUMN'S HEADER MUST SIT OVER ITS OWN VALUES.
//
// The numeric columns right-align their cells so digits line up between rows, and for a long
// time nothing told their headers: bubbles renders every header left-flush, so EVENTS sat
// eight columns from the 4 it labelled and TOKENS eleven from its figure. Reading down the
// sessions pane, the header row simply did not point at the numbers:
//
//	SESSION         UPDATED         EVENTS    TOKENS      COST        SAVED       ACTIVE
//	ecb7387f-bffd…  33s ago                4      629.4k     $0.2475    ~$0.0039  ●
//
// The tests below derive the expected header alignment from the CELLS rather than from a list
// of column names, so a new right-aligned column is covered the day it is added — including
// one whose cells are padded without its header being told, which is the defect itself.

// flush is which edge of its column a string is pressed against.
type flush int

const (
	flushNone  flush = iota // blank, or padded on both sides — says nothing about alignment
	flushLeft               // text, then padding
	flushRight              // padding, then text
	flushBoth               // fills the width exactly — consistent with either alignment
)

func (f flush) String() string {
	switch f {
	case flushLeft:
		return "left"
	case flushRight:
		return "right"
	case flushBoth:
		return "full-width"
	default:
		return "empty"
	}
}

// flushOf reports which edge s is pressed against within a field of the given display width.
//
// Measured in DISPLAY columns, not bytes: a TOKENS or COST cell can carry U+2212 (three bytes,
// one column) once a saving is attached, and a session id ends in "…".
func flushOf(s string, width int) flush {
	if strings.TrimSpace(s) == "" {
		return flushNone
	}
	lead := lipgloss.Width(s) - lipgloss.Width(strings.TrimLeft(s, " "))
	trail := lipgloss.Width(s) - lipgloss.Width(strings.TrimRight(s, " "))
	// A cell narrower than its column with no padding at all has been produced by something
	// that does not align — treat the slack as trailing, which is what bubbles will pad.
	if lead == 0 && trail == 0 && lipgloss.Width(s) < width {
		return flushLeft
	}
	switch {
	case lead == 0 && trail == 0:
		return flushBoth
	case lead == 0:
		return flushLeft
	case trail == 0:
		return flushRight
	default:
		return flushNone
	}
}

// assertHeadersMatchCells is the shared assertion: for every column, the header is pressed
// against the same edge its values are.
func assertHeadersMatchCells(t *testing.T, what string, cols []table.Column, rows []table.Row) {
	t.Helper()
	checked := 0
	for i, c := range cols {
		if c.Width <= 0 {
			continue
		}
		// The cells' own verdict, which is what the header has to agree with. Columns whose
		// every cell is blank or exactly column-width say nothing and are skipped.
		cell := flushNone
		for _, r := range rows {
			if i >= len(r) {
				t.Fatalf("%s: row has %d cells, column %d (%q) is past the end",
					what, len(r), i, c.Title)
			}
			switch f := flushOf(r[i], c.Width); f {
			case flushNone, flushBoth:
			default:
				if cell != flushNone && cell != f {
					t.Errorf("%s: column %q has both %s- and %s-aligned cells",
						what, headerTitle(c), cell, f)
				}
				cell = f
			}
		}
		if cell == flushNone {
			continue
		}
		checked++
		if got := flushOf(c.Title, c.Width); got != cell && got != flushBoth {
			t.Errorf("%s: column %q is %s-aligned in its cells but its header %q is %s — "+
				"the heading does not sit over the values it names",
				what, headerTitle(c), cell, c.Title, got)
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no column had an alignable cell, so nothing was asserted", what)
	}
}

// sessionsAlignModel is a two-row sessions pane with every numeric column populated: one live
// session carrying tokens and money, one bare so a blank money cell is in the mix too.
func sessionsAlignModel(width int) *model {
	now := time.Now()
	m := &model{
		pane:         paneSessions,
		width:        width,
		height:       40,
		bodyHeight:   12,
		events:       map[string][]pipeline.SessionEvent{},
		eventColumns: defaultColumnSelection(),
		sessions: []session.SessionSummary{{
			ID:         "ecb7387f-bffd-4172-adc4-8da23e992e9a",
			UpdatedAt:  now.Add(-33 * time.Second),
			EventCount: 4, TotalTokens: 629_400,
			CostMicros: 247_500, AvoidedMicros: 3_900, Active: true,
		}, {
			ID:        "default",
			UpdatedAt: now.Add(-56 * time.Second),
			// 105 against 4 above: two different digit counts is the case
			// right-alignment exists for.
			EventCount: 105,
		}},
	}
	m.sessionsTbl = newSessionsTable()
	m.eventsTbl = newEventsTable()
	m.rebuildSessionsTable()
	return m
}

func TestSessionsHeader_SitsOverItsOwnValues(t *testing.T) {
	// Every width the money columns survive at, plus a narrow one where they are dropped and
	// the fitter has squeezed the rest: the padding is computed against the FITTED width, so a
	// header padded to the declared 10 in a column fitted to 9 would be clipped by bubbles.
	for _, w := range []int{200, 118, 90, 80, 74, 60, 50} {
		m := sessionsAlignModel(w)
		assertHeadersMatchCells(t, "sessions@"+strconv.Itoa(w), m.sessionsTbl.Columns(), m.sessionsTbl.Rows())
	}
}

// eventsAlignModel is an events pane holding one full exchange: a request and the response
// that carries its token counts and its cost record, so DURATION, TOKENS and COST all render.
func eventsAlignModel(t *testing.T, width int) *model {
	t.Helper()
	const id = "align"
	now := time.Now()
	resp := pipeline.SessionEvent{
		At: now.Add(1200 * time.Millisecond), SessionID: id,
		Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		Host: "litellm.ai-models.svc.cluster.local", StatusCode: 200,
		Duration:  1230 * time.Millisecond,
		RequestID: "r1",
		Inference: &pipeline.InferenceExtension{
			Model: "claude-opus-5", InputTokens: 629_312, OutputTokens: 1_165,
		},
		Plugins: costRecord(t, costevent.Event{
			CostUSD: 0.2481, Settled: true, PromptUSD: 0.2460, OutputUSD: 0.0221,
			Avoided: []costevent.Saving{{Component: "tool-prune", TokensAvoided: 10_300, USD: 0.0039}},
		}),
	}
	req := pipeline.SessionEvent{
		At: now, SessionID: id,
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "litellm.ai-models.svc.cluster.local", RequestID: "r1",
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5"},
	}
	m := &model{
		pane: paneEvents, selectedSess: id,
		width: width, height: 40, bodyHeight: 12,
		events:       map[string][]pipeline.SessionEvent{id: {req, resp}},
		eventColumns: defaultColumnSelection(),
		sessions:     []session.SessionSummary{{ID: id}},
	}
	m.eventsTbl = newEventsTable()
	m.sessionsTbl = newSessionsTable()
	m.rebuildEventsTable()
	return m
}

// costRecord attaches a cost record the way the proxy does, so costCell has a figure to show.
func costRecord(t *testing.T, ev costevent.Event) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]json.RawMessage{costevent.Key: raw}
}

func TestEventsHeader_SitsOverItsOwnValues(t *testing.T) {
	for _, w := range []int{200, 140, 118, 100, 80} {
		m := eventsAlignModel(t, w)
		assertHeadersMatchCells(t, "events@"+strconv.Itoa(w), m.eventsTbl.Columns(), m.eventsTbl.Rows())
	}
}

// And with a sort glyph on a right-aligned column: the marker rides at the name's right, so it
// must not push the name out of the column or break the alignment.
func TestEventsHeader_SitsOverItsValuesWhileSorted(t *testing.T) {
	for _, desc := range []bool{false, true} {
		m := eventsAlignModel(t, 140)
		m.sortCol, m.sortDesc = colTokens, desc
		m.rebuildEventsTable()
		assertHeadersMatchCells(t, "events sorted", m.eventsTbl.Columns(), m.eventsTbl.Rows())
	}
}

// THE RENDERED PANE, not just the strings handed to bubbles. bubbles truncates each header
// with runewidth.Truncate and renders it inside a lipgloss box of exactly the column width, so
// a padded title could in principle come back clipped or re-flowed — in which case the fix
// above would be invisible on screen while every assertion on Title still passed.
func TestHeaderAlignment_SurvivesRendering(t *testing.T) {
	m := sessionsAlignModel(118)
	cols := m.sessionsTbl.Columns()
	lines := strings.Split(stripANSI(m.sessionsTbl.View()), "\n")
	if len(lines) < 3 {
		t.Fatalf("rendered %d lines, want a header and two rows:\n%s", len(lines), m.sessionsTbl.View())
	}
	// Each column occupies Width+2 rendered columns — bubbles' Cell and Header styles both
	// carry Padding(0, 1) — with the content in the middle Width of them. The offset advances
	// at the END of each iteration: sliced with it already advanced, every cell was read out of
	// the NEXT column and the failures named the wrong values.
	off := 0
	checked := 0
	for i, c := range cols {
		if c.Width <= 0 {
			continue
		}
		slice := func(ln string, at int) string {
			r := []rune(ln)
			from, to := at+1, at+1+c.Width
			if from > len(r) {
				return ""
			}
			if to > len(r) {
				to = len(r)
			}
			return string(r[from:to])
		}
		head := slice(lines[0], off)
		start := off
		off += c.Width + cellPadding
		if !sessionsRightAligned[headerTitle(c)] {
			continue
		}
		for _, ln := range lines[1:3] {
			cell := slice(ln, start)
			if strings.TrimSpace(cell) == "" {
				continue
			}
			checked++
			// Both end at the column's right edge, which is what a reader follows down.
			if hEnd, cEnd := lastInk(head), lastInk(cell); hEnd != cEnd {
				t.Errorf("column %d (%q): header ends at offset %d, value %q ends at %d — "+
					"they do not line up on screen\n%s",
					i, headerTitle(c), hEnd, strings.TrimSpace(cell), cEnd, strings.Join(lines[:3], "\n"))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no numeric column was compared, so this asserted nothing")
	}
}

// lastInk is the display offset just past the last non-space rune of s, or -1 when blank.
func lastInk(s string) int {
	trimmed := strings.TrimRight(s, " ")
	if trimmed == "" {
		return -1
	}
	return lipgloss.Width(trimmed)
}

// TestSessionsRightAligned_EveryNameResolvesToAColumn keeps the set honest, so that a renamed
// column cannot quietly lose its RENDER-LEVEL coverage.
//
// The two alignment tests here fail differently on a stale entry, and the difference is the
// whole reason this exists:
//
//   - assertHeadersMatchCells derives the expected alignment from the CELLS and consults no
//     name list, so it catches a renamed column loudly. Verified by mutation: renaming the
//     SAVED entry fails TestSessionsHeader_SitsOverItsOwnValues at every width.
//   - TestHeaderAlignment_SurvivesRendering skips columns absent from sessionsRightAligned, so
//     the same rename SILENTLY drops that column out of it — and that is the test which slices
//     the rendered View() rather than inspecting Title, precisely because "every assertion on
//     Title would keep passing if bubbles clipped or re-flowed the padding on the way to the
//     screen". Losing it costs the one check that looks at what reaches the terminal.
//
// So the exposure is a loss of coverage rather than a green suite over a broken heading. Worth
// closing anyway, because it is invisible: the suite stays green, the column count checked
// quietly drops by one, and nothing says so.
//
// NOT HYPOTHETICAL. This branch renames SAVED to carry headerMarker, which is exactly that
// shape — it stays covered only because headerTitle strips the marker before the lookup. This
// test makes that a checked property rather than a coincidence.
func TestSessionsRightAligned_EveryNameResolvesToAColumn(t *testing.T) {
	cols := sessionsColumns()
	for name := range sessionsRightAligned {
		found := false
		for _, c := range cols {
			if headerTitle(c) == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sessionsRightAligned names %q, which is not a column in sessionsColumns(). "+
				"A name that resolves to nothing exempts that column from "+
				"assertHeadersMatchCells — which skips columns absent from this set — so its "+
				"heading can stop sitting over its values with the whole suite still green.",
				name)
		}
	}
	// And the reverse direction for the money columns specifically, because they are the ones a
	// marker can rename: every column whose CELLS are right-aligned has to be in the set.
	for _, c := range cols {
		switch headerTitle(c) {
		case "EVENTS", "TOKENS", "COST", "SAVED":
			if !sessionsRightAligned[headerTitle(c)] {
				t.Errorf("column %q has right-aligned cells but is not in sessionsRightAligned, "+
					"so its heading is left-flushed and assertHeadersMatchCells skips it",
					headerTitle(c))
			}
		}
	}
}
