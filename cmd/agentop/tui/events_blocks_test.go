package tui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/observe/claude"
	"github.com/rossoctl/cortex/core/pipeline"
)

// dirColumn is the DIR column's definition, for the gutter test. eventColumns is
// the single definition; this picks the one entry out rather than indexing by
// position, which a reordered list would silently move.
func dirColumn() eventColumn {
	for _, c := range eventColumns {
		if c.id == colDir {
			return c
		}
	}
	panic("no DIR column")
}

// blockEvents is a session with two subagents running beside the main conversation,
// interleaved as live traffic is. Each subagent carries a priced response so the
// header's rollup has figures to sum. The price is the exchange total, cost_usd, which
// is what a merged row's COST shows (#1681).
func blockEvents() []pipeline.SessionEvent {
	sub := func(id string, phase pipeline.SessionPhase, tokens int) pipeline.SessionEvent {
		return pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: phase, Host: "ete", RequestID: id,
			Subagent:  id,
			Inference: &pipeline.InferenceExtension{OutputTokens: tokens},
			Plugins: map[string]json.RawMessage{
				"cost": json.RawMessage(`{"cost_usd":0.1}`),
			},
		}
	}
	return []pipeline.SessionEvent{
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "ete", RequestID: "m1"},
		sub("a1", pipeline.SessionRequest, 0),
		sub("a1", pipeline.SessionResponse, 900),
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "ete", RequestID: "m2"},
		sub("a2", pipeline.SessionRequest, 0),
		sub("a2", pipeline.SessionResponse, 40),
	}
}

func newBlockModel(t *testing.T, events []pipeline.SessionEvent) *model {
	t.Helper()
	m := &model{selectedSess: "s", events: map[string][]pipeline.SessionEvent{"s": events}}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	return m
}

// The default view: each subagent's rows are hidden behind a collapsed header
// anchored at its first row, and the main conversation's rows stay top-level and
// in time order between them.
func TestBlocks_CollapsedByDefaultWithHeaderAtTheAnchor(t *testing.T) {
	m := newBlockModel(t, blockEvents())

	if m.subagentBlocks == nil || len(m.subagentBlocks) != 2 {
		t.Fatalf("blocks = %v, want 2", m.subagentBlocks)
	}
	// Rendered: m1, header(a1), m2, header(a2) — every member row hidden.
	if got := len(m.eventsTbl.Rows()); got != 4 {
		t.Fatalf("rendered rows = %d, want 4 (2 main + 2 headers)", got)
	}
	// visibleRows stays parallel to the rendered rows; headers hold empty rows.
	if len(m.visibleRows) != 4 {
		t.Fatalf("visibleRows = %d, want 4", len(m.visibleRows))
	}
	if m.visibleRows[0].event.RequestID != "m1" || m.visibleRows[2].event.RequestID != "m2" {
		t.Errorf("main rows moved: %s, %s", m.visibleRows[0].event.RequestID, m.visibleRows[2].event.RequestID)
	}
	// headerRows names the two header positions.
	if b := m.headerRows[1]; b == nil || b.id != "a1" {
		t.Errorf("header at 1 = %v, want a1", m.headerRows[1])
	}
	if b := m.headerRows[3]; b == nil || b.id != "a2" {
		t.Errorf("header at 3 = %v, want a2", m.headerRows[3])
	}
	// The header carries the rollup: a1 holds one exchange (one row) whose
	// response carries 900 tokens at $0.10.
	h := m.eventsTbl.Rows()[1][0]
	if !strings.Contains(h, "▸─") || !strings.Contains(h, "1 msg") || !strings.Contains(h, "900 tok") || !strings.Contains(h, "$0.10") {
		t.Errorf("a1 header = %q", h)
	}
}

