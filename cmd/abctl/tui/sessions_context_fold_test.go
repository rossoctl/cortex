package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// THE FOLD MUST AGREE WITH A FULL RESCAN AT EVERY LENGTH, because the fold is the only thing that
// runs in production and the rescan is the definition.
//
// Why a fold at all: the sessions row loop asks for every session, rebuildSessionsTable runs on
// every streamed event, and retention is unbounded. Measured before this — 6.1ms and 3.49MB per
// call at 100k events, against ~14ns for the tail scan it replaced — ten sessions of that size
// cost 60ms and 35MB for one arriving event, on the pane abctl opens on.
func TestSessionContextFor_FoldMatchesAFullRescan(t *testing.T) {
	base := time.Now()
	const id = "s"
	m := &model{events: map[string][]pipeline.SessionEvent{}}

	var all []pipeline.SessionEvent
	for i := 0; i < 40; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		var next []pipeline.SessionEvent
		switch {
		case i%3 == 0:
			next = oneShot(fmt.Sprintf("o%d", i), at, 200_000+i) // a big one-shot, must never win
		case i%7 == 0:
			next = conversation(fmt.Sprintf("sub%d", i), at, 5, 30_000) // a short thread
		default:
			next = conversation(fmt.Sprintf("c%d", i), at, 100+i, 400_000+i)
		}
		all = append(all, next...)
		m.events[id] = all

		got := m.sessionContextFor(id, nil)
		want := pipeline.PromptContextOf(all)
		if got != want {
			t.Fatalf("after %d events: folded %d, rescan %d", len(all), got, want)
		}
	}
	// And the run really was folded rather than rescanned: every event has been accounted for.
	if run := m.contextRun[id]; run.Folded() != len(all) {
		t.Errorf("folded %d events, slice holds %d", run.Folded(), len(all))
	}
}

// A repeat call with nothing appended must not rescan. Asserted through the run's own counter,
// since that is what the length check reads.
func TestSessionContextFor_RepeatCallIsAHit(t *testing.T) {
	const id = "s"
	m := &model{events: map[string][]pipeline.SessionEvent{
		id: conversation("c1", time.Now(), 600, 500_000),
	}}

	first := m.sessionContextFor(id, nil)
	run := m.contextRun[id]
	if got := m.sessionContextFor(id, nil); got != first {
		t.Errorf("second call = %d, first = %d", got, first)
	}
	if m.contextRun[id] != run {
		t.Errorf("the run changed on a call that folded nothing: %+v -> %+v", run, m.contextRun[id])
	}
}

// A WHOLESALE REPLACEMENT OF THE SAME LENGTH is invisible to the length check, so the run is
// REBASED — the figure kept, the new slice folded on top of it. Driven through the message handler
// rather than the helper, because the handler is what has to call it.
//
// An earlier version of this test asserted the opposite: that the replacement dropped the run and
// the new events alone answered. That is what blanked the column for every session an operator
// opened, since a snapshot is projected and carries no candidate — see
// TestSessionContextFor_AProjectedSnapshotKeepsTheStreamsFigure. Rebasing is the fix, and these
// two cases are its two halves.
func TestSessionContextFor_AReplacementRebasesRatherThanDropping(t *testing.T) {
	base := time.Now()
	const id = "s"
	newModel := func() *model {
		m := &model{events: map[string][]pipeline.SessionEvent{
			id: conversation("c1", base, 600, 500_000),
		}}
		m.sessionsTbl = newSessionsTable()
		if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
			t.Fatalf("before the snapshot: %d, want %d", got, want)
		}
		return m
	}

	// A LONGER conversation in the replacement WINS, which is what "re-fold" buys over merely
	// re-basing the count: nothing here assumes a replacement is the poorer record.
	t.Run("a longer conversation in the replacement wins", func(t *testing.T) {
		m := newModel()
		m.Update(snapshotLoadedMsg{id: id,
			events: conversation("c2", base.Add(time.Hour), 900, 700_000)})
		if got, want := m.sessionContextFor(id, nil), 700_000; got != want {
			t.Errorf("after the snapshot: %d, want %d", got, want)
		}
	})

	// A SHORTER one does not WHERE NO ROLE IS STATED, and this is the knowingly-stale case. If the
	// server has evicted the pre-compaction request, a refetch carries only the short
	// post-compaction conversation and the gauge keeps the old figure — the same trade-off
	// pipeline.PromptContextOf documents for the live case, reached by a different route. A dash or a
	// subagent's figure is worse; the fix is the role, one subtest down.
	t.Run("a shorter one keeps the remembered figure", func(t *testing.T) {
		m := newModel()
		m.Update(snapshotLoadedMsg{id: id,
			events: conversation("c2", base.Add(time.Hour), 40, 62_000)})
		if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
			t.Errorf("after the snapshot: %d, want %d", got, want)
		}
	})

	// WITH THE ROLE STATED, A SHORTER-BUT-LATER REPLACEMENT WINS, and that is the whole fix seen
	// through the rebase rather than through the live fold: a refetch that carries only the
	// post-compaction conversation is not a poorer record of the session, it is a newer one.
	t.Run("a stated later turn in the replacement wins however short", func(t *testing.T) {
		m := newModel()
		m.Update(snapshotLoadedMsg{id: id,
			events: mainAgent("c2", base.Add(time.Hour), 17, 59_807)})
		if got, want := m.sessionContextFor(id, nil), 59_807; got != want {
			t.Errorf("after the snapshot: %d, want %d", got, want)
		}
	})

	// AND AN OLDER ONE STILL LOSES, which is what keeps `at` load-bearing once it is the sole
	// comparator: opening a turn from earlier in the session must not take the column from a
	// newer remembered figure.
	t.Run("a stated earlier turn in the replacement loses", func(t *testing.T) {
		m := &model{events: map[string][]pipeline.SessionEvent{
			id: mainAgent("c1", base.Add(time.Hour), 600, 500_000),
		}}
		m.sessionsTbl = newSessionsTable()
		if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
			t.Fatalf("before the snapshot: %d, want %d", got, want)
		}
		m.Update(snapshotLoadedMsg{id: id, events: mainAgent("c0", base, 900, 700_000)})
		if got, want := m.sessionContextFor(id, nil), 500_000; got != want {
			t.Errorf("after the snapshot: %d, want %d — an earlier turn took the column", got, want)
		}
	})
}

// Ties keep the LATEST turn, and the fold walks forward where the rescan it replaced walked back,
// so the comparison had to flip with it. A fold using `>` would report the earlier turn.
func TestFoldSessionContext_TiesKeepTheLatest(t *testing.T) {
	base := time.Now()
	evs := conversation("first", base, 700, 445_000)
	evs = append(evs, conversation("second", base.Add(time.Minute), 700, 448_000)...)

	if got, want := pipeline.PromptContextOf(evs), 448_000; got != want {
		t.Errorf("pipeline.PromptContextOf = %d, want %d", got, want)
	}
	// And the same answer when the two arrive in separate folds, which is the production path.
	m := &model{events: map[string][]pipeline.SessionEvent{"s": evs[:2]}}
	_ = m.sessionContextFor("s", nil)
	m.events["s"] = evs
	if got, want := m.sessionContextFor("s", nil), 448_000; got != want {
		t.Errorf("folded in two steps = %d, want %d", got, want)
	}
}
