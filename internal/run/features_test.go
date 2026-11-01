package run

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/scenario"
	"github.com/ymg4798/lt/internal/yamlx"
)

func TestIterationsExecutor(t *testing.T) {
	srv, hits := target(t)
	f := load(t, `
scenarios:
  once:
    executor: iterations
    iterations: 7
    users: 3
    flow:
      - http: "{{ base }}/me"
`, nil, map[string]string{"base": srv.URL})
	res, err := Local(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := key(res, metrics.IterationStep); n != 7 {
		t.Fatalf("iterations = %d, want 7", n)
	}
	if hits.Load() != 7 {
		t.Fatalf("hits = %d, want 7", hits.Load())
	}
}

func TestTracingHeaders(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("traceparent"))
		mu.Unlock()
		if r.URL.Path == "/bad" {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	f := load(t, `
options:
  tracing: { url: "http://jaeger/trace/{traceId}" }
scenarios:
  s:
    executor: iterations
    iterations: 5
    flow:
      - http: "{{ base }}/ok"
      - http: "{{ base }}/bad"
`, nil, map[string]string{"base": srv.URL})
	res, err := Local(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	ids := map[string]bool{}
	for _, h := range seen {
		if !re.MatchString(h) {
			t.Fatalf("bad traceparent %q", h)
		}
		ids[h[3:35]] = true
	}
	if len(seen) != 10 || len(ids) != 10 {
		t.Fatalf("want 10 requests with distinct trace IDs, got %d / %d", len(seen), len(ids))
	}
	if res.Meta.TraceURL != "http://jaeger/trace/{traceId}" {
		t.Fatalf("trace url %q", res.Meta.TraceURL)
	}
	var slow, failed int
	for _, k := range res.Summary.Keys {
		slow += len(k.Slowest)
		failed += len(k.Failed)
		for _, e := range k.Failed {
			if !ids[e.TraceID] || e.Err != "status:500" {
				t.Fatalf("failed exemplar %+v", e)
			}
		}
	}
	if slow == 0 || failed != 5 {
		t.Fatalf("exemplars: slowest %d, failed %d (want >0, 5)", slow, failed)
	}
}

// snap builds a one-second snapshot with n requests of latency us each.
func snap(t int, load float64, n, errs int64, us int64) *metrics.Snapshot {
	h := metrics.NewHist()
	for i := int64(0); i < n; i++ {
		_ = h.RecordValue(us)
	}
	enc, _ := metrics.EncodeHist(h)
	ks := &metrics.KeySnap{Count: n, Errors: errs, Stats: metrics.StatsOf(h), Hist: enc}
	if errs > 0 {
		ks.ErrKinds = map[string]int64{"status:503": errs}
	}
	return &metrics.Snapshot{T: t, TargetRate: load, Keys: map[string]*metrics.KeySnap{"s|api": ks}}
}

func TestInsightsKneeAndErrors(t *testing.T) {
	acc := metrics.NewAccumulator()
	for i := 1; i <= 30; i++ {
		lat, errs := int64(5_000), int64(0)
		if i >= 20 {
			lat = 200_000 // knee at 20s
		}
		if i >= 25 {
			errs = 10 // errors from 25s
		}
		s := snap(i, float64(i*10), 100, errs, lat)
		s.Server = map[string]float64{"cpu": 40}
		if i >= 20 {
			s.Server["cpu"] = 95
		}
		if err := acc.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	r := &Result{Meta: Meta{Scenarios: []ScenarioInfo{{Executor: "arrival-rate"}}, Server: []ServerSeries{{Name: "cpu", Unit: "%"}}},
		Summary: acc.Summary(), Snapshots: acc.Snapshots}
	got := map[string]Insight{}
	for _, in := range Insights(r) {
		got[in.Kind] = in
	}
	if in, ok := got["latency_knee"]; !ok || in.T != 20 {
		t.Fatalf("knee: %+v", got)
	}
	if in, ok := got["error_onset"]; !ok || in.T != 25 || !strings.Contains(in.Detail, "status:503") {
		t.Fatalf("errors: %+v", in)
	}
	if _, ok := got["spike"]; ok {
		t.Fatal("the knee itself must not be reported as spikes")
	}
	if in, ok := got["server"]; !ok || !strings.Contains(in.Title, "40% → 95%") {
		t.Fatalf("server: %+v", in)
	}
}

func TestLimiterStops(t *testing.T) {
	stopped := false
	l, err := NewLimiter(map[string][]string{"latency": {"p95 < 100ms"}}, 3, func() { stopped = true })
	if err != nil || l == nil {
		t.Fatal(err)
	}
	acc := metrics.NewAccumulator()
	for i := 1; i <= 10 && !stopped; i++ {
		lat := int64(10_000)
		if i >= 6 {
			lat = 300_000
		}
		s := snap(i, float64(i*100), 50, 0, lat)
		_ = acc.Add(s)
		l.Snapshot(s, acc)
	}
	st := l.Stopped()
	if !stopped || st == nil {
		t.Fatal("limiter did not stop the test")
	}
	// The window is 3s, so p95 crosses 100ms once 1 of 3 seconds is slow.
	if st.T != 6 || st.LastGood != 500 || st.Rate != 600 {
		t.Fatalf("stop = %+v", st)
	}
}

func TestLimitsEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	f := load(t, `
options: { limitWindow: 1s }
scenarios:
  s:
    rate: 20
    duration: 30s
    flow:
      - http: "{{ base }}/"
limits:
  errors: ["rate < 50%"]
`, nil, map[string]string{"base": srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := NewLimiter(f.Limits, 1, cancel)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, _ := Local(ctx, f, l)
	if time.Since(start) > 10*time.Second {
		t.Fatal("limit did not stop the run early")
	}
	if res.Stopped == nil || res.Insights[0].Kind != "stopped" {
		t.Fatalf("stopped = %+v, insights = %+v", res.Stopped, res.Insights)
	}
}

func TestAlerter(t *testing.T) {
	var mu sync.Mutex
	var got []AlertEvent
	a, err := NewAlerter([]scenario.AlertRule{
		{Name: "slow", When: "latency p95 > 100ms", For: yamlx.Duration(2 * time.Second), Window: yamlx.Duration(time.Second), Severity: "critical"},
		{Name: "hot", When: "server{cpu} > 80", Window: yamlx.Duration(time.Second), Severity: "warning"},
	}, func(e AlertEvent, _ []string) { mu.Lock(); got = append(got, e); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	acc := metrics.NewAccumulator()
	for i := 1; i <= 10; i++ {
		lat, cpu := int64(10_000), 50.0
		if i >= 3 && i <= 6 {
			lat, cpu = 300_000, 95
		}
		s := snap(i, 100, 20, 0, lat)
		s.Server = map[string]float64{"cpu": cpu}
		_ = acc.Add(s)
		a.Snapshot(s, acc)
	}
	ev := a.Events()
	want := []string{"hot firing 3", "slow firing 4", "slow resolved 7", "hot resolved 7"}
	var have []string
	for _, e := range ev {
		have = append(have, fmt.Sprintf("%s %s %d", e.Name, e.State, e.T))
	}
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v", have, want)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("notified %d times", len(got))
	}
	if _, err := NewAlerter([]scenario.AlertRule{{When: "nonsense"}}, nil); err == nil {
		t.Fatal("bad rule accepted")
	}
}

func TestHitmapAndActive(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-release
		}
	}))
	defer srv.Close()
	f := load(t, `
scenarios:
  s:
    executor: iterations
    iterations: 3
    users: 3
    flow:
      - http: { name: wait, url: "{{ base }}/slow" }
`, nil, map[string]string{"base": srv.URL})
	var eng ActiveSource
	ready := make(chan struct{})
	done := make(chan *Result)
	go func() {
		res, _ := LocalWith(context.Background(), f, func(e ActiveSource) { eng = e; close(ready) })
		done <- res
	}()
	<-ready
	var act []metrics.ActiveTx
	for i := 0; i < 100 && len(act) < 3; i++ {
		time.Sleep(20 * time.Millisecond)
		act = eng.Active(10)
	}
	if len(act) != 3 || act[0].Detail != "http wait" || act[0].Source != "load" {
		t.Fatalf("active %+v", act)
	}
	time.Sleep(1100 * time.Millisecond) // let a snapshot see them in flight
	close(release)
	res := <-done
	var inflight int64
	var hits int32
	for _, s := range res.Snapshots {
		for _, v := range s.Active {
			inflight += v
		}
		if s.Hits != nil {
			for _, v := range s.Hits.OK {
				hits += v
			}
		}
	}
	if inflight == 0 || hits != 3 {
		t.Fatalf("active buckets %d, hitmap count %d", inflight, hits)
	}
}

type fakeLogs struct{ n int }

func (f *fakeLogs) Search(string, int) []LogLine { return nil }
func (f *fakeLogs) Count(q string, _ time.Time) int {
	if q == "level:error" {
		return f.n
	}
	return 0
}

func TestLogAlert(t *testing.T) {
	a, err := NewAlerter([]scenario.AlertRule{{Name: "errs", When: "log{level:error} > 5", Severity: "critical"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fl := &fakeLogs{}
	a.SetLogs(fl)
	a.Observe(1, nil)
	fl.n = 9
	a.Observe(2, nil)
	fl.n = 0
	a.Observe(3, nil)
	ev := a.Events()
	if len(ev) != 2 || ev[0].State != "firing" || ev[0].Value != "9 lines" || ev[1].State != "resolved" {
		t.Fatalf("events %+v", ev)
	}
}
