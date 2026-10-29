package session

import (
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// ListSessions hands out summaries that Link reads after the store's lock is released, while
// appends keep folding new processes in. The summary must own its procs map: iterating the
// entry's own while an append writes it is a fatal runtime error, not a stale read.
func TestLink_ListingWhileAppendingIsSafe(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	s.Append("s1", named("0.0", time.Now()))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 2000 {
			e := named(string(rune('a'+i%26))+".1", time.Now())
			e.Phase = pipeline.SessionRequest
			s.Append("s1", e)
		}
	}()
	for range 200 {
		Link(s.ListSessions())
	}
	wg.Wait()
}
