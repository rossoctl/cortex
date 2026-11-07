# agentop details search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On the events pane, a `details` box in the `/` prompt makes Enter, `n` and `N` find the next row whose detail view shows the query.

**Architecture:** All code is in `cmd/agentop/tui`. The detail pane's text builder moves into `detailContent`, and `detailHas` searches what it builds. A scan loop walks the visible rows from the cursor. It fetches each projected event with `GetEventRaw`, checks it, and drops it. The proxy does not change.

**Tech Stack:** Go 1.26, Bubble Tea, `cmd/agentop/tui/find`, `charmbracelet/x/ansi`, `net/http/httptest` for tests.

**Spec:** `docs/superpowers/specs/2026-11-07-agentop-search-details-design.md`

## Global Constraints

- The proxy, `core/` and the session API do not change.
- The scan never calls `replaceHeldEvent` or `markFullFetched`. A fetched event lives only in its reply message.
- One fetch runs at a time. Checks that need no fetch run at most `detailScanBatch = 20` per update.
- With the box off, `/`, `n` and `N` behave exactly as now. The current tests in `search_test.go`, `search_panes_test.go` and `detail_search_test.go` pass with no change.
- Footer texts, exactly: `[/<q> details: Enter to search]`, `[/<q> details: searching <k> of <n>…]`, `[/<q> details: match]`, `[/<q> details: no match in <n> rows]`.
- Prompt box, exactly: `[ ] details` or `[x] details`, then `  (tab)`, after three spaces.
- Commit with `git commit -s`. End each message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Comments, commit messages and docs in ASD-STE100 Simplified Technical English (short sentences, active voice, no semicolons).
- Run Go commands from `cmd/agentop` with `GOWORK=off`. Run `go vet ./...` and `gofmt -l .` before each commit.

## Review Focus

1. **Rows move during a scan** (the stream appends rows, FIFO evicts). The scan must not panic on an index past the end, and must not report a match on a row other than the one it checked. Pinned in Task 2 (`TestDetailScan_RowsShrinkDuringScan`).
2. **The proxy evicted an event the scan wants** (404). The scan skips it and goes on. Pinned in Task 2 (`TestDetailScan_SkipsAnEvictedEvent`).
3. **The operator leaves the events pane while a fetch is in flight.** The late reply must not move the events cursor or the pane. Pinned in Task 2 (`TestDetailScan_LeavingThePaneEndsTheScan`).
4. **The operator opens another session.** The box, the checked results and a running scan go with the old session, as the query does now. Pinned in Task 3 (`TestDetailSearch_ResetClearsEverything`).
5. **A query that is only in a header block** (for example the tunnel header). The detail pane shows it, so the scan must match it. Pinned in Task 1 (`TestDetailHas_AgreesWithThePane`, header case).

---

### Task 1: One function for the detail view's text

**Files:**
- Modify: `cmd/agentop/tui/detail_pane.go:39-66` (`showDetail`)
- Create: `cmd/agentop/tui/search_details.go`
- Test: `cmd/agentop/tui/search_details_test.go`

**Interfaces:**
- Consumes: `tlsHeader`, `rewriteHeader`, `servedHeader`, `timingHeader`, `tunnelHeader`, `filterForDetail`, `withRaw`, `treeFromJSON`, `ColorizeJSONBytes`, `renderTree`, `foldState{all: true}` (all in `tui`).
- Produces: `func detailContent(r eventRow) (head []string, root *node)` and `func detailHas(r eventRow, q string) bool`.

- [ ] **Step 1: Write the failing tests**

Create `cmd/agentop/tui/search_details_test.go`:

```go
package tui

import (
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// A response event holds its request's messages too, and the detail view hides them. So a
// prompt matches the request row and not the response row.
func TestDetailHas_RequestRowNotResponse(t *testing.T) {
	inf := &pipeline.InferenceExtension{Model: "m",
		Messages:   []pipeline.InferenceMessage{{Role: "user", Content: "find the NEEDLE-PROMPT"}},
		Completion: "the reply"}
	req := eventRow{event: &pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionRequest, Inference: inf}}
	resp := eventRow{event: &pipeline.SessionEvent{Seq: 2, Phase: pipeline.SessionResponse, Inference: inf}}

	if !detailHas(req, "needle-prompt") {
		t.Error("the request row's detail shows the prompt, so it must match (case-insensitive)")
	}
	if detailHas(resp, "needle-prompt") {
		t.Error("the response row's detail hides the messages, so it must not match")
	}
	if !detailHas(resp, "THE REPLY") {
		t.Error("the response row's detail shows the completion, so it must match")
	}
}

// Text that only the raw capture holds matches, because the detail view shows the capture.
func TestDetailHas_RawCapture(t *testing.T) {
	ev := &pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionRequest,
		Inference: &pipeline.InferenceExtension{Model: "m"}}
	raw := &pipeline.RawCapture{Request: `{"system":"RAW-ONLY-NEEDLE"}`}

	if detailHas(eventRow{event: ev}, "raw-only-needle") {
		t.Fatal("without the capture there is nothing to match")
	}
	if !detailHas(eventRow{event: ev, raw: raw}, "raw-only-needle") {
		t.Error("the capture is a node of the detail tree, so its text must match")
	}
}

// detailHas and the detail pane's own search must agree on every row and query: a row the
// scan reports must open on a highlighted match, and a row it skips must open on none.
func TestDetailHas_AgreesWithThePane(t *testing.T) {
	tunnel := &pipeline.SessionEvent{Seq: 9, Host: "tunnel-host.example:443"}
	rows := []eventRow{
		{event: &pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionRequest,
			Inference: &pipeline.InferenceExtension{Model: "m",
				Messages: []pipeline.InferenceMessage{{Role: "user", Content: "alpha beta"}}}}},
		{event: &pipeline.SessionEvent{Seq: 2, Phase: pipeline.SessionResponse,
			Inference: &pipeline.InferenceExtension{Model: "m",
				Messages:   []pipeline.InferenceMessage{{Role: "user", Content: "alpha beta"}},
				Completion: "gamma"}}},
		{event: &pipeline.SessionEvent{Seq: 3, Phase: pipeline.SessionRequest, Host: "h"}, tunnel: tunnel},
	}
	for _, r := range rows {
		for _, q := range []string{"alpha", "gamma", "tunnel-host", "absent-text"} {
			m := detailModel(t, "s1", r.event)
			m.search = map[paneID]string{paneDetail: q}
			m.showDetail(r, true)
			pane := len(m.detailDoc.matches) > 0
			if got := detailHas(r, q); got != pane {
				t.Errorf("seq %d, %q: detailHas = %v, the pane shows a match = %v", r.event.Seq, q, got, pane)
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui -run 'TestDetailHas' -count=1`
Expected: FAIL to compile with `undefined: detailHas`.

