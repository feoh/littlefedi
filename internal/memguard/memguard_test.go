package memguard

import (
	"runtime"
	"testing"
)

const mib = 1 << 20

func TestComputeRoomyHost(t *testing.T) {
	plan := Compute(Inputs{Cache: 64 * mib, PhysMem: 2048 * mib})
	if plan.CacheReduced || plan.Cache != 64*mib {
		t.Errorf("cache should be untouched on a roomy host: %+v", plan)
	}
	if plan.Ceiling != 2048*mib*70/100 {
		t.Errorf("Ceiling = %d MiB, want 70%% of RAM", plan.Ceiling/mib)
	}
	if plan.Limit != MinAutoLimit {
		t.Errorf("Limit = %d MiB, want the %d MiB floor", plan.Limit/mib, MinAutoLimit/mib)
	}
}

func TestComputeShrinksCacheOnSmallHost(t *testing.T) {
	plan := Compute(Inputs{Cache: 512 * mib, PhysMem: 512 * mib})
	if !plan.CacheReduced || plan.Cache >= 512*mib {
		t.Errorf("cache should shrink to fit 70%% of 512 MiB: %+v", plan)
	}
	if plan.Cache+WorkingHeadroom > plan.Ceiling {
		t.Errorf("cache plus headroom exceeds the ceiling: %+v", plan)
	}
	if plan.Limit > plan.Ceiling {
		t.Errorf("Limit %d exceeds Ceiling %d", plan.Limit, plan.Ceiling)
	}
}

func TestComputeSubtractsArena(t *testing.T) {
	without := Compute(Inputs{Cache: 32 * mib, PhysMem: 1024 * mib})
	with := Compute(Inputs{Arena: 100 * mib, Cache: 32 * mib, PhysMem: 1024 * mib})
	if without.Ceiling-with.Ceiling != 100*mib {
		t.Errorf("arena not subtracted from the ceiling: %d vs %d", without.Ceiling, with.Ceiling)
	}
}

func TestComputeWithoutPhysicalMemory(t *testing.T) {
	plan := Compute(Inputs{Cache: 512 * mib})
	if plan.Limit != 512*mib+WorkingHeadroom {
		t.Errorf("Limit = %d MiB", plan.Limit/mib)
	}
	if plan.Ceiling != plan.Limit*2 {
		t.Errorf("Ceiling = %d, want twice the limit", plan.Ceiling)
	}
}

func TestPhysicalMemory(t *testing.T) {
	n := PhysicalMemory()
	if runtime.GOOS == "linux" && n <= 0 {
		t.Errorf("PhysicalMemory() = %d on Linux", n)
	}
	if n < 0 {
		t.Errorf("PhysicalMemory() = %d", n)
	}
}
