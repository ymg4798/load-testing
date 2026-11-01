package metrics

import (
	"math"
	"math/rand/v2"
)

// Hitmap: every request of a second counted into log-scale latency
// buckets (ok and failed apart), plus a small random sample of the
// requests themselves so a dragged region of the chart can list them.

// HitBuckets is the number of latency buckets: 10 per decade from 100µs
// to 100s, with the first bucket also taking anything faster and the last
// anything slower.
const HitBuckets = 60

// MaxHitSamples caps the requests kept per second.
const MaxHitSamples = 60

// HitBucket returns the bucket of a latency in µs.
func HitBucket(us int64) int {
	if us <= 100 {
		return 0
	}
	b := int(math.Log10(float64(us)/100) * 10)
	if b >= HitBuckets {
		return HitBuckets - 1
	}
	return b
}

// HitBucketFloor is the lower edge of bucket b in µs.
func HitBucketFloor(b int) float64 { return 100 * math.Pow(10, float64(b)/10) }

// HitSample is one request kept for drill-down.
type HitSample struct {
	Key     string `json:"k"`            // scenario|step, or service|operation for server spans
	Off     int    `json:"o"`            // ms into the second
	Latency int64  `json:"l"`            // µs
	Err     string `json:"e,omitempty"`  // error kind
	TraceID string `json:"tr,omitempty"` // when tracing is on
}

// Hits is one second of the hitmap.
type Hits struct {
	OK      []int32     `json:"ok,omitempty"`
	Err     []int32     `json:"err,omitempty"`
	Samples []HitSample `json:"s,omitempty"`
	seen    int         // samples offered, for reservoir sampling
}

// Add counts one request and may keep it as a sample.
func (h *Hits) Add(s HitSample, rnd *rand.Rand) {
	b := HitBucket(s.Latency)
	if s.Err == "" {
		if h.OK == nil {
			h.OK = make([]int32, HitBuckets)
		}
		h.OK[b]++
	} else {
		if h.Err == nil {
			h.Err = make([]int32, HitBuckets)
		}
		h.Err[b]++
	}
	h.seen++
	if len(h.Samples) < MaxHitSamples {
		h.Samples = append(h.Samples, s)
	} else if j := rnd.IntN(h.seen); j < MaxHitSamples {
		h.Samples[j] = s
	}
}

// Trim drops trailing empty buckets to keep result files small.
func (h *Hits) Trim() *Hits {
	if h == nil {
		return nil
	}
	trim := func(v []int32) []int32 {
		n := len(v)
		for n > 0 && v[n-1] == 0 {
			n--
		}
		if n == 0 {
			return nil
		}
		return v[:n]
	}
	h.OK, h.Err = trim(h.OK), trim(h.Err)
	if h.OK == nil && h.Err == nil {
		return nil
	}
	return h
}

// MergeHits adds b into a (either may be nil) and returns the result.
func MergeHits(a, b *Hits) *Hits {
	if b == nil {
		return a
	}
	if a == nil {
		a = &Hits{}
	}
	add := func(dst, src []int32) []int32 {
		if len(src) > len(dst) {
			dst = append(dst, make([]int32, len(src)-len(dst))...)
		}
		for i, v := range src {
			dst[i] += v
		}
		return dst
	}
	a.OK, a.Err = add(a.OK, b.OK), add(a.Err, b.Err)
	for _, s := range b.Samples {
		if len(a.Samples) >= MaxHitSamples {
			break
		}
		a.Samples = append(a.Samples, s)
	}
	return a
}

// ActiveTx is one transaction in flight right now: a load-test iteration
// or, from the APM agent, a request the server is still handling.
type ActiveTx struct {
	Source    string `json:"source"`           // "load" or the service name
	Name      string `json:"name"`             // scenario, or the server's endpoint
	Detail    string `json:"detail,omitempty"` // current step, SQL, outgoing call
	ElapsedMs int64  `json:"elapsedMs"`
	Who       string `json:"who,omitempty"` // VU number, host
	TraceID   string `json:"traceId,omitempty"`
	// Stack is where the server's code is right now, for transactions that
	// have run long (lt agent "active stack"); empty otherwise.
	Stack string `json:"stack,omitempty"`
}

// ActiveBuckets are the edges (ms) of the active-status counts kept per
// second: under 1s, 1–3s, 3–8s, 8s and more.
var ActiveBuckets = []int64{1000, 3000, 8000}

// ActiveBucket returns the active-status bucket of an elapsed time.
func ActiveBucket(ms int64) int {
	for i, edge := range ActiveBuckets {
		if ms < edge {
			return i
		}
	}
	return len(ActiveBuckets)
}
