package metrics

import (
	"strings"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// Key names one measured thing: "scenario|step". Steps whose name starts
// with "_" (such as "_iteration") are bookkeeping and are left out of the
// "all steps" aggregates.
func Key(scenario, step string) string { return scenario + "|" + step }

// SplitKey reverses Key.
func SplitKey(k string) (scenario, step string) {
	if i := strings.IndexByte(k, '|'); i >= 0 {
		return k[:i], k[i+1:]
	}
	return "", k
}

// IsInternal reports whether the key is bookkeeping rather than a request.
func IsInternal(k string) bool {
	_, step := SplitKey(k)
	return strings.HasPrefix(step, "_")
}

// IterationStep is the step name under which whole iterations are recorded.
const IterationStep = "_iteration"

// KeySnap is one key's numbers for one second (or for any merged span).
type KeySnap struct {
	Count    int64            `json:"count"`
	Errors   int64            `json:"errors"`
	Bytes    int64            `json:"bytes,omitempty"`
	ErrKinds map[string]int64 `json:"errKinds,omitempty"`
	Stats
	Hist string `json:"hist,omitempty"`
	// With tracing on: this second's slowest request and a few failed ones.
	Slow   *Exemplar  `json:"slow,omitempty"`
	Failed []Exemplar `json:"failed,omitempty"`
}

// Exemplar is one traced request worth looking at in a tracing backend.
type Exemplar struct {
	T       int    `json:"t"`       // second of the test
	Latency int64  `json:"latency"` // µs
	TraceID string `json:"traceId"`
	Err     string `json:"err,omitempty"`
}

// maxExemplars caps failed exemplars kept per key per second, and the
// slowest/failed lists kept per key for the whole test.
const maxExemplars = 10

// GenStats describes the load generator itself during one second.
type GenStats struct {
	CPU        float64 `json:"cpu"`        // percent of all cores
	LagP99     int64   `json:"lagP99"`     // scheduling lag p99, µs
	LagMax     int64   `json:"lagMax"`     // scheduling lag max, µs
	Goroutines int     `json:"goroutines"` // live goroutines
	Agents     int     `json:"agents,omitempty"`
}

// Snapshot is everything measured during one second of the test.
type Snapshot struct {
	T          int                 `json:"t"` // seconds since start; this snapshot covers (T-1, T]
	Keys       map[string]*KeySnap `json:"keys"`
	Dropped    int64               `json:"dropped"`
	InFlight   int64               `json:"inFlight"`
	Users      int64               `json:"users"`
	TargetRate float64             `json:"targetRate"`
	Gen        GenStats            `json:"gen"`
	LagHist    string              `json:"lagHist,omitempty"`
	// Server holds the target's own metrics for this second (see monitor),
	// keyed by display name.
	Server map[string]float64 `json:"server,omitempty"`
	// Hits is the client-side hitmap of this second; APMHits the same for
	// server transactions reported by the APM collector.
	Hits    *Hits `json:"hits,omitempty"`
	APMHits *Hits `json:"apmHits,omitempty"`
	// Active counts in-flight iterations by how long they have run (see ActiveBuckets).
	Active []int64 `json:"active,omitempty"`
}

// Merge combines snapshots for the same second from several agents.
func Merge(snaps []*Snapshot) (*Snapshot, error) {
	out := &Snapshot{Keys: map[string]*KeySnap{}}
	hists := map[string]*hdr.Histogram{}
	lag := NewHist()
	for _, s := range snaps {
		if s == nil {
			continue
		}
		out.T = s.T
		out.Dropped += s.Dropped
		out.InFlight += s.InFlight
		out.Users += s.Users
		out.TargetRate += s.TargetRate
		out.Hits = MergeHits(out.Hits, s.Hits)
		out.APMHits = MergeHits(out.APMHits, s.APMHits)
		if len(s.Active) > len(out.Active) {
			out.Active = append(out.Active, make([]int64, len(s.Active)-len(out.Active))...)
		}
		for i, v := range s.Active {
			out.Active[i] += v
		}
		out.Gen.Agents++
		if s.Gen.CPU > out.Gen.CPU {
			out.Gen.CPU = s.Gen.CPU
		}
		out.Gen.Goroutines += s.Gen.Goroutines
		if s.LagHist != "" {
			h, err := DecodeHist(s.LagHist)
			if err != nil {
				return nil, err
			}
			lag.Merge(h)
		}
		for k, ks := range s.Keys {
			dst := out.Keys[k]
			if dst == nil {
				dst = &KeySnap{}
				out.Keys[k] = dst
				hists[k] = NewHist()
			}
			if ks.Slow != nil && (dst.Slow == nil || ks.Slow.Latency > dst.Slow.Latency) {
				dst.Slow = ks.Slow
			}
			for _, f := range ks.Failed {
				if len(dst.Failed) < maxExemplars {
					dst.Failed = append(dst.Failed, f)
				}
			}
			dst.Count += ks.Count
			dst.Errors += ks.Errors
			dst.Bytes += ks.Bytes
			for e, n := range ks.ErrKinds {
				if dst.ErrKinds == nil {
					dst.ErrKinds = map[string]int64{}
				}
				dst.ErrKinds[e] += n
			}
			h, err := DecodeHist(ks.Hist)
			if err != nil {
				return nil, err
			}
			hists[k].Merge(h)
		}
	}
	for k, h := range hists {
		out.Keys[k].Stats = StatsOf(h)
		enc, err := EncodeHist(h)
		if err != nil {
			return nil, err
		}
		out.Keys[k].Hist = enc
	}
	ls := StatsOf(lag)
	out.Gen.LagP99, out.Gen.LagMax = ls.P99, ls.Max
	if enc, err := EncodeHist(lag); err == nil && lag.TotalCount() > 0 {
		out.LagHist = enc
	}
	return out, nil
}

// Overall merges every request key (not bookkeeping keys) of the snapshot.
func (s *Snapshot) Overall() (count, errors int64, st Stats) {
	h := NewHist()
	for k, ks := range s.Keys {
		if IsInternal(k) {
			continue
		}
		count += ks.Count
		errors += ks.Errors
		if x, err := DecodeHist(ks.Hist); err == nil {
			h.Merge(x)
		}
	}
	return count, errors, StatsOf(h)
}
