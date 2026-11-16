package archive

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

var _ session.PriorKeeper = (*Archive)(nil)

func listed(t *testing.T, st *session.Store, id string) session.SessionSummary {
	t.Helper()
	for _, s := range st.ListSessions() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("%s is not listed", id)
	return session.SessionSummary{}
}

// THE POINT OF THE CHANGE, end to end: a session recorded through a store and its archive, both
// closed, then resumed under a new store over the reopened archive, lists as the whole session —
// its events, tokens and title from before the restart, plus what it did after.
func TestPrior_ARestartedSessionListsAsTheWholeSession(t *testing.T) {
	evs := synthSession(71, 4, 512)
	ref := session.New(0, 0, 0)
	defer ref.Close()
	for _, e := range evs {
		ref.Append("s1", e)
	}
	want := listed(t, ref, "s1")
	if want.Title == "" || want.TotalTokens == 0 {
		t.Fatalf("fixture exercises nothing: %+v", want)
	}

	a, st := restarted(t, t.TempDir(), newClock(), "s1", evs[:len(evs)/2])
	defer a.Close()
	defer st.Close()
	for _, e := range evs[len(evs)/2:] {
		st.Append("s1", e)
	}
	settle(a)
	got := listed(t, st, "s1")
	if got.EventCount != want.EventCount || got.TotalTokens != want.TotalTokens || got.Title != want.Title {
		t.Fatalf("after a restart the session lists as\n %+v\nnot as one that never restarted\n %+v", got, want)
	}
}

// Prior answers for exactly the entry whose start the writer has marked, and for no other: not
// before the writer takes the entry's first event, not for a different seq, not for an id it does
// not hold.
func TestPrior_AnswersOnlyForTheEntryTheWriterMarked(t *testing.T) {
	a, st := restarted(t, t.TempDir(), newClock(), "s1", synthSession(72, 2, 256))
	defer a.Close()
	defer st.Close()
	after := a.LastSeq("s1")
	if _, ok := a.Prior("s1", after); ok {
		t.Fatal("Prior answered before the entry existed")
	}
	a.gate = make(chan struct{}) // hold the writer: the store has the entry, the writer has not taken it
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
	if _, ok := a.Prior("s1", after); ok {
		t.Fatal("Prior answered before the writer marked the entry's start")
	}
	close(a.gate)
	settle(a)
	p, ok := a.Prior("s1", after)
	if !ok || p.Fold.Summary("s1").EventCount != int(after) || p.CreatedAt.IsZero() {
		t.Fatalf("Prior = %+v, %v; want the %d events before the entry, with a creation time", p, ok, after)
	}
	if _, ok := a.Prior("s1", after+1); ok {
		t.Fatal("Prior answered for an entry numbered after a different seq")
	}
	if _, ok := a.Prior("nobody", 3); ok {
		t.Fatal("Prior answered for an id the archive does not hold")
	}
}

// A paused writer still marks the entry's start: begin runs before write's pause check, and
// publishes for itself.
func TestPrior_APausedWriterStillMarksTheEntry(t *testing.T) {
	a, st := restarted(t, t.TempDir(), newClock(), "s1", synthSession(73, 2, 256))
	defer a.Close()
	defer st.Close()
	after := a.LastSeq("s1")
	a.paused.Store(true)
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
	settle(a)
	if _, ok := a.Prior("s1", after); !ok {
		t.Fatal("a paused writer left the entry's start unmarked")
	}
}

// After a clear the session starts over, and so does its row: nothing from before comes back.
func TestPrior_AClearedSessionListsOnlyWhatCameAfter(t *testing.T) {
	a, st := restarted(t, t.TempDir(), newClock(), "s1", synthSession(74, 2, 256))
	defer a.Close()
	defer st.Close()
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
	settle(a)
	st.Clear()
	settle(a)
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
	settle(a)
	if got := listed(t, st, "s1").EventCount; got != 1 {
		t.Fatalf("EventCount = %d after a clear, want 1", got)
	}
}

