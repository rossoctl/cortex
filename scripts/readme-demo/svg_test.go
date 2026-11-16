package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func toyStates() []State {
	return []State{
		{Label: "one", At: 0, Dur: 10 * time.Second, Rows: []Row{
			{Runs: []Run{{Text: "$ echo <hi> & 'bye'", FG: promptFG}},
				Reveal: Reveal{Kind: "type", Dur: 800 * time.Millisecond}},
			{Runs: []Run{{Text: "done", FG: defaultFG}}, Reveal: Reveal{Kind: "fill", At: time.Second}},
		}},
		{Label: "two", At: 10 * time.Second, Dur: 10 * time.Second, Final: true, Rows: []Row{
			{Runs: []Run{{Text: "SESSION", FG: titleFG, Bold: true}, {Text: " api", BG: "#303050"}}},
		}},
	}
}

func emit(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := EmitSVG(&buf, toyStates(), 20*time.Second, Grid{Cols: 40, Rows: 4}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestEmitSVG_IsWellFormedXML(t *testing.T) {
	d := xml.NewDecoder(strings.NewReader(emit(t)))
	for {
		_, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("not well-formed: %v", err)
		}
	}
}

func TestEmitSVG_EscapesMarkupInTerminalText(t *testing.T) {
	out := emit(t)
	if strings.Contains(out, "<hi>") {
		t.Error("raw < > reached the output; terminal text must be escaped")
	}
	if !strings.Contains(out, "&lt;hi&gt;") {
		t.Error("expected the escaped form of <hi>")
	}
	if !strings.Contains(out, "&amp;") {
		t.Error("expected & to be escaped")
	}
}

func TestEmitSVG_EveryTspanCarriesTextLength(t *testing.T) {
	// Without a forced advance the grid shears on any reader whose default
	// monospace differs from the author's, which for a table-heavy TUI is the
	// difference between a screenshot and a mess.
	for _, tspan := range regexp.MustCompile(`<tspan[^>]*>`).FindAllString(emit(t), -1) {
		if !strings.Contains(tspan, "textLength=") || !strings.Contains(tspan, `lengthAdjust="spacingAndGlyphs"`) {
			t.Errorf("tspan without forced metrics: %s", tspan)
		}
	}
}

// TestEmitSVG_StatesUseOpacityNotVisibility guards the bug that had every state
// drawn on top of every other: visibility is inheritable, but a descendant may
// re-declare `visible` and show through a hidden ancestor — and every row reveal
// does exactly that. opacity cannot be escaped that way.
func TestEmitSVG_StatesUseOpacityNotVisibility(t *testing.T) {
	out := emit(t)
	if !strings.Contains(out, "g.st{opacity:0}") {
		t.Error("state groups must default to opacity:0")
	}
	for _, kf := range regexp.MustCompile(`@keyframes st\d+\{[^}]*\}[^}]*\}`).FindAllString(out, -1) {
		if strings.Contains(kf, "visibility") {
			t.Errorf("state keyframes must animate opacity, not visibility: %s", kf)
		}
	}
}

// TestEmitSVG_StateWindowsTileTheTimeline checks that consecutive states hand off
// with no gap (a blank frame) and no overlap (two screens at once).
func TestEmitSVG_StateWindowsTileTheTimeline(t *testing.T) {
	out := emit(t)
	re := regexp.MustCompile(`@keyframes st(\d+)\{(.*?)\}\n`)
	matches := re.FindAllStringSubmatch(out, -1)
	if len(matches) != 2 {
		t.Fatalf("want 2 state keyframe blocks, got %d", len(matches))
	}
	pctRe := regexp.MustCompile(`([\d.]+)%`)
	var ends []float64
	for _, m := range matches {
		var got []float64
		for _, p := range pctRe.FindAllStringSubmatch(m[2], -1) {
			v, err := strconv.ParseFloat(p[1], 64)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, v)
		}
		ends = append(ends, got...)
	}
	// State 0 spans 0..50%, state 1 spans 50..100% of a 20s timeline.
	if !containsApprox(ends, 50) {
		t.Errorf("expected a handoff at 50%%, got stops %v", ends)
	}
}

