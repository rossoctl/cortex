package session

import (
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// A session shows NoRecord from its first flagged event on, and a later event without the flag
// (a header-less call filed to it) does not clear it. Another session is not marked.
func TestNoRecord_SummaryIsSticky(t *testing.T) {
	s := New(time.Hour, 0, 0)
	defer s.Close()
	s.Append("s1", model.SessionEvent{Phase: model.SessionRequest, NoRecord: true})
	s.Append("s1", model.SessionEvent{Phase: model.SessionRequest})
	s.Append("s2", model.SessionEvent{Phase: model.SessionRequest})

	got := map[string]bool{}
	for _, sum := range s.ListSessions() {
		got[sum.ID] = sum.NoRecord
	}
	if !got["s1"] || got["s2"] {
		t.Fatalf("NoRecord by session = %v, want s1 only", got)
	}
}

// MarkNoRecord tells every Withholder (the archive, which owns the opt-out) and marks the live
// entry's summary, for rows the listener does not build (praxis mode's /v1/ingest). It never
// names a shared bucket.
func TestNoRecord_MarkNoRecordTellsTheArchive(t *testing.T) {
	s := New(time.Hour, 0, 0)
	defer s.Close()
	w := &withholder{}
	s.AddRecorder(w)
	s.Append("s1", model.SessionEvent{Phase: model.SessionRequest})
	s.MarkNoRecord("s1")
	s.MarkNoRecord("s2") // no live entry yet: the archive still hears it
	s.MarkNoRecord(DefaultSessionID)
	if want := []string{"s1", "s2"}; !slices.Equal(w.ids, want) {
		t.Fatalf("Withhold calls %v, want %v", w.ids, want)
	}
	for _, sum := range s.ListSessions() {
		if sum.ID == "s1" && !sum.NoRecord {
			t.Fatal("s1's live summary is not marked")
		}
	}
}

// withholder is a Recorder that also implements Withholder, as the archive does.
type withholder struct{ ids []string }

func (w *withholder) Record(string, *model.SessionEvent) {}
func (w *withholder) Withhold(id string)                 { w.ids = append(w.ids, id) }

// A header may name the default bucket or a pending bucket. Neither is one conversation, so
// the store clears the flag there and MarkNoRecord ignores them: no recorder sees a withheld
// shared bucket.
func TestNoRecord_SharedBucketsAreNeverWithheld(t *testing.T) {
	s := New(time.Hour, 0, 0)
	defer s.Close()
	rec := &flagRecorder{}
	s.AddRecorder(rec)
	pending := PendingSessionID("claude-code")
	s.Append(DefaultSessionID, model.SessionEvent{Phase: model.SessionRequest, NoRecord: true})
	s.Append(pending, model.SessionEvent{Phase: model.SessionRequest, NoRecord: true})
	s.MarkNoRecord(DefaultSessionID)
	s.MarkNoRecord(pending)
	s.Append(DefaultSessionID, model.SessionEvent{Phase: model.SessionRequest})
	s.Append(pending, model.SessionEvent{Phase: model.SessionRequest})
	if want := []bool{false, false, false, false}; !slices.Equal(rec.flags, want) {
		t.Fatalf("NoRecord by event = %v, want %v", rec.flags, want)
	}
	for _, sum := range s.ListSessions() {
		if sum.NoRecord {
			t.Fatalf("%s is marked NoRecord", sum.ID)
		}
	}
}

// flagRecorder keeps each event's NoRecord in order. The store calls it under its write lock.
type flagRecorder struct{ flags []bool }

func (r *flagRecorder) Record(_ string, e *model.SessionEvent) { r.flags = append(r.flags, e.NoRecord) }

// The archive's fold keeps the mark through session.json.
func TestNoRecord_FoldRoundTrips(t *testing.T) {
	f := NewSummaryFold()
	f.Add("s1", &model.SessionEvent{Phase: model.SessionRequest, NoRecord: true})
	f.Add("s1", &model.SessionEvent{Phase: model.SessionRequest})
	b, err := f.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var back SummaryFold
	if err := back.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if !back.NoRecord() || !back.Summary("s1").NoRecord {
		t.Fatalf("NoRecord lost through %s", b)
	}
	plain, err := NewSummaryFold().MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var p SummaryFold
	if err := p.UnmarshalJSON(plain); err != nil {
		t.Fatal(err)
	}
	if p.NoRecord() {
		t.Fatal("an unmarked fold decodes as NoRecord")
	}
}
