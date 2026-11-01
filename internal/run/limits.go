package run

import (
	"fmt"
	"math"
	"sync"

	"github.com/ymg4798/load-testing/internal/metrics"
)

// Stop says why a test ended before its plan.
type Stop struct {
	T      int     `json:"t"`      // second the limit failed
	Reason string  `json:"reason"` // e.g. "latency p95 < 1s (actual 1.31s)"
	Rate   float64 `json:"rate"`   // target rate (or users) at that second
	// LastGood is the target rate (or users) at the end of the last window
	// in which every limit held: the highest load the target sustained.
	LastGood float64 `json:"lastGood"`
}

// Limiter judges limits over the last few seconds while the test runs and
// calls cancel at the first failure.
type Limiter struct {
	limits []*metrics.Threshold
	window int
	cancel func()

	mu       sync.Mutex
	stop     *Stop
	lastGood float64
}

// NewLimiter returns nil when there are no limits.
func NewLimiter(limits map[string][]string, windowSec int, cancel func()) (*Limiter, error) {
	ls, err := metrics.ParseThresholds(limits)
	if err != nil || len(ls) == 0 {
		return nil, err
	}
	if windowSec < 1 {
		windowSec = 1
	}
	return &Limiter{limits: ls, window: windowSec, cancel: cancel}, nil
}

// loadOf is the load at a second: target rate, or users for closed models.
func loadOf(s *metrics.Snapshot) float64 {
	if s.TargetRate > 0 {
		return s.TargetRate
	}
	return float64(s.Users)
}

// Snapshot implements Sink.
func (l *Limiter) Snapshot(s *metrics.Snapshot, acc *metrics.Accumulator) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop != nil || len(acc.Snapshots) < l.window {
		return
	}
	win := metrics.NewAccumulator()
	for _, x := range acc.Snapshots[len(acc.Snapshots)-l.window:] {
		if win.Add(x) != nil {
			return
		}
	}
	for _, r := range metrics.Evaluate(l.limits, win) {
		if r.Pass || math.IsNaN(r.Value) {
			continue
		}
		l.stop = &Stop{
			T:        s.T,
			Reason:   fmt.Sprintf("%s %s (actual %s over the last %ds)", r.Metric, r.Expr, r.Actual, l.window),
			Rate:     loadOf(s),
			LastGood: l.lastGood,
		}
		l.cancel()
		return
	}
	l.lastGood = loadOf(s)
}

// Done implements Sink.
func (l *Limiter) Done(*Result) {}

// Stopped returns why the test stopped early, or nil.
func (l *Limiter) Stopped() *Stop {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stop
}
