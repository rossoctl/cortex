package main

// svg emits the states as one animated SVG.
//
// Two techniques carry the whole file.
//
// ROWS, NOT CHARACTERS, WITH FORCED METRICS. Every run of text is one <tspan>
// carrying textLength and lengthAdjust="spacingAndGlyphs", so the grid holds its
// columns whatever monospace font the reader happens to have. Without it the
// columns shear on any machine whose default differs from the author's, which for
// a table-heavy TUI is the difference between a screenshot and a mess.
//
// ONE ABSOLUTE TIMELINE. Every animation runs the full duration with its own
// keyframe percentages marking when it acts. Nothing is chained off anything else,
// so nothing can drift, and no script is needed — which matters because GitHub
// serves this through camo as an image, where scripts never run but declarative
// CSS animation does.

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Cell metrics. textLength pins the real advance, so these only have to be
// self-consistent, not matched to any particular font's metrics.
const (
	cellW    = 8.4
	lineH    = 18.0
	fontSize = 14.0
	padX     = 16.0
	padY     = 14.0
	barH     = 30.0
)

type emitter struct {
	w     io.Writer
	total time.Duration
	grid  Grid
	css   []string
	// rows holds each distinct row markup once, by content, and rowIDs its id in order.
	// A state draws a row it shares with another state as a <use> of it: the timeline's
	// frames repeat most of their rows, and drawn inline the asset was twice its budget.
	rows   map[string]int
	rowIDs []string
}

// pct converts an absolute offset on the timeline into a keyframe percentage.
func (e *emitter) pct(d time.Duration) float64 {
	if e.total <= 0 {
		return 0
	}
	p := float64(d) / float64(e.total) * 100
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}

// demoLabel is the animated demo's aria-label.
const demoLabel = "Cortex: install, point Claude Code at it, run several sessions, then watch an agent and its subagents work in the agentop timeline and open a tool call"

// EmitSVG writes the animated SVG.
func EmitSVG(w io.Writer, states []State, total time.Duration, g Grid) error {
	return emitSVG(w, states, total, g, demoLabel)
}

func emitSVG(w io.Writer, states []State, total time.Duration, g Grid, label string) error {
	if len(states) == 0 {
		return fmt.Errorf("no states to emit")
	}
	e := &emitter{w: w, total: total, grid: g, rows: map[string]int{}}

	width := padX*2 + float64(g.Cols)*cellW
	height := barH + padY*2 + float64(g.Rows)*lineH

	var body strings.Builder
	for i, st := range states {
		e.stateCSS(i, st, i == len(states)-1)
		body.WriteString(e.stateBody(i, st))
	}

	fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" font-size="%.0f" role="img" aria-label="%s">`+"\n",
		width, height, width, height, fontSize, escapeXML(label))

	fmt.Fprintf(w, "<style>\n%s</style>\n", e.styleSheet())

	// Window chrome: a rounded panel and a title bar with three dots, so the
	// asset reads as a terminal rather than as text floating on a page.
	fmt.Fprintf(w, `<rect width="%.0f" height="%.0f" rx="8" fill="%s"/>`+"\n", width, height, defaultBG)
	fmt.Fprintf(w, `<path d="M0 8a8 8 0 0 1 8-8h%.0fa8 8 0 0 1 8 8v%.0fH0z" fill="#161b22"/>`+"\n", width-16, barH-8)
	for i, c := range []string{"#ff5f57", "#febc2e", "#28c840"} {
		fmt.Fprintf(w, `<circle cx="%.0f" cy="%.0f" r="5" fill="%s"/>`+"\n", 18+float64(i)*18, barH/2, c)
	}
	fmt.Fprintf(w, `<text x="%.0f" y="%.1f" fill="%s" font-size="12" text-anchor="middle">cortex</text>`+"\n",
		width/2, barH/2+4, chromeFG)

	io.WriteString(w, "<defs>\n")
	for id, markup := range e.rowIDs {
		fmt.Fprintf(w, `<g id="r%d">%s</g>`+"\n", id, markup)
	}
	io.WriteString(w, "</defs>\n")
	io.WriteString(w, body.String())
	io.WriteString(w, "</svg>\n")
	return nil
}

