package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/session/timeline"
)

// The Timeline pane ([T]): one session's Claude Code agents and hosts on one time axis, from
// GET /v1/sessions/{id}/timeline. The proxy computes the spans; this file fetches, bins and
// draws them.
//
// An agent cell is a stacked bar of how that cell's time divided: tools running at the
// baseline, then the model generating, retrying after a failed call, waiting on its subagents,
// and waiting on the user. A host cell's height is how many requests were in flight at once.

// timelineRefresh is the most often a live session's timeline is refetched, and how often the
// layout's "now" moves for one with a call in flight.
const timelineRefresh = time.Second

// timelineIdleRefresh is how often a live session with no new events is refetched anyway:
// spans in flight grow and the window slides even when nothing new is recorded.
const timelineIdleRefresh = 10 * time.Second

// timelineReadTimeout bounds one refresh. timelineFirstReadTimeout bounds the first fetch of a
// visit, which for a session only on disk is a cold read of its archive: over a second at normal
// priority and 5.3s measured at background priority. The proxy finishes that read whether or
// not the client waits, so a short first deadline gains nothing and turns a slow load into an
// error.
const (
	timelineReadTimeout      = 5 * time.Second
	timelineFirstReadTimeout = 30 * time.Second
)

// timelineRetryCap bounds the backoff after a failed fetch.
const timelineRetryCap = 30 * time.Second

// timelineRetryAfter is the wait before the next fetch after fails consecutive failures:
// timelineRefresh doubled per failure, capped.
func timelineRetryAfter(fails int) time.Duration {
	return min(timelineRefresh<<min(max(fails-1, 0), 5), timelineRetryCap)
}

// timelineLiveFor is how long after its last recorded event a session still counts as live:
// its open spans run to now and its windows follow now. Past it, open spans end at the last
// event, the last moment anything was seen, rather than running on to now. Thirty minutes is
// past every quiet stretch a working session showed on real data, such as a foreground
// `sleep 600` poll, which records nothing while it runs.
const timelineLiveFor = 30 * time.Minute

var errTimelineUnsupported = errors.New("timeline endpoint not available")

type timelineLoadedMsg struct {
	tl  *timeline.Timeline
	req uint64
	err error
}

// timelineTickMsg drives the refresh; gen ties it to one visit, as usageTickMsg does.
type timelineTickMsg struct{ gen uint64 }

type timelineState struct {
	session string
	// windowIdx is the chosen window, an index into timelineWindows. A session shorter than
	// it shows "all" without changing it; see window.
	windowIdx int
	tl        *timeline.Timeline
	err       error
	loading   bool
	// stale is set when the stream delivers an event for this session; the next tick refetches.
	stale bool
	// fails counts consecutive failed fetches, and retryAt is when the next may go; see
	// timelineRetryAfter.
	fails   int
	retryAt time.Time
	// cursor is the highlighted cell, -1 for none. scroll is how many rows are scrolled past:
	// agent rows first, then host rows, so one pair of keys reaches every row.
	cursor int
	scroll int
	// fetchedAt is when tl arrived.
	fetchedAt  time.Time
	reqSeq     uint64
	tickGen    uint64
	returnPane paneID

	// layout caches the last layout, keyed by what it was built from: rebuilt on a new fetch,
	// a window, width or "now" change, not on every keypress and redraw.
	layout    tlLayout
	layoutKey tlLayoutKey
}

type tlLayoutKey struct {
	tl        *timeline.Timeline
	window    int
	cells     int
	from, now time.Time
}

// sessionSpan is how long the session ran, first event to last activity.
func (t *timelineState) sessionSpan(now time.Time) time.Duration {
	if t.tl == nil {
		return 0
	}
	return activityEnd(t.tl, now).Sub(t.tl.Start)
}

// window is the window drawn: the chosen one if the session is longer than it, else "all".
func (t *timelineState) window(now time.Time) int {
	if slices.Contains(offeredWindows(t.sessionSpan(now)), t.windowIdx) {
		return t.windowIdx
	}
	return len(timelineWindows) - 1
}

// openTimeline enters the pane for session, remembering where to return to.
func (m *model) openTimeline(session string) tea.Cmd {
	m.timeline.returnPane = m.pane
	m.pane = paneTimeline
	if m.timeline.session != session {
		m.timeline.tl, m.timeline.err, m.timeline.cursor, m.timeline.scroll = nil, nil, -1, 0
		m.timeline.fails, m.timeline.retryAt = 0, time.Time{}
	}
	m.timeline.session = session
	return m.resumeTimeline()
}

