package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// t0 is a fixed instant for exchange fixtures. Tests that depend on the lost rule set m.now
// from it, so the wall clock never decides a result.
var t0 = time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)

// httpExchange is one outbound HTTP exchange: a request and its response, sharing id. An empty
// id makes the pair an older proxy's, which computeEventPairs can only guess.
func httpExchange(id, host string, at time.Time, status int) []pipeline.SessionEvent {
	return []pipeline.SessionEvent{{
		At: at, SessionID: "s", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: host, RequestID: id, BytesUp: 318,
	}, {
		At: at.Add(96 * time.Millisecond), SessionID: "s", Direction: pipeline.Outbound,
		Phase: pipeline.SessionResponse, Host: host, RequestID: id, StatusCode: status,
		Duration: 96 * time.Millisecond, BytesDown: 842,
	}}
}

// exchangeModel is sortTestModel with the clock pinned one second after the newest event,
// so no request in the fixture is old enough to be lost.
func exchangeModel(events []pipeline.SessionEvent) *model {
	m := sortTestModel(events)
	newest := t0
	for _, e := range events {
		if e.At.After(newest) {
			newest = e.At
		}
	}
	m.now = func() time.Time { return newest.Add(time.Second) }
	m.rebuildEventsTable()
	return m
}

func trimmedCell(t *testing.T, m *model, row int, id eventColumnID) string {
	t.Helper()
	return strings.TrimSpace(cellAt(t, m, row, id))
}

// The issue (#1681): another call's request lands between a request and its response, and the
// reader had to match # values by eye. Each exchange is one row now, at its request.
func TestExchange_PairIsOneRow(t *testing.T) {
	a := httpExchange("a", "a.test", t0, 200)
	b := httpExchange("b", "b.test", t0.Add(10*time.Millisecond), 503)
	m := exchangeModel([]pipeline.SessionEvent{a[0], b[0], a[1], b[1]})

	if got := hostsInOrder(m); !slices.Equal(got, []string{"a.test", "b.test"}) {
		t.Fatalf("rows = %v, want one per exchange, in request order", got)
	}
	for row, want := range map[int][2]string{0: {"1", "200"}, 1: {"2", "503"}} {
		if got := trimmedCell(t, m, row, colIndex); got != want[0] {
			t.Errorf("row %d # = %q, want %q", row, got, want[0])
		}
		if got := trimmedCell(t, m, row, colStatus); got != want[1] {
			t.Errorf("row %d STATUS = %q, want %q", row, got, want[1])
		}
	}
	if got := trimmedCell(t, m, 0, colBytes); got != "↑318B ↓842B" {
		t.Errorf("BYTES = %q, want both directions on one row", got)
	}
	if got := trimmedCell(t, m, 0, colDuration); got != "96ms" {
		t.Errorf("DURATION = %q, want the response's", got)
	}
}

// A guessed pair can belong to another call. Merging it would put that call's status on this
// row, so it stays two rows that share a number, as before.
func TestExchange_GuessedPairStaysTwoRows(t *testing.T) {
	m := exchangeModel(httpExchange("", "old.test", t0, 200))
	if n := len(m.visibleRows); n != 2 {
		t.Fatalf("%d rows, want 2: a pair with no RequestID must not merge", n)
	}
	if a, b := trimmedCell(t, m, 0, colIndex), trimmedCell(t, m, 1, colIndex); a != b {
		t.Errorf("# = %q and %q, want one number for the guessed pair", a, b)
	}
}

func TestExchange_SingleHalvesKeepOneRow(t *testing.T) {
	inflight := httpExchange("f", "f.test", t0, 200)[0]
	denied := pipeline.SessionEvent{
		At: t0.Add(time.Millisecond), SessionID: "s", Direction: pipeline.Outbound,
		Phase: pipeline.SessionDenied, Host: "d.test", RequestID: "d", StatusCode: 403,
	}
	orphan := httpExchange("o", "o.test", t0.Add(2*time.Millisecond), 200)[1]
	m := exchangeModel([]pipeline.SessionEvent{inflight, denied, orphan})

	if got := hostsInOrder(m); !slices.Equal(got, []string{"f.test", "d.test", "o.test"}) {
		t.Fatalf("rows = %v", got)
	}
	for row, want := range []string{"", "403", "200"} {
		if got := trimmedCell(t, m, row, colStatus); got != want {
			t.Errorf("row %d STATUS = %q, want %q", row, got, want)
		}
	}
}

func TestExchange_BridgedConnectWithResponseIsOneRow(t *testing.T) {
	open := pipeline.SessionEvent{
		At: t0, SessionID: "s", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Tunnel: true, Host: "api.test:443", RequestID: "t",
	}
	ex := httpExchange("x", "api.test", t0.Add(time.Millisecond), 200)
	m := exchangeModel(append([]pipeline.SessionEvent{open}, ex...))
	if n := len(m.visibleRows); n != 1 {
		t.Fatalf("%d rows, want 1: tunnel, request and response are one exchange", n)
	}
	if r := m.visibleRows[0]; r.tunnel == nil || r.resp == nil {
		t.Errorf("row = %+v, want the tunnel and the response folded in", r)
	}
}

