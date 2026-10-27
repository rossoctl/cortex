package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/session/timeline"
)

// The Timeline pane's layout: spans from the proxy binned into a fixed number of cells per row.
// Pure functions of (timeline, window, width, now), so the drawing is tested without a model.

// timelineWindow is one [w] choice. span 0 is "all": the whole session, idle stretches
// collapsed.
type timelineWindow struct {
	label string
	span  time.Duration
}

var timelineWindows = []timelineWindow{
	{"5m", 5 * time.Minute},
	{"1h", time.Hour},
	{"24h", 24 * time.Hour},
	{"all", 0},
}

// offeredWindows is the [w] cycle for a session of length d: only the windows shorter than
// the session, then "all". A window at least as long as the session would draw the same
// thing as "all", only with blank space on the left.
func offeredWindows(d time.Duration) []int {
	var out []int
	for i, w := range timelineWindows {
		if w.span == 0 || w.span < d {
			out = append(out, i)
		}
	}
	return out
}

// timelineGapCollapse is the idle stretch "all" squeezes into a marker. Real sessions span
// days with single gaps up to 51h; drawn to scale, the active periods vanish.
const timelineGapCollapse = 30 * time.Minute

// timelineGapCells is how wide a collapsed gap is drawn: the marker and a cell either side.
const timelineGapCells = 3

// tlAxis maps cells to time. Each non-gap cell covers [from, to); a gap cell stands for a
// collapsed idle stretch.
type tlAxis struct {
	cells []tlAxisCell
}

type tlAxisCell struct {
	from, to time.Time
	gap      bool
	// marker is the gap's middle cell, where ┆ is drawn and the gap's length is labelled.
	marker bool
}

func (a tlAxis) width() int { return len(a.cells) }

// tlLinearAxis divides [from, to) evenly.
func tlLinearAxis(from, to time.Time, cells int) tlAxis {
	ax := tlAxis{cells: make([]tlAxisCell, cells)}
	step := to.Sub(from) / time.Duration(cells)
	for c := range ax.cells {
		ax.cells[c] = tlAxisCell{from: from.Add(step * time.Duration(c)), to: from.Add(step * time.Duration(c+1))}
	}
	ax.cells[cells-1].to = to
	return ax
}

// tlCollapsedAxis lays busy periods side by side, each idle stretch over timelineGapCollapse
// drawn as a gap. busy must be sorted by start.
func tlCollapsedAxis(from, to time.Time, busy [][2]time.Time, cells int) tlAxis {
	var merged [][2]time.Time
	for _, b := range busy {
		s, e := tlMaxTime(b[0], from), tlMinTime(b[1], to)
		if e.Before(s) {
			continue
		}
		if n := len(merged); n > 0 && s.Sub(merged[n-1][1]) <= timelineGapCollapse {
			merged[n-1][1] = tlMaxTime(merged[n-1][1], e)
			continue
		}
		merged = append(merged, [2]time.Time{s, e})
	}
	for i := range merged {
		if !merged[i][1].After(merged[i][0]) {
			merged[i][1] = merged[i][0].Add(time.Second)
		}
	}
	// Keep at least four cells per period on average; beyond that, merge the shortest gaps,
	// which is what a linear axis would do with them anyway.
	for len(merged) > 1 && len(merged)*(4+timelineGapCells) > cells {
		merged = tlMergeSmallestGap(merged)
	}
	if len(merged) <= 1 {
		if len(merged) == 1 {
			from, to = merged[0][0], merged[0][1]
		}
		if !to.After(from) {
			to = from.Add(time.Second)
		}
		return tlLinearAxis(from, to, cells)
	}
	gaps := len(merged) - 1
	avail := cells - gaps*timelineGapCells
	// Every period gets one cell first, then the rest by share of busy time, so no period can
	// be squeezed out and the widths always sum to avail.
	widths := make([]int, len(merged))
	var busyDur time.Duration
	for _, b := range merged {
		busyDur += b[1].Sub(b[0])
	}
	rest := avail - len(merged)
	given := 0
	for i, b := range merged {
		w := 1
		if busyDur > 0 {
			w += int(float64(rest) * float64(b[1].Sub(b[0])) / float64(busyDur))
		} else {
			w += rest / len(merged)
		}
		widths[i] = w
		given += w
	}
	widths[len(widths)-1] += avail - given
	var ax tlAxis
	for i, b := range merged {
		w := widths[i]
		step := b[1].Sub(b[0]) / time.Duration(w)
		for c := 0; c < w; c++ {
			cell := tlAxisCell{from: b[0].Add(step * time.Duration(c)), to: b[0].Add(step * time.Duration(c+1))}
			if c == w-1 {
				cell.to = b[1]
			}
			ax.cells = append(ax.cells, cell)
		}
		if i < gaps {
			for c := 0; c < timelineGapCells; c++ {
				ax.cells = append(ax.cells, tlAxisCell{from: b[1], to: merged[i+1][0], gap: true, marker: c == timelineGapCells/2})
			}
		}
	}
	return ax
}

