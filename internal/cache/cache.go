// Package cache provides a small, dependency-free, memory-bounded LRU cache with
// per-entry TTL. It is the shared primitive behind littleFedi's optional in-RAM
// caching (the Storage entity cache and the app-level page cache), giving a
// Redis-like speedup on low-power devices without an external service.
//
// The cache is bounded by an approximate byte budget rather than an entry count:
// callers pass a size estimate for each value at Set time, and the cache evicts
// least-recently-used entries until the total is back under budget. Values are
// stored as opaque any; callers are responsible for treating retrieved values as
// immutable (or copying them) since a single stored value may be shared.
package cache

import (
	"container/list"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type entry struct {
	key     string
	val     any
	size    int
	expires time.Time // zero means no expiry
	deps    []string
}

// Stats is a point-in-time snapshot of cache activity.
type Stats struct {
	Hits            uint64
	Misses          uint64
	Evictions       uint64
	Expirations     uint64
	Entries         int
	Bytes           int64
	MaxBytes        int64
	Dependencies    int
	DependencyBytes int64
}

// LookupResult explains whether a lookup hit or why it missed. It is primarily
// used by debug diagnostics; normal callers can continue using Get.
type LookupResult string

const (
	LookupHit      LookupResult = "hit"
	LookupMissing  LookupResult = "missing"
	LookupExpired  LookupResult = "expired"
	LookupDisabled LookupResult = "disabled"
)

// MemoryCache is a thread-safe, byte-bounded LRU cache with per-entry TTL.
type MemoryCache struct {
	mu              sync.Mutex
	maxBytes        int64
	bytes           int64
	ll              *list.List // front = most recently used
	items           map[string]*list.Element
	dependents      map[string]map[string]struct{}
	dependencyBytes int64

	hits        uint64
	misses      uint64
	evictions   uint64
	expirations uint64

	// Fencing state for SetIfVersion*: seq is a monotonic invalidation
	// counter (returned by Version). globalFenceSeq is the seq of the last
	// event that must fence every in-flight store (Clear, or a Delete/
	// DeletePrefix that actually removed entries). wipedDeps records the seq
	// of the last DeleteDependencies per dependency key, including no-op
	// wipes, which target entries that may be mid-build, so a store is
	// rejected only when one of its own dependencies was wiped after the
	// build began. Routine federation traffic wipes dependencies constantly;
	// fencing globally on those made a busy instance unable to store any
	// page or component at all.
	seq            uint64
	globalFenceSeq uint64
	wipedDeps      map[string]uint64
}

// maxWipedDeps bounds the per-dependency wipe index; on overflow the map is
// reset and the global fence raised once, which is always safe.
const maxWipedDeps = 4096

// New returns a cache bounded to maxBytes. A maxBytes <= 0 yields a cache that
// stores nothing (every Set is a no-op), which is a convenient "disabled" value.
func New(maxBytes int64) *MemoryCache {
	return &MemoryCache{
		maxBytes:   maxBytes,
		ll:         list.New(),
		items:      make(map[string]*list.Element),
		dependents: make(map[string]map[string]struct{}),
		wipedDeps:  make(map[string]uint64),
	}
}

// Get returns the value stored under key. Expired entries are treated as misses
// and removed. A hit moves the entry to the most-recently-used position.
func (c *MemoryCache) Get(key string) (any, bool) {
	v, ok, _ := c.GetWithResult(key)
	return v, ok
}

// GetWithResult is Get plus a reason suitable for debug logging.
func (c *MemoryCache) GetWithResult(key string) (any, bool, LookupResult) {
	if c == nil {
		return nil, false, LookupDisabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false, LookupMissing
	}
	en := el.Value.(*entry)
	if !en.expires.IsZero() && time.Now().After(en.expires) {
		c.removeElement(el)
		c.misses++
		c.expirations++
		return nil, false, LookupExpired
	}
	c.ll.MoveToFront(el)
	c.hits++
	return en.val, true, LookupHit
}

// Set stores val under key with the given approximate size in bytes and TTL
// (ttl <= 0 means no expiry). Entries larger than the whole budget are dropped
// rather than immediately evicting everything else. After insertion the cache
// evicts LRU entries until it is back under budget.
func (c *MemoryCache) Set(key string, val any, size int, ttl time.Duration) {
	c.SetWithDependencies(key, val, size, ttl, nil)
}

// SetWithDependencies stores an entry and indexes the dependency keys that can
// invalidate it. Dependencies are de-duplicated and their index overhead counts
// against the same memory budget as values.
func (c *MemoryCache) SetWithDependencies(key string, val any, size int, ttl time.Duration, dependencies []string) {
	if c == nil || c.maxBytes <= 0 || size <= 0 {
		return
	}
	if int64(size) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}

	if el, ok := c.items[key]; ok {
		en := el.Value.(*entry)
		c.removeDependencies(en)
		c.bytes += int64(size) - int64(en.size)
		en.val = val
		en.size = size
		en.expires = exp
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&entry{key: key, val: val, size: size, expires: exp})
		c.items[key] = el
		c.bytes += int64(size)
	}
	c.addDependencies(c.items[key].Value.(*entry), dependencies)

	for c.bytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeElement(back)
		c.evictions++
	}
}

