// Package prom exposes a running test on a Prometheus /metrics endpoint so
// it can be graphed in Grafana next to the target's own metrics.
//
// Counters are running totals; latency gauges cover the most recent second.
package prom

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/run"
)

type keyTotals struct {
	count, errors, bytes int64
	errKinds             map[string]int64
	last                 metrics.Stats
	lastCount            int64
}

// Exporter is a run.Sink that serves /metrics.
type Exporter struct {
	srv  *http.Server
	ln   net.Listener
	test string

	mu       sync.Mutex
	keys     map[string]*keyTotals
	dropped  int64
	inFlight int64
	users    int64
	target   float64
	cpu      float64
	lagP99   int64
	elapsed  int
	running  bool
}

// New starts serving on addr (e.g. ":9464").
func New(addr, testName string) (*Exporter, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	e := &Exporter{ln: ln, test: testName, keys: map[string]*keyTotals{}, running: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", e.serve)
	e.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = e.srv.Serve(ln) }()
	return e, nil
}

// Addr is the listening address.
func (e *Exporter) Addr() string { return e.ln.Addr().String() }

// Snapshot implements run.Sink.
func (e *Exporter) Snapshot(s *metrics.Snapshot, _ *metrics.Accumulator) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dropped += s.Dropped
	e.inFlight, e.users, e.target = s.InFlight, s.Users, s.TargetRate
	e.cpu, e.lagP99, e.elapsed = s.Gen.CPU, s.Gen.LagP99, s.T
	for _, kt := range e.keys {
		kt.lastCount = 0
	}
	for k, ks := range s.Keys {
		kt := e.keys[k]
		if kt == nil {
			kt = &keyTotals{errKinds: map[string]int64{}}
			e.keys[k] = kt
		}
		kt.count += ks.Count
		kt.errors += ks.Errors
		kt.bytes += ks.Bytes
		for kind, n := range ks.ErrKinds {
			kt.errKinds[kind] += n
		}
		kt.last, kt.lastCount = ks.Stats, ks.Count
	}
}

// Done implements run.Sink.
func (e *Exporter) Done(*run.Result) {
	e.mu.Lock()
	e.running = false
	e.inFlight, e.users, e.target = 0, 0, 0
	e.mu.Unlock()
}

// Close stops the server.
func (e *Exporter) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = e.srv.Shutdown(ctx)
}

func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func (e *Exporter) serve(w http.ResponseWriter, _ *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	t := esc(e.test)
	help := func(name, typ, text string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ)
	}
	names := make([]string, 0, len(e.keys))
	for k := range e.keys {
		names = append(names, k)
	}
	sort.Strings(names)
	lbl := func(k string) string {
		sc, st := metrics.SplitKey(k)
		return fmt.Sprintf(`test="%s",scenario="%s",step="%s"`, t, esc(sc), esc(st))
	}

	help("lt_requests_total", "counter", "Completed requests (and iterations, step=_iteration).")
	for _, k := range names {
		fmt.Fprintf(&b, "lt_requests_total{%s} %d\n", lbl(k), e.keys[k].count)
	}
	help("lt_errors_total", "counter", "Failed requests by error kind.")
	for _, k := range names {
		kinds := make([]string, 0, len(e.keys[k].errKinds))
		for kind := range e.keys[k].errKinds {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			fmt.Fprintf(&b, "lt_errors_total{%s,kind=\"%s\"} %d\n", lbl(k), esc(kind), e.keys[k].errKinds[kind])
		}
	}
	help("lt_received_bytes_total", "counter", "Response bytes received.")
	for _, k := range names {
		fmt.Fprintf(&b, "lt_received_bytes_total{%s} %d\n", lbl(k), e.keys[k].bytes)
	}
	help("lt_latency_seconds", "gauge", "Latency over the last second (coordinated-omission corrected).")
	for _, k := range names {
		kt := e.keys[k]
		if kt.lastCount == 0 {
			continue
		}
		for _, q := range []struct {
			name string
			v    int64
		}{{"0.5", kt.last.P50}, {"0.9", kt.last.P90}, {"0.95", kt.last.P95}, {"0.99", kt.last.P99}, {"1", kt.last.Max}} {
			fmt.Fprintf(&b, "lt_latency_seconds{%s,quantile=\"%s\"} %g\n", lbl(k), q.name, float64(q.v)/1e6)
		}
	}
	help("lt_dropped_total", "counter", "Arrivals dropped because maxInFlight was reached.")
	fmt.Fprintf(&b, "lt_dropped_total{test=\"%s\"} %d\n", t, e.dropped)
	help("lt_in_flight", "gauge", "Iterations running now.")
	fmt.Fprintf(&b, "lt_in_flight{test=\"%s\"} %d\n", t, e.inFlight)
	help("lt_users", "gauge", "Active users (closed-model executors).")
	fmt.Fprintf(&b, "lt_users{test=\"%s\"} %d\n", t, e.users)
	help("lt_target_rate", "gauge", "Planned arrivals per second.")
	fmt.Fprintf(&b, "lt_target_rate{test=\"%s\"} %g\n", t, e.target)
	help("lt_generator_cpu_percent", "gauge", "Load generator CPU, percent of all cores.")
	fmt.Fprintf(&b, "lt_generator_cpu_percent{test=\"%s\"} %g\n", t, e.cpu)
	help("lt_scheduling_lag_seconds", "gauge", "p99 of how late iterations started, last second.")
	fmt.Fprintf(&b, "lt_scheduling_lag_seconds{test=\"%s\"} %g\n", t, float64(e.lagP99)/1e6)
	help("lt_elapsed_seconds", "gauge", "Seconds since the test started.")
	fmt.Fprintf(&b, "lt_elapsed_seconds{test=\"%s\"} %d\n", t, e.elapsed)
	help("lt_running", "gauge", "1 while the test runs.")
	running := 0
	if e.running {
		running = 1
	}
	fmt.Fprintf(&b, "lt_running{test=\"%s\"} %d\n", t, running)
	_, _ = w.Write([]byte(b.String()))
}
