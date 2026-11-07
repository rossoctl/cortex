# agentop: one row per exchange — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show each request and its response as one row in the agentop events pane, and both halves of the exchange on one full-width detail page.

**Architecture:** `rebuildEventsTable` folds each exact (RequestID) pair into the row of its request, after `computeEventPairs` has numbered the exchanges. Every cell reads the exchange through two helpers (`answer`, `exchangeResp`). The detail tree gets a section node kind, and `showDetail` builds a REQUEST and a RESPONSE section. A fixed line above the viewport names the section on screen.

**Tech Stack:** Go 1.26.10, Bubble Tea, lipgloss, agentop's own `tui/table` copy.

**Spec:** `docs/superpowers/specs/2026-11-07-agentop-exchange-rows-design.md` (commit `2e95e001`). Read it before each task. The plan argues from it.

## Global Constraints

- Go 1.26.10. No new module dependency.
- Run Go commands per module with `GOWORK=off`. Module roots: `cmd/agentop` and `core`.
- Before each commit, run `gofmt -l <path>` and `go vet ./...` in each module you touched. Always give `gofmt` a path.
- An agentop table imports `github.com/rossoctl/cortex/cmd/agentop/tui/table`, never `charmbracelet/bubbles/table`.
- Commit with `git commit -s` (DCO). End every commit message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never add `Co-Authored-By`.
- Commit subjects: `<Prefix>: <Subject starting with uppercase>`. Use `Feat:`, `Fix:`, `Refactor:`, `Test:` or `Docs:`.
- Write commit messages and code comments in ASD-STE100 Simplified Technical English: short sentences, active voice, no semicolons.
- Match the comment density of the file you edit. These files explain *why* at length. A new non-obvious rule gets a comment that says why.
- Merge only pairs with the same RequestID. A guessed pair stays two rows.
- Merged-only: no key or setting brings back two rows per exchange.
- `#` keeps the numbers `computeEventPairs` gives today.
- The lost rule is `timeline.HostLostAfter` (10 minutes), measured at the later of now and the session's newest event. It never applies to a tunnel open.
- TOKENS cap: 20. COST cap stays 19. BYTES cap stays 17.
- `y` writes one event per file, as today.

## Review Focus

These five conditions are implied by the spec, but no spec test names them. Each one has a test in the task that owns the code.

1. **A table sorted by DURATION when a response lands.** The merged row moves to its new rank, and the cursor stays on it (Task 4, `TestExchange_SortedCursorStaysWhenResponseLands`).
2. **`/503` in the events pane.** The search finds the merged row, because the status is now on the request's row (Task 2, `TestExchange_SearchFindsResponseStatus`).
3. **A 60-column terminal.** The detail's fixed line is cut to the terminal width and never wraps (Task 6, `TestDetail_FixedLineFitsNarrowTerminal`).
4. **`y` with the cursor in a missing half.** The footer says there is nothing to yank, and no file is written (Task 6, `TestDetail_YankMissingHalfWritesNothing`).
5. **`Tab` on a row whose request is not held.** The cursor moves to the REQUEST note and nothing panics (Task 6, `TestDetail_TabWithOneHalf`).

---
### Task 1: Export the Timeline's lost rule

The Events pane must use the Timeline's rule for a request that never got an answer, so the two panes agree. The constant is unexported today.

**Files:**
- Modify: `core/session/timeline/hosts.go:102-105`
- Modify: `core/session/timeline/timeline.go:258-270`

**Interfaces:**
- Produces: `timeline.HostLostAfter time.Duration` (10 minutes). Task 3 uses it.

- [ ] **Step 1: Rename the constant and its one use**

In `core/session/timeline/hosts.go`, replace the comment and constant:

```go
// HostLostAfter is how long an unanswered request may sit before it is called lost. Ten
// minutes covers the longest real model calls (the slowest seen was under 22 minutes, and
// those were the rare tail), while not drawing a dead stream as busy.
//
// Exported for agentop's events pane, which marks the same requests lost by the same rule,
// so a request is never lost in one pane and in flight in the other (#1681).
const HostLostAfter = 10 * time.Minute
```

In `core/session/timeline/timeline.go`, in the `LoseUnanswered` comment and body, replace `hostLostAfter` with `HostLostAfter` (two places).

- [ ] **Step 2: Check that nothing else names the old constant**

Run: `grep -rn "hostLostAfter" --include=*.go .` from the repo root.
Expected: no output.

- [ ] **Step 3: Run the timeline tests**

Run: `cd core && GOWORK=off go test ./session/timeline/ && GOWORK=off go vet ./session/timeline/ && gofmt -l session/timeline`
Expected: `ok`, no vet output, no gofmt output.

- [ ] **Step 4: Commit**

```bash
git add core/session/timeline/hosts.go core/session/timeline/timeline.go
git commit -s -F - <<'EOF'
Refactor: Export the timeline's lost-request limit

agentop's events pane will mark a request lost by the same rule as the
Timeline (#1681). Export the limit so that the two cannot disagree.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 2: One events row per exchange

The rows, the cells that read the response, the sort keys, the PLUGIN count, and the removal of PHASE land together. A merged row without its cells shows blank figures, so none of these parts works alone.

**Files:**
- Modify: `cmd/agentop/tui/events_pane.go` (`eventRow`, `invocations`, `rebuildEventsTable`, `topInvocation`, `shortPhase`, `foldPairKeys`, `pairedResponse`, `tokensCell`, `costCell`, `generatedTokensCell`, `bytesCell` callers)
- Modify: `cmd/agentop/tui/events_columns.go` (column table, `pairFold`, `durationSortKey`, `rowTokens`, `rowCostUSD`)
- Modify: `cmd/agentop/tui/cost_event.go` (add `exchangeCost`)
- Create: `cmd/agentop/tui/events_exchange_test.go`
- Modify: the existing tests that assume two rows per exchange (Step 9 lists them)

**Interfaces:**
- Produces:
  - `eventRow.resp *pipeline.SessionEvent` and `eventRow.respRaw *pipeline.RawCapture`
  - `mergeExchanges(rows []eventRow, partner map[int]int) ([]eventRow, map[int]int)`
  - `answer(r eventRow) *pipeline.SessionEvent`, `exchangeResp(r eventRow) *pipeline.SessionEvent`
  - `exchangeBytes(r eventRow) (up, down int64)`
  - `exchangeTokensCell(r eventRow) string`, `exchangeCostCell(r eventRow) string`
  - `exchangeCost(resp *pipeline.SessionEvent) (float64, bool)`
  - test helper `httpExchange(id, host string, at time.Time, status int) []pipeline.SessionEvent`
- Later tasks use all of these.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agentop/tui/events_exchange_test.go`:

```go
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
```

- [ ] **Step 2: Run the new tests to see them fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run 'TestExchange_|TestSettings_PhaseEntryLoads' 2>&1 | tail -20`
Expected: compile errors or failures, for example `rows = [a.test b.test a.test b.test]`.

- [ ] **Step 3: Give a row its response, and fold pairs**

In `cmd/agentop/tui/events_pane.go`, replace the `eventRow` type and `invocations`:

```go
// eventRow is one display row: one exchange (#1681). event is the row's first event — the
// request, or the only event of a row with one half: a denial, a request no answer reached,
// a response whose request is not held. resp is the response mergeExchanges folded in.
// tunnel, when non-nil, is a CONNECT tunnel-open event folded into a TLS-bridged request
// (see buildEventRows); it contributes a summary line to the detail pane but no separate row.
type eventRow struct {
	event  *pipeline.SessionEvent
	tunnel *pipeline.SessionEvent
	resp   *pipeline.SessionEvent
	// raw and respRaw are the exact captures of event and resp, set on the row the detail
	// pane fetched them for.
	raw     *pipeline.RawCapture
	respRaw *pipeline.RawCapture
}

// invocations returns every plugin invocation the row's ACTION/PLUGIN cell and the inactive
// filter should consider: both halves of the exchange, plus any folded CONNECT tunnel's gate
// invocations — so a bridged row reflects activity on the tunnel-open (e.g. an egress gate
// that allowed the CONNECT) and isn't wrongly hidden or under-reported.
func (er eventRow) invocations() []pipeline.Invocation {
	invs := allInvocations(er.event)
	if er.tunnel != nil {
		invs = append(invs, allInvocations(er.tunnel)...)
	}
	if er.resp != nil {
		invs = append(invs, allInvocations(er.resp)...)
	}
	return invs
}
```

Add after `buildEventRows`:

```go
// mergeExchanges folds each response into the row of the request it answers (#1681): one row
// per exchange, at the request's position, so a response changes its row instead of adding
// one.
//
// ONLY AN EXACT PAIR MERGES — one partner joined by RequestID. The heuristic pass in
// computeEventPairs can pair a response with another call's request, and a merged row would
// then show that call's status and duration as this one's. A guessed pair stays two rows.
//
// Returns the merged rows and the partner map re-indexed onto them. Only guessed pairs are
// left in it, which is what rowInactiveHidden still needs to keep such a pair together.
func mergeExchanges(rows []eventRow, partner map[int]int) ([]eventRow, map[int]int) {
	out := make([]eventRow, 0, len(rows))
	at := make(map[int]int, len(rows)) // index in rows → index in out
	for i, r := range rows {
		if j, ok := partner[i]; ok && exactPair(rows, i, j) {
			if r.event.Phase != pipeline.SessionRequest {
				continue // it went out with its request
			}
			r.resp = rows[j].event
		}
		at[i] = len(out)
		out = append(out, r)
	}
	left := make(map[int]int)
	for i, j := range partner {
		if !exactPair(rows, i, j) {
			left[at[i]] = at[j]
		}
	}
	return out, left
}

