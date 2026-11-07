package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeSearch is a proxy with the search route. answer is the seqs for each session id, last the
// lastSeq it reports. noRoute answers as a proxy without the route. It counts the searches and
// every other request, because the search must fetch no event.
type fakeSearch struct {
	mu       sync.Mutex
	answer   map[string][]uint64
	last     map[string]uint64
	noRoute  bool
	searches int
	other    int
	// missing answers as the route does for a session it does not hold.
	missing map[string]bool
	// hold keeps each search open until the client ends it or release closes. started gets one
	// value when a held search arrives, and ended one when the client ended it.
	hold           bool
	release        chan struct{}
	started, ended chan struct{}
}

func (f *fakeSearch) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

func (f *fakeSearch) counts() (searches, other int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searches, f.other
}

func (f *fakeSearch) client(t *testing.T) *apiclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sessions/{id}/search", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.searches++
		if f.noRoute {
			http.NotFound(w, r)
			return
		}
		id := r.PathValue("id")
		if f.missing[id] {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		if f.hold {
			f.mu.Unlock()
			f.started <- struct{}{}
			select {
			case <-r.Context().Done():
				f.ended <- struct{}{}
			case <-f.release:
			}
			f.mu.Lock()
			return
		}
		seqs := f.answer[id]
		if seqs == nil {
			seqs = []uint64{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"seqs": seqs, "lastSeq": f.last[id], "schemaVersion": 1})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.other++
		f.mu.Unlock()
		http.NotFound(w, r)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return apiclient.New(ts.URL)
}

// searchModel is an events pane of n request rows, seqs 1 to n, of session "s", against f, with
// the search prompt production builds.
func searchModel(t *testing.T, n int, f *fakeSearch) *model {
	t.Helper()
	resetSettingsForTest(t)
	events := eventSeq(n, "e")
	for i := range events {
		events[i].Seq = uint64(i + 1)
	}
	if f.last == nil {
		f.last = map[string]uint64{"s": uint64(n)}
	}
	m := newEventsPaneModel(events)
	m.ctx, m.client, m.serverProjects = context.Background(), f.client(t), true
	m.searchInput = newSearchInput()
	m.width, m.height = 200, 40
	m.eventColumns = defaultColumnSelection()
	m.layout()
	m.rebuildEventsTable()
	return m
}

// runAll runs a command and every command its messages lead to, through Update.
func runAll(m *model, cmd tea.Cmd) {
	for i := 0; cmd != nil && i < 100; i++ {
		_, cmd = m.Update(cmd())
	}
}

// detailsQuery opens the prompt on the events pane, turns the box on, and types q.
func detailsQuery(m *model, q string) {
	typeKeys(m, "/")
	press(m, tea.KeyTab)
	typeKeys(m, q)
}

func enter(m *model) tea.Cmd {
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return cmd
}

func key(m *model, r rune) { _, cmd := m.Update(keyRune(r)); runAll(m, cmd) }

func TestDetailSearch_TabTogglesTheBoxOnlyOnEvents(t *testing.T) {
	m := searchModel(t, 3, &fakeSearch{})
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

// Tab off in the prompt moves the cursor to the table's match, as typing does. Tab on puts it
// back where `/` was pressed.
func TestDetailSearch_TabOffFollowsTheTableMatch(t *testing.T) {
	m := searchModel(t, 5, &fakeSearch{})
	origin := m.eventsTbl.Cursor()
	detailsQuery(m, "e3")
	press(m, tea.KeyTab)
	if e := m.selectedEvent(); e == nil || e.Host != "e3" {
		t.Errorf("box off: cursor on %+v, want the row of host e3", e)
	}
	press(m, tea.KeyTab)
	if c := m.eventsTbl.Cursor(); c != origin {
		t.Errorf("box on: cursor on %d, want %d, where `/` was pressed", c, origin)
	}
}

// Enter lands on the first match at or after the row where `/` was pressed. n and N step and
// wrap with no new search. No event is fetched.
func TestDetailSearch_EnterLandsAndNSteps(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {3, 7}}}
	m := searchModel(t, 10, f)
	setCursorVisible(&m.eventsTbl, 1)
	detailsQuery(m, "needle")
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("typing moved the cursor to %d, want 1", c)
	}
	if st := m.searchStatus(paneEvents); st != "[/needle details: Enter to search]" {
		t.Errorf("status while typing = %q", st)
	}
	cmd := enter(m)
	if st := m.searchStatus(paneEvents); st != "[/needle details: searching…]" {
		t.Errorf("status while searching = %q", st)
	}
	runAll(m, cmd)
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Fatalf("Enter landed on %d, want 2 (seq 3)", c)
	}
	if st := m.searchStatus(paneEvents); st != "[/needle details 1/2]" {
		t.Errorf("status = %q", st)
	}
	for _, step := range []struct {
		key  rune
		want int
	}{{'n', 6}, {'n', 2}, {'N', 6}} {
		key(m, step.key)
		if c := m.eventsTbl.Cursor(); c != step.want {
			t.Errorf("%c landed on %d, want %d", step.key, c, step.want)
		}
	}
	if s, o := f.counts(); s != 1 || o != 0 {
		t.Errorf("searches %d, other requests %d, want 1 and 0", s, o)
	}
}