- [ ] **Step 3: Move the text builder into `detailContent`**

In `cmd/agentop/tui/detail_pane.go`, replace the body of `showDetail` from `e := r.event` down to the closing `}` of the `if data, err := json.Marshal(e)` block with:

```go
	m.detailRow = r
	m.detailEvent = r.event
	m.detailHead, m.detailTree = detailContent(r)
```

Keep the rest of `showDetail` (from `if resetScroll {`) as it is.

Add below `showDetail`:

```go
// detailContent is what the detail pane draws for r: the header blocks, each followed by a
// blank line, and the event's JSON as a tree. When the JSON does not build a tree, it is one
// more header, colorized. showDetail draws this and detailHas searches it, so a details search
// matches exactly what opening the row shows.
func detailContent(r eventRow) (head []string, root *node) {
	e := r.event
	data, err := json.Marshal(e)
	if err != nil {
		return []string{"error marshaling event: " + err.Error()}, nil
	}
	// The header blocks first, in the order they have always had: TLS, what plugins rewrote,
	// then the folded tunnel. Lines like the JSON below them, so a search reads them too.
	var tunnel string
	if r.tunnel != nil {
		tunnel = tunnelHeader(r.tunnel)
	}
	for _, h := range []string{tlsHeader(e.TLS), rewriteHeader(e), servedHeader(e), timingHeader(e), tunnel} {
		if h != "" {
			head = append(head, strings.Split(h, "\n")...)
			head = append(head, "")
		}
	}
	filtered := withRaw(filterForDetail(data, e.Phase), r.raw)
	if root, err := treeFromJSON(filtered); err == nil {
		return head, root
	}
	return append(head, ColorizeJSONBytes(filtered)), nil
}
```

- [ ] **Step 4: Add `detailHas`**

Create `cmd/agentop/tui/search_details.go`:

```go
package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
)

// THE EVENTS PANE'S `details` BOX (#1680). With the box on, Enter, n and N find the next row
// whose detail view shows the query: every field the pane draws, the raw capture too. The
// events list carries summaries, so the scan fetches each event it checks, one at a time, and
// drops it after the check. It stops at the first match, so its cost is the distance to the
// next match, not the size of the session.

// detailHas reports whether the detail pane, opened on r, shows q: in a header line, or in a
// line of the tree with every fold open. It reads the lines before wrapping, with the matcher
// the pane's own search uses. So a row it reports opens on a highlighted match.
func detailHas(r eventRow, q string) bool {
	head, root := detailContent(r)
	for _, h := range head {
		for _, l := range strings.Split(h, "\n") {
			if find.Contains(ansi.Strip(l), q) {
				return true
			}
		}
	}
	if root == nil {
		return false
	}
	for _, l := range renderTree(root, foldState{all: true}) {
		if find.Contains(ansi.Strip(l.text), q) {
			return true
		}
	}
	return false
}
```

Check that `github.com/charmbracelet/x/ansi` is the import path `searchdoc.go` uses. If it differs, use the path from `searchdoc.go`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd cmd/agentop && GOWORK=off go test ./tui -run 'TestDetailHas|TestSearch_|TestApplyDetailEvent|TestShowDetail' -count=1`
Expected: PASS. If `TestDetailHas_AgreesWithThePane` fails on one case, read why before you change `detailHas`: the pane is the reference.

- [ ] **Step 6: Run the whole package, vet and gofmt**

Run: `cd cmd/agentop && GOWORK=off go test ./tui -count=1 && GOWORK=off go vet ./... && gofmt -l .`
Expected: `ok`, no vet output, no gofmt output.

- [ ] **Step 7: Commit**

```bash
git add cmd/agentop/tui/detail_pane.go cmd/agentop/tui/search_details.go cmd/agentop/tui/search_details_test.go
git commit -s -m "Refactor: Build the detail view's text in one function

Move the header blocks and the JSON tree of showDetail into
detailContent. Add detailHas, which checks a query against that text
with the detail search's own matcher. The detail pane does not change.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Task 2: The scan loop

**Files:**
- Modify: `cmd/agentop/tui/search_details.go`
- Modify: `cmd/agentop/tui/app.go` (model fields after `searchOrigin` near line 636, and two `case` arms in `Update` next to `case detailEventLoadedMsg:` near line 1965)
- Test: `cmd/agentop/tui/search_details_test.go`

**Interfaces:**
- Consumes: `detailHas` (Task 1). `needsFullEvent`, `heldRaw`, `eventSession`, `keyOf`, `findByKey`, `eventsSurface`, `setFlash`, `apiclient.ErrNotFound`, `(*apiclient.Client).GetEventRaw(ctx, id string, seq uint64) (*pipeline.SessionEvent, *pipeline.RawCapture, error)`.
- Produces:
  - model fields `searchDetails bool`, `detailScan *detailScan`, `detailScanGen uint64`, `detailChecked map[eventKey]bool`, `detailCheckedQ string`
  - `type detailScanState int` with `scanRunning`, `scanFound`, `scanNone`
  - `func (m *model) startDetailScan(forward, inclusive bool) tea.Cmd`
  - `func (m *model) applyDetailScanFetched(msg detailScanFetchedMsg) tea.Cmd`
  - `func (m *model) applyDetailScanNext(msg detailScanNextMsg) tea.Cmd`
  - `func (m *model) detailScanRunning() bool`
  - `func (m *model) stopDetailScan()` (Esc: cursor back, scan ends)
  - `func (m *model) cancelDetailScan()` (scan ends, cursor stays)
  - `func (m *model) resetDetailSearch()` (box off, scan ends, results cleared)

