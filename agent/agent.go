// Package agent is a small APM agent for Go services. It traces incoming
// HTTP requests, database/sql calls and outgoing HTTP calls, and reports
// them to lt (or any OpenTelemetry collector) as OTLP/HTTP JSON. To lt it
// also reports the requests in flight right now, with the SQL or call each
// one is waiting on.
//
// It has no dependencies outside the standard library.
//
//	a := agent.Start(agent.Config{Service: "shop", Endpoint: "http://localhost:4318"})
//	defer a.Shutdown(context.Background())
//
//	db, err := agent.OpenDB("postgres", dsn)          // traced *sql.DB
//	client := &http.Client{Transport: agent.Transport(nil)}
//	mux := http.NewServeMux()
//	mux.HandleFunc("GET /items/{id}", getItem)
//	http.ListenAndServe(":8080", agent.Middleware(mux))
//
// Incoming requests continue the caller's trace from a W3C traceparent,
// B3 or uber-trace-id header, so a load test run by lt with tracing on
// links each request it sent to the server's own spans.
//
// One agent runs per process: Middleware, Transport, the database wrapper
// and StartSpan use the agent most recently started, and do nothing (but
// pass calls through) while none is running.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config sets up an agent.
type Config struct {
	Service  string // service.name; default: the executable's name
	Endpoint string // collector base URL; default http://localhost:4318
	Instance string // this process; default host:pid
	// FlushInterval is how often spans are sent; default 1s.
	FlushInterval time.Duration
	// QueueSize bounds the spans waiting to be sent; more are dropped. Default 20000.
	QueueSize int
	// StackAfter is how long a request runs before the active list shows
	// where its goroutine is (its "active stack"), captured at most once a
	// second and only while such requests exist. Default 1s; negative
	// turns active stacks (and the per-request pprof label) off.
	StackAfter time.Duration
	// LogQueueSize bounds the log records (from SlogHandler) waiting to be
	// sent; more are dropped. Default 10000.
	LogQueueSize int
}

// Agent collects spans and sends them in the background.
type Agent struct {
	cfg     Config
	queue   chan *Span
	client  *http.Client
	dropped atomic.Int64
	sent    atomic.Int64
	stop    chan context.Context
	done    chan struct{}
	warn    sync.Once
	resAttr []jsonKV

	logs        chan logRecord
	logsDropped atomic.Int64
	logsSent    atomic.Int64
	logWarn     sync.Once

	stackBuf bytes.Buffer // used by the loop goroutine only

	mu     sync.Mutex
	active map[*tx]struct{}
}

var current atomic.Pointer[Agent]

