package cache

import (
	"testing"
	"time"
)

func TestGetSet(t *testing.T) {
	c := New()
	c.Set("token-abc", "aud-1", "openid", "http://idp", "exchanged-xyz", 5*time.Minute)

	got, ok := c.Get("token-abc", "aud-1", "openid", "http://idp")
	if !ok || got != "exchanged-xyz" {
		t.Errorf("Get() = (%q, %v), want (%q, true)", got, ok, "exchanged-xyz")
	}
}

func TestGetMiss(t *testing.T) {
	c := New()
	_, ok := c.Get("nonexistent", "aud", "openid", "http://idp")
	if ok {
		t.Error("expected cache miss")
	}
}

func TestGetDifferentAudience(t *testing.T) {
	c := New()
	c.Set("token-abc", "aud-1", "openid", "http://idp", "exchanged-1", 5*time.Minute)

	_, ok := c.Get("token-abc", "aud-2", "openid", "http://idp")
	if ok {
		t.Error("expected cache miss for different audience")
	}
}

func TestTTLTooShort(t *testing.T) {
	c := New()
	c.Set("token", "aud", "openid", "http://idp", "val", 20*time.Second) // below 30s threshold
	if c.Len() != 0 {
		t.Error("expected no entry for TTL below threshold")
	}
}

func TestMaxSize(t *testing.T) {
	c := New(WithMaxSize(2))
	c.Set("t1", "a", "openid", "http://idp", "v1", 5*time.Minute)
	c.Set("t2", "a", "openid", "http://idp", "v2", 5*time.Minute)
	c.Set("t3", "a", "openid", "http://idp", "v3", 5*time.Minute) // should trigger eviction

	if c.Len() > 2 {
		t.Errorf("cache has %d entries, want <= 2", c.Len())
	}
}

func TestCacheKeyCollisionResistance(t *testing.T) {
	// Keys must differ when tokens, audiences, scopes or endpoints differ
	k1 := cacheKey("abc", "def", "openid", "http://idp")
	k2 := cacheKey("ab", "cdef", "openid", "http://idp")
	k3 := cacheKey("abc", "de", "openid", "http://idp")
	k4 := cacheKey("abc", "def", "openid admin", "http://idp")
	k5 := cacheKey("abc", "def", "openid", "http://other-idp")
	if k1 == k2 {
		t.Error("different token/audience combos produced same key")
	}
	if k1 == k3 {
		t.Error("different audience produced same key")
	}
	if k1 == k4 {
		t.Error("different scopes produced same key")
	}
	if k1 == k5 {
		t.Error("different token endpoint produced same key")
	}
}

func TestMaxSize_RandomEviction(t *testing.T) {
	const maxSize = 100
	c := New(WithMaxSize(maxSize))
	for i := range maxSize {
		c.Set("token"+string(rune(i)), "aud", "openid", "http://idp", "val", 5*time.Minute)
	}
	if c.Len() != maxSize {
		t.Fatalf("pre-check: cache len = %d, want %d", c.Len(), maxSize)
	}

	c.Set("overflow", "aud", "openid", "http://idp", "val", 5*time.Minute)

	got := c.Len()
	// After eviction ~25% is removed, so we expect roughly 75 + 1 = 76 entries.
	// Allow some margin: between 70 and 80.
	if got < 70 || got > 80 {
		t.Errorf("after overflow: cache len = %d, want ~76 (75%% of %d + 1)", got, maxSize)
	}
	// The new entry must be present.
	if _, ok := c.Get("overflow", "aud", "openid", "http://idp"); !ok {
		t.Error("overflow entry not found after eviction")
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New()
	done := make(chan struct{})
	for i := range 100 {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			token := string(rune('A' + n%26))
			c.Set(token, "aud", "openid", "http://idp", "val", 5*time.Minute)
			c.Get(token, "aud", "openid", "http://idp")
		}(i)
	}
	for range 100 {
		<-done
	}
}