func TestExchange_OpaqueTunnelIsOneRow(t *testing.T) {
	open := pipeline.SessionEvent{
		At: t0, SessionID: "s", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Tunnel: true, TunnelReason: "passthrough-host", Host: "api.test:443", RequestID: "p",
	}
	closed := pipeline.SessionEvent{
		At: t0.Add(3 * time.Minute), SessionID: "s", Direction: pipeline.Outbound,
		Phase: pipeline.SessionResponse, Tunnel: true, Host: "api.test:443", RequestID: "p",
		StatusCode: 200, Duration: 3 * time.Minute, BytesUp: 5000, BytesDown: 9000,
	}
	m := exchangeModel([]pipeline.SessionEvent{open, closed})
	if n := len(m.visibleRows); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
	if got := trimmedCell(t, m, 0, colBytes); got != "↑5.0kB ↓9.0kB" {
		t.Errorf("BYTES = %q", got)
	}
	if got := trimmedCell(t, m, 0, colAction); got != tunnelAction {
		t.Errorf("ACTION = %q, want %q", got, tunnelAction)
	}
}

func TestExchange_TokensAndCost(t *testing.T) {
	saving := &event.Saving{Component: "tool-prune", TokensAvoided: 3100, USD: 0.0037, Estimated: true}
	m := sizingModel(t, 200, sizingExchange(t, "x", "api.test", t0, 70_600, saving))
	if got := trimmedCell(t, m, 0, colTokens); got != "70.6k(−3.1k)→208" {
		t.Errorf("TOKENS = %q, want prompt(saving)→output", got)
	}
	if got := trimmedCell(t, m, 0, colCost); got != "$0.2767(−$0.0037)" {
		t.Errorf("COST = %q, want the exchange total with the saving", got)
	}
}

// The widest merged TOKENS cell measured on real traffic is 20 wide. It must render whole.
func TestExchange_WidestTokensCellFits(t *testing.T) {
	ex := sizingExchange(t, "w", "api.test", t0, 100_000,
		&event.Saving{Component: "tool-prune", TokensAvoided: 10_300, Estimated: true})
	ex[1].Inference.OutputTokens = 99_900
	m := sizingModel(t, 200, ex)
	const want = "100.0k(−10.3k)→99.9k"
	if got := trimmedCell(t, m, 0, colTokens); got != want {
		t.Errorf("TOKENS = %q, want %q whole", got, want)
	}
	if w := renderedWidth(t, m, colTokens); w != 20 {
		t.Errorf("TOKENS width = %d, want 20", w)
	}
}

// The same two plugins observe both halves of most inference calls. A plain union of the
// halves read "4 plugins".
func TestExchange_PluginCountsDistinctNames(t *testing.T) {
	obs := func(names ...string) *pipeline.Invocations {
		inv := &pipeline.Invocations{}
		for _, n := range names {
			inv.Outbound = append(inv.Outbound, pipeline.Invocation{Plugin: n, Action: pipeline.ActionObserve})
		}
		return inv
	}
	ex := httpExchange("x", "api.test", t0, 200)
	ex[0].Invocations = obs("inference-parser", "thinking-fill")
	ex[1].Invocations = obs("thinking-fill", "inference-parser")
	m := exchangeModel(ex)
	if got := trimmedCell(t, m, 0, colPlugin); got != "2 plugins" {
		t.Errorf("PLUGIN = %q, want 2 plugins", got)
	}
}

func TestExchange_ResponseDenyWinsAction(t *testing.T) {
	ex := httpExchange("x", "api.test", t0, 403)
	ex[0].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "parser", Action: pipeline.ActionObserve}}}
	ex[1].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "guard", Action: pipeline.ActionDeny}}}
	m := exchangeModel(ex)
	if a, p := trimmedCell(t, m, 0, colAction), trimmedCell(t, m, 0, colPlugin); a != "deny" || p != "guard" {
		t.Errorf("ACTION/PLUGIN = %q/%q, want deny/guard", a, p)
	}
}

func TestExchange_SortByDurationRanksExchanges(t *testing.T) {
	slow := httpExchange("s", "slow.test", t0, 200)
	slow[1].Duration = 2 * time.Second
	fast := httpExchange("f", "fast.test", t0.Add(time.Millisecond), 200)
	m := exchangeModel(append(slow, fast...))
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	if got := hostsInOrder(m); !slices.Equal(got, []string{"slow.test", "fast.test"}) {
		t.Errorf("rows = %v, want the slow exchange first", got)
	}
}

func TestExchange_HideInactiveJudgesBothHalves(t *testing.T) {
	acted := httpExchange("a", "acted.test", t0, 200)
	acted[1].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "p", Action: pipeline.ActionObserve}}}
	idle := httpExchange("i", "idle.test", t0.Add(time.Millisecond), 200)
	m := exchangeModel(append(acted, idle...))
	m.hideInactive = true
	m.rebuildEventsTable()
	if got := hostsInOrder(m); !slices.Equal(got, []string{"acted.test"}) {
		t.Errorf("rows = %v, want only the exchange a plugin acted on", got)
	}
}

