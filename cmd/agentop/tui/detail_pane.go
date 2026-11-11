package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// showDetail renders one exchange on the detail page (#1681): a REQUEST section, then a
// RESPONSE section, each its own event's tree under a title and its header lines. One page
// rather than two panes side by side, because the detail is for reading long JSON, and half
// a screen wraps it badly.
//
// Each half is marshalled with SessionEvent.MarshalJSON (readable wire form — string enums,
// durationMs), then filterForDetail keeps the half's own inference fields. The response also
// leaves out the fields equal to the request's (dropShared). Yank still writes the full
// event.
//
// resetScroll is false for a re-render of the same exchange — a resize, a fetched body, a
// response that just arrived — which keeps the reader's folds, cursor and scroll.
func (m *model) showDetail(r eventRow, resetScroll bool) {
	m.detailRow = r
	m.detailEvent = r.event
	// The fixed line above the viewport takes one row of the body.
	if m.bodyHeight > 1 {
		m.detailVp.Height = m.bodyHeight - 1
	}
	lost := rowLost(r, nil, 0, m.lostRef(m.events[m.selectedSess]))
	m.detailSummary = exchangeSummary(r, m.detailNum, lost)
	m.detailTree = detailContent(r, lost)
	if resetScroll {
		// A new exchange: the folds the reader opened were for the last one.
		m.detailFolds = nil
		m.detailCursorPath, m.detailCursorOff = "", 0
	}
	aimed := m.applyToolAim()
	if aimed {
		m.detailAim = nil
	}
	m.renderDetail()
	if resetScroll {
		m.detailVp.GotoTop()
		if aimed {
			m.scrollToDetailCursor()
		}
		return
	}
	// Held position, clamped to what the new content and height can show: assigning
	// viewport.Height moves maxYOffset without touching YOffset, and SetContent only
	// clamps against the LINE COUNT — so a re-wrap at a wider terminal, or a refresh
	// whose content shrank, can leave the offset past the bottom and render the body
	// with dead space beneath it. SetYOffset is the clamping setter; a no-op when the
	// offset is already in range. Same three lines as syncHelpViewport.
	m.detailVp.SetYOffset(m.detailVp.YOffset)
	m.scrollToDetailCursor()
}

// detailContent is the page showDetail draws for r: its REQUEST and RESPONSE sections. detailHas
// searches it too, so a details search matches exactly what opening the row shows. lost picks
// the missing response's note.
func detailContent(r eventRow, lost bool) *node {
	req, resp, reqRaw, respRaw := detailHalves(r)
	return sectionsRoot(
		detailSection("request", "REQUEST", req, nil, reqRaw, r.tunnel, missingNote(r, "request", lost)),
		detailSection("response", "RESPONSE", resp, req, respRaw, nil, missingNote(r, "response", lost)),
	)
}

// detailHalves splits row r into its request and response and their captures. Either half can
// be nil: a response-only row has no request held, and a request has no response until one
// arrives. A denial on a row of its own is the request half.
func detailHalves(r eventRow) (req, resp *pipeline.SessionEvent, reqRaw, respRaw *pipeline.RawCapture) {
	if r.event != nil && r.event.Phase == pipeline.SessionResponse {
		return nil, r.event, nil, r.raw
	}
	return r.event, r.resp, r.raw, r.respRaw
}

