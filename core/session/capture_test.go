package session

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// captureRecorder is a Recorder that takes request captures.
type captureRecorder struct {
	fakeRecorder
	got []*pipeline.RequestCapture
}

func (c *captureRecorder) RecordCapture(sessionID string, e *pipeline.SessionEvent, cap *pipeline.RequestCapture) {
	c.Record(sessionID, e)
	c.got = append(c.got, cap)
}

// A capture goes to every recorder that takes one, with the event as the store recorded it, and
// to the others as a plain Record. The store itself never holds it: raw bodies are megabytes,
// and the store keeps every event in memory.
func TestAppendCapture_ReachesRecordersAndNotTheStore(t *testing.T) {
	store := New(5*time.Minute, 100, 0)
	defer store.Close()
	plain, takes := &fakeRecorder{}, &captureRecorder{}
	store.AddRecorder(plain)
	store.AddRecorder(takes)

	c := &pipeline.RequestCapture{Original: []byte(`{"messages":[]}`)}
	store.AppendCapture("alice", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest}, c)

	if n, _, _ := plain.snapshot(); n != 1 {
		t.Fatalf("plain recorder called %d times, want 1", n)
	}
	if len(takes.got) != 1 || takes.got[0] != c {
		t.Fatalf("capture recorder got %v, want the capture", takes.got)
	}
	if _, id, e := takes.snapshot(); id != "alice" || e.Seq != 1 || e.SessionID != "alice" {
		t.Fatalf("capture recorder saw %q seq %d, want the stamped event", id, e.Seq)
	}
}

// A paired append carries its capture on the second event, the request; the first, a tunnel's
// open, has none.
func TestAppendPairCapture_CapturesTheSecond(t *testing.T) {
	store := New(5*time.Minute, 100, 0)
	defer store.Close()
	takes := &captureRecorder{}
	store.AddRecorder(takes)

	c := &pipeline.RequestCapture{Original: []byte(`{}`)}
	store.AppendPairCapture("s", pairEvent("r1", true), pairEvent("r1", false), c)
	if len(takes.got) != 2 || takes.got[0] != nil || takes.got[1] != c {
		t.Fatalf("captures %v, want nil then the capture", takes.got)
	}
}

// A listener copies bodies for capture only when some recorder takes them: an in-cluster sidecar
// has no archive, and must not pay for a copy nobody keeps.
func TestTakesCaptures(t *testing.T) {
	store := New(5*time.Minute, 100, 0)
	defer store.Close()
	if store.TakesCaptures() {
		t.Fatal("an empty store takes captures")
	}
	store.AddRecorder(&fakeRecorder{})
	if store.TakesCaptures() {
		t.Fatal("a plain recorder takes captures")
	}
	store.AddRecorder(&captureRecorder{})
	if !store.TakesCaptures() {
		t.Fatal("a capture recorder was not seen")
	}
	var nilStore *Store
	if nilStore.TakesCaptures() {
		t.Fatal("a nil store takes captures")
	}
}