func (e *emitter) styleSheet() string {
	var b strings.Builder
	// State groups hide with OPACITY, not visibility. visibility is inheritable
	// but a child may re-declare `visible` and show through a hidden parent — and
	// every row reveal below does exactly that, which had all eight states drawn
	// on top of each other. opacity cannot be escaped by a descendant.
	b.WriteString(`text{font-family:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace;white-space:pre}
g.st{opacity:0}
.rv{visibility:hidden}
.cur{fill:` + cursorFG + `}
@keyframes blink{0%,49%{opacity:1}50%,100%{opacity:0.15}}
`)
	for _, rule := range e.css {
		b.WriteString(rule)
	}
	// Reduced motion: freeze on the final state rather than on an install log.
	// Same courtesy the reference animation extends, and the only accessible
	// answer for an asset that cannot be paused without script.
	b.WriteString(`@media (prefers-reduced-motion:reduce){
g.st,.rv,.cur,clipPath rect{animation:none!important}
g.st{opacity:0}
g.st.final{opacity:1}
.rv{visibility:visible}
.cur{visibility:hidden}
clipPath rect{width:100000px}
}
`)
	return b.String()
}

// stateCSS writes the visibility keyframes for one state and the reveal keyframes
// for its animated rows.
func (e *emitter) stateCSS(i int, st State, final bool) {
	in, out := e.pct(st.At), e.pct(st.At+st.Dur)
	// Values are pinned at both ends of every range. Visibility is a discrete
	// property, so between two keyframes a browser flips it at the midpoint — the
	// near-duplicate stops confine that flip to a sliver instead of letting it
	// drift a whole state's width.
	var kf strings.Builder
	fmt.Fprintf(&kf, "@keyframes st%d{", i)
	if in > 0 {
		fmt.Fprintf(&kf, "0%%,%.4f%%{opacity:0}", maxf(in-0.001, 0))
	}
	if final {
		fmt.Fprintf(&kf, "%.4f%%,100%%{opacity:1}", in)
	} else {
		fmt.Fprintf(&kf, "%.4f%%,%.4f%%{opacity:1}", in, maxf(out-0.001, in))
		fmt.Fprintf(&kf, "%.4f%%,100%%{opacity:0}", out)
	}
	kf.WriteString("}\n")
	e.css = append(e.css, kf.String())
	e.css = append(e.css, fmt.Sprintf("g.st%d{animation:st%d %s linear infinite}\n", i, i, secs(e.total)))

	for ri, row := range st.Rows {
		switch row.Reveal.Kind {
		case "fill":
			at := e.pct(st.At + row.Reveal.At)
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes rv%d_%d{0%%,%.4f%%{visibility:hidden}%.4f%%,100%%{visibility:visible}}\n",
				i, ri, maxf(at-0.001, 0), at))
			e.css = append(e.css, fmt.Sprintf(".rv%d_%d{animation:rv%d_%d %s linear infinite}\n",
				i, ri, i, ri, secs(e.total)))
		case "type":
			chars := runeLen(row)
			startAt := e.pct(st.At + row.Reveal.At)
			endAt := e.pct(st.At + row.Reveal.At + row.Reveal.Dur)
			full := float64(chars) * cellW
			// steps() applies per keyframe segment, so the reveal advances one
			// character at a time rather than sweeping smoothly across them.
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes tp%d_%d{0%%,%.4f%%{width:0}%.4f%%,100%%{width:%.1fpx}}\n",
				i, ri, startAt, endAt, full))
			e.css = append(e.css, fmt.Sprintf("#cl%d_%d rect{animation:tp%d_%d %s steps(%d,end) infinite}\n",
				i, ri, i, ri, secs(e.total), maxi(chars, 1)))
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes cx%d_%d{0%%,%.4f%%{x:%.1fpx}%.4f%%,100%%{x:%.1fpx}}\n",
				i, ri, startAt, padX, endAt, padX+full))
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes cv%d_%d{0%%,%.4f%%{visibility:hidden}%.4f%%,%.4f%%{visibility:visible}%.4f%%,100%%{visibility:hidden}}\n",
				i, ri, maxf(startAt-0.001, 0), startAt, endAt, minf(endAt+0.6, 100)))
			e.css = append(e.css, fmt.Sprintf(
				".cur%d_%d{animation:cx%d_%d %s steps(%d,end) infinite,cv%d_%d %s linear infinite,blink 1s steps(1,end) infinite}\n",
				i, ri, i, ri, secs(e.total), maxi(chars, 1), i, ri, secs(e.total)))
		case "bar":
			// The clip shows the row up to From, widens a cell at a time to To,
			// then jumps to the whole row: a bar filling, then its figures. No
			// cursor, as install.sh hides it while the bar draws.
			startAt := e.pct(st.At + row.Reveal.At)
			endAt := e.pct(st.At + row.Reveal.At + row.Reveal.Dur)
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes tp%d_%d{0%%,%.4f%%{width:%.1fpx}%.4f%%{width:%.1fpx}%.4f%%,100%%{width:%.1fpx}}\n",
				i, ri, startAt, float64(row.Reveal.From)*cellW, endAt, float64(row.Reveal.To)*cellW,
				minf(endAt+0.001, 100), float64(runeLen(row))*cellW))
			e.css = append(e.css, fmt.Sprintf("#cl%d_%d rect{animation:tp%d_%d %s steps(%d,end) infinite}\n",
				i, ri, i, ri, secs(e.total), maxi(row.Reveal.To-row.Reveal.From, 1)))
		}
	}
}