// A block's header counts exchanges. With merged rows that is also the number of rows it shows.
func TestExchange_BlockCountsRows(t *testing.T) {
	var events []pipeline.SessionEvent
	for i := range 3 {
		ex := httpExchange(fmt.Sprintf("x%d", i), "sub.test", t0.Add(time.Duration(i)*time.Millisecond), 200)
		ex[0].Subagent, ex[1].Subagent = "agent-1", "agent-1"
		events = append(events, ex...)
	}
	m := exchangeModel(events)
	m.subagentOpen = map[string]bool{"agent-1": true}
	m.rebuildEventsTable()
	if n := len(m.visibleRows); n != 4 {
		t.Fatalf("%d rows, want the header and 3 exchanges", n)
	}
	if b := m.headerRows[0]; b == nil || b.msgs != 3 {
		t.Errorf("header block = %+v, want 3 msgs", b)
	}
}

// Review focus: the status moved onto the request's row, so a search for it must still land.
func TestExchange_SearchFindsResponseStatus(t *testing.T) {
	m := newSearchEventsModel(t)
	at := time.Now().Add(-time.Minute)
	m.events["s"] = append(httpExchange("a", "a.test", at, 200), httpExchange("b", "b.test", at.Add(time.Second), 503)...)
	m.rebuildEventsTable()
	typeKeys(m, "/503")
	press(m, tea.KeyEnter)
	if got := m.eventsTbl.Matches(); !slices.Equal(got, []int{1}) {
		t.Errorf("matches = %v, want [1]: b's merged row", got)
	}
}

// A config file written before PHASE was removed must load: the entry is ignored and the sort
// falls back to chronological.
func TestSettings_PhaseEntryLoads(t *testing.T) {
	var u UserSettings
	u.Events.Columns = []ColumnSetting{{Name: "PHASE", Visible: true}, {Name: "HOST", Visible: false}}
	u.Events.SortColumn = "PHASE"
	sel := u.columnSelection()
	if _, ok := sel["PHASE"]; ok {
		t.Error("PHASE is still in the selection")
	}
	if sel[colHost] {
		t.Error("HOST is on, want the file's off")
	}
	if col, ok := u.sortSelection(); ok || col != "" {
		t.Errorf("sortSelection = %q, %v, want chronological", col, ok)
	}
}

// lostModel holds the given events with the clock at now.
func lostModel(now time.Time, events ...pipeline.SessionEvent) *model {
	m := sortTestModel(events)
	m.now = func() time.Time { return now }
	m.rebuildEventsTable()
	return m
}

// The Timeline's rule (timeline.HostLostAfter): a request with no answer for more than 10
// minutes, measured at the later of now and the session's newest event, is lost.
func TestExchange_LostRequest(t *testing.T) {
	req := httpExchange("l", "lost.test", t0, 200)[0]

	m := lostModel(t0.Add(11*time.Minute), req)
	if got := trimmedCell(t, m, 0, colStatus); got != "lost" {
		t.Errorf("11 min: STATUS = %q, want lost", got)
	}
	m = lostModel(t0.Add(9*time.Minute), req)
	if got := trimmedCell(t, m, 0, colStatus); got != "" {
		t.Errorf("9 min: STATUS = %q, want blank: still on its way", got)
	}
	// The newest event decides too, when the clock is behind it (a shifted demo clock).
	newer := httpExchange("n", "new.test", t0.Add(11*time.Minute), 200)
	m = lostModel(t0, append([]pipeline.SessionEvent{req}, newer...)...)
	if got := trimmedCell(t, m, 0, colStatus); got != "lost" {
		t.Errorf("newest event 11 min later: STATUS = %q, want lost", got)
	}
}

// A passthrough tunnel stays open for hours, and its open row carries no status. It is never
// lost.
func TestExchange_TunnelOpenIsNeverLost(t *testing.T) {
	open := pipeline.SessionEvent{
		At: t0, SessionID: "s", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Tunnel: true, TunnelReason: "passthrough-host", Host: "api.test:443", RequestID: "t",
	}
	m := lostModel(t0.Add(6*time.Hour), open)
	if got := trimmedCell(t, m, 0, colStatus); got != "" {
		t.Errorf("STATUS = %q, want blank", got)
	}
}

func TestExchange_LateResponseMergesIntoLostRow(t *testing.T) {
	ex := httpExchange("l", "late.test", t0, 200)
	m := lostModel(t0.Add(20*time.Minute), ex[0])
	if got := trimmedCell(t, m, 0, colStatus); got != "lost" {
		t.Fatalf("before: STATUS = %q, want lost", got)
	}
	m.events["s"] = ex
	m.rebuildEventsTable()
	if got := trimmedCell(t, m, 0, colStatus); got != "200" {
		t.Errorf("after the response: STATUS = %q, want 200", got)
	}
}
