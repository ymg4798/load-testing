package store

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
)

// Point is one returned bucket of history.
type Point struct {
	At      time.Time // start of the bucket (first second it covers)
	Seconds int       // width of the bucket in seconds
	N       int       // seconds of data actually merged into it (≤ Seconds; gaps when lt was not running)
	// Snapshot is the merge of those seconds: Server series and the
	// Users/InFlight/TargetRate/Gen gauges are averaged over the seconds
	// that reported them; Active buckets are averaged and rounded; Hits,
	// APMHits, Keys counts and Dropped are summed (divide by N for a
	// rate). Hit samples are capped at 60 per bucket, errors first, then
	// slowest. T is the largest T merged. Per-key histograms and Stats
	// are not stored.
	Snapshot *metrics.Snapshot
}

// view is what a reader needs from the writer, taken under the lock: the
// open hour, how much of its files is complete, and what is still only
// in memory.
type view struct {
	hour             int64
	segSize, minSize int64
	pending          []*agg
	cur              *agg
}

func (s *Store) view() view {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := view{hour: s.hour, segSize: s.segSize, minSize: s.minSize,
		pending: append([]*agg(nil), s.pending...)}
	if s.cur != nil {
		v.cur = &agg{}
		v.cur.merge(s.cur, minuteSamples)
	}
	return v
}

// collect reads every record of kind whose time is in [lo, hi] and
// merges it into the bucket key(record) returns. Minute records are
// filtered on the minute's start. Segment hours are decoded in parallel,
// each into its own buckets, then combined in hour order so results
// (float sums included) do not depend on scheduling.
func (s *Store) collect(v view, kind byte, lo, hi int64, key func(*agg) int64, sampleCap int) (map[int64]*agg, error) {
	all, err := s.hours()
	if err != nil {
		return nil, err
	}
	var hours []int64
	for _, h := range all {
		if h+3599 >= lo && h <= hi && (v.hour < 0 || h <= v.hour) {
			hours = append(hours, h)
		}
	}
	parts := make([]map[int64]*agg, len(hours))
	errs := make([]error, len(hours))
	next := make(chan int, len(hours))
	for i := range hours {
		next <- i
	}
	close(next)
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(hours)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := hourReader{s: s, dec: decoder{intern: map[string]string{}}}
			for i := range next {
				parts[i], errs[i] = r.read(v, hours[i], kind, lo, hi, key, sampleCap)
			}
		}()
	}
	wg.Wait()
	out := map[int64]*agg{}
	add := func(k int64, a *agg) {
		b := out[k]
		if b == nil {
			b = &agg{}
			out[k] = b
		}
		b.merge(a, sampleCap)
	}
	for i, p := range parts {
		if errs[i] != nil {
			return nil, errs[i]
		}
		keys := make([]int64, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if out[k] == nil {
				out[k] = p[k] // owned by us now; no copy needed
			} else {
				out[k].merge(p[k], sampleCap)
			}
		}
	}
	// What is still only in memory.
	if kind == kindSeconds {
		for _, a := range v.pending {
			if a.at >= lo && a.at <= hi {
				add(key(a), a)
			}
		}
	} else if a := v.cur; a != nil {
		if t := a.at - mod(a.at, 60); t >= lo && t <= hi {
			add(key(a), a)
		}
	}
	return out, nil
}

type hourReader struct {
	s   *Store
	dec decoder
	buf []byte
}

func (r *hourReader) read(v view, h int64, kind byte, lo, hi int64, key func(*agg) int64, sampleCap int) (map[int64]*agg, error) {
	ext, slack := ".seg", int64(0)
	if kind == kindMinutes {
		ext, slack = ".min", 59
	}
	data, err := os.ReadFile(r.s.path(h, ext))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // pruned meanwhile, or no index yet
	}
	if err != nil {
		return nil, err
	}
	if h == v.hour { // only what was complete when the view was taken
		limit := v.segSize
		if kind == kindMinutes {
			limit = v.minSize
		}
		if int64(len(data)) > limit {
			data = data[:limit]
		}
	}
	out := map[int64]*agg{}
	scanBlocks(data, func(bh blockHeader, payload []byte) bool {
		if bh.kind != kind || bh.last+slack < lo || bh.first > hi {
			return true
		}
		r.buf, _ = inflate(payload, r.buf, func(rec []byte) {
			a, err := r.dec.decode(rec)
			if err != nil {
				return
			}
			t := a.at
			if kind == kindMinutes {
				t -= mod(t, 60)
			}
			if t < lo || t > hi {
				return
			}
			k := key(a)
			b := out[k]
			if b == nil {
				b = &agg{}
				out[k] = b
			}
			b.merge(a, sampleCap)
		})
		return true
	})
	return out, nil
}