// Enter on a header opens the block: member rows appear together under the
// header (see TestBlocks_ExpandedMembersStayTogether), marked via the DIR
// gutter, and the header's glyph turns over. Enter again closes it.
func TestBlocks_EnterToggles(t *testing.T) {
	m := newBlockModel(t, blockEvents())

	m.eventsTbl.SetCursor(1)
	if !m.toggleSubagentBlock() {
		t.Fatal("Enter on the header row did not toggle")
	}
	if got := len(m.eventsTbl.Rows()); got != 5 {
		t.Fatalf("open block: rendered rows = %d, want 5", got)
	}
	// The member rows sit inside the block, between the header and m2. Row 0 is
	// m1, row 1 the header (its visibleRows entry is empty — no event of its own).
	if rid := m.visibleRows[0].event.RequestID; rid != "m1" {
		t.Fatalf("row 0 = %s, want m1", rid)
	}
	if m.visibleRows[1].event != nil {
		t.Errorf("header row holds an event: %+v", m.visibleRows[1].event)
	}
	// a1's one exchange is one row (#1681).
	if r := m.visibleRows[2]; r.event.Subagent != "a1" || r.resp == nil {
		t.Errorf("row 2 not a1's merged exchange: %+v", r)
	}
	if h := m.eventsTbl.Rows()[1][0]; !strings.Contains(h, "▾─") {
		t.Errorf("open header = %q, want ▾", h)
	}
	// a2 stays collapsed through a1's rebuild.
	if got := len(m.eventsTbl.Rows()); got != 5 {
		t.Fatalf("a2 should still be collapsed: %d rows", got)
	}

	m.eventsTbl.SetCursor(1)
	m.toggleSubagentBlock()
	if got := len(m.eventsTbl.Rows()); got != 4 {
		t.Fatalf("re-collapsed: rendered rows = %d, want 4", got)
	}
}

// An expanded block's rows render TOGETHER, directly under the header — pulled
// out of the chronological interleaving with the main conversation. Live traffic
// interleaves (a subagent's exchange spans several main-conversation turns), and
// rendering members at their own timestamps scattered the block across the pane,
// which read as "expanding did nothing" — the header said 56 rows and the rows
// below it were the main conversation's.
func TestBlocks_ExpandedMembersStayTogether(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "ete", RequestID: "a1", Subagent: "a1"},
		// The main conversation's row, chronologically INSIDE a1's exchange.
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "ete", RequestID: "m1"},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "ete", RequestID: "a1", Subagent: "a1"},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "ete", RequestID: "m2"},
	}
	m := newBlockModel(t, events)

	// Collapsed: header anchored at a1's first row, then the main rows.
	if got := len(m.eventsTbl.Rows()); got != 3 {
		t.Fatalf("collapsed rows = %d, want 3 (header + m1 + m2)", got)
	}

	m.eventsTbl.SetCursor(0)
	if !m.toggleSubagentBlock() {
		t.Fatal("Enter on the header row did not toggle")
	}

	// Expanded: header, a1's whole exchange as one row, THEN the main rows.
	want := []string{"a1", "m1", "m2"}
	if got := len(m.eventsTbl.Rows()); got != 4 {
		t.Fatalf("expanded rows = %d, want 4 (header + a1's exchange + m1 + m2)", got)
	}
	if m.visibleRows[1].resp == nil {
		t.Error("a1's row has no response: its exchange is split")
	}
	for i, id := range want {
		// Row 0 is the header, whose visibleRows entry holds no event.
		if rid := m.visibleRows[i+1].event.RequestID; rid != id {
			t.Errorf("expanded row %d = %s, want %s", i+1, rid, id)
		}
	}
}

