package shared

import (
	"sync"
	"testing"
	"time"
)

func TestStore_PutGet(t *testing.T) {
	s := New()
	defer s.Close()
	s.Put("k", "v", time.Minute)
	got, ok := s.Get("k")
	if !ok || got.(string) != "v" {
		t.Fatalf("Get = %v, %v; want v, true", got, ok)
	}
}

func TestStore_GetMissing(t *testing.T) {
	s := New()
	defer s.Close()
	if _, ok := s.Get("nope"); ok {
		t.Fatal("expected miss")
	}
}

func TestStore_Expiry(t *testing.T) {
	s := New()
	defer s.Close()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	s.Put("k", "v", time.Minute)
	now = now.Add(30 * time.Second)
	if _, ok := s.Get("k"); !ok {
		t.Fatal("should still be live at 30s")
	}
	now = now.Add(31 * time.Second)
	if _, ok := s.Get("k"); ok {
		t.Fatal("should be expired past 60s")
	}
}

func TestStore_Delete(t *testing.T) {
	s := New()
	defer s.Close()
	s.Put("k", "v", time.Minute)
	s.Delete("k")
	if _, ok := s.Get("k"); ok {
		t.Fatal("expected deleted")
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s := New()
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := string(rune('a' + n%26))
			s.Put(key, n, time.Minute)
			s.Get(key)
		}(i)
	}
	wg.Wait()
}

// TestStore_SweepEvictsExpired verifies sweep() reclaims expired entries and
// leaves unexpired ones intact. Uses an injected clock and calls sweep()
// directly so the test is deterministic — it does not rely on the janitor's
// timer.
func TestStore_SweepEvictsExpired(t *testing.T) {
	s := New()
	defer s.Close()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }

	s.Put("a", 1, time.Second)
	s.Put("b", 2, time.Second)
	s.Put("live", 3, time.Hour)

	now = now.Add(2 * time.Second) // a and b expired; live still valid

	s.sweep()

	s.mu.RLock()
	_, aPresent := s.items["a"]
	_, bPresent := s.items["b"]
	_, livePresent := s.items["live"]
	s.mu.RUnlock()

	if aPresent || bPresent {
		t.Errorf("sweep should have evicted expired keys: a=%v b=%v", aPresent, bPresent)
	}
	if !livePresent {
		t.Error("sweep should have kept the unexpired key 'live'")
	}
	if _, ok := s.Get("live"); !ok {
		t.Error("Get('live') should still succeed after sweep")
	}
}

// TestStore_CloseIsIdempotent verifies Close can be called multiple times
// without panicking.
func TestStore_CloseIsIdempotent(t *testing.T) {
	s := New()
	s.Close()
	s.Close()
}
