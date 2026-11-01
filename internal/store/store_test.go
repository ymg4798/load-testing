package store

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
)

// base is a fixed, minute- and hour-aligned time well inside retention
// of the tests that use the real clock relative to now.
func base() time.Time { return time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour) }

// snap is a deterministic second i.
func snap(i int) *metrics.Snapshot {
	ok := make([]int32, metrics.HitBuckets)
	ok[3] = int32(i)
	ok[10] = 2
	er := make([]int32, metrics.HitBuckets)
	er[20] = int32(i % 3)
	var samples []metrics.HitSample
	for j := range 30 {
		hs := metrics.HitSample{Key: "s|get", Off: j * 30, Latency: int64(1000 + i*10 + j)}
		if j == 7 {
			hs.Err = "timeout"
		}
		samples = append(samples, hs)
	}
	return &metrics.Snapshot{
		T:      i + 1,
		Users:  int64(i),
		Server: map[string]float64{"cpu": float64(i), "mem": 100},
		Hits:   &metrics.Hits{OK: ok, Err: er, Samples: samples},
		Active: []int64{int64(i), 1, 0, 0},
		Keys:   map[string]*metrics.KeySnap{"s|get": {Count: int64(i), Errors: 1}},
	}
}

func open(t testing.TB, dir string) *Store {
	t.Helper()
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func appendN(t testing.TB, s *Store, start time.Time, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if err := s.Append(start.Add(time.Duration(i)*time.Second), snap(i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	b := base()
	appendN(t, s, b, 0, 12) // some flushed, some pending
	check := func(pts []Point) {
		t.Helper()
		if len(pts) != 12 {
			t.Fatalf("got %d points", len(pts))
		}
		for i, p := range pts {
			ss := p.Snapshot
			if !p.At.Equal(b.Add(time.Duration(i)*time.Second)) || p.Seconds != 1 || p.N != 1 {
				t.Fatalf("point %d: %v %d %d", i, p.At, p.Seconds, p.N)
			}
			if ss.Server["cpu"] != float64(i) || ss.Server["mem"] != 100 || ss.T != i+1 || ss.Users != int64(i) {
				t.Fatalf("point %d: %+v", i, ss)
			}
			if ss.Hits.OK[3] != int32(i) || ss.Hits.OK[10] != 2 || len(ss.Hits.Samples) != secondSamples {
				t.Fatalf("point %d hits: %+v", i, ss.Hits)
			}
			if ss.Hits.Samples[0].Err != "timeout" { // errors kept first
				t.Fatalf("point %d: first sample %+v", i, ss.Hits.Samples[0])
			}
			if ss.Active[0] != int64(i) || ss.Keys["s|get"].Count != int64(i) {
				t.Fatalf("point %d: %v %+v", i, ss.Active, ss.Keys)
			}
		}
	}
	pts, err := s.Range(b, b.Add(11*time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	check(pts)
	if pts, _ = s.Latest(12); true {
		check(pts)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	defer s.Close()
	pts, _ = s.Range(b, b.Add(11*time.Second), 0)
	check(pts)
	names, _ := s.Names(b, b.Add(time.Hour))
	if fmt.Sprint(names) != "[cpu mem]" {
		t.Fatalf("names %v", names)
	}
}

func TestDownsampleSeconds(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	b := base()
	appendN(t, s, b, 0, 120)
	pts, err := s.Range(b, b.Add(119*time.Second), 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 12 {
		t.Fatalf("%d points", len(pts))
	}
	for k, p := range pts {
		var ok3, er20, keys int64
		var cpu, act float64
		for i := k * 10; i < k*10+10; i++ {
			ok3 += int64(i)
			er20 += int64(i % 3)
			keys += int64(i)
			cpu += float64(i)
			act += float64(i)
		}
		ss := p.Snapshot
		if p.Seconds != 10 || p.N != 10 || !p.At.Equal(b.Add(time.Duration(k*10)*time.Second)) {
			t.Fatalf("bucket %d: %v %d %d", k, p.At, p.Seconds, p.N)
		}
		if int64(ss.Hits.OK[3]) != ok3 || ss.Hits.OK[10] != 20 || int64(ss.Hits.Err[20]) != er20 {
			t.Fatalf("bucket %d hits %v %v", k, ss.Hits.OK[3], ss.Hits.Err[20])
		}
		if ss.Server["cpu"] != cpu/10 || ss.Server["mem"] != 100 {
			t.Fatalf("bucket %d server %v", k, ss.Server)
		}
		if ss.Active[0] != int64(math.Round(act/10)) || ss.Active[1] != 1 {
			t.Fatalf("bucket %d active %v", k, ss.Active)
		}
		if ss.Keys["s|get"].Count != keys || ss.Keys["s|get"].Errors != 10 {
			t.Fatalf("bucket %d keys %+v", k, ss.Keys["s|get"])
		}
		if n := len(ss.Hits.Samples); n != bucketSamples {
			t.Fatalf("bucket %d: %d samples", k, n)
		}
		// errors first, then slowest: the 10 error samples, then the slowest ok.
		if ss.Hits.Samples[9].Err == "" || ss.Hits.Samples[10].Err != "" ||
			ss.Hits.Samples[10].Latency < ss.Hits.Samples[11].Latency {
			t.Fatalf("bucket %d sample order", k)
		}
	}
}

// Minute-index buckets must equal merging the raw seconds.
func TestDownsampleMinutes(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	b := base().Add(-17 * time.Second) // unaligned start, spans hours
	const n = 3*3600 + 100
	appendN(t, s, b, 0, n)
	from, to := b, b.Add((n-1)*time.Second)
	pts, err := s.Range(from, to, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) == 0 || len(pts) > 30 {
		t.Fatalf("%d points", len(pts))
	}
	raw, _ := s.Range(from.Add(-time.Minute), to.Add(time.Minute), 0)
	total := 0
	for _, p := range pts {
		if p.Seconds%60 != 0 || p.At.Unix()%60 != 0 {
			t.Fatalf("bucket not minute aligned: %v %d", p.At, p.Seconds)
		}
		var want agg
		for _, r := range raw {
			if !r.At.Before(p.At) && r.At.Before(p.At.Add(time.Duration(p.Seconds)*time.Second)) {
				want.merge(fromSnapshot(r.At.Unix(), r.Snapshot), bucketSamples)
			}
		}
		ws := want.snapshot()
		if p.N != want.n || p.Snapshot.Hits.OK[3] != ws.Hits.OK[3] || p.Snapshot.Hits.Err[20] != ws.Hits.Err[20] {
			t.Fatalf("bucket %v: n %d/%d hits %d/%d", p.At, p.N, want.n, p.Snapshot.Hits.OK[3], ws.Hits.OK[3])
		}
		if math.Abs(p.Snapshot.Server["cpu"]-ws.Server["cpu"]) > 1e-9 || p.Snapshot.Active[0] != ws.Active[0] {
			t.Fatalf("bucket %v: cpu %v/%v", p.At, p.Snapshot.Server["cpu"], ws.Server["cpu"])
		}
		total += p.N
	}
	if total != n {
		t.Fatalf("total seconds %d, want %d", total, n)
	}
}

func TestTornTail(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	b := base()
	appendN(t, s, b, 0, 30) // 6 blocks of 5 seconds
	s.Close()
	seg := filepath.Join(dir, b.Format(segLayout)+".seg")
	st, _ := os.Stat(seg)
	if err := os.Truncate(seg, st.Size()-7); err != nil { // mid-record of the last block
		t.Fatal(err)
	}
	s = open(t, dir)
	pts, _ := s.Latest(100)
	if len(pts) != 25 || pts[24].Snapshot.T != 25 {
		t.Fatalf("after torn tail: %d points", len(pts))
	}
	// The minute index was rebuilt to match.
	r, _ := s.Range(b, b.Add(time.Hour), 10)
	if r[0].N != 25 {
		t.Fatalf("minute index has %d seconds", r[0].N)
	}
	// Appending carries on after the cut.
	appendN(t, s, b, 30, 40)
	s.Close()

	// Corrupt a byte in the middle: only that block is lost.
	data, _ := os.ReadFile(seg)
	data[headerSize+5] ^= 0xff
	os.WriteFile(seg, data, 0o644)
	s = open(t, dir)
	defer s.Close()
	pts, _ = s.Latest(100)
	if len(pts) != 30 || pts[0].Snapshot.T != 6 || pts[29].Snapshot.T != 40 {
		t.Fatalf("after corruption: %d points, first T %d", len(pts), pts[0].Snapshot.T)
	}
}

func TestRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	s, err := Open(dir, Options{NoSync: true, Retention: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-72 * time.Hour)
	mid := now.Add(-30 * time.Hour)
	appendN(t, s, old, 0, 3)
	appendN(t, s, mid, 0, 3)
	s.Close()
	segs := func() int {
		m, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
		return len(m)
	}
	if segs() != 2 {
		t.Fatalf("%d segments", segs())
	}
	s, _ = Open(dir, Options{NoSync: true, Retention: 48 * time.Hour})
	if segs() != 1 {
		t.Fatalf("on open: %d segments left", segs())
	}
	if _, err := os.Stat(filepath.Join(dir, old.Format(segLayout)+".min")); !os.IsNotExist(err) {
		t.Fatal("old index not deleted")
	}
	// Hourly check while appending: pretend a day has passed.
	s.now = func() time.Time { return now.Add(24 * time.Hour) }
	appendN(t, s, now, 0, 1)
	if segs() != 1 { // mid deleted, now's kept
		t.Fatalf("hourly: %d segments", segs())
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(dir, now.Format(segLayout)+".seg")); err != nil {
		t.Fatal(err)
	}
}

func TestReopenContinues(t *testing.T) {
	dir := t.TempDir()
	b := base().Add(10 * time.Second)
	s := open(t, dir)
	appendN(t, s, b, 0, 10)
	s.Close()
	s = open(t, dir)
	if err := s.Append(b.Add(5*time.Second), snap(5)); err != ErrOutOfOrder {
		t.Fatalf("got %v", err)
	}
	appendN(t, s, b, 10, 20)
	pts, _ := s.Latest(100)
	if len(pts) != 20 {
		t.Fatalf("%d points", len(pts))
	}
	r, _ := s.Range(b, b.Add(time.Hour), 10) // minute path, two records for one minute
	if r[0].N != 20 || r[0].Snapshot.Hits.OK[3] != 190 {
		t.Fatalf("minute: n %d ok %d", r[0].N, r[0].Snapshot.Hits.OK[3])
	}
	s.Close()
	if err := s.Append(b.Add(time.Minute), snap(1)); err != ErrClosed {
		t.Fatalf("after close: %v", err)
	}
	s = open(t, dir) // the index rebuilt from seconds agrees
	defer s.Close()
	r, _ = s.Range(b, b.Add(time.Hour), 10)
	if r[0].N != 20 || r[0].Snapshot.Hits.OK[3] != 190 {
		t.Fatalf("rebuilt minute: n %d ok %d", r[0].N, r[0].Snapshot.Hits.OK[3])
	}
}

// Readers must see every second exactly once whatever the writer is
// doing: buffered, mid-flush, rotating hours.
func TestConcurrent(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	b := base().Add(-30 * time.Minute)
	const n = 4000 // crosses an hour boundary
	var done atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range n {
			if err := s.Append(b.Add(time.Duration(i)*time.Second), snap(i)); err != nil {
				t.Error(err)
				return
			}
			done.Store(int64(i + 1))
		}
	}()
	errs := make(chan error, 8)
	for r := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done.Load() < n {
				before := done.Load()
				var got int
				if r%2 == 0 {
					pts, err := s.Range(b, b.Add(2*time.Hour), 0)
					if err != nil {
						errs <- err
						return
					}
					for i, p := range pts {
						if p.Snapshot.T != i+1 {
							errs <- fmt.Errorf("range: point %d has T %d", i, p.Snapshot.T)
							return
						}
					}
					got = len(pts)
				} else {
					pts, err := s.Range(b, b.Add(2*time.Hour), 20)
					if err != nil {
						errs <- err
						return
					}
					for _, p := range pts {
						got += p.N
					}
				}
				after := done.Load()
				// Both paths are exact: unflushed seconds and the current
				// minute come from memory under the same lock as file sizes.
				if int64(got) < before || int64(got) > after {
					errs <- fmt.Errorf("reader %d saw %d seconds, appended %d..%d", r, got, before, after)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