// resumeTimeline starts one refresh chain and fetches now.
func (m *model) resumeTimeline() tea.Cmd {
	m.timeline.tickGen++
	return tea.Batch(m.fetchTimeline(), timelineTick(m.timeline.tickGen))
}

func timelineTick(gen uint64) tea.Cmd {
	return tea.Tick(timelineRefresh, func(time.Time) tea.Msg { return timelineTickMsg{gen: gen} })
}

func (m *model) fetchTimeline() tea.Cmd {
	if m.client == nil || m.timeline.session == "" {
		return nil
	}
	m.timeline.reqSeq++
	m.timeline.loading, m.timeline.stale = true, false
	client, id, req := m.client, m.timeline.session, m.timeline.reqSeq
	timeout := timelineReadTimeout
	if m.timeline.tl == nil {
		timeout = timelineFirstReadTimeout
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		tl, err := client.GetTimeline(ctx, id)
		return timelineLoadedMsg{tl: tl, req: req, err: err}
	}
}

func (m *model) handleTimelineLoaded(msg timelineLoadedMsg) {
	if msg.req != m.timeline.reqSeq {
		return
	}
	m.timeline.loading = false
	if msg.err != nil {
		// A 404 is an old proxy only if it never answered for this session: once a timeline has
		// loaded, the endpoint exists, and a 404 means the session itself went away.
		if errors.Is(msg.err, apiclient.ErrNotFound) && m.timeline.tl == nil && m.sessionServed(m.timeline.session) {
			m.timeline.err = errTimelineUnsupported
			return
		}
		m.timeline.err = msg.err
		m.timeline.fails++
		m.timeline.retryAt = m.clock().Add(timelineRetryAfter(m.timeline.fails))
		return
	}
	m.timeline.tl, m.timeline.err, m.timeline.fetchedAt = msg.tl, nil, m.clock()
	m.timeline.fails, m.timeline.retryAt = 0, time.Time{}
}

// sessionServed reports a session the proxy lists, so it can be snapshotted and has a timeline.
func (m *model) sessionServed(id string) bool {
	for _, s := range m.sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}

func (m *model) handleTimelineTick(msg timelineTickMsg) tea.Cmd {
	if m.pane != paneTimeline || msg.gen != m.timeline.tickGen {
		return nil
	}
	t := &m.timeline
	var fetch tea.Cmd
	now := m.clock()
	idle := !t.fetchedAt.IsZero() && now.Sub(t.fetchedAt) >= timelineIdleRefresh && m.timelineLive()
	// A failed fetch is retried on its backoff, and holds back the other triggers until then: a
	// stream event or a live session's idle refresh would otherwise retry a dead proxy every tick.
	retry := t.err != nil && !errors.Is(t.err, errTimelineUnsupported)
	if waiting := !t.retryAt.IsZero() && now.Before(t.retryAt); (t.stale || idle || retry) && !waiting && !t.loading {
		fetch = m.fetchTimeline()
	}
	return tea.Batch(fetch, timelineTick(msg.gen))
}

// timelineLive reports a session still working: its last recorded event is recent. Not its
// last activity, which counts an open span as running until now and so would call every
// session with one open live for ever.
func (m *model) timelineLive() bool {
	t := &m.timeline
	return t.tl != nil && m.clock().Sub(t.tl.End) <= timelineLiveFor
}

// timelineKey handles the pane's own keys; false leaves the key to the global handlers.
func (m *model) timelineKey(key string) (tea.Cmd, bool) {
	t := &m.timeline
	switch key {
	case "w":
		_, _, now, _ := m.timelineRange() // the now the pane draws with, so w offers its tabs
		offered := offeredWindows(t.sessionSpan(now))
		if t.tl == nil || len(offered) == 1 {
			return nil, true // only "all": nothing to change, and a saved choice is kept
		}
		i := slices.Index(offered, t.window(now))
		t.windowIdx = offered[(i+1)%len(offered)]
		t.cursor = -1
		m.persistTimeline()
		return nil, true
	case "left", "h":
		m.moveTimelineCursor(-1)
		return nil, true
	case "right", "l":
		m.moveTimelineCursor(1)
		return nil, true
	case "up", "k":
		if lay, ok := m.timelineLayoutNow(); ok {
			// Clamped first: a window change or a resize can leave scroll past what is hidden now.
			t.scroll = max(min(t.scroll, m.timelineScrollMax(lay))-1, 0)
		}
		return nil, true
	case "down", "j":
		if lay, ok := m.timelineLayoutNow(); ok {
			t.scroll = min(t.scroll+1, m.timelineScrollMax(lay))
		}
		return nil, true
	case "enter":
		if seq, ok := m.timelineCursorSeq(); ok {
			return m.openEventsAtSeq(t.session, seq), true
		}
		return nil, true
	}
	return nil, false
}

