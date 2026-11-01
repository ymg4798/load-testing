package store

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
)

// benchSeries is the number of server series in the synthetic week, about
// what a host monitor plus an APM collector report.
const benchSeries = 20

var benchNames = func() []string {
	n := make([]string, benchSeries)
	for i := range n {
		n[i] = fmt.Sprintf("host.metric_%02d", i)
	}
	return n
}()

// synth is a realistic watch second: noisy gauges, a hitmap of a few
// hundred requests over ~15 buckets, 60 samples, active counts.
func synth(rnd *rand.Rand, i int) *metrics.Snapshot {
	s := &metrics.Snapshot{T: i + 1, Server: make(map[string]float64, benchSeries)}
	for j, k := range benchNames {
		s.Server[k] = float64(j*10) + rnd.Float64()*5
	}
	h := &metrics.Hits{}
	a := &metrics.Hits{}
	for range 300 {
		lat := int64(rnd.ExpFloat64() * 20000)
		hs := metrics.HitSample{Key: "svc|GET /api/items", Off: rnd.IntN(1000), Latency: lat}
		if rnd.IntN(100) == 0 {
			hs.Err = "500"
		}
		h.Add(hs, rnd)
		a.Add(hs, rnd)
	}
	s.Hits, s.APMHits = h.Trim(), a.Trim()
	s.Active = []int64{int64(rnd.IntN(20)), int64(rnd.IntN(3)), 0, 0}
	return s
}

const week = 7 * 24 * 3600

var (
	weekOnce sync.Once
	weekDir  string
	weekEnd  time.Time
	weekGen  time.Duration
	weekSize int64
	keepDir  string
)

// weekStore writes 7 days of seconds once per test binary run.
func weekStore(b *testing.B) string {
	weekOnce.Do(func() {
		// LT_STORE_BENCH_DIR keeps the generated week between runs.
		if d := os.Getenv("LT_STORE_BENCH_DIR"); d != "" {
			if raw, err := os.ReadFile(filepath.Join(d, "end")); err == nil {
				if sec, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
					weekEnd, keepDir = time.Unix(sec, 0), d
					return
				}
			}
		}
		dir, err := os.MkdirTemp("", "lt-store-bench-")
		if err != nil {
			b.Fatal(err)
		}
		weekDir = dir
		s, err := Open(dir, Options{NoSync: true})
		if err != nil {
			b.Fatal(err)
		}
		weekEnd = time.Now().Truncate(time.Second)
		start := weekEnd.Add(-week * time.Second)
		rnd := rand.New(rand.NewPCG(1, 2))
		t0 := time.Now()
		for i := range week {
			if err := s.Append(start.Add(time.Duration(i)*time.Second), synth(rnd, i)); err != nil {
				b.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
		weekGen = time.Since(t0)
		var seg, min int64
		filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				if filepath.Ext(p) == ".min" {
					min += fi.Size()
				} else {
					seg += fi.Size()
				}
			}
			return nil
		})
		weekSize = seg + min
		if d := os.Getenv("LT_STORE_BENCH_DIR"); d != "" {
			os.RemoveAll(d)
			if os.Rename(dir, d) == nil {
				weekDir, keepDir = "", d
				os.WriteFile(filepath.Join(d, "end"), []byte(strconv.FormatInt(weekEnd.Unix(), 10)), 0o644)
			}
		}
		fmt.Printf("week: %d seconds, %d series, written in %v (%.1fµs/append); disk %.1f MiB/day (seconds %.1f, minute index %.2f)\n",
			week, benchSeries, weekGen.Round(time.Millisecond), float64(weekGen.Microseconds())/week,
			float64(weekSize)/7/(1<<20), float64(seg)/7/(1<<20), float64(min)/7/(1<<20))
	})
	if keepDir != "" {
		return keepDir
	}
	return weekDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if weekDir != "" {
		os.RemoveAll(weekDir)
	}
	os.Exit(code)
}

func benchRange(b *testing.B, span time.Duration) {
	dir := weekStore(b)
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ResetTimer()
	for b.Loop() {
		pts, err := s.Range(weekEnd.Add(-span), weekEnd, 1000)
		if err != nil || len(pts) == 0 {
			b.Fatal(err, len(pts))
		}
	}
}

func BenchmarkRange7dTo1000(b *testing.B)  { benchRange(b, 7*24*time.Hour) }
func BenchmarkRange1dTo1000(b *testing.B)  { benchRange(b, 24*time.Hour) }
func BenchmarkRange1hTo1000(b *testing.B)  { benchRange(b, time.Hour) }    // seconds path
func BenchmarkRange12hTo1000(b *testing.B) { benchRange(b, 12*time.Hour) } // minute path, 1-min buckets

func BenchmarkLatest3600(b *testing.B) {
	s, err := Open(weekStore(b), Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ResetTimer()
	for b.Loop() {
		if pts, err := s.Latest(3600); err != nil || len(pts) != 3600 {
			b.Fatal(err, len(pts))
		}
	}
}

func BenchmarkAppend(b *testing.B) {
	s, err := Open(b.TempDir(), Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	rnd := rand.New(rand.NewPCG(1, 2))
	snaps := make([]*metrics.Snapshot, 100)
	for i := range snaps {
		snaps[i] = synth(rnd, i)
	}
	at := time.Now().Add(-24 * time.Hour)
	i := 0
	for b.Loop() {
		if err := s.Append(at.Add(time.Duration(i)*time.Second), snaps[i%100]); err != nil {
			b.Fatal(err)
		}
		i++
	}
}