// stateBody draws one state's rows.
func (e *emitter) stateBody(i int, st State) string {
	var b strings.Builder
	class := fmt.Sprintf("st st%d", i)
	if st.Final {
		class += " final"
	}
	if st.Label != "" {
		class += " " + safeClass(st.Label)
	}
	fmt.Fprintf(&b, `<g class="%s">`+"\n", class)

	for ri, row := range st.Rows {
		if len(row.Runs) == 0 {
			continue
		}
		e.row(&b, i, ri, row)
	}
	b.WriteString("</g>\n")
	return b.String()
}

// row draws one row into out: inline when it has a reveal, whose markup names its own
// state, and otherwise as a <use> of the one copy in the shared table.
func (e *emitter) row(out *strings.Builder, i, ri int, row Row) {
	var b strings.Builder
	{
		y := barH + padY + float64(ri+1)*lineH - 4
		rowClass := ""
		switch row.Reveal.Kind {
		case "fill":
			rowClass = fmt.Sprintf(` class="rv rv%d_%d"`, i, ri)
		case "type", "bar":
			rowClass = fmt.Sprintf(` clip-path="url(#cl%d_%d)"`, i, ri)
			fmt.Fprintf(&b, `<clipPath id="cl%d_%d"><rect x="%.1f" y="%.1f" width="0" height="%.1f"/></clipPath>`+"\n",
				i, ri, padX, y-lineH+5, lineH)
		}

		// Backgrounds and full blocks first, then partial blocks over them, then text on
		// top. A █ covers its whole cell, so it can share the background layer, and a
		// bar of them merges with the backgrounds beside it into one rect.
		var under, over cellRects
		col := 0
		for _, run := range row.Runs {
			n := len([]rune(run.Text))
			if run.BG != "" {
				under.add(col, n, lineH, run.BG)
			}
			col += n
		}
		if row.SolidBlocks {
			blockRects(&under, &over, row)
		}
		under.write(&b, rowClassForRect(rowClass), y)
		over.write(&b, rowClassForRect(rowClass), y)

		fmt.Fprintf(&b, `<text%s y="%.1f">`, rowClass, y)
		col = 0
		for _, run := range row.Runs {
			n := len([]rune(run.Text))
			if row.SolidBlocks {
				run.Text = strings.Map(func(r rune) rune { // drawn by blockRects
					if lowerEighths(r) > 0 {
						return ' '
					}
					return r
				}, run.Text)
			}
			if strings.TrimSpace(run.Text) != "" {
				fill := run.FG
				if fill == "" {
					fill = defaultFG
				}
				weight := ""
				if run.Bold {
					weight = ` font-weight="600"`
				}
				opacity := ""
				if run.Dim {
					opacity = ` opacity="0.65"`
				}
				fmt.Fprintf(&b, `<tspan x="%.1f" textLength="%.1f" lengthAdjust="spacingAndGlyphs" fill="%s"%s%s>%s</tspan>`,
					padX+float64(col)*cellW, float64(n)*cellW, fill, weight, opacity, escapeXML(run.Text))
			}
			col += n
		}
		b.WriteString("</text>\n")

		if row.Reveal.Kind == "type" {
			fmt.Fprintf(&b, `<rect class="cur cur%d_%d" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`+"\n",
				i, ri, padX, y-lineH+6, cellW, lineH-4)
		}
	}
	if row.Reveal.Kind != "" {
		out.WriteString(b.String())
		return
	}
	id, ok := e.rows[b.String()]
	if !ok {
		id = len(e.rowIDs)
		e.rows[b.String()] = id
		e.rowIDs = append(e.rowIDs, b.String())
	}
	fmt.Fprintf(out, `<use href="#r%d"/>`+"\n", id)
}

