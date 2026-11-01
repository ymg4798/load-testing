// Package engine runs scenario files: it compiles flows into steps, drives
// them with the configured executors and feeds measurements to the
// metrics aggregator.
package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/protocol"
	"github.com/ymg4798/load-testing/internal/scenario"
	"github.com/ymg4798/load-testing/internal/script"

	// Step kinds available to scenario files.
	_ "github.com/ymg4798/load-testing/internal/protocol/builtin"
	_ "github.com/ymg4798/load-testing/internal/protocol/graphqlstep"
	_ "github.com/ymg4798/load-testing/internal/protocol/grpcstep"
	_ "github.com/ymg4798/load-testing/internal/protocol/httpstep"
	_ "github.com/ymg4798/load-testing/internal/protocol/redisstep"
	_ "github.com/ymg4798/load-testing/internal/protocol/sqlstep"
	_ "github.com/ymg4798/load-testing/internal/protocol/tcpstep"
	_ "github.com/ymg4798/load-testing/internal/protocol/wsstep"
)

// Engine runs one scenario file once.
type Engine struct {
	file    *scenario.File
	agg     *metrics.Aggregator
	runners []*runner

	dropped  atomic.Int64
	inFlight atomic.Int64
	users    atomic.Int64
	started  atomic.Pointer[time.Time]
	elapsed  time.Duration
}

type compiledStep struct {
	step  protocol.Step
	once  bool
	label string // shown in the active-transaction list
}

type runner struct {
	e       *Engine
	name    string
	sc      *scenario.Scenario
	steps   []compiledStep
	env     *protocol.Env
	globals map[string]any
	sched   *schedule
	vuSeq   atomic.Int64
	abort   bool

	slotsMu sync.Mutex
	slots   []*slot // every slot ever made, for the active list
}

// New compiles the file. Snapshots are sent to out every second while the
// engine runs; out is closed when Run returns.
func New(f *scenario.File, out chan<- *metrics.Snapshot) (*Engine, error) {
	e := &Engine{file: f}
	globals := maps.Clone(f.Vars)
	if f.Setup != "" {
		extra, err := script.RunSetup(f.Dir, f.Setup, globals)
		if err != nil {
			return nil, err
		}
		maps.Copy(globals, extra)
	}
	for name, d := range f.Data {
		if err := d.Open(f.Dir); err != nil {
			return nil, fmt.Errorf("data %s: %w", name, err)
		}
	}
	for _, name := range f.ScenarioNames() {
		sc := f.Scenarios[name]
		r := &runner{e: e, name: name, sc: sc, globals: globals, abort: sc.OnError == "abort"}
		r.env = &protocol.Env{
			BaseDir:  f.Dir,
			Scenario: name,
			Options: protocol.Options{
				Timeout:         f.Options.Timeout.D(),
				Insecure:        f.Options.Insecure,
				HTTP2:           *f.Options.HTTP2,
				MaxConnsPerHost: f.Options.MaxConnsPerHost,
				NoKeepAlive:     f.Options.NoKeepAlive,
				Cookies:         f.Options.Cookies,
				MaxInFlight:     sc.MaxInFlight,
				Tracing:         f.Options.Tracing.Protocol(),
			},
		}
		for _, st := range sc.Flow {
			if _, ok := st["script"]; ok {
				r.env.HasScript = true
			}
		}
		for i, st := range sc.Flow {
			cs, err := compileStep(st, r.env)
			if err != nil {
				return nil, fmt.Errorf("scenario %s, step %d: %w", name, i+1, err)
			}
			r.steps = append(r.steps, cs)
		}
		start := sc.StartRate
		if sc.Executor != scenario.ArrivalRate {
			start = float64(sc.StartUsers)
		}
		r.sched = newSchedule(start, sc.Stages)
		e.runners = append(e.runners, r)
	}
	e.agg = metrics.NewAggregator(e.gauges, out)
	return e, nil
}

func compileStep(raw map[string]any, env *protocol.Env) (compiledStep, error) {
	var cs compiledStep
	for k, v := range raw {
		if k == "once" {
			b, ok := v.(bool)
			if !ok {
				return cs, fmt.Errorf("once must be true or false")
			}
			cs.once = b
			continue
		}
		f, ok := protocol.Lookup(k)
		if !ok {
			return cs, fmt.Errorf("unknown step kind %q (known: %v)", k, protocol.Kinds())
		}
		cs.label = stepLabel(k, v)
		st, err := f(v, env)
		if err != nil {
			return cs, err
		}
		cs.step = st
	}
	return cs, nil
}

// stepLabel names a step for people: its name, else its URL or target.
func stepLabel(kind string, v any) string {
	switch x := v.(type) {
	case string:
		return kind + " " + x
	case map[string]any:
		for _, k := range []string{"name", "url", "method", "target"} {
			if s, ok := x[k].(string); ok && s != "" {
				return kind + " " + s
			}
		}
	}
	return kind
}

// Active lists in-flight iterations, longest running first.
func (e *Engine) Active(limit int) []metrics.ActiveTx {
	now := time.Now().UnixNano()
	var out []metrics.ActiveTx
	for _, r := range e.runners {
		r.slotsMu.Lock()
		for _, s := range r.slots {
			since := s.busySince.Load()
			if since == 0 {
				continue
			}
			tx := metrics.ActiveTx{Source: "load", Name: r.name, ElapsedMs: (now - since) / 1e6, Who: fmt.Sprintf("VU %d", s.vu.ID)}
			if l := s.step.Load(); l != nil {
				tx.Detail = *l
			}
			out = append(out, tx)
		}
		r.slotsMu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ElapsedMs > out[j].ElapsedMs })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// activeBuckets counts in-flight iterations by how long they have run.
