package metrics

import (
	"sort"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// KeySummary is one key's numbers over the whole test.
type KeySummary struct {
	Key       string           `json:"key"`
	Scenario  string           `json:"scenario"`
	Step      string           `json:"step"`
	Count     int64            `json:"count"`
	Errors    int64            `json:"errors"`
	ErrorRate float64          `json:"errorRate"`
	RPS       float64          `json:"rps"`
	Bytes     int64            `json:"bytes"`
	ErrKinds  map[string]int64 `json:"errKinds,omitempty"`
	Stats
	// With tracing on: the slowest requests and the first failed ones.
	Slowest []Exemplar `json:"slowest,omitempty"`
	Failed  []Exemplar `json:"failed,omitempty"`
}

// GenSummary is the worst the load generator looked during the test.
type GenSummary struct {
	MaxCPU       float64 `json:"maxCpu"`
	MaxLagP99    int64   `json:"maxLagP99"`
	PeakInFlight int64   `json:"peakInFlight"`
	PeakUsers    int64   `json:"peakUsers"`
	Agents       int     `json:"agents,omitempty"`
}

// Summary is the whole test in numbers.
type Summary struct {
	Duration   float64       `json:"duration"` // seconds
	Requests   int64         `json:"requests"`
	Errors     int64         `json:"errors"`
	ErrorRate  float64       `json:"errorRate"`
	RPS        float64       `json:"rps"`
	Iterations int64         `json:"iterations"`
	Dropped    int64         `json:"dropped"`
	Latency    Stats         `json:"latency"` // all request steps together
	Keys       []*KeySummary `json:"keys"`
	Gen        GenSummary    `json:"gen"`
}

type cum struct {
	count, errors, bytes int64
	errKinds             map[string]int64
	hist                 *hdr.Histogram
	slowest, failed      []Exemplar
}

// Accumulator folds snapshots into whole-test totals.
type Accumulator struct {
	Snapshots []*Snapshot
	keys      map[string]*cum
	dropped   int64
	gen       GenSummary
	wall      float64
}

func NewAccumulator() *Accumulator { return &Accumulator{keys: map[string]*cum{}} }

// Add folds one snapshot in and keeps it for the result file.
func (a *Accumulator) Add(s *Snapshot) error {
	a.Snapshots = append(a.Snapshots, s)
	a.dropped += s.Dropped
	if s.Gen.CPU > a.gen.MaxCPU {
		a.gen.MaxCPU = s.Gen.CPU
	}
	if s.Gen.LagP99 > a.gen.MaxLagP99 {
		a.gen.MaxLagP99 = s.Gen.LagP99
	}
	if s.InFlight > a.gen.PeakInFlight {
		a.gen.PeakInFlight = s.InFlight
	}
	if s.Users > a.gen.PeakUsers {
		a.gen.PeakUsers = s.Users
	}
	if s.Gen.Agents > a.gen.Agents {
		a.gen.Agents = s.Gen.Agents
	}
	for k, ks := range s.Keys {
		c := a.keys[k]
		if c == nil {
			c = &cum{hist: NewHist()}
			a.keys[k] = c
		}
		c.count += ks.Count
		c.errors += ks.Errors
		c.bytes += ks.Bytes
		for e, n := range ks.ErrKinds {
			if c.errKinds == nil {
				c.errKinds = map[string]int64{}
			}
			c.errKinds[e] += n
		}
		h, err := DecodeHist(ks.Hist)
		if err != nil {
			return err
		}
		c.hist.Merge(h)
		if ks.Slow != nil {
			c.slowest = keepSlowest(c.slowest, *ks.Slow)
		}
		for _, f := range ks.Failed {
			if len(c.failed) < maxExemplars {
				c.failed = append(c.failed, f)
			}
		}
	}
	return nil
}

// keepSlowest inserts e into a list sorted slowest first, capped at maxExemplars.
func keepSlowest(list []Exemplar, e Exemplar) []Exemplar {
	i := sort.Search(len(list), func(i int) bool { return list[i].Latency < e.Latency })
	if i >= maxExemplars {
		return list
	}
	list = append(list, Exemplar{})
	copy(list[i+1:], list[i:])
	list[i] = e
	if len(list) > maxExemplars {
		list = list[:maxExemplars]
	}
	return list
}

// HistFor merges the whole-test histograms of every key that match.
func (a *Accumulator) HistFor(match func(key string) bool) (h *hdr.Histogram, count, errors int64) {
	h = NewHist()
	for k, c := range a.keys {
		if match(k) {
			h.Merge(c.hist)
			count += c.count
			errors += c.errors
		}
	}
	return h, count, errors
}

// SetDuration sets the measured test length in seconds. Without it the
// number of snapshots is used.
func (a *Accumulator) SetDuration(sec float64) { a.wall = sec }

// Duration is the test length in seconds.
func (a *Accumulator) Duration() float64 {
	if a.wall > 0 {
		return a.wall
	}
	return float64(len(a.Snapshots))
}

// Summary computes whole-test numbers.
func (a *Accumulator) Summary() *Summary {
	d := a.Duration()
	s := &Summary{Duration: d, Dropped: a.dropped, Gen: a.gen}
	all := NewHist()
	for k, c := range a.keys {
		sc, st := SplitKey(k)
		ks := &KeySummary{
			Key: k, Scenario: sc, Step: st, Count: c.count, Errors: c.errors,
			Bytes: c.bytes, ErrKinds: c.errKinds, Stats: StatsOf(c.hist),
			Slowest: c.slowest, Failed: c.failed,
		}
		if c.count > 0 {
			ks.ErrorRate = float64(c.errors) / float64(c.count)
		}
		if d > 0 {
			ks.RPS = float64(c.count) / d
		}
		s.Keys = append(s.Keys, ks)
		if st == IterationStep {
			s.Iterations += c.count
			continue
		}
		if IsInternal(k) {
			continue
		}
		s.Requests += c.count
		s.Errors += c.errors
		all.Merge(c.hist)
	}
	sort.Slice(s.Keys, func(i, j int) bool { return s.Keys[i].Key < s.Keys[j].Key })
	s.Latency = StatsOf(all)
	if s.Requests > 0 {
		s.ErrorRate = float64(s.Errors) / float64(s.Requests)
	}
	if d > 0 {
		s.RPS = float64(s.Requests) / d
	}
	return s
}