// moveTimelineCursor steps the cursor by d cells, skipping collapsed gaps. The first press
// lands on the newest cell.
func (m *model) moveTimelineCursor(d int) {
	lay, ok := m.timelineLayoutNow()
	if !ok {
		return
	}
	n := lay.axis.width()
	c := m.timeline.cursor
	if c < 0 || c >= n {
		c = n
		d = -1
	}
	for c += d; c >= 0 && c < n; c += d {
		if !lay.axis.cells[c].gap {
			m.timeline.cursor = c
			return
		}
	}
}

// The label column and the right gutter, "│  1.6h    29m    316".
const (
	timelineLabelWidth  = 26
	timelineGutterWidth = 22
)

// timelineCells is how many cells the chart gets at the current width.
func (m *model) timelineCells() int {
	return max(m.width-timelineLabelWidth-timelineGutterWidth, 10)
}

// timelineRange is the window's [from, to] and the layout's now. For a live session now is the
// clock, so open spans grow and the window follows it. For one that is not, now is its last
// recorded event: an open span ends there, and the window ends at the session's last activity,
// so opening yesterday's session does not show an empty five minutes. A zero from means "all",
// whose to is now for a live session and zero, its last activity, for one that is not.
func (m *model) timelineRange() (from, to, now time.Time, win int) {
	t := &m.timeline
	// Rounded to the refresh interval, so the window, and the cached layout built from it,
	// move once a second rather than on every redraw.
	now = m.clock().Truncate(timelineRefresh)
	if t.tl != nil && !m.timelineLive() {
		now = t.tl.End
	}
	win = t.window(now)
	w := timelineWindows[win]
	if t.tl == nil || (w.span == 0 && !m.timelineLive()) {
		return time.Time{}, time.Time{}, now, win
	}
	if w.span == 0 {
		return time.Time{}, now, now, win
	}
	end := now
	if !m.timelineLive() {
		end = activityEnd(t.tl, now)
	}
	return end.Add(-w.span), end, now, win
}

func (m *model) timelineLayoutNow() (tlLayout, bool) {
	t := &m.timeline
	if t.tl == nil {
		return tlLayout{}, false
	}
	from, to, now, win := m.timelineRange()
	// now is rounded to the refresh interval, so a redraw within it reuses the layout.
	key := tlLayoutKey{tl: t.tl, window: win, cells: m.timelineCells(), from: from, now: now}
	if key != t.layoutKey {
		t.layout, t.layoutKey = buildTimelineLayout(t.tl, from, to, key.cells, now), key
		// A narrower layout can leave the cursor past its end, or on what is now a gap.
		if c := t.cursor; c >= t.layout.axis.width() || (c >= 0 && t.layout.axis.cells[c].gap) {
			t.cursor = -1
		}
	}
	return t.layout, true
}

// The pane's fixed lines: tabs, axis, column heads, the separator, the closing rule, the
// legend and the cursor line. Each agent and host row takes two lines, its own and the blank
// one that keeps a row's bars off the next row's.
const timelineFixedLines = 7

// tlEmptyLines is what an empty side draws in place of its rows: a note and a blank line.
func tlEmptyLines(lay tlLayout) int {
	n := 0
	if len(lay.agents) == 0 {
		n += 2
	}
	if len(lay.hosts) == 0 {
		n += 2
	}
	return n
}

// timelineRowBudget is how many two-line rows fit under the fixed lines and any empty side's
// note. A body too short for the legend drops it; see renderTimeline.
func (m *model) timelineRowBudget(lay tlLayout) int {
	fixed := timelineFixedLines + tlEmptyLines(lay)
	if m.bodyHeight < timelineFixedLines+2*4 {
		fixed-- // no legend
	}
	return max((m.bodyHeight-fixed)/2, 0)
}