// detailSection builds one half's section. other is the request when e is the response: the
// response leaves out what other already shows. tunnel is a CONNECT folded into the request.
// note is what the section says when e is nil.
func detailSection(path, name string, e, other *pipeline.SessionEvent, raw *pipeline.RawCapture,
	tunnel *pipeline.SessionEvent, note string) *node {
	if e == nil {
		sec, _ := sectionTree(path, "━━ "+name, nil, nil, note)
		return sec
	}
	invs := allInvocations(e)
	if tunnel != nil {
		invs = append(invs, allInvocations(tunnel)...)
	}
	title := sectionTitle(name, e, invs)
	data, err := json.Marshal(e)
	if err != nil {
		sec, _ := sectionTree(path, title, nil, nil, "error marshaling event: "+err.Error())
		return sec
	}
	data = filterForDetail(data, e.Phase)
	if other != nil {
		if od, err := json.Marshal(other); err == nil {
			data = dropShared(data, filterForDetail(od, other.Phase))
		}
	}
	data = withRaw(data, raw)
	var heads []string
	var tun string
	if tunnel != nil {
		tun = tunnelHeader(tunnel)
	}
	// The header blocks in the order they have always had: TLS, what plugins rewrote, the
	// served model, the timing, then the folded tunnel. Each helper returns nothing for an
	// event without its field, so the request and the response each get their own.
	for _, h := range []string{tlsHeader(e.TLS), rewriteHeader(e), servedHeader(e), timingHeader(e), tun} {
		if h != "" {
			heads = append(heads, h)
		}
	}
	sec, err := sectionTree(path, title, heads, data, "")
	if err != nil {
		sec, _ = sectionTree(path, title, heads, nil, ColorizeJSONBytes(data))
	}
	return sec
}

// sectionTitle is a section's title line: the half, its time, and the plugins that acted on
// it. The events row's ACTION cell merges both halves, so this is where the direction is.
func sectionTitle(name string, e *pipeline.SessionEvent, invs []pipeline.Invocation) string {
	t := "━━ " + name + " · " + e.At.Format("15:04:05.00")
	if acted := actedPlugins(invs); acted != "" {
		t += " · " + acted
	}
	return t
}

// actedPlugins names the plugins that did more than skip, by action in rank order:
// "modify tool-prune, thinking-fill · observe inference-parser". A shadow action keeps its
// "*", as the ACTION cell does.
func actedPlugins(invs []pipeline.Invocation) string {
	by := map[pipeline.InvocationAction][]string{}
	for _, iv := range invs {
		if actionRank(iv.Action) <= actionRank(pipeline.ActionSkip) {
			continue
		}
		name := iv.Plugin
		if iv.Shadow {
			name += "*"
		}
		if !slices.Contains(by[iv.Action], name) {
			by[iv.Action] = append(by[iv.Action], name)
		}
	}
	var parts []string
	for _, a := range []pipeline.InvocationAction{pipeline.ActionDeny, pipeline.ActionModify, pipeline.ActionObserve, pipeline.ActionAllow} {
		if len(by[a]) > 0 {
			parts = append(parts, string(a)+" "+strings.Join(by[a], ", "))
		}
	}
	return strings.Join(parts, " · ")
}

// missingNote is what a section says when its half is not there (#1681).
func missingNote(r eventRow, path string, lost bool) string {
	if path == "request" {
		return "request not loaded: Enter on the ▲ row in events loads it"
	}
	e := r.event
	switch {
	case e != nil && e.Phase == pipeline.SessionDenied:
		d, _ := topInvocation(allInvocations(e), func(iv pipeline.Invocation) bool {
			return iv.Action == pipeline.ActionDeny && !iv.Shadow
		})
		if d.Plugin != "" {
			return "denied by " + d.Plugin + ": no upstream call"
		}
		return "denied: no upstream call"
	case e != nil && e.Tunnel:
		return "no close recorded yet"
	case lost:
		return "no response recorded"
	}
	return "waiting for response…"
}