func tlMergeSmallestGap(m [][2]time.Time) [][2]time.Time {
	best := 0
	for i := 1; i < len(m)-1; i++ {
		if m[i+1][0].Sub(m[i][1]) < m[best+1][0].Sub(m[best][1]) {
			best = i
		}
	}
	m[best][1] = m[best+1][1]
	return slices.Delete(m, best+1, best+2)
}

// cellAt is the non-gap cell holding t, and false for an instant in a gap or outside the axis.
// An instant on a boundary belongs to the later cell, except at a busy period's end, which
// belongs to the period.
func (a tlAxis) cellAt(t time.Time) (int, bool) {
	n := len(a.cells)
	i := sort.Search(n, func(i int) bool { return a.cells[i].to.After(t) })
	if i == n {
		if n > 0 && !a.cells[n-1].gap && t.Equal(a.cells[n-1].to) {
			return n - 1, true
		}
		return 0, false
	}
	if a.cells[i].gap || t.Before(a.cells[i].from) {
		if i > 0 && !a.cells[i-1].gap && t.Equal(a.cells[i-1].to) {
			return i - 1, true
		}
		return 0, false
	}
	return i, true
}

// share is how much of one cell's time went to each state.
type share struct {
	tool, model, retry, subs, user float64
}

func (s share) total() float64 { return s.tool + s.model + s.retry + s.subs + s.user }

// tlState is one of the states a row's cell is split into, stacked bottom to top in this order.
type tlState int

const (
	stTool tlState = iota
	stModel
	stRetry
	stSubs
	stUser
	stNone
)

func (s share) of(st tlState) float64 {
	return [...]float64{s.tool, s.model, s.retry, s.subs, s.user, 0}[st]
}

// top2 is the cell's two largest states in stacking order, with their heights. A terminal cell
// holds two colours, so a cell that mixed three states shows its two largest, scaled up to the
// cell's whole filled share so dropping the third does not make the cell look idler; the cursor
// line carries the full split. On real sessions that is 8% of cells at "all", none at 1h or 5m.
func (s share) top2() (lo tlState, loF float64, hi tlState, hiF float64) {
	a, b := stNone, stNone
	for st := stTool; st < stNone; st++ {
		switch f := s.of(st); {
		case f <= 0:
		case a == stNone || f > s.of(a):
			a, b = st, a
		case b == stNone || f > s.of(b):
			b = st
		}
	}
	if a == stNone {
		return stNone, 0, stNone, 0
	}
	if b == stNone {
		return a, s.of(a), stNone, 0
	}
	if b < a {
		a, b = b, a
	}
	scale := s.total() / (s.of(a) + s.of(b))
	return a, s.of(a) * scale, b, s.of(b) * scale
}

type tlAgentRow struct {
	agent timeline.Agent
	depth int
	cells []share
	// alive marks the cells the agent existed in; a subagent's cells are measured against
	// the part of the cell it was alive for.
	alive     []bool
	modelTime time.Duration
	toolTime  time.Duration
	calls     int
}

type tlHostRow struct {
	host     string
	kind     timeline.Kind
	peak     []int
	err      []bool
	requests int
}

// tlLayout is a Timeline binned to a width, ready to draw.
type tlLayout struct {
	axis   tlAxis
	agents []tlAgentRow
	hosts  []tlHostRow
	from   time.Time
	to     time.Time
}

