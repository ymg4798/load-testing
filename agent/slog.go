package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"time"
)

// SlogHandler wraps next so that every record carries the trace_id and
// span_id of the span in its context, and is also sent to the collector
// as an OTLP/HTTP JSON log record (Endpoint + /v1/logs) with the
// service name, severity and attributes. Sending is batched with the
// spans' flush interval through a bounded queue (Config.LogQueueSize):
// logging never blocks, and records are dropped when the queue is full or
// no agent is running. next decides which levels are enabled; a nil next
// only exports (Info and above).
//
//	slog.SetDefault(slog.New(agent.SlogHandler(slog.NewJSONHandler(os.Stdout, nil))))
//	slog.ErrorContext(r.Context(), "payment failed", "order", id)
//
// After WithGroup, trace_id and span_id are added inside the group in
// next's output (slog cannot add top-level attributes there); the
// exported record always has them as its trace and span IDs.
func SlogHandler(next slog.Handler) slog.Handler {
	return &slogHandler{next: next}
}

type slogHandler struct {
	next   slog.Handler
	attrs  []jsonKV // from WithAttrs, already prefixed with their groups
	prefix string   // open groups, "a.b."
}

func (h *slogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	if h.next == nil {
		return l >= slog.LevelInfo
	}
	return h.next.Enabled(ctx, l)
}

func (h *slogHandler) Handle(ctx context.Context, r slog.Record) error {
	sp := SpanFromContext(ctx)
	if a := current.Load(); a != nil {
		a.enqueueLog(h.record(a, r, sp))
	}
	if h.next == nil {
		return nil
	}
	if sp != nil {
		r = r.Clone()
		r.AddAttrs(slog.String("trace_id", sp.TraceID()), slog.String("span_id", sp.SpanID()))
	}
	return h.next.Handle(ctx, r)
}

func (h *slogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	n := &slogHandler{prefix: h.prefix}
	n.attrs = append(append([]jsonKV(nil), h.attrs...), flattenAttrs(nil, h.prefix, as)...)
	if h.next != nil {
		n.next = h.next.WithAttrs(as)
	}
	return n
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := &slogHandler{prefix: h.prefix + name + ".", attrs: h.attrs}
	if h.next != nil {
		n.next = h.next.WithGroup(name)
	}
	return n
}

// ---- export ----

type logRecord struct {
	Time     string    `json:"timeUnixNano"`
	Observed string    `json:"observedTimeUnixNano"`
	Severity int       `json:"severityNumber"`
	SevText  string    `json:"severityText"`
	Body     jsonValue `json:"body"`
	Attrs    []jsonKV  `json:"attributes,omitempty"`
	TraceID  string    `json:"traceId,omitempty"`
	SpanID   string    `json:"spanId,omitempty"`
}

func (h *slogHandler) record(a *Agent, r slog.Record, sp *Span) logRecord {
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	msg := r.Message
	lr := logRecord{
		Time:     strconv.FormatInt(t.UnixNano(), 10),
		Observed: strconv.FormatInt(time.Now().UnixNano(), 10),
		Severity: severity(r.Level),
		SevText:  r.Level.String(),
		Body:     jsonValue{String: &msg},
	}
	if n := len(h.attrs) + r.NumAttrs(); n > 0 {
		lr.Attrs = make([]jsonKV, 0, n)
		lr.Attrs = append(lr.Attrs, h.attrs...)
		r.Attrs(func(at slog.Attr) bool {
			lr.Attrs = flattenAttrs(lr.Attrs, h.prefix, []slog.Attr{at})
			return true
		})
	}
	if sp != nil {
		lr.TraceID, lr.SpanID = sp.TraceID(), sp.SpanID()
	}
	return lr
}

// severity maps a slog level to an OTel severity number: DEBUG 5,
// INFO 9, WARN 13, ERROR 17 (levels in between land in between).
func severity(l slog.Level) int {
	return min(max(int(l)+9, 1), 24)
}

func flattenAttrs(out []jsonKV, prefix string, as []slog.Attr) []jsonKV {
	for _, at := range as {
		v := at.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			p := prefix
			if at.Key != "" {
				p += at.Key + "."
			}
			out = flattenAttrs(out, p, v.Group())
			continue
		}
		if at.Key == "" {
			continue
		}
		out = append(out, kv(prefix+at.Key, slogValue(v)))
	}
	return out
}

func slogValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		if u := v.Uint64(); u <= 1<<63-1 {
			return int64(u)
		}
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	}
	switch x := v.Any().(type) {
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	}
	return fmt.Sprint(v.Any())
}

func (a *Agent) enqueueLog(r logRecord) {
	select {
	case a.logs <- r:
	default:
		a.logsDropped.Add(1)
	}
}

// DroppedLogs is the number of log records lost because the queue was
// full or the collector could not be reached.
func (a *Agent) DroppedLogs() int64 { return a.logsDropped.Load() }

// SentLogs is the number of log records the collector accepted.
func (a *Agent) SentLogs() int64 { return a.logsSent.Load() }

func (a *Agent) flushLogs(ctx context.Context) {
	for {
		batch := make([]logRecord, 0, min(len(a.logs), batchSize))
	drain:
		for len(batch) < batchSize {
			select {
			case r := <-a.logs:
				batch = append(batch, r)
			default:
				break drain
			}
		}
		if len(batch) == 0 {
			return
		}
		body, _ := json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
			"resource": map[string]any{"attributes": a.resAttr},
			"scopeLogs": []any{map[string]any{
				"scope":      map[string]any{"name": "github.com/ymg4798/load-testing/agent"},
				"logRecords": batch,
			}},
		}}})
		if err := a.post(ctx, "/v1/logs", body); err != nil {
			a.logsDropped.Add(int64(len(batch)))
			a.logWarn.Do(func() { log.Printf("lt agent: sending logs: %v (will keep trying)", err) })
		} else {
			a.logsSent.Add(int64(len(batch)))
		}
		if len(batch) < batchSize || ctx.Err() != nil {
			return
		}
	}
}