- [ ] **Step 1: Write the failing tests**

Append to `cmd/agentop/tui/search_details_test.go`, and add these imports to its import block: `"context"`, `"encoding/json"`, `"net/http"`, `"net/http/httptest"`, `"path"`, `"reflect"`, `"slices"`, `"strconv"`, `"strings"`, `"sync/atomic"`, `tea "github.com/charmbracelet/bubbletea"`, `"github.com/rossoctl/cortex/cmd/agentop/apiclient"`.

```go
// scanServer serves the full events by seq, as GET /v1/sessions/{id}/events/{seq}?raw=1
// does, and counts the fetches. A seq in status answers with that status instead.
func scanServer(t *testing.T, full map[uint64]pipeline.SessionEvent, status map[uint64]int) (*apiclient.Client, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seq, _ := strconv.ParseUint(path.Base(r.URL.Path), 10, 64)
		if code := status[seq]; code != 0 {
			http.Error(w, "test status", code)
			return
		}
		ev, ok := full[seq]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ev)
	}))
	t.Cleanup(ts.Close)
	return apiclient.New(ts.URL), &hits
}

// scanModel is an events pane of n summary request rows against a proxy that projects. The
// full form of row i holds "NEEDLE" for each i in hits, and "hello" for every other row.
func scanModel(t *testing.T, n int, status map[uint64]int, hits ...int) (*model, *atomic.Int32) {
	t.Helper()
	events := eventSeq(n, "e")
	full := map[uint64]pipeline.SessionEvent{}
	for i := range events {
		events[i].Seq = uint64(i + 1)
		f := events[i]
		inf := *f.Inference
		text := "hello"
		if slices.Contains(hits, i) {
			text = "the NEEDLE is here"
		}
		inf.Messages = []pipeline.InferenceMessage{{Role: "user", Content: text}}
		f.Inference = &inf
		full[f.Seq] = f
	}
	client, count := scanServer(t, full, status)
	m := newEventsPaneModel(events)
	m.ctx, m.client, m.serverProjects = context.Background(), client, true
	m.search = map[paneID]string{paneEvents: "needle"}
	m.searchDetails = true
	return m, count
}

// drain runs cmd and every command its messages lead to, as the program does.
func drain(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 10_000 {
			t.Fatal("the scan did not end")
		}
		switch msg := cmd().(type) {
		case detailScanFetchedMsg:
			cmd = m.applyDetailScanFetched(msg)
		case detailScanNextMsg:
			cmd = m.applyDetailScanNext(msg)
		default:
			t.Fatalf("unexpected message %T", msg)
		}
	}
}

func TestDetailScan_StopsAtTheFirstMatch(t *testing.T) {
	m, fetches := scanModel(t, 10, nil, 3, 7)
	setCursorVisible(&m.eventsTbl, 0)

	drain(t, m, m.startDetailScan(true, true))

	if c := m.eventsTbl.Cursor(); c != 3 {
		t.Errorf("cursor = %d, want 3, the first match", c)
	}
	if n := fetches.Load(); n != 4 {
		t.Errorf("fetches = %d, want 4: rows 0 to 3, and none after the match", n)
	}
	if m.detailScan == nil || m.detailScan.state != scanFound {
		t.Errorf("scan = %+v, want state scanFound", m.detailScan)
	}
}

// The memory rule: the scan drops each fetched event. If it kept them, agentop would hold
// every event it checked in full.
func TestDetailScan_LeavesTheHeldEventsAlone(t *testing.T) {
	m, _ := scanModel(t, 6, nil)
	before := slices.Clone(m.events["s"])

	drain(t, m, m.startDetailScan(true, true))

	if !reflect.DeepEqual(m.events["s"], before) {
		t.Error("the scan changed the held events: it must drop what it fetches")
	}
	if len(m.fullFetched) != 0 {
		t.Errorf("fullFetched = %v, want empty: the scan must not mark events as fetched", m.fullFetched)
	}
}

func TestDetailScan_NoMatch(t *testing.T) {
	m, fetches := scanModel(t, 5, nil)
	setCursorVisible(&m.eventsTbl, 2)

	drain(t, m, m.startDetailScan(true, true))

	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Errorf("cursor = %d, want 2: no match leaves it where it was", c)
	}
	if n := fetches.Load(); n != 5 {
		t.Errorf("fetches = %d, want 5", n)
	}
	if st := m.searchStatus(paneEvents); st != "[/needle details: no match in 5 rows]" {
		t.Errorf("status = %q", st)
	}
}

// The scan wraps past the end, and a pass over events already checked fetches nothing.
func TestDetailScan_WrapsAndRemembers(t *testing.T) {
	m, fetches := scanModel(t, 8, nil, 2)
	setCursorVisible(&m.eventsTbl, 3)

	// Enter from row 3: rows 3 to 7, then 0 to 2. Every row is fetched one time.
	drain(t, m, m.startDetailScan(true, true))
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Fatalf("cursor = %d, want 2, past the end and round", c)
	}
	first := fetches.Load()
	if first != 8 {
		t.Errorf("first pass fetches = %d, want 8", first)
	}

	// n from row 2: every other row is a known no-match, and row 2 a known match.
	drain(t, m, m.startDetailScan(true, false))
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Errorf("second pass cursor = %d, want 2 again", c)
	}
	if n := fetches.Load(); n != first {
		t.Errorf("second pass fetched %d events, want 0", n-first)
	}
}

func TestDetailScan_Backward(t *testing.T) {
	m, _ := scanModel(t, 10, nil, 2, 6)
	setCursorVisible(&m.eventsTbl, 9)

	drain(t, m, m.startDetailScan(false, false))

	if c := m.eventsTbl.Cursor(); c != 6 {
		t.Errorf("cursor = %d, want 6, the first match above row 9", c)
	}
}

// Esc ends the scan and puts the cursor back. A reply that lands after that is dropped.
func TestDetailScan_StopDropsTheLateReply(t *testing.T) {
	m, _ := scanModel(t, 4, nil, 0)
	setCursorVisible(&m.eventsTbl, 1)
	cmd := m.startDetailScan(true, true)
	msg := cmd()

	m.stopDetailScan()
	if got := m.applyDetailScanFetched(msg.(detailScanFetchedMsg)); got != nil {
		t.Error("a stale reply returned a command")
	}
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("cursor = %d, want 1, where the scan started", c)
	}
	if m.detailScan != nil {
		t.Errorf("scan = %+v, want nil after Esc", m.detailScan)
	}
}

// A new scan, for example after a new query, makes the old scan's reply stale.
func TestDetailScan_NewScanDropsTheOldReply(t *testing.T) {
	m, _ := scanModel(t, 4, nil, 0)
	setCursorVisible(&m.eventsTbl, 0)
	old := m.startDetailScan(true, true)().(detailScanFetchedMsg)

	m.search[paneEvents] = "other"
	next := m.startDetailScan(true, true)
	if got := m.applyDetailScanFetched(old); got != nil {
		t.Error("the old scan's reply returned a command")
	}
	if m.detailScan == nil || m.detailScan.query != "other" {
		t.Errorf("scan = %+v, want the new scan still running", m.detailScan)
	}
	drain(t, m, next)
}

// Against a proxy that sends whole events, nothing is fetched. The scan yields after
// detailScanBatch checks, so keys and the stream still get through.
func TestDetailScan_YieldsBetweenBatches(t *testing.T) {
	m, fetches := scanModel(t, 50, nil)
	m.serverProjects = false
	setCursorVisible(&m.eventsTbl, 0)

	cmd := m.startDetailScan(true, true)
	if _, ok := cmd().(detailScanNextMsg); !ok {
		t.Fatal("the first batch did not yield with a detailScanNextMsg")
	}
	if left := m.detailScan.left; left != 50-detailScanBatch {
		t.Errorf("rows left = %d, want %d", left, 50-detailScanBatch)
	}
	drain(t, m, cmd)
	if n := fetches.Load(); n != 0 {
		t.Errorf("fetches = %d, want 0: the events are already whole", n)
	}
}

// The proxy evicted an event (404). The scan skips it and goes on.
func TestDetailScan_SkipsAnEvictedEvent(t *testing.T) {
	m, _ := scanModel(t, 6, map[uint64]int{2: http.StatusNotFound}, 4)
	setCursorVisible(&m.eventsTbl, 0)

	drain(t, m, m.startDetailScan(true, true))

	if c := m.eventsTbl.Cursor(); c != 4 {
		t.Errorf("cursor = %d, want 4, past the evicted event", c)
	}
}

// Any other fetch error stops the scan and says why.
func TestDetailScan_FetchErrorStops(t *testing.T) {
	m, fetches := scanModel(t, 6, map[uint64]int{2: http.StatusInternalServerError}, 4)
	setCursorVisible(&m.eventsTbl, 0)

	drain(t, m, m.startDetailScan(true, true))

	if m.detailScan != nil {
		t.Errorf("scan = %+v, want nil after an error", m.detailScan)
	}
	if !strings.Contains(m.flash, "details search stopped") {
		t.Errorf("flash = %q, want the reason", m.flash)
	}
	if n := fetches.Load(); n != 2 {
		t.Errorf("fetches = %d, want 2: nothing after the error", n)
	}
}

// A reply that lands after the operator left the events pane ends the scan, and does not
// move the events cursor.
func TestDetailScan_LeavingThePaneEndsTheScan(t *testing.T) {
	m, _ := scanModel(t, 4, nil, 0)
	setCursorVisible(&m.eventsTbl, 2)
	msg := m.startDetailScan(true, true)().(detailScanFetchedMsg)

	m.pane = paneSessions
	if got := m.applyDetailScanFetched(msg); got != nil {
		t.Error("the reply returned a command after the pane changed")
	}
	if m.detailScan != nil {
		t.Errorf("scan = %+v, want nil", m.detailScan)
	}
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Errorf("cursor = %d, want 2", c)
	}
}

// Rows can go away during a scan (FIFO eviction). The scan must not index past the end.
func TestDetailScan_RowsShrinkDuringScan(t *testing.T) {
	m, _ := scanModel(t, 10, nil, 1)
	setCursorVisible(&m.eventsTbl, 5)
	cmd := m.startDetailScan(true, false)
	msg := cmd().(detailScanFetchedMsg)

	m.events["s"] = m.events["s"][:3]
	m.rebuildEventsTable()
	drain(t, m, func() tea.Msg { return msg })

	if m.detailScan == nil || m.detailScan.state != scanFound {
		t.Fatalf("scan = %+v, want the match on the row that is left", m.detailScan)
	}
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("cursor = %d, want 1", c)
	}
}
```