// blockRects adds a row's block cells: each █ to under, the full size of its cell in
// the run's colour, so block art is solid across rows as a terminal draws it, and each
// lower block ▁…▇ to over, filling the bottom of its cell over the run's background. A
// font glyph does not reach a cell's edges, which is why neither is drawn as text.
func blockRects(under, over *cellRects, row Row) {
	col := 0
	for _, run := range row.Runs {
		fill := run.FG
		if fill == "" {
			fill = defaultFG
		}
		for _, r := range run.Text {
			switch e := lowerEighths(r); {
			case e == 8:
				under.add(col, 1, lineH, fill)
			case e > 0:
				over.add(col, 1, lineH*float64(e)/8, fill)
			}
			col++
		}
	}
}

// cellRect is a run of cells of one height and colour, bottom-aligned in its row.
type cellRect struct {
	col, n int
	h      float64
	fill   string
}

// cellRects collects a row's cell rects in column order, merging each into the one
// before it when they touch and match: the timeline's bars are a rect per cell
// otherwise, which was most of the asset's size.
type cellRects []cellRect

func (r *cellRects) add(col, n int, h float64, fill string) {
	if k := len(*r) - 1; k >= 0 {
		if last := &(*r)[k]; last.col+last.n == col && last.h == h && last.fill == fill {
			last.n += n
			return
		}
	}
	*r = append(*r, cellRect{col, n, h, fill})
}

func (r cellRects) write(b *strings.Builder, class string, y float64) {
	for _, c := range r {
		fmt.Fprintf(b, `<rect%s x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`+"\n",
			class, padX+float64(c.col)*cellW, y+5-c.h, float64(c.n)*cellW+seamOverlap, c.h, c.fill)
	}
}

// seamOverlap widens each cell rect into its right neighbour. Cells sit at fractional
// x, and the browser's anti-aliasing of two touching edges otherwise leaves a faint
// line between cells that a terminal never draws.
const seamOverlap = 0.5

// lowerEighths is how many eighths of a cell a lower block element fills from the
// bottom: 8 for █, 1 for ▁ up to 7 for ▇, and 0 for any other rune.
func lowerEighths(r rune) int {
	switch {
	case r == '█':
		return 8
	case r >= '▁' && r <= '▇':
		return int(r-'▁') + 1
	}
	return 0
}

// rowClassForRect keeps a background rect on the same reveal as its text, but a
// clip-path belongs only on the text element.
func rowClassForRect(rowClass string) string {
	if strings.HasPrefix(rowClass, ` clip-path`) {
		return ""
	}
	return rowClass
}

func runeLen(row Row) int {
	n := 0
	for _, r := range row.Runs {
		n += len([]rune(r.Text))
	}
	return n
}

func secs(d time.Duration) string { return fmt.Sprintf("%gs", d.Seconds()) }

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func safeClass(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return "lbl-" + b.String()
}

// escapeXML escapes terminal text for XML. Box-drawing characters and the like
// pass through as UTF-8; only the five markup-significant characters change.
func escapeXML(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
