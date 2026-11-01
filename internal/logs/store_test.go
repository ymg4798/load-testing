package logs

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/run"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestStore(o Options) (*Store, *clock) {
	c := &clock{t: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)}
	s := NewStore(o)
	s.now = c.now
	return s, c
}

func TestStoreRingAndSearch(t *testing.T) {
	s, _ := newTestStore(Options{MaxLines: 100})
	for i := range 250 {
		lvl := "info"
		if i%10 == 0 {
			lvl = "error"
		}
		s.Add(run.LogLine{Source: "a", Level: lvl, Message: fmt.Sprintf("line %d", i)})
	}
	st := s.Stats()
	if st.Total != 250 || st.Held != 100 {
		t.Fatalf("stats %+v", st)
	}
	got := s.Search("line", 3)
	if len(got) != 3 || got[0].Message != "line 249" || got[2].Message != "line 247" {
		t.Errorf("newest first: %+v", got)
	}
	if got := s.Search("level:error", 0); len(got) != 10 || got[0].Message != "line 240" {
		t.Errorf("errors held: %d %+v", len(got), got)
	}
	if got := s.Search(`"line 5"`, 10); len(got) != 0 {
		t.Errorf("evicted line found: %+v", got)
	}
	if got := s.Search("/(/", 10); len(got) != 0 {
		t.Errorf("bad regex searched as text: %v", got)
	}

	// Byte limit.
	b, _ := newTestStore(Options{MaxBytes: 10 * (recOverhead + 100)})
	for range 50 {
		b.Add(run.LogLine{Message: strings.Repeat("x", 100)})
	}
	if st := b.Stats(); st.Held > 10 || st.Held < 9 || st.Bytes > 10*(recOverhead+100) {
		t.Errorf("byte cap: %+v", st)
	}
}

func TestStoreCount(t *testing.T) {
	s, c := newTestStore(Options{MaxLines: 10})
	start := c.now()
	for i := range 30 {
		lvl := "info"
		if i%3 == 0 {
			lvl = "error"
		}
		s.Add(run.LogLine{Level: lvl, Message: fmt.Sprintf("m%d", i)})
		c.add(time.Second)
	}
	// Level-only counts come from per-second counters, beyond what is held.
	if n := s.Count("level:error", start); n != 10 {
		t.Errorf("errors since start = %d", n)
	}
	if n := s.Count("", start.Add(20*time.Second)); n != 10 {
		t.Errorf("all since 20s = %d", n)
	}
	// Text counts scan what is held (the last 10).
	if n := s.Count("m2", start); n != 10 { // m2 is evicted; m20..m29 are held
		t.Errorf("text count = %d", n)
	}
	if n := s.Count("m29", start.Add(29*time.Second)); n != 1 {
		t.Errorf("m29 = %d", n)
	}
}

func TestStoreDecorate(t *testing.T) {
	s, c := newTestStore(Options{})
	snap := &metrics.Snapshot{}
	s.Decorate(snap)
	if snap.Server != nil || s.Series() != nil {
		t.Fatal("series before any line")
	}
	for i := range 30 {
		lvl := "info"
		if i < 5 {
			lvl = "error"
		} else if i < 7 {
			lvl = "fatal"
		}
		s.Add(run.LogLine{Level: lvl})
	}
	c.add(time.Second)
	snap = &metrics.Snapshot{Server: map[string]float64{"cpu": 1}}
	s.Decorate(snap)
	if snap.Server["logs"] != 30 || snap.Server["log errors"] != 7 || snap.Server["cpu"] != 1 {
		t.Errorf("first second %v", snap.Server)
	}
	for range 20 {
		s.Add(run.LogLine{Level: "warn"})
	}
	c.add(2 * time.Second)
	snap = &metrics.Snapshot{}
	s.Decorate(snap)
	if snap.Server["logs"] != 10 || snap.Server["log errors"] != 0 {
		t.Errorf("2s window %v", snap.Server)
	}
	ser := s.Series()
	if len(ser) != 2 || ser[0].Unit != "/s" || ser[1].Name != "logs" {
		t.Errorf("series %+v", ser)
	}
}

