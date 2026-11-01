package agent

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// Kind is a span's role, with OpenTelemetry's numbering.
type Kind int

// Span kinds.
const (
	KindInternal Kind = 1
	KindServer   Kind = 2
	KindClient   Kind = 3 // outgoing call or database query
	KindProducer Kind = 4
	KindConsumer Kind = 5
)

// Span is one timed operation. A nil *Span (no agent running) is valid and
// does nothing.
type Span struct {
	a        *Agent
	traceID  [16]byte
	spanID   [8]byte
	parentID [8]byte
	kind     Kind
	start    time.Time
	tx       *tx // server spans: the request in the active list

	mu      sync.Mutex
	name    string
	attrs   []attr
	end     time.Time
	ended   bool
	errored bool
	errMsg  string
	method  string // server spans, for SetRoute
	routed  bool   // SetRoute was called
	exc     *exception
}

// exception is the span's "exception" event (OpenTelemetry's way of
// recording an error); the first one recorded is kept.
type exception struct {
	at                  time.Time
	message, typ, stack string
	escaped             bool // a panic that left the handler
}

type attr struct {
	k string
	v any
}

type spanKey struct{}
type txKey struct{}

// remote is a parent span context taken from incoming headers.
type remote struct {
	traceID [16]byte
	spanID  [8]byte
}

// StartSpan starts a span as a child of the span in ctx (or a new trace)
// and returns a context carrying it. End it with span.End.
func StartSpan(ctx context.Context, name string, kind Kind) (context.Context, *Span) {
	s := newSpan(ctx, nil, name, kind)
	if s == nil {
		return ctx, nil
	}
	return context.WithValue(ctx, spanKey{}, s), s
}

// SpanFromContext returns the current span, or nil.
func SpanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}

func newSpan(ctx context.Context, from *remote, name string, kind Kind) *Span {
	a := current.Load()
	if a == nil {
		return nil
	}
	s := &Span{a: a, name: name, kind: kind, start: time.Now()}
	if p := SpanFromContext(ctx); p != nil {
		s.traceID, s.parentID = p.traceID, p.spanID
	} else if from != nil {
		s.traceID, s.parentID = from.traceID, from.spanID
	} else {
		randID(s.traceID[:])
	}
	randID(s.spanID[:])
	return s
}

func randID(b []byte) {
	for {
		for i := 0; i < len(b); i += 8 {
			v := rand.Uint64()
			for j := 0; j < 8 && i+j < len(b); j++ {
				b[i+j] = byte(v >> (8 * j))
			}
		}
		for _, c := range b {
			if c != 0 {
				return
			}
		}
	}
}

func hexID(b []byte) string { return hex.EncodeToString(b) }

// TraceID is the span's trace ID in hex.
func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return hexID(s.traceID[:])
}

// SpanID is the span's ID in hex.
func (s *Span) SpanID() string {
	if s == nil {
		return ""
	}
	return hexID(s.spanID[:])
}

// SetAttr adds an attribute (string, int, int64, bool or float64).
func (s *Span) SetAttr(key string, value any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.attrs {
		if s.attrs[i].k == key {
			s.attrs[i].v = value
			return
		}
	}
	s.attrs = append(s.attrs, attr{key, value})
}

// SetName renames the span.
func (s *Span) SetName(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
}

// Fail marks the span as failed without ending it, and records msg as
// an exception event (exception.message).
func (s *Span) Fail(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.errored, s.errMsg = true, msg
	s.recordLocked(&exception{at: time.Now(), message: msg})
	s.mu.Unlock()
}

// End finishes the span; a non-nil err marks it failed and is recorded as
// an exception event (exception.message, exception.type). Later calls do
// nothing.
func (s *Span) End(err error) {
	if s == nil {
		return
	}
	if err != nil {
		s.finish(true, err.Error(), &exception{message: err.Error(), typ: fmt.Sprintf("%T", err)})
		return
	}
	s.finish(false, "", nil)
}

func (s *Span) recordLocked(e *exception) {
	if s.exc == nil && e != nil {
		s.exc = e
	}
}

// finish ends the span with an error status (when failed) and, if no
// exception was recorded yet, exc.
func (s *Span) finish(failed bool, msg string, exc *exception) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.end = time.Now()
	if failed {
		s.errored, s.errMsg = true, msg
	}
	if exc != nil && exc.at.IsZero() {
		exc.at = s.end
	}
	s.recordLocked(exc)
	s.mu.Unlock()
	s.a.enqueue(s)
}

// traceparent is the W3C header value naming this span as the parent.
func (s *Span) traceparent() string {
	return "00-" + hexID(s.traceID[:]) + "-" + hexID(s.spanID[:]) + "-01"
}

// SetRoute names the request in ctx by its route ("/items/{id}") instead
// of its path, so requests for different IDs count as one endpoint. With
// Go 1.22+ ServeMux patterns this happens on its own.
func SetRoute(ctx context.Context, route string) {
	t, _ := ctx.Value(txKey{}).(*tx)
	if t == nil {
		return
	}
	t.span.setRoute(route)
}

func (s *Span) setRoute(route string) {
	s.mu.Lock()
	s.name = s.method + " " + route
	s.routed = true
	name := s.name
	s.mu.Unlock()
	s.SetAttr("http.route", route)
	if s.tx != nil {
		s.tx.name.Store(&name)
	}
}

// ---- propagation ----

// extract reads a parent span context from W3C traceparent, B3 (single
// or multi header) or Jaeger uber-trace-id headers.
func extract(get func(string) string) *remote {
	if v := get("traceparent"); v != "" {
		p := strings.Split(strings.TrimSpace(v), "-")
		if len(p) >= 4 && len(p[0]) == 2 && p[0] != "ff" {
			if r := parseIDs(p[1], p[2]); r != nil {
				return r
			}
		}
	}
	if v := get("b3"); v != "" {
		p := strings.Split(strings.TrimSpace(v), "-")
		if len(p) >= 2 {
			if r := parseIDs(p[0], p[1]); r != nil {
				return r
			}
		}
	}
	if t, sp := get("X-B3-TraceId"), get("X-B3-SpanId"); t != "" && sp != "" {
		if r := parseIDs(t, sp); r != nil {
			return r
		}
	}
	if v := get("uber-trace-id"); v != "" {
		v = strings.ReplaceAll(strings.ReplaceAll(v, "%3A", ":"), "%3a", ":")
		p := strings.Split(strings.TrimSpace(v), ":")
		if len(p) == 4 {
			if r := parseIDs(p[0], p[1]); r != nil {
				return r
			}
		}
	}
	return nil
}

// parseIDs reads hex trace and span IDs, left-padding short ones (64-bit
// B3 and Jaeger trace IDs).
func parseIDs(trace, span string) *remote {
	if len(trace) == 0 || len(trace) > 32 || len(span) == 0 || len(span) > 16 {
		return nil
	}
	var r remote
	tb, err1 := hex.DecodeString(strings.Repeat("0", 32-len(trace)) + trace)
	sb, err2 := hex.DecodeString(strings.Repeat("0", 16-len(span)) + span)
	if err1 != nil || err2 != nil {
		return nil
	}
	copy(r.traceID[:], tb)
	copy(r.spanID[:], sb)
	if r.traceID == ([16]byte{}) || r.spanID == ([8]byte{}) {
		return nil
	}
	return &r
}