// Two subagents running concurrently, each interleaved with the other and with
// the main conversation — and a1's traffic resuming long after a2 finished its
// first exchange. Each block still renders as ONE contiguous segment under its
// own single header: membership is by subagent id over the whole session, not
// by time-contiguity, so interleaving cannot split a block into segments or
// emit a second header for the same agent.
func TestBlocks_ConcurrentSubagentsEachStayOneSegment(t *testing.T) {
	req := func(id, sub string) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "ete", RequestID: id, Subagent: sub}
	}
	resp := func(id, sub string) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "ete", RequestID: id, Subagent: sub}
	}
	events := []pipeline.SessionEvent{
		req("a1.1", "a1"),
		req("m1", ""),
		req("a2.1", "a2"), // a2 starts while a1's first exchange is in flight
		resp("a1.1", "a1"),
		resp("a2.1", "a2"),
		req("m2", ""),
		resp("m2", ""),
		req("a2.2", "a2"), // a2 continues after main turns
		resp("a2.2", "a2"),
		req("m3", ""),
		resp("m3", ""),
		req("a1.2", "a1"), // a1's traffic resumes long after it started
		resp("a1.2", "a1"),
		req("m4", ""),
	}
	m := newBlockModel(t, events)
	m.subagentOpen = map[string]bool{"a1": true, "a2": true}
	m.rebuildEventsTable()

	// Each subagent's rows render in arrival order within its segment.
	order := map[string][]string{}
	for _, vr := range m.visibleRows {
		if vr.event != nil && vr.event.Subagent != "" {
			order[vr.event.Subagent] = append(order[vr.event.Subagent], vr.event.RequestID)
		}
	}
	if got := order["a1"]; !slices.Equal(got, []string{"a1.1", "a1.2"}) {
		t.Errorf("a1's segment = %v, want its whole exchange run in order", got)
	}
	if got := order["a2"]; !slices.Equal(got, []string{"a2.1", "a2.2"}) {
		t.Errorf("a2's segment = %v, want its whole exchange run in order", got)
	}
	// A subagent's rendered rows must form ONE contiguous run: their positions
	// span exactly their count. A block split into segments — or a second
	// header for the same agent — would leave gaps in that span.
	for _, sub := range []string{"a1", "a2"} {
		var pos []int
		for i, vr := range m.visibleRows {
			if vr.event != nil && vr.event.Subagent == sub {
				pos = append(pos, i)
			}
		}
		if len(pos) == 0 || pos[len(pos)-1]-pos[0]+1 != len(pos) {
			t.Errorf("%s's rows at %v, not one contiguous run", sub, pos)
		}
	}
	// Exactly two headers rendered.
	if headers := len(m.headerRows); headers != 2 {
		t.Errorf("headers = %d, want 2 (one per subagent, however interleaved)", headers)
	}
}

// Member rows number their exchanges from 1 WITHIN the block — the block reads
// as the subagent's own transcript — while main rows keep the session-wide
// numbers. Each exchange is one row (#1681), and the block's count agrees with the
// last number shown inside it.
func TestBlocks_MembersNumberFromOneInsideTheBlock(t *testing.T) {
	req := func(id, sub string) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "ete", RequestID: id, Subagent: sub}
	}
	resp := func(id, sub string) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "ete", RequestID: id, Subagent: sub}
	}
	events := []pipeline.SessionEvent{
		req("m1", ""), resp("m1", ""), // session exchange 1
		req("a1.1", "a1"), resp("a1.1", "a1"), // session 2 — block-local 1
		req("m2", ""), resp("m2", ""), // session 3
		req("a1.2", "a1"), resp("a1.2", "a1"), // session 4 — block-local 2
	}
	m := newBlockModel(t, events)
	m.eventsTbl.SetCursor(1) // a1's header: m1's exchange, then the header
	if !m.toggleSubagentBlock() {
		t.Fatal("Enter on the header row did not toggle")
	}

	// Rendered: m1, header, a1's two exchanges, m2. The first cell of each EVENT
	// row is the # column. The header row is one cell of its own, standing in for
	// the position.
	want := []string{"1", "HEADER", "1", "2", "3"}
	rows := m.eventsTbl.Rows()
	if len(rows) != len(want) {
		t.Fatalf("expanded rows = %d, want %d", len(rows), len(want))
	}
	for i, n := range want {
		if n == "HEADER" {
			continue // checked below, with the rollup
		}
		if got := rows[i][0]; got != n {
			t.Errorf("row %d # = %q, want %q", i, got, n)
		}
	}
	// The header's rollup counts exchanges, in the same unit the rows show.
	if h := rows[1][0]; !strings.Contains(h, "2 msgs") {
		t.Errorf("header = %q, want the exchange count (2 msgs)", h)
	}
}

