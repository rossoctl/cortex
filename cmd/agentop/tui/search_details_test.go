package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
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
