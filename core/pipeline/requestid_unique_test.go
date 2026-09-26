package pipeline

import (
	"sync"
	"testing"
)

// TestRequestIDNoCollisions: the id is what pairs a response to its request, so
// a duplicate silently files a response under the wrong request. Randomness
// alone made that unlikely; the counter makes it impossible in-process.
func TestRequestIDNoCollisions(t *testing.T) {
	const n = 200_000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := newRequestID()
		if _, dup := seen[id]; dup {
			t.Fatalf("collision after %d ids: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestRequestIDConcurrent: listeners generate ids from many goroutines.
func TestRequestIDConcurrent(t *testing.T) {
	const goroutines, each = 32, 2000
	var mu sync.Mutex
	seen := make(map[string]struct{}, goroutines*each)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := make([]string, 0, each)
			for i := 0; i < each; i++ {
				ids = append(ids, newRequestID())
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				if _, dup := seen[id]; dup {
					t.Errorf("concurrent collision: %q", id)
				}
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()
	if len(seen) != goroutines*each {
		t.Errorf("got %d unique ids, want %d", len(seen), goroutines*each)
	}
}