Check the flash field name with `grep -n "flash " cmd/agentop/tui/app.go`. If it is not `m.flash`, use the name `setFlash` writes.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui -run 'TestDetailScan' -count=1`
Expected: FAIL to compile with `undefined: detailScanFetchedMsg` (and the other new names).

- [ ] **Step 3: Add the model fields**

In `cmd/agentop/tui/app.go`, after the `searchOrigin searchSpot` field (near line 636), add:

```go
	// searchDetails is the events pane's `details` box: with it on, Enter, n and N find the
	// next row whose detail view shows the query. detailScan is that search, running or
	// ended, and detailScanGen numbers each one, so a reply for an older one is dropped.
	// detailChecked is what each checked event gave for detailCheckedQ: true for a match.
	// See search_details.go.
	searchDetails  bool
	detailScan     *detailScan
	detailScanGen  uint64
	detailChecked  map[eventKey]bool
	detailCheckedQ string
```

- [ ] **Step 4: Write the loop**

Append to `cmd/agentop/tui/search_details.go`, and add `"errors"`, `tea "github.com/charmbracelet/bubbletea"`, `"github.com/rossoctl/cortex/cmd/agentop/apiclient"` and `"github.com/rossoctl/cortex/core/pipeline"` to its imports:

```go
// detailScanBatch is how many events one update checks with no fetch. Then the scan yields,
// so a long scan against a proxy that sends whole events still lets keys and the stream in.
const detailScanBatch = 20

type detailScanState int

const (
	scanRunning detailScanState = iota
	scanFound
	scanNone
)