// exactPair reports that rows i and j share a RequestID: the pair computeEventPairs made from
// the proxy's stamp, not from its guess.
func exactPair(rows []eventRow, i, j int) bool {
	a, b := rows[i].event, rows[j].event
	return a.RequestID != "" && a.RequestID == b.RequestID
}

// answer is the event that holds row r's outcome — STATUS, DURATION, the response's bytes:
// the merged response when there is one, else the row's own event (a denial, a response
// whose request is not held, or a request no answer reached yet).
func answer(r eventRow) *pipeline.SessionEvent {
	if r.resp != nil {
		return r.resp
	}
	return r.event
}

// exchangeResp is the response that speaks for row r: the merged one, or the row's own event
// when the row is a response. Nil while no answer arrived — which is also what a guessed
// pair's request row gets, as pairedResponse always gave it.
func exchangeResp(r eventRow) *pipeline.SessionEvent {
	if r.resp != nil {
		return r.resp
	}
	if r.event != nil && r.event.Phase == pipeline.SessionResponse {
		return r.event
	}
	return nil
}

// exchangeBytes is what row r carried: the request's body up and the response's down. An
// opaque tunnel's close row counts both directions itself, so it answers alone.
func exchangeBytes(r eventRow) (up, down int64) {
	if r.resp == nil || r.resp.Tunnel {
		a := answer(r)
		return a.BytesUp, a.BytesDown
	}
	return r.event.BytesUp, r.resp.BytesDown
}
```

- [ ] **Step 4: Wire the merge into `rebuildEventsTable`**

In `rebuildEventsTable`, replace:

```go
	ids, partner := computeEventPairs(eventRows)
```

with:

```go
	ids, partner := computeEventPairs(eventRows)
	// One row per exchange (#1681). ids stays the pass above's: it is keyed by event pointer,
	// so a merged row keeps the number its request had, and # reads as it always did.
	eventRows, partner = mergeExchanges(eventRows, partner)
```

Remove the `foldPairKeys` call and its comment from the sort block, so it reads:

```go
	if sortCol != nil {
		sortEventRows(rows, m.visibleRows, full, keys, m.sortDesc)
	}
```

Delete the `foldPairKeys` function and its comment. In the comments of `rowKey` and `sortEventRows`, replace each sentence that names `foldPairKeys` or says that a pair shares the request's arrival index with this sentence: "Each row is one exchange (#1681), so exchange is the row's own arrival index."

- [ ] **Step 5: Change the cells and sort keys**

In `cmd/agentop/tui/cost_event.go`, after `outputCost`, add:

```go
// exchangeCost is what one exchange cost in total: the record's cost_usd (#1681). It is the
// figure an authoritative gateway sets, and on real traffic prompt_usd + output_usd equal it
// to within 1% in 5,704 of 5,707 records.
func exchangeCost(resp *pipeline.SessionEvent) (usd float64, ok bool) {
	ev, ok := event.Record(resp)
	if !ok || ev.CostUSD <= 0 {
		return 0, false
	}
	return ev.CostUSD, true
}
```

In `cmd/agentop/tui/events_pane.go`, delete `pairedResponse`, `generatedTokensCell`, the method `tokensCell` and the method `costCell`, with their comments. Add in their place:

```go
// outputTokens is what a response generated. CompletionTokens is the older field for the same
// count.
func outputTokens(inf *pipeline.InferenceExtension) int {
	if inf.OutputTokens > 0 {
		return inf.OutputTokens
	}
	return inf.CompletionTokens
}

// exchangeTokensCell renders an exchange's TOKENS as prompt → output, each compact, with what
// tool-prune kept off the prompt in parentheses: "70.6k(−3.1k)→9" (#1681). The provider is
// the only party that tokenizes, so both figures are on the response, and the cell is blank
// until it arrives.
func exchangeTokensCell(r eventRow) string {
	resp := exchangeResp(r)
	if resp == nil || resp.Inference == nil {
		return ""
	}
	prompt, out := pipeline.PromptTokensOf(resp.Inference), outputTokens(resp.Inference)
	if prompt <= 0 && out <= 0 {
		return ""
	}
	cell := formatCompact(float64(prompt))
	if s, ok := pruneSavingFor(resp); ok && s.TokensAvoided > 0 {
		cell += "(" + savingSign(s.Projected) + formatCompact(float64(s.TokensAvoided)) + ")"
	}
	return cell + "→" + formatCompact(float64(out))
}

