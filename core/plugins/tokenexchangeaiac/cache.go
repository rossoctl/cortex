package tokenexchangeaiac

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	// expirySkew is taken off every token's lifetime, so a token is never
	// served from cache in the seconds before the IdP would reject it.
	expirySkew = 30 * time.Second

	// maxCacheEntries bounds each cache. A subject token is per user session,
	// so a long-running agent sees an unbounded stream of them.
	maxCacheEntries = 10000
)

type tokenEntry struct {
	token   string
	expires time.Time
}

// tokenCache holds issued tokens by (subject, audience). Keys are a SHA-256 of
// the subject, so the cache never holds a usable copy of a caller's token.
type tokenCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]tokenEntry
}

func newTokenCache(now func() time.Time) *tokenCache {
	return &tokenCache{now: now, entries: make(map[string]tokenEntry)}
}

func tokenKey(subject, audience string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:]) + "\x00" + audience
}

func (c *tokenCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !c.now().Before(e.expires) {
		delete(c.entries, key)
		return "", false
	}
	return e.token, true
}

// drop forgets one entry.
func (c *tokenCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// put caches a token for its lifetime less expirySkew. A token too short-lived
// to outlast the skew is not cached at all.
func (c *tokenCache) put(key string, tok issuedToken) {
	if tok.expiresIn <= expirySkew {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= maxCacheEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		// Still full of live entries: start over rather than evict
		// selectively. A miss costs one IdP round trip.
		if len(c.entries) >= maxCacheEntries {
			c.entries = make(map[string]tokenEntry)
		}
	}
	c.entries[key] = tokenEntry{token: tok.accessToken, expires: now.Add(tok.expiresIn - expirySkew)}
}

// unknownCache remembers audiences the IdP said it has no client for, so that
// traffic to a non-target — an inference endpoint, a telemetry collector — costs
// one IdP round trip per TTL rather than one per request. The TTL is also how
// long a newly registered target keeps being passed through.
type unknownCache struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	entries map[string]time.Time
}

func newUnknownCache(now func() time.Time, ttl time.Duration) *unknownCache {
	return &unknownCache{now: now, ttl: ttl, entries: make(map[string]time.Time)}
}

func (c *unknownCache) has(audience string) bool {
	if c.ttl <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[audience]
	if !ok {
		return false
	}
	if !c.now().Before(exp) {
		delete(c.entries, audience)
		return false
	}
	return true
}

func (c *unknownCache) add(audience string) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCacheEntries {
		c.entries = make(map[string]time.Time)
	}
	c.entries[audience] = c.now().Add(c.ttl)
}