// A field the summary keeps matches with no help from the proxy.
func TestDetailSearch_TheSummaryMatchesLocally(t *testing.T) {
	m := searchModel(t, 6, &fakeSearch{})
	m.events["s"][4].Host = "needle-host.example"
	m.rebuildEventsTable()
	detailsQuery(m, "needle-host")
	runAll(m, enter(m))
	if c := m.eventsTbl.Cursor(); c != 4 {
		t.Errorf("cursor %d, want 4", c)
	}
}

func TestDetailSearch_NoMatch(t *testing.T) {
	m := searchModel(t, 4, &fakeSearch{})
	setCursorVisible(&m.eventsTbl, 2)
	detailsQuery(m, "absent")
	runAll(m, enter(m))
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Errorf("cursor %d, want 2", c)
	}
	if st := m.searchStatus(paneEvents); st != "[/absent details: no match]" {
		t.Errorf("status = %q", st)
	}
}

func TestDetailSearch_AProxyWithoutTheRoute(t *testing.T) {
	m := searchModel(t, 4, &fakeSearch{noRoute: true})
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	if st := m.searchStatus(paneEvents); st != "[/needle details: this Cortex has no details search]" {
		t.Errorf("status = %q", st)
	}
}

// An event newer than the answer's lastSeq makes an n that reaches it search again. With one
// match, n wraps past the new row.
func TestDetailSearch_NewEventsSearchAgain(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2}}}
	m := searchModel(t, 4, f)
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	key(m, 'n')
	if s, _ := f.counts(); s != 1 {
		t.Fatalf("searches %d, want 1: nothing new arrived", s)
	}
	more := eventSeq(1, "new")
	more[0].Seq = 5
	m.events["s"] = append(m.events["s"], more...)
	m.rebuildEventsTable()
	f.set(func() { f.last["s"] = 5 })
	key(m, 'n')
	if s, _ := f.counts(); s != 2 {
		t.Errorf("searches %d, want 2 after a new event", s)
	}
}

// A new event does not make every step wait for a pass: a step that lands on a known match
// before the new row moves at once, and only a step that reaches the new row searches again.
func TestDetailSearch_NewEventsSearchOnlyWhenReached(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2, 3}}}
	m := searchModel(t, 4, f)
	setCursorVisible(&m.eventsTbl, 0)
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Fatalf("Enter landed on %d, want 1 (seq 2)", c)
	}
	more := eventSeq(1, "new")
	more[0].Seq = 5
	m.events["s"] = append(m.events["s"], more...)
	m.rebuildEventsTable()
	key(m, 'n')
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Errorf("n landed on %d, want 2 (seq 3)", c)
	}
	if s, _ := f.counts(); s != 1 {
		t.Errorf("searches %d, want 1: the step did not reach the new row", s)
	}
	f.set(func() { f.last["s"] = 5 })
	key(m, 'n')
	if s, _ := f.counts(); s != 2 {
		t.Errorf("searches %d, want 2: the step reached the new row", s)
	}
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("after the new answer n landed on %d, want 1 (wrapped to seq 2)", c)
	}
}