// Version returns the invalidation sequence used to fence in-flight builders:
// capture it before building, pass it to SetIfVersion*.
func (c *MemoryCache) Version() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq
}

// SetIfVersion stores only when no fencing invalidation happened since
// version was captured: a Clear, or a Delete/DeletePrefix that actually
// removed entries. Ordinary insertions never fence.
func (c *MemoryCache) SetIfVersion(version uint64, key string, val any, size int, ttl time.Duration) bool {
	return c.SetIfVersionWithDependencies(version, key, val, size, ttl, nil)
}

// SetIfVersionWithDependencies is SetWithDependencies fenced by (a) the
// global fence (Clear / key-prefix removals) and (b) per-dependency wipes: the
// store is rejected when any of its own dependencies was invalidated after
// the build began, even if that wipe matched nothing at the time (the entry
// being built is exactly what it targeted). Unrelated invalidations do not
// fence, so stores survive routine federation traffic.
func (c *MemoryCache) SetIfVersionWithDependencies(version uint64, key string, val any, size int, ttl time.Duration, dependencies []string) bool {
	if c == nil || c.maxBytes <= 0 || size <= 0 || int64(size) > c.maxBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.globalFenceSeq > version {
		slog.Debug("cache: store fenced globally", "key", key, "version", version, "fence", c.globalFenceSeq)
		return false
	}
	for _, dep := range dependencies {
		if c.wipedDeps[dep] > version {
			slog.Debug("cache: store fenced by dependency", "key", key, "dependency", dep, "version", version, "wiped_at", c.wipedDeps[dep])
			return false
		}
	}
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	if el, ok := c.items[key]; ok {
		en := el.Value.(*entry)
		c.removeDependencies(en)
		c.bytes += int64(size - en.size)
		en.val, en.size, en.expires = val, size, exp
		c.ll.MoveToFront(el)
	} else {
		c.items[key] = c.ll.PushFront(&entry{key: key, val: val, size: size, expires: exp})
		c.bytes += int64(size)
	}
	c.addDependencies(c.items[key].Value.(*entry), dependencies)
	for c.bytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeElement(back)
		c.evictions++
	}
	return true
}

