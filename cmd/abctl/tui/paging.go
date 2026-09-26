package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/cmd/abctl/apiclient"
)

// maxPagesHeld bounds how many pages of one session abctl keeps stitched together.
//
// There is a bound at all because a page is not small: one page of a real session is
// ~175MB of JSON on the wire, and abctl reached 1.64GB — more than the proxy it was
// watching — before its decode path was fixed. Unbounded paging would put that growth back
// under a key the operator can hold down.
//
// Three, so there is a page of context on either side of the one being read. When a fourth
// arrives the NEWEST is dropped, not the oldest: paging backward means the operator is
// moving away from the tail, so the tail is the page furthest from what they are looking
// at. Dropping the oldest would discard the page just fetched and make the key a no-op.
const maxPagesHeld = 3

// pagingState is what a session's timeline needs once the operator has paged off its tail.
type pagingState struct {
	// pageSizes is the event count of each page currently stitched into m.events[id],
	// OLDEST page first. The events carry no page boundary of their own, so this is how
	// the newest page is found again when the cap evicts one.
	pageSizes []int

	// serverOldest is the Seq of the oldest event the server still holds, from the most
	// recent response. Reaching it means there is nothing left to ask for.
	serverOldest uint64

	// loading is set while a page request is in flight, so holding [o] down cannot stack
	// requests — each one costs a page of decode.
	loading bool

	// wantGen is the request generation this state will accept a response for, taken from
	// the model's monotonic counter. A response from any other generation is stale and
	// dropped — see applyOlderPage.
	wantGen uint64

	// fetched counts pages successfully stitched in. Zero means this state is TENTATIVE:
	// [o] created it but no older page ever arrived, so it must be torn down rather than
	// left suppressing live appends for a timeline that never moved.
	fetched int

	// returning is set while [t]'s tail snapshot is in flight. The state deliberately
	// survives until that snapshot LANDS, so a failed fetch leaves the window described
	// honestly rather than silently resuming appends onto a middle page.
	returning bool

	// droppedNewer records that the cap has evicted at least one page from the newer end,
	// so the footer can say the timeline no longer reaches the present. Without it the
	// operator would see a timeline that simply stops, which is the failure this whole
	// change exists to stop doing.
	droppedNewer bool
}

// pagedBack reports whether the session's timeline has been walked off its tail. Live
// streamed events are not appended while it is true — see handleStreamEvent.
func (m *model) pagedBack(id string) bool {
	return m.paging[id] != nil
}

// loadOlderPage is [o]: fetch the page immediately before the oldest event held.
//
// Suspending the live stream for this session is deliberate, and it is why paging has state
// at all rather than just fetching. Streamed events append to the END of the timeline; once
// the operator is reading a page from the middle of a long session, appending live events
// would leave a gap between what they are reading and what just arrived, presented as one
// continuous list. Better to stop extending it and say so.
func (m *model) loadOlderPage() tea.Cmd {
	id := m.selectedSess
	if id == "" || m.client == nil {
		return nil
	}
	held := m.events[id]
	if len(held) == 0 {
		return nil
	}

	st := m.paging[id] // nil until the first page actually lands
	if st != nil && (st.loading || st.returning) {
		return nil
	}

	// EVERY reason to refuse is checked before any state is created. Creating it first and
	// validating after left a session paged-back — live appends suppressed, timeline
	// silently frozen — after a single [o] that could never have fetched anything.
	oldest := held[0].Seq
	if oldest == 0 {
		// A proxy that predates Seq. Paging cannot work against it, and saying so beats
		// sending before=0 and silently refetching the tail forever.
		m.setFlash("this proxy is too old to page back")
		return nil
	}
	if st != nil && st.serverOldest != 0 && oldest <= st.serverOldest {
		m.setFlash("at the beginning of the session")
		return nil
	}

	if st == nil {
		// The events already held are page one, whatever mixture of snapshot and streamed
		// events they are. Tentative until a page arrives: fetched stays 0, and
		// applyOlderPage tears this down if nothing comes back.
		st = &pagingState{pageSizes: []int{len(held)}}
		if m.paging == nil {
			m.paging = map[string]*pagingState{}
		}
		m.paging[id] = st
	}

	// One counter for the whole model, not per state: a fresh state after [t] would start
	// its own generation at the same number an in-flight request already carries, and the
	// stale response would be accepted as current.
	m.pageGen++
	st.wantGen = m.pageGen
	st.loading = true
	m.setFlash("loading older…")
	return m.olderPageCmd(id, oldest, st.wantGen)
}

