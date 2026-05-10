package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
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
			{Title: "PHASE", Width: 6},
			{Title: "ACTION", Width: 8},
			{Title: "PLUGIN", Width: 18},
			{Title: "METHOD", Width: 22},
			{Title: "STATUS", Width: 7},
			{Title: "DURATION", Width: 10},
			{Title: "TOKENS", Width: 8},
			{Title: "HOST", Width: 20},
		}),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
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

	// Flatten (event, invocation) into row specs up-front so pair-linking
	// and filtering can run against the flat row list. Events without
	// invocations fall back to a single pseudo-row (unusual — the listener
	// only records events that have at least one Invocation or A2A/MCP/
	// Inference extension, but parser-only events can still land here if
	// the parser populated its extension without emitting an Invocation).
	rowSpecs := flattenInvocations(events)

	// Pair request/response rows by (direction, plugin) so each plugin's
	// contribution on the request side connects to its contribution on the
	// response side, independent of other plugins in the same pipeline.
	pairs := pairInvocationRows(rowSpecs)

	// Event-level pair IDs for the # column. Assigns each event a small
	// integer; events that match as a (request, response) pair share one
	// integer so the operator can scan the column for the repeated
	// number. Derived from the same row-level pair map that drives the
	// └resp glyph, so the two visual cues stay consistent.
	eventIDs := computeEventPairIDs(rowSpecs, pairs)

	rows := make([]table.Row, 0, len(rowSpecs))
	m.visibleRows = m.visibleRows[:0]
	var lastEvent *pipeline.SessionEvent // most-recent event already rendered (post-filter)
	for i, rs := range rowSpecs {
		if m.filter != "" && !matchInvocationRow(rs, m.filter) {
			continue
		}
		// A "continuation" row is one whose event is the same as the
		// previous RENDERED row's event (filtering-aware). We blank the
		// event-level columns (#, TIME, DIR, PHASE, STATUS, DURATION,
		// TOKENS, HOST) on continuation rows so an event's multi-plugin
		// group reads as one visual block — only PLUGIN and ACTION vary.
		// METHOD stays populated since a multi-plugin row set can still
		// show per-plugin method context (e.g. a2a-parser observes
		// message/stream while jwt-validation has no method at all).
		continuation := lastEvent == rs.event

		var idCell, timeCell, dirCell, phaseCell, statusC, durCell, tokC, hostC string
		if !continuation {
			if id, ok := eventIDs[rs.event]; ok {
				idCell = strconv.Itoa(id)
			}
			timeCell = rs.event.At.Format("15:04:05.00")
			dirCell = shortDirection(rs.event.Direction)
			phaseCell = shortPhase(rs.event.Phase)
			if rs.event.Phase == pipeline.SessionResponse {
				if _, paired := pairs[i]; paired {
					// └ prefix visually connects the response row to its
					// request row in the same (direction, plugin) pair.
					phaseCell = "└" + phaseCell
				}
			}
			statusC = statusCell(*rs.event)
			durCell = durationCell(*rs.event)
			tokC = tokensCell(*rs.event)
			hostC = truncStr(rs.event.Host, 20)
		}

		rows = append(rows, table.Row{
			idCell,
			timeCell,
			dirCell,
			phaseCell,
			rs.actionCell(),
			truncStr(rs.pluginCell(), 18),
			eventMethod(*rs.event),
			statusC,
			durCell,
			tokC,
			hostC,
		})
		m.visibleRows = append(m.visibleRows, rs)
		lastEvent = rs.event
	}
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
// points into m.visibleRows (the flattened row list), and each row carries
// a reference to its source event.
func (m *model) selectedEvent() *pipeline.SessionEvent {
	if len(m.visibleRows) == 0 {
		return nil
	}
	cur := m.eventsTbl.Cursor()
	if cur < 0 || cur >= len(m.visibleRows) {
		return nil
	}
	return m.visibleRows[cur].event
}

// invocationRow is one table row — the cartesian product of SessionEvent
// × Invocation. An event with N plugin invocations produces N rows; an
// event with no invocations produces one row with an empty invocation.
// Rendering and filtering both work off this flat list.
type invocationRow struct {
	event *pipeline.SessionEvent
	// inv may be nil when the event has no Invocation records. The
	// pseudo-row still renders so the event is reachable in the table.
	inv *pipeline.Invocation
	// direction is the Invocations.{Inbound,Outbound} this row came
	// from, disambiguating when a single event somehow carries both
	// (doesn't happen today but cheap to be explicit).
	direction pipeline.Direction
}

func (r invocationRow) actionCell() string {
	if r.inv == nil {
		return "—"
	}
	return string(r.inv.Action)
}

func (r invocationRow) pluginCell() string {
	if r.inv == nil {
		return "—"
	}
	return r.inv.Plugin
}