// exchangeCostCell renders an exchange's COST: its total, with what tool-prune saved in
// parentheses. Blank when no figure was published, which reads as "not reported".
func exchangeCostCell(r eventRow) string {
	resp := exchangeResp(r)
	if resp == nil {
		return ""
	}
	usd, ok := exchangeCost(resp)
	if !ok {
		return ""
	}
	var saved float64
	var projected bool
	if s, ok := pruneSavingFor(resp); ok {
		saved, projected = s.USD, s.Projected
	}
	return money.Relabel(formatUSDWithSaving(usd, saved, projected), recordUnit(resp), 0)
}
```

Replace `topInvocation`, so that a tie counts distinct plugin names:

```go
// topInvocation returns the highest-ranked invocation satisfying keep and how many distinct
// plugins tied at that top rank. Distinct, because a row is an exchange (#1681): the same
// two plugins observe both halves of most inference calls, and counting invocations read
// "4 plugins". winner is the zero Invocation (Action "", rank 0) when none satisfy keep.
func topInvocation(invs []pipeline.Invocation, keep func(pipeline.Invocation) bool) (pipeline.Invocation, int) {
	best := -1
	var winner pipeline.Invocation
	var names []string
	for _, iv := range invs {
		if !keep(iv) {
			continue
		}
		switch r := actionRank(iv.Action); {
		case r > best:
			best, winner, names = r, iv, []string{iv.Plugin}
		case r == best && !slices.Contains(names, iv.Plugin):
			names = append(names, iv.Plugin)
		}
	}
	return winner, len(names)
}
```

Delete `shortPhase` and its comment.

In `cmd/agentop/tui/events_columns.go`:

1. Delete the `colPhase` constant and the PHASE entry of `eventColumns` (its comment too).
2. Delete the `pairFold` field and its comment from `eventColumn`. Remove `pairFold: true` from the STATUS and DURATION entries. In the `sortKey` comment, replace the sentences about a request row reaching its paired response with: "Takes a cellContext rather than an event because a row is an exchange (#1681), and the figures of most columns are on its response."
3. Replace `durationSortKey`:

```go
// durationSortKey ranks a tunnel row with the blank ones: its DURATION is how long the
// tunnel stayed open, not how long a call took.
func durationSortKey(c cellContext) sortValue {
	if a := answer(c.row); !a.Tunnel {
		return numKey(a.Duration)
	}
	return numKey(0)
}
```

4. Change these entries of `eventColumns` (keep the comments above each entry, and edit any sentence there that says a request and its response are two rows):

```go
	{id: colIndex, width: 4, defaultOn: true, keep: keepHigh,
		desc: "exchange number: one per request and its response, numbered per subagent block",
		// cell unchanged
```

```go
	{id: colTime, width: 11, defaultOn: true, keep: keepNormal,
		desc: "wall-clock time the request was recorded",
		// cell and sortKey unchanged
```

```go
	{id: colStatus, width: 7, defaultOn: true, keep: keepNormal, rightAlign: true,
		desc:    "HTTP status of the response; blank while it is on its way",
		cell:    func(c cellContext) string { return statusCell(*answer(c.row)) },
		sortKey: func(c cellContext) sortValue { return numKey(answer(c.row).StatusCode) }},
```

```go
	{id: colDuration, width: 9, defaultOn: true, keep: keepLow, rightAlign: true,
		desc:    "how long the exchange took",
		cell:    func(c cellContext) string { return durationCell(*answer(c.row)) },
		sortKey: durationSortKey},
```

```go
	{id: colBytes, width: 17, defaultOn: true, keep: keepLow, rightAlign: true,
		desc: "body bytes: ↑ request sent, ↓ response received",
		cell: func(c cellContext) string {
			e := *answer(c.row)
			e.BytesUp, e.BytesDown = exchangeBytes(c.row)
			return bytesCell(e)
		},
		sortKey: func(c cellContext) sortValue {
			up, down := exchangeBytes(c.row)
			return numKey(up + down)
		}},
```

```go
	// 20: the widest merged cell measured on real traffic, "100.0k(−10.3k)→99.9k" (#1681).
	// A cap, not the width: fitsContent renders the column as wide as the session's widest
	// cell, and without a saving most cells are "70.6k→9".
	{id: colTokens, width: 20, defaultOn: true, keep: keepLow, rightAlign: true, fitsContent: true,
		desc:    "prompt→output tokens, and what tool-prune saved",
		cell:    func(c cellContext) string { return exchangeTokensCell(c.row) },
		sortKey: func(c cellContext) sortValue { return numKey(rowTokens(c)) }},
```

```go
	{id: colCost, width: 19, defaultOn: true, keep: keepLow, rightAlign: true, fitsContent: true,
		desc:    "the exchange's estimated cost, and what tool-prune saved",
		cell:    func(c cellContext) string { return exchangeCostCell(c.row) },
		sortKey: func(c cellContext) sortValue { return numKey(rowCostUSD(c)) }},
```

5. In the `cellContext` doc comment, replace the paragraph that says TOKENS and COST pair a request with its response with: "rows, partner and i locate the row among the merged rows. After mergeExchanges, partner holds only guessed pairs (#1681)."
6. Replace `rowTokens` and `rowCostUSD` and their comment:

```go
// rowTokens and rowCostUSD are the numeric readings behind the TOKENS and COST cells, for
// sorting (#865) and for a subagent block's rollup: the exchange's totals (#1681). Zero means
// "nothing to order by" — no figure published, or no response yet — and lands at the bottom
// of a descending sort.
func rowTokens(c cellContext) int {
	resp := exchangeResp(c.row)
	if resp == nil || resp.Inference == nil {
		return 0
	}
	return pipeline.PromptTokensOf(resp.Inference) + outputTokens(resp.Inference)
}

func rowCostUSD(c cellContext) float64 {
	resp := exchangeResp(c.row)
	if resp == nil {
		return 0
	}
	usd, _ := exchangeCost(resp)
	return usd
}
```

- [ ] **Step 6: Build and run the new tests**

Run: `cd cmd/agentop && GOWORK=off go build ./... && GOWORK=off go test ./tui/ -run 'TestExchange_|TestSettings_PhaseEntryLoads' -v 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: every listed test `--- PASS`. If the build fails, the message names a caller of a deleted function: Step 7 handles callers.

- [ ] **Step 7: Remove what nothing calls now**

Run: `grep -n "outputCost\|promptCost" cmd/agentop/tui/*.go | grep -v _test.go`
If `outputCost` has no caller outside tests, delete it and the tests that call only it. Keep `promptCost`: `prune_saving.go` uses it.

- [ ] **Step 8: Run the whole tui suite**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ 2>&1 | grep -E '^(--- FAIL|FAIL|ok|.*cannot|.*undefined)' | head -60`
Expected: failures in tests that assume two rows per exchange. Step 9 says how to handle each kind.

- [ ] **Step 9: Migrate the split-row tests**

The files that build request/response fixtures and read the events table are: `events_pane_test.go`, `events_columns_test.go`, `events_column_sizing_test.go`, `events_blocks_test.go`, `header_align_test.go`, `search_test.go`, `cost_event_test.go`, `local_preview_test.go`, `money_units_test.go`, `tunnel_close_test.go`. Treat each failing test by this rule, and do not weaken an assertion to make it pass:

| The test asserts | Do this |
|---|---|
| a removed mechanism: `foldPairKeys`, `pairFold`, `shortPhase`, `colPhase`, `pairedResponse`, `generatedTokensCell` | Delete the test. The behaviour it guarded no longer exists. |
| an exact pair (shared RequestID) renders as two rows | Rewrite it: one row, with the same facts read from that row (STATUS, DURATION, BYTES, TOKENS, COST, `#`). |
| a request row's TOKENS or COST from its paired response, or a response row's own figure | Rewrite it to assert the merged cell (`70.6k→208`, the total cost). Call `exchangeTokensCell` / `exchangeCostCell` where it called `m.tokensCell` / `m.costCell`. |
| a sort keeps a request beside its response | Rewrite it: the exchange is one row, so assert its rank. |
| row counts or row indexes in a fixture of exact pairs | Halve the count, and move each index to the merged row. |
| a guessed pair (no RequestID) | Keep it as it is. It must still pass unchanged. |
| a row count in a layout or width test | Keep the layout assertion. Fix only the count of rows. |

After each file, run that file's tests: `GOWORK=off go test ./tui/ -run '<TestNames>'`.

- [ ] **Step 10: Run every check**

Run: `cd cmd/agentop && GOWORK=off go test ./... && GOWORK=off go vet ./... && gofmt -l tui`
Expected: all `ok`, no vet output, no gofmt output. One known exception: the root `cmd/agentop` package can time out in `TestSetupRollbackWaitsOutLaunchdTeardown`, and it does so on `main` too. If that is the only failure, say so in the task report and continue.

- [ ] **Step 11: Commit**

```bash
git add cmd/agentop/tui
git commit -s -F - <<'EOF'
Feat: Show each request and its response as one events row

agentop's events pane showed a request and its response as two rows,
and other rows could land between them (#1681). Each exact pair is now
one row at its request. STATUS, DURATION and BYTES read the response.
TOKENS shows prompt→output, and COST shows the exchange total. PLUGIN
counts distinct plugins. PHASE is removed. A pair the proxy did not
stamp with a RequestID stays two rows.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 3: Mark requests that never got an answer as lost

On real traffic, 52 requests have no response while later rows in the same session do. With merged rows, a blank STATUS means "on its way", so these rows must say `lost`.

**Files:**
- Modify: `cmd/agentop/tui/events_columns.go` (`cellContext`, STATUS entry)
- Modify: `cmd/agentop/tui/events_pane.go` (`rebuildEventsTable`, new `lostRef`, `rowLost`)
- Test: `cmd/agentop/tui/events_exchange_test.go`

**Interfaces:**
- Consumes: `timeline.HostLostAfter` (Task 1), `httpExchange`, `exchangeModel`, `t0`, `trimmedCell` (Task 2).
- Produces: `cellContext.lost bool`, `(m *model) lostRef(events []pipeline.SessionEvent) time.Time`, `rowLost(r eventRow, partner map[int]int, i int, ref time.Time) bool`. Task 6 uses `lostRef` and `rowLost`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/agentop/tui/events_exchange_test.go`:

```go
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
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run 'TestExchange_(LostRequest|TunnelOpenIsNeverLost|LateResponseMergesIntoLostRow)' 2>&1 | tail -8`
Expected: FAIL, `11 min: STATUS = "", want lost`.

- [ ] **Step 3: Implement the rule**

In `cmd/agentop/tui/events_columns.go`, add to `cellContext` after `plugin`:

```go
	// lost is rowLost's answer for this row, decided once per rebuild against one instant.
	lost bool
```

Change the STATUS entry's `desc` and `cell`:

```go
		desc: "HTTP status of the response; blank while it is on its way, lost when none came",
		cell: func(c cellContext) string {
			if c.lost {
				return "lost"
			}
			return statusCell(*answer(c.row))
		},
```

In `cmd/agentop/tui/events_pane.go`, add the import `"github.com/rossoctl/cortex/core/session/timeline"`, and add after `exchangeBytes`:

```go
// lostRef is the instant the lost rule measures against: the later of now and the newest
// event held. It is the ref Timeline.LoseUnanswered uses, so the two panes call the same
// request lost. The newest event matters when the clock is behind the session's — the
// README demo runs one shifted.
func (m *model) lostRef(events []pipeline.SessionEvent) time.Time {
	ref := m.clock()
	for i := range events {
		if events[i].At.After(ref) {
			ref = events[i].At
		}
	}
	return ref
}

// rowLost reports that row i is a request no answer will reach (#1681): no response, and
// older than timeline.HostLostAfter at ref. On real traffic 52 such requests sat among
// answered ones, and a blank STATUS would have said "on its way" forever.
//
// A tunnel open is never lost. It carries no status, and a passthrough tunnel stays open for
// hours (6.4 measured), so age says nothing about it. A guessed pair's request is not lost
// either: its response is on a row of its own.
func rowLost(r eventRow, partner map[int]int, i int, ref time.Time) bool {
	e := r.event
	if r.resp != nil || e == nil || e.Phase != pipeline.SessionRequest || e.Tunnel {
		return false
	}
	if _, ok := partner[i]; ok {
		return false
	}
	return ref.Sub(e.At) > timeline.HostLostAfter
}
```

In `rebuildEventsTable`, before the `ctxs` loop, add:

```go
	ref := m.lostRef(events)
```

and set the field in the loop's `cellContext` literal:

```go
		ctxs[i] = cellContext{
			m: m, rows: eventRows, partner: partner, i: i, row: er, ids: ids,
			invs: invs, action: action, plugin: plugin, lost: rowLost(er, partner, i, ref),
		}
```

- [ ] **Step 4: Run the tests**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ 2>&1 | grep -E '^(--- FAIL|FAIL|ok)' | head -30`
Expected: the three new tests pass. A test whose fixture holds requests with no response and a zero or old `At` (`eventSeq`, `hostEvents`) can now read `lost` in STATUS. If such a test asserts a blank STATUS, it is about a request on its way: set `m.now` to within 10 minutes of the fixture's newest event. Do not change the rule.

- [ ] **Step 5: Run every check and commit**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ && GOWORK=off go vet ./... && gofmt -l tui`
Expected: `ok`, no other output.

```bash
git add cmd/agentop/tui
git commit -s -F - <<'EOF'
Feat: Mark events rows that never got an answer as lost

A merged row with a blank STATUS reads as a call on its way. A request
with no answer for more than 10 minutes now shows "lost", by the rule
the Timeline uses. A tunnel open is never lost, because a passthrough
tunnel can stay open for hours.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 4: Find a row by either half

The cursor pin, the search position and Enter in the Timeline find a row by one event. A merged row holds two, and the Timeline names a tool by its **response's** seq (`Tool.Seq = c.resp.Seq` in `core/session/timeline/agents.go`).

**Files:**
- Modify: `cmd/agentop/tui/events_pane.go` (`findByKey`, new `rowHolds`)
- Modify: `cmd/agentop/tui/timeline_pane.go` (`revealPendingSeq`)
- Test: `cmd/agentop/tui/events_exchange_test.go`

**Interfaces:**
- Consumes: `eventRow.resp`, `httpExchange`, `sortTestModel`, `hostsInOrder` (Task 2).
- Produces: `rowHolds(r eventRow, match func(*pipeline.SessionEvent) bool) bool`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/agentop/tui/events_exchange_test.go`:

```go
// A response whose request is not held is a row of its own, keyed by the response. When `o`
// loads the request, the row merges and its first event becomes the request. The pin must
// still find it, or the cursor falls back to row 0 — which is w's row here, so the fallback
// cannot pass for the fix.
func TestExchange_PinOnResponseFollowsTheMerge(t *testing.T) {
	w := httpExchange("w", "w.test", t0.Add(-time.Second), 200)
	x := httpExchange("x", "x.test", t0, 200)
	y := httpExchange("y", "y.test", t0.Add(time.Second), 200)
	m := newEventsPaneModel(append([]pipeline.SessionEvent{x[1]}, y...))
	m.now = func() time.Time { return t0.Add(2 * time.Second) }
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 0)
	m.selectedEventKey = keyOf(m.selectedEvent())

	// `o` loaded the older page: w, and x's request.
	m.events["s"] = append(append(w, x...), y...)
	m.rebuildEventsTable()
	if got := m.selectedEvent(); got == nil || got.Host != "x.test" {
		t.Errorf("cursor on %v, want x's merged row", got)
	}
}

// Enter on a tool in the Timeline names the response that called it.
func TestExchange_RevealResponseSeqLandsOnItsRow(t *testing.T) {
	a := httpExchange("a", "a.test", t0, 200)
	b := httpExchange("b", "b.test", t0.Add(time.Second), 200)
	a[0].Seq, a[1].Seq, b[0].Seq, b[1].Seq = 1, 2, 3, 4
	m := newEventsPaneModel(append(a, b...))
	m.revealSeq, m.revealSess = 2, "s"
	if !m.revealPendingSeq() {
		t.Fatal("the reveal of a response seq did not land")
	}
	if got := m.selectedEvent(); got.Host != "a.test" {
		t.Errorf("cursor on %s, want a.test", got.Host)
	}
}

// Review focus: under a sort, a response that lands re-ranks its row. The cursor stays on it.
func TestExchange_SortedCursorStaysWhenResponseLands(t *testing.T) {
	a := httpExchange("a", "a.test", t0, 200)
	a[1].Duration = time.Second
	b := httpExchange("b", "b.test", t0.Add(time.Millisecond), 200)
	m := sortTestModel([]pipeline.SessionEvent{a[0], b[0], a[1]})
	m.now = func() time.Time { return t0.Add(time.Minute) }
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	i := slices.IndexFunc(m.visibleRows, func(r eventRow) bool { return r.event.Host == "b.test" })
	setCursorVisible(&m.eventsTbl, i)
	m.selectedEventKey = keyOf(m.selectedEvent())

	b[1].Duration = 5 * time.Second
	m.events["s"] = append(m.events["s"], b[1])
	m.rebuildEventsTable()
	if got := hostsInOrder(m); got[0] != "b.test" {
		t.Errorf("rows = %v, want b first: 5s ranks above 1s", got)
	}
	if got := m.selectedEvent(); got.Host != "b.test" {
		t.Errorf("cursor on %s, want it to follow b", got.Host)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run 'TestExchange_(PinOnResponse|RevealResponseSeq|SortedCursorStays)' 2>&1 | tail -8`
Expected: FAIL for the first two (`cursor on … want x's merged row`, `did not land`). The third may already pass: it is a regression guard.

- [ ] **Step 3: Implement**

In `cmd/agentop/tui/events_pane.go`, replace `findByKey`:

```go
// findByKey is the row that holds the event with key k, by either half (#1681). A pin can
// name a response: a response-only row is keyed by it, and the row merges with its request
// when `o` loads the request.
func findByKey(rows []eventRow, k eventKey) int {
	return slices.IndexFunc(rows, func(r eventRow) bool {
		return rowHolds(r, func(e *pipeline.SessionEvent) bool { return keyOf(e) == k })
	})
}

// rowHolds reports whether either half of row r satisfies match.
func rowHolds(r eventRow, match func(*pipeline.SessionEvent) bool) bool {
	return (r.event != nil && match(r.event)) || (r.resp != nil && match(r.resp))
}
```

`eventsSurface.lineOf` in `search_surfaces.go` calls `findByKey`, so it needs no change.

In `cmd/agentop/tui/timeline_pane.go`, in `revealPendingSeq`, replace the final loop:

```go
	// Either half: the Timeline names a tool by the response that called it (#1681).
	for i, r := range m.visibleRows {
		if rowHolds(r, func(e *pipeline.SessionEvent) bool {
			return e.Seq == m.revealSeq && eventSession(m.selectedSess, e) == m.selectedSess
		}) {
			setCursorVisible(&m.eventsTbl, i)
			m.selectedEventKey = keyOf(r.event)
			m.revealSeq, m.revealSess = 0, ""
			return true
		}
	}
	return false
```

Add the `pipeline` import to `timeline_pane.go` if it is not there.

- [ ] **Step 4: Run every check and commit**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ && GOWORK=off go vet ./... && gofmt -l tui`
Expected: `ok`, no other output.

```bash
git add cmd/agentop/tui
git commit -s -F - <<'EOF'
Fix: Find a merged events row by its response too

The cursor pin and Enter in the Timeline found a row by one event. A
merged row holds two, and the Timeline names a tool by its response.
Both now match either half.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 5: Detail tree sections

The detail tree learns to hold two sections. This task adds the tree code and its rules, with unit tests on the rendered lines. Task 6 puts it in the pane.

**Files:**
- Modify: `cmd/agentop/tui/detail_tree.go` (node kinds, `node`, `foldDefaults`, `isOpen`, `value`, new `sectionTree`, `sectionsRoot`, `sectionOf`)
- Modify: `cmd/agentop/tui/detail_pane.go` (new `sharedWithRequest`, `dropShared`)
- Create: `cmd/agentop/tui/detail_sections_test.go`

**Interfaces:**
- Produces:
  - `sectionTree(path, title string, heads []string, data []byte, note string) (*node, error)` — `path` is `"request"` or `"response"`. `data == nil` makes a section that shows `note`.
  - `sectionsRoot(secs ...*node) *node` — the root `renderTree` takes for a detail page.
  - `sectionOf(path string) string` — `"response"` for a path in the response section, else `"request"`.
  - `dropShared(resp, req []byte) []byte`
- Task 6 uses all of these.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agentop/tui/detail_sections_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func sectionLines(t *testing.T, sec *node, fs foldState) []treeLine {
	t.Helper()
	return renderTree(sectionsRoot(sec), fs)
}

func TestSectionTree_TitleHeadsAndPaths(t *testing.T) {
	sec, err := sectionTree("response", "━━ RESPONSE · 14:00:00.00", []string{"served:      m"},
		[]byte(`{"host":"h","inference":{"model":"m"}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	ls := sectionLines(t, sec, foldState{})
	if got := stripANSI(ls[0].text); !strings.Contains(got, "━━ RESPONSE") || ls[0].path != "response" || !ls[0].head {
		t.Errorf("line 0 = %q path %q head %v, want the section title as a fold head", got, ls[0].path, ls[0].head)
	}
	if got := stripANSI(ls[1].text); !strings.Contains(got, "served:") {
		t.Errorf("line 1 = %q, want the section's header line", got)
	}
	for _, l := range ls {
		if l.path != "response" && !strings.HasPrefix(l.path, "response.") {
			t.Errorf("line %q has path %q, want it under response", stripANSI(l.text), l.path)
		}
	}
}

func TestSectionTree_FoldsAsOne(t *testing.T) {
	sec, _ := sectionTree("request", "━━ REQUEST", nil, []byte(`{"host":"h"}`), "")
	ls := sectionLines(t, sec, foldState{open: map[string]foldChoice{"request": {open: false}}})
	if len(ls) != 1 {
		t.Errorf("%d lines, want only the closed title", len(ls))
	}
}

func TestSectionTree_MissingHalfShowsNote(t *testing.T) {
	sec, _ := sectionTree("request", "━━ REQUEST", nil, nil, "request not loaded: press o in events")
	ls := sectionLines(t, sec, foldState{})
	if len(ls) != 2 || !strings.Contains(stripANSI(ls[1].text), "request not loaded") {
		t.Errorf("lines = %v, want the title and the note", lineTexts(ls))
	}
}

// The section title names the plugins that acted, so the full list starts closed.
func TestSectionTree_InvocationsStartFolded(t *testing.T) {
	sec, _ := sectionTree("request", "━━ REQUEST", nil,
		[]byte(`{"invocations":{"outbound":[{"plugin":"tool-prune","action":"skip"}]}}`), "")
	text := stripANSI(strings.Join(lineTexts(sectionLines(t, sec, foldState{})), "\n"))
	if !strings.Contains(text, `▸ "invocations"`) || strings.Contains(text, "tool-prune") {
		t.Errorf("want invocations closed:\n%s", text)
	}
	// A search with a match inside opens it, as for any fold.
	text = stripANSI(strings.Join(lineTexts(sectionLines(t, sec, foldState{query: "tool-prune"})), "\n"))
	if !strings.Contains(text, "tool-prune") {
		t.Errorf("a search for a skipped plugin did not open invocations:\n%s", text)
	}
}

// The raw capture holds a second copy of the conversation. A query never opens it, under a
// section path as at the root.
func TestSectionTree_QueryDoesNotOpenRaw(t *testing.T) {
	sec, _ := sectionTree("request", "━━ REQUEST", nil,
		[]byte(`{"host":"needle","raw":{"body":"needle"}}`), "")
	text := stripANSI(strings.Join(lineTexts(sectionLines(t, sec, foldState{query: "needle"})), "\n"))
	if strings.Contains(text, `"body"`) {
		t.Errorf("the query opened raw:\n%s", text)
	}
}

func TestSectionOf(t *testing.T) {
	for path, want := range map[string]string{
		"response": "response", "response.host": "response", "response.inference.messages[0]": "response",
		"request": "request", "request.host": "request", "": "request",
	} {
		if got := sectionOf(path); got != want {
			t.Errorf("sectionOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDropShared(t *testing.T) {
	req := []byte(`{"host":"a","httpMethod":"POST","at":"1","sessionId":"s"}`)
	resp := []byte(`{"host":"b","httpMethod":"POST","at":"2","sessionId":"s","statusCode":200}`)
	if got, want := string(dropShared(resp, req)), `{"at":"2","host":"b","statusCode":200}`; got != want {
		t.Errorf("dropShared = %s, want %s: equal fields go, a different host and at stay", got, want)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run 'TestSectionTree_|TestSectionOf|TestDropShared' 2>&1 | tail -5`
Expected: compile error, `undefined: sectionTree`.

- [ ] **Step 3: Add the section kinds and builder**

In `cmd/agentop/tui/detail_tree.go`, extend the kinds:

```go
	kindGroup    // the messages before this turn's, under one head
	kindSection  // one half of an exchange on the detail page (#1681): a title, header lines, the event
	kindSections // the detail page's root: its sections, one after another
```

Add two fields to `node`, after `label`:

```go
	heads       []string // a section's header lines, drawn under its title
	noQueryOpen bool     // a search never opens this fold: the raw capture
```

Add after `treeFromJSON`:

```go
// sectionTree builds one half of the detail page (#1681). path is "request" or "response",
// and it prefixes every node's path, so the halves' folds and the cursor's path never meet.
// heads are the half's header lines (TLS, timing, …), drawn under the title. With data nil
// the half is missing, and the section shows note instead of a tree.
func sectionTree(path, title string, heads []string, data []byte, note string) (*node, error) {
	s := &node{kind: kindSection, path: path, fold: true, defOpen: true, label: title, heads: heads}
	if data == nil {
		s.heads = append(s.heads, note)
	} else {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		ev := buildTree(v, path, "", false)
		ev.fold = false // the section folds; the event's object does not fold a second time
		foldDefaults(ev)
		s.kids = []*node{ev}
	}
	// A search for a header's text must find it in a closed section too.
	s.plain = strings.Join(s.heads, "\n")
	return s, nil
}

// sectionsRoot is the root renderTree draws for a detail page: the sections in order.
func sectionsRoot(secs ...*node) *node {
	return &node{kind: kindSections, kids: secs}
}

// sectionOf names the section a tree path belongs to. Every path in the response section
// starts with "response"; everything else — the request's paths, and "" — is the request's.
func sectionOf(path string) string {
	if path == "response" || strings.HasPrefix(path, "response.") || strings.HasPrefix(path, "response[") {
		return "response"
	}
	return "request"
}
```

In `foldDefaults`, replace the raw block, and add the invocations fold after it:

```go
	// The exact capture: large, and opened when it is the point. A search does not open it
	// either: see isOpen.
	if raw := root.child("raw"); raw != nil && raw.fold {
		raw.defOpen, raw.noQueryOpen = false, true
	}
	// Every plugin's invocation, skips included, was half of a short request (#1681). The
	// section title names the plugins that acted, so the full list starts closed.
	if inv := root.child("invocations"); inv != nil && inv.fold {
		inv.defOpen = false
	}
```

In `isOpen`, replace `n.path != "raw"` with `!n.noQueryOpen`, and change the comment above that line to start: "Not the raw capture (noQueryOpen): it holds a second copy …".

In `renderer.value`, add two cases before `default:`:

```go
	case kindSections:
		for _, k := range n.kids {
			r.value(k, 0, false)
		}
	case kindSection:
		open := r.isOpen(n)
		marker := "▸ "
		if open {
			marker = "▾ "
		}
		if r.fs.all {
			marker = ""
		}
		r.emit(styleJSONPunct.Render(marker)+styleTitle.Render(n.label), n, true, open)
		if open {
			for _, h := range n.heads {
				for _, l := range strings.Split(h, "\n") {
					r.emit("  "+l, n, false, false)
				}
			}
			for _, k := range n.kids {
				r.value(k, 0, false)
			}
		}
```

- [ ] **Step 4: Add `dropShared`**

In `cmd/agentop/tui/detail_pane.go`, after `filterForDetail`, add:

```go
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
```

Add `"bytes"` to the imports of `detail_pane.go` if it is not there.

- [ ] **Step 5: Run the tests**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ 2>&1 | grep -E '^(--- FAIL|FAIL|ok)' | head -20`
Expected: the new tests pass. A test in `detail_raw_test.go`, `detail_folds_test.go` or `detail_tree_test.go` that expected `invocations` open now sees it closed. Rewrite such a test to open it first (Enter on its line, or a `foldState.open` entry), or to assert the closed line. Keep every other assertion.

- [ ] **Step 6: Run every check and commit**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ && GOWORK=off go vet ./... && gofmt -l tui`
Expected: `ok`, no other output.

```bash
git add cmd/agentop/tui
git commit -s -F - <<'EOF'
Feat: Add request and response sections to the detail tree

The detail tree can now hold two sections, each with a title, header
lines and its event. A section folds with Enter like any node. A
missing half shows a note. invocations starts folded, because the
section title will name the plugins that acted. dropShared leaves out
response fields that repeat the request's.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 6: The detail page shows both halves

Enter on a row opens one full-width page: a fixed line, then a REQUEST section and a RESPONSE section. `Tab` jumps between them, and `y` writes the half the cursor is in.

**Files:**
- Modify: `cmd/agentop/tui/detail_pane.go` (`showDetail`, new `detailHalves`, `detailSection`, `sectionTitle`, `actedPlugins`, `missingNote`, `exchangeSummary`, `detailFixedLine`, `jumpDetailSection`, `detailHalf`, `shownNum`)
- Modify: `cmd/agentop/tui/detail_fetch.go` (`applyDetailEvent`, `detailIsProjected`, new `fetchDetailHalves`)
- Modify: `cmd/agentop/tui/keys.go` (Enter on events, `tab`, `y`, the detail footer)
- Modify: `cmd/agentop/tui/app.go` (model fields, the paneDetail body in `View`)
- Modify: `cmd/agentop/tui/help_overlay.go` (paneDetail entry)
- Create: `cmd/agentop/tui/detail_exchange_test.go`

**Interfaces:**
- Consumes: `sectionTree`, `sectionsRoot`, `sectionOf`, `dropShared` (Task 5). `answer`, `exchangeBytes`, `exchangeTokensCell`, `exchangeCostCell`, `topInvocation` (Task 2). `lostRef`, `rowLost` (Task 3). `httpExchange`, `t0` (Task 2 tests).
- Produces: `model.detailNum string`, `model.detailSummary string`, `(m *model) detailHalf() *pipeline.SessionEvent`, `(m *model) fetchDetailHalves(r eventRow) tea.Cmd`, `detailHalves(r eventRow) (req, resp *pipeline.SessionEvent, reqRaw, respRaw *pipeline.RawCapture)`. Task 7 uses `detailHalves` and `showDetail`.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agentop/tui/detail_exchange_test.go`:

```go
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/pipeline"
)

// turnExchange is turnEvent's request with a response that has enough lines to fill a short
// viewport, so the RESPONSE title can scroll to the top.
func turnExchange(at time.Time) (req, resp pipeline.SessionEvent) {
	req = turnEvent()
	req.At, req.SessionID, req.RequestID = at, "s", "x"
	resp = pipeline.SessionEvent{
		Seq: 8, At: at.Add(2 * time.Second), SessionID: "s", Direction: pipeline.Outbound,
		Phase: pipeline.SessionResponse, Host: "h", RequestID: "x", StatusCode: 200,
		Duration: 2 * time.Second,
		Inference: &pipeline.InferenceExtension{Model: "m", Completion: "the-answer", InputTokens: 70_600, OutputTokens: 9},
		Plugins:   map[string]json.RawMessage{},
	}
	for i := range 12 {
		resp.Plugins[fmt.Sprintf("p%02d", i)] = json.RawMessage("1")
	}
	return req, resp
}

// openDetail opens the detail of the events pane's first row, with the clock at now, on a
// terminal short enough that the page scrolls.
func openDetail(t *testing.T, now time.Time, events ...pipeline.SessionEvent) *model {
	t.Helper()
	m := newSearchEventsModel(t)
	m.height = 16
	m.layout()
	m.now = func() time.Time { return now }
	m.events["s"] = events
	m.rebuildEventsTable()
	setCursorVisible(&m.eventsTbl, 0)
	m.selectedEventKey = keyOf(m.selectedEvent())
	press(m, tea.KeyEnter)
	if m.pane != paneDetail {
		t.Fatalf("setup: Enter left pane %v, want detail", m.pane)
	}
	return m
}

func exchangeDetail(t *testing.T) *model {
	t.Helper()
	at := time.Now().Add(-time.Minute)
	req, resp := turnExchange(at)
	return openDetail(t, at.Add(time.Minute), req, resp)
}

func detailText(m *model) string {
	var b strings.Builder
	for _, l := range m.detailLines {
		b.WriteString(stripANSI(l.text) + "\n")
	}
	return b.String()
}

func cursorPath(m *model) string { return m.detailLines[m.detailDoc.cursor].path }

func TestDetail_TwoSectionsInOrder(t *testing.T) {
	text := detailText(exchangeDetail(t))
	r, s := strings.Index(text, "━━ REQUEST"), strings.Index(text, "━━ RESPONSE")
	if r < 0 || s < 0 || r > s {
		t.Fatalf("want REQUEST then RESPONSE titles:\n%s", text)
	}
	if !strings.Contains(text, "the-answer") {
		t.Errorf("the response's completion is missing:\n%s", text)
	}
}

func TestDetail_ResponseLeavesOutSharedFields(t *testing.T) {
	m := exchangeDetail(t)
	var reqHost, respHost bool
	for _, l := range m.detailLines {
		reqHost = reqHost || l.path == "request.host"
		respHost = respHost || l.path == "response.host"
	}
	if !reqHost || respHost {
		t.Errorf("request.host shown %v, response.host shown %v: want the host once, in the request", reqHost, respHost)
	}
}

func TestDetail_FixedLineFollowsTheScroll(t *testing.T) {
	m := exchangeDetail(t)
	if m.detailVp.Height != m.bodyHeight-1 {
		t.Errorf("viewport height %d, want %d: the fixed line takes one row", m.detailVp.Height, m.bodyHeight-1)
	}
	line := stripANSI(m.detailFixedLine())
	if !strings.HasPrefix(line, "REQUEST │") || !strings.Contains(line, "#1") || !strings.Contains(line, "200") {
		t.Errorf("fixed line = %q, want REQUEST, the row's # and its status", line)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if line := stripANSI(m.detailFixedLine()); !strings.HasPrefix(line, "RESPONSE │") {
		t.Errorf("after Tab: fixed line = %q, want RESPONSE", line)
	}
}

func TestDetail_TabJumpsBetweenTitles(t *testing.T) {
	m := exchangeDetail(t)
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if l := m.detailLines[m.detailDoc.cursor]; l.path != "response" || !l.head {
		t.Errorf("after Tab the cursor is on %q, want the RESPONSE title", stripANSI(l.text))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if l := m.detailLines[m.detailDoc.cursor]; l.path != "request" || !l.head {
		t.Errorf("after a second Tab the cursor is on %q, want the REQUEST title", stripANSI(l.text))
	}
}

func TestDetail_EnterFoldsSection(t *testing.T) {
	m := exchangeDetail(t)
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	press(m, tea.KeyEnter)
	for _, l := range m.detailLines {
		if strings.HasPrefix(l.path, "response.") {
			t.Fatalf("line %q still shows: the RESPONSE section did not fold", stripANSI(l.text))
		}
	}
}

func TestDetail_SearchCrossesSections(t *testing.T) {
	m := exchangeDetail(t)
	typeKeys(m, "/the-answer")
	press(m, tea.KeyEnter)
	m.Update(keyRune('n'))
	if got := sectionOf(cursorPath(m)); got != "response" {
		t.Errorf("the search left the cursor in %s, want the match in the response", got)
	}
}

func TestDetail_MissingHalfNotes(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	req, resp := turnExchange(at)
	denied := req
	denied.Phase = pipeline.SessionDenied
	denied.Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "guard", Action: pipeline.ActionDeny}}}
	tunnel := pipeline.SessionEvent{
		At: at, SessionID: "s", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Tunnel: true, TunnelReason: "passthrough-host", Host: "api.test:443", RequestID: "t",
	}
	for name, tc := range map[string]struct {
		now    time.Time
		events []pipeline.SessionEvent
		want   string
	}{
		"in flight":     {at.Add(time.Minute), []pipeline.SessionEvent{req}, "waiting for response…"},
		"lost":          {at.Add(20 * time.Minute), []pipeline.SessionEvent{req}, "no response recorded"},
		"denied":        {at.Add(time.Minute), []pipeline.SessionEvent{denied}, "denied by guard: no upstream call"},
		"response only": {at.Add(time.Minute), []pipeline.SessionEvent{resp}, "request not loaded: press o in events"},
		"open tunnel":   {at.Add(6 * time.Hour), []pipeline.SessionEvent{tunnel}, "no close recorded yet"},
	} {
		if text := detailText(openDetail(t, tc.now, tc.events...)); !strings.Contains(text, tc.want) {
			t.Errorf("%s: want %q in:\n%s", name, tc.want, text)
		}
	}
}

// yanked reads the file the footer names.
func yanked(t *testing.T, m *model) string {
	t.Helper()
	path, ok := strings.CutPrefix(m.flash, "yanked → ")
	if !ok {
		t.Fatalf("flash = %q, want a yank", m.flash)
	}
	path, _, _ = strings.Cut(path, "  (")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDetail_YankWritesCursorHalf(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := exchangeDetail(t)
	m.Update(keyRune('y'))
	if got := yanked(t, m); !strings.Contains(got, `"phase": "request"`) {
		t.Errorf("y in REQUEST wrote:\n%s", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(keyRune('y'))
	if got := yanked(t, m); !strings.Contains(got, `"phase": "response"`) || !strings.Contains(got, `"host": "h"`) {
		t.Errorf("y in RESPONSE wrote:\n%s\nwant the full response event, host included", got)
	}
}

// Review focus: the cursor sits in a missing half.
func TestDetail_YankMissingHalfWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	at := time.Now().Add(-time.Minute)
	req, _ := turnExchange(at)
	m := openDetail(t, at.Add(time.Minute), req)
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(keyRune('y'))
	if !strings.Contains(m.flash, "nothing to yank") {
		t.Errorf("flash = %q, want nothing to yank", m.flash)
	}
	if files, _ := filepath.Glob(filepath.Join(home, ".cortex", "agentop-events", "*")); len(files) > 0 {
		t.Errorf("y wrote %v for a missing half", files)
	}
}

// Review focus: a row whose request is not held. The page opens on the REQUEST note, Tab
// goes to the response, and a second Tab comes back to the note.
func TestDetail_TabWithOneHalf(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	_, resp := turnExchange(at)
	m := openDetail(t, at.Add(time.Minute), resp)
	for _, want := range []string{"response", "request"} {
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
		if l := m.detailLines[m.detailDoc.cursor]; l.path != want || !l.head {
			t.Errorf("Tab put the cursor on %q, want the %s title", stripANSI(l.text), want)
		}
	}
}

// Review focus: the fixed line never wraps.
func TestDetail_FixedLineFitsNarrowTerminal(t *testing.T) {
	m := exchangeDetail(t)
	m.width = 60
	m.layout()
	if w := lipgloss.Width(m.detailFixedLine()); w > 60 {
		t.Errorf("fixed line is %d wide on a 60-column terminal", w)
	}
}

func TestDetail_FetchReplyForEitherHalf(t *testing.T) {
	m := exchangeDetail(t)
	full := *m.detailRow.resp
	full.Inference = &pipeline.InferenceExtension{Model: "m", Completion: "full-body"}
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s", seq: 99, event: &full})
	if strings.Contains(detailText(m), "full-body") {
		t.Fatal("a reply for another seq was applied")
	}
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s", seq: 8, event: &full})
	if !strings.Contains(detailText(m), "full-body") {
		t.Errorf("the response's fetched body did not reach the page:\n%s", detailText(m))
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run 'TestDetail_' 2>&1 | tail -10`
Expected: compile error, `m.detailFixedLine undefined`.

- [ ] **Step 3: Add the model fields**

In `cmd/agentop/tui/app.go`, after `detailRow eventRow`, add:

```go
	// detailNum is the # the row showed when Enter opened it, and detailSummary the
	// exchange's figures. The detail's fixed line quotes both (#1681).
	detailNum     string
	detailSummary string
```

Change the comment above `detailTree` so that it says the tree holds the page's two sections (see `sectionsRoot`), and `detailHead` is only for a line above them.

- [ ] **Step 4: Build the page in `showDetail`**

In `cmd/agentop/tui/detail_pane.go`, replace `showDetail` and its comment with:

```go
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
	m.detailHead, m.detailTree = nil, nil
	// The fixed line above the viewport takes one row of the body.
	if m.bodyHeight > 1 {
		m.detailVp.Height = m.bodyHeight - 1
	}
	req, resp, reqRaw, respRaw := detailHalves(r)
	lost := rowLost(r, nil, 0, m.lostRef(m.events[m.selectedSess]))
	m.detailSummary = exchangeSummary(r, m.detailNum, lost)
	m.detailTree = sectionsRoot(
		detailSection("request", "REQUEST", req, nil, reqRaw, r.tunnel, missingNote(r, "request", lost)),
		detailSection("response", "RESPONSE", resp, req, respRaw, nil, missingNote(r, "response", lost)),
	)
	if resetScroll {
		// A new exchange: the folds the reader opened were for the last one.
		m.detailFolds = nil
		m.detailCursorPath, m.detailCursorOff = "", 0
	}
	m.renderDetail()
	if resetScroll {
		m.detailVp.GotoTop()
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

// detailHalves splits row r into its request and response and their captures. Either half can
// be nil: a response-only row has no request held, and a request has no response until one
// arrives. A denial is the request half.
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
		return "request not loaded: press o in events"
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
func (m *model) shownNum() string {
	cols, _ := layoutColumns(m.eventColumns, m.eventColWidths, m.width)
	rows, cur := m.eventsTbl.Rows(), m.eventsTbl.Cursor()
	for i, c := range cols {
		if c.id == colIndex && cur >= 0 && cur < len(rows) && i < len(rows[cur]) {
			return strings.TrimSpace(rows[cur][i])
		}
	}
	return ""
}
```

Add `"slices"` to the imports of `detail_pane.go` if it is not there.

- [ ] **Step 5: Fetch both halves, and accept a reply for either**

In `cmd/agentop/tui/detail_fetch.go`, add:

```go
// fetchDetailHalves fetches in full each half of r that the summary left bodies out of
// (#1681). Nil when there is nothing to fetch.
func (m *model) fetchDetailHalves(r eventRow) tea.Cmd {
	if m.client == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, e := range []*pipeline.SessionEvent{r.event, r.resp} {
		if e != nil && m.needsFullEvent(e) {
			cmds = append(cmds, fetchDetailEventCmd(m, eventSession(m.selectedSess, e), e.Seq))
		}
	}
	return tea.Batch(cmds...)
}
```

In `applyDetailEvent`, replace the guard and the write-back. The function becomes:

```go
func (m *model) applyDetailEvent(msg detailEventLoadedMsg) {
	if m.pane != paneDetail || m.detailEvent == nil {
		return
	}
	// The reply is for one half of the exchange on screen, or for a row the reader left.
	held := func(e *pipeline.SessionEvent) bool {
		return e != nil && eventSession(m.selectedSess, e) == msg.sessionID && e.Seq == msg.seq
	}
	var slot **pipeline.SessionEvent
	var rawSlot **pipeline.RawCapture
	switch {
	case held(m.detailRow.event):
		slot, rawSlot = &m.detailRow.event, &m.detailRow.raw
	case held(m.detailRow.resp):
		slot, rawSlot = &m.detailRow.resp, &m.detailRow.respRaw
	default:
		return
	}
	if msg.err != nil {
		m.setFlash("could not load the full event: " + msg.err.Error())
		return
	}
	m.replaceHeldEvent(m.selectedSess, msg.event)
	m.markFullFetched(msg.sessionID, msg.seq, msg.raw)
	m.rebuildSessionsTable()

	// Re-render through showDetail so the headers, wrapping and scroll position are all
	// rebuilt exactly as the first render built them.
	*slot, *rawSlot = msg.event, msg.raw
	m.showDetail(m.detailRow, false)
}
```

Keep the comments the old function had above its `setFlash`, `replaceHeldEvent` and `rebuildSessionsTable` calls: move each one above the same call in the new body.

Change `detailIsProjected` to judge the half `y` writes:

```go
func (m *model) detailIsProjected() bool {
	return m.needsFullEvent(m.detailHalf())
}
```

and add one sentence to its comment: "It judges the half the cursor is in, because that is the event y writes (#1681)."

- [ ] **Step 6: Wire the keys and the footer**

In `cmd/agentop/tui/keys.go`, in the `paneEvents` arm of Enter, replace the lines from `er.raw, _ = m.heldRaw(er.event)` to the end of the arm with:

```go
			// Render from the summary immediately, then fill the message bodies in
			// when they arrive — the timeline no longer carries them. See
			// fetchDetailEventCmd for why this is not a blocking spinner.
			er.raw, _ = m.heldRaw(er.event)
			if er.resp != nil {
				er.respRaw, _ = m.heldRaw(er.resp)
			}
			m.detailNum = m.shownNum()
			m.showDetail(er, true)
			m.pane = paneDetail
			return m.fetchDetailHalves(er)
```

Before `case "y":`, add:

```go
	case "tab":
		// The detail page's two sections (#1681): Tab is bound nowhere else, and ← already
		// means back.
		if m.pane == paneDetail {
			m.jumpDetailSection()
		}
		return nil
```

In `case "y":`, replace `path, err := yankEventToFile(m.detailEvent)` with:

```go
		half := m.detailHalf()
		if half == nil {
			m.setFlash("nothing to yank: this half was not recorded")
			return nil
		}
		path, err := yankEventToFile(half)
```

Replace the `paneDetail` footer line with:

```go
	case paneDetail:
		other, half := "response", "request"
		if sectionOf(m.detailCursorPath) == "response" {
			other, half = "request", "response"
		}
		return "[↑↓] move  [↵] fold  [tab] " + other + "  [y] yank " + half + "  [u] usage  " +
			m.searchHints(m.pane) + "  [esc] back  [?] keys  [q] quit"
```

In `cmd/agentop/tui/app.go`, in `View`, replace the paneDetail body:

```go
	case paneDetail:
		title = m.sessionHeader(m.selectedSess, "event")
		body = m.detailFixedLine() + "\n" + m.detailVp.View()
```

In `cmd/agentop/tui/help_overlay.go`, replace the `paneDetail` entry's `purpose` and `bindings`:

```go
		purpose: "one exchange in full: its request, then its response",
		bindings: []keyBinding{
			{"↑↓", "move the cursor"},
			{"↵", "open or close the fold at the cursor"},
			{"tab", "jump to the other half: request or response"},
			{"y", "yank the half the cursor is in to ~/.cortex/agentop-events"},
			{"esc / ← / h", "back to events"},
		},
```

- [ ] **Step 7: Run the tests**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run 'TestDetail_' -v 2>&1 | grep -E '^(---|ok|FAIL)'`
Expected: every `TestDetail_` test in the new file passes.

Then run the whole suite: `cd cmd/agentop && GOWORK=off go test ./tui/ 2>&1 | grep -E '^(--- FAIL|FAIL|ok)' | head -40`.
Existing detail tests open one event and read its lines. They now see a section title first, and paths under `request.` or `response.`. Migrate each failing test by these rules, and keep what it asserts:

| The test reads | Do this |
|---|---|
| a tree path such as `"raw"` or `"inference.messages[<1]"` | Prefix the half: `"request.raw"`, `"response.inference…"`. A response-only fixture's paths are under `response.`. |
| a line index or a line count | Add the section title and the note of the missing half, or find the line by text with `cursorTo`. |
| `m.detailVp.Height == bodyHeight` | It is `bodyHeight-1` for the event detail. The plugin detail pane keeps `bodyHeight`. |
| a footer or help text that names `[y] yank` | Expect `[tab] response  [y] yank request`. Check the footer-fit tests still fit at their widths. |
| the layout of the detail pane fitting the terminal | Keep the assertion. The fixed line plus the viewport must fit. |

- [ ] **Step 8: Run every check and commit**

Run: `cd cmd/agentop && GOWORK=off go test ./... && GOWORK=off go vet ./... && gofmt -l tui`
Expected: all `ok` (the known `TestSetupRollbackWaitsOutLaunchdTeardown` timeout excepted), no other output.

```bash
git add cmd/agentop/tui
git commit -s -F - <<'EOF'
Feat: Show an exchange's request and response on one detail page

Enter on an events row now opens one full-width page with a REQUEST
section and a RESPONSE section. A fixed line above it names the
section on screen and gives the exchange's figures. Tab jumps between
the sections. y writes the half the cursor is in, in the same file
format as before. A missing half says why it is missing.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 7: A response fills the open detail

While the detail pane is open, the events table does not rebuild. A response that streams in must still reach the RESPONSE section of the exchange on screen.

**Files:**
- Modify: `cmd/agentop/tui/detail_fetch.go` (new `fillDetailResponse`)
- Modify: `cmd/agentop/tui/app.go` (the `streamMsg` arm of `Update`)
- Test: `cmd/agentop/tui/detail_exchange_test.go`

**Interfaces:**
- Consumes: `showDetail`, `detailRow`, `heldRaw`, `needsFullEvent`, `fetchDetailEventCmd`, `turnExchange`, `openDetail`, `detailText`, `cursorPath` (Task 6).
- Produces: `(m *model) fillDetailResponse(e *pipeline.SessionEvent) tea.Cmd`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/agentop/tui/detail_exchange_test.go`:

```go
func TestDetail_ResponseFillsOpenDetail(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	req, resp := turnExchange(at)
	m := openDetail(t, at.Add(time.Minute), req)
	if !strings.Contains(detailText(m), "waiting for response…") {
		t.Fatalf("setup: want the waiting note:\n%s", detailText(m))
	}
	m.Update(keyRune('j'))
	m.Update(keyRune('j'))
	before := cursorPath(m)

	m.Update(streamMsg(apiclient.StreamEvent{Event: &resp}))
	if !strings.Contains(detailText(m), "the-answer") {
		t.Errorf("the response did not fill the page:\n%s", detailText(m))
	}
	if got := cursorPath(m); got != before {
		t.Errorf("cursor moved from %q to %q: the fill must keep the reader's place", before, got)
	}
}
```

Add the import `"github.com/rossoctl/cortex/cmd/agentop/apiclient"` to the file.

- [ ] **Step 2: Run it to see it fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run TestDetail_ResponseFillsOpenDetail 2>&1 | tail -5`
Expected: FAIL, `the response did not fill the page`.

- [ ] **Step 3: Implement**

In `cmd/agentop/tui/detail_fetch.go`, add:

```go
// fillDetailResponse puts a response that just streamed in into the open detail of the
// request it answers (#1681), and fetches its bodies. The events table does not rebuild while
// the detail is open, so without this the page would say "waiting for response…" until the
// reader left it. showDetail keeps the reader's folds, cursor and scroll.
func (m *model) fillDetailResponse(e *pipeline.SessionEvent) tea.Cmd {
	r := m.detailRow
	req, resp, _, _ := detailHalves(r)
	if m.pane != paneDetail || e == nil || req == nil || resp != nil ||
		req.Phase != pipeline.SessionRequest || e.Phase != pipeline.SessionResponse ||
		e.RequestID == "" || e.RequestID != req.RequestID ||
		eventSession(m.selectedSess, e) != eventSession(m.selectedSess, req) {
		return nil
	}
	got := *e
	r.resp = &got
	r.respRaw, _ = m.heldRaw(r.resp)
	m.showDetail(r, false)
	if m.client != nil && m.needsFullEvent(r.resp) {
		return fetchDetailEventCmd(m, eventSession(m.selectedSess, r.resp), r.resp.Seq)
	}
	return nil
}
```

In `cmd/agentop/tui/app.go`, in the `streamMsg` arm, replace:

```go
		m.handleStreamEvent(ev)
		// Re-pump the same channel for the next message. A single apiclient
		// goroutine fills the channel for the duration of ctx.
		return m, streamPump(m.streamCh)
```

with:

```go
		m.handleStreamEvent(ev)
		// Re-pump the same channel for the next message. A single apiclient
		// goroutine fills the channel for the duration of ctx. A response for the
		// exchange open in the detail pane fills it (#1681).
		return m, tea.Batch(streamPump(m.streamCh), m.fillDetailResponse(ev.Event))
```

- [ ] **Step 4: Run every check and commit**

Run: `cd cmd/agentop && GOWORK=off go test ./tui/ && GOWORK=off go vet ./... && gofmt -l tui`
Expected: `ok`, no other output.

```bash
git add cmd/agentop/tui
git commit -s -F - <<'EOF'
Feat: Fill the open detail when its response arrives

The events table does not rebuild while the detail pane is open, so a
response that arrived left the page saying it was waiting. The stream
now fills the RESPONSE section in place and keeps the reader's place.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

### Task 8: README, and the final checks

**Files:**
- Modify: `cmd/agentop/README.md` (the Events and Detail bullets of "Panes", the keys table, the column-id table)

- [ ] **Step 1: Update the Events bullet**

In `cmd/agentop/README.md`, in the **Events** bullet:

1. Replace `since thirteen abbreviated` with `since twelve abbreviated`.
2. Replace the sentence ``Sorting never changes the `#` exchange pairing or the per-row token and cost figures; it reorders the finished rows only.`` with ``Sorting never changes the `#` numbers or a row's figures; it reorders the finished rows only.``
3. Replace `The thirteen default columns together need ~185 terminal columns` with `The twelve default columns together need about 180 terminal columns`.
4. Replace the paragraph that starts `All thirteen are on by default:` up to `touching the picker.` with:

```markdown
  All twelve are on by default: `#` (exchange number), TIME, DIR, ACTION, PLUGIN,
  METHOD, STATUS, DURATION, BYTES, TOKENS, COST, HOST. On a narrow terminal the
  low-ranked ones are hidden rather than turned off, so widening the window brings
  them back without touching the picker.
```

5. In the BYTES paragraph, replace `— a request row the body size it forwarded, a response row the body size that came back —` with `— the body size the request forwarded (↑) and the body size that came back (↓) —`.
6. Replace the paragraph that starts `All thirteen columns, on a terminal wide enough` and the code block after it with:

````markdown
  All twelve columns, on a terminal wide enough for them. Each row is one
  exchange: a request and its response. A blank STATUS means the response is on
  its way. `lost` means none came for 10 minutes while the session went on, the
  rule the Timeline uses. A request the proxy did not stamp with a request id
  (an older proxy) keeps two rows that share one `#`:

  ```
  agentop · ctx-abc-1234…

   #     TIME          DIR   ACTION    PLUGIN              METHOD              STATUS   DURATION              BYTES               TOKENS                COST   HOST
   1     14:23:07.41   in    allow     jwt-validation                             200      118ms       ↑1.4kB ↓842B                                              weather-agent
   2     14:23:07.71   out   observe   inference-parser    claude-sonnet-5        200      1.20s   ↑118.6kB ↓12.4kB   681.3k(−9.9k)→412   $0.2767(−$0.0037)   api.anthropic.com
   3     14:23:09.01   out   modify    token-exchange      tools/call             503       96ms              ↑318B                                              github-tool-mcp
   4     14:23:09.40   out   observe   inference-parser    claude-sonnet-5                                ↑121.0kB                                              api.anthropic.com

  ● connected   2.1 events/sec   [/anthropic 2 matches]   [sort: DURATION▼]
  [↑↓] nav  [b/f] page  [↵] detail  [c] columns  [u] usage  [s] hide passthru/skip  [p] pause  [/] search  [n/N] next/prev  [esc] back  ·  [?] keys  [q] quit
  ```
````

7. Replace `Exchange 3's response has no BYTES figure because there was nothing to count:` with `Exchange 3 has no ↓ figure because there was nothing to count:`, and add one sentence at the end of that paragraph: `Exchange 4 is still on its way.`
8. In the paragraph about `—` and `tunnel`, replace ``The tunnel's STATUS arrives on its `resp` row when it closes,`` with `A tunnel's row gets its STATUS when the tunnel closes,`, and replace ``The TOKENS and COST figures on a request row carry what `tool-prune` saved in parentheses`` with ``TOKENS reads prompt→output. TOKENS and COST carry what `tool-prune` saved in parentheses``.

- [ ] **Step 2: Update the Detail bullet**

Replace the start of the **Detail** bullet, `- **Detail**: pretty-printed JSON of a single event. Scroll with arrow keys;`, with:

```markdown
- **Detail**: one exchange in full, at the full width of the terminal: a
  REQUEST section, then a RESPONSE section, each the event as the proxy recorded
  it, under a title that names its time and the plugins that acted on it. The
  line above the JSON names the section at the top of the screen, then the
  exchange's figures. `Tab` moves to the other section's title, and `Enter` folds
  the section or node at the cursor. The response leaves out the fields equal to
  the request's (client, direction, host, method, path, ids), and `invocations`
  starts folded. A half that is missing says why: on its way, lost, denied, or
  not loaded yet. Move with the arrow keys;
```

In the same bullet, replace `` `y` yanks to `` with `` `y` yanks the section the cursor is in to ``.

- [ ] **Step 3: Update the keys table and the column table**

Replace the `y` row for `detail` with two rows:

```markdown
| `y` | detail | yank the event of the section the cursor is in (request or response) to `~/.cortex/agentop-events` (path stays until the next keypress) |
| `Tab` | detail | jump to the other section's title: request or response |
```

In the `s` row for `events`, replace `a request and its response are hidden or shown together` with `an exchange is hidden only when no plugin acted on either half`.

In the column-id table, delete the `PHASE` row and replace these rows:

```markdown
| `#` | exchange number: one per request and its response — quote it as `"#"` in YAML, or it reads as a comment |
| `TIME` | wall-clock time the request was recorded |
| `STATUS` | HTTP status of the response; blank while it is on its way, `lost` when none came for 10 minutes |
| `BYTES` | body bytes: ↑ request sent, ↓ response received; blank when the proxy counted nothing |
| `TOKENS` | prompt→output tokens, and what `tool-prune` saved |
| `COST` | the exchange's estimated cost, and what `tool-prune` saved |
```

- [ ] **Step 4: Check that no doc still describes two rows per exchange**

Run: ``grep -n "PHASE\|resp\` row\|shared by a request\|appears twice\|single event" cmd/agentop/README.md``
Expected: no line that describes the events table or the detail pane in the old way. Fix each one that does.

- [ ] **Step 5: Run every check**

Run, from the repo root:

```bash
(cd core && GOWORK=off go vet ./... && GOWORK=off go test ./session/timeline/ && gofmt -l session)
(cd cmd/agentop && GOWORK=off go vet ./... && GOWORK=off go test ./... && gofmt -l .)
pre-commit run --files cmd/agentop/README.md
```

Expected: `ok` for every package (the known `TestSetupRollbackWaitsOutLaunchdTeardown` timeout excepted, if it appears), no vet or gofmt output, pre-commit `Passed`.

- [ ] **Step 6: Run it against real sessions**

This step needs the human, or an `expect` drive of the TUI. Do not run `make dev-install` or `agentop service restart`: they restart the proxy that every session shares.

```bash
cd cmd/agentop && GOWORK=off go build -o "$CLAUDE_JOB_DIR/tmp/agentop" . && "$CLAUDE_JOB_DIR/tmp/agentop" observe
```

Open three sessions: a Claude Code session with subagents, a Bob or OpenCode session, and `default`. In each, check:

1. Each exchange is one row, and `#` goes up row by row.
2. A call in flight has a blank STATUS, and fills in place when it returns.
3. Enter on an inference row shows REQUEST, then RESPONSE. `Tab` moves to RESPONSE, and the fixed line says `RESPONSE`.
4. `y` in each section writes that half.
5. `T`, then Enter on a tool, lands on the row of the call that ran it.

- [ ] **Step 7: Commit**

```bash
git add cmd/agentop/README.md
git commit -s -F - <<'EOF'
Docs: Describe one events row per exchange and the two-part detail

The README described two rows per exchange, the PHASE column and a
detail pane for one event. It now describes merged rows, the lost
status, the REQUEST and RESPONSE sections, Tab, and which half y
writes.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
EOF
```

---

## After the last task

Open one PR with all eight commits. Title: `Feat: Show each request and its response as one events row`. The body names #1681, sums up the measured facts from the spec, and lists what was not measured: cluster traffic (inbound A2A, response-side guardrails). End the body with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Write the body to a file and pass it with `gh pr create --body-file`, not an inline heredoc.
