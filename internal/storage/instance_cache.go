package storage

import (
	"sync"
	"time"
)

// instanceReachCache lives inside bunStorage (decorators cannot intercept the
// internal IsInstanceReachable calls made during delivery fan-out) and does two
// things:
//
//  1. Throttles MarkInstanceReachable writes: every successful delivery and
//     every inbound activity marks the sender reachable, which would be one
//     instances-table UPDATE per activity even though the row almost never
//     changes. A mark within reachableWriteInterval is skipped; any
//     MarkInstanceUnreachable clears the throttle so the first success after a
//     failure always clears the streak in the DB immediately.
//  2. Memoizes IsInstanceReachable for reachableMemoTTL: delivery fan-out asks
//     it once per inbox at enqueue time and once per job at dequeue time, each
//     an uncached point SELECT.
//
// Both are process-local. In postgres multi-process mode another process may
// observe state up to reachableWriteInterval stale - harmless, because
// delivery gating works on the 14-day InstanceOfflineWindow.
type instanceReachCache struct {
	mu              sync.Mutex
	recentReachable map[string]time.Time
	memo            map[string]reachMemoEntry
}

type reachMemoEntry struct {
	reachable bool
	expires   time.Time
}

const (
	reachableWriteInterval = 5 * time.Minute
	reachableMemoTTL       = 30 * time.Second
	// reachMapCap bounds both maps; wiping on overflow is fine (they refill).
	reachMapCap = 4096
)

// shouldWriteReachable reports whether a reachable mark for domain must hit
// the DB, recording the write time when it returns true.
func (c *instanceReachCache) shouldWriteReachable(domain string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recentReachable == nil {
		c.recentReachable = make(map[string]time.Time)
	}
	if t, ok := c.recentReachable[domain]; ok && time.Since(t) < reachableWriteInterval {
		return false
	}
	if len(c.recentReachable) >= reachMapCap {
		c.recentReachable = make(map[string]time.Time)
	}
	c.recentReachable[domain] = time.Now()
	return true
}

// undoWriteReachable removes the throttle stamp recorded by
// shouldWriteReachable when the DB write it authorized failed.
func (c *instanceReachCache) undoWriteReachable(domain string) {
	c.mu.Lock()
	delete(c.recentReachable, domain)
	c.mu.Unlock()
}

func (c *instanceReachCache) getMemo(domain string) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.memo[domain]
	if !ok || time.Now().After(e.expires) {
		return false, false
	}
	return e.reachable, true
}

func (c *instanceReachCache) setMemo(domain string, reachable bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.memo == nil {
		c.memo = make(map[string]reachMemoEntry)
	}
	if len(c.memo) >= reachMapCap {
		c.memo = make(map[string]reachMemoEntry)
	}
	c.memo[domain] = reachMemoEntry{reachable: reachable, expires: time.Now().Add(reachableMemoTTL)}
}

// invalidateMemo drops only the memo entry, forcing the next reachability
// check to re-read the DB (reachability also depends on the Suspended flag,
// so a mark can never assert the memoized value directly).
func (c *instanceReachCache) invalidateMemo(domain string) {
	c.mu.Lock()
	delete(c.memo, domain)
	c.mu.Unlock()
}

// invalidate drops both the throttle stamp and the memo entry for domain, so
// the next mark writes and the next reachability check re-reads the DB.
func (c *instanceReachCache) invalidate(domain string) {
	c.mu.Lock()
	delete(c.recentReachable, domain)
	delete(c.memo, domain)
	c.mu.Unlock()
}

// invalidateAll wipes the cache; used by rare admin-scoped writes
// (suspend/delete instance) that only know the instance ID.
func (c *instanceReachCache) invalidateAll() {
	c.mu.Lock()
	c.recentReachable = nil
	c.memo = nil
	c.mu.Unlock()
}