// flattenInvocations walks the event slice in order and, for each event,
// emits one invocationRow per Invocation it carries (Inbound then
// Outbound). Events with no Invocations fall back to a single pseudo-row
// so parser-only events (a SessionEvent carrying just MCP or A2A with no
// matching Invocation) remain reachable.
func flattenInvocations(events []pipeline.SessionEvent) []invocationRow {
	out := make([]invocationRow, 0, len(events))
	for i := range events {
		e := &events[i]
		if e.Invocations == nil || (len(e.Invocations.Inbound) == 0 && len(e.Invocations.Outbound) == 0) {
			out = append(out, invocationRow{event: e, direction: e.Direction})
			continue
		}
		for j := range e.Invocations.Inbound {
			out = append(out, invocationRow{
				event:     e,
				inv:       &e.Invocations.Inbound[j],
				direction: pipeline.Inbound,
			})
		}
		for j := range e.Invocations.Outbound {
			out = append(out, invocationRow{
				event:     e,
				inv:       &e.Invocations.Outbound[j],
				direction: pipeline.Outbound,
			})
		}
	}
	return out
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
		return "deny"
	}
	return "?"
}

// (authCell and responsiblePlugin are gone — their roles moved onto
// invocationRow's actionCell/pluginCell because each row now corresponds
// to exactly one plugin's invocation rather than a whole event.)

func eventMethod(e pipeline.SessionEvent) string {
	switch {
	case e.A2A != nil:
		return truncStr(e.A2A.Method, 22)
	case e.Inference != nil:
		return truncStr(e.Inference.Model, 22)
	case e.MCP != nil:
		return truncStr(e.MCP.Method, 22)
	}
	return ""
}

func statusCell(e pipeline.SessionEvent) string {
	if e.StatusCode == 0 {
		return ""
	}
	return fmt.Sprintf("%d", e.StatusCode)
}