// exchangeSummary is the fixed line's figures: the # the row showed, then what the row's
// cells say — method, status, duration, bytes, tokens, cost — each left out when blank.
func exchangeSummary(r eventRow, num string, lost bool) string {
	var parts []string
	add := func(s string) {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if num != "" {
		add("#" + num)
	}
	add(eventMethodValue(*r.event))
	if lost {
		add("lost")
	} else {
		add(statusCell(*answer(r)))
	}
	add(durationCell(*answer(r)))
	e := *answer(r)
	e.BytesUp, e.BytesDown = exchangeBytes(r)
	add(bytesCell(e))
	if tok := exchangeTokensCell(r); tok != "" {
		add(tok + " tok")
	}
	add(exchangeCostCell(r))
	return strings.Join(parts, " · ")
}

// detailFixedLine is the line above the detail viewport (#1681): the name of the section that
// holds the top visible line, then the exchange's figures. The reader always sees which half
// is on screen, and never scrolls to the response for its status. Cut to the terminal, never
// wrapped: a wrapped line would take a row the viewport already has.
func (m *model) detailFixedLine() string {
	name := "REQUEST"
	if i := m.detailDoc.lineAtRow(m.detailVp.YOffset); i >= 0 && i < len(m.detailLines) &&
		sectionOf(m.detailLines[i].path) == "response" {
		name = "RESPONSE"
	}
	return truncToWidth(styleTitle.Render(name)+" │ "+m.detailSummary, max(m.width, 20))
}

// jumpDetailSection is Tab on the detail page: the cursor goes to the other section's title,
// and the title scrolls to the top. On inference calls the request is one to two screens
// long, so this is how the reader reaches the response.
func (m *model) jumpDetailSection() {
	want := "response"
	if sectionOf(m.detailCursorPath) == "response" {
		want = "request"
	}
	for i, l := range m.detailLines {
		if l.path == want && l.head {
			m.moveDetailCursor(i)
			m.detailVp.SetYOffset(m.detailDoc.firsts[i])
			return
		}
	}
}

// detailHalf is the event of the section the cursor is in: what y writes. Nil when that half
// is missing.
func (m *model) detailHalf() *pipeline.SessionEvent {
	req, resp, _, _ := detailHalves(m.detailRow)
	if sectionOf(m.detailCursorPath) == "response" {
		return resp
	}
	return req
}

// shownNum is the # cell of the events row under the cursor, so the fixed line quotes the
// number the reader just saw — a subagent block's own number inside a block. "" when # is
// not shown.
func (m *model) shownNum() string { return m.numAt(m.eventsTbl.Cursor()) }

// numAt is the # cell of events table row i. "" when # is not shown or i is out of range.
func (m *model) numAt(i int) string {
	cols, _ := layoutColumns(m.eventColumns, m.eventColWidths, m.width)
	rows := m.eventsTbl.Rows()
	for c, col := range cols {
		if col.id == colIndex && i >= 0 && i < len(rows) && c < len(rows[i]) {
			return strings.TrimSpace(rows[i][c])
		}
	}
	return ""
}

// renderDetail draws the event detail pane from the tree showDetail built, with the folds the
// reader chose and the search query in force, and puts the cursor back on its node.
func (m *model) renderDetail() {
	q := m.searchQuery(paneDetail)
	m.detailLines = m.detailLines[:0]
	if m.detailTree != nil {
		m.detailLines = append(m.detailLines, renderTree(m.detailTree, foldState{open: m.detailFolds, query: q})...)
	}
	texts := make([]string, len(m.detailLines))
	for i, l := range m.detailLines {
		texts[i] = l.text
	}
	m.detailDoc.gutter = true
	m.detailDoc.cursor = m.cursorLineFor(m.detailCursorPath, m.detailCursorOff)
	m.detailDoc.setLines(texts, m.detailVp.Width)
	m.detailVp.SetContent(m.detailDoc.render(q))
}

// cursorLineFor is the line the cursor goes back to after a re-render: line off of the node at
// path, else that node's last line, else the first line of the nearest node above it still
// drawn. The root is the node "".
func (m *model) cursorLineFor(path string, off int) int {
	for p := path; ; p = parentPath(p) {
		first, last := -1, -1
		for i, l := range m.detailLines {
			if l.path == p {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		switch {
		case first < 0 && p != "":
			continue
		case first < 0:
			return 0
		case p != path || off == 0:
			return first
		case first+off < len(m.detailLines) && m.detailLines[first+off].path == p:
			return first + off
		}
		return last
	}
}

// moveDetailCursor puts the cursor on line i, clamped, and scrolls only as far as it takes
// to keep the line's rows on screen. Drawing is only the gutter: the tree is not rebuilt.
// A line taller than the view shows its top when the view moves down to it, and its bottom
// when the view moves up to it. A line that fills the view does not move it.
func (m *model) moveDetailCursor(i int) {
	if len(m.detailLines) == 0 {
		return
	}
	m.setDetailCursor(i)
	m.detailVp.SetContent(m.detailDoc.render(m.searchQuery(paneDetail)))
	m.scrollToDetailCursor()
}

// scrollToDetailCursor scrolls only as far as it takes to keep the cursor line's rows on screen.
func (m *model) scrollToDetailCursor() {
	first, last := m.detailDoc.rowsOf(m.detailDoc.cursor)
	top, bottom := m.detailVp.YOffset, m.detailVp.YOffset+m.detailVp.Height-1
	switch {
	case first < top && last < bottom:
		m.detailVp.SetYOffset(max(first, last-m.detailVp.Height+1))
	case last > bottom && first > top:
		m.detailVp.SetYOffset(min(first, last-m.detailVp.Height+1))
	}
}

// setDetailCursor puts the cursor on line i, clamped, without drawing, and returns the line.
func (m *model) setDetailCursor(i int) int {
	i = min(max(i, 0), max(len(m.detailLines)-1, 0))
	m.detailDoc.cursor = i
	if i < len(m.detailLines) {
		p := m.detailLines[i].path
		m.detailCursorPath, m.detailCursorOff = p, i-slices.IndexFunc(m.detailLines, func(l treeLine) bool { return l.path == p })
	}
	return i
}

// toggleDetailFold is Enter on the event detail pane: it opens or closes the fold whose head
// is under the cursor, and does nothing on any other line.
func (m *model) toggleDetailFold() {
	i := m.detailDoc.cursor
	if i < 0 || i >= len(m.detailLines) || !m.detailLines[i].head {
		return
	}
	l := m.detailLines[i]
	m.detailAim = nil
	if m.detailFolds == nil {
		m.detailFolds = map[string]foldChoice{}
	}
	m.detailFolds[l.path] = foldChoice{open: !l.open, query: m.searchQuery(paneDetail)}
	m.detailCursorPath, m.detailCursorOff = l.path, 0
	m.renderDetail()
	m.moveDetailCursor(m.cursorLineFor(l.path, 0))
}

// openRawMatch opens a capture when the details search's query matches in it and nowhere the
// pane draws: a query does not open the capture by itself (isOpen). Each half has its own capture,
// under its section (#1681). A fold the reader chose holds.
func (m *model) openRawMatch() {
	q := m.searchQuery(paneDetail)
	if !m.searchDetails || q == "" || q != m.searchQuery(paneEvents) || len(m.detailDoc.matches) > 0 || m.detailTree == nil {
		return
	}
	r := renderer{fs: foldState{query: q}, memo: map[*node]bool{}}
	for _, sec := range m.detailTree.kids {
		if len(sec.kids) == 0 {
			continue // a missing half
		}
		raw := sec.kids[0].child("raw")
		if raw == nil {
			continue
		}
		if _, chosen := m.detailFolds[raw.path]; chosen || !r.hasMatch(raw) {
			continue
		}
		if m.detailFolds == nil {
			m.detailFolds = map[string]foldChoice{}
		}
		m.detailFolds[raw.path] = foldChoice{open: true, query: q}
		m.renderDetail()
		return
	}
}

// revealDetailMatch puts the cursor on the first match of the details search's query while the
// reader has not moved it from the top: on a long event the match is often below the first
// screen. It runs when the row opens, and again when its full event arrives.
func (m *model) revealDetailMatch() {
	q := m.searchQuery(paneDetail)
	if !m.searchDetails || q == "" || q != m.searchQuery(paneEvents) ||
		m.detailCursorPath != "" || m.detailCursorOff != 0 || m.detailVp.YOffset != 0 {
		return
	}
	if s := m.surface(paneDetail); s != nil {
		if rows := s.matchLines(); len(rows) > 0 {
			s.reveal(rows[0])
		}
	}
}

// clearFoldQueries is a changed search on the event detail pane: a fold the reader closed
// under the old one is closed as if under no search, so a later search opens it when it holds
// a match.
func (m *model) clearFoldQueries() {
	for p, c := range m.detailFolds {
		c.query = ""
		m.detailFolds[p] = c
	}
}

// detailCursorKey moves the event detail pane's cursor for a motion key, and reports whether
// msg was one. On a line taller than the view, a key scrolls through the line's rows first,
// and moves the cursor off the line when its end in that direction is on screen.
func (m *model) detailCursorKey(msg string) bool {
	i, page, half := m.detailDoc.cursor, max(m.detailVp.Height-1, 1), max(m.detailVp.Height/2, 1)
	var step int
	switch msg {
	case "up", "k":
		step = -1
	case "down", "j":
		step = 1
	case "pgup", "b":
		step = -page
	case "pgdown", "pgdn", "f", " ":
		step = page
	case "ctrl+u":
		step = -half
	case "d", "ctrl+d":
		step = half
	case "g":
		m.detailAim = nil
		m.moveDetailCursor(0)
		return true
	case "G":
		m.detailAim = nil
		m.moveDetailCursor(len(m.detailLines) - 1)
		return true
	default:
		return false
	}
	m.detailAim = nil // a reader move is never undone
	first, last := m.detailDoc.rowsOf(i)
	top, tall := m.detailVp.YOffset, last-first >= m.detailVp.Height
	switch {
	case tall && step > 0 && last >= top+m.detailVp.Height:
		m.detailVp.SetYOffset(min(max(top+step, first), last-m.detailVp.Height+1))
	case tall && step < 0 && first < top:
		m.detailVp.SetYOffset(max(min(top+step, last-m.detailVp.Height+1), first))
	case step == 1 || step == -1:
		m.moveDetailCursor(i + step)
	case step > 0:
		m.moveDetailCursor(max(m.detailDoc.lineAtRow(m.detailDoc.firsts[i]+step), i+1))
	default:
		m.moveDetailCursor(m.detailDoc.lineAtRow(m.detailDoc.firsts[i] + step))
	}
	return true
}

// timingHeader decomposes a response row's duration the way #1251 asked for:
// how long the origin took to answer (ttfb) against how long the body then
// took. Duration alone cannot tell an 11-minute response with 217 tokens
// apart from a fast one — the two need different fixes, and the reader
// sorting by duration is asking exactly that question. The body's share is
// Duration − Ttfb rather than a third measurement: the row already holds
// both ends of the span. Response rows only, and only when ttfb exists — a
// failed exchange has no headers to time, and a request row no duration.
//
//	timing:  ttfb 412ms · body 10m32s of 11m07s total
func timingHeader(e *pipeline.SessionEvent) string {
	if e.Phase != pipeline.SessionResponse || e.Ttfb <= 0 || e.Duration == 0 {
		return ""
	}
	body := e.Duration - e.Ttfb
	if body < 0 {
		body = 0
	}
	// formatDuration renders zero blank because a zero in the DURATION column
	// means "the listener counted nothing" — an unknown. This zero is a computed
	// difference and means the body took under a millisecond, which the sentence
	// wants said rather than elided: a blank would print "body  of 412ms total".
	bodyText := formatDuration(body)
	if body == 0 {
		bodyText = "0ms"
	}
	return fmt.Sprintf("timing:      ttfb %s · body %s of %s total",
		formatDuration(e.Ttfb), bodyText, formatDuration(e.Duration))
}

// tunnelHeader summarizes the CONNECT tunnel folded into a TLS-bridged
// request row: the bridged origin (host:port) and any gate invocations that
// ran on the tunnel-open before the bytes were decrypted. Lets an operator
// see the connection-level decision without a separate row.
//
//	tunnel:  CONNECT example.com:443 (TLS bridge)
//	  jwt-validation skip (no_inbound_identity)
func tunnelHeader(tunnel *pipeline.SessionEvent) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("tunnel:  CONNECT %s (TLS bridge)", tunnel.Host))
	for _, iv := range allInvocations(tunnel) {
		b.WriteString(fmt.Sprintf("\n  %s %s", iv.Plugin, iv.Action))
		if iv.Reason != "" {
			b.WriteString(" (" + iv.Reason + ")")
		}
	}
	return b.String()
}

// redirectHeader names both hosts of a request a plugin sent elsewhere. The events
// table's host column already shows where it went; this line says where the client
// asked for, which is the other half of the story. Empty when nothing redirected it,
// so the caller can prepend unconditionally.
//
//	redirected:  ete-litellm.example.com → glm-litellm.example.com
func redirectHeader(e *pipeline.SessionEvent) string {
	if e.RequestedHost == "" {
		return ""
	}
	// RequestedHost is the client's Host header, and Host can be too: both pass
	// sanitizeLabel, as every caller-supplied label agentop prints does.
	return fmt.Sprintf("redirected:  %s → %s", sanitizeLabel(e.RequestedHost), sanitizeLabel(e.Host))
}

// upgradeHeader says a request asked to switch protocol and that Cortex did not pass
// the request on as one. Empty on every request that asked for nothing, which is nearly
// all of them, so the caller can prepend unconditionally.
//
//	upgrade:     websocket not forwarded — the client is expected to retry over HTTPS
//
// It says what happens next because that is the question this line exists to answer:
// the client's own output reports a failed WebSocket handshake, and without this the
// user blames Cortex. Cortex must not forward Upgrade (RFC 9110 makes it hop-by-hop),
// so the request either reaches the origin as a bare GET — which an endpoint wanting
// a POST or an upgrade answers 405 — or, on a Responses API path, is answered 426 by
// the proxy itself (listener.responses_websockets). Codex falls back to
// HTTPS after either.
//
// The wording holds for both because this line is drawn from the REQUEST row, which
// cannot tell them apart: which one happened is the paired response's status, beside
// it on the timeline. It used to say "origin may answer 405", which was wrong for every
// Codex handshake once the proxy began declining them.
func upgradeHeader(e *pipeline.SessionEvent) string {
	if e.UpgradeStripped == "" {
		return ""
	}
	// Sanitised at record time already; passed through sanitizeLabel anyway, as
	// every caller-supplied label agentop prints is, so one rule covers them all.
	return fmt.Sprintf("upgrade:     %s not forwarded — the client is expected to retry over HTTPS",
		sanitizeLabel(e.UpgradeStripped))
}

// modelHeader names both models of a request a plugin sent for another model: the
// one the client asked for, then the one it was sent for. The events table's model
// column already shows the second. Empty when nothing changed the model.
//
//	model:       claude-opus-5-5 → glm-5.3
func modelHeader(e *pipeline.SessionEvent) string {
	if e.Inference == nil || e.Inference.RequestedModel == "" {
		return ""
	}
	// RequestedModel is the client's model name, and the session API is
	// unauthenticated: both names pass sanitizeLabel before they reach the terminal.
	return fmt.Sprintf("model:       %s → %s", sanitizeLabel(e.Inference.RequestedModel), sanitizeLabel(e.Inference.Model))
}

// servedHeader names the model the upstream says answered, when that is not the model
// the request was sent for: a gateway that resolved an alias, or a provider that answered
// with a dated version. The events table's model column keeps the requested name. Empty
// when the upstream named no other model.
//
//	served:      bedrock/us.anthropic.claude-opus-4-6-v1
func servedHeader(e *pipeline.SessionEvent) string {
	if e.Inference == nil || e.Inference.ServedModel == "" {
		return ""
	}
	// Upstream-supplied, and the session API is unauthenticated: sanitizeLabel, as for every
	// label agentop prints.
	return "served:      " + sanitizeLabel(e.Inference.ServedModel)
}

// rewriteHeader is how the request that went upstream differs from the one the
// client sent, as one block: redirectHeader's line, then modelHeader's, then
// upgradeHeader's, each only when it applies. Empty when none does, so the caller
// can prepend unconditionally.
//
// Not all of it is a plugin's doing, which is why this is no longer described as
// such: the first two are (pctx.Redirect, pctx.SetRequestModel) and the third is
// the proxy's own hop-by-hop strip. What groups them is the reader's question —
// "why doesn't this row match what my agent asked for" — and that is the same
// question whoever did it.
func rewriteHeader(e *pipeline.SessionEvent) string {
	var lines []string
	for _, line := range []string{redirectHeader(e), modelHeader(e), upgradeHeader(e)} {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// tlsHeader builds a one-block summary of the TLS connection state.
// Returns the empty string when tls is nil (plaintext events) so the
// caller can prepend unconditionally.
//
// The block stays on three lines so it fits in the detail pane
// without pushing the JSON off-screen on small terminals:
//
//	TLS:
//	  version: TLS 1.3 · cipher: TLS_AES_128_GCM_SHA256
//	  peer:    spiffe://rossoctl.local/ns/team1/sa/caller-agent
//
// Empty fields are skipped so the block stays terse on partial data.
func tlsHeader(tls *pipeline.EventTLS) string {
	if tls == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("TLS:")
	if tls.Version != "" || tls.CipherSuite != "" {
		parts := []string{}
		if tls.Version != "" {
			parts = append(parts, fmt.Sprintf("version: %s", tls.Version))
		}
		if tls.CipherSuite != "" {
			parts = append(parts, fmt.Sprintf("cipher: %s", tls.CipherSuite))
		}
		b.WriteString("\n  " + strings.Join(parts, " · "))
	}
	if tls.PeerSPIFFEID != "" {
		b.WriteString(fmt.Sprintf("\n  peer:    %s", tls.PeerSPIFFEID))
	}
	return b.String()
}

// filterForDetail rewrites the TUI-side view of a SessionEvent so the
// inference and mcp extensions only expose the fields relevant to the
// event's phase. Request events drop response-side fields (completion,
// tokens, toolCalls, mcp result/error); response events drop request-side
// fields (messages, tools, mcp params). The underlying event is unchanged.
func filterForDetail(data []byte, phase pipeline.SessionPhase) []byte {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	resp := phase == pipeline.SessionResponse
	keep := sessionapi.DetailKeys("inference", resp)
	mcpKeep := sessionapi.DetailKeys("mcp", resp)
	a2aKeep := sessionapi.DetailKeys("a2a", resp)
	if inf, ok := m["inference"].(map[string]any); ok {
		restoreReportedZeroSplits(inf)
		inf = filterFields(inf, keep)
		if phase != pipeline.SessionResponse {
			nestToolBlocks(inf)
		}
		m["inference"] = inf
	}
	if mcp, ok := m["mcp"].(map[string]any); ok {
		m["mcp"] = filterFields(mcp, mcpKeep)
	}
	if a2a, ok := m["a2a"].(map[string]any); ok {
		m["a2a"] = filterFields(a2a, a2aKeep)
	}
	// ONE cost record, not two.
	//
	// The proxy publishes the record under the current key AND the legacy plugin-name
	// key, so an agentop older than the rename keeps showing cost at all. This agentop reads
	// the current one, so rendering both would show an operator the same object twice
	// under two names and give them no way to tell which is authoritative.
	//
	// Dropped from the VIEW only. yankEventToFile marshals the event itself, so the
	// yanked file still holds exactly what went over the wire — the same split this
	// function already makes for identity, one line down.
	if pl, ok := m["plugins"].(map[string]any); ok {
		if _, current := pl[event.Key]; current {
			delete(pl, event.PluginName)
		}
	}
	// Identity is summarized at the session level (events pane banner).
	// Drop it from per-event detail rows to reduce repetition — the full
	// value is still in the wire JSON that yank writes out.
	delete(m, "identity")
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

// sharedWithRequest are the response fields that repeat the request's on almost every
// exchange: about 10 rows of every RESPONSE section on real traffic (#1681).
var sharedWithRequest = []string{"client", "direction", "host", "httpMethod", "httpPath", "requestId", "sessionId"}

// dropShared leaves out of resp each sharedWithRequest field whose value equals req's. A field
// that differs stays — a redirected response's host is news. at always stays: it is the
// response's own time. Both inputs are filterForDetail's output, so equal values are equal
// bytes. Any decode failure returns resp as it was.
func dropShared(resp, req []byte) []byte {
	var a, b map[string]json.RawMessage
	if json.Unmarshal(resp, &a) != nil || json.Unmarshal(req, &b) != nil {
		return resp
	}
	for _, k := range sharedWithRequest {
		if v, ok := a[k]; ok && bytes.Equal(v, b[k]) {
			delete(a, k)
		}
	}
	out, err := json.Marshal(a)
	if err != nil {
		return resp
	}
	return out
}

// restoreReportedZeroSplits puts back a split the provider MEASURED AND FOUND TO BE ZERO.
//
// InferenceExtension.ReasoningTokens is omitempty, so a provider that measured the split and
// got none serialises identically to one that never measured at all — and filterFields keeps
// only keys the map actually has, so both read as "no reasoning" in the pane that
// the response list of sessionapi.DetailKeys calls the place the exact figure lives. A measured zero is a
// figure; absence is the lack of one. PresentKinds is the only thing that separates them,
// and it travels in the same object.
//
// THE PANE, NOT THE WIRE FORMAT. Dropping omitempty upstream would publish a zero for every
// provider that never reports a split — the opposite error, and the one the tier panel's
// JSON tests forbid. So the repair belongs here, at the surface that lost the distinction.
//
// NO PHASE GUARD. Reasoning is response-side, but what keeps it off a request row is
// the request list of sessionapi.DetailKeys not listing it — one allow-list deciding visibility, per this file's
// existing rule about a second copy of a predicate. A phase check here would be a guard the
// allow-list already makes unreachable, so nothing could prove it worked.
//
// presentKinds is itself absent from both allow-lists and stays that way: the reader gets
// the figure, not the bitfield.
func restoreReportedZeroSplits(inf map[string]any) {
	if _, ok := inf["reasoningTokens"]; ok {
		return // a non-zero figure cleared omitempty on its own
	}
	// json.Unmarshal into `any` numbers everything as float64, including a uint8 bitfield.
	bits, ok := inf["presentKinds"].(float64)
	if !ok {
		return // no presence bits: a producer predating them, where absence is all we know
	}
	if uint8(bits)&usage.KindReasoning != 0 {
		inf["reasoningTokens"] = 0
	}
}

// nestToolBlocks moves each history tool call and tool result under the message that
// carried it, named by the message's toolUseIds, so a turn reads as "called Bash with this
// command" and "got this back" in place. The parser keeps both out of the message on purpose
// (no plugin may read tool output as conversation); the view puts them back together. An
// entry no message names stays in its top-level list, and a list left empty is dropped.
func nestToolBlocks(inf map[string]any) {
	msgs, _ := inf["messages"].([]any)
	// Each top-level list, the key that holds its id, the key it takes under a message
	// ("toolCalls" reads as what the message asked for, as on a response row), and which
	// messages it goes under: the same id names the call on the assistant message and the
	// result on the message after it.
	for _, l := range [...]struct {
		from, id, to string
		assistant    bool
	}{
		{"historyToolCalls", "id", "toolCalls", true},
		{"toolResults", "toolUseId", "toolResults", false},
	} {
		entries, _ := inf[l.from].([]any)
		byID := map[string]int{}
		for i, e := range entries {
			obj, _ := e.(map[string]any)
			if id, _ := obj[l.id].(string); id != "" {
				if _, dup := byID[id]; !dup {
					byID[id] = i
				}
			}
		}
		nested := make([]bool, len(entries))
		for _, raw := range msgs {
			msg, _ := raw.(map[string]any)
			ids, _ := msg["toolUseIds"].(string)
			if ids == "" || (msg["role"] == "assistant") != l.assistant {
				continue
			}
			for _, id := range strings.Split(ids, ", ") {
				if i, ok := byID[id]; ok && !nested[i] {
					got, _ := msg[l.to].([]any)
					msg[l.to] = append(got, entries[i])
					nested[i] = true
				}
			}
		}
		var rest []any
		for i, e := range entries {
			if !nested[i] {
				rest = append(rest, e)
			}
		}
		if len(rest) == 0 {
			delete(inf, l.from)
		} else {
			inf[l.from] = rest
		}
	}
	// The ids did their job: each one is on the entry it names, under its message, or on an
	// entry still in its top-level list.
	for _, raw := range msgs {
		if msg, ok := raw.(map[string]any); ok {
			delete(msg, "toolUseIds")
		}
	}
}

// filterFields returns a new map containing only the keys in `keep` that are
// present in obj. Keys not listed are dropped. This is strict filtering —
// unlike a partition, fields absent from the allow-list do not pass through.
func filterFields(obj map[string]any, keep []string) map[string]any {
	out := map[string]any{}
	for _, k := range keep {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	return out
}