// Range returns history in [from, to] (whole seconds, both inclusive),
// downsampled to at most maxPoints buckets (maxPoints ≤ 0: one per
// second). Buckets are consecutive and of equal width; only buckets with
// data are returned, oldest first. When the width comes out at 60s or
// more it is rounded up to whole minutes and buckets are aligned to the
// minute, so the first and last bucket may include up to 59s outside
// [from, to]; those reads use the per-minute index.
func (s *Store) Range(from, to time.Time, maxPoints int) ([]Point, error) {
	lo, hi := from.Unix(), to.Unix()
	if hi < lo {
		return nil, nil
	}
	span := hi - lo + 1
	w := int64(1)
	if maxPoints > 0 {
		w = (span + int64(maxPoints) - 1) / int64(maxPoints)
	}
	kind, start := byte(kindSeconds), lo
	if w >= 60 {
		kind, start = kindMinutes, lo-mod(lo, 60)
		w = (w + 59) / 60 * 60
		for (hi-start)/w+1 > int64(maxPoints) {
			w += 60
		}
	}
	buckets, err := s.collect(s.view(), kind, start, hi, func(a *agg) int64 { return (a.at - start) / w }, bucketSamples)
	if err != nil {
		return nil, err
	}
	idx := make([]int64, 0, len(buckets))
	for i := range buckets {
		idx = append(idx, i)
	}
	slices.Sort(idx)
	out := make([]Point, 0, len(idx))
	for _, i := range idx {
		b := buckets[i]
		b.finish()
		out = append(out, Point{
			At:       time.Unix(start+i*w, 0),
			Seconds:  int(w),
			N:        b.n,
			Snapshot: b.snapshot(),
		})
	}
	return out, nil
}

// Latest returns up to the last n stored seconds, oldest first, one point
// per second (no downsampling).
func (s *Store) Latest(n int) ([]Point, error) {
	if n <= 0 {
		return nil, nil
	}
	v := s.view()
	hours, err := s.hours()
	if err != nil {
		return nil, err
	}
	bySec := func(a *agg) int64 { return a.at }
	got := map[int64]*agg{}
	for _, a := range v.pending {
		got[a.at] = a
	}
	// Walk segments newest first until n seconds are in hand.
	disk := view{hour: v.hour, segSize: v.segSize, minSize: v.minSize}
	for i := len(hours) - 1; i >= 0 && len(got) < n; i-- {
		h := hours[i]
		if v.hour >= 0 && h > v.hour {
			continue
		}
		m, err := s.collect(disk, kindSeconds, h, h+3599, bySec, secondSamples)
		if err != nil {
			return nil, err
		}
		maps.Copy(got, m)
	}
	secs := slices.Sorted(maps.Keys(got))
	if len(secs) > n {
		secs = secs[len(secs)-n:]
	}
	out := make([]Point, len(secs))
	for i, sec := range secs {
		b := &agg{}
		b.merge(got[sec], secondSamples)
		b.finish()
		out[i] = Point{At: time.Unix(sec, 0), Seconds: 1, N: 1, Snapshot: b.snapshot()}
	}
	return out, nil
}

// Names lists the server series names seen in [from, to], sorted. It reads
// the per-minute index, so it is cheap over any span.
func (s *Store) Names(from, to time.Time) ([]string, error) {
	lo, hi := from.Unix(), to.Unix()
	lo -= mod(lo, 60)
	m, err := s.collect(s.view(), kindMinutes, lo, hi, func(*agg) int64 { return 0 }, 0)
	if err != nil || m[0] == nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(m[0].server)), nil
}
