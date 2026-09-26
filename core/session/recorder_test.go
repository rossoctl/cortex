package session

import (
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// fakeRecorder captures what Append handed it.
type fakeRecorder struct {
	mu      sync.Mutex
	calls   int
	lastID  string
	lastEvt pipeline.SessionEvent
}

func (f *fakeRecorder) Record(sessionID string, e *pipeline.SessionEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastID = sessionID
	if e != nil {
		f.lastEvt = *e
	}
}

func (f *fakeRecorder) snapshot() (int, string, pipeline.SessionEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastID, f.lastEvt
}

// A registered Recorder must fire on Append, with the session id it was
// appended under and the event as recorded. This is the only wiring that feeds
// /v1/usage: if it silently stops firing, every chart goes flat while the event
// timeline keeps working, so nothing else would reveal the break.
func TestAddRecorder_FiresOnAppend(t *testing.T) {
	store := New(5*time.Minute, 100, 0)
	defer store.Close()

	rec := &fakeRecorder{}
	store.AddRecorder(rec)

	store.Append("alice", pipeline.SessionEvent{
		At:         time.Now(),
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		StatusCode: 200,
		Host:       "api.example",
	})

	calls, id, evt := rec.snapshot()
	if calls != 1 {
		t.Fatalf("Record called %d times, want 1", calls)
	}
	if id != "alice" {
		t.Errorf("session id = %q, want alice", id)
	}
	if evt.StatusCode != 200 || evt.Host != "api.example" {
		t.Errorf("event not passed through intact: %+v", evt)
	}
	// Append stamps SessionID on the event; a Recorder must see the stamped
	// value, since an outbound event has no protocol-native session field.
	if evt.SessionID != "alice" {
		t.Errorf("event.SessionID = %q, want alice — the stamp must happen before Record", evt.SessionID)
	}
}

// Every registered Recorder must be called, not just the first.
func TestAddRecorder_MultipleRecordersAllFire(t *testing.T) {
	store := New(5*time.Minute, 100, 0)
	defer store.Close()

	a, b := &fakeRecorder{}, &fakeRecorder{}
	store.AddRecorder(a)
	store.AddRecorder(b)

	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse})

	if ca, _, _ := a.snapshot(); ca != 1 {
		t.Errorf("first recorder called %d times, want 1", ca)
	}
	if cb, _, _ := b.snapshot(); cb != 1 {
		t.Errorf("second recorder called %d times, want 1", cb)
	}
}

// A nil Recorder must be ignored rather than stored, or Append would panic on
// the request hot path.
func TestAddRecorder_NilIsIgnored(t *testing.T) {
	store := New(5*time.Minute, 100, 0)
	defer store.Close()

	store.AddRecorder(nil)
	store.Append("s1", pipeline.SessionEvent{At: time.Now()}) // must not panic
}

// Recorders see every event, including ones the store will immediately trim.
// A bucket counter must keep counting after the events it counted have aged out
// of the event list — otherwise a chart loses history an operator could still
// see a minute ago.
func TestAddRecorder_FiresForEventsThatWillBeTrimmed(t *testing.T) {
	const maxEvents = 3
	store := New(5*time.Minute, maxEvents, 0)
	defer store.Close()

	rec := &fakeRecorder{}
	store.AddRecorder(rec)

	for i := 0; i < 10; i++ {
		store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse})
	}

	calls, _, _ := rec.snapshot()
	if calls != 10 {
		t.Errorf("Record called %d times, want 10 — the recorder must see events the store trims", calls)
	}
	if v := store.View("s1"); v == nil || len(v.Events) != maxEvents {
		got := 0
		if v != nil {
			got = len(v.Events)
		}
		t.Errorf("store kept %d events, want %d — precondition for this test", got, maxEvents)
	}
}

// No recorder registered is the common case and must stay a no-op.
func TestAppend_NoRecordersIsFine(t *testing.T) {
	store := New(5*time.Minute, 100, 0)
	defer store.Close()
	store.Append("s1", pipeline.SessionEvent{At: time.Now()})
	if v := store.View("s1"); v == nil || len(v.Events) != 1 {
		t.Error("append broke with no recorders registered")
	}
}
