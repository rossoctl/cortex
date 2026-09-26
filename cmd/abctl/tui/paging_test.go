package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
)

// pagingEpoch is the base timestamp for fixture events, so a test can place one batch
// before or after another in wall-clock time independently of its Seq.
var pagingEpoch = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// pagedEvents builds n events carrying Seq from start, which paging needs as cursors, and
// timestamps that ascend with Seq — the normal case, where the two agree.
// cursorRowsFixture leaves Seq zero, which is what a proxy predating paging sends.
func pagedEvents(start, n int) []pipeline.SessionEvent {
	return pagedEventsAt(start, n, pagingEpoch.Add(time.Duration(start)*time.Second))
}

// pagedEventsAt is pagedEvents with the timestamps placed explicitly, for the case Seq and
// wall-clock time DISAGREE: a re-created session numbers its new events from 1 again.
func pagedEventsAt(start, n int, at time.Time) []pipeline.SessionEvent {
	out := make([]pipeline.SessionEvent, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, pipeline.SessionEvent{
			Seq:       uint64(start + i),
			At:        at.Add(time.Duration(i) * time.Second),
			SessionID: "sess-1",
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			Host:      fmt.Sprintf("h%04d", start+i),
			Inference: &pipeline.InferenceExtension{Model: "m"},
		})
	}
	return out
}

// pagedModel is a model already one page deep, as it stands after the first [o].
//
// It sets a client, which fitModel does not. Without one, loadOlderPage returns at its
// m.client == nil guard before reaching anything else — which made the request-stacking
// test below pass whether or not the behaviour it names existed.
func pagedModel(t *testing.T, held []pipeline.SessionEvent) *model {
	t.Helper()
	m := fitModel(t, paneEvents, 200, 40, held)
	m.client = apiclient.New("http://127.0.0.1:1")
	m.ctx = context.Background()
	m.paging = map[string]*pagingState{
		"sess-1": {pageSizes: []int{len(held)}},
	}
	m.olderNotFetched = map[string]int{"sess-1": 500}
	return m
}

func heldSeqs(m *model) []uint64 {
	out := make([]uint64, 0, len(m.events["sess-1"]))
	for _, e := range m.events["sess-1"] {
		out = append(out, e.Seq)
	}
	return out
}

// An older page goes in FRONT of what is held. Appending it would put older events after
// newer ones and present them as a timeline.
func TestApplyOlderPage_PrependsInOrder(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))

	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", events: pagedEvents(1, 10), serverOldest: 1})

	got := heldSeqs(m)
	if len(got) != 20 {
		t.Fatalf("held %d events, want 20: %v", len(got), got)
	}
	for i, seq := range got {
		if want := uint64(i + 1); seq != want {
			t.Fatalf("event %d has Seq %d, want %d — the page was not prepended in order", i, seq, want)
		}
	}
	// The count is decremented by what arrived, not recomputed from a total.
	if got, want := m.olderNotFetched["sess-1"], 490; got != want {
		t.Errorf("olderNotFetched = %d, want %d", got, want)
	}
}

// At the cap the NEWEST page is dropped, because paging backward moves the operator away
// from the tail — dropping the oldest would discard the page just fetched.
func TestApplyOlderPage_DropsTheNewestPageAtTheCap(t *testing.T) {
	// Start at the tail: Seq 301..400 is page one.
	m := pagedModel(t, pagedEvents(301, 100))

	// Three more pages, newest-to-oldest as [o] fetches them.
	for i, start := range []int{201, 101, 1} {
		m.applyOlderPage(olderPageLoadedMsg{
			id: "sess-1", events: pagedEvents(start, 100), serverOldest: 1,
		})
		if held := len(m.paging["sess-1"].pageSizes); held > maxPagesHeld {
			t.Fatalf("after %d pages, %d are held; cap is %d", i+2, held, maxPagesHeld)
		}
	}

	got := heldSeqs(m)
	if len(got) != maxPagesHeld*100 {
		t.Fatalf("held %d events, want %d", len(got), maxPagesHeld*100)
	}
	// The three OLDEST pages survive: 1..300. The tail page (301..400) is the one dropped.
	if got[0] != 1 {
		t.Errorf("oldest held Seq = %d, want 1 — the fetched pages should survive", got[0])
	}
	if last := got[len(got)-1]; last != 300 {
		t.Errorf("newest held Seq = %d, want 300 — the tail page should have been dropped", last)
	}
	if !m.paging["sess-1"].droppedNewer {
		t.Error("droppedNewer not set; the footer would not say the timeline stops short of the present")
	}
}

