package metrics

import (
	"math/rand/v2"
	"runtime"
	"sync"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// Sample is one measured request (or iteration).
type Sample struct {
	Key     string
	Latency time.Duration
	Bytes   int64
	Err     string // empty when the request succeeded
	TraceID string // set when tracing is on; kept for the slowest and failed requests
	lag     bool
}

// Gauges are read from the engine once per second.
type Gauges struct {
	InFlight   int64
	Users      int64
	TargetRate float64
	Dropped    int64   // running total; the aggregator turns it into a per-second count
	Active     []int64 // in-flight iterations per active-status bucket
}

type bucket struct {
	count, errors, bytes int64
	errKinds             map[string]int64
	hist                 *hdr.Histogram
	slow                 *Exemplar
	failed               []Exemplar
}

// Aggregator collects samples from many goroutines on one goroutine and
// emits a Snapshot every second. Recording only sends on a channel, so the
// hot path never takes a lock.
type Aggregator struct {
	ch     chan Sample
	out    chan<- *Snapshot
	gauges func() Gauges
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	cur         map[string]*bucket
	hits        *Hits
	secStart    time.Time
	rnd         *rand.Rand
	lag         *hdr.Histogram
	t           int
	lastDropped int64
	lastCPU     time.Duration
	lastWall    time.Time
}

// NewAggregator creates an aggregator that sends snapshots to out.
// gauges may be nil.
func NewAggregator(gauges func() Gauges, out chan<- *Snapshot) *Aggregator {
	if gauges == nil {
		gauges = func() Gauges { return Gauges{} }
	}
	return &Aggregator{
		ch:     make(chan Sample, 1<<16),
		out:    out,
		gauges: gauges,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		cur:    map[string]*bucket{},
		lag:    NewHist(),
		rnd:    rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 7)),
	}
}

// Record adds one sample. Safe for concurrent use.
func (a *Aggregator) Record(s Sample) { a.ch <- s }

// RecordLag records how late the scheduler started an iteration.
func (a *Aggregator) RecordLag(d time.Duration) { a.ch <- Sample{Latency: d, lag: true} }

// Run processes samples until Stop is called. It emits a final partial
// snapshot and closes out when it returns.
func (a *Aggregator) Run() {
	defer close(a.done)
	defer close(a.out)
	a.lastWall, a.lastCPU = time.Now(), processCPU()
	a.secStart = a.lastWall
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case s := <-a.ch:
			a.add(s)
		case <-tick.C:
			a.flush()
		case <-a.stop:
			for {
				select {
				case s := <-a.ch:
					a.add(s)
				default:
					a.flush()
					return
				}
			}
		}
	}
}

// Stop ends Run after all samples sent so far are processed, and waits for it.
func (a *Aggregator) Stop() {
	a.once.Do(func() { close(a.stop) })
	<-a.done
}

func (a *Aggregator) add(s Sample) {
	us := s.Latency.Microseconds()
	if us < histMin {
		us = histMin
	} else if us > histMax {
		us = histMax
	}
	if s.lag {
		_ = a.lag.RecordValue(us)
		return
	}
	b := a.cur[s.Key]
	if b == nil {
		b = &bucket{hist: NewHist()}
		a.cur[s.Key] = b
	}
	if !IsInternal(s.Key) {
		if a.hits == nil {
			a.hits = &Hits{}
		}
		off := int(time.Since(a.secStart).Milliseconds())
		a.hits.Add(HitSample{Key: s.Key, Off: min(max(off, 0), 999), Latency: us, Err: s.Err, TraceID: s.TraceID}, a.rnd)
	}
	b.count++
	b.bytes += s.Bytes
	if s.Err != "" {
		b.errors++
		if b.errKinds == nil {
			b.errKinds = map[string]int64{}
		}
		b.errKinds[s.Err]++
	}
	_ = b.hist.RecordValue(us)
	if s.TraceID != "" {
		if b.slow == nil || us > b.slow.Latency {
			b.slow = &Exemplar{Latency: us, TraceID: s.TraceID, Err: s.Err}
		}
		if s.Err != "" && len(b.failed) < maxExemplars {
			b.failed = append(b.failed, Exemplar{Latency: us, TraceID: s.TraceID, Err: s.Err})
		}
	}
}

func (a *Aggregator) flush() {
	a.t++
	g := a.gauges()
	now, cpu := time.Now(), processCPU()
	wall := now.Sub(a.lastWall)
	cpuPct := 0.0
	if wall > 0 {
		cpuPct = float64(cpu-a.lastCPU) / float64(wall) / float64(runtime.NumCPU()) * 100
	}
	a.lastWall, a.lastCPU = now, cpu

	snap := &Snapshot{
		T:          a.t,
		Keys:       make(map[string]*KeySnap, len(a.cur)),
		Dropped:    g.Dropped - a.lastDropped,
		InFlight:   g.InFlight,
		Users:      g.Users,
		TargetRate: g.TargetRate,
		Active:     g.Active,
		Hits:       a.hits.Trim(),
	}
	a.hits, a.secStart = nil, now
	a.lastDropped = g.Dropped
	for k, b := range a.cur {
		if b.count == 0 {
			continue
		}
		enc, _ := EncodeHist(b.hist)
		ks := &KeySnap{
			Count: b.count, Errors: b.errors, Bytes: b.bytes, ErrKinds: b.errKinds,
			Stats: StatsOf(b.hist), Hist: enc, Slow: b.slow, Failed: b.failed,
		}
		if ks.Slow != nil {
			ks.Slow.T = a.t
		}
		for i := range ks.Failed {
			ks.Failed[i].T = a.t
		}
		snap.Keys[k] = ks
		// Reuse the histogram; drop the rest of the bucket.
		b.hist.Reset()
		b.count, b.errors, b.bytes, b.errKinds, b.slow, b.failed = 0, 0, 0, nil, nil, nil
	}
	ls := StatsOf(a.lag)
	if a.lag.TotalCount() > 0 {
		snap.LagHist, _ = EncodeHist(a.lag)
	}
	a.lag.Reset()
	snap.Gen = GenStats{CPU: cpuPct, LagP99: ls.P99, LagMax: ls.Max, Goroutines: runtime.NumGoroutine()}
	a.out <- snap
}
