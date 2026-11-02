package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
)

// #1568: a snapshot that lands while events are streaming used to REPLACE
// the held list wholesale, silently dropping everything that arrived after
// the request was issued. The merge keeps held events with a higher seq.
func TestSnapshot_MergesStreamedEventsAfterItsTail(t *testing.T) {
	const id = "merge"
	m := newRetentionModel(t, id, 0)

	at := time.Now()
	streamed := make([]pipeline.SessionEvent, 0, 10)
	for i := 1; i <= 10; i++ {
		streamed = append(streamed, pipeline.SessionEvent{
			At: at.Add(time.Duration(i) * time.Millisecond), SessionID: id, Seq: uint64(i),
			Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h",
		})
	}
	m.events[id] = streamed

	// The snapshot answers everything the server held at request time: the
	// first eight. Events 9 and 10 streamed in while it was in flight.
	snapshot := make([]pipeline.SessionEvent, 0, 8)
	for i := 1; i <= 8; i++ {
		snapshot = append(snapshot, pipeline.SessionEvent{
			At: at.Add(time.Duration(i) * time.Millisecond), SessionID: id, Seq: uint64(i),
			Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h",
		})
	}
	m.Update(snapshotLoadedMsg{id: id, events: snapshot, serverOldest: 1})

	got := m.events[id]
	if len(got) != 10 {
		t.Fatalf("snapshot of 8 merged with 10 held events kept %d, want 10 — events streamed during the fetch were dropped", len(got))
	}
	if got[len(got)-1].Seq != 10 {
		t.Fatalf("last event seq = %d, want 10", got[len(got)-1].Seq)
	}
}

// A snapshot from a proxy that predates paging carries no seqs; unordered
// events cannot merge, so the snapshot lands whole, as before.
func TestSnapshot_ReplacesWhenSeqsAbsent(t *testing.T) {
	const id = "noseq"
	m := newRetentionModel(t, id, 0)

	at := time.Now()
	held := make([]pipeline.SessionEvent, 3)
	snap := make([]pipeline.SessionEvent, 2)
	for i := range held {
		held[i] = pipeline.SessionEvent{At: at, SessionID: id, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h"}
	}
	for i := range snap {
		snap[i] = pipeline.SessionEvent{At: at, SessionID: id, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h"}
	}
	m.events[id] = held
	m.Update(snapshotLoadedMsg{id: id, events: snap})

	if got := len(m.events[id]); got != 2 {
		t.Fatalf("seq-less snapshot stored %d events, want the snapshot's 2", got)
	}
}

// #1568: pausing dropped every streamed event, and resuming did not
// refetch — the timeline just stopped. Resuming now issues a snapshot for
// the watched session (which merges by seq, so nothing doubles).
func TestPause_ResumeRefetchesWatchedSession(t *testing.T) {
	const id = "resume"
	m := newRetentionModel(t, id, 0)

	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")}); cmd != nil {
		t.Fatal("pausing must not fetch anything")
	}
	if !m.paused {
		t.Fatal("p did not pause")
	}
	cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if m.paused {
		t.Fatal("second p did not resume")
	}
	if cmd == nil {
		t.Fatal("resuming must refetch the watched session's tail — events streamed while paused were dropped")
	}
}

// With nothing watched, resume has nothing to refetch and returns no cmd.
func TestPause_ResumeWithoutWatchedSession(t *testing.T) {
	m := newRetentionModel(t, "unused", 0)
	m.selectedSess = ""
	m.timeline.session = ""

	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")}); cmd != nil {
		t.Fatal("resume with no watched session must not fetch")
	}
}

func ownEv(id string, seq int, at time.Time) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At: at, SessionID: id, Seq: uint64(seq),
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h",
	}
}

// A snapshot's list is TIME-sorted, and mergeLinked interleaves linked-bucket
// events into it — events numbered in their OWN bucket's seq space. When a
// linked call is newer by time than the session's own tail, the last element
// is a bucket event, and keying the merge on its seq compares the session's
// own numbering against a bucket's: held events the snapshot already holds
// get appended a second time.
func TestSnapshot_MergeComparesOnlyOwnSessionSeqs(t *testing.T) {
	const id = "own"
	const bucket = "pending:claude@123.4"
	m := newRetentionModel(t, id, 0)

	at := time.Now()
	// The snapshot: the session's own first eight events, then a linked call
	// from the bucket that postdates the session's own tail — the shape
	// mergeLinked produces for it.
	snapshot := make([]pipeline.SessionEvent, 0, 9)
	for i := 1; i <= 8; i++ {
		snapshot = append(snapshot, ownEv(id, i, at.Add(time.Duration(i)*time.Millisecond)))
	}
	snapshot = append(snapshot, pipeline.SessionEvent{
		At: at.Add(9 * time.Millisecond), SessionID: bucket, Seq: 2,
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h",
	})
	// Held: the session's own events 6..10, streamed while the snapshot was
	// in flight — 6..8 are in the snapshot, 9 and 10 are not.
	held := make([]pipeline.SessionEvent, 0, 5)
	for i := 6; i <= 10; i++ {
		held = append(held, ownEv(id, i, at.Add(time.Duration(i)*time.Millisecond)))
	}
	m.events[id] = held

	m.Update(snapshotLoadedMsg{id: id, events: snapshot, serverOldest: 1})

	got := m.events[id]
	seen := map[uint64]bool{}
	for _, e := range got {
		if e.SessionID != id {
			continue
		}
		if seen[e.Seq] {
			t.Fatalf("own event seq %d appears twice after the merge — own seqs were compared against a linked bucket's", e.Seq)
		}
		seen[e.Seq] = true
	}
	if len(got) != 11 {
		t.Fatalf("merged list holds %d events, want 11 (8 own + 1 linked + streamed 9 and 10)", len(got))
	}
	if !seen[10] {
		t.Fatal("streamed event 10 was dropped by the merge")
	}
}

// Resuming the stream must not refetch while paged back: a snapshot IS the
// tail, so it ends the paging window and jumps the timeline there — [t] does
// that on purpose, an un-pause must not destroy the page the operator paused
// to read.
func TestPause_ResumeWhilePagedBackKeepsTheWindow(t *testing.T) {
	const id = "pagedback"
	m := newRetentionModel(t, id, 0)
	m.paging = map[string]*pagingState{id: {pageSizes: []int{10}, fetched: 1}}

	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if !m.paused {
		t.Fatal("p did not pause")
	}
	if cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")}); cmd != nil {
		t.Fatal("resume while paged back must not fetch — the snapshot would end the paging window and jump to the tail")
	}
	if m.paging[id] == nil {
		t.Fatal("the paging window did not survive the resume")
	}
}