// While paged back, a streamed event must not be appended: it belongs at the end of the
// session, and the end is not what is on screen.
func TestHandleStreamEvent_DoesNotAppendWhilePagedBack(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	before := len(m.events["sess-1"])

	live := pagedEvents(21, 1)[0]
	m.handleStreamEvent(apiclient.StreamEvent{Event: &live})

	if got := len(m.events["sess-1"]); got != before {
		t.Errorf("held %d events after a streamed one arrived, want %d unchanged", got, before)
	}
	// The traffic still counts — the rate meter is about the proxy, not about the window.
	if m.eventCt != 1 {
		t.Errorf("eventCt = %d, want 1: a suppressed append is still an observed event", m.eventCt)
	}
}

// [t] keeps the paged window until its snapshot LANDS, and the snapshot is what clears it.
//
// Resuming appends at keypress instead looked friendlier and lost data: they landed on the
// paged window, which the snapshot then replaced wholesale. Holding the state costs the same
// events — the snapshot carries whatever the server had when it built the response — and
// keeps the failure case honest, which the test below covers.
func TestReturnToTail_KeepsTheWindowUntilTheSnapshotLands(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	m.returnToTail()

	if !m.pagedBack("sess-1") {
		t.Fatal("paging state dropped before the snapshot landed")
	}
	live := pagedEvents(21, 1)[0]
	m.handleStreamEvent(apiclient.StreamEvent{Event: &live})
	if got := len(m.events["sess-1"]); got != 10 {
		t.Errorf("held %d events, want 10 — an event was appended to a window about to be replaced", got)
	}

	// The snapshot completes the return: window replaced, appends resumed.
	m.Update(snapshotLoadedMsg{id: "sess-1", events: pagedEvents(50, 3), olderNotFetched: 47})
	if m.pagedBack("sess-1") {
		t.Fatal("still paged back after the snapshot landed")
	}
	m.handleStreamEvent(apiclient.StreamEvent{Event: &live})
	if got := len(m.events["sess-1"]); got != 4 {
		t.Errorf("held %d events, want 4 — appends did not resume", got)
	}
}

// A FAILED tail snapshot must leave the window exactly as it was, and re-arm [t].
//
// The alternative is the failure this whole branch keeps arguing against: state cleared,
// live events splicing onto a page from the middle of the session, and nothing saying so.
func TestReturnToTail_FailedSnapshotLeavesTheWindowDescribed(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	m.returnToTail()

	m.Update(errMsg{where: "snapshot sess-1", err: errors.New("connection refused")})

	if !m.pagedBack("sess-1") {
		t.Fatal("paging state dropped by a FAILED snapshot; the timeline would silently resume mid-session")
	}
	if m.paging["sess-1"].returning {
		t.Error("still marked as returning, so [t] would refuse to retry")
	}
	if got := m.helpView(); !strings.Contains(got, "paged back") {
		t.Errorf("footer stopped reporting the paged-back window: %q", got)
	}
	// And [t] works on the second attempt.
	if cmd := m.returnToTail(); cmd == nil {
		t.Error("[t] did not retry after the failure")
	}
}

// [o] against a proxy that stamps no Seq must not enter paging mode at all.
//
// It used to create the state before validating the cursor, so one keypress suspended live
// appends for a session that could never page — the flash explained the refusal while the
// timeline quietly stopped moving.
func TestLoadOlderPage_WithoutSeqDoesNotEnterPagingMode(t *testing.T) {
	m := pagedModel(t, cursorRowsFixture(3)) // Seq zero throughout
	delete(m.paging, "sess-1")               // start from the live tail

	if cmd := m.loadOlderPage(); cmd != nil {
		t.Error("issued a page request against events carrying no Seq")
	}
	if m.pagedBack("sess-1") {
		t.Fatal("entered paging mode anyway, which suppresses live appends for good")
	}
	if !strings.Contains(m.flash, "too old") {
		t.Errorf("flash = %q, want it to name the reason", m.flash)
	}
	// Appends still work, which is the consequence that matters.
	live := pagedEvents(21, 1)[0]
	m.handleStreamEvent(apiclient.StreamEvent{Event: &live})
	if got := len(m.events["sess-1"]); got != 4 {
		t.Errorf("held %d events, want 4 — live appends were suppressed", got)
	}
}

// A first page that comes back empty tears the tentative state down, for the same reason.
func TestApplyOlderPage_EmptyFirstPageAbandonsPaging(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	m.paging["sess-1"].wantGen = 7

	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", gen: 7, serverOldest: 11})

	if m.pagedBack("sess-1") {
		t.Error("state survived a page that loaded nothing, suppressing live appends")
	}
}