// DeleteDependencies removes every entry that depends on any supplied key.
// Each entry is removed once even when several dependencies match. Every wipe
// - including one that matches nothing - is recorded per dependency so an
// in-flight build of an entry with that dependency is fenced, while stores
// with unrelated dependencies are not.
func (c *MemoryCache) DeleteDependencies(dependencies ...string) int {
	if c == nil || len(dependencies) == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	if len(c.wipedDeps)+len(dependencies) > maxWipedDeps {
		// Bounded index: reset and raise the global fence once instead.
		c.wipedDeps = make(map[string]uint64)
		c.globalFenceSeq = c.seq
	}
	for _, dep := range dependencies {
		c.wipedDeps[dep] = c.seq
	}
	removed := 0
	for _, dep := range dependencies {
		for key := range c.dependents[dep] {
			if el := c.items[key]; el != nil {
				c.removeElement(el)
				removed++
			}
		}
	}
	return removed
}

// Delete removes the given keys if present. The global fence rises only when
// an entry was actually removed, so no-op deletes (constant under federation
// traffic) never fence concurrent stores.
func (c *MemoryCache) Delete(keys ...string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := false
	for _, k := range keys {
		if el, ok := c.items[k]; ok {
			if !removed {
				removed = true
				c.seq++
				c.globalFenceSeq = c.seq
			}
			c.removeElement(el)
		}
	}
}

// DeletePrefix removes every entry whose key starts with prefix. It is used to
// drop a whole namespace at once, e.g. all cached pages belonging to one viewer.
// The global fence rises only when an entry was actually removed.
func (c *MemoryCache) DeletePrefix(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := false
	for k, el := range c.items {
		if strings.HasPrefix(k, prefix) {
			if !removed {
				removed = true
				c.seq++
				c.globalFenceSeq = c.seq
			}
			c.removeElement(el)
		}
	}
}

// Clear empties the cache and fences every in-flight store.
func (c *MemoryCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.globalFenceSeq = c.seq
	c.wipedDeps = make(map[string]uint64)
	c.ll.Init()
	c.items = make(map[string]*list.Element)
	c.dependents = make(map[string]map[string]struct{})
	c.bytes = 0
	c.dependencyBytes = 0
}

// Stats returns a snapshot of cache counters.
func (c *MemoryCache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits:            c.hits,
		Misses:          c.misses,
		Evictions:       c.evictions,
		Expirations:     c.expirations,
		Entries:         len(c.items),
		Bytes:           c.bytes,
		MaxBytes:        c.maxBytes,
		Dependencies:    len(c.dependents),
		DependencyBytes: c.dependencyBytes,
	}
}

// removeElement drops el from both the list and the index. Caller holds c.mu.
func (c *MemoryCache) removeElement(el *list.Element) {
	en := el.Value.(*entry)
	c.removeDependencies(en)
	c.ll.Remove(el)
	delete(c.items, en.key)
	c.bytes -= int64(en.size)
}

func (c *MemoryCache) addDependencies(en *entry, dependencies []string) {
	seen := make(map[string]struct{}, len(dependencies))
	for _, dep := range dependencies {
		if dep == "" {
			continue
		}
		if _, ok := seen[dep]; ok {
			continue
		}
		seen[dep] = struct{}{}
		set := c.dependents[dep]
		if set == nil {
			set = make(map[string]struct{})
			c.dependents[dep] = set
			c.dependencyBytes += int64(64 + len(dep))
			c.bytes += int64(64 + len(dep))
		}
		set[en.key] = struct{}{}
		overhead := int64(32 + len(en.key))
		c.dependencyBytes += overhead
		c.bytes += overhead
		en.deps = append(en.deps, dep)
	}
}

func (c *MemoryCache) removeDependencies(en *entry) {
	for _, dep := range en.deps {
		set := c.dependents[dep]
		if set == nil {
			continue
		}
		delete(set, en.key)
		overhead := int64(32 + len(en.key))
		c.dependencyBytes -= overhead
		c.bytes -= overhead
		if len(set) == 0 {
			delete(c.dependents, dep)
			overhead = int64(64 + len(dep))
			c.dependencyBytes -= overhead
			c.bytes -= overhead
		}
	}
	en.deps = nil
}
