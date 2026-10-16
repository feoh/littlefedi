// Package memguard decides how much memory this process is allowed to use and
// then checks, while it runs, that the decision was survivable.
//
// A Go soft memory limit is not a safety net. Setting one below the working set
// the process has already committed to hold does not prevent an out-of-memory
// kill: it pins the collector at a heap goal it can never reach, so the GC
// restarts the moment it finishes and the machine burns its cores forever
// without freeing anything. The failure is silent, survives a restart only for
// as long as the caches take to refill, and looks like ordinary load from the
// outside. That is a worse outcome than the OOM the limit was meant to avoid.
//
// So the limit is derived here rather than chosen: from the cache budget the
// operator asked for, from the memory the host actually has, and from the
// SQLite arena the driver maps outside the heap. The one rule the arithmetic
// must never break is that the announced limit is never smaller than the Go
// memory already spoken for.
package memguard

const (
	// WorkingHeadroom covers everything held that is neither the SQLite arena
	// nor the in-memory cache: HTTP request and response buffers, compiled
	// templates, JSON-LD canonicalisation, gzip writers, TLS session state and
	// the decode buffer of whatever image is being resized. It is the term that
	// cannot be read off the configuration, so it is a measured guess rather
	// than a calculation, and it is generous: being wrong high
	// costs a little resident memory, being wrong low costs the whole machine.
	WorkingHeadroom int64 = 96 << 20

	// MinAutoLimit is the smallest limit this package will derive on its own.
	// It is the value the low_power preset has always used, kept as a floor so
	// that no existing installation gets a tighter limit than it had before.
	MinAutoLimit int64 = 256 << 20

	// ceilingPercent is the share of host memory this process may claim, across
	// the Go heap and whatever it maps beside it. The rest belongs to the page
	// cache, to the media files being served from it, and to everything else on
	// the box.
	ceilingPercent = 70

	// minCacheBytes is the point below which an in-memory cache stops being
	// worth its own bookkeeping and is switched off instead.
	minCacheBytes int64 = 8 << 20
)

// Inputs are the three quantities the limit is derived from. Every value is in
// bytes.
type Inputs struct {
	// Arena is the memory the portable SQLite driver maps for its WASM engine.
	// It is resident memory on the host, but an anonymous mmap rather than Go
	// allocations, so debug.SetMemoryLimit does not see it and it must be
	// subtracted from what the Go heap may claim instead of added to it. Zero
	// on PostgreSQL and on the native SQLite builds.
	Arena int64
	// Cache is the in-memory cache budget the configuration asked for. Zero
	// when the cache is disabled. This one is Go heap.
	Cache int64
	// PhysMem is the physical memory of the host, or zero when it could not be
	// determined. Zero means unknown, never "none".
	PhysMem int64
}

// Plan is the outcome of the derivation.
type Plan struct {
	// Limit is the value to hand to debug.SetMemoryLimit. Always at least
	// Cache, since the cache is Go memory that will certainly be held.
	Limit int64
	// Cache is the budget the caches should actually be built with, which is
	// Inputs.Cache unless the host could not hold it.
	Cache int64
	// Ceiling is the most the memory limit may ever be raised to, and it means
	// only that. On a host whose memory is known it is the host's share less
	// whatever the SQLite arena maps beside the heap; where it is not known it
	// is twice Limit, which is the headroom the derivation would have granted a
	// host twice this size. Zero means there is none left to give, and never
	// "unknown", so a caller does not have to guess which of the two it is
	// looking at.
	Ceiling int64
	// CacheReduced reports whether Cache came out below what was asked for, so
	// the caller can say so.
	CacheReduced bool
}

// Compute derives a memory limit that the process can actually reach.
//
// It has no side effects and reads nothing from the environment, so the whole
// policy is testable without starting a server.
func Compute(in Inputs) Plan {
	plan := Plan{Cache: in.Cache}

	// The arena is resident on the host but invisible to the memory limit, so
	// it comes off the host's share before the Go heap gets to divide up what
	// is left. Adding it to the limit instead would let the process hold the
	// arena twice: once in the limit and once in the mapping.
	hasCeiling := in.PhysMem > 0
	if hasCeiling {
		plan.Ceiling = in.PhysMem*ceilingPercent/100 - in.Arena
		if plan.Ceiling < 0 {
			plan.Ceiling = 0
		}
	}

	// The cache is the only term an operator can give back: the headroom is the
	// cost of answering a request. When the host cannot hold what was
	// configured, take it from there and from nowhere else.
	if hasCeiling {
		room := plan.Ceiling - WorkingHeadroom
		if room < minCacheBytes {
			room = 0
		}
		if room < plan.Cache {
			plan.Cache = room
			plan.CacheReduced = true
		}
	}

	// Go memory already spoken for. Announcing a limit below this line is the
	// bug this package exists to prevent, so the floor is applied last and
	// nothing after it may lower the result.
	committed := plan.Cache + WorkingHeadroom

	plan.Limit = committed
	if plan.Limit < MinAutoLimit {
		plan.Limit = MinAutoLimit
	}
	if hasCeiling {
		if plan.Limit > plan.Ceiling {
			// The ceiling outranks the floor: on a host too small for
			// MinAutoLimit the smaller number wins. It does not
			// outrank what is already committed, though. If even the bare
			// headroom does not fit, say so by returning the number we need
			// rather than the one that fits, and let the caller warn about it.
			plan.Limit = plan.Ceiling
			if plan.Limit < committed {
				plan.Limit = committed
			}
		}
	} else {
		// Nothing known about the host, so nothing was clamped and there is no
		// physical bound to offer. Allow the limit to double and no further,
		// which is the room the derivation would have given a host twice this
		// size. Deciding it here keeps Ceiling meaning one thing.
		plan.Ceiling = plan.Limit * 2
	}
	return plan
}

// PhysicalMemory reports the host's physical memory in bytes, or zero when the
// platform has no answer we trust.
func PhysicalMemory() int64 {
	n := physicalMemoryBytes()
	// A value that does not fit in an int64 is a value we misread.
	if n == 0 || n > 1<<62 {
		return 0
	}
	return int64(n)
}
