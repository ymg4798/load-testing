package protocol

import (
	"encoding/hex"
	"fmt"
)

// Tracing makes every request carry a fresh trace context, so the target's
// tracing (OpenTelemetry, Jaeger, Zipkin) records it and slow or failed
// requests in the report can be looked up by trace ID.
type Tracing struct {
	Enabled    bool
	Propagator string // w3c (default), b3, jaeger
	Sampled    bool   // ask the target to keep the trace
}

// Trace is the context sent with one request.
type Trace struct {
	ID      string // 32 hex chars
	Headers [][2]string
}

// NewTrace returns a trace context for one request, or nil when tracing is off.
func (t Tracing) NewTrace(vu *VU) *Trace {
	if !t.Enabled {
		return nil
	}
	var b [24]byte
	for i := 0; i < 24; i += 8 {
		v := vu.Rand.Uint64()
		for j := 0; j < 8; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	if b[0] == 0 {
		b[0] = 1 // an all-zero ID is invalid; keep it non-zero
	}
	trace, span := hex.EncodeToString(b[:16]), hex.EncodeToString(b[16:])
	flag := "0"
	if t.Sampled {
		flag = "1"
	}
	tr := &Trace{ID: trace}
	switch t.Propagator {
	case "b3":
		tr.Headers = [][2]string{{"b3", trace + "-" + span + "-" + flag}}
	case "jaeger":
		tr.Headers = [][2]string{{"uber-trace-id", trace + ":" + span + ":0:" + flag}}
	default:
		tr.Headers = [][2]string{{"traceparent", "00-" + trace + "-" + span + "-0" + flag}}
	}
	return tr
}

// ValidPropagator checks a propagator name from the scenario file.
func ValidPropagator(p string) error {
	switch p {
	case "", "w3c", "b3", "jaeger":
		return nil
	}
	return fmt.Errorf("tracing.propagator must be w3c, b3 or jaeger, got %q", p)
}