// timelineAgentRows is how many agent rows fit, sharing the body with the host rows: hosts get
// up to three rows and agents the rest, agents first when only one row fits.
func (m *model) timelineAgentRows(lay tlLayout) int {
	rows := m.timelineRowBudget(lay)
	hosts := min(len(lay.hosts), 3, max(rows-1, 0))
	return max(min(len(lay.agents), rows-hosts), 0)
}

func (m *model) timelineHostRows(lay tlLayout, agentRows int) int {
	return max(min(len(lay.hosts), m.timelineRowBudget(lay)-agentRows), 0)
}

// timelineScrollMax is how far scroll can go: every agent row hidden, then every host row.
func (m *model) timelineScrollMax(lay tlLayout) int {
	agents := m.timelineAgentRows(lay)
	return len(lay.agents) - agents + len(lay.hosts) - m.timelineHostRows(lay, agents)
}

// timelineOffsets splits scroll into the first agent row and the first host row shown: the
// agent rows scroll until the last is in view, then the host rows.
func (m *model) timelineOffsets(lay tlLayout) (agent, host int) {
	s := min(m.timeline.scroll, m.timelineScrollMax(lay))
	agent = min(s, len(lay.agents)-m.timelineAgentRows(lay))
	return agent, s - agent
}

func (m *model) visibleAgentRows(lay tlLayout) []tlAgentRow {
	n := m.timelineAgentRows(lay)
	s, _ := m.timelineOffsets(lay)
	return lay.agents[s:min(len(lay.agents), s+n)]
}

func (m *model) visibleHostRows(lay tlLayout) []tlHostRow {
	n := m.timelineHostRows(lay, m.timelineAgentRows(lay))
	_, s := m.timelineOffsets(lay)
	return lay.hosts[s:min(len(lay.hosts), s+n)]
}

// scrollHint counts one side's rows scrolled out of view above and below.
func scrollHint(above, below int, noun string) string {
	var parts []string
	if above > 0 {
		parts = append(parts, fmt.Sprintf("%d %s above", above, noun))
	}
	if below > 0 {
		parts = append(parts, fmt.Sprintf("%d %s below", below, noun))
	}
	return "  " + strings.Join(parts, " · ") + "  [↑↓] scroll"
}

// cellTools are the visible agent rows' tools in one cell: those that started there, then those
// still running from before it. Agent calls are left out of the second, as they are out of the
// row's fill: their own span is the few seconds to the async ack, and the child's row carries
// the work.
func (m *model) cellTools(lay tlLayout, cell tlAxisCell) (started, running []timeline.Tool) {
	_, _, now, _ := m.timelineRange()
	visible := map[string]bool{}
	for _, r := range m.visibleAgentRows(lay) {
		visible[r.agent.ID] = true
	}
	for _, x := range m.timeline.tl.Tools {
		switch {
		case !visible[x.Agent]:
		case !x.Start.Before(cell.from) && x.Start.Before(cell.to):
			started = append(started, x)
		case x.Start.Before(cell.from) && tlEndOr(x.End, now).After(cell.from) && x.Name != "Agent" && x.Name != "Task":
			running = append(running, x)
		}
	}
	return started, running
}

// cellHost is the first request in the visible host rows to start in the cell.
func (m *model) cellHost(lay tlLayout, cell tlAxisCell) (timeline.Host, bool) {
	visible := map[string]bool{}
	for _, r := range m.visibleHostRows(lay) {
		visible[r.host] = true
	}
	for _, h := range m.timeline.tl.Hosts {
		if visible[h.Host] && !h.Start.Before(cell.from) && h.Start.Before(cell.to) {
			return h, true
		}
	}
	return timeline.Host{}, false
}

// timelineCursorSeq is the event under the cursor: the first tool the cursor line lists, else
// the request it names. The same rules as cursorStrip, so ↵ opens what the line shows.
func (m *model) timelineCursorSeq() (uint64, bool) {
	lay, ok := m.timelineLayoutNow()
	c := m.timeline.cursor
	if !ok || c < 0 || c >= lay.axis.width() {
		return 0, false
	}
	cell := lay.axis.cells[c]
	if started, running := m.cellTools(lay, cell); len(started) > 0 {
		return started[0].Seq, true
	} else if len(running) > 0 {
		return running[0].Seq, true
	}
	if h, ok := m.cellHost(lay, cell); ok {
		return h.Seq, true
	}
	return 0, false
}