func TestStoreFinish(t *testing.T) {
	s, c := newTestStore(Options{})
	const tr1, tr2, tr3 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccc"
	s.Add(run.LogLine{Source: "api", Level: "info", Message: "begin", TraceID: tr1})
	s.Add(run.LogLine{Source: "api", Level: "info", Message: "unrelated", TraceID: "dddddddddddddddddddddddddddddddd"})
	for i := range 3000 {
		c.add(time.Millisecond)
		s.Add(run.LogLine{Source: "api", Level: "error", Message: fmt.Sprintf("order %d failed: timeout after %dms", i, i%7), TraceID: tr2,
			Attrs: map[string]string{"stack": "at x"}})
	}
	s.Add(run.LogLine{Source: "worker", Level: "fatal", Message: `job "x-1" crashed`, TraceID: tr3})
	s.Add(run.LogLine{Source: "worker", Level: "fatal", Message: `job "y-2" crashed`})
	s.Add(run.LogLine{Source: "api", Level: "info", Message: "hitmap one", TraceID: strings.ToUpper(tr3)})
	s.Add(run.LogLine{Source: "api", Level: "debug", Message: "apm trace", TraceID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"})

	r := &run.Result{
		Summary:   &metrics.Summary{Keys: []*metrics.KeySummary{{Slowest: []metrics.Exemplar{{TraceID: strings.ToUpper(tr1)}}}}},
		Snapshots: []*metrics.Snapshot{{Hits: &metrics.Hits{Samples: []metrics.HitSample{{TraceID: tr3}}}}},
		APM: &run.APMReport{
			Traces: []run.Trace{{TraceID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}},
			Errors: []run.ErrorGroup{{Service: "api", Source: "span", Message: "500", Count: 1}},
		},
	}
	s.Finish(r)
	rep := r.Logs
	if rep == nil || rep.Total != 3006 || rep.ByLevel["error"] != 3000 || rep.ByLevel["fatal"] != 2 || rep.ByLevel["info"] != 3 || rep.ByLevel["debug"] != 1 {
		t.Fatalf("report %+v", rep)
	}
	if len(rep.Lines) > MaxReportLines {
		t.Errorf("%d lines", len(rep.Lines))
	}
	has := map[string]bool{}
	for i, l := range rep.Lines {
		has[l.Message] = true
		if i > 0 && rep.Lines[i-1].Time.After(l.Time) {
			t.Errorf("lines out of order at %d", i)
			break
		}
	}
	for _, m := range []string{"begin", `job "x-1" crashed`, `job "y-2" crashed`, "hitmap one", "apm trace", "order 0 failed: timeout after 0ms", "order 2999 failed: timeout after 3ms"} {
		if !has[m] {
			t.Errorf("missing line %q", m)
		}
	}
	if has["unrelated"] {
		t.Error("unrelated info line kept")
	}
	// APM errors: the span group stays, log groups are appended.
	errs := r.APM.Errors
	if len(errs) != 3 || errs[0].Source != "span" {
		t.Fatalf("apm errors %+v", errs)
	}
	g := errs[1]
	if g.Service != "api" || g.Source != "log" || g.Message != "order <n> failed: timeout after <n>" || g.Count != 3000 || g.TraceID != tr2 || g.Stack != "at x" || !g.Last.After(g.First) {
		t.Errorf("group %+v", g)
	}
	if w := errs[2]; w.Service != "worker" || w.Message != `job "…" crashed` || w.Count != 2 || w.TraceID != tr3 {
		t.Errorf("worker group %+v", w)
	}

	// No APM report: one is created for the log groups.
	r2 := &run.Result{}
	s.Finish(r2)
	if r2.APM == nil || len(r2.APM.Errors) != 2 {
		t.Errorf("apm created %+v", r2.APM)
	}
	// Nothing logged: nothing added.
	empty, _ := newTestStore(Options{})
	r3 := &run.Result{}
	empty.Finish(r3)
	if r3.Logs != nil || r3.APM != nil {
		t.Error("empty store changed the result")
	}
}

func TestStoreConcurrent(t *testing.T) {
	s := NewStore(Options{MaxLines: 5000})
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 5000 {
				s.Add(run.LogLine{Source: fmt.Sprint(w), Level: "error", Message: fmt.Sprintf("e %d", i)})
			}
		}()
	}
	for range 50 {
		s.Search("level:error e 1", 10)
		s.Count("e", time.Now().Add(-time.Minute))
		s.Decorate(&metrics.Snapshot{})
	}
	wg.Wait()
	if s.Stats().Total != 20000 {
		t.Error(s.Stats())
	}
}

func BenchmarkParseAndAdd(b *testing.B) {
	s := NewStore(Options{})
	p := &Parser{Source: "bench"}
	lines := []string{
		`{"time":"2026-11-01T10:00:00.5Z","level":"info","msg":"GET /items 200","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","dur":1.2}`,
		`time=2026-11-01T10:00:00Z level=warn msg="slow query" dur=120ms`,
		`2026-11-01 10:00:00,123 ERROR [http-1] checkout failed for order 1234`,
	}
	b.ReportAllocs()
	batch := make([]run.LogLine, 0, 256)
	for i := 0; b.Loop(); i++ {
		batch = append(batch, p.Feed(lines[i%len(lines)])...)
		if len(batch) == cap(batch) {
			s.AddBatch(batch)
			batch = batch[:0]
		}
	}
}