// tokensCell shows the total token count for inference response rows so
// operators can spot expensive calls while scrolling. Blank for every
// other event type (a2a, mcp, inference *request*). Uses the same
// thousands-separator formatter as the sessions-pane totals.
func tokensCell(e pipeline.SessionEvent) string {
	if e.Phase != pipeline.SessionResponse || e.Inference == nil || e.Inference.TotalTokens == 0 {
		return ""
	}
	return formatCount(e.Inference.TotalTokens)
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

// computeEventPairIDs assigns a small integer to every SessionEvent,
// sharing one integer across a (request, response) pair and minting a new
// one for unpaired events. The pairing decision is delegated to the row-
// level pair map from pairInvocationRows — if any plugin row on event A
// pairs with a plugin row on event B, then A and B pair at the event
// level too. This keeps the # column's IDs consistent with the `└resp`
// glyph the operator already sees (both derive from the same plugin-
// level (direction, plugin) match).
//
// Deriving from pairInvocationRows rather than recomputing by
// direction+host+method avoids a class of bugs with "featureless"
// requests (no parser matched, so method is empty): multiple concurrent
// passthrough calls to the same host all share the same host+method
// key and a naive matcher claims the wrong response.
//
// IDs are keyed by event pointer so render loops can look up a row's ID
// without knowing the slice index. IDs start at 1 and increment in
// first-seen row order so adjacent pairs get adjacent integers.
func computeEventPairIDs(rowSpecs []invocationRow, pairs map[int]int) map[*pipeline.SessionEvent]int {
	// Derive event-level pairs from row-level pairs. pairs is symmetric
	// (pairs[i]=j and pairs[j]=i), so iterating either entry sets the
	// map symmetrically. Last-write-wins when a single event pairs
	// through multiple plugins, but in practice all plugin rows on one
	// request event point at the same response event.
	eventPair := make(map[*pipeline.SessionEvent]*pipeline.SessionEvent)
	for i, j := range pairs {
		ei, ej := rowSpecs[i].event, rowSpecs[j].event
		if ei != ej {
			eventPair[ei] = ej
		}
	}

	// Event-level fallback for pairs the row-level matcher can't see.
	// When a response event has no plugin invocations (e.g. a bypass
	// path like /.well-known/agent.json — jwt-validation skipped on the
	// request and no parser matched on the response), its pseudo-row
	// has no (direction, plugin) key and pairInvocationRows leaves it
	// unpaired. Scan the ordered event list and link each unpaired
	// response to the closest preceding unpaired request with matching
	// direction + host so the # column still reflects the pairing.
	//
	// Closest-preceding match is sufficient for bypass traffic where
	// the response event immediately follows its request in the slice.
	// Multiple concurrent bypass requests on the same host could
	// theoretically cross-pair, but that's a near-simultaneous
	// duplicate-path pattern we don't expect in real traffic.
	orderedEvents := orderedUniqueEvents(rowSpecs)
	for i, e := range orderedEvents {
		if e.Phase != pipeline.SessionResponse {
			continue
		}
		if _, paired := eventPair[e]; paired {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			prev := orderedEvents[j]
			if prev.Phase != pipeline.SessionRequest {
				continue
			}
			if _, already := eventPair[prev]; already {
				continue
			}
			if prev.Direction != e.Direction || prev.Host != e.Host {
				continue
			}
			eventPair[e] = prev
			eventPair[prev] = e
			break
		}
	}

	ids := make(map[*pipeline.SessionEvent]int)
	seen := make(map[*pipeline.SessionEvent]bool)
	next := 0
	for _, rs := range rowSpecs {
		e := rs.event
		if seen[e] {
			continue
		}
		seen[e] = true
		// If this event's paired partner has already been assigned an
		// ID (partner appeared earlier in row order), reuse it.
		if partner := eventPair[e]; partner != nil {
			if pid, ok := ids[partner]; ok {
				ids[e] = pid
				continue
			}
		}
		next++
		ids[e] = next
	}
	return ids
}

// orderedUniqueEvents returns distinct event pointers in the order they
// first appear in rowSpecs. Used by computeEventPairIDs' event-level
// fallback to walk events sequentially while looking backward for
// unpaired request counterparts.
func orderedUniqueEvents(rowSpecs []invocationRow) []*pipeline.SessionEvent {
	seen := make(map[*pipeline.SessionEvent]bool, len(rowSpecs))
	out := make([]*pipeline.SessionEvent, 0, len(rowSpecs))
	for _, rs := range rowSpecs {
		if seen[rs.event] {
			continue
		}
		seen[rs.event] = true
		out = append(out, rs.event)
	}
	return out
}

// matchInvocationRow does a case-insensitive substring match across every
// string field the operator might reasonably search for — the invocation's
// own fields plus the containing event's protocol extensions. Two prefix
// shortcuts:
//
//   - `deny` alone matches SessionDenied events and any invocation
//     whose Action == ActionDeny — the one-word "show me failures"
//     filter.
//   - `plugin:<name>` matches rows whose escape-hatch Plugins map on
//     the parent event has <name> as a key.
func matchInvocationRow(r invocationRow, q string) bool {
	q = strings.ToLower(q)

	if q == "deny" {
		if r.event.Phase == pipeline.SessionDenied {
			return true
		}
		if r.inv != nil && r.inv.Action == pipeline.ActionDeny {
			return true
		}
		return false
	}

	if after, ok := strings.CutPrefix(q, "plugin:"); ok {
		_, present := r.event.Plugins[after]
		return present
	}

	e := r.event
	hay := []string{e.Host, eventMethod(*e)}
	if r.inv != nil {
		hay = append(hay,
			r.inv.Plugin, string(r.inv.Action), r.inv.Reason, r.inv.Path)
		// Plugin-specific diagnostic context — iterate keys + values so
		// filter text matches on e.g. "target_audience" / the target
		// audience value without the UI having to know which keys
		// each plugin writes.
		for k, v := range r.inv.Details {
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
	for _, s := range hay {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

// pairInvocationRows pairs request-phase rows with their response-phase
// counterparts by (direction, plugin). Each plugin's contribution on the
// request side connects to its own contribution on the response side,
// independent of other plugins in the same pipeline — so a jwt-validation
// request row pairs with a jwt-validation response row even when several
// other plugins fired on the same event.
//
// Sequential pairing is good enough for current traffic: each request
// row is paired with the NEXT response row that shares (direction, plugin)
// and hasn't been claimed.
func pairInvocationRows(rows []invocationRow) map[int]int {
	pairs := make(map[int]int)
	// Pair key includes plugin + direction + method (from whichever
	// parser extension is populated). Without the method component,
	// a fire-and-forget request like MCP's notifications/initialized
	// would greedily claim the NEXT mcp-parser response — typically
	// the response to tools/list — and orphan the actual tools/list
	// request from its own response. Method discrimination makes the
	// match specific: mcp-parser/out/tools/list only pairs with
	// mcp-parser/out/tools/list. Auth plugins have no method; empty
	// methods still pair with empty methods (same key), preserving
	// pair behaviour for token-exchange and jwt-validation rows.
	key := func(r invocationRow) (string, pipeline.Direction, bool) {
		if r.inv == nil {
			return "", r.direction, false
		}
		return r.inv.Plugin + "|" + eventMethod(*r.event), r.direction, true
	}
	for i := range rows {
		if rows[i].event.Phase != pipeline.SessionRequest {
			continue
		}
		if _, already := pairs[i]; already {
			continue
		}
		k, dir, ok := key(rows[i])
		if !ok {
			continue
		}
		for j := i + 1; j < len(rows); j++ {
			if rows[j].event.Phase != pipeline.SessionResponse {
				continue
			}
			if _, taken := pairs[j]; taken {
				continue
			}
			rk, rdir, rok := key(rows[j])
			if !rok || rk != k || rdir != dir {
				continue
			}
			pairs[i] = j
			pairs[j] = i
			break
		}
	}
	return pairs
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