// A failed fetch clears the in-flight marker so [o] can be retried, and must not touch the
// SSE connection state — a page request and the event stream are different connections.
func TestFailOlderPage_ClearsLoadingAndSparesTheConnection(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	m.paging["sess-1"].fetched = 1 // an earlier page succeeded, so the state is not tentative
	m.connState.phase = connOpen

	cmd := m.loadOlderPage()
	if cmd == nil {
		t.Fatal("no request issued")
	}
	gen := m.paging["sess-1"].wantGen
	m.Update(olderPageFailedMsg{id: "sess-1", gen: gen, err: errors.New("i/o timeout")})

	if m.paging["sess-1"].loading {
		t.Error("loading still set, so [o] would refuse every retry")
	}
	if m.connState.phase != connOpen {
		t.Errorf("connection phase = %v, want it untouched: one failed page is not a dead stream",
			m.connState.phase)
	}
	if !strings.Contains(m.flash, "older page failed") {
		t.Errorf("flash = %q, want it to name the failure", m.flash)
	}
	if cmd := m.loadOlderPage(); cmd == nil {
		t.Error("[o] did not retry after the failure")
	}
}

// A response from a superseded request is dropped.
//
// The path: [o] is in flight, [t] tears the state down, the tail lands, a fresh [o] builds
// new state — and only then does the first response arrive. A SINGLE-event page is used
// deliberately, because that is the case the timestamp ordering guard cannot catch: one
// event's first and last timestamps are equal, so nothing about it looks out of order.
func TestApplyOlderPage_DropsAStaleGeneration(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	m.paging["sess-1"].fetched = 1

	if cmd := m.loadOlderPage(); cmd == nil {
		t.Fatal("no first request issued")
	}
	stale := m.paging["sess-1"].wantGen

	// [t], its snapshot, then a fresh [o] — the new state expects a later generation.
	m.returnToTail()
	m.Update(snapshotLoadedMsg{id: "sess-1", events: pagedEvents(11, 10), olderNotFetched: 10})
	if cmd := m.loadOlderPage(); cmd == nil {
		t.Fatal("no second request issued")
	}
	if m.paging["sess-1"].wantGen == stale {
		t.Fatal("the new request reused the stale generation; it could not be told apart")
	}

	before := len(m.events["sess-1"])
	m.applyOlderPage(olderPageLoadedMsg{
		id: "sess-1", gen: stale, events: pagedEventsAt(10, 1, pagedEvents(11, 1)[0].At),
	})
	if got := len(m.events["sess-1"]); got != before {
		t.Errorf("held %d events, want %d — a stale response was stitched in", got, before)
	}
}

// olderNotFetched is not always allocated: the snapshot handler builds it lazily and a pod
// switch sets it to nil. A page landing for a session whose events came over the stream
// therefore used to write to a nil map, which panics and takes the TUI down.
func TestApplyOlderPage_SurvivesAnUnallocatedOlderCount(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	m.olderNotFetched = nil // as it stands after a pod switch, or with no snapshot yet

	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", events: pagedEvents(1, 10), serverOldest: 1})

	if got := len(m.events["sess-1"]); got != 20 {
		t.Errorf("held %d events, want 20", got)
	}
	if got := m.olderNotFetched["sess-1"]; got != 0 {
		t.Errorf("olderNotFetched = %d, want 0 from an absent baseline", got)
	}
}

// A page landing for a session whose events were RELEASED must be dropped, not stitched in.
//
// Switching sessions frees the events of the ones the server still lists, and the paging
// state has to go with them. Left behind, a page still in flight would land with no events
// held: the ordering guard has nothing to compare, the released session is resurrected, and
// pageSizes starts describing pages that no longer exist — which is how a later cap drop
// computes a negative slice bound and panics.
func TestApplyOlderPage_DropsAPageWhoseEventsWereReleased(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	delete(m.events, "sess-1") // what a session switch does

	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", events: pagedEvents(1, 10), serverOldest: 1})

	if got := len(m.events["sess-1"]); got != 0 {
		t.Errorf("resurrected %d events for a released session", got)
	}
	if m.pagedBack("sess-1") {
		t.Error("paging state survived the release, so it would keep describing a window that is gone")
	}
}