func (m *model) olderPageCmd(id string, before, gen uint64) tea.Cmd {
	return func() tea.Msg {
		view, err := m.client.GetSessionPage(m.ctx, id, before, apiclient.SnapshotEventLimit)
		if err != nil {
			// Its OWN message type, not errMsg. errMsg's handler decides severity by
			// string-prefixing `where`, and anything it does not recognise flips the whole
			// view into a terminal connection failure — so one failed page reported a dead
			// stream that was in fact healthy, and left loading set, refusing every retry.
			return olderPageFailedMsg{id: id, gen: gen, err: err}
		}
		return olderPageLoadedMsg{
			id: id, gen: gen, events: view.Events, serverOldest: view.OldestSeq,
		}
	}
}

// failOlderPage clears the in-flight marker so [o] can be retried, and tears the state down
// if this was the request that created it.
func (m *model) failOlderPage(msg olderPageFailedMsg) {
	st := m.paging[msg.id]
	if st == nil || st.wantGen != msg.gen {
		return
	}
	st.loading = false
	m.setFlash("older page failed: " + msg.err.Error())
	m.abandonIfTentative(msg.id, st)
}

// abandonIfTentative removes paging state that never managed to load a page, so live
// appends resume for a timeline that never actually moved off its tail.
func (m *model) abandonIfTentative(id string, st *pagingState) {
	if st.fetched == 0 {
		delete(m.paging, id)
	}
}