// Start starts an agent and makes it the process's agent.
func Start(cfg Config) *Agent {
	if cfg.Service == "" {
		cfg.Service = "unknown_service"
		if exe, err := os.Executable(); err == nil {
			cfg.Service = exe[strings.LastIndexAny(exe, `/\`)+1:]
		}
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "http://localhost:4318"
	}
	cfg.Endpoint = strings.TrimSuffix(strings.TrimSuffix(cfg.Endpoint, "/"), "/v1/traces")
	if cfg.Instance == "" {
		h, _ := os.Hostname()
		cfg.Instance = fmt.Sprintf("%s:%d", h, os.Getpid())
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 20_000
	}
	if cfg.StackAfter == 0 {
		cfg.StackAfter = time.Second
	}
	if cfg.LogQueueSize <= 0 {
		cfg.LogQueueSize = 10_000
	}
	a := &Agent{
		cfg:    cfg,
		queue:  make(chan *Span, cfg.QueueSize),
		logs:   make(chan logRecord, cfg.LogQueueSize),
		client: &http.Client{Timeout: 5 * time.Second},
		stop:   make(chan context.Context),
		done:   make(chan struct{}),
		active: map[*tx]struct{}{},
		resAttr: []jsonKV{
			kv("service.name", cfg.Service),
			kv("service.instance.id", cfg.Instance),
			kv("telemetry.sdk.name", "lt-agent"),
			kv("telemetry.sdk.language", "go"),
		},
	}
	current.Store(a)
	go a.loop()
	return a
}

// Shutdown sends what is queued and stops the agent.
func (a *Agent) Shutdown(ctx context.Context) error {
	current.CompareAndSwap(a, nil)
	select {
	case a.stop <- ctx:
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-a.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dropped is the number of spans lost because the queue was full or the
// collector could not be reached.
func (a *Agent) Dropped() int64 { return a.dropped.Load() }

// Sent is the number of spans the collector accepted.
func (a *Agent) Sent() int64 { return a.sent.Load() }

func (a *Agent) enqueue(s *Span) {
	select {
	case a.queue <- s:
	default:
		a.dropped.Add(1)
	}
}

func (a *Agent) loop() {
	defer close(a.done)
	flush := time.NewTicker(a.cfg.FlushInterval)
	defer flush.Stop()
	act := time.NewTicker(time.Second)
	defer act.Stop()
	for {
		select {
		case ctx := <-a.stop:
			a.flush(ctx)
			a.flushLogs(ctx)
			a.postActive(ctx, true)
			return
		case <-flush.C:
			a.flush(context.Background())
			a.flushLogs(context.Background())
		case <-act.C:
			a.postActive(context.Background(), false)
		}
	}
}

const batchSize = 1000

func (a *Agent) flush(ctx context.Context) {
	for {
		batch := make([]*Span, 0, min(len(a.queue), batchSize))
	drain:
		for len(batch) < batchSize {
			select {
			case s := <-a.queue:
				batch = append(batch, s)
			default:
				break drain
			}
		}
		if len(batch) == 0 {
			return
		}
		if err := a.post(ctx, "/v1/traces", a.encode(batch)); err != nil {
			a.dropped.Add(int64(len(batch)))
			a.warn.Do(func() { log.Printf("lt agent: sending spans: %v (will keep trying)", err) })
		} else {
			a.sent.Add(int64(len(batch)))
		}
		if len(batch) < batchSize || ctx.Err() != nil {
			return
		}
	}
}

func (a *Agent) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return nil
}

// ---- active transactions ----

// tx is a request being handled: the active list shows it with its
// current activity (running SQL, outgoing call).
type tx struct {
	span     *Span
	start    time.Time
	traceID  string
	label    string // pprof label value (span ID); "" when stacks are off
	name     atomic.Pointer[string]
	activity atomic.Pointer[string]
	stack    atomic.Pointer[string] // last active stack captured
}

func (a *Agent) addTx(t *tx) {
	a.mu.Lock()
	a.active[t] = struct{}{}
	a.mu.Unlock()
}

func (a *Agent) removeTx(t *tx) {
	a.mu.Lock()
	delete(a.active, t)
	a.mu.Unlock()
}

type activeItem struct {
	Name      string `json:"name"`
	ElapsedMs int64  `json:"elapsedMs"`
	Detail    string `json:"detail,omitempty"`
	TraceID   string `json:"traceId,omitempty"`
	Stack     string `json:"stack,omitempty"`
}

func (a *Agent) postActive(ctx context.Context, empty bool) {
	items := []activeItem{}
	if !empty {
		now := time.Now()
		var slow []*tx
		a.mu.Lock()
		for t := range a.active {
			if t.label != "" && now.Sub(t.start) >= a.cfg.StackAfter {
				slow = append(slow, t)
			}
		}
		a.mu.Unlock()
		stacks := a.captureSlow(slow)
		a.mu.Lock()
		for t := range a.active {
			it := activeItem{Name: *t.name.Load(), ElapsedMs: now.Sub(t.start).Milliseconds(), TraceID: t.traceID}
			if d := t.activity.Load(); d != nil {
				it.Detail = *d
			}
			if st, ok := stacks[t]; ok {
				it.Stack = st
			}
			items = append(items, it)
		}
		a.mu.Unlock()
	}
	body, _ := json.Marshal(map[string]any{"service": a.cfg.Service, "instance": a.cfg.Instance, "active": items})
	_ = a.post(ctx, "/v1/lt/active", body) // best effort; the collector forgets stale lists
}

// captureSlow takes the stacks of the longest-running slow requests (at
// most maxStacksPerRp) and remembers each on its transaction, for the
// span's lt.stack attribute.
func (a *Agent) captureSlow(slow []*tx) map[*tx]string {
	if len(slow) == 0 {
		return nil
	}
	sort.Slice(slow, func(i, j int) bool { return slow[i].start.Before(slow[j].start) })
	if len(slow) > maxStacksPerRp {
		slow = slow[:maxStacksPerRp]
	}
	want := make(map[string]bool, len(slow))
	for _, t := range slow {
		want[t.label] = true
	}
	got := captureStacks(&a.stackBuf, want)
	if a.stackBuf.Cap() > 4<<20 { // don't keep a huge buffer after a spike
		a.stackBuf = bytes.Buffer{}
	}
	out := make(map[*tx]string, len(got))
	for _, t := range slow {
		if st, ok := got[t.label]; ok {
			t.stack.Store(&st)
			out[t] = st
		}
	}
	return out
}

// setActivity marks what the request in ctx is waiting on and returns a
// function that puts the previous activity back.
func setActivity(ctx context.Context, what string) func() {
	t, _ := ctx.Value(txKey{}).(*tx)
	if t == nil {
		return func() {}
	}
	prev := t.activity.Swap(&what)
	return func() { t.activity.Store(prev) }
}

// ---- OTLP/JSON encoding ----

type jsonKV struct {
	Key   string    `json:"key"`
	Value jsonValue `json:"value"`
}

type jsonValue struct {
	String *string  `json:"stringValue,omitempty"`
	Int    string   `json:"intValue,omitempty"`
	Bool   *bool    `json:"boolValue,omitempty"`
	Double *float64 `json:"doubleValue,omitempty"`
}

func kv(k string, v any) jsonKV {
	out := jsonKV{Key: k}
	switch x := v.(type) {
	case string:
		out.Value.String = &x
	case int:
		out.Value.Int = fmt.Sprint(x)
	case int64:
		out.Value.Int = fmt.Sprint(x)
	case bool:
		out.Value.Bool = &x
	case float64:
		out.Value.Double = &x
	default:
		s := fmt.Sprint(x)
		out.Value.String = &s
	}
	return out
}

type jsonSpan struct {
	TraceID      string      `json:"traceId"`
	SpanID       string      `json:"spanId"`
	ParentSpanID string      `json:"parentSpanId,omitempty"`
	Name         string      `json:"name"`
	Kind         int         `json:"kind"`
	Start        string      `json:"startTimeUnixNano"`
	End          string      `json:"endTimeUnixNano"`
	Attributes   []jsonKV    `json:"attributes,omitempty"`
	Events       []jsonEvent `json:"events,omitempty"`
	Status       jsonStatus  `json:"status"`
}

type jsonEvent struct {
	Time       string   `json:"timeUnixNano"`
	Name       string   `json:"name"`
	Attributes []jsonKV `json:"attributes,omitempty"`
}

type jsonStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func (a *Agent) encode(batch []*Span) []byte {
	spans := make([]jsonSpan, 0, len(batch))
	for _, s := range batch {
		s.mu.Lock()
		js := jsonSpan{
			TraceID: hexID(s.traceID[:]), SpanID: hexID(s.spanID[:]),
			Name: s.name, Kind: int(s.kind),
			Start: fmt.Sprint(s.start.UnixNano()), End: fmt.Sprint(s.end.UnixNano()),
		}
		if s.parentID != ([8]byte{}) {
			js.ParentSpanID = hexID(s.parentID[:])
		}
		for _, at := range s.attrs {
			js.Attributes = append(js.Attributes, kv(at.k, at.v))
		}
		if s.errored {
			js.Status = jsonStatus{Code: 2, Message: s.errMsg}
		}
		if e := s.exc; e != nil {
			ev := jsonEvent{Time: fmt.Sprint(e.at.UnixNano()), Name: "exception",
				Attributes: []jsonKV{kv("exception.message", e.message)}}
			if e.typ != "" {
				ev.Attributes = append(ev.Attributes, kv("exception.type", e.typ))
			}
			if e.stack != "" {
				ev.Attributes = append(ev.Attributes, kv("exception.stacktrace", e.stack))
			}
			if e.escaped {
				ev.Attributes = append(ev.Attributes, kv("exception.escaped", true))
			}
			js.Events = []jsonEvent{ev}
		}
		s.mu.Unlock()
		spans = append(spans, js)
	}
	req := map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": a.resAttr},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]any{"name": "github.com/ymg4798/load-testing/agent"},
			"spans": spans,
		}},
	}}}
	b, _ := json.Marshal(req)
	return b
}
