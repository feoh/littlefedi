package queue

import (
	"context"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/metrics"
)

// DefaultPreemptionGrace is how long a running background job may keep going
// after a foreground request arrives. Cancelling on the first request throws
// away whatever the job had already done, including remote fetches, and the
// retry then repeats all of it: on slow storage the same inbox activity can be
// processed several times before one attempt survives long enough to commit.
// Most jobs finish well inside this window, so the grace usually costs the
// request nothing at all.
const DefaultPreemptionGrace = 2 * time.Second

// ForegroundGate gives interactive HTTP work priority over cooperative
// background jobs. Enter closes the current interrupt channel immediately;
// workers resume only after the last foreground request leaves.
type ForegroundGate struct {
	mu        sync.Mutex
	active    int
	idle      chan struct{}
	interrupt chan struct{}
	reserve   chan struct{}
	grace     time.Duration
}

func NewForegroundGate() *ForegroundGate {
	idle := make(chan struct{})
	close(idle)
	reserve := make(chan struct{}, 1)
	reserve <- struct{}{}
	return &ForegroundGate{
		idle:      idle,
		interrupt: make(chan struct{}),
		reserve:   reserve,
		grace:     DefaultPreemptionGrace,
	}
}

// SetPreemptionGrace overrides DefaultPreemptionGrace. Zero restores the
// original behavior of cancelling a preemptible job as soon as a foreground
// request arrives.
func (g *ForegroundGate) SetPreemptionGrace(grace time.Duration) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.grace = grace
	g.mu.Unlock()
}

// WaitIdleOrReserve waits for foreground work to finish. After maxWait it
// grants one background job a shared reserve slot, preventing total queue
// starvation while keeping forced work serialized across pools. The slot is
// returned as soon as the job is done: the rate limit is the maxWait every
// candidate pays before it may contend, so no pool can monopolize the slot,
// and a pool that took it only to find its queue empty does not hold the
// other pool up.
func (g *ForegroundGate) WaitIdleOrReserve(ctx context.Context, maxWait time.Duration) (bool, func(), error) {
	if g == nil {
		return false, func() {}, nil
	}
	if maxWait <= 0 {
		return false, func() {}, g.WaitIdle(ctx)
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()

	g.mu.Lock()
	idle := g.idle
	active := g.active > 0
	g.mu.Unlock()
	if !active {
		return false, func() {}, nil
	}

	select {
	case <-idle:
		return false, func() {}, nil
	case <-ctx.Done():
		return false, func() {}, ctx.Err()
	case <-timer.C:
	}

	select {
	case <-idle:
		return false, func() {}, nil
	case <-ctx.Done():
		return false, func() {}, ctx.Err()
	case <-g.reserve:
	}

	if !g.Active() {
		g.reserve <- struct{}{}
		return false, func() {}, nil
	}
	metrics.IncrementQueueReserveGrants()
	var once sync.Once
	release := func() {
		once.Do(func() { g.reserve <- struct{}{} })
	}
	return true, release, nil
}

func (g *ForegroundGate) Enter() func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	if g.active == 0 {
		close(g.interrupt)
		g.idle = make(chan struct{})
	}
	g.active++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			if g.active == 0 {
				close(g.idle)
				g.interrupt = make(chan struct{})
			}
			g.mu.Unlock()
		})
	}
}

func (g *ForegroundGate) Active() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active > 0
}

func (g *ForegroundGate) WaitIdle(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	idle := g.idle
	g.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BackgroundContext is cancelled once foreground work has been in flight for
// the whole grace period, for a preemptible worker. A request that completes
// inside the grace leaves the job running, which is the common case and saves
// redoing its work from scratch.
func (g *ForegroundGate) BackgroundContext(parent context.Context, preemptible bool) (context.Context, context.CancelFunc) {
	grace := DefaultPreemptionGrace
	if g != nil {
		g.mu.Lock()
		grace = g.grace
		g.mu.Unlock()
	}
	return g.BackgroundContextWithGrace(parent, preemptible, grace)
}

// BackgroundContextWithGrace overrides the gate's grace period for one piece
// of work. Pass zero for speculative work that produces nothing worth keeping,
// such as cache warming: redoing it is cheap, so it should get out of the way
// of a request immediately rather than hold it up.
func (g *ForegroundGate) BackgroundContextWithGrace(parent context.Context, preemptible bool, grace time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if g == nil || !preemptible {
		return ctx, cancel
	}
	go g.cancelOnSustainedForeground(ctx, cancel, grace)
	return ctx, cancel
}

func (g *ForegroundGate) cancelOnSustainedForeground(ctx context.Context, cancel context.CancelFunc, grace time.Duration) {
	for {
		g.mu.Lock()
		idle, interrupt, active := g.idle, g.interrupt, g.active > 0
		g.mu.Unlock()

		if !active {
			select {
			case <-interrupt:
				continue
			case <-ctx.Done():
				return
			}
		}
		if grace <= 0 {
			cancel()
			return
		}
		timer := time.NewTimer(grace)
		select {
		case <-idle:
			// The burst passed without outlasting the grace. Wait for the
			// next one instead of killing a job that is already running.
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			cancel()
			return
		}
	}
}