// buildTimelineLayout bins tl into cells over [from, to]. A zero from means the window is "all".
func buildTimelineLayout(tl *timeline.Timeline, from, to time.Time, cells int, now time.Time) tlLayout {
	all := from.IsZero()
	if all {
		from, to = tl.Start, tlMaxTime(activityEnd(tl, now), tl.Start.Add(time.Second))
	}
	if !to.After(from) {
		to = from.Add(time.Second)
	}
	var ax tlAxis
	if all {
		ax = tlCollapsedAxis(from, to, tlBusyPeriods(tl, now), cells)
	} else {
		ax = tlLinearAxis(from, to, cells)
	}
	return tlLayout{axis: ax, from: from, to: to, agents: tlAgentRows(tl, ax, from, to, now), hosts: tlHostRows(tl, ax, from, to, now)}
}

// tlEndOr is a span's end, or now for one still in flight.
func tlEndOr(end *time.Time, now time.Time) time.Time {
	if end == nil {
		return now
	}
	return *end
}

// tlBusyPeriods is what "all" keeps in view: the agents' turns and tools, or the hosts' for a
// session with no agents.
func tlBusyPeriods(tl *timeline.Timeline, now time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, t := range tl.Turns {
		out = append(out, [2]time.Time{t.Start, tlEndOr(t.End, now)})
	}
	for _, t := range tl.Tools {
		out = append(out, [2]time.Time{t.Start, tlEndOr(t.End, now)})
	}
	if len(out) == 0 {
		for _, h := range tl.Hosts {
			out = append(out, [2]time.Time{h.Start, tlEndOr(h.End, now)})
		}
	}
	// A zero-length span (a lost tool, an instant request) is padded to a second, so its
	// period has a duration to give cells to.
	for i := range out {
		if !out[i][1].After(out[i][0]) {
			out[i][1] = out[i][0].Add(time.Second)
		}
	}
	slices.SortFunc(out, func(a, b [2]time.Time) int { return a[0].Compare(b[0]) })
	return out
}

// activityEnd is when the session's agents last did anything, or its last event when it has
// no agent rows. An ended session's windows end here, not at trailing background traffic.
func activityEnd(tl *timeline.Timeline, now time.Time) time.Time {
	var end time.Time
	for _, t := range tl.Turns {
		end = tlMaxTime(end, tlEndOr(t.End, now))
	}
	for _, t := range tl.Tools {
		end = tlMaxTime(end, tlEndOr(t.End, now))
	}
	if end.IsZero() {
		return tl.End
	}
	return end
}

// ivs is a set of half-open intervals, sorted and merged, for O(log n) coverage queries.
type ivs [][2]time.Time

func newIvs(in [][2]time.Time) ivs {
	in = slices.Clone(in)
	slices.SortFunc(in, func(a, b [2]time.Time) int { return a[0].Compare(b[0]) })
	var out ivs
	for _, iv := range in {
		if !iv[1].After(iv[0]) {
			continue
		}
		if n := len(out); n > 0 && !iv[0].After(out[n-1][1]) {
			out[n-1][1] = tlMaxTime(out[n-1][1], iv[1])
			continue
		}
		out = append(out, iv)
	}
	return out
}

// cover is how much of [lo, hi) the set covers.
func (v ivs) cover(lo, hi time.Time) time.Duration {
	i := sort.Search(len(v), func(i int) bool { return v[i][1].After(lo) })
	var d time.Duration
	for ; i < len(v) && v[i][0].Before(hi); i++ {
		d += tlMinTime(v[i][1], hi).Sub(tlMaxTime(v[i][0], lo))
	}
	return d
}

