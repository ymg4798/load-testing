package metrics

import (
	"testing"
	"time"
)

func snapWith(t int, key string, lat []int64, errs int) *Snapshot {
	h := NewHist()
	for _, v := range lat {
		_ = h.RecordValue(v)
	}
	enc, _ := EncodeHist(h)
	return &Snapshot{T: t, Keys: map[string]*KeySnap{
		key: {Count: int64(len(lat)), Errors: int64(errs), Stats: StatsOf(h), Hist: enc},
	}}
}

func TestEncodeRoundTrip(t *testing.T) {
	h := NewHist()
	for i := int64(1); i <= 1000; i++ {
		_ = h.RecordValue(i * 100)
	}
	s, err := EncodeHist(h)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := DecodeHist(s)
	if err != nil {
		t.Fatal(err)
	}
	if h2.TotalCount() != 1000 || h2.ValueAtQuantile(99) != h.ValueAtQuantile(99) {
		t.Fatalf("round trip changed the histogram")
	}
}

func TestMergeAndSummary(t *testing.T) {
	k := Key("s", "req")
	a := snapWith(1, k, []int64{1000, 2000, 3000}, 1)
	b := snapWith(1, k, []int64{4000}, 0)
	m, err := Merge([]*Snapshot{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if m.Keys[k].Count != 4 || m.Keys[k].Errors != 1 || m.Keys[k].Max < 3990 || m.Gen.Agents != 2 {
		t.Fatalf("merged %+v", m.Keys[k])
	}
	acc := NewAccumulator()
	_ = acc.Add(m)
	_ = acc.Add(snapWith(2, Key("s", IterationStep), []int64{5000}, 0))
	sum := acc.Summary()
	if sum.Requests != 4 || sum.Iterations != 1 || sum.Errors != 1 {
		t.Fatalf("summary %+v", sum)
	}
}

func TestThresholds(t *testing.T) {
	acc := NewAccumulator()
	lat := make([]int64, 100)
	for i := range lat {
		lat[i] = int64(i+1) * 1000 // 1ms..100ms
	}
	_ = acc.Add(snapWith(1, Key("s", "login"), lat, 2))
	_ = acc.Add(snapWith(1, Key("s", "other"), []int64{500_000}, 0))
	ths, err := ParseThresholds(map[string][]string{
		"latency{step:login}": {"p95 < 100ms", "p50 < 10ms"},
		"errors":              {"rate < 1%"},
		"errors{step:login}":  {"count <= 2"},
		"latency":             {"max < 1s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"p95 < 100ms": true, "p50 < 10ms": false, "rate < 1%": false, "count <= 2": true, "max < 1s": true}
	for _, r := range Evaluate(ths, acc) {
		if r.Pass != want[r.Expr] {
			t.Errorf("%s %s: pass=%v (actual %s)", r.Metric, r.Expr, r.Pass, r.Actual)
		}
	}
	for _, bad := range []map[string][]string{
		{"latency": {"p95 300ms"}},
		{"latency": {"bogus < 3ms"}},
		{"nope": {"rate < 1"}},
		{"latency{host:x}": {"p95 < 1s"}},
	} {
		if _, err := ParseThresholds(bad); err == nil {
			t.Errorf("%v: expected error", bad)
		}
	}
}

func TestAggregator(t *testing.T) {
	out := make(chan *Snapshot, 10)
	a := NewAggregator(nil, out)
	go a.Run()
	for i := 0; i < 50; i++ {
		a.Record(Sample{Key: "s|x", Latency: time.Millisecond})
	}
	a.Record(Sample{Key: "s|x", Latency: time.Millisecond, Err: "timeout"})
	a.Stop()
	var n, e int64
	for s := range out {
		if ks := s.Keys["s|x"]; ks != nil {
			n += ks.Count
			e += ks.Errors
			if ks.ErrKinds != nil && ks.ErrKinds["timeout"] != 1 {
				t.Fatalf("err kinds %v", ks.ErrKinds)
			}
		}
	}
	if n != 51 || e != 1 {
		t.Fatalf("count %d errors %d", n, e)
	}
}
