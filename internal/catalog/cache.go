package catalog

import (
	"sync"
	"time"
)

// cache is a tiny per-service snapshot cache shared by the catalog and FX
// services. It records the fetch time so a stale value can be reported with its
// original fetch time, and so concurrent refreshes can coalesce.
type cache[T any] struct {
	mu        sync.Mutex
	value     T
	hasValue  bool
	fetchedAt time.Time
	until     time.Time
}

// fresh returns the cached value only while it is within its TTL.
func (c *cache[T]) fresh(now time.Time) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasValue || !c.until.After(now) {
		var zero T
		return zero, false
	}
	return c.value, true
}

// completedSince returns the cached value when it was stored at or after the
// caller started waiting, which lets a concurrent forced refresh share an
// already in-flight refresh instead of issuing a duplicate request.
func (c *cache[T]) completedSince(arrival time.Time) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasValue || c.fetchedAt.Before(arrival) {
		var zero T
		return zero, false
	}
	return c.value, true
}

// previous returns the cached value regardless of TTL, so a failed refresh can
// still serve the last good value as an explicit stale response.
func (c *cache[T]) previous() (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasValue {
		var zero T
		return zero, false
	}
	return c.value, true
}

// store records a freshly fetched value together with its fetch time.
func (c *cache[T]) store(value T, fetchedAt time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = value
	c.hasValue = true
	c.fetchedAt = fetchedAt
	c.until = fetchedAt.Add(ttl)
}