// waits classifies the gaps between an agent's consecutive turns. After a failed call it was
// retrying. When the next turn was woken by a subagent's result, or one of its subagents was
// running, it was waiting on its subagents. After a finished reply otherwise, it was waiting
// on the user; only the main agent has one.
func waits(tl *timeline.Timeline, agent string, children []string, life map[string][2]time.Time, now time.Time) (retry, subs, user ivs) {
	var kids [][2]time.Time
	for _, k := range children {
		if l, ok := life[k]; ok {
			kids = append(kids, l)
		}
	}
	woke := map[int64]bool{}
	for _, r := range tl.Returns {
		if r.Into == agent {
			woke[r.At.UnixNano()] = true
		}
	}
	var turns []timeline.Turn
	for _, t := range tl.Turns {
		if t.Agent == agent {
			turns = append(turns, t)
		}
	}
	var r, s, u [][2]time.Time
	for i := 0; i+1 < len(turns); i++ {
		g0, g1 := tlEndOr(turns[i].End, now), turns[i+1].Start
		if !g1.After(g0) {
			continue
		}
		switch {
		case turns[i].Error:
			r = append(r, [2]time.Time{g0, g1})
		case woke[g1.UnixNano()]:
			s = append(s, [2]time.Time{g0, g1})
		case turns[i].Finish == "end_turn" && agent == timeline.MainAgent:
			// The whole gap: where a subagent ran during it, that part is drawn as waiting on
			// subagents, which a cell takes before the user's share.
			u = append(u, [2]time.Time{g0, g1})
		}
	}
	// 5: a main agent whose last call finished its reply is waiting on the user now.
	if n := len(turns); n > 0 && agent == timeline.MainAgent && turns[n-1].Finish == "end_turn" && turns[n-1].End != nil && now.After(*turns[n-1].End) {
		u = append(u, [2]time.Time{*turns[n-1].End, now})
	}
	return newIvs(r), newIvs(append(s, kids...)), newIvs(u)
}

// tlAgentRows orders agents as a tree, main first and each child under its parent, and fills
// each cell with how its time divided.
func tlAgentRows(tl *timeline.Timeline, ax tlAxis, from, to, now time.Time) []tlAgentRow {
	life := map[string][2]time.Time{}
	note := func(id string, s, e time.Time) {
		l, ok := life[id]
		if !ok {
			life[id] = [2]time.Time{s, e}
			return
		}
		life[id] = [2]time.Time{tlMinTime(l[0], s), tlMaxTime(l[1], e)}
	}
	model, tool := map[string][][2]time.Time{}, map[string][][2]time.Time{}
	calls := map[string]int{}
	for _, t := range tl.Turns {
		e := tlEndOr(t.End, now)
		note(t.Agent, t.Start, e)
		model[t.Agent] = append(model[t.Agent], [2]time.Time{t.Start, e})
	}
	for _, t := range tl.Tools {
		e := tlEndOr(t.End, now)
		note(t.Agent, t.Start, e)
		if !t.Start.Before(from) && !t.Start.After(to) {
			calls[t.Agent]++
		}
		// An Agent call's own span is the few seconds until the async ack; the child's row
		// carries the work.
		if t.Name != "Agent" && t.Name != "Task" {
			tool[t.Agent] = append(tool[t.Agent], [2]time.Time{t.Start, e})
		}
	}
	children := map[string][]string{}
	byID := map[string]timeline.Agent{}
	var roots []string
	for _, a := range tl.Agents {
		byID[a.ID] = a
	}
	for _, a := range tl.Agents {
		if _, ok := byID[a.Parent]; ok && a.Parent != a.ID {
			children[a.Parent] = append(children[a.Parent], a.ID)
		} else {
			roots = append(roots, a.ID)
		}
	}
	for _, kids := range children {
		slices.SortStableFunc(kids, func(x, y string) int { return life[x][0].Compare(life[y][0]) })
	}

	build := func(a timeline.Agent, depth int) tlAgentRow {
		m, t := newIvs(model[a.ID]), newIvs(tool[a.ID])
		retry, subs, user := waits(tl, a.ID, children[a.ID], life, now)
		row := tlAgentRow{agent: a, depth: depth, cells: make([]share, ax.width()), alive: make([]bool, ax.width()),
			modelTime: m.cover(from, to), toolTime: t.cover(from, to), calls: calls[a.ID]}
		l, known := life[a.ID]
		for c, cell := range ax.cells {
			if cell.gap {
				continue
			}
			lo, hi := cell.from, cell.to
			// A subagent is measured against the part of the cell it was alive for; the main
			// agent against the whole cell, since it is alive for the whole session.
			if a.ID != timeline.MainAgent {
				if !known {
					continue
				}
				lo, hi = tlMaxTime(lo, l[0]), tlMinTime(hi, l[1])
			}
			if !hi.After(lo) {
				continue
			}
			row.alive[c] = true
			span := float64(hi.Sub(lo))
			left := 1.0
			take := func(v ivs) float64 {
				f := min(float64(v.cover(lo, hi))/span, left)
				left -= f
				return f
			}
			row.cells[c] = share{tool: take(t), model: take(m), retry: take(retry), subs: take(subs), user: take(user)}
		}
		return row
	}
	var out []tlAgentRow
	var walk func(id string, depth int)
	walk = func(id string, depth int) {
		out = append(out, build(byID[id], depth))
		for _, k := range children[id] {
			walk(k, depth+1)
		}
	}
	for _, id := range roots {
		walk(id, 0)
	}
	return out
}

