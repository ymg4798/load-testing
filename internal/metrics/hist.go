// Package metrics records latencies into HDR histograms, cuts them into
// one-second snapshots and turns them into summaries and threshold verdicts.
package metrics

import (
	"encoding/base64"
	"fmt"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// Latencies are stored in microseconds, from 1µs up to 1 hour, at 3 significant digits.
const (
	histMin    = 1
	histMax    = 3_600_000_000
	histDigits = 3
)

// NewHist returns an empty latency histogram (values in microseconds).
func NewHist() *hdr.Histogram { return hdr.New(histMin, histMax, histDigits) }

// EncodeHist returns the compressed, base64 form of h used in result files.
func EncodeHist(h *hdr.Histogram) (string, error) {
	b, err := h.Encode(hdr.V2CompressedEncodingCookieBase)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// DecodeHist reverses EncodeHist. An empty string gives an empty histogram.
func DecodeHist(s string) (*hdr.Histogram, error) {
	if s == "" {
		return NewHist(), nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("histogram: %w", err)
	}
	return hdr.Decode(b)
}

// Stats is the set of numbers shown for one latency distribution, in microseconds.
type Stats struct {
	Min  int64   `json:"min"`
	Mean float64 `json:"mean"`
	P50  int64   `json:"p50"`
	P90  int64   `json:"p90"`
	P95  int64   `json:"p95"`
	P99  int64   `json:"p99"`
	P999 int64   `json:"p999"`
	Max  int64   `json:"max"`
}

// StatsOf reads the usual percentiles from h.
func StatsOf(h *hdr.Histogram) Stats {
	if h.TotalCount() == 0 {
		return Stats{}
	}
	return Stats{
		Min:  h.Min(),
		Mean: h.Mean(),
		P50:  h.ValueAtQuantile(50),
		P90:  h.ValueAtQuantile(90),
		P95:  h.ValueAtQuantile(95),
		P99:  h.ValueAtQuantile(99),
		P999: h.ValueAtQuantile(99.9),
		Max:  h.Max(),
	}
}

// Quantile reads a named statistic ("p95", "avg", "max", ...) from s.
func (s Stats) Quantile(name string) (float64, bool) {
	switch name {
	case "min":
		return float64(s.Min), true
	case "avg", "mean":
		return s.Mean, true
	case "med", "p50":
		return float64(s.P50), true
	case "p90":
		return float64(s.P90), true
	case "p95":
		return float64(s.P95), true
	case "p99":
		return float64(s.P99), true
	case "p99.9", "p999":
		return float64(s.P999), true
	case "max":
		return float64(s.Max), true
	}
	return 0, false
}