func (e *Engine) activeBuckets() []int64 {
	out := make([]int64, len(metrics.ActiveBuckets)+1)
	now := time.Now().UnixNano()
	for _, r := range e.runners {
		r.slotsMu.Lock()
		for _, s := range r.slots {
			if since := s.busySince.Load(); since != 0 {
				out[metrics.ActiveBucket((now-since)/1e6)]++
			}
		}
		r.slotsMu.Unlock()
	}
	return out
}

// Duration is how long the test is planned to run.
func (e *Engine) Duration() time.Duration {
	var d time.Duration
	for _, r := range e.runners {
		if t := r.sc.TotalDuration(); t > d {
			d = t
		}
	}
	return d
}

// Elapsed is how long Run actually took (valid after Run returns).
func (e *Engine) Elapsed() time.Duration { return e.elapsed }

func (e *Engine) gauges() metrics.Gauges {
	g := metrics.Gauges{
		InFlight: e.inFlight.Load(),
		Users:    e.users.Load(),
		Dropped:  e.dropped.Load(),
		Active:   e.activeBuckets(),
	}
	if st := e.started.Load(); st != nil {
		for _, r := range e.runners {
			if r.sc.Executor == scenario.ArrivalRate {
				// Middle of the second this snapshot covers.
				t := time.Since(*st) - r.sc.StartAfter.D() - 500*time.Millisecond
				g.TargetRate += r.sched.At(t.Seconds())
			}
		}
	}
	return g
}

// Run executes every scenario and blocks until all have finished or ctx
// is canceled.
func (e *Engine) Run(ctx context.Context) error {
	start := time.Now()
	e.started.Store(&start)
	go e.agg.Run()
	var wg sync.WaitGroup
	for _, r := range e.runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.sc.Executor == scenario.ArrivalRate {
				r.runArrival(ctx, start)
			} else {
				r.runUsers(ctx, start)
			}
		}()
	}
	wg.Wait()
	e.elapsed = time.Since(start)
	for _, r := range e.runners {
		r.env.Close()
	}
	e.agg.Stop()
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return nil
}

// slot is a VU plus the lateness of its current iteration, which is added
// to the first measurement of that iteration (coordinated-omission correction).
type slot struct {
	vu  *protocol.VU
	lag time.Duration
	// For the active list: when the current iteration started (0 = idle)
	// and the step it is on.
	busySince atomic.Int64
	step      atomic.Pointer[string]
}

func (r *runner) newSlot() *slot {
	id := int(r.vuSeq.Add(1))
	s := &slot{}
	key := func(step string) string { return metrics.Key(r.name, step) }
	s.vu = protocol.NewVU(id, r.name, r.globals, func(res protocol.Result) {
		if res.Err == "canceled" {
			return // stopped by us, not a failure of the target
		}
		lat := res.Latency
		if s.lag > 0 && r.e.file.Options.CorrectLag {
			lat += s.lag
			s.lag = 0
		}
		r.e.agg.Record(metrics.Sample{Key: key(res.Step), Latency: lat, Bytes: res.Bytes, Err: res.Err, TraceID: res.TraceID})
	})
	r.slotsMu.Lock()
	r.slots = append(r.slots, s)
	r.slotsMu.Unlock()
	return s
}

var errDataExhausted = errors.New("data rows exhausted")

// iterate runs the flow once. scheduled is zero for closed-model executors.
func (r *runner) iterate(ctx context.Context, s *slot, scheduled time.Time) error {
	vu := s.vu
	vu.Iter++
	begin := scheduled
	if begin.IsZero() {
		begin = time.Now()
	}
	for name, d := range r.e.file.Data {
		row, ok := d.Next(vu.Rand)
		if !ok {
			r.e.agg.Record(metrics.Sample{Key: metrics.Key(r.name, metrics.IterationStep), Err: "data_exhausted"})
			return errDataExhausted
		}
		vu.Vars[name] = row
	}
	s.busySince.Store(begin.UnixNano())
	defer s.busySince.Store(0)
	failed := ""
	for i := range r.steps {
		cs := &r.steps[i]
		if cs.once && vu.Iter > 1 {
			continue
		}
		s.step.Store(&cs.label)
		if err := cs.step.Run(ctx, vu); err != nil {
			if failed == "" {
				failed = protocol.ClassifyErr(err)
			}
			if r.abort {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	vu.EndIteration()
	s.lag = 0
	if failed == "canceled" || (ctx.Err() != nil && failed == "") {
		return nil
	}
	r.e.agg.Record(metrics.Sample{Key: metrics.Key(r.name, metrics.IterationStep), Latency: time.Since(begin), Err: failed})
	return nil
}

// sleepUntil waits for t or ctx; false means ctx ended first.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitGraceful waits for wg, canceling iterations after the graceful period.
func waitGraceful(ctx context.Context, wg *sync.WaitGroup, grace time.Duration, cancel context.CancelFunc) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		cancel()
		<-done
	case <-ctx.Done():
		cancel()
		<-done
	}
}

// Validate compiles every flow without running setup or the test. Steps
// that need a server to compile (gRPC reflection) will contact it.
func Validate(f *scenario.File) error {
	for _, name := range f.ScenarioNames() {
		sc := f.Scenarios[name]
		env := &protocol.Env{BaseDir: f.Dir, Scenario: name, Options: protocol.Options{
			Timeout: f.Options.Timeout.D(), HTTP2: *f.Options.HTTP2, MaxInFlight: sc.MaxInFlight,
		}}
		for i, st := range sc.Flow {
			if _, err := compileStep(st, env); err != nil {
				env.Close()
				return fmt.Errorf("scenario %s, step %d: %w", name, i+1, err)
			}
		}
		env.Close()
	}
	return nil
}
