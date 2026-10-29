package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// A pending bucket every request of which is linked to a session is not a row of its own;
// one with calls after its process's last claim still is.
func TestSessionsPane_HidesAFullyLinkedBucket(t *testing.T) {
	const all, part = "pending:claude-code@1.1", "pending:claude-code@2.2"
	m := uuidPicker(t, 200, uuidOpened, all, part)
	until := time.Now()
	for i := range m.sessions {
		switch m.sessions[i].ID {
		case all:
			m.sessions[i].LinkedTo, m.sessions[i].LinkedAll = []session.PendingLink{{Session: uuidOpened, First: until, Last: until}}, true
		case part:
			m.sessions[i].LinkedTo = []session.PendingLink{{Session: uuidOpened, First: until, Last: until}}
		}
	}
	m.rebuildSessionsTable()
	if !slices.Equal(m.sessionRowIDs, []string{uuidOpened, part}) {
		t.Errorf("rows = %v, want the session and the partly linked bucket only", m.sessionRowIDs)
	}
}

// Opening a session merges its linked calls into the timeline at the time they happened —
// not on top: for a resumed session they belong days after its first rows. Each keeps the
// bucket as its SessionID, which is where the detail pane fetches it from.
func TestSnapshot_MergesLinkedCallsByTime(t *testing.T) {
	const bucket = "pending:claude-code@1.1"
	t0 := time.Unix(1_700_000_000, 0).UTC()
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	views := map[string]pipeline.SessionView{
		"s1": {ID: "s1", Events: []pipeline.SessionEvent{
			{SessionID: "s1", Seq: 1, At: at(0), Phase: pipeline.SessionRequest, RequestID: "yesterday"},
			{SessionID: "s1", Seq: 2, At: at(100), Phase: pipeline.SessionRequest, RequestID: "today"},
		}},
		bucket: {ID: bucket, Events: []pipeline.SessionEvent{
			{SessionID: bucket, Seq: 1, At: at(50), Phase: pipeline.SessionRequest, RequestID: "mcp-init"},
			{SessionID: bucket, Seq: 2, At: at(51), Phase: pipeline.SessionResponse, RequestID: "mcp-init"},
			{SessionID: bucket, Seq: 3, At: at(200), Phase: pipeline.SessionRequest, RequestID: "after-claim"},
		}},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		v, ok := views[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer ts.Close()

	m := &model{ctx: context.Background(), client: apiclient.New(ts.URL), sessions: []session.SessionSummary{
		{ID: "s1"},
		{ID: bucket, LinkedTo: []session.PendingLink{{Session: "s1", First: at(100), Last: at(100)}}},
	}}
	msg := m.snapshotCmd("s1")().(snapshotLoadedMsg)
	var got []string
	for _, e := range msg.events {
		got = append(got, e.SessionID+"/"+e.RequestID)
	}
	want := []string{"s1/yesterday", bucket + "/mcp-init", bucket + "/mcp-init", "s1/today"}
	if !slices.Equal(got, want) {
		t.Errorf("timeline = %v, want %v", got, want)
	}
	if got := eventSession("s1", &msg.events[1]); got != bucket {
		t.Errorf("a linked event's detail is fetched from %q, want %q", got, bucket)
	}
}

// A session longer than one snapshot still shows the calls a resume linked into its newest
// page; one older than the page is left to the page that holds its time.
func TestInWindow_KeepsTheLinkedCallsTheSnapshotCovers(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	view := &pipeline.SessionView{TotalEvents: 5000, Events: []pipeline.SessionEvent{{At: t0.Add(time.Hour)}}}
	linked := []pipeline.SessionEvent{{RequestID: "old", At: t0}, {RequestID: "resume", At: t0.Add(2 * time.Hour)}}
	if got := ids(inWindow(linked, view)); !slices.Equal(got, []string{"resume"}) {
		t.Errorf("inWindow = %v, want only the call inside the page", got)
	}
	whole := &pipeline.SessionView{Events: view.Events}
	if got := ids(inWindow([]pipeline.SessionEvent{{RequestID: "old", At: t0}}, whole)); !slices.Equal(got, []string{"old"}) {
		t.Errorf("a whole session = %v, want every linked call", got)
	}
}

func ids(events []pipeline.SessionEvent) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.RequestID)
	}
	return out
}