// Esc while the search runs drops the answer. The cursor has not moved, and Esc does not
// leave the pane.
func TestDetailSearch_EscDropsTheAnswer(t *testing.T) {
	m := searchModel(t, 6, &fakeSearch{answer: map[string][]uint64{"s": {5}}})
	setCursorVisible(&m.eventsTbl, 1)
	detailsQuery(m, "needle")
	cmd := enter(m)
	if !m.detailSearching() {
		t.Fatal("Enter started no search")
	}
	press(m, tea.KeyEsc)
	if m.pane != paneEvents || m.detailSearch != nil {
		t.Fatalf("pane %v search %+v, want the events pane and no search", m.pane, m.detailSearch)
	}
	runAll(m, cmd)
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("cursor %d, want 1", c)
	}
}

func TestDetailSearch_LeavingThePaneDropsTheAnswer(t *testing.T) {
	m := searchModel(t, 6, &fakeSearch{answer: map[string][]uint64{"s": {5}}})
	setCursorVisible(&m.eventsTbl, 1)
	detailsQuery(m, "needle")
	cmd := enter(m)
	m.pane = paneSessions
	runAll(m, cmd)
	if m.detailSearch != nil {
		t.Errorf("search %+v, want none after the pane changed", m.detailSearch)
	}
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("cursor %d, want 1", c)
	}
}

// Matches older than the rows agentop holds are counted, for `o`.
func TestDetailSearch_CountsOlderMatches(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2, 13}}, last: map[string]uint64{"s": 15}}
	m := searchModel(t, 5, f)
	for i := range m.events["s"] {
		m.events["s"][i].Seq = uint64(i + 11)
	}
	m.rebuildEventsTable()
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	if st := m.searchStatus(paneEvents); st != "[/needle details 1/1] · 1 older (o)" {
		t.Errorf("status = %q", st)
	}
}

// A linked bucket's events keep their own session id and seqs. Each id is searched.
func TestDetailSearch_LinkedBucketIsSearchedByItsOwnID(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"bucket": {1}}, last: map[string]uint64{"s": 4, "bucket": 1}}
	m := searchModel(t, 4, f)
	m.events["s"][2].SessionID, m.events["s"][2].Seq = "bucket", 1
	m.rebuildEventsTable()
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	if e := m.selectedEvent(); e == nil || e.SessionID != "bucket" {
		t.Errorf("cursor on %+v, want the bucket's event", e)
	}
	if s, _ := f.counts(); s != 2 {
		t.Errorf("searches %d, want one for each session id", s)
	}
}

// Opening a row while the box is on gives the detail search the same query.
func TestDetailSearch_OpeningARowCarriesTheQuery(t *testing.T) {
	m := searchModel(t, 5, &fakeSearch{answer: map[string][]uint64{"s": {4}}})
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	press(m, tea.KeyEnter)
	if m.pane != paneDetail || m.search[paneDetail] != "needle" {
		t.Errorf("pane %v detail query %q, want detail and needle", m.pane, m.search[paneDetail])
	}
}

func TestDetailSearch_BoxOffKeepsTheTableSearch(t *testing.T) {
	f := &fakeSearch{}
	m := searchModel(t, 5, f)
	typeKeys(m, "/e3")
	press(m, tea.KeyEnter)
	key(m, 'n')
	if s, _ := f.counts(); s != 0 || m.detailSearch != nil {
		t.Errorf("box off: searches %d, search %+v, want neither", s, m.detailSearch)
	}
}

func TestDetailSearch_ResetClearsEverything(t *testing.T) {
	m := searchModel(t, 5, &fakeSearch{answer: map[string][]uint64{"s": {3}}})
	detailsQuery(m, "needle")
	cmd := enter(m)
	m.resetDetailSearch()
	if m.searchDetails || m.detailSearch != nil {
		t.Errorf("after reset: box %v search %+v", m.searchDetails, m.detailSearch)
	}
	runAll(m, cmd)
	if m.detailSearch != nil {
		t.Error("an answer from before the reset came back")
	}
}

// n or N while a search runs sends nothing more: a pass costs the proxy seconds of CPU.
func TestDetailSearch_NWhileSearchingSendsNothing(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {3}}}
	m := searchModel(t, 6, f)
	detailsQuery(m, "needle")
	cmd := enter(m)
	key(m, 'n')
	key(m, 'N')
	if s, _ := f.counts(); s != 0 {
		t.Errorf("searches %d while the first one ran, want 0", s)
	}
	runAll(m, cmd)
	if s, _ := f.counts(); s != 1 {
		t.Errorf("searches %d, want 1", s)
	}
}

