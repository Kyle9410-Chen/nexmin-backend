package googlegroup

import (
	"sync"
	"time"
)

// cache is a per-process, in-memory list cache keyed by an arbitrary string.
//
// Because it lives in process memory, replicas cache independently and a restart
// clears it. That is acceptable for lists that change rarely and are only ever read.
// If this service is scaled out and cache coherence starts to matter, replace this
// with a shared Redis cache (see clustron's internal/redis: gob-encoded values,
// colon-namespaced keys, TTL on write).
type cache[T any] struct {
	mu      sync.RWMutex
	ttl     time.Duration
	entries map[string]entry[T]
}

type entry[T any] struct {
	items     []T
	expiresAt time.Time
}

func newCache[T any](ttl time.Duration) *cache[T] {
	return &cache[T]{
		ttl:     ttl,
		entries: make(map[string]entry[T]),
	}
}

// get returns the cached items for key and whether a live entry was found.
func (c *cache[T]) get(key string) ([]T, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		return nil, false
	}

	if time.Now().After(e.expiresAt) {
		c.mu.Lock()
		// Re-check under the write lock: another goroutine may have refreshed the
		// entry between the read unlock and here.
		if current, still := c.entries[key]; still && time.Now().After(current.expiresAt) {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return nil, false
	}

	return e.items, true
}

func (c *cache[T]) set(key string, items []T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = entry[T]{
		items:     items,
		expiresAt: time.Now().Add(c.ttl),
	}
}