// detailScan is one details search. It walks the visible rows from where it started, in one
// direction, wrapping, until a match or until it has visited each row one time.
type detailScan struct {
	gen     uint64
	query   string
	forward bool
	// origin is where the cursor was, for Esc.
	origin searchSpot
	// at is the row visited last. left and total count the rows, for the footer.
	at, left, total int
	state           detailScanState
}

// detailScanFetchedMsg is the full event a scan fetched to check.
type detailScanFetchedMsg struct {
	gen   uint64
	key   eventKey
	event *pipeline.SessionEvent
	raw   *pipeline.RawCapture
	err   error
}

// detailScanNextMsg continues a scan that yielded after a batch.
type detailScanNextMsg struct{ gen uint64 }

// startDetailScan starts a details search for the events pane's committed query from the
// cursor. inclusive lets the cursor's own row match: Enter starts there, n and N start past it.
func (m *model) startDetailScan(forward, inclusive bool) tea.Cmd {
	m.cancelDetailScan()
	q, n := m.search[paneEvents], len(m.visibleRows)
	if q == "" || n == 0 {
		return nil
	}
	if q != m.detailCheckedQ {
		m.detailChecked, m.detailCheckedQ = map[eventKey]bool{}, q
	}
	at := m.eventsTbl.Cursor()
	if inclusive {
		// One step back, so the first step lands on the cursor's row.
		if forward {
			at--
		} else {
			at++
		}
	}
	m.detailScan = &detailScan{gen: m.detailScanGen, query: q, forward: forward,
		origin: eventsSurface{m}.spot(), at: at, left: n, total: n}
	return m.runDetailScan()
}

// runDetailScan visits rows until a match, a fetch, a full batch, or no rows left.
func (m *model) runDetailScan() tea.Cmd {
	s := m.detailScan
	for checked := 0; s.left > 0; {
		if checked == detailScanBatch {
			gen := s.gen
			return func() tea.Msg { return detailScanNextMsg{gen: gen} }
		}
		n := len(m.visibleRows)
		if n == 0 {
			break
		}
		if s.forward {
			s.at = ((s.at+1)%n + n) % n
		} else {
			s.at = ((s.at-1)%n + n) % n
		}
		s.left--
		r := m.visibleRows[s.at]
		if r.event == nil {
			continue // a subagent block's header row
		}
		k := keyOf(r.event)
		if matched, ok := m.detailChecked[k]; ok {
			if matched {
				m.foundDetail(k)
				return nil
			}
			continue
		}
		if m.needsFullEvent(r.event) && m.client != nil {
			ctx, client, gen := m.ctx, m.client, s.gen
			sess, seq := eventSession(m.selectedSess, r.event), r.event.Seq
			return func() tea.Msg {
				ev, raw, err := client.GetEventRaw(ctx, sess, seq)
				return detailScanFetchedMsg{gen: gen, key: k, event: ev, raw: raw, err: err}
			}
		}
		r.raw, _ = m.heldRaw(r.event)
		checked++
		if m.checkDetailRow(r, k) {
			return nil
		}
	}
	s.state = scanNone
	return nil
}

// checkDetailRow records whether r's detail shows the query, and ends the scan on a match.
func (m *model) checkDetailRow(r eventRow, k eventKey) bool {
	matched := detailHas(r, m.detailScan.query)
	m.detailChecked[k] = matched
	if matched {
		m.foundDetail(k)
	}
	return matched
}

// foundDetail ends the scan on the row of k, which can have moved since the scan visited it.
func (m *model) foundDetail(k eventKey) {
	if i := findByKey(m.visibleRows, k); i >= 0 {
		eventsSurface{m}.reveal(i)
	}
	m.detailScan.state = scanFound
}

// detailScanLive reports whether gen is the running scan, on the events pane. A scan whose pane was
// left ends here, so its late reply cannot move a cursor the operator no longer sees.
func (m *model) detailScanLive(gen uint64) bool {
	s := m.detailScan
	if s == nil || s.gen != gen || s.state != scanRunning {
		return false
	}
	if m.pane != paneEvents {
		m.cancelDetailScan()
		return false
	}
	return true
}

// applyDetailScanFetched checks a fetched event and drops it. The event goes into no list: the
// scan holds one event at a time, never the session.
func (m *model) applyDetailScanFetched(msg detailScanFetchedMsg) tea.Cmd {
	if !m.detailScanLive(msg.gen) {
		return nil
	}
	switch {
	case errors.Is(msg.err, apiclient.ErrNotFound):
		// The proxy evicted it. Nothing to check.
	case msg.err != nil:
		m.cancelDetailScan()
		m.setFlash("details search stopped: " + msg.err.Error())
		return nil
	default:
		if i := findByKey(m.visibleRows, msg.key); i >= 0 {
			r := m.visibleRows[i]
			r.event, r.raw = msg.event, msg.raw
			if m.checkDetailRow(r, msg.key) {
				return nil
			}
		}
	}
	return m.runDetailScan()
}

// applyDetailScanNext continues a scan after a batch.
func (m *model) applyDetailScanNext(msg detailScanNextMsg) tea.Cmd {
	if !m.detailScanLive(msg.gen) {
		return nil
	}
	return m.runDetailScan()
}

func (m *model) detailScanRunning() bool {
	return m.detailScan != nil && m.detailScan.state == scanRunning
}

// cancelDetailScan ends any scan. The cursor stays where it is.
func (m *model) cancelDetailScan() {
	m.detailScanGen++
	m.detailScan = nil
}

// stopDetailScan is Esc during a scan: the scan ends and the cursor goes back.
func (m *model) stopDetailScan() {
	if s := m.detailScan; s != nil {
		eventsSurface{m}.restore(s.origin)
	}
	m.cancelDetailScan()
}

// resetDetailSearch clears the box and the results with the events pane's query, when the
// operator opens another session.
func (m *model) resetDetailSearch() {
	m.cancelDetailScan()
	m.searchDetails = false
	m.detailChecked, m.detailCheckedQ = nil, ""
}
```

Note: `startDetailScan` calls `cancelDetailScan` first, which increases `detailScanGen`. The new scan takes that number, so every older reply is stale.

- [ ] **Step 5: Route the messages in `Update`**

In `cmd/agentop/tui/app.go`, next to `case detailEventLoadedMsg:` (near line 1965), add:

```go
	case detailScanFetchedMsg:
		return m, m.applyDetailScanFetched(msg)

	case detailScanNextMsg:
		return m, m.applyDetailScanNext(msg)