// Esc ends the request itself, so the proxy stops its pass.
func TestDetailSearch_EscEndsTheRequest(t *testing.T) {
	f := &fakeSearch{hold: true, release: make(chan struct{}), started: make(chan struct{}, 1), ended: make(chan struct{}, 1)}
	m := searchModel(t, 4, f)
	t.Cleanup(func() { close(f.release) })
	detailsQuery(m, "needle")
	cmd := enter(m)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	<-f.started
	press(m, tea.KeyEsc)
	select {
	case <-f.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy's request went on after Esc")
	}
	<-done
}

// The rows `o` loads are older than what the answer covered. n searches again, and reaches them.
func TestDetailSearch_OlderPageMakesNSearchAgain(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2, 13}}, last: map[string]uint64{"s": 15}}
	m := searchModel(t, 5, f)
	for i := range m.events["s"] {
		m.events["s"][i].Seq = uint64(i + 11)
	}
	m.rebuildEventsTable()
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	older := eventSeq(10, "old")
	for i := range older {
		older[i].Seq = uint64(i + 1)
		older[i].At = m.events["s"][0].At.Add(-time.Duration(10-i) * time.Second)
	}
	m.events["s"] = append(older, m.events["s"]...)
	m.rebuildEventsTable()
	key(m, 'n')
	if s, _ := f.counts(); s != 2 {
		t.Fatalf("searches %d, want 2: the page loaded older rows", s)
	}
	if e := m.selectedEvent(); e == nil || e.Seq != 2 {
		t.Errorf("cursor on %+v, want seq 2", e)
	}
	if st := m.searchStatus(paneEvents); strings.Contains(st, "older") {
		t.Errorf("status %q still counts an older match agentop now holds", st)
	}
}

// Opening the prompt drops a search in flight, so its answer cannot move the cursor while the
// operator types.
func TestDetailSearch_OpeningThePromptDropsTheAnswer(t *testing.T) {
	m := searchModel(t, 6, &fakeSearch{answer: map[string][]uint64{"s": {5}}})
	setCursorVisible(&m.eventsTbl, 1)
	detailsQuery(m, "needle")
	cmd := enter(m)
	typeKeys(m, "/")
	runAll(m, cmd)
	if c := m.eventsTbl.Cursor(); c != 1 {
		t.Errorf("cursor %d, want 1: the answer landed while the prompt was open", c)
	}
}

// A linked bucket the proxy no longer holds gives no matches. It does not fail the search.
func TestDetailSearch_AMissingBucketDoesNotFailTheSearch(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2}}, last: map[string]uint64{"s": 4},
		missing: map[string]bool{"bucket": true}}
	m := searchModel(t, 4, f)
	m.events["s"][2].SessionID, m.events["s"][2].Seq = "bucket", 1
	m.rebuildEventsTable()
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	if e := m.selectedEvent(); e == nil || e.Seq != 2 || e.SessionID == "bucket" {
		t.Errorf("cursor on %+v, want seq 2 of s", e)
	}
	if st := m.searchStatus(paneEvents); st != "[/needle details 1/1]" {
		t.Errorf("status %q", st)
	}
	key(m, 'n')
	if s, _ := f.counts(); s != 2 {
		t.Errorf("searches %d, want 2 (one for each id), and no new search on n", s)
	}
}

// Leaving the events pane while a search runs ends the request, so the proxy stops its pass.
func TestDetailSearch_LeavingThePaneEndsTheRequest(t *testing.T) {
	for _, k := range []tea.KeyMsg{{Type: tea.KeyLeft}, keyRune('h'), {Type: tea.KeyEnter}} {
		t.Run(k.String(), func(t *testing.T) {
			f := &fakeSearch{hold: true, release: make(chan struct{}), started: make(chan struct{}, 1), ended: make(chan struct{}, 1)}
			m := searchModel(t, 4, f)
			t.Cleanup(func() { close(f.release) })
			detailsQuery(m, "needle")
			cmd := enter(m)
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			<-f.started
			m.Update(k)
			if m.pane == paneEvents {
				t.Fatalf("%s did not leave the events pane", k)
			}
			select {
			case <-f.ended:
			case <-time.After(5 * time.Second):
				t.Fatalf("the proxy's request went on after %s left the pane", k)
			}
			<-done
		})
	}
}

