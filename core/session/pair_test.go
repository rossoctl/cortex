package session

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

func pairEvent(requestID string, tunnel bool) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		RequestID: requestID, Tunnel: tunnel,
	}
}

// AppendPair exists for a reader that pairs an event with the one after it — agentop
// folds a bridged tunnel's open into the next event in the session — so no concurrent
// append may ever land between the two.
func TestAppendPair_NothingLandsBetweenThePair(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	const n = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			id := fmt.Sprint("pair-", i)
			s.AppendPair("sess-1", pairEvent(id, true), pairEvent(id, false))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.Append("sess-1", pairEvent(fmt.Sprint("other-", i), false))
		}
	}()
	wg.Wait()

	evs := s.View("sess-1").Events
	if len(evs) != 3*n {
		t.Fatalf("%d events, want %d", len(evs), 3*n)
	}
	opens := 0
	for i, e := range evs {
		if !e.Tunnel {
			continue
		}
		opens++
		if i+1 >= len(evs) || evs[i+1].Tunnel || evs[i+1].RequestID != e.RequestID {
			t.Fatalf("event %d (%s) is not directly followed by its pair", i, e.RequestID)
		}
		if evs[i+1].Seq != e.Seq+1 {
			t.Errorf("pair %s has seqs %d and %d, want consecutive", e.RequestID, e.Seq, evs[i+1].Seq)
		}
	}
	if opens != n {
		t.Errorf("%d first-of-pair events, want %d", opens, n)
	}
}