```

- [ ] **Step 6: Add the footer status**

In `cmd/agentop/tui/search.go`, at the start of `searchStatus`, add:

```go
	if t == paneEvents && m.searchDetails {
		return m.detailSearchStatus()
	}
```

Append to `cmd/agentop/tui/search_details.go` (add `"fmt"` to its imports):

```go
// detailSearchStatus is the footer's account of the details search.
func (m *model) detailSearchStatus() string {
	q := m.searchQuery(paneEvents)
	if q == "" {
		return ""
	}
	s := m.detailScan
	switch {
	case m.searching && m.searchPane == paneEvents:
		return fmt.Sprintf("[/%s details: Enter to search]", q)
	case s == nil || s.query != q:
		return fmt.Sprintf("[/%s details]", q)
	case s.state == scanRunning:
		return fmt.Sprintf("[/%s details: searching %d of %d…]", q, s.total-s.left, s.total)
	case s.state == scanFound:
		return fmt.Sprintf("[/%s details: match]", q)
	}
	return fmt.Sprintf("[/%s details: no match in %d rows]", q, s.total)
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd cmd/agentop && GOWORK=off go test ./tui -run 'TestDetailScan|TestDetailHas' -count=1 -race`
Expected: PASS.

- [ ] **Step 8: Run the whole package, vet and gofmt**

Run: `cd cmd/agentop && GOWORK=off go test ./tui -count=1 && GOWORK=off go vet ./... && gofmt -l .`
Expected: `ok`, no vet output, no gofmt output.

- [ ] **Step 9: Commit**

```bash
git add cmd/agentop/tui/search_details.go cmd/agentop/tui/search_details_test.go cmd/agentop/tui/app.go cmd/agentop/tui/search.go
git commit -s -m "Feat: Add the details scan to the agentop events pane

The scan walks the visible rows from the cursor and stops at the first
row whose detail view shows the query. It fetches one projected event
at a time, checks it, and drops it. It remembers each result for the
query, so a second pass makes no fetch. No key starts it yet.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Task 3: Keys, prompt box, resets and docs

**Files:**
- Modify: `cmd/agentop/tui/search.go` (`searchKey`, `searchStep`, header comment)
- Modify: `cmd/agentop/tui/search_details.go` (`searchPromptView`, `detailsBoxWidth`)
- Modify: `cmd/agentop/tui/keys.go` (Esc near line 92, `n`/`N` near line 101, the events Enter arm near line 742, the session change near line 708, the search hint near line 1197)
- Modify: `cmd/agentop/tui/layout.go:122`
- Modify: `cmd/agentop/tui/app.go` (the prompt near line 2671, the connection reset near line 1153)
- Modify: `cmd/agentop/tui/timeline_pane.go:481`
- Modify: `cmd/agentop/tui/help_overlay.go:326-335`
- Modify: `cmd/agentop/README.md` (the `/` row near line 1476, the summary note near line 1758)
- Test: `cmd/agentop/tui/search_details_test.go`

**Interfaces:**
- Consumes: everything Task 2 produces. `newSearchEventsModel`, `typeKeys`, `press`, `keyRune`, `statusRow`, `stripANSI` (test helpers in `search_test.go`, `footer_test.go`).
- Produces: `func (m *model) searchPromptView() string`, `const detailsBoxWidth`. `searchStep` now returns `tea.Cmd`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/agentop/tui/search_details_test.go`:

```go
// keysModel is scanModel's pane with the search prompt production builds, so keys reach it.
func keysModel(t *testing.T, n int, hits ...int) (*model, *atomic.Int32) {
	t.Helper()
	resetSettingsForTest(t)
	m, fetches := scanModel(t, n, nil, hits...)
	m.searchInput = newSearchInput()
	m.width, m.height = 200, 40
	m.eventColumns = defaultColumnSelection()
	m.search = nil
	m.searchDetails = false
	m.layout()
	m.rebuildEventsTable()
	return m, fetches
}

// runAll runs a command and every command its messages lead to, through Update.
func runAll(m *model, cmd tea.Cmd) {
	for i := 0; cmd != nil && i < 10_000; i++ {
		_, cmd = m.Update(cmd())
	}
}

func TestDetailSearch_TabTogglesTheBoxOnlyOnEvents(t *testing.T) {
	m, _ := keysModel(t, 3)
	typeKeys(m, "/")
	press(m, tea.KeyTab)
	if !m.searchDetails {
		t.Fatal("Tab on the events prompt did not turn the box on")
	}
	if v := stripANSI(m.searchPromptView()); !strings.Contains(v, "[x] details  (tab)") {
		t.Errorf("prompt = %q, want the box on", v)
	}
	press(m, tea.KeyTab)
	if m.searchDetails {
		t.Error("a second Tab did not turn the box off")
	}

	press(m, tea.KeyEsc)
	// The prompt open on another pane: Tab there changes nothing.
	m.pane, m.searching, m.searchPane = paneSessions, true, paneSessions
	press(m, tea.KeyTab)
	if m.searchDetails {
		t.Error("Tab on the sessions prompt turned the events box on")
	}
	if v := stripANSI(m.searchPromptView()); strings.Contains(v, "details") {
		t.Errorf("prompt = %q, want no box off the events pane", v)
	}
}

// With the box on, typing does not move the cursor. Enter runs the scan from the row where
// `/` was pressed, and n finds the next match.
func TestDetailSearch_EnterAndN(t *testing.T) {
	m, _ := keysModel(t, 10, 2, 6)
	setCursorVisible(&m.eventsTbl, 1)
	typeKeys(m, "/")
	press(m, tea.KeyTab)
	typeKeys(m, "needle")
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("typing moved the cursor to %d, want 1", c)
	}
	if st := m.searchStatus(paneEvents); st != "[/needle details: Enter to search]" {
		t.Errorf("status while typing = %q", st)
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	runAll(m, cmd)
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Fatalf("Enter landed on %d, want 2", c)
	}
	if st := m.searchStatus(paneEvents); st != "[/needle details: match]" {
		t.Errorf("status after Enter = %q", st)
	}

	_, cmd = m.Update(keyRune('n'))
	runAll(m, cmd)
	if c := m.eventsTbl.Cursor(); c != 6 {
		t.Errorf("n landed on %d, want 6", c)
	}
	_, cmd = m.Update(keyRune('N'))
	runAll(m, cmd)
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Errorf("N landed on %d, want 2", c)
	}
}