// An answer that landed stays when the operator opens a match and comes back: n steps on
// without a new pass.
func TestDetailSearch_ComingBackKeepsTheAnswer(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2, 4}}}
	m := searchModel(t, 5, f)
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	press(m, tea.KeyEnter)
	press(m, tea.KeyLeft)
	if m.pane != paneEvents {
		t.Fatalf("pane %v, want events", m.pane)
	}
	key(m, 'n')
	if s, _ := f.counts(); s != 1 {
		t.Errorf("searches %d, want 1", s)
	}
	if e := m.selectedEvent(); e == nil || e.Seq != 4 {
		t.Errorf("cursor on %+v, want seq 4", e)
	}
}

// A row that matches only in its capture opens with the capture open, on a highlighted match:
// when the capture is held, and when it arrives after the row opens. A match outside the capture
// leaves it closed.
func TestDetailSearch_AMatchOnlyInTheCaptureOpensIt(t *testing.T) {
	raw := &pipeline.RawCapture{Request: `{"system":"RAW-ONLY-NEEDLE"}`}
	t.Run("held", func(t *testing.T) {
		m := searchModel(t, 4, &fakeSearch{})
		m.markFullFetched("s", 3, raw)
		detailsQuery(m, "raw-only-needle")
		runAll(m, enter(m))
		if e := m.selectedEvent(); e == nil || e.Seq != 3 {
			t.Fatalf("cursor on %+v, want seq 3", e)
		}
		press(m, tea.KeyEnter)
		if len(m.detailDoc.matches) == 0 {
			t.Errorf("no match drawn, status %q", m.searchStatus(paneDetail))
		}
	})
	t.Run("fetched", func(t *testing.T) {
		m := searchModel(t, 4, &fakeSearch{answer: map[string][]uint64{"s": {3}}})
		detailsQuery(m, "raw-only-needle")
		runAll(m, enter(m))
		press(m, tea.KeyEnter)
		full := m.events["s"][2]
		m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s", seq: 3, event: &full, raw: raw})
		if len(m.detailDoc.matches) == 0 {
			t.Errorf("no match drawn, status %q", m.searchStatus(paneDetail))
		}
	})
	t.Run("outside the capture", func(t *testing.T) {
		m := searchModel(t, 4, &fakeSearch{})
		m.markFullFetched("s", 3, &pipeline.RawCapture{Request: `{"system":"RAW-ONLY-NEEDLE","host":"e2"}`})
		detailsQuery(m, "e2")
		runAll(m, enter(m))
		press(m, tea.KeyEnter)
		if _, open := m.detailFolds["request.raw"]; open || len(m.detailDoc.matches) == 0 {
			t.Errorf("raw fold %v, matches %d: want the capture closed, on the host's match", open, len(m.detailDoc.matches))
		}
	})
}

// A merged row is its request and its response (#1681): each half's text matches, a proxy match
// on the response's seq marks the row, a response that lands after the answer makes the row new,
// and a match only in the response's capture opens that capture.
func TestDetailSearch_AMergedRowMatchesByEitherHalf(t *testing.T) {
	req := &pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionRequest, RequestID: "r",
		Inference: &pipeline.InferenceExtension{Model: "m",
			Messages: []pipeline.InferenceMessage{{Role: "user", Content: "the NEEDLE-PROMPT"}}}}
	resp := &pipeline.SessionEvent{Seq: 2, Phase: pipeline.SessionResponse, RequestID: "r",
		Inference: &pipeline.InferenceExtension{Model: "m", Completion: "the reply"}}
	row := eventRow{event: req, resp: resp}
	for _, q := range []string{"needle-prompt", "THE REPLY"} {
		if !detailHas(row, q) {
			t.Errorf("detailHas(merged row, %q) = false, want true", q)
		}
	}

	f := &fakeSearch{answer: map[string][]uint64{"s": {2}}, last: map[string]uint64{"s": 2}}
	if msg := runDetailSearch(context.Background(), f.client(t), 0, "s", []eventRow{row}, "proxy-only"); !msg.hits[keyOf(req)] {
		t.Error("a proxy match on the response's seq did not mark the merged row")
	}

	m := searchModel(t, 1, &fakeSearch{})
	m.visibleRows = []eventRow{row}
	m.detailSearch = &detailSearch{hits: map[eventKey]bool{keyOf(req): false},
		last: map[string]uint64{"s": 1}, oldest: map[string]uint64{"s": 1}, newest: map[string]uint64{"s": 1}}
	if got := m.detailNewRows(); !slices.Equal(got, []int{0}) {
		t.Errorf("detailNewRows = %v, want [0]: the response landed after the answer", got)
	}

	m.detailSearch, m.searchDetails = nil, true
	m.search = map[paneID]string{paneEvents: "resp-raw-needle", paneDetail: "resp-raw-needle"}
	row.respRaw = &pipeline.RawCapture{Request: `{"id":"RESP-RAW-NEEDLE"}`}
	m.pane = paneDetail
	m.showDetail(row, true)
	m.openRawMatch()
	if c := m.detailFolds["response.raw"]; !c.open || len(m.detailDoc.matches) == 0 {
		t.Errorf("response.raw fold %+v, matches %d: want the response's capture open on the match", c, len(m.detailDoc.matches))
	}
}