// A fold Prior hands out never changes afterwards. renameEntry turns the old id's before back into
// its live fold, which later events Add to; it must clone it first, or a reader still holding the
// published copy would watch it move under the store's read lock.
func TestPrior_AFoldHandedOutNeverChanges(t *testing.T) {
	a, st := restarted(t, t.TempDir(), newClock(), "X", synthSession(75, 1, 256))
	defer a.Close()
	defer st.Close()
	after := a.LastSeq("X")
	st.Append("X", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
	settle(a)
	p, ok := a.Prior("X", after)
	if !ok {
		t.Fatal("no prior for the resumed entry")
	}
	held := p.Fold.Summary("X").EventCount
	st.Rekey("X", "Y")
	settle(a)
	st.Append("X", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
	settle(a)
	if got := p.Fold.Summary("X").EventCount; got != held {
		t.Fatalf("a handed-out fold moved from %d to %d events", held, got)
	}
}

// A rename the archive does not carry out leaves the renamed entry's events under the old id. A
// later entry there must not list them a second time: together, the resident rows hold at most
// what was appended. Each case renames X's entry to Y that way after a restart over 3 events;
// X then gets a new entry, which is renamed in turn.
func TestPrior_ARenameLeftUnderTheOldIdIsNotCountedTwice(t *testing.T) {
	earlier, first, second := synthSession(81, 1, 256), synthSession(82, 1, 256), synthSession(83, 1, 256)
	ref := session.New(0, 0, 0)
	defer ref.Close()
	for _, evs := range [][]pipeline.SessionEvent{earlier, first, second} {
		for _, e := range evs {
			ref.Append("all", e)
		}
	}
	all := listed(t, ref, "all")

	cases := []struct {
		name string
		// rename moves X's entry to Y in the store and leaves its events under X in the archive.
		rename func(t *testing.T, a *Archive, st *session.Store)
	}{
		{"the target's directory exists", func(t *testing.T, a *Archive, st *session.Store) {
			if err := os.MkdirAll(filepath.Join(a.data, dirName("Y")), dirMode); err != nil {
				t.Fatal(err)
			}
			st.Rekey("X", "Y")
		}},
		{"the rename is lost to a full queue", func(t *testing.T, a *Archive, st *session.Store) {
			a.gate = make(chan struct{})
			a.Rekeyed("filler", "filler2") // the writer takes it and waits at the gate
			for len(a.ops) > 0 {
				runtime.Gosched()
			}
			for len(a.ops) < cap(a.ops) {
				a.Rekeyed("filler", "filler2")
			}
			st.Rekey("X", "Y")
			close(a.gate)
			if got := a.Stats().DroppedRenames; got != 1 {
				t.Fatalf("dropped %d renames, want 1", got)
			}
		}},
		{"the target's session.json cannot be written", func(t *testing.T, a *Archive, st *session.Store) {
			skipWithoutUnixModes(t)
			if os.Geteuid() == 0 {
				t.Skip("root writes through a read-only directory")
			}
			if err := os.Chmod(a.data, 0o500); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(a.data, dirMode)
			st.Rekey("X", "Y")
			settle(a)
			if got := a.Stats().WriteErrors; got != 1 {
				t.Fatalf("%d write errors, want the rename's 1", got)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, st := restarted(t, t.TempDir(), newClock(), "X", earlier)
			defer a.Close()
			defer st.Close()
			atMost := func(when string) {
				t.Helper()
				var events, tokens int
				for _, s := range st.ListSessions() {
					events, tokens = events+s.EventCount, tokens+s.TotalTokens
				}
				if events > all.EventCount || tokens > all.TotalTokens {
					t.Fatalf("%s, the rows hold %d events and %d tokens; %d and %d were appended",
						when, events, tokens, all.EventCount, all.TotalTokens)
				}
				if events < len(first)+len(second) {
					t.Fatalf("%s, the rows hold %d events, fewer than the two entries' own %d",
						when, events, len(first)+len(second))
				}
			}
			for _, e := range first {
				st.Append("X", e)
			}
			settle(a)
			c.rename(t, a, st)
			settle(a)
			for _, e := range second {
				st.Append("X", e)
			}
			settle(a)
			atMost("with a new entry under X")
			st.Rekey("X", "Z")
			settle(a)
			atMost("with that entry renamed in turn")
		})
	}
}