// A bucket is folded only when it belongs to exactly one session: one linked to two (a /clear
// in its middle) stays a row, because its calls and its spend cannot be given to one row.
func TestFoldedInto(t *testing.T) {
	one := []session.PendingLink{{Session: "s1"}}
	two := []session.PendingLink{{Session: "s1"}, {Session: "s2"}}
	for _, tc := range []struct {
		s    session.SessionSummary
		want string
	}{
		{session.SessionSummary{ID: "pending:a@1.1", LinkedTo: one, LinkedAll: true}, "s1"},
		{session.SessionSummary{ID: "pending:a@1.1", LinkedTo: one}, ""},
		{session.SessionSummary{ID: "pending:a@1.1", LinkedTo: two, LinkedAll: true}, ""},
	} {
		if got := foldedInto(tc.s); got != tc.want {
			t.Errorf("foldedInto(%+v) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

// A folded bucket's spend and tokens are its session's row's: hidden, they would show nowhere.
func TestSessionsPane_AFoldedBucketsFiguresJoinItsSession(t *testing.T) {
	const bucket = "pending:bob@1.1"
	m := uuidPicker(t, 200, uuidOpened, bucket)
	for i := range m.sessions {
		m.sessions[i].CostMicros, m.sessions[i].TotalTokens = 1_000_000, 100
		if m.sessions[i].ID == bucket {
			m.sessions[i].LinkedTo, m.sessions[i].LinkedAll = []session.PendingLink{{Session: uuidOpened}}, true
		}
	}
	m.rebuildSessionsTable()
	row := m.sessionsTbl.Rows()[rowOf(t, m, uuidOpened)]
	if !slices.ContainsFunc(row, func(c string) bool { return strings.Contains(c, "$2.00") }) {
		t.Errorf("session row %v, want the bucket's $1.00 added to its own", row)
	}
}

// Paging back into a session merges the folded calls that fall in the older page, so a fresh
// launch's handshake is reachable in a session longer than one snapshot.
func TestOlderPage_MergesTheLinkedCallsItCovers(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	page := []pipeline.SessionEvent{{SessionID: "s1", RequestID: "p1", At: at(10)}, {SessionID: "s1", RequestID: "p2", At: at(20)}}
	linked := []pipeline.SessionEvent{{SessionID: "b", RequestID: "init", At: at(5)}, {SessionID: "b", RequestID: "later", At: at(500)}}
	got := ids(pageLinked(page, linked, true, at(30)))
	if !slices.Equal(got, []string{"init", "p1", "p2"}) {
		t.Errorf("oldest page = %v, want the handshake before it merged", got)
	}
	got = ids(pageLinked(page, linked, false, at(30)))
	if !slices.Equal(got, []string{"p1", "p2"}) {
		t.Errorf("a middle page = %v, want none: the later call is at or after the held window", got)
	}
}

// A linked call between an older page's last event and the oldest event already held goes on
// that page: the held window takes calls from its own oldest event on, so nothing else shows it.
func TestOlderPage_TakesTheLinkedCallsUpToTheHeldWindow(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	page := []pipeline.SessionEvent{{SessionID: "s1", RequestID: "p1", At: at(90)}, {SessionID: "s1", RequestID: "p2", At: at(100)}}
	linked := []pipeline.SessionEvent{
		{SessionID: "b", RequestID: "gap", At: at(150)},
		{SessionID: "b", RequestID: "held", At: at(200)},
	}
	if got := ids(pageLinked(page, linked, false, at(200))); !slices.Equal(got, []string{"p1", "p2", "gap"}) {
		t.Errorf("older page = %v, want the call between it and the held window", got)
	}
	tail := &pipeline.SessionView{Events: []pipeline.SessionEvent{{SessionID: "s1", At: at(200)}}, TotalEvents: 3}
	if got := ids(inWindow(slices.Clone(linked), tail)); !slices.Equal(got, []string{"held"}) {
		t.Errorf("tail window = %v, want only the call at its first event", got)
	}
}

// Erasing a session erases the buckets folded into it: the calls the operator saw inside it
// would otherwise come back as a row of their own.
func TestEraseTargets_IncludeTheBucketsFoldedIntoTheSession(t *testing.T) {
	m := &model{sessions: []session.SessionSummary{
		{ID: "s1"},
		{ID: "pending:a@1.1", LinkedTo: []session.PendingLink{{Session: "s1"}}, LinkedAll: true},
		{ID: "pending:a@2.2", LinkedTo: []session.PendingLink{{Session: "s1"}, {Session: "s2"}}, LinkedAll: true},
	}}
	if got := m.withFolded([]string{"s1"}); !slices.Equal(got, []string{"s1", "pending:a@1.1"}) {
		t.Errorf("withFolded = %v, want s1 and only the bucket folded into it alone", got)
	}
}

// linkedPagingServer serves s1 in pages by ?before and its one linked bucket whole.
func linkedPagingServer(t *testing.T, s1 []pipeline.SessionEvent, bucket pipeline.SessionView, total int) *apiclient.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		if id == bucket.ID {
			_ = json.NewEncoder(w).Encode(bucket)
			return
		}
		before, _ := strconv.ParseUint(r.URL.Query().Get("before"), 10, 64)
		var page []pipeline.SessionEvent
		for _, e := range s1 {
			if before == 0 || e.Seq < before {
				page = append(page, e)
			}
		}
		if before == 0 && len(page) > 2 {
			page = page[len(page)-2:]
		}
		_ = json.NewEncoder(w).Encode(pipeline.SessionView{ID: "s1", Events: page, TotalEvents: total, OldestSeq: s1[0].Seq})
	}))
	t.Cleanup(ts.Close)
	return apiclient.New(ts.URL)
}