// openEventsAtSeq shows session's Events pane with the cursor on event seq, once the event is
// held. A live session is always snapshotted, as Enter on Sessions does, so a stream-only
// partial cache gets its history; the cursor lands when the snapshot does.
func (m *model) openEventsAtSeq(session string, seq uint64) tea.Cmd {
	if m.selectedSess != session {
		m.selectedEventKey = eventKey{}
		delete(m.search, paneEvents)
		delete(m.search, paneDetail)
	}
	m.selectedSess = session
	m.pane = paneEvents
	// Leaving the timeline pane: its session is a #1542 retention key, so the
	// drill-down drops it the same way Esc does — the session stays watched
	// through selectedSess, which the lines above just set.
	m.timeline.session = ""
	m.revealSeq, m.revealSess = seq, session
	m.rebuildEventsTable()
	revealed := m.revealPendingSeq()
	if !m.sessionServed(session) {
		if !revealed {
			m.revealSeq, m.revealSess = 0, ""
		}
		return nil
	}
	return m.snapshotCmd(session)
}

// revealPendingSeq moves the events cursor to revealSeq if that event is on screen in its own
// session, and reports whether it was.
func (m *model) revealPendingSeq() bool {
	if m.revealSeq == 0 || m.revealSess != m.selectedSess {
		return false
	}
	for i, r := range m.visibleRows {
		if r.event != nil && r.event.Seq == m.revealSeq && eventSession(m.selectedSess, r.event) == m.selectedSess {
			setCursorVisible(&m.eventsTbl, i)
			m.selectedEventKey = keyOf(r.event)
			m.revealSeq, m.revealSess = 0, ""
			return true
		}
	}
	return false
}

// timelineBody is the pane drawn and searched. Every line is clipped to the terminal: the
// chart is sized to fit, but a header or cursor line naming a long command is not.
func (m *model) timelineBody() string {
	lines := strings.Split(strings.TrimRight(m.renderTimeline(), "\n"), "\n")
	for i, l := range lines {
		lines[i] = truncToWidth(l, max(m.width, 20))
	}
	m.timelineDoc.setLines(lines, 0)
	return m.timelineDoc.render(m.searchQuery(paneTimeline))
}

// The state colours, one per state, and the host kinds'.
var (
	colorTLTool  = lipgloss.AdaptiveColor{Light: "#D97706", Dark: "#FBBF24"}
	colorTLModel = lipgloss.AdaptiveColor{Light: "#7C3AED", Dark: "#A78BFA"}
	colorTLRetry = lipgloss.AdaptiveColor{Light: "#DC2626", Dark: "#F87171"}
	colorTLSubs  = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#0E7490"}
	colorTLUser  = lipgloss.AdaptiveColor{Light: "#CBD5E1", Dark: "#3B4558"}

	tlStateColor = [...]lipgloss.AdaptiveColor{colorTLTool, colorTLModel, colorTLRetry, colorTLSubs, colorTLUser}

	styleTLKind = map[timeline.Kind]lipgloss.Style{
		timeline.KindLLM:    lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#C4B5FD"}),
		timeline.KindMCP:    lipgloss.NewStyle().Foreground(colorOK),
		timeline.KindHTTP:   lipgloss.NewStyle().Foreground(colorInbound),
		timeline.KindTunnel: lipgloss.NewStyle().Foreground(colorMuted),
	}
	styleTLCursor = lipgloss.NewStyle().Reverse(true)
	styleTLAgent  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#67E8F9"})
)

// gapGlyph draws a collapsed gap's marker in the colour of the state that held most of the
// stretch it stands for, muted when nothing did. A subagent not alive across the gap has an
// empty share, so it draws muted too.
func gapGlyph(s share) string {
	best, bestF := stNone, 0.0
	for st := stTool; st < stNone; st++ {
		if f := s.of(st); f > bestF {
			best, bestF = st, f
		}
	}
	if best == stNone {
		return styleMuted.Render("┆")
	}
	return lipgloss.NewStyle().Foreground(tlStateColor[best]).Render("┆")
}

