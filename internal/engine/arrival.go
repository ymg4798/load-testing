package engine

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// runArrival is the open-model executor: iterations start on a schedule
// regardless of how long earlier ones take. When MaxInFlight iterations
// are already running, a due arrival is dropped and counted rather than
// delayed, so a slow target can never slow the offered load down.
func (r *runner) runArrival(ctx context.Context, testStart time.Time) {
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

	free := make(chan *slot, r.sc.MaxInFlight)
	created := 0
	var wg sync.WaitGroup
	var exhausted atomic.Bool
	total := time.Duration(r.sched.total * float64(time.Second))
	end := begin.Add(total)
	k := 0.0

	for ctx.Err() == nil && !exhausted.Load() {
		// Start every arrival that is due by now.
		now := time.Now()
		var next time.Time
		for {
			at, ok := r.sched.TimeOf(k)
			if !ok {
				next = end
				break
			}
			due := begin.Add(time.Duration(at * float64(time.Second)))
			if due.After(now) || !due.Before(end) {
				next = due
				break
			}
			k++
			var s *slot
			select {
			case s = <-free:
			default:
				if created < r.sc.MaxInFlight {
					created++
					s = r.newSlot()
				}
			}
			if s == nil {
				r.e.dropped.Add(1)
				continue
			}
			r.e.inFlight.Add(1)
			wg.Add(1)
			go func(s *slot, due time.Time) {
				defer wg.Done()
				defer r.e.inFlight.Add(-1)
				lag := time.Since(due)
				r.e.agg.RecordLag(lag)
				s.lag = lag
				if r.iterate(iterCtx, s, due) == errDataExhausted {
					exhausted.Store(true)
				}
				free <- s
			}(s, due)
		}
		if !next.Before(end) {
			if !sleepUntil(ctx, end) {
				break
			}
			break
		}
		waitPrecise(ctx, next)
	}

	waitGraceful(ctx, &wg, r.e.file.Options.GracefulStop.D(), cancel)
	close(free)
	for s := range free {
		s.vu.Close()
	}
}

// waitPrecise sleeps until t, yielding instead of sleeping for the last
// stretch. Timers still wake late now and then (macOS coalesces them by
// several milliseconds); that lateness is recorded as scheduling lag.
func waitPrecise(ctx context.Context, t time.Time) {
	const spin = 300 * time.Microsecond
	if d := time.Until(t); d > spin {
		if !sleepUntil(ctx, t.Add(-spin)) {
			return
		}
	}
	for time.Now().Before(t) {
		runtime.Gosched()
	}
}