// Esc during a scan stops it and puts the cursor back. It does not leave the pane.
func TestDetailSearch_EscStopsTheScan(t *testing.T) {
	m, _ := keysModel(t, 10, 8)
	setCursorVisible(&m.eventsTbl, 1)
	typeKeys(m, "/")
	press(m, tea.KeyTab)
	typeKeys(m, "needle")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.detailScanRunning() {
		t.Fatal("Enter started no scan")
	}
	pending := cmd()

	press(m, tea.KeyEsc)
	if m.pane != paneEvents {
		t.Errorf("Esc during a scan left the pane for %v", m.pane)
	}
	if m.detailScan != nil {
		t.Error("Esc did not stop the scan")
	}
	m.Update(pending)
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("cursor = %d, want 1", c)
	}
}

// Opening a row while the box is on gives the detail search the same query, so the match
// is highlighted.
func TestDetailSearch_OpeningARowCarriesTheQuery(t *testing.T) {
	m, _ := keysModel(t, 5, 3)
	m.search = map[paneID]string{paneEvents: "needle"}
	m.searchDetails = true
	runAll(m, m.startDetailScan(true, true))

	press(m, tea.KeyEnter)
	if m.pane != paneDetail {
		t.Fatalf("pane = %v, want detail", m.pane)
	}
	if q := m.search[paneDetail]; q != "needle" {
		t.Errorf("detail query = %q, want needle", q)
	}
}

// With the box off, n uses the table's matches, as before. No scan starts.
func TestDetailSearch_BoxOffKeepsTheTableSearch(t *testing.T) {
	m, fetches := keysModel(t, 5)
	typeKeys(m, "/e3")
	press(m, tea.KeyEnter)
	_, cmd := m.Update(keyRune('n'))
	runAll(m, cmd)
	if m.detailScan != nil || fetches.Load() != 0 {
		t.Errorf("box off: scan = %+v, fetches = %d, want neither", m.detailScan, fetches.Load())
	}
}