// Two of one subagent's exchanges that overlap (A starts, B starts, A ends, B
// ends) keep their pairing: each exchange is one row with its own block-local
// number, in request order.
func TestBlocks_OverlappingExchangesKeepTheirNumbers(t *testing.T) {
	ev := func(id string, phase pipeline.SessionPhase) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: phase, Host: "ete", RequestID: id, Subagent: "a1"}
	}
	events := []pipeline.SessionEvent{
		ev("a1.1", pipeline.SessionRequest), ev("a1.2", pipeline.SessionRequest),
		ev("a1.1", pipeline.SessionResponse), ev("a1.2", pipeline.SessionResponse),
	}
	m := newBlockModel(t, events)
	m.subagentOpen = map[string]bool{"a1": true}
	m.rebuildEventsTable()

	rows := m.eventsTbl.Rows()
	if len(rows) != 3 {
		t.Fatalf("expanded rows = %d, want 3 (header + 2 exchanges)", len(rows))
	}
	var got []string
	for _, r := range rows[1:] {
		got = append(got, r[0])
	}
	if want := []string{"1", "2"}; !slices.Equal(got, want) {
		t.Errorf("# cells = %v, want %v", got, want)
	}
	if h := rows[0][0]; !strings.Contains(h, "2 msgs") {
		t.Errorf("header = %q, want 2 msgs", h)
	}
}

// Enter on a message row is still the detail drill-down; only headers toggle.
func TestBlocks_EnterOnAMessageOpensDetail(t *testing.T) {
	m := newBlockModel(t, blockEvents())
	m.eventsTbl.SetCursor(0)
	if m.toggleSubagentBlock() {
		t.Fatal("Enter on a message row toggled a block")
	}
}

// A live search opens every block, so a match inside a collapsed block is
// findable — and the search rows stay parallel to the rendered rows.
func TestBlocks_SearchExpandsCollapsedBlocks(t *testing.T) {
	m := newBlockModel(t, blockEvents())
	m.search = map[paneID]string{paneEvents: "ete"}
	m.rebuildEventsTable()

	if got := len(m.eventsTbl.Rows()); got != 6 {
		t.Fatalf("searching: rendered rows = %d, want 6 (all rows + 2 headers)", got)
	}
	if len(m.eventsTbl.Matches()) == 0 {
		t.Error("search matched nothing")
	}
}

// The header's search value is the header TEXT, not the label alone: the rollup is
// on screen, so a figure search ("$0.10", the costly subagent's cost) must match
// the header that displays it — the README's search contract is the text the pane
// shows. Before the search cell carried the same string as the row, the label was
// the only searchable part.
func TestBlocks_SearchMatchesTheHeaderRollup(t *testing.T) {
	m := newBlockModel(t, blockEvents())
	m.search = map[paneID]string{paneEvents: "$0.10"}
	m.rebuildEventsTable()

	// a1's header sits at rendered row 1 (m1, header, a1's exchange, m2, header).
	for _, hit := range m.eventsTbl.Matches() {
		if hit == 1 {
			return
		}
	}
	t.Errorf("searching the rollup figure: matches = %v, want the header at 1 among them", m.eventsTbl.Matches())
}

// A sort ranks the rows, so no blocks: every row renders flat, no header, no
// gutter — grouping would contradict the ranking the operator asked for.
func TestBlocks_SortDisablesGrouping(t *testing.T) {
	m := newBlockModel(t, blockEvents())
	m.sortCol = colDuration
	m.rebuildEventsTable()

	if m.subagentBlocks != nil {
		t.Fatal("blocks active under a sort")
	}
	if got := len(m.eventsTbl.Rows()); got != 4 {
		t.Fatalf("sorted: rendered rows = %d, want 4 flat", got)
	}
}

// The DIR cell carries the block's gutter while blocks are active, and the bare
// direction once they are not — under a sort, or for a main row.
func TestBlocks_DirGutterMarksMemberRows(t *testing.T) {
	events := blockEvents()
	m := &model{selectedSess: "s", events: map[string][]pipeline.SessionEvent{"s": events}}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()

	cc := cellContext{m: m, row: eventRow{event: &events[1]}}
	if got := dirColumn().cell(cc); got != "│out" {
		t.Errorf("member row DIR = %q, want │out", got)
	}
	cc = cellContext{m: m, row: eventRow{event: &events[0]}}
	if got := dirColumn().cell(cc); got != "out" {
		t.Errorf("main row DIR = %q, want out", got)
	}

	// Under a sort the same row renders bare: m.subagentBlocks is nil.
	m.sortCol = colDuration
	m.rebuildEventsTable()
	cc = cellContext{m: m, row: eventRow{event: &events[1]}}
	if got := dirColumn().cell(cc); got != "out" {
		t.Errorf("sorted member row DIR = %q, want out", got)
	}
}

