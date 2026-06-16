// Package cache provides a SHA-256 keyed token cache with TTL-based eviction.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

type entry struct {
	token     string
	expiresAt time.Time
}

// Cache is a thread-safe token cache keyed by SHA-256 hash of (subjectToken, audience).
type Cache struct {
	mu      sync.RWMutex
	entries map[string]entry
	maxSize int
}

// Option configures cache behavior.
type Option func(*Cache)

// WithMaxSize sets the maximum number of cache entries.
// When exceeded, expired entries are evicted first; if still full,
// ~25% of entries are randomly evicted using Go's non-deterministic
// map iteration order.
func WithMaxSize(n int) Option {
	return func(c *Cache) { c.maxSize = n }
}

// New creates a token cache.
func New(opts ...Option) *Cache {
	c := &Cache{
		entries: make(map[string]entry),
		maxSize: 10000,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Get returns a cached token for the given subject token and audience.
// Returns ("", false) if not found or expired. Expired entries are left
// for eviction during Set() to avoid write-lock contention on reads.
func (c *Cache) Get(subjectToken, audience string) (string, bool) {
	key := cacheKey(subjectToken, audience)
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return "", false
	}
	return e.token, true
}

// Set stores a token with the given TTL. A buffer of 30 seconds is subtracted
// from the TTL to ensure tokens are refreshed before they expire.
func (c *Cache) Set(subjectToken, audience, token string, ttl time.Duration) {
	if ttl <= 30*time.Second {
		return // too short to cache
	}
	key := cacheKey(subjectToken, audience)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxSize {
		c.evictExpired()
		if len(c.entries) >= c.maxSize {
			c.evictRandom()
		}
	}
	c.entries[key] = entry{
		token:     token,
		expiresAt: time.Now().Add(ttl - 30*time.Second),
	}
}

// Len returns the number of entries (including potentially expired ones).
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func (c *Cache) evictRandom() {
	target := c.maxSize * 3 / 4
	for k := range c.entries {
		if len(c.entries) <= target {
			break
		}
		delete(c.entries, k)
	}
}

func (c *Cache) evictExpired() {
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}

func cacheKey(subjectToken, audience string) string {
	h := sha256.New()
	h.Write([]byte(subjectToken))
	h.Write([]byte{0}) // separator
	h.Write([]byte(audience))
	return hex.EncodeToString(h.Sum(nil))
}