// The older-events count is the session's own: linked calls merged into a snapshot or a page
// are not events the server still holds behind it. And [o] takes the linked call between the
// older page and the held window, which it can only do if loadOlderPage hands it the held time.
func TestOlderEvents_CountTheSessionsOwnAndPageTheGap(t *testing.T) {
	const bucket = "pending:claude-code@1.1"
	t0 := time.Unix(1_700_000_000, 0).UTC()
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	own := func(seq uint64, s int) pipeline.SessionEvent {
		return pipeline.SessionEvent{SessionID: "s1", Seq: seq, At: at(s), Phase: pipeline.SessionRequest, RequestID: fmt.Sprint("own-", seq)}
	}
	s1 := []pipeline.SessionEvent{own(1, 10), own(2, 20), own(3, 200), own(4, 210)}
	b := pipeline.SessionView{ID: bucket, Events: []pipeline.SessionEvent{
		{SessionID: bucket, Seq: 1, At: at(150), Phase: pipeline.SessionRequest, RequestID: "gap"},
		{SessionID: bucket, Seq: 2, At: at(205), Phase: pipeline.SessionRequest, RequestID: "resume"},
	}}
	m := fitModel(t, paneEvents, 200, 40, nil)
	m.ctx, m.client = context.Background(), linkedPagingServer(t, s1, b, len(s1))
	m.sessions = []session.SessionSummary{{ID: "s1"}, {ID: bucket, LinkedTo: []session.PendingLink{{Session: "s1", First: at(200), Last: at(210)}}}}
	m.selectedSess = "s1"

	snap := m.snapshotCmd("s1")().(snapshotLoadedMsg)
	if snap.olderNotFetched != 2 {
		t.Errorf("snapshot olderNotFetched = %d, want 2: the merged linked call is not one of them", snap.olderNotFetched)
	}
	m.Update(snap)
	cmd := m.loadOlderPage()
	if cmd == nil {
		t.Fatalf("no page request (flash %q)", m.flash)
	}
	page := cmd().(olderPageLoadedMsg)
	if got := ids(page.events); !slices.Equal(got, []string{"own-1", "own-2", "gap"}) {
		t.Errorf("older page = %v, want own-1, own-2 and the linked call before the held window", got)
	}
	m.Update(page)
	if n := m.olderNotFetched["s1"]; n != 0 {
		t.Errorf("after the last page olderNotFetched = %d, want 0", n)
	}
}

// A page that merges linked calls lowers the older count by the session's own events only.
func TestApplyOlderPage_CountsOnlyTheSessionsOwnEvents(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 3))
	m.olderNotFetched["sess-1"] = 5
	m.paging["sess-1"].wantGen = 3
	page := pagedEvents(6, 2)
	linked := page[0]
	linked.SessionID, linked.Seq = "pending:claude-code@1.1", 1
	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", gen: 3, events: append([]pipeline.SessionEvent{linked}, page...), serverOldest: 1})
	if n := m.olderNotFetched["sess-1"]; n != 3 {
		t.Errorf("olderNotFetched = %d, want 3: 5 less the page's 2 own events", n)
	}
}