// tlEighths are the lower block elements, by eighths of a cell.
var tlEighths = []string{" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

// shareGlyph draws one agent cell: the lower state's colour up to its height, the upper
// state's above it, as a lower block in the lower colour on the upper colour's background. A
// terminal cell holds two colours, so this is exactly two states; see share.top2.
func shareGlyph(s share) string {
	lo, loF, hi, hiF := s.top2()
	if lo == stNone {
		return " "
	}
	loE := int(loF*8 + 0.5)
	if hi == stNone {
		if loE == 0 {
			loE = 1 // a sliver of work still shows
		}
		return lipgloss.NewStyle().Foreground(tlStateColor[lo]).Render(tlEighths[min(loE, 8)])
	}
	topE := int((loF+hiF)*8 + 0.5)
	loE = max(loE, 1)
	topE = max(topE, loE+1)
	if topE >= 8 {
		// A full cell: the lower state as a block on the upper's background.
		return lipgloss.NewStyle().Foreground(tlStateColor[lo]).Background(tlStateColor[hi]).Render(tlEighths[min(loE, 7)])
	}
	// The cell is not full, so a background would paint it to the top. Draw the larger of the
	// two alone at the combined height; the cursor line has the split.
	big := lo
	if hiF > loF {
		big = hi
	}
	return lipgloss.NewStyle().Foreground(tlStateColor[big]).Render(tlEighths[topE])
}

func (m *model) renderTimeline() string {
	t := &m.timeline
	var b strings.Builder
	switch {
	case t.err != nil && errors.Is(t.err, errTimelineUnsupported):
		b.WriteString(styleWarn.Render("  This proxy does not serve /v1/sessions/{id}/timeline — upgrade cortex to see the timeline.") + "\n")
		return b.String()
	case t.err != nil && t.tl == nil:
		// Retried on its backoff, so a slow first read reads as loading, not as a dead end.
		wait := ""
		if left := t.retryAt.Sub(m.clock()); left > 0 {
			wait = fmt.Sprintf(" in %ds", int(left.Round(time.Second).Seconds()))
		}
		b.WriteString(styleMuted.Render("  loading… the last try failed, retrying"+wait+": "+t.err.Error()) + "\n")
		return b.String()
	case t.tl == nil:
		b.WriteString(styleMuted.Render("  loading…") + "\n")
		return b.String()
	}
	// The chart needs its fixed lines, any empty side's note and one row; below that, say so
	// rather than run past the terminal.
	lay, _ := m.timelineLayoutNow()
	if m.bodyHeight < timelineFixedLines+tlEmptyLines(lay)+1 {
		b.WriteString(styleMuted.Render("  make the terminal taller to see the timeline"))
		return b.String()
	}
	_, _, now, win := m.timelineRange()
	var tabs []string
	for _, i := range offeredWindows(t.sessionSpan(now)) {
		l := timelineWindows[i].label
		if i == win {
			l = "[" + l + "]"
		}
		tabs = append(tabs, l)
	}
	fmt.Fprintf(&b, "%s  %s\n", styleTitle.Render("window "+strings.Join(tabs, " ")), styleMuted.Render(m.timelineStatus(lay)))
	width := lay.axis.width()
	b.WriteString(strings.Repeat(" ", timelineLabelWidth) + styleMuted.Render(axisLabels(lay)) + "\n")
	b.WriteString(strings.Repeat(" ", timelineLabelWidth+width) + styleMuted.Render("   model  tools  calls") + "\n")

	agentRows := m.timelineAgentRows(lay)
	agentOff, hostOff := m.timelineOffsets(lay)
	if agentRows == 0 && len(lay.agents) > 0 {
		b.WriteString(styleMuted.Render(fmt.Sprintf("  %d agents — make the terminal taller to see them", len(lay.agents))) + "\n")
	}
	if len(lay.agents) == 0 {
		b.WriteString(styleMuted.Render("  no Claude Code agents in this session") + "\n\n")
	}
	for i, r := range m.visibleAgentRows(lay) {
		b.WriteString(m.agentLine(r, lay) + "\n")
		if below := len(lay.agents) - agentOff - agentRows; i == agentRows-1 && (agentOff > 0 || below > 0) {
			b.WriteString(styleMuted.Render(scrollHint(agentOff, below, "agents")) + "\n")
		} else {
			b.WriteString("\n")
		}
	}
	b.WriteString(styleMuted.Render(strings.Repeat("╌", timelineLabelWidth+width+timelineGutterWidth)) + "\n")
	hostRows := m.timelineHostRows(lay, agentRows)
	for i, r := range m.visibleHostRows(lay) {
		b.WriteString(m.hostLine(r, lay) + "\n")
		if below := len(lay.hosts) - hostOff - hostRows; i == hostRows-1 && (hostOff > 0 || below > 0) {
			b.WriteString(styleMuted.Render(scrollHint(hostOff, below, "hosts")) + "\n")
		} else {
			b.WriteString("\n")
		}
	}
	if len(lay.hosts) == 0 {
		b.WriteString(styleMuted.Render("  no outbound requests in this window") + "\n\n")
	}
	b.WriteString(styleMuted.Render(strings.Repeat("─", timelineLabelWidth+width+timelineGutterWidth)) + "\n")
	if m.bodyHeight >= timelineFixedLines+2*4 {
		b.WriteString(m.legend() + "\n")
	}
	b.WriteString(m.cursorStrip(lay))
	return b.String()
}

func (m *model) legend() string {
	sw := func(c lipgloss.AdaptiveColor, s string) string {
		return lipgloss.NewStyle().Foreground(c).Render("█") + styleMuted.Render(" "+s+"  ")
	}
	return sw(colorTLTool, "tools") + sw(colorTLModel, "model") + sw(colorTLRetry, "retrying") +
		sw(colorTLSubs, "waiting on subagents") + sw(colorTLUser, "waiting on you") +
		styleMuted.Render("┆ time cut from the axis, in its colour   ▁…█ requests in flight")
}

func (m *model) timelineStatus(lay tlLayout) string {
	t := &m.timeline
	parts := []string{fmt.Sprintf("%s → %s", lay.from.Local().Format("01-02 15:04"), lay.to.Local().Format("01-02 15:04"))}
	if t.tl.Truncated {
		parts = append(parts, "oldest events not shown")
	}
	if t.err != nil {
		parts = append(parts, "refresh failed: "+t.err.Error())
	}
	return strings.Join(parts, " · ")
}

// axisLabels is the time axis: each collapsed gap's length centred on its marker, then a time
// every 16 cells where it does not crowd one.
func axisLabels(lay tlLayout) string {
	w := lay.axis.width()
	line := []rune(strings.Repeat(" ", w+16))
	taken := make([]bool, len(line))
	claim := func(c int, s string) {
		rs := []rune(s)
		if c < 0 || c+len(rs) > len(line) {
			return
		}
		for i := max(c-1, 0); i <= c+len(rs) && i < len(taken); i++ {
			if taken[i] {
				return
			}
		}
		for i, r := range rs {
			line[c+i], taken[c+i] = r, true
		}
	}
	for c, cell := range lay.axis.cells {
		if cell.marker {
			label := tlDuration(cell.to.Sub(cell.from))
			claim(c-len(label)/2, label)
		}
	}
	// A squeezed live wait is labelled with its length, like a gap, since its cells are not to
	// scale with the rest.
	if n := lay.axis.tail; n > 0 {
		label := "+" + tlDuration(lay.axis.cells[w-1].to.Sub(lay.axis.cells[w-n].from))
		claim(w-n, label)
	}
	format := "15:04"
	if span := lay.to.Sub(lay.from); span <= 10*time.Minute {
		format = "15:04:05"
	} else if span > 36*time.Hour {
		format = "01-02 15:04"
	}
	for c := 0; c < w-5; c += 16 {
		if !lay.axis.cells[c].gap {
			claim(c, lay.axis.cells[c].from.Local().Format(format))
		}
	}
	return strings.TrimRight(string(line[:min(len(line), w+8)]), " ")
}

func (m *model) agentLine(r tlAgentRow, lay tlLayout) string {
	label := r.agent.Label
	if r.depth > 0 {
		label = strings.Repeat("  ", r.depth) + label
	}
	style := styleTLAgent
	if r.agent.ID == timeline.MainAgent {
		style = styleTitle
	}
	var b strings.Builder
	b.WriteString(style.Render(padRight(truncToWidth(sanitizeLabel(label), timelineLabelWidth-1), timelineLabelWidth)))
	for c, cell := range r.cells {
		g := " "
		switch {
		case lay.axis.cells[c].marker:
			g = gapGlyph(cell)
		case lay.axis.cells[c].gap:
		case r.alive[c]:
			g = shareGlyph(cell)
		}
		b.WriteString(m.cursorAt(c, g))
	}
	fmt.Fprintf(&b, "%s%s%s%s", styleMuted.Render("│"),
		lipgloss.NewStyle().Foreground(colorTLModel).Render(fmt.Sprintf("%6s", tlDuration(r.modelTime))),
		lipgloss.NewStyle().Foreground(colorTLTool).Render(fmt.Sprintf("%7s", tlDuration(r.toolTime))),
		styleMuted.Render(fmt.Sprintf("%7d", r.calls)))
	return b.String()
}

func (m *model) hostLine(r tlHostRow, lay tlLayout) string {
	tag := map[timeline.Kind]string{timeline.KindLLM: "LLM", timeline.KindMCP: "MCP", timeline.KindHTTP: "", timeline.KindTunnel: "🔒"}[r.kind]
	name := padRight(truncToWidth(sanitizeLabel(r.host), timelineLabelWidth-6), timelineLabelWidth-5)
	st := styleTLKind[r.kind]
	var b strings.Builder
	b.WriteString(st.Render(name + strings.Repeat(" ", 4-lipgloss.Width(tag)) + tag + " "))
	for c := range r.peak {
		g := " "
		switch {
		case lay.axis.cells[c].marker && r.err[c]:
			g = styleError.Render("┆") // a request failed in the stretch cut from the axis
		case lay.axis.cells[c].marker:
			g = styleMuted.Render("┆")
		case lay.axis.cells[c].gap:
		case r.err[c]:
			g = styleError.Render(tlEighths[min(max(r.peak[c], 1), 8)])
		default:
			g = st.Render(tlEighths[min(r.peak[c], 8)])
		}
		b.WriteString(m.cursorAt(c, g))
	}
	b.WriteString(styleMuted.Render("│" + fmt.Sprintf("%15d req", r.requests)))
	return b.String()
}

func (m *model) cursorAt(c int, s string) string {
	if c == m.timeline.cursor {
		// Unstyled, so the cell's own reset does not undo the cursor's reverse video.
		g := ansi.Strip(s)
		if g == " " {
			g = "│"
		}
		return styleTLCursor.Render(g)
	}
	return s
}

// cursorStrip says what happened in the cell under the cursor: the visible rows' tools that
// started there, then those still running from before it, with durations and previews. A cell
// with no tool names the request ↵ would open.
func (m *model) cursorStrip(lay tlLayout) string {
	c := m.timeline.cursor
	if c < 0 || c >= lay.axis.width() {
		return styleMuted.Render("▸ [←→] pick a moment: lists the tools running then, with their durations and commands")
	}
	cell := lay.axis.cells[c]
	head := fmt.Sprintf("▸ %s  ", cell.from.Local().Format("15:04:05"))
	started, running := m.cellTools(lay, cell)
	var parts []string
	for i, x := range append(started, running...) {
		dur := "running"
		switch {
		case x.Lost:
			dur = "no result"
		case x.End != nil:
			dur = tlDuration(x.End.Sub(x.Start))
		}
		if i >= len(started) {
			dur += " (since " + x.Start.Local().Format("15:04:05") + ")"
		}
		p := fmt.Sprintf("%s %s %s", m.shortAgent(x.Agent), x.Name, dur)
		if x.Preview != "" {
			p += " " + x.Preview
		}
		// The agent label, the tool name and the preview are all the model's text.
		parts = append(parts, sanitizeLabel(p))
	}
	if len(parts) == 0 {
		if h, ok := m.cellHost(lay, cell); ok {
			return styleMuted.Render(head + "no tool running; a request to " + sanitizeLabel(h.Host) + "  [enter] events")
		}
		return styleMuted.Render(head + "no tool running in this cell")
	}
	if len(parts) > 1 {
		head += fmt.Sprintf("(%d) ", len(parts))
	}
	// Whole entries only: a duration cut in half reads as a different number. Past what fits,
	// the rest is counted.
	const hint = "  [enter] events"
	budget := max(m.width-1-lipgloss.Width(hint), 20)
	line := head + parts[0]
	if lipgloss.Width(line) > budget {
		line = truncToWidth(line, budget-1) + "…"
	}
	shown := 1
	for _, p := range parts[1:] {
		more := ""
		if shown < len(parts)-1 {
			more = fmt.Sprintf("  +%d", len(parts)-shown-1)
		}
		if lipgloss.Width(line+"  ·  "+p+more) > budget {
			break
		}
		line, shown = line+"  ·  "+p, shown+1
	}
	if rest := len(parts) - shown; rest > 0 {
		line += fmt.Sprintf("  +%d", rest)
	}
	return line + styleMuted.Render(hint)
}

func (m *model) shortAgent(id string) string {
	if id == timeline.MainAgent {
		return "main"
	}
	for _, a := range m.timeline.tl.Agents {
		if a.ID == id {
			l := []rune(a.Label)
			if len(l) > 14 {
				return string(l[:13]) + "…"
			}
			return a.Label
		}
	}
	return id
}