// isLoopback reports a host on this machine: the proxy's own ports and local tools, not egress.
func isLoopback(host string) bool {
	return strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "[::1]")
}

// tlHostRows draws each host's requests in flight per cell, by one sweep over its request
// boundaries: O(n log n + cells) per host however many requests it carried.
func tlHostRows(tl *timeline.Timeline, ax tlAxis, from, to, now time.Time) []tlHostRow {
	type acc struct {
		row  *tlHostRow
		pts  []tlPt
		errs []time.Time
	}
	byHost := map[string]*acc{}
	var order []string
	for _, h := range tl.Hosts {
		if isLoopback(h.Host) {
			continue
		}
		e := tlEndOr(h.End, now)
		if e.Before(from) || h.Start.After(to) {
			continue
		}
		a := byHost[h.Host]
		if a == nil {
			a = &acc{row: &tlHostRow{host: h.Host, kind: h.Kind, peak: make([]int, ax.width()), err: make([]bool, ax.width())}}
			byHost[h.Host] = a
			order = append(order, h.Host)
		}
		a.row.requests++
		a.row.kind = tlStrongerKind(a.row.kind, h.Kind)
		a.pts = append(a.pts, tlPt{at: h.Start, d: 1}, tlPt{at: tlMaxTime(e, h.Start), d: -1, zero: !e.After(h.Start)})
		if h.Error {
			a.errs = append(a.errs, h.Start)
		}
	}
	out := make([]tlHostRow, 0, len(order))
	for _, host := range order {
		a := byHost[host]
		tlSweepPeaks(a.pts, ax, a.row.peak)
		for _, at := range a.errs {
			if c, ok := ax.cellAt(at); ok {
				a.row.err[c] = true
				a.row.peak[c] = max(a.row.peak[c], 1)
			}
		}
		out = append(out, *a.row)
	}
	slices.SortStableFunc(out, func(x, y tlHostRow) int { return y.requests - x.requests })
	return out
}

type tlPt struct {
	at time.Time
	d  int
	// zero marks the close of a zero-length request, which sorts after the opens at its
	// instant so it is counted; every other close sorts before them, so requests that only
	// touch, one ending as the next starts, are not counted as concurrent.
	zero bool
}

// tlSweepPeaks writes into peak the most intervals open at once in each cell.
func tlSweepPeaks(pts []tlPt, ax tlAxis, peak []int) {
	rank := func(p tlPt) int {
		switch {
		case p.d < 0 && !p.zero:
			return 0
		case p.d > 0:
			return 1
		}
		return 2
	}
	slices.SortFunc(pts, func(a, b tlPt) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return rank(a) - rank(b)
	})
	cur, i := 0, 0
	for c, cell := range ax.cells {
		if cell.gap {
			continue
		}
		for i < len(pts) && pts[i].at.Before(cell.from) {
			cur += pts[i].d
			i++
		}
		best := cur
		for i < len(pts) && pts[i].at.Before(cell.to) {
			cur += pts[i].d
			best = max(best, cur)
			i++
		}
		peak[c] = best
	}
}

// tlStrongerKind keeps the most specific kind a host was seen as: a gateway answering LLM
// calls and the odd model-list GET is an LLM host.
func tlStrongerKind(a, b timeline.Kind) timeline.Kind {
	rank := map[timeline.Kind]int{timeline.KindTunnel: 0, timeline.KindHTTP: 1, timeline.KindMCP: 2, timeline.KindLLM: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// tlDuration is a duration in at most five characters.
func tlDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 100*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func tlMaxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func tlMinTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
