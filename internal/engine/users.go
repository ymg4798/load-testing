package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ymg4798/lt/internal/scenario"
)

// runUsers is the closed-model executor (constant-users, ramping-users):
// each user runs iterations back to back, so the offered load follows the
// target's speed. Latency is measured from the actual send.
func (r *runner) runUsers(ctx context.Context, testStart time.Time) {
	begin := testStart.Add(r.sc.StartAfter.D())
	if !sleepUntil(ctx, begin) {
		return
	}
	iterCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-iterCtx.Done():
		}
	}()

	var (
		target    atomic.Int64
		alive     []*atomic.Bool
		wg        sync.WaitGroup
		exhausted atomic.Bool // data rows or the iteration budget ran out
		budget    atomic.Int64
	)
	budget.Store(int64(r.sc.Iterations))
	limited := r.sc.Executor == scenario.Iterations
	user := func(id int, flag *atomic.Bool) {
		defer wg.Done()
		defer r.e.users.Add(-1)
		defer flag.Store(false)
		s := r.newSlot()
		defer s.vu.Close()
		for iterCtx.Err() == nil && int64(id) < target.Load() && !exhausted.Load() {
			if limited && budget.Add(-1) < 0 {
				exhausted.Store(true)
				return
			}
			r.e.inFlight.Add(1)
			err := r.iterate(iterCtx, s, time.Time{})
			r.e.inFlight.Add(-1)
			if err == errDataExhausted {
				exhausted.Store(true)
			}
		}
	}

	total := time.Duration(r.sched.total * float64(time.Second))
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil && !exhausted.Load() {
		el := time.Since(begin)
		if el >= total {
			break
		}
		n := int64(r.sched.At(el.Seconds()) + 0.5)
		target.Store(n)
		for id := 0; int64(id) < n; id++ {
			if id >= len(alive) {
				alive = append(alive, new(atomic.Bool))
			}
			if !alive[id].Load() {
				alive[id].Store(true)
				r.e.users.Add(1)
				wg.Add(1)
				go user(id, alive[id])
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
		}
	}
	target.Store(0)
	waitGraceful(ctx, &wg, r.e.file.Options.GracefulStop.D(), cancel)
}