// The label prefers the harvested description, then the agent type, then falls
// back to the raw id — which is what a teammate or a stale harvest renders.
func TestBlocks_LabelSources(t *testing.T) {
	m := newBlockModel(t, blockEvents())
	if got := m.subagentLabel("a1", false, false); got != "a1" {
		t.Errorf("no harvest: label = %q, want the id", got)
	}
	m.sessionsData = map[string]SessionMetadata{"s": {Subagents: map[string]claude.SubagentInfo{
		"a1": {AgentType: "general-purpose", Description: "PR review (execution-first)"},
		"a2": {AgentType: "Explore"},
	}}}
	if got := m.subagentLabel("a1", false, false); got != "PR review (execution-first)" {
		t.Errorf("label = %q, want the description", got)
	}
	if got := m.subagentLabel("a2", false, false); got != "Explore" {
		t.Errorf("label = %q, want the agent type", got)
	}
}

// A session whose rows name no subagent rebuilds exactly as before: no blocks,
// no headers, the same rendered row count.
func TestBlocks_NoSubagentsNoBlocks(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "a", RequestID: "m1"},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "a", RequestID: "m1"},
	}
	m := newBlockModel(t, events)
	if m.subagentBlocks != nil || m.headerRows != nil {
		t.Fatal("blocks built for a session with no subagent rows")
	}
	if got := len(m.eventsTbl.Rows()); got != 1 {
		t.Fatalf("rendered rows = %d, want 1: one exchange", got)
	}
}

// Opening another session drops the previous one's collapse state: subagent ids
// belong to the session that spawned them, and a same-named id in another
// session is a different agent.
func TestBlocks_CollapseStateIsPerSession(t *testing.T) {
	m := newBlockModel(t, blockEvents())
	m.eventsTbl.SetCursor(1)
	m.toggleSubagentBlock() // a1 open

	other := blockEvents()
	m.events["t"] = other
	m.selectedSess = "t"
	m.rebuildEventsTable()
	if m.subagentOpen != nil {
		t.Fatalf("collapse state survived a session change: %v", m.subagentOpen)
	}
	if got := len(m.eventsTbl.Rows()); got != 4 {
		t.Fatalf("new session: rendered rows = %d, want 4 (collapsed again)", got)
	}
}

// The block's population is the rows the pane would show: hideInactive hides a
// subagent's whole exchange, and a block with no surviving rows leaves no header
// behind — a header naming nothing is worse than no header, because it claims a
// subagent the filter just said produced nothing visible.
func TestBlocks_RollupFollowsTheInactiveFilter(t *testing.T) {
	active := func(id, sub string, phase pipeline.SessionPhase) pipeline.SessionEvent {
		return pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: phase, Host: "ete", RequestID: id, Subagent: sub,
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "inference-parser", Action: pipeline.ActionObserve},
			}},
		}
	}
	inactive := func(id, sub string, phase pipeline.SessionPhase) pipeline.SessionEvent {
		return pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: phase, Host: "ete", RequestID: id, Subagent: sub,
		}
	}
	events := []pipeline.SessionEvent{
		active("m1", "", pipeline.SessionRequest),
		active("a1", "a1", pipeline.SessionRequest),
		// a1's response is inactive but kept: its partner is shown (an exchange
		// hides whole or not at all).
		inactive("a1", "a1", pipeline.SessionResponse),
		active("m2", "", pipeline.SessionRequest),
		// a2's whole exchange is inactive: no block, no header.
		inactive("a2", "a2", pipeline.SessionRequest),
		inactive("a2", "a2", pipeline.SessionResponse),
	}
	m := &model{selectedSess: "s", events: map[string][]pipeline.SessionEvent{"s": events}, hideInactive: true}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()

	if _, ok := m.subagentBlocks["a2"]; ok {
		t.Fatal("a2 has a block despite every row being hidden")
	}
	// Rendered: m1, header(a1), m2 — collapsed by default, so the kept exchange
	// sits behind the header rather than on screen. No a2 header anywhere.
	if got := len(m.eventsTbl.Rows()); got != 3 {
		t.Fatalf("rendered rows = %d, want 3", got)
	}
	for i, b := range m.headerRows {
		if b.id == "a2" {
			t.Errorf("header for a2 at row %d", i)
		}
	}
	// a1's header counts what expanding it would show: the kept exchange, 1 msg.
	if h := m.eventsTbl.Rows()[1][0]; !strings.Contains(h, "1 msg") {
		t.Errorf("a1 header = %q, want the kept exchange counted", h)
	}
}

