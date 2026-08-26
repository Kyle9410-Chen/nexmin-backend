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

// writeSuppression is how long after an invalidation results are refused entry to the
// cache entirely.
//
// Google's Directory API is eventually consistent: a read issued right after a
// successful write can still come back with the pre-write list. Discarding the entry
// generation is not enough for that, because such a read carries the *current*
// generation -- it started after the clear -- and would be stored and served for a full
// TTL. Refusing to store anything for a few seconds costs a handful of uncached reads
// and removes the whole class of "the change I just made came back".
const writeSuppression = 5 * time.Second

type cache[T any] struct {
	mu      sync.RWMutex
	ttl     time.Duration
	entries map[string]entry[T]

	// generation increments on every clear. A fetch that began before an invalidation
	// carries the old generation, and its result is dropped instead of being written
	// back over the clear -- otherwise it would resurrect pre-write data and serve it
	// for a full TTL.
	generation uint64

	// suppressUntil is when results may be stored again. See writeSuppression.
	suppressUntil time.Time
	suppress      time.Duration
}

type entry[T any] struct {
	items     []T
	expiresAt time.Time
}

func newCache[T any](ttl time.Duration) *cache[T] {
	return &cache[T]{
		ttl:      ttl,
		entries:  make(map[string]entry[T]),
		suppress: writeSuppression,
	}
}

// begin returns the generation a fetch about to start should carry into set.
func (c *cache[T]) begin() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.generation
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

// clear drops every entry, invalidates fetches already in flight, and closes the cache
// to new entries for writeSuppression.
//
// Writes invalidate broadly rather than by key because a group can be addressed by its
// email address or by its immutable ID, so the key a write sees need not match the key
// an earlier read cached under. Writes are rare and these lists are small, so throwing
// the whole map away is cheaper than getting the invalidation wrong.
func (c *cache[T]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	clear(c.entries)
	c.generation++
	c.suppressUntil = time.Now().Add(c.suppress)
}

// set stores items under key, unless the cache was invalidated since the fetch began or
// is still inside the post-write suppression window. gen comes from begin.
func (c *cache[T]) set(key string, gen uint64, items []T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// The fetch started before an invalidation, so what it read is pre-write.
	if gen != c.generation {
		return
	}

	// Google may not have propagated the write yet, so even a fetch that started after
	// the invalidation cannot be trusted this soon.
	if time.Now().Before(c.suppressUntil) {
		return
	}

	c.entries[key] = entry[T]{
		items:     items,
		expiresAt: time.Now().Add(c.ttl),
	}
}