func TestDetailSearch_ResetClearsEverything(t *testing.T) {
	m, _ := keysModel(t, 5, 3)
	m.search = map[paneID]string{paneEvents: "needle"}
	m.searchDetails = true
	cmd := m.startDetailScan(true, true)

	m.resetDetailSearch()
	if m.searchDetails || m.detailScan != nil || m.detailChecked != nil || m.detailCheckedQ != "" {
		t.Errorf("after reset: box=%v scan=%+v checked=%v q=%q", m.searchDetails, m.detailScan, m.detailChecked, m.detailCheckedQ)
	}
	runAll(m, cmd)
	if m.detailScan != nil {
		t.Error("a reply from before the reset started the scan again")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd cmd/agentop && GOWORK=off go test ./tui -run 'TestDetailSearch' -count=1`
Expected: FAIL to compile with `undefined: m.searchPromptView`.

- [ ] **Step 3: Add the prompt box**

Append to `cmd/agentop/tui/search_details.go`:

```go
// detailsBoxWidth is the room the box takes beside the events pane's prompt.
const detailsBoxWidth = len("   [ ] details  (tab)")

// searchPromptView is the open prompt, with the details box beside it on the events pane.
func (m *model) searchPromptView() string {
	v := m.searchInput.View()
	if m.searchPane != paneEvents {
		return v
	}
	box := "[ ] details"
	if m.searchDetails {
		box = "[x] details"
	}
	return v + "   " + box + "  (tab)"
}
```

In `cmd/agentop/tui/layout.go:122`, replace `m.searchInput.Width = m.width - 4` with:

```go
	m.searchInput.Width = m.width - 4
	if m.searchPane == paneEvents {
		m.searchInput.Width = max(m.searchInput.Width-detailsBoxWidth, 1)
	}
```

In `cmd/agentop/tui/app.go` near line 2671, in the session-view body (the block after `header := styleTitle.Render(title)`), replace `body = m.searchInput.View() + "\n" + body` with `body = m.searchPromptView() + "\n" + body`. Do not change the namespaces and pods pickers' prompts (near lines 2504 and 2531).

- [ ] **Step 4: Handle Tab, Enter and typing in `searchKey`**

In `cmd/agentop/tui/search.go`, in `searchKey`'s `switch msg.String()`, add a case before `case "enter":`:

```go
	case "tab":
		// The events pane's details box. Tab types nothing, so the prompt can take it. With the
		// box on, typing does not move the cursor, so it goes back to where `/` was pressed.
		if t == paneEvents {
			m.searchDetails = !m.searchDetails
			if s := m.surface(t); s != nil {
				s.restore(m.searchOrigin)
			}
		}
		return nil
```

In `case "enter":`, replace the final `m.closeSearch()` and `return nil` with:

```go
		m.closeSearch()
		if t == paneEvents && m.searchDetails {
			return m.startDetailScan(true, true)
		}
		return nil
```

After `s.redraw()` in the typing path (before `if m.searchInput.Value() != "" {`), add:

```go
	if t == paneEvents && m.searchDetails {
		return cmd // the details search runs on Enter, not on each key
	}
```

- [ ] **Step 5: Make `n` and `N` start a scan**

In `cmd/agentop/tui/search.go`, change `searchStep` to:

```go
// searchStep is n (forward) and N on target t: the next match past where it is, wrapping.
// Nothing happens with no match, which includes having no search. With the events pane's
// details box on, it starts a details scan instead.
func (m *model) searchStep(t paneID, forward bool) tea.Cmd {
	if t == paneEvents && m.searchDetails && m.search[t] != "" {
		return m.startDetailScan(forward, false)
	}
	s := m.surface(t)
	if s == nil {
		return nil
	}
	if i, ok := nextMatch(s.matchLines(), s.spot().row, forward, false); ok {
		s.reveal(i)
	}
	return nil
}
```

In `cmd/agentop/tui/keys.go` near line 101, replace `m.searchStep(m.pane, msg.String() == "n")` and the `return nil` after it with `return m.searchStep(m.pane, msg.String() == "n")`. The help overlay's call near line 55 does not change.

- [ ] **Step 6: Esc during a scan**

In `cmd/agentop/tui/keys.go`, just before the comment ``// `/`, n and N search the pane, on every pane.`` (near line 92), add:

```go
	// Esc during a details search stops it and puts the cursor back. Esc again backs out.
	if msg.String() == "esc" && m.pane == paneEvents && m.detailScanRunning() {
		m.stopDetailScan()
		return nil
	}
```

- [ ] **Step 7: Carry the query into the detail pane**

In `cmd/agentop/tui/keys.go`, in the `case paneEvents:` Enter arm near line 742, before `er.raw, _ = m.heldRaw(er.event)`, add:

```go
			// A details match opens on its highlight: the detail search gets the same query.
			if q := m.search[paneEvents]; m.searchDetails && q != "" && m.search[paneDetail] != q {
				m.clearFoldQueries()
				m.search[paneDetail] = q
			}
```

- [ ] **Step 8: Clear the box with the query**

Add `m.resetDetailSearch()` in each place that clears the events pane's query:
- `cmd/agentop/tui/keys.go` near line 708, after `delete(m.search, paneDetail)`.
- `cmd/agentop/tui/timeline_pane.go` near line 481, after `delete(m.search, paneDetail)`.
- `cmd/agentop/tui/app.go` near line 1153, after the `for t := range m.search { ... }` loop.

- [ ] **Step 9: The hint line while the prompt is open**

In `cmd/agentop/tui/keys.go` near line 1197, replace:

```go
	if m.searching {
		return "type to search · [enter] done · [esc] cancel"
	}
```

with:

```go
	if m.searching {
		if m.searchPane == paneEvents {
			return "type to search · [tab] details · [enter] done · [esc] cancel"
		}
		return "type to search · [enter] done · [esc] cancel"
	}
```

- [ ] **Step 10: Run the tests to verify they pass**

Run: `cd cmd/agentop && GOWORK=off go test ./tui -run 'TestDetailSearch|TestDetailScan|TestDetailHas|TestSearch' -count=1 -race`
Expected: PASS.

- [ ] **Step 11: Docs**

In `cmd/agentop/tui/help_overlay.go`, in the `paneEvents` group's bindings, after `{"s", "toggle passthru/skip rows"},`, add:

```go
			{"/ then tab", "search each row's details: ↵, n and N find the next match"},
```

In `cmd/agentop/tui/search.go`, at the end of the header comment (after the paragraph that starts `// A search is neither saved nor shared.`), add:

```go
//
// The events pane's `details` box (Tab in the prompt) keeps this rule one level down: a row
// matches when its detail view shows the query. Those matches are not drawn in the table, so
// they are found one at a time, by Enter, n and N. See search_details.go.
```

In `cmd/agentop/README.md`, at the end of the `/` row's description (near line 1476), add:

```
On the events pane, `Tab` in the prompt turns on `[x] details`: then `Enter`, `n` and `N` find the next row whose detail view shows the query — every field, the exact capture too. agentop fetches one event at a time from the cursor, checks it, drops it, and stops at the first match. The footer shows the progress, and `Esc` stops it. It searches the events agentop holds; press `o` first for older ones
```

In `cmd/agentop/README.md`, in "How a timeline is fetched", replace the paragraph that starts `The summary keeps completion and A2A message text` with:

```
The summary keeps completion and A2A message text, which the table does not show,
because the events search used to match them. It no longer does: `/` matches what a
pane displays. A message's text is searched in its detail, or from the events pane
with the `details` box on, which fetches one event at a time and keeps none of them.
Dropping those fields as well would be ~299x instead of ~163x.
```

- [ ] **Step 12: Run the whole module, vet and gofmt**

Run: `cd cmd/agentop && GOWORK=off go test ./... -count=1 && GOWORK=off go vet ./... && gofmt -l .`
Expected: `ok` for every package, no vet output, no gofmt output. If a test that reads the help overlay or the hint line fails on the new text, update its expected text, and nothing else.

- [ ] **Step 13: Commit**

```bash
git add cmd/agentop/tui cmd/agentop/README.md
git commit -s -m "Feat: Search event details from the agentop events pane

On the events pane, Tab in the / prompt turns on a details box. Then
Enter, n and N find the next row whose detail view shows the query.
Esc stops a running search. Opening the row carries the query into the
detail search. The box clears with the query when another session
opens.

Fixes #1680

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

### Task 4: Measure, review, open the PR

**Files:** none changed, unless the review finds a problem.

- [ ] **Step 1: Build and install agentop only**

Run: `cd cmd/agentop && GOWORK=off go build -o /tmp/agentop-1680 .`
Do not run `make dev-install`. It restarts the shared proxy.

- [ ] **Step 2: Measure the worst case**

Run `AGENTOP_PPROF_ADDR=127.0.0.1:6061 /tmp/agentop-1680 observe` against the laptop proxy. Open session `1c22ef81-848f-4f1f-8eec-6610a1c8a200` (10,767 events). Search `/`, Tab, a query with no match, Enter. Record:
- the time from Enter to `no match in N rows`
- agentop's peak heap: `go tool pprof -sample_index=inuse_space -top http://127.0.0.1:6061/debug/pprof/heap` during the scan
- RSS from `ps -o rss= -p <pid>` before and after

If peak heap grows with the number of rows checked, stop: the memory rule is broken. Find the reference that keeps the fetched events.

- [ ] **Step 3: Pre-commit and review**

Run: `pre-commit run --files $(git diff --name-only main...HEAD)`
Then run `/code-review` on the branch and fix Critical and Important findings only.

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin docs/search-bodies-1680
gh pr create -R rossoctl/cortex --title "Feat: Search event details from the agentop events pane" --body-file /tmp/pr-1680.md
```

The PR body (`/tmp/pr-1680.md`) says: the problem, the rule, why the search runs in agentop one event at a time, the measurement from Step 2, what is left out (spec "Out of scope"), and the stale `eventHaystack` note in `core/sessionapi/summary.go`. End it with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.
