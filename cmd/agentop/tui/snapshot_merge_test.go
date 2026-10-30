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
