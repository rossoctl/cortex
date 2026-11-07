package session

import (
	"math/rand"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
)

func inference() pipeline.SessionEvent {
	return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Inference: &pipeline.InferenceExtension{Model: "m"}}
}

func tunnelRow() pipeline.SessionEvent {
	return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Tunnel: true}
}

// lastInferenceIn is the newest inference request of the view, as a pointer into v.Events.
func lastInferenceIn(v *model.SessionView) *model.SessionEvent {
	for i := len(v.Events) - 1; i >= 0; i-- {
		if model.IsInferenceRequest(&v.Events[i]) {
			return &v.Events[i]
		}
	}
	return nil
}

// checkFact fails when the fact does not point at want, or its State does not agree with its Event.
func checkFact(t *testing.T, name string, f model.SessionFact, want *model.SessionEvent) {
	t.Helper()
	if f.Event != want {
		t.Fatalf("%s: Event = %p, want %p", name, f.Event, want)
	}
	wantState := model.FactNone
	if want != nil {
		wantState = model.FactPresent
	}
	if f.State != wantState {
		t.Fatalf("%s: State = %d, want %d", name, f.State, wantState)
	}
}

func wantNoEntry(t *testing.T, s *Store, id string) {
	t.Helper()
	v, f := s.ViewFacts(id)
	if v != nil {
		t.Fatalf("ViewFacts(%q) view = %+v, want nil", id, v)
	}
	checkFact(t, "LastIntent", f.LastIntent, nil)
	checkFact(t, "LastInference", f.LastInference, nil)
}

// The facts point at the same event as the view's own accessors, after each append, for each
// max_events. The pinned intent stays present.
func TestFactsAgreeWithTheView(t *testing.T) {
	for _, maxEvents := range []int{0, 1, 2, 3, 7} {
		r := rand.New(rand.NewSource(int64(maxEvents) + 1))
		s := New(0, maxEvents, 0)
		sawIntent := false
		for n := 0; n < 3000; n++ {
			var e pipeline.SessionEvent
			switch r.Intn(5) {
			case 0:
				e, sawIntent = intent("send"), true
			case 1:
				e = inference()
			case 2:
				e = pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Inference: &pipeline.InferenceExtension{}}
			case 3:
				e = tunnelRow()
			default:
				e = pipeline.SessionEvent{Direction: pipeline.Inbound, Phase: pipeline.SessionResponse, A2A: &pipeline.A2AExtension{}}
			}
			s.Append("s", e)
			v, f := s.ViewFacts("s")
			if v == nil {
				t.Fatalf("max %d n %d: no view", maxEvents, n)
			}
			checkFact(t, "LastIntent", f.LastIntent, v.LastIntent())
			checkFact(t, "LastInference", f.LastInference, lastInferenceIn(v))
			if sawIntent && f.LastIntent.State != model.FactPresent {
				t.Fatalf("max %d n %d: the pinned intent is not present", maxEvents, n)
			}
		}
	}
}

// With max_events 2 the intent is pinned, and a trimmed inference request reads FactNone at the
// same moment the view stops holding it.
func TestFacts_ATrimmedInferenceRequestIsNone(t *testing.T) {
	s := New(0, 2, 0)
	s.Append("s", intent("send"))
	for i := 0; i < 5; i++ {
		s.Append("s", inference())
		v, f := s.ViewFacts("s")
		checkFact(t, "LastInference", f.LastInference, lastInferenceIn(v))
		if f.LastInference.State != model.FactPresent {
			t.Fatalf("round %d: the new inference request is not present", i)
		}

		s.Append("s", tunnelRow())
		v, f = s.ViewFacts("s")
		if got := v.InferenceRequests(); len(got) != 0 {
			t.Fatalf("round %d: the view holds %d inference requests after the trim, want 0", i, len(got))
		}
		checkFact(t, "LastInference", f.LastInference, nil)
		checkFact(t, "LastIntent", f.LastIntent, v.LastIntent())
		if f.LastIntent.State != model.FactPresent {
			t.Fatalf("round %d: the pinned intent is not present", i)
		}
	}
}

func TestFacts_AnIDTheStoreDoesNotHoldIsNone(t *testing.T) {
	wantNoEntry(t, New(0, 0, 0), "never-seen")
}

func TestFacts_ADroppedEntryIsNone(t *testing.T) {
	fill := func(s *Store, id string) {
		s.Append(id, intent("send"))
		s.Append(id, inference())
	}

	t.Run("max_sessions", func(t *testing.T) {
		s := New(0, 0, 1)
		fill(s, "first")
		fill(s, "second")
		wantNoEntry(t, s, "first")
	})
	t.Run("Delete", func(t *testing.T) {
		s := New(0, 0, 0)
		fill(s, "s")
		if !s.Delete("s") {
			t.Fatal("Delete returned false")
		}
		wantNoEntry(t, s, "s")
	})
	t.Run("Clear", func(t *testing.T) {
		s := New(0, 0, 0)
		fill(s, "s")
		s.Clear()
		wantNoEntry(t, s, "s")
	})
	t.Run("ttl", func(t *testing.T) {
		clk := &procTestClock{t: time.Unix(1_700_000_000, 0)}
		s := New(time.Minute, 0, 0, WithClock(clk.now))
		fill(s, "s")
		clk.advance(2 * time.Minute)
		wantNoEntry(t, s, "s")
	})
}

func TestFacts_MoveWithRekey(t *testing.T) {
	s := New(0, 0, 0)
	s.Append(DefaultSessionID, intent("send"))
	s.Append(DefaultSessionID, inference())
	s.Rekey(DefaultSessionID, "ctx-1")

	wantNoEntry(t, s, DefaultSessionID)
	v, f := s.ViewFacts("ctx-1")
	checkFact(t, "LastIntent", f.LastIntent, v.LastIntent())
	checkFact(t, "LastInference", f.LastInference, lastInferenceIn(v))
	if f.LastIntent.Event == nil || f.LastIntent.Event.SessionID != "ctx-1" {
		t.Fatalf("LastIntent = %+v, want an event with SessionID ctx-1", f.LastIntent)
	}
	if f.LastInference.State != model.FactPresent {
		t.Fatalf("LastInference.State = %d, want FactPresent", f.LastInference.State)
	}
}

// An event of neither kind does not change the facts.
func TestFacts_AnEventOfNeitherKindChangesNothing(t *testing.T) {
	s := New(0, 0, 0)
	s.Append("s", intent("send"))
	s.Append("s", inference())
	_, before := s.ViewFacts("s")

	s.Append("s", tunnelRow())
	s.Append("s", chatter("tools/call"))
	s.Append("s", pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Inference: &pipeline.InferenceExtension{}})
	_, after := s.ViewFacts("s")

	if after.LastIntent.State != model.FactPresent || after.LastIntent.Event.Seq != before.LastIntent.Event.Seq {
		t.Fatalf("LastIntent moved: %+v, before %+v", after.LastIntent, before.LastIntent)
	}
	if after.LastInference.State != model.FactPresent || after.LastInference.Event.Seq != before.LastInference.Event.Seq {
		t.Fatalf("LastInference moved: %+v, before %+v", after.LastInference, before.LastInference)
	}
}
