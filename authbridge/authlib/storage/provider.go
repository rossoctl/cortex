package storage

import (
	"fmt"
	"sync"
)

// Factory creates a Store from a connection URL.
type Factory func(url string) (Store, error)

var (
	mu        sync.RWMutex
	factories = make(map[string]Factory)
)

// Register makes a store factory available by scheme (e.g. "redis", "valkey").
// Typically called from init() in the implementation module.
func Register(scheme string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[scheme]; dup {
		panic("storage: Register called twice for scheme " + scheme)
	}
	factories[scheme] = f
}

// Open creates a Store using the registered factory for the given scheme.
func Open(scheme, url string) (Store, error) {
	mu.RLock()
	f, ok := factories[scheme]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("storage: unknown scheme %q (forgot to import the driver?)", scheme)
	}
	return f(url)
}
