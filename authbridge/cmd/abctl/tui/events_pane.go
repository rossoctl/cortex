package tui

import (
	"fmt"
	"net"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// newEventsTable builds an empty events table. Uses the shared tableStyles
// (including the Reverse-based Selected highlight) like the other panes —
// now safe because per-cell ANSI coloring was removed from this table.
func newEventsTable() table.Model {
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "#", Width: 4},
			{Title: "TIME", Width: 12},
			{Title: "DIR", Width: 4},
			{Title: "PHASE", Width: 7},
			{Title: "ACTION", Width: actionColWidth},
			{Title: "PLUGIN", Width: 18},
			// methodColWidth rather than 22: the widest realistic value is a model
			// name ("claude-opus-5"), and the columns freed pay for splitting
			// TOKENS and COST apart below.
			{Title: "METHOD", Width: methodColWidth},
			{Title: "STATUS", Width: 7},
			{Title: "DURATION", Width: 10},
			// 17, not 15: sized for a SEVEN-digit prompt, "1,048,576(−12.3k)".
			// Million-token contexts are in service, and bubbles truncates a cell
			// at the column width, so 15 rendered "1,048,576(−1…" — dropping the
			// saving, which is the half of this cell that appears nowhere else.
			{Title: "TOKENS", Width: 17},
			// 19 fits the widest cell the formatter can produce:
			// "<$0.0001(−<$0.0001)", where both halves fell under the
			// four-decimal floor. The ordinary shape is "$0.2546(−$0.0037)" at 17.
			{Title: "COST", Width: 19},
			{Title: "HOST", Width: 20},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// eventRow is one display row — exactly one network message. The cartesian
// "row per plugin invocation" model is gone: a message touched by N plugins
// is a single row, with the per-plugin breakdown available in the detail
// pane. tunnel, when non-nil, is a CONNECT tunnel-open event folded into a
// TLS-bridged request (see buildEventRows); it contributes a summary line to
// the detail pane but no separate row.
type eventRow struct {
	event  *pipeline.SessionEvent
	tunnel *pipeline.SessionEvent
}

// invocations returns every plugin invocation the row's ACTION/PLUGIN cell and
// the inactive filter should consider: the event's own, plus any folded
// CONNECT tunnel's gate invocations — so a bridged row reflects activity on the
// tunnel-open (e.g. an egress gate that allowed the CONNECT) and isn't wrongly
// hidden or under-reported.
func (er eventRow) invocations() []pipeline.Invocation {
	invs := allInvocations(er.event)
	if er.tunnel != nil {
		invs = append(invs, allInvocations(er.tunnel)...)
	}
	return invs
}

// rebuildEventsTable populates the events table from the cache for the
// currently selected session, applying filter + preserving cursor. Also
// resizes the table height to account for the IDENTITY banner — when
// the session has inbound identity, subtract the banner's rendered
// height so it doesn't push rows off-screen; otherwise claim the full
// body height.
func (m *model) rebuildEventsTable() {
	events := m.events[m.selectedSess]

	if m.bodyHeight > 0 {
		h := m.bodyHeight
		if len(distinctInboundIdentities(events)) > 0 {
			h -= identityBannerHeight
		}
		if h < 3 {
			h = 3
		}
		m.eventsTbl.SetHeight(h)
	}

	prevRow := m.eventsTbl.Cursor()
	wasAtEnd := prevRow >= len(m.eventsTbl.Rows())-1

	// One display row per network message. CONNECT tunnel-opens are folded
	// into the decrypted inner request that immediately follows them (TLS
	// bridge), so a bridged call reads as a single request row — the same
	// shape as a plaintext call.
	eventRows := buildEventRows(events)

	// Pair request rows with their response rows. ids drives the # column: one
	// integer repeated across a request/response exchange, which is how an
	// exchange is read off the timeline.
	ids, partner := computeEventPairs(eventRows)

	// One selection per rebuild, fitted to the terminal. Both the header and every
	// row cell come from `cols`, so they cannot disagree.
	cols, dropped := fitColumns(selectedColumns(m.eventColumns), m.width)
	m.eventColsDropped = dropped

	// Rows are built first and handed to the table together with their columns at
	// the end of this function — see the note there on why the order matters.
	rows := make([]table.Row, 0, len(eventRows))
	m.visibleRows = m.visibleRows[:0]
	m.hiddenInactive = 0
	for i, er := range eventRows {
		if m.filter != "" && !matchEventRow(er, m.filter) {
			continue
		}
		// hideInactive (the `s` toggle) is off by default — every message is
		// shown, including passthrough/skip-only ones, per "I should see all
		// network messages". Turning it on focuses the timeline on plugin
		// activity (deny/modify/observe/allow). Both the filter and the
		// headline consider the folded tunnel's invocations too.
		invs := er.invocations()
		if m.hideInactive && eventInactive(invs) {
			m.hiddenInactive++
			continue
		}

		// Cells come from the selected columns, in their order — see
		// events_columns.go. Previously this was a positional table.Row literal
		// parallel to a []table.Column slice, so inserting a column meant editing
		// both in step and a mismatch shifted every later cell under the wrong
		// heading.
		//
		// PHASE carries no bracket glyphs. They were box-drawing corners (┌/│/└)
		// meant to visually connect a request to its response, and they could only
		// ever be correct for exchanges that NEST. Concurrent requests cross
		// instead: A starts, B starts, A ends, B ends — for which a tree has no
		// notation, so both rows claimed to contain each other and the output was
		// actively misleading. The # column pairs exchanges exactly (by the
		// proxy-stamped RequestID), which is what the glyphs approximated.
		cc := cellContext{m: m, rows: eventRows, partner: partner, i: i, row: er, ids: ids}
		row := make(table.Row, 0, len(cols))
		for _, c := range cols {
			row = append(row, c.cell(cc))
		}
		rows = append(rows, row)
		m.visibleRows = append(m.visibleRows, er)
	}
	// Clear, set columns, then set the matching rows.
	//
	// SetColumns calls UpdateViewport, which re-renders whatever rows are loaded,
	// and bubbles' renderRow walks the ROW's cells while indexing m.cols[i] — so a
	// row with more cells than there are columns reads past the end and panics
	// ("index out of range [10] with length 10"). Toggling a column off is exactly
	// that. Clearing first leaves SetColumns nothing to mis-render.
	m.eventsTbl.SetRows(nil)
	m.eventsTbl.SetColumns(tableColumns(cols))
	m.eventsTbl.SetRows(rows)

	// Auto-follow: if user was at the bottom, stay at the bottom. Otherwise
	// preserve position so reading isn't disturbed by new events.
	if wasAtEnd && len(rows) > 0 {
		m.eventsTbl.SetCursor(len(rows) - 1)
	} else if prevRow < len(rows) {
		m.eventsTbl.SetCursor(prevRow)
	}
}

// selectedEvent returns the event at the cursor row, or nil. The cursor
// points into m.visibleRows (one entry per rendered message), and each row
// carries a reference to its source event.
func (m *model) selectedEvent() *pipeline.SessionEvent {
	er, ok := m.selectedEventRow()
	if !ok {
		return nil
	}
	return er.event
}

// selectedEventRow returns the full row (event + any folded tunnel) under the
// cursor. ok is false when there are no rendered rows or the cursor is out of
// range.
func (m *model) selectedEventRow() (eventRow, bool) {
	if len(m.visibleRows) == 0 {
		return eventRow{}, false
	}
	cur := m.eventsTbl.Cursor()
	if cur < 0 || cur >= len(m.visibleRows) {
		return eventRow{}, false
	}
	return m.visibleRows[cur], true
}

// buildEventRows turns the chronological event slice into display rows, one
// per network message. The only folding is the TLS-bridge CONNECT pair: a
// tunnel-open event (opaque, host:port) immediately followed by the decrypted
// inner request for the same host is rendered as a single row keyed on the
// inner request, with the tunnel attached for the detail pane. A non-bridged
// (passthrough) tunnel has no inner request following it, so it stands as its
// own row — it IS the whole message.
func buildEventRows(events []pipeline.SessionEvent) []eventRow {
	rows := make([]eventRow, 0, len(events))
	for i := 0; i < len(events); i++ {
		e := &events[i]
		if i+1 < len(events) && isTunnelOpen(e) {
			inner := &events[i+1]
			if isBridgedInner(e, inner) {
				rows = append(rows, eventRow{event: inner, tunnel: e})
				i++ // consume the inner request too
				continue
			}
		}
		rows = append(rows, eventRow{event: e})
	}
	return rows
}

// isTunnelOpen reports whether e is a CONNECT / transparent-redirect
// tunnel-open. It keys on the explicit Tunnel marker the producer
// (recordTunnelOpened) sets — NOT on host/extension shape, which an ordinary
// unparsed outbound request could mimic and get wrongly folded.
func isTunnelOpen(e *pipeline.SessionEvent) bool {
	return e.Tunnel
}

// isBridgedInner reports whether inner is the decrypted request the TLS bridge
// produced right after opening tunnel. The two fold into one row: tunnel
// carries "host:port" + opaque bytes, inner carries the same host (the Host
// header, usually port-stripped) plus the real method/path. Matching on the
// host-part (port-insensitive) handles both the standard-443 case (inner host
// = "example.com") and the non-standard case (inner host = "example.com:8443").
//
// Two guards keep unrelated events from folding:
//   - inner must be another outbound REQUEST, never a response, so a plain
//     request→response exchange isn't mistaken for a bridged pair.
//   - inner must NOT itself be a tunnel-open (Tunnel marker). Two back-to-back
//     passthrough CONNECTs to the same host (common with connection pooling)
//     would otherwise fold — hiding one real message and mislabeling the other.
func isBridgedInner(tunnel, inner *pipeline.SessionEvent) bool {
	host := hostOnly(inner.Host)
	return inner.Direction == pipeline.Outbound &&
		inner.Phase == pipeline.SessionRequest &&
		!isTunnelOpen(inner) &&
		host != "" &&
		host == hostOnly(tunnel.Host)
}

// hostOnly returns the host portion of a "host:port" string, or the input
// unchanged when it carries no port. Handles bracketed IPv6 literals.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// allInvocations returns every plugin invocation on an event, both
// directions concatenated (inbound first). Returns nil when the event
// carries no Invocations.
func allInvocations(e *pipeline.SessionEvent) []pipeline.Invocation {
	if e == nil || e.Invocations == nil {
		return nil
	}
	out := make([]pipeline.Invocation, 0, len(e.Invocations.Inbound)+len(e.Invocations.Outbound))
	out = append(out, e.Invocations.Inbound...)
	out = append(out, e.Invocations.Outbound...)
	return out
}

// actionRank orders the five invocation verbs for at-a-glance aggregation,
// highest wins: deny > modify > observe > allow > skip. The pipeline's own
// outcome only distinguishes deny vs allow (pipeline/outcome.go); the rest of
// this ordering is an abctl display choice. deny/modify rank top because they
// changed the message's fate. observe ranks ABOVE allow on purpose: a parser
// that understood the message (and supplied the METHOD shown on the row) tells
// the operator more than a gate that merely permitted it — and surfacing the
// gate while METHOD came from the parser reads as inconsistent. skip is the
// floor (a plugin ran but didn't apply); 0 is the sentinel for "no plugin
// acted".
func actionRank(a pipeline.InvocationAction) int {
	switch a {
	case pipeline.ActionDeny:
		return 5
	case pipeline.ActionModify:
		return 4
	case pipeline.ActionObserve:
		return 3
	case pipeline.ActionAllow:
		return 2
	case pipeline.ActionSkip:
		return 1
	}
	return 0
}

// topInvocation returns the highest-ranked invocation satisfying keep and how
// many tied at that top rank. winner is the zero Invocation (Action "", rank 0)
// when none satisfy keep.
func topInvocation(invs []pipeline.Invocation, keep func(pipeline.Invocation) bool) (pipeline.Invocation, int) {
	best := -1
	count := 0
	var winner pipeline.Invocation
	for _, iv := range invs {
		if !keep(iv) {
			continue
		}
		switch r := actionRank(iv.Action); {
		case r > best:
			best, count, winner = r, 1, iv
		case r == best:
			count++
		}
	}
	return winner, count
}

// shadowFlagged reports whether any invocation is a shadow deny/modify — a
// plugin that ran under on_error: observe and WOULD have blocked or rewritten
// the message, but didn't (the framework converted its Reject to a pass and
// set Shadow). These are the signal a shadow-policy rollout is watching for.
func shadowFlagged(invs []pipeline.Invocation) bool {
	for _, iv := range invs {
		if iv.Shadow && (iv.Action == pipeline.ActionDeny || iv.Action == pipeline.ActionModify) {
			return true
		}
	}
	return false
}

// actionColWidth is the ACTION column's width. Named rather than inlined in the
// column literal so a test can assert against the real value: transcribing it
// into the test gave two independent 8s, and narrowing the column left the test
// passing while the label overflowed.
const actionColWidth = 8

// methodColWidth is the METHOD column's width, shared with eventMethod for the
// same reason actionColWidth is named: two independent numbers drifted apart the
// moment the column was narrowed to pay for the TOKENS/COST split.
//
// 18 rather than 14. The values reaching it are wider than a short model alias:
// eventMethodValue also returns A2A and MCP method names
// ("notifications/initialized", 25) and dated provider model IDs
// ("claude-sonnet-4-5-20250929", 26). At 14 both "claude-sonnet-4-5-20250929"
// and "claude-sonnet-4-20250514" render as "claude-sonnet…", which makes the
// column unable to say which model the COST cell beside it is reporting — the
// one thing it most needs to disambiguate now that costs are per-row.
const methodColWidth = 18

// tunnelAction is the ACTION cell for an opaque CONNECT that no plugin acted on.
//
// It must fit actionColWidth. Truncation matters more here than on other rows:
// METHOD and STATUS are both empty for an unbridged CONNECT, so a clipped
// "tunne" would take the row back to unreadable — the state this label exists to
// fix.
const tunnelAction = "tunnel"

// rowAction is the ACTION + PLUGIN pair for one display row.
//
// It wraps eventAction to name an unbridged CONNECT. Such a row carries TLS bytes,
// so no plugin ran, no protocol was parsed and there is no status — left as "— —"
// it reads as a request that failed or that the pipeline ignored, which is how a
// routine egress tunnel came to look like a bug.
//
// The label applies only when nothing acted: a gate CAN deny a CONNECT on the
// tunnel-open itself, and that deny must keep the headline. A BRIDGED tunnel never
// reaches this branch — buildEventRows folds it into the decrypted inner request,
// whose own action is the interesting one.
//
// invs is passed in rather than derived from er, so the headline is computed from
// the same set the caller's visibility decision used. Deriving it again would make
// that relationship implicit and silently divergent: were the call site ever to
// filter invs — to honour a plugin-name filter in the hide logic, say — the ACTION
// cell would keep using the unfiltered set and disagree with the row's own reason
// for being visible.
func rowAction(er eventRow, invs []pipeline.Invocation) (action, plugin string) {
	action, plugin = eventAction(invs)
	if er.event != nil && er.event.Tunnel && action == "—" {
		return tunnelAction, "—"
	}
	return action, plugin
}

// eventAction folds a message's per-plugin invocations into the single ACTION +
// PLUGIN cell pair shown in the timeline. The headline reflects what actually
// took effect:
//
//   - The winner is the highest-ranked ENFORCED (non-shadow) invocation —
//     deny > modify > observe > allow > skip (see actionRank). A shadow
//     deny/modify did NOT take effect (outcome.go enforces deny only when
//     !Shadow), so it must not headline over the action that really applied.
//     PLUGIN names that plugin, or "N plugins" when several tie at the top.
//   - A trailing "*" flags that a shadow policy would have blocked/changed the
//     message (e.g. "allow*"). Lets a rollout stay scannable without claiming
//     a block that never happened.
//   - When nothing enforced acted above a skip, a shadow deny/modify — if
//     present — becomes the headline ("deny*"), since it's the only signal
//     worth surfacing (a lone shadow deny on an otherwise-passthrough request).
//   - Otherwise "—  —": nothing meaningful happened. A skip never credits a
//     plugin (naming a skipper like "token-exchange" on an unrelated host
//     reads as if it processed the message). Per-plugin detail is on drill-in.
func eventAction(invs []pipeline.Invocation) (action, plugin string) {
	winner, count := topInvocation(invs, func(iv pipeline.Invocation) bool { return !iv.Shadow })
	shadow := shadowFlagged(invs)

	if actionRank(winner.Action) > actionRank(pipeline.ActionSkip) {
		action = string(winner.Action)
		if shadow {
			action += "*"
		}
		if count == 1 {
			plugin = winner.Plugin
		} else {
			plugin = fmt.Sprintf("%d plugins", count)
		}
		return action, plugin
	}

	if shadow {
		sh, _ := topInvocation(invs, func(iv pipeline.Invocation) bool { return iv.Shadow })
		return string(sh.Action) + "*", sh.Plugin
	}

	return "—", "—"
}

// eventInactive reports whether no plugin took a meaningful action — either no
// invocations at all (a pure passthrough / unprocessed message) or every
// invocation was a skip (plugins ran but none matched). A shadow deny/modify
// counts as activity (it flags a would-have-blocked message worth keeping
// visible during a rollout). The `s` key hides inactive messages so an
// operator can focus on plugin activity.
func eventInactive(invs []pipeline.Invocation) bool {
	if len(invs) == 0 {
		return true
	}
	for _, iv := range invs {
		if iv.Action != pipeline.ActionSkip {
			return false
		}
	}
	return true
}

func shortDirection(d pipeline.Direction) string {
	if d == pipeline.Inbound {
		return "in"
	}
	return "out"
}

func shortPhase(p pipeline.SessionPhase) string {
	switch p {
	case pipeline.SessionRequest:
		return "req"
	case pipeline.SessionResponse:
		return "resp"
	case pipeline.SessionDenied:
		// A denied event is a request that didn't reach the response
		// phase. The terminal-deny semantics are already conveyed by
		// the ACTION column ("deny") and STATUS column (4xx/5xx);
		// rendering "deny" in PHASE too is duplicative. Show "req" so
		// PHASE always communicates lifecycle position.
		return "req"
	}
	return "?"
}

// eventMethodValue is the raw, untruncated method/model for an event — the A2A
// method, inference model, or MCP method. Used for logic (pairing, filtering)
// where truncation would conflate distinct names sharing a 22-char prefix or
// hide searchable suffixes.
func eventMethodValue(e pipeline.SessionEvent) string {
	switch {
	case e.A2A != nil:
		return e.A2A.Method
	case e.Inference != nil:
		return e.Inference.Model
	case e.MCP != nil:
		return e.MCP.Method
	}
	return ""
}

// eventMethod is the display form of the method/model — truncated to the
// METHOD column width. Render-only; never compare or search on it.
//
// Shares methodColWidth with the column definition rather than repeating the
// number, the same drift guard actionColWidth provides for ACTION: the two were
// already 22 and 14 after the column shrank, which bubbles hid by re-truncating
// each cell at the column width anyway.
func eventMethod(e pipeline.SessionEvent) string {
	return truncStr(eventMethodValue(e), methodColWidth)
}

func statusCell(e pipeline.SessionEvent) string {
	if e.StatusCode == 0 {
		return ""
	}
	return fmt.Sprintf("%d", e.StatusCode)
}

// generatedTokensCell shows what a response generated. Deliberately the output
// count and not TotalTokens: for a long-running agent the prompt dominates the
// aggregate so completely (cache reads in the hundreds of thousands) that
// TotalTokens barely moves between turns, hiding the one component that actually
// varies. The prompt side is on the request row, so between them the two rows
// account for the whole exchange without either repeating the other.
//
// They usually also ADD UP to TotalTokens, but that is not guaranteed and is not
// relied on: TokenUsage.Fill prefers the provider's own reported total when it
// sends one, which need not equal the parts. Each row reports its own measured
// half, so the display stays honest either way.
//
// The CompletionTokens fallback cannot currently fire — Fill sets it from the
// same Output value read above — and is kept only for symmetry with promptTokens.
func generatedTokensCell(e *pipeline.SessionEvent) string {
	if e.Inference == nil {
		return ""
	}
	n := e.Inference.OutputTokens
	if n == 0 {
		n = e.Inference.CompletionTokens
	}
	if n <= 0 {
		return ""
	}
	return formatCount(n)
}

func durationCell(e pipeline.SessionEvent) string {
	if e.Duration == 0 {
		return ""
	}
	ms := e.Duration.Milliseconds()
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000)
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 2 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// computeEventPairs matches each response row to its request row and returns
// two views of the result:
//
//   - ids: a small integer per event, shared across a (request, response)
//     exchange and freshly minted for unpaired rows. Drives the # column.
//   - partner: a bidirectional row-index map (partner[i]=j and partner[j]=i)
//     for matched pairs. Drives the PHASE-column span glyphs.
//
// Each response row is matched to the closest preceding unpaired request row
// sharing direction + host (port-normalized) + method. The method component
// keeps a fire-and-forget request (e.g. MCP notifications/initialized, which
// never gets a response) from stealing a later response that belongs to a
// different method.
//
// Pairing prefers SessionEvent.RequestID, which the proxy stamps on both the
// request and response event of the same exchange. That is exact, including
// under concurrency.
//
// The closest-preceding heuristic below remains for events with no RequestID —
// an older proxy, or a listener that has not been taught to stamp it. It matches
// on direction + host (port-normalized) + method, and it cross-pairs when a
// client has concurrent same-host+method calls in flight. That is not
// hypothetical: Claude Code fires its session-title request alongside the main
// one, and the heuristic drew a 400 from the title request under the main
// request's row, which read as the pipeline plugin on that row having caused it.
//
// IDs are keyed by event pointer so the render loop can look one up without
// knowing the row index. They start at 1 and increment in first-seen row order
// so adjacent exchanges get adjacent integers.
func computeEventPairs(rows []eventRow) (map[*pipeline.SessionEvent]int, map[int]int) {
	partner := make(map[int]int) // row index → matched row index

	// Exact pass: pair by the proxy-stamped RequestID. Indexed by id so a
	// response finds its request regardless of how much traffic interleaves
	// between them.
	reqByID := make(map[string]int)
	for i := range rows {
		e := rows[i].event
		if e.RequestID == "" || e.Phase != pipeline.SessionRequest {
			continue
		}
		if _, dup := reqByID[e.RequestID]; !dup {
			reqByID[e.RequestID] = i
		}
	}
	for j := range rows {
		e := rows[j].event
		if e.RequestID == "" || e.Phase != pipeline.SessionResponse {
			continue
		}
		i, ok := reqByID[e.RequestID]
		if !ok {
			continue
		}
		if _, taken := partner[i]; taken {
			continue
		}
		partner[i] = j
		partner[j] = i
	}

	// Heuristic pass: only for rows the exact pass could not place.
	for j := range rows {
		rj := rows[j].event
		if rj.Phase != pipeline.SessionResponse {
			continue
		}
		if _, done := partner[j]; done {
			continue // already paired exactly by RequestID
		}
		if rj.RequestID != "" {
			// It carried an id and still did not pair — a second response for
			// the same request (a retry, or a streamed reply recorded twice).
			// Letting it fall through would have the heuristic walk back and
			// claim an unrelated earlier request, which is exactly the
			// mis-attribution the id was added to end. Leave it unpaired.
			continue
		}
		for i := j - 1; i >= 0; i-- {
			if _, taken := partner[i]; taken {
				continue
			}
			ri := rows[i].event
			if ri.Phase != pipeline.SessionRequest {
				continue
			}
			if ri.Direction != rj.Direction ||
				hostOnly(ri.Host) != hostOnly(rj.Host) ||
				eventMethodValue(*ri) != eventMethodValue(*rj) {
				continue
			}
			partner[i] = j
			partner[j] = i
			break
		}
	}

	ids := make(map[*pipeline.SessionEvent]int, len(rows))
	next := 0
	for i := range rows {
		e := rows[i].event
		if _, done := ids[e]; done {
			continue
		}
		if p, ok := partner[i]; ok {
			if pid, ok := ids[rows[p].event]; ok {
				ids[e] = pid
				continue
			}
		}
		next++
		ids[e] = next
	}
	return ids, partner
}

// matchEventRow does a case-insensitive substring match across every string
// field the operator might reasonably search for — the event's host/method,
// the fields of every plugin invocation on it, and its protocol extensions.
// A folded tunnel's fields are searched too, so filtering by a bridged
// origin's host still surfaces the collapsed row. Two prefix shortcuts:
//
//   - `deny` alone matches a SessionDenied event and any invocation whose
//     Action == ActionDeny — the one-word "show me failures" filter.
//   - `plugin:<name>` matches rows whose escape-hatch Plugins map has <name>
//     as a key.
func matchEventRow(r eventRow, q string) bool {
	q = strings.ToLower(q)

	if q == "deny" {
		return eventMatchesDeny(r.event) || (r.tunnel != nil && eventMatchesDeny(r.tunnel))
	}

	if after, ok := strings.CutPrefix(q, "plugin:"); ok {
		if _, present := r.event.Plugins[after]; present {
			return true
		}
		if r.tunnel != nil {
			_, present := r.tunnel.Plugins[after]
			return present
		}
		return false
	}

	hay := eventHaystack(r.event)
	if r.tunnel != nil {
		hay = append(hay, eventHaystack(r.tunnel)...)
	}
	for _, s := range hay {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

// eventMatchesDeny reports whether e is a deny — either the terminal
// SessionDenied phase or any invocation with ActionDeny.
func eventMatchesDeny(e *pipeline.SessionEvent) bool {
	if e.Phase == pipeline.SessionDenied {
		return true
	}
	for _, iv := range allInvocations(e) {
		if iv.Action == pipeline.ActionDeny {
			return true
		}
	}
	return false
}

// eventHaystack collects every searchable string on an event: host, method,
// each invocation's plugin/action/reason/path and detail key=values, the
// caller identity, and protocol-specific content (A2A parts, MCP error, the
// inference completion / finish reason).
func eventHaystack(e *pipeline.SessionEvent) []string {
	hay := []string{e.Host, eventMethodValue(*e)}
	for _, iv := range allInvocations(e) {
		hay = append(hay, iv.Plugin, string(iv.Action), iv.Reason, iv.Path)
		// Plugin-specific diagnostic context — iterate keys + values so
		// filter text matches on e.g. "target_audience" / the target
		// audience value without the UI having to know which keys each
		// plugin writes.
		for k, v := range iv.Details {
			hay = append(hay, k, v)
		}
	}
	if e.Identity != nil {
		hay = append(hay, e.Identity.Subject, e.Identity.ClientID)
	}
	if e.A2A != nil {
		hay = append(hay, e.A2A.SessionID, e.A2A.MessageID, e.A2A.Role)
		for _, p := range e.A2A.Parts {
			hay = append(hay, p.Content)
		}
	}
	if e.MCP != nil && e.MCP.Err != nil {
		hay = append(hay, e.MCP.Err.Message)
	}
	if e.Inference != nil {
		hay = append(hay, e.Inference.Completion, e.Inference.FinishReason)
	}
	return hay
}

// identityBannerStyle renders the small bordered box above the events
// table. Rounded border matches the outer frame; muted color keeps the
// banner as context rather than competing with the event rows.
var identityBannerStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.AdaptiveColor{Light: "#94A3B8", Dark: "#475569"}).
	Padding(0, 1)

// identityBannerHeight is the rendered height of the banner — four lines
// of content plus two border lines. layout() subtracts this from the
// events-table height so the banner doesn't push rows off-screen.
const identityBannerHeight = 6

// identityBanner renders a compact "IDENTITY" box summarizing the caller
// of this session's inbound events. If callers diverge across the
// session, it reports the count so the operator knows to check detail
// rows. Returns an empty string when no inbound identity is present
// (e.g. outbound-only buckets).
func identityBanner(events []pipeline.SessionEvent) string {
	idents := distinctInboundIdentities(events)
	if len(idents) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(styleTitle.Render("IDENTITY"))
	b.WriteByte('\n')

	if len(idents) == 1 {
		id := idents[0]
		b.WriteString(fmt.Sprintf("subject  %s\n", nonEmpty(id.Subject, "—")))
		b.WriteString(fmt.Sprintf("client   %s\n", nonEmpty(id.ClientID, "—")))
		b.WriteString(fmt.Sprintf("scopes   %s", nonEmpty(truncateScopes(id.Scopes, 3), "—")))
	} else {
		// Multiple distinct callers — surface the count; detail rows
		// carry the full identity for drill-down.
		subjects := make([]string, 0, len(idents))
		for _, id := range idents {
			subjects = append(subjects, nonEmpty(id.Subject, "—"))
		}
		b.WriteString(fmt.Sprintf("subjects  %d distinct: %s\n", len(idents), strings.Join(subjects, ", ")))
		b.WriteString("client    (see individual events)\n")
		b.WriteString("scopes    (see individual events)")
	}
	return identityBannerStyle.Render(b.String())
}

// identityKey is the comparable shape used to dedupe identities in the
// banner. Using a struct avoids string concatenation (and the theoretical
// "|" collision) — subject+clientID are the two fields that define a
// unique caller; scopes can legitimately vary turn-to-turn.
type identityKey struct {
	subject  string
	clientID string
}

// distinctInboundIdentities returns the unique EventIdentity values seen on
// inbound events, in first-seen order.
func distinctInboundIdentities(events []pipeline.SessionEvent) []*pipeline.EventIdentity {
	var out []*pipeline.EventIdentity
	seen := map[identityKey]bool{}
	for i := range events {
		e := &events[i]
		if e.Direction != pipeline.Inbound || e.Identity == nil {
			continue
		}
		k := identityKey{subject: e.Identity.Subject, clientID: e.Identity.ClientID}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e.Identity)
	}
	return out
}

// truncateScopes joins the first n scopes with commas and appends a
// "+N more" suffix if the list was longer. Keeps the identity banner
// from overflowing the terminal when a caller has many scopes.
func truncateScopes(scopes []string, n int) string {
	if len(scopes) == 0 {
		return ""
	}
	if len(scopes) <= n {
		return strings.Join(scopes, ", ")
	}
	return strings.Join(scopes[:n], ", ") + fmt.Sprintf(" +%d more", len(scopes)-n)
}

// pairedResponse resolves the response row belonging to a request row, or nil
// when there isn't one yet.
//
// Extracted because the TOKENS and COST cells both need it and the id guard must
// not drift between them: only a response that pairs by RequestID may be used. A
// heuristically-matched response can belong to a different request, and its cache
// tier would then pick the wrong rate — a 12.5x error presented as a measurement.
func pairedResponse(rows []eventRow, partner map[int]int, i int, ev *pipeline.SessionEvent) *pipeline.SessionEvent {
	j, ok := partner[i]
	if !ok || j < 0 || j >= len(rows) {
		return nil // no response yet: the ratio and tier are not known
	}
	resp := rows[j].event
	if resp == nil || resp.Phase != pipeline.SessionResponse {
		return nil
	}
	if ev.RequestID == "" || resp.RequestID != ev.RequestID {
		return nil
	}
	return resp
}

// tokensCell renders the TOKENS column, split so the two rows of an exchange sum
// to the billed total rather than one repeating the other:
//
//   - a REQUEST row shows the prompt tokens it sent, with what tool-prune kept
//     off them in parentheses — which is where the plugin's `modify` invocation
//     already sits;
//   - a RESPONSE row shows the tokens generated.
//
// The prompt count lives on the response event because the provider is the only
// party that tokenizes, but it is a request-side quantity — which is what lets a
// request row show a total at all. So a request row looks forward to its pair.
func (m *model) tokensCell(rows []eventRow, partner map[int]int, i int, ev *pipeline.SessionEvent) string {
	switch ev.Phase {
	case pipeline.SessionResponse:
		return generatedTokensCell(ev)
	case pipeline.SessionRequest:
		resp := pairedResponse(rows, partner, i, ev)
		if resp == nil {
			return ""
		}
		var saved float64
		var projected bool
		if ps, ok := decodePruneSaving(ev); ok {
			saved, _, _ = savedTokensAndCost(ps, resp.Inference)
			projected = ps.Projected
		}
		return formatTokensWithSaving(promptTokens(resp.Inference), saved, projected)
	default:
		return ""
	}
}

// costCell renders the COST column.
//
// The two rows are NOT two halves of one sum, unlike TOKENS:
//
//   - a REQUEST row shows what its prompt cost, modelled per-tier from the rates
//     tool-prune published, with the saving in parentheses. Blank without
//     tool-prune, which is the only plugin that puts rates on the wire.
//   - a RESPONSE row shows what the whole exchange cost, as reported by
//     litellm-budget-track: the gateway's own post-discount figure when it
//     stamped one, otherwise the plugin's own per-token pricing. A streamed
//     response always reports 0 in the header, so for a streaming agent the
//     modelled path is the common case rather than the exception.
//
// Both figures in this column can therefore be models, and neither is marked as
// one. That is deliberate: marking the response cost while leaving the request
// cost — which is always modelled — unmarked would imply a distinction the column
// does not actually draw. costEvent.Source carries the provenance for anyone who
// needs it.
//
// The response figure *includes* the request figure. It is not the generated-token
// cost, because no plugin publishes an output rate: tool-prune deliberately omits
// one (it only ever shrinks the prompt, so attributing output cost to it would be
// false) and budget-track emits a finished total rather than its rates. Deriving
// the completion cost by subtraction would concentrate all of the prompt model's
// error into it and can go negative, so the reported total is shown instead of a
// computed delta.
func (m *model) costCell(rows []eventRow, partner map[int]int, i int, ev *pipeline.SessionEvent) string {
	switch ev.Phase {
	case pipeline.SessionResponse:
		ce, ok := decodeCostEvent(ev)
		if !ok {
			return ""
		}
		return formatUSDCell(ce.CostUSD)
	case pipeline.SessionRequest:
		resp := pairedResponse(rows, partner, i, ev)
		if resp == nil {
			return ""
		}
		ps, ok := decodePruneSaving(ev)
		if !ok {
			return ""
		}
		total, ok := promptCost(ps, resp.Inference)
		if !ok {
			return ""
		}
		_, savedUSD, _ := savedTokensAndCost(ps, resp.Inference)
		return formatUSDWithSaving(total, savedUSD, ps.Projected)
	default:
		return ""
	}
}
