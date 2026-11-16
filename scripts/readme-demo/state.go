package main

// state is the intermediate form between capture and emission: a list of full
// screens, each with the timing and reveal metadata the emitter needs.
//
// Keeping this in the middle is what makes the SVG emitter replaceable. If
// animated SVG ever stops working on GitHub, a GIF encoder consumes exactly this
// and nothing upstream changes.

import (
	"strings"
	"time"
)

// Terminal palette. Dark only: a light variant doubles the review surface for a
// demo that is conventionally dark anyway.
const (
	defaultFG = "#c9d1d9"
	defaultBG = "#0d1117"
	dimFG     = "#8b949e"
	promptFG  = "#3fb950"
	cursorFG  = "#58a6ff"
	chromeFG  = "#6e7681"
	titleFG   = "#adbac7"
)

// Grid is the terminal size every state is rendered at.
type Grid struct {
	Cols int `yaml:"cols"`
	Rows int `yaml:"rows"`
}

// Reveal says how a row appears once its state is visible.
type Reveal struct {
	// Kind is "instant", "type" (character by character, with a cursor), "fill"
	// (whole row at once, after a delay) or "bar" (a progress bar: columns From to
	// To fill in one cell at a time, with no cursor, then the whole row shows).
	Kind     string
	At       time.Duration // offset from the state's start
	Dur      time.Duration // typing or filling duration; zero for instant and fill
	From, To int           // the columns a bar fills between
}

// Row is one terminal line.
type Row struct {
	Runs   []Run
	Reveal Reveal
	// SolidBlocks draws each █, and each lower block ▁…▇, as a filled cell rather than a glyph. A terminal's
	// cells touch, so block art such as setup's logo reads as solid there; here the
	// line height is taller than the glyph, and drawn as text it reads as stripes.
	// Not for agentop's screens, whose block-drawn bars sit on adjacent rows and
	// would merge into one.
	SolidBlocks bool
}

// Window is the chrome drawn around a group of rows, for the stacked
// mini-terminals of act 3.
type Window struct {
	Title    string
	TopRow   int // first grid row occupied by the chrome
	Height   int // rows including chrome
	Active   bool
	ActiveAt time.Duration
}

// State is one full screen on the timeline.
type State struct {
	Label   string // for diagnostics and the Chrome verification pass
	Rows    []Row
	Windows []Window
	At      time.Duration // absolute offset on the whole timeline
	Dur     time.Duration
	// Final marks the last state on the timeline. It stays on screen to the end
	// of the loop, and it is what a reduced-motion reader is shown instead of the
	// animation — so the still frame is the product, not an install log.
	Final bool
}

// screenRows decodes a captured terminal screen into rows, padded or trimmed to
// the grid height so every state is exactly the same size. A state shorter than
// the grid would let the previous state's rows show through underneath it.
func screenRows(screen string, g Grid) []Row {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	rows := make([]Row, 0, g.Rows)
	for i := 0; i < g.Rows; i++ {
		if i < len(lines) {
			rows = append(rows, Row{Runs: DecodeLine(lines[i]), SolidBlocks: true})
			continue
		}
		rows = append(rows, Row{})
	}
	return rows
}

// plainRows is the visible text of a state, one line per row. Used by tests to
// assert on what a captured screen actually says.
func plainRows(s State) string {
	var b strings.Builder
	for _, r := range s.Rows {
		b.WriteString(PlainText(r.Runs))
		b.WriteByte('\n')
	}
	return b.String()
}