// applyOlderPage stitches a fetched page onto the front of the timeline.
func (m *model) applyOlderPage(msg olderPageLoadedMsg) {
	st := m.paging[msg.id]
	if st == nil {
		// The operator returned to the tail, or left the session, while this was in
		// flight. Dropping it is correct: the state it would extend is gone.
		return
	}
	// A response from a superseded request. Reachable: [t] tears the state down while a
	// page is still in flight, the tail snapshot lands, and a fresh [o] builds new state —
	// at which point the old response would be stitched in as if it were current. On a
	// single-event page the timestamp guard below cannot catch that (one event's first and
	// last timestamps are equal), so the generation is what makes it impossible.
	if msg.gen != st.wantGen {
		return
	}
	st.loading = false
	st.serverOldest = msg.serverOldest

	if len(msg.events) == 0 {
		m.setFlash("at the beginning of the session")
		m.abandonIfTentative(msg.id, st)
		return
	}

	held := m.events[msg.id]

	// The window this state described is gone — the session's events were released while
	// this page was in flight. Everything below assumes held and pageSizes agree, and with
	// held empty they cannot: the ordering guard has nothing to compare against, and
	// pageSizes would keep describing pages that no longer exist, until a recorded size
	// exceeded len(merged) and the cap's slice bound went negative. Resurrecting a released
	// session's events would also undo the release.
	if len(held) == 0 {
		delete(m.paging, msg.id)
		return
	}

	// The page must actually be older than what is held, and the test for that is the
	// TIMESTAMP, not Seq.
	//
	// Seq is per store entry and restarts at 1: a session dropped by TTL cleanup or the
	// max_sessions eviction, then re-created under the same id, numbers its new events from
	// the beginning. A cursor from the previous incarnation is then above everything the
	// store now holds, so ViewPage finds nothing at or after it and answers with the tail —
	// the re-created session's NEWEST events, carrying low Seq values. Comparing Seq would
	// find that perfectly ordered and prepend live traffic to the front of the timeline.
	// Wall-clock time is what stays comparable across the two incarnations.
	//
	// Refused here rather than by teaching ViewPage to reject high cursors, because the store
	// cannot tell a re-created session from a trimmed one, and this check does not need to.
	if len(held) > 0 && msg.events[len(msg.events)-1].At.After(held[0].At) {
		m.setFlash("session restarted — [t] for the tail")
		m.abandonIfTentative(msg.id, st)
		return
	}

	// A fresh slice, oldest first: appending to the held events would put the older page
	// after the newer ones, and growing the page in place would alias the response.
	merged := make([]pipeline.SessionEvent, 0, len(msg.events)+len(held))
	merged = append(merged, msg.events...)
	merged = append(merged, held...)
	st.pageSizes = append([]int{len(msg.events)}, st.pageSizes...)

	for len(st.pageSizes) > maxPagesHeld {
		newest := st.pageSizes[len(st.pageSizes)-1]
		st.pageSizes = st.pageSizes[:len(st.pageSizes)-1]
		// Clamped, because this loop slices by a RECORDED size and a recorded size larger
		// than what is held would compute a negative low bound and panic. The guard above
		// removes the way that was reachable; this makes the arithmetic safe regardless,
		// since the cost of being wrong here is taking the whole TUI down.
		if newest > len(merged) {
			newest = len(merged)
		}
		// Cleared before the truncate, not just sliced away. Reslicing leaves the dropped
		// events reachable through the backing array, so the three-page cap would hold four
		// pages' worth of prompts and completions until the next page reallocated — in a
		// change whose whole point is retained heap.
		clear(merged[len(merged)-newest:])
		merged = merged[:len(merged)-newest]
		st.droppedNewer = true
	}
	m.events[msg.id] = merged
	// Older events land BEFORE the ones already folded, and the page cap above can drop
	// newer ones off the end, so the running answer cannot simply be extended. It is
	// re-folded over the merged slice rather than dropped: these pages are projected, and
	// so is anything the cap just discarded — see rebaseSessionContext.
	m.rebaseSessionContext(msg.id, merged)
	st.fetched++

	// The count is decremented rather than recomputed from the server's total, because
	// the total counts events at BOTH ends: once the cap has dropped a newer page,
	// total-minus-held would report those as older and send the operator looking for them
	// in the wrong direction. Pages are contiguous going back, so subtracting what just
	// arrived is exact — give or take events appended since, which only ever makes this
	// an undercount of what is now available.
	//
	// Created if absent, because writing to a nil map panics and this map is not always
	// there: it is built lazily by the snapshot handler and set to nil outright on a pod
	// switch, so a session whose events arrived over the stream — or whose snapshot failed
	// — reaches here with nothing allocated.
	if m.olderNotFetched == nil {
		m.olderNotFetched = map[string]int{}
	}
	if n := m.olderNotFetched[msg.id] - len(msg.events); n > 0 {
		m.olderNotFetched[msg.id] = n
	} else {
		m.olderNotFetched[msg.id] = 0
	}
	m.setFlash("")
}

// returnToTail is [t]: refetch the newest page, which replaces the paged window and resumes
// live appends for this session.
//
// The paging state is kept until that snapshot LANDS — the snapshotLoadedMsg handler is what
// clears it. Clearing it here instead looked better and was not: appends resumed onto the
// paged window, the snapshot then replaced that window wholesale, so events arriving in
// between were appended and immediately discarded — and if the snapshot FAILED, the state
// was already gone, leaving live events splicing onto a page from the middle of the session
// with nothing in the footer saying so. Holding the state costs the same events (the
// snapshot carries everything the server had when it built the response, and anything later
// is lost either way) and keeps a failure honest and retryable.
func (m *model) returnToTail() tea.Cmd {
	id := m.selectedSess
	st := m.paging[id]
	if id == "" || st == nil || st.returning {
		return nil
	}
	st.returning = true
	m.setFlash("returning to the live tail…")
	return m.snapshotCmd(id)
}

// tailReturnFailed re-arms [t] after its snapshot failed, leaving the paged window and its
// footer notice exactly as they were.
func (m *model) tailReturnFailed(id string) {
	if st := m.paging[id]; st != nil {
		st.returning = false
	}
}
