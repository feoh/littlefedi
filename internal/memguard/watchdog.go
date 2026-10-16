package memguard

import (
	"context"
	"log/slog"
	"runtime/debug"
	"runtime/metrics"
	"time"

	appmetrics "brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
)

const (
	// watchInterval is long enough that the sampling itself costs nothing and
	// short enough that a spiral is caught in minutes rather than hours.
	watchInterval = 30 * time.Second

	// badWindows is how many consecutive bad samples it takes to act. One
	// window can be a backup run or a burst of image resizes; three in a row,
	// a minute and a half apart, is a state and not an event.
	badWindows = 3

	// gcCPUThreshold is the share of process CPU spent collecting above which
	// the collector is no longer keeping up with the mutator. A healthy Go
	// server sits far below this even under load.
	gcCPUThreshold = 0.20

	// goalSlack allows the heap goal to sit slightly above the live heap and
	// still count as clamped. Exactly equal is the textbook signature; a few
	// percent above it is the same condition observed a moment later.
	goalSlack = 1.05
)

// sample is one observation window.
type sample struct {
	gcCPUFraction float64
	heapGoal      uint64
	liveHeap      uint64
}

// clamped reports whether this window shows a collector running flat out
// against a goal it cannot reach.
//
// The two conditions have to hold together. High GC CPU on its own is just a
// busy allocator. A goal at or below the live heap on its own can be a single
// sample caught mid-cycle. Both at once, repeatedly, only happens when a soft
// memory limit has pulled the goal underneath the working set.
func (s sample) clamped() bool {
	if s.heapGoal == 0 || s.liveHeap == 0 {
		return false
	}
	if s.gcCPUFraction < gcCPUThreshold {
		return false
	}
	return float64(s.heapGoal) <= float64(s.liveHeap)*goalSlack
}

// Watch samples the collector every half minute and reports a memory limit
// that cannot be honoured.
//
// mayEscalate says whether the limit in force is one this process derived for
// itself, in which case the watchdog raises it toward ceiling. An operator who
// sets GOMEMLIMIT has made a decision and it is not overruled here, but the
// condition is still worth saying out loud, so the watching happens either way.
//
// ceiling is Plan.Ceiling: the most the limit may ever be raised to, where zero
// means there is none left to give. It is ignored when mayEscalate is false.
//
// A limit has to be in force for any of this to mean anything, so the caller
// must not start it otherwise: with no limit set the runtime reports
// math.MaxInt64 and the diagnosis below would be describing something else.
func Watch(ctx context.Context, ceiling int64, mayEscalate bool) {

	// runtime/metrics rather than runtime.ReadMemStats: the CPU classes give a
	// per-window figure, while MemStats only carries a lifetime average that
	// hides exactly the transition we are looking for. It also avoids a
	// stop-the-world on every tick.
	//
	// /gc/heap/live:bytes and not /memory/classes/heap/objects:bytes. The
	// second one counts dead objects the collector has not swept yet, so it
	// climbs toward the goal on every cycle of any busy server and would read
	// as clamped whenever allocation is heavy. The goal is computed from the
	// live heap, so the live heap is what it has to be compared against.
	samples := []metrics.Sample{
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/gc/heap/live:bytes"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/cpu/classes/total:cpu-seconds"},
	}

	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	var (
		prevGCSeconds    float64
		prevTotalSeconds float64
		consecutive      int
		escalations      int
		exhaustedLogged  bool
		primed           bool
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		metrics.Read(samples)
		goal := samples[0].Value.Uint64()
		live := samples[1].Value.Uint64()
		gcSeconds := samples[2].Value.Float64()
		totalSeconds := samples[3].Value.Float64()

		gcDelta := gcSeconds - prevGCSeconds
		totalDelta := totalSeconds - prevTotalSeconds
		prevGCSeconds, prevTotalSeconds = gcSeconds, totalSeconds
		if !primed {
			// The first tick would measure from process start, which averages
			// away whatever is happening now.
			primed = true
			continue
		}
		if totalDelta <= 0 {
			continue
		}

		current := sample{
			gcCPUFraction: gcDelta / totalDelta,
			heapGoal:      goal,
			liveHeap:      live,
		}
		if !current.clamped() {
			consecutive = 0
			continue
		}
		consecutive++
		if consecutive < badWindows {
			continue
		}
		consecutive = 0

		limit := debug.SetMemoryLimit(-1)
		if !mayEscalate {
			if !exhaustedLogged {
				exhaustedLogged = true
				slog.Error("garbage collector is running against the configured GOMEMLIMIT and cannot reach its heap goal; raise GOMEMLIMIT or lower cache.max_memory_mb",
					"limit_mib", limit>>20,
					"heap_goal_mib", goal>>20,
					"live_heap_mib", live>>20,
					"gc_cpu_fraction", current.gcCPUFraction)
			}
			continue
		}

		next := limit / 2 * 3
		if next > ceiling {
			next = ceiling
		}
		if next <= limit {
			if !exhaustedLogged {
				exhaustedLogged = true
				slog.Error("garbage collector is running against a memory limit it cannot reach, and this host has no headroom left to give it; lower cache.max_memory_mb, or move this instance to a host with more memory",
					"limit_mib", limit>>20,
					"ceiling_mib", ceiling>>20,
					"heap_goal_mib", goal>>20,
					"live_heap_mib", live>>20,
					"gc_cpu_fraction", current.gcCPUFraction)
			}
			continue
		}

		escalations++
		exhaustedLogged = false
		debug.SetMemoryLimit(next)
		appmetrics.IncrementMemoryLimitEscalations()
		slog.Warn("garbage collector could not reach its heap goal; raising the Go memory limit",
			"previous_limit_mib", limit>>20,
			"limit_mib", next>>20,
			"ceiling_mib", ceiling>>20,
			"heap_goal_mib", goal>>20,
			"live_heap_mib", live>>20,
			"gc_cpu_fraction", current.gcCPUFraction,
			"escalation", escalations)
	}
}