// The cap slices by a RECORDED page size, so a size larger than what is held must not
// compute a negative bound. The guard above removes the way that became reachable; this
// pins the arithmetic, because the cost of being wrong is the whole TUI.
func TestApplyOlderPage_ClampsAnOversizedPageRecord(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	// A state whose bookkeeping already disagrees with reality.
	m.paging["sess-1"].pageSizes = []int{10, 10, 500}

	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", events: pagedEvents(1, 5), serverOldest: 1})

	if len(m.paging["sess-1"].pageSizes) > maxPagesHeld {
		t.Errorf("pageSizes = %v, over the cap", m.paging["sess-1"].pageSizes)
	}
}

// A page can land after the operator has left the session or returned to the tail. It has
// to be dropped: the window it would extend no longer exists.
func TestApplyOlderPage_IgnoresAPageWhosePagingEnded(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	delete(m.paging, "sess-1")

	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", events: pagedEvents(1, 10), serverOldest: 1})

	if got := len(m.events["sess-1"]); got != 10 {
		t.Errorf("held %d events, want the original 10 — a stale page was stitched in", got)
	}
}

// A second [o] while one request is in flight is a no-op, so holding the key down cannot
// stack fetches that each cost a page of decode.
//
// The positive case is asserted first, and that is the point: without it this test passed
// against a model with no client at all, where loadOlderPage returns nil for a reason that
// has nothing to do with in-flight requests.
func TestLoadOlderPage_DoesNotStackRequests(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))

	if cmd := m.loadOlderPage(); cmd == nil {
		t.Fatal("the first request was not issued, so this test cannot show anything about the second")
	}
	if !m.paging["sess-1"].loading {
		t.Fatal("loading not set by the first request")
	}

	if cmd := m.loadOlderPage(); cmd != nil {
		t.Error("a second request was issued while one was in flight")
	}
}

// A page whose events are NEWER than what is held must be refused rather than prepended.
//
// Reachable because Seq restarts at 1 for a re-created session: TTL cleanup or max_sessions
// eviction drops the entry, traffic under the same id makes a new one, and a cursor from
// the previous incarnation is then above everything held. ViewPage answers "everything
// before that cursor" — the whole new session — and prepending it would put its newest
// events in front of the older ones already on screen.
func TestApplyOlderPage_RefusesAPageThatIsNotOlder(t *testing.T) {
	m := pagedModel(t, pagedEvents(3000, 10)) // held: Seq 3000..3009
	before := heldSeqs(m)

	// What a re-created session returns: Seq 1..5, numbered far BELOW the cursor that asked
	// for them — so a Seq comparison sees a well-ordered page — but recorded an hour after
	// everything held, because the store dropped the session and started over.
	newer := pagedEventsAt(1, 5, m.events["sess-1"][0].At.Add(time.Hour))
	m.applyOlderPage(olderPageLoadedMsg{id: "sess-1", events: newer, serverOldest: 1})

	if got := heldSeqs(m); len(got) != len(before) {
		t.Errorf("held %d events, want %d unchanged — an out-of-order page was stitched in",
			len(got), len(before))
	}
	if !strings.Contains(m.flash, "restarted") {
		t.Errorf("flash = %q, want it to name the reason", m.flash)
	}
}

// The dropped page must be cleared, not just sliced off. Reslicing leaves those events
// reachable through the backing array, so the cap would hold one page more than it claims.
func TestApplyOlderPage_ClearsTheDroppedPage(t *testing.T) {
	m := pagedModel(t, pagedEvents(301, 100))
	for _, start := range []int{201, 101, 1} {
		m.applyOlderPage(olderPageLoadedMsg{
			id: "sess-1", events: pagedEvents(start, 100), serverOldest: 1,
		})
	}

	held := m.events["sess-1"]
	// The dropped page sits just past the length, inside the same backing array.
	spare := held[:len(held)+100]
	for i := len(held); i < len(spare); i++ {
		if spare[i].Inference != nil || spare[i].Seq != 0 {
			t.Fatalf("dropped event at %d is still live (Seq %d): the cap holds more than it says",
				i, spare[i].Seq)
		}
	}
}

// The footer has to say that live updates are suspended. A timeline that silently stopped
// updating is the same class of lie as one that silently omitted its beginning.
func TestFooter_SaysWhenPagedBack(t *testing.T) {
	m := pagedModel(t, pagedEvents(11, 10))
	m.rebuildEventsTable()

	got := m.helpView()
	if !strings.Contains(got, "live paused") || !strings.Contains(got, "[t]") {
		t.Errorf("footer does not report the paged-back state: %q", got)
	}

	m.paging["sess-1"].droppedNewer = true
	if got := m.helpView(); !strings.Contains(got, "newer dropped") {
		t.Errorf("footer does not report that newer events were dropped: %q", got)
	}
}