// The Timeline's Enter names subagent rows as readily as the conversation's, and
// a subagent's row inside a collapsed block is not on screen — the reveal must
// open the block rather than wait forever for a row that cannot appear.
func TestBlocks_RevealOpensACollapsedBlock(t *testing.T) {
	events := blockEvents()
	// Give the target a seq; the fixture leaves them zero.
	events[1].Seq = 11
	m := newBlockModel(t, events)
	m.revealSeq, m.revealSess = 11, "s"

	if !m.revealPendingSeq() {
		t.Fatal("reveal did not land")
	}
	if !m.subagentOpen["a1"] {
		t.Error("a1's block was not opened")
	}
	cur := m.eventsTbl.Cursor()
	if m.visibleRows[cur].event == nil || m.visibleRows[cur].event.Seq != 11 {
		t.Errorf("cursor on %v, want the event with seq 11", m.visibleRows[cur].event)
	}
}

// A Bob session (#1803): a subtask, then two parallel subagents, then a
// header-less router call, all in the root's session. Each subagent is a
// collapsed block labeled by its kind. The subtask is the same block, open by
// default and labeled "subtask", at its place in time. Enter still collapses it.
func TestEventsBlocks_BobSubagentsAndSubtask(t *testing.T) {
	ev := func(id, sub, kind string, phase pipeline.SessionPhase) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: phase, Host: "ete",
			RequestID: id, Subagent: sub, SubagentKind: kind}
	}
	req, resp := pipeline.SessionRequest, pipeline.SessionResponse
	m := newBlockModel(t, []pipeline.SessionEvent{
		ev("r1", "", "", req), ev("r1", "", "", resp),
		ev("s1", "c-sub", pipeline.SubagentKindSubtask, req), ev("s1", "c-sub", pipeline.SubagentKindSubtask, resp),
		ev("a1", "c-a", pipeline.SubagentKindSubagent, req),
		ev("b1", "c-b", pipeline.SubagentKindSubagent, req),
		ev("a1", "c-a", pipeline.SubagentKindSubagent, resp),
		ev("b1", "c-b", pipeline.SubagentKindSubagent, resp),
		ev("x1", "", "", req), ev("x1", "", "", resp),
		ev("r2", "", "", req), ev("r2", "", "", resp),
	})

	// Rendered: r1, header(c-sub), s1, header(c-a), header(c-b), x1, r2.
	rows := m.eventsTbl.Rows()
	if len(rows) != 7 || len(m.headerRows) != 3 {
		t.Fatalf("rendered rows = %d, headers = %d; want 7 and 3", len(rows), len(m.headerRows))
	}
	for pos, want := range map[int]string{1: "▾─ subtask c-sub", 3: "▸─ subagent c-a", 4: "▸─ subagent c-b"} {
		if h := rows[pos][0]; !strings.HasPrefix(h, want) {
			t.Errorf("row %d = %q, want prefix %q", pos, h, want)
		}
	}
	for pos, want := range map[int]string{0: "r1", 2: "s1", 5: "x1", 6: "r2"} {
		if e := m.visibleRows[pos].event; e == nil || e.RequestID != want {
			t.Errorf("row %d = %+v, want %s", pos, e, want)
		}
	}
	if e := m.visibleRows[5].event; e != nil && e.Subagent != "" {
		t.Errorf("header-less row filed in block %q", e.Subagent)
	}

	m.eventsTbl.SetCursor(1)
	if !m.toggleSubagentBlock() {
		t.Fatal("Enter on the subtask header did not toggle")
	}
	if got := len(m.eventsTbl.Rows()); got != 6 || !strings.HasPrefix(m.eventsTbl.Rows()[1][0], "▸─ subtask") {
		t.Fatalf("collapsed subtask: rows = %d, header = %q", got, m.eventsTbl.Rows()[1][0])
	}
	m.eventsTbl.SetCursor(1)
	m.toggleSubagentBlock()
	if got := len(m.eventsTbl.Rows()); got != 7 {
		t.Fatalf("re-opened subtask: rows = %d, want 7", got)
	}
}
