// Copyright 2026 Kagenti Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// cacheEntry stores a cached secret value with expiration
type cacheEntry struct {
	value     string
	expiresAt time.Time
}

// SecretCache provides lease-aware caching for Vault secrets
//
// Caching strategy adapted from Klaviger: use the shorter of (configured TTL, Vault lease duration)
// to ensure we don't serve expired secrets.
type SecretCache struct {
	mu         sync.RWMutex
	entries    map[string]cacheEntry
	maxSize    int
	defaultTTL time.Duration
}

// NewSecretCache creates a new secret cache
func NewSecretCache(defaultTTL time.Duration) *SecretCache {
	if defaultTTL == 0 {
		defaultTTL = 5 * time.Minute
	}

	return &SecretCache{
		entries:    make(map[string]cacheEntry),
		maxSize:    1000, // Reasonable default for secret caching
		defaultTTL: defaultTTL,
	}
}

// Get retrieves a cached secret value
// Returns ("", false) if not found or expired
func (c *SecretCache) Get(path string) (string, bool) {
	key := c.cacheKey(path)

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		return "", false
	}

	// Check if expired
	if time.Now().After(entry.expiresAt) {
		return "", false
	}

	return entry.value, true
}

// Set stores a secret value with lease-aware TTL
//
// TTL calculation (from Klaviger pattern):
// - Use the shorter of (configured default TTL, Vault lease duration)
// - This ensures we refresh secrets before they expire in Vault
func (c *SecretCache) Set(path, value string, leaseDuration int64) {
	key := c.cacheKey(path)

	// Calculate TTL: min(defaultTTL, leaseDuration)
	ttl := c.defaultTTL
	if leaseDuration > 0 {
		leaseTTL := time.Duration(leaseDuration) * time.Second
		if leaseTTL < ttl {
			ttl = leaseTTL
		}
	}

	// Don't cache if TTL is too short
	if ttl < 30*time.Second {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict expired entries if cache is full
	if len(c.entries) >= c.maxSize {
		c.evictExpiredLocked()
		if len(c.entries) >= c.maxSize {
			// Clear all if still full
			c.entries = make(map[string]cacheEntry)
		}
	}

	// Store with 30s buffer before expiration (to ensure refresh)
	c.entries[key] = cacheEntry{
		value:     value,
		expiresAt: time.Now().Add(ttl - 30*time.Second),
	}
}

// Clear removes all cached entries
func (c *SecretCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
}

// Len returns the number of cached entries
func (c *SecretCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// evictExpiredLocked removes expired entries (must hold write lock)
func (c *SecretCache) evictExpiredLocked() {
	now := time.Now()
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}

// cacheKey generates a cache key from the secret path
func (c *SecretCache) cacheKey(path string) string {
	h := sha256.New()
	h.Write([]byte(path))
	return hex.EncodeToString(h.Sum(nil))
}
