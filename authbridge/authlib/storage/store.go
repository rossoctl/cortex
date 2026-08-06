package storage

import (
	"context"
	"time"
)

// Store provides key-value and hash operations for plugins that need
// persistent, cross-pod state (budget counters, session data, caches,
// rate limiting). Implementations live in separate modules to isolate
// their dependencies.
type Store interface {
	// Get returns the value for a simple key. Returns "" and nil if the key does not exist.
	Get(ctx context.Context, key string) (string, error)

	// Set stores a value with an expiration. Pass 0 for no expiration.
	Set(ctx context.Context, key, value string, ttl time.Duration) error

	// Incr atomically increments a simple key by delta and returns the new value.
	Incr(ctx context.Context, key string, delta int64) (int64, error)

	// HashIncr atomically increments a field in a hash by delta.
	HashIncr(ctx context.Context, key, field string, delta int64) (int64, error)

	// HashGet returns all fields of a hash as a string map.
	HashGet(ctx context.Context, key string) (map[string]string, error)

	// HashSetNX sets a field only if it does not already exist. Returns true if set.
	HashSetNX(ctx context.Context, key, field, value string) (bool, error)

	// Expire sets a TTL on a key. No-op if the key does not exist.
	Expire(ctx context.Context, key string, ttl time.Duration) error

	// Close releases the underlying connection.
	Close() error
}