// A details match far below the first screen is in view when the row opens: when the capture
// is held, and when it arrives after the row opens.
func TestDetailSearch_OpeningARowShowsADeepMatch(t *testing.T) {
	var keys []string
	for i := range 120 {
		keys = append(keys, fmt.Sprintf(`"k%03d":"v"`, i))
	}
	raw := &pipeline.RawCapture{Request: `{` + strings.Join(keys, ",") + `,"system":"DEEP-NEEDLE"}`}
	inView := func(t *testing.T, m *model) {
		t.Helper()
		rows := m.detailDoc.matches
		if len(rows) == 0 {
			t.Fatalf("no match drawn, status %q", m.searchStatus(paneDetail))
		}
		if off, h := m.detailVp.YOffset, m.detailVp.Height; rows[0] < off || rows[0] >= off+h {
			t.Errorf("match on row %d, view shows rows %d to %d", rows[0], off, off+h-1)
		}
	}
	t.Run("held", func(t *testing.T) {
		m := searchModel(t, 4, &fakeSearch{})
		m.markFullFetched("s", 3, raw)
		detailsQuery(m, "deep-needle")
		runAll(m, enter(m))
		press(m, tea.KeyEnter)
		inView(t, m)
	})
	t.Run("fetched", func(t *testing.T) {
		m := searchModel(t, 4, &fakeSearch{answer: map[string][]uint64{"s": {3}}})
		detailsQuery(m, "deep-needle")
		runAll(m, enter(m))
		press(m, tea.KeyEnter)
		full := m.events["s"][2]
		m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s", seq: 3, event: &full, raw: raw})
		inView(t, m)
	})
}

// A row hidden when the search ran is outside its answer. Shown again, n searches again and
// reaches it.
func TestDetailSearch_ARowShownAgainSearchesAgain(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {3}}}
	m := searchModel(t, 5, f)
	for i := range m.events["s"] {
		if i != 2 {
			m.events["s"][i].Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "p", Action: pipeline.ActionObserve}}}
		}
	}
	m.hideInactive = true
	m.rebuildEventsTable()
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	m.hideInactive = false
	m.rebuildEventsTable()
	key(m, 'n')
	if s, _ := f.counts(); s != 2 {
		t.Errorf("searches %d, want 2: a row the answer did not check is shown", s)
	}
	if e := m.selectedEvent(); e == nil || e.Seq != 3 {
		t.Errorf("cursor on %+v, want seq 3", e)
	}
}

// A row that the proxy read before agentop held it is a new row too: a step that does not reach
// it moves at once.
func TestDetailSearch_ARowTheProxyReadFirstSearchesOnlyWhenReached(t *testing.T) {
	f := &fakeSearch{answer: map[string][]uint64{"s": {2, 3}}, last: map[string]uint64{"s": 5}}
	m := searchModel(t, 4, f)
	setCursorVisible(&m.eventsTbl, 0)
	detailsQuery(m, "needle")
	runAll(m, enter(m))
	more := eventSeq(1, "new")
	more[0].Seq = 5
	m.events["s"] = append(m.events["s"], more...)
	m.rebuildEventsTable()
	key(m, 'n')
	if c := m.eventsTbl.Cursor(); c != 2 {
		t.Errorf("n landed on %d, want 2 (seq 3)", c)
	}
	if s, _ := f.counts(); s != 1 {
		t.Errorf("searches %d, want 1: the step did not reach the new row", s)
	}
	key(m, 'n')
	if s, _ := f.counts(); s != 2 {
		t.Errorf("searches %d, want 2: the step reached the new row", s)
	}
}