func containsApprox(vals []float64, want float64) bool {
	for _, v := range vals {
		if v > want-0.01 && v < want+0.01 {
			return true
		}
	}
	return false
}

func TestEmitSVG_ReducedMotionPinsTheFinalState(t *testing.T) {
	out := emit(t)
	if !strings.Contains(out, "@media (prefers-reduced-motion:reduce)") {
		t.Fatal("no reduced-motion block")
	}
	if !strings.Contains(out, "g.st.final{opacity:1}") {
		t.Error("reduced motion must pin the final state visible")
	}
	if !strings.Contains(out, `class="st st1 final`) {
		t.Error("the last state must carry the final class")
	}
}

func TestEmitSVG_RejectsNoStates(t *testing.T) {
	if err := EmitSVG(io.Discard, nil, time.Second, Grid{Cols: 1, Rows: 1}); err == nil {
		t.Error("want an error for an empty state list")
	}
}

// A bar fills under a clip that starts at its first cell and steps one cell at a
// time, and draws no cursor: install.sh hides the cursor while the bar is up.
func TestEmitSVG_BarRevealStepsByCellWithNoCursor(t *testing.T) {
	states := []State{{Label: "bar", Dur: 10 * time.Second, Final: true, Rows: []Row{
		{Runs: []Run{{Text: "  ━━━━  100%"}}, Reveal: Reveal{Kind: "bar", At: time.Second, Dur: 2 * time.Second, From: 2, To: 6}},
	}}}
	var buf bytes.Buffer
	if err := EmitSVG(&buf, states, 10*time.Second, Grid{Cols: 20, Rows: 2}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `clip-path="url(#cl0_0)"`) {
		t.Error("bar row is not clipped")
	}
	if !strings.Contains(out, "steps(4,end)") {
		t.Error("bar does not step one cell at a time")
	}
	if !strings.Contains(out, "10.0000%{width:16.8px}30.0000%{width:50.4px}") {
		t.Errorf("bar does not widen from its first cell to its last over the delay:\n%s", out)
	}
	if strings.Contains(out, "cur0_0") {
		t.Error("bar row drew a cursor")
	}
}

// A SolidBlocks row's █ becomes one rect per run, the full cell height, and leaves the
// text; a row without the flag keeps its block glyphs.
func TestEmitSVG_SolidBlocksDrawCellsNotGlyphs(t *testing.T) {
	// ▃ is three eighths of the cell, at its bottom, over the run's background.
	lower := []State{{Label: "bars", Dur: time.Second, Final: true, Rows: []Row{
		{Runs: []Run{{Text: "▃", FG: "#fbbf24", BG: "#a78bfa"}}, SolidBlocks: true},
	}}}
	var lb bytes.Buffer
	if err := EmitSVG(&lb, lower, time.Second, Grid{Cols: 4, Rows: 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lb.String(), `y="56.2" width="8.9" height="6.8" fill="#fbbf24"/>`) || strings.Contains(lb.String(), "▃") {
		t.Errorf("want ▃ as a 3/8-height rect at the cell's bottom, not a glyph:\n%s", lb.String())
	}

	states := []State{{Label: "logo", Dur: 10 * time.Second, Final: true, Rows: []Row{
		{Runs: []Run{{Text: " ██╗", FG: "#f97066"}}, SolidBlocks: true},
		{Runs: []Run{{Text: "██", FG: "#9ca3af"}}},
	}}}
	var buf bytes.Buffer
	if err := EmitSVG(&buf, states, 10*time.Second, Grid{Cols: 20, Rows: 2}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `<rect x="24.4" y="45.0" width="17.3" height="18.0" fill="#f97066"/>`) {
		t.Errorf("want one 2-cell rect for the logo row's █ run:\n%s", out)
	}
	if strings.Contains(out, `fill="#f97066"> ██╗`) || !strings.Contains(out, "╗</tspan>") {
		t.Errorf("logo row should keep ╗ as text and drop its █ glyphs:\n%s", out)
	}
	if !strings.Contains(out, ">██</tspan>") {
		t.Errorf("a row without SolidBlocks lost its █ glyphs:\n%s", out)
	}
}
