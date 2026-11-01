// Package apm is lt's trace collector: the target's own tracing (the lt Go
// agent, or any OpenTelemetry SDK exporting OTLP/HTTP) reports spans to it
// while a test runs. It turns server transactions into a hitmap next to
// the client-side one, sums operations (endpoints, SQL, outgoing calls),
// keeps recent traces for drill-down, lists the requests the server is
// handling right now (with their stacks, from the lt agent), draws the
// service map and groups the errors.
//
// Endpoints:
//
//	POST /v1/traces     OTLP/HTTP traces (application/x-protobuf or application/json, gzip allowed)
//	POST /v1/lt/active  in-flight transactions from the lt agent (see ActiveReport)
//
// Other packages add endpoints on the same listener with Mount (lt's log
// store mounts OTLP logs at /v1/logs).
package apm

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/run"
)

// Memory bounds.
const (
	TraceWindow      = 10 * time.Minute // traces older than this (first seen) are dropped
	MaxTraces        = 20_000
	MaxSpansPerTrace = 500
	MaxSpans         = 200_000 // spans held over all traces
	MaxOperations    = 1_000   // further operations are counted as "(other)" per service and kind
	MaxReportOps     = 100
	MaxReportTraces  = 300
	ActiveTTL        = 5 * time.Second
	maxActivePerInst = 1_000
	maxBody          = 16 << 20
	maxBodyInflated  = 64 << 20
	maxQueued        = 100_000 // server transactions waiting for Decorate
	// pendingWait is how long a server span whose parent has not been seen
	// waits for it, to tell an entry point from a call within the service.
	pendingWait = 500 * time.Millisecond
)

type span struct {
	traceID, spanID, parentID string
	service, name, kind, op   string
	start, end                int64 // unix nanos
	err                       bool
	status                    int // HTTP status, 0 when unknown
	attrs                     map[string]string
}

func (s *span) durUs() int64 {
	if s.end <= s.start {
		return 0
	}
	return (s.end - s.start) / 1000
}

type traceEntry struct {
	spans   []*span
	byID    map[string]*span // built once the trace is large
	firstAt time.Time
	dropped int
}

const indexFrom = 32 // spans in a trace before lookups use a map

type opKey struct{ service, kind, name string }

type opStat struct {
	count, errors, totalUs, maxUs int64
	hist                          *hdr.Histogram
}

type pending struct {
	s  *span
	at time.Time
}

type activeEntry struct {
	at   time.Time
	list []ActiveItem
}

// ActiveReport is the body of POST /v1/lt/active: what one instance of a
// service is handling right now. Each report replaces the previous one of
// the same instance and is forgotten after 5s.
type ActiveReport struct {
	Service  string       `json:"service"`
	Instance string       `json:"instance"`
	Active   []ActiveItem `json:"active"`
}

// ActiveItem is one in-flight transaction.
type ActiveItem struct {
	Name      string `json:"name"`             // endpoint, "GET /items/{id}"
	ElapsedMs int64  `json:"elapsedMs"`        // at the time of the report
	Detail    string `json:"detail,omitempty"` // running SQL or outgoing call
	TraceID   string `json:"traceId,omitempty"`
	// Stack is where the request's goroutine is, for long-running ones.
	Stack string `json:"stack,omitempty"`
}

// Collector receives spans and active lists over HTTP.
type Collector struct {
	ln  net.Listener
	srv *http.Server
	now func() time.Time

	mu        sync.Mutex
	traces    map[string]*traceEntry
	order     []string // trace IDs by first sight, oldest first from head
	head      int
	nspans    int
	ingested  int64
	services  map[string]bool
	ops       map[opKey]*opStat
	pending   []pending
	done      []*span // server transactions finished since the last Decorate
	rootSvcs  map[string]bool
	series    map[string]string
	rnd       *rand.Rand
	active    map[string]*activeEntry
	badReqs   int64
	lastError string

	nodes        map[string]*nodeStat // service → its server spans
	edges        map[edgeKey]*edgeStat
	aliases      map[aliasKey]string // (caller, host) → service answering there
	edgesDropped int64
	errs         map[errKey]*errGroup
	stacks       map[string]string // interned stack texts

	mountMu sync.Mutex
	mounts  atomic.Pointer[map[string]http.Handler]
}

// New starts a collector listening on listen (such as ":4318").
func New(listen string) (*Collector, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("apm: %w", err)
	}
	c := newCollector()
	c.ln = ln
	c.srv = &http.Server{Handler: c, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = c.srv.Serve(ln) }()
	return c, nil
}

func newCollector() *Collector {
	return &Collector{
		now:      time.Now,
		traces:   map[string]*traceEntry{},
		services: map[string]bool{},
		ops:      map[opKey]*opStat{},
		rootSvcs: map[string]bool{},
		series:   map[string]string{},
		rnd:      rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
		active:   map[string]*activeEntry{},
		nodes:    map[string]*nodeStat{},
		edges:    map[edgeKey]*edgeStat{},
		aliases:  map[aliasKey]string{},
		errs:     map[errKey]*errGroup{},
		stacks:   map[string]string{},
	}
}

// Mount serves h at path on the collector's listener (exactly that path,
// or everything under it when path ends in "/"), next to the built-in
// endpoints, which cannot be replaced. It may be called at any time; a nil
// h removes the mount.
func (c *Collector) Mount(path string, h http.Handler) {
	if !strings.HasPrefix(path, "/") || path == "/v1/traces" || path == "/v1/lt/active" {
		panic("apm: cannot mount " + strconv.Quote(path))
	}
	c.mountMu.Lock()
	defer c.mountMu.Unlock()
	m := map[string]http.Handler{}
	if old := c.mounts.Load(); old != nil {
		for k, v := range *old {
			m[k] = v
		}
	}
	if h == nil {
		delete(m, path)
	} else {
		m[path] = h
	}
	c.mounts.Store(&m)
}

func (c *Collector) mounted(path string) http.Handler {
	m := c.mounts.Load()
	if m == nil {
		return nil
	}
	if h := (*m)[path]; h != nil {
		return h
	}
	best := ""
	for p := range *m {
		if strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) && len(p) > len(best) {
			best = p
		}
	}
	if best == "" {
		return nil
	}
	return (*m)[best]
}

// Addr is the address the collector listens on.
func (c *Collector) Addr() string {
	if c.ln == nil {
		return ""
	}
	return c.ln.Addr().String()
}

// Close stops listening.
func (c *Collector) Close() {
	if c.srv != nil {
		_ = c.srv.Close()
	}
}

// ServeHTTP serves the collector endpoints, so the collector can also be
// mounted on another server.
func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/traces":
		c.serveTraces(w, r)
	case "/v1/lt/active":
		c.serveActive(w, r)
	default:
		if h := c.mounted(r.URL.Path); h != nil {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

func readBody(r *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, r.Body, maxBody)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		rd = io.LimitReader(zr, maxBodyInflated+1)
	}
	b, err := io.ReadAll(rd)
	if err == nil && len(b) > maxBodyInflated {
		err = errors.New("body too large")
	}
	return b, err
}

func (c *Collector) fail(w http.ResponseWriter, code int, err error) {
	c.mu.Lock()
	c.badReqs++
	c.lastError = err.Error()
	c.mu.Unlock()
	http.Error(w, err.Error(), code)
}

func (c *Collector) serveTraces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	b, err := readBody(r)
	if err != nil {
		c.fail(w, http.StatusBadRequest, err)
		return
	}
	ct := r.Header.Get("Content-Type")
	isJSON := strings.Contains(ct, "json")
	if !isJSON && !strings.Contains(ct, "protobuf") {
		t := strings.TrimLeft(string(b[:min(len(b), 16)]), " \t\r\n")
		isJSON = strings.HasPrefix(t, "{")
	}
	var spans []rawSpan
	if isJSON {
		spans, err = decodeJSON(b)
	} else {
		spans, err = decodeProto(b)
	}
	if err != nil {
		c.fail(w, http.StatusBadRequest, err)
		return
	}
	c.ingest(spans)
	// An empty ExportTraceServiceResponse means full success.
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (c *Collector) serveActive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	b, err := readBody(r)
	if err != nil {
		c.fail(w, http.StatusBadRequest, err)
		return
	}
	var rep ActiveReport
	if err := json.Unmarshal(b, &rep); err != nil {
		c.fail(w, http.StatusBadRequest, err)
		return
	}
	if len(rep.Active) > maxActivePerInst {
		sort.Slice(rep.Active, func(i, j int) bool { return rep.Active[i].ElapsedMs > rep.Active[j].ElapsedMs })
		rep.Active = rep.Active[:maxActivePerInst]
	}
	if rep.Service == "" {
		rep.Service = "unknown_service"
	}
	for i := range rep.Active {
		rep.Active[i].Stack = truncUTF8(rep.Active[i].Stack, maxStack)
	}
	c.mu.Lock()
	c.active[rep.Service+"\x00"+rep.Instance] = &activeEntry{at: c.now(), list: rep.Active}
	c.services[rep.Service] = true
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// ingest stores decoded spans.
func (c *Collector) ingest(raw []rawSpan) {
	spans := make([]span, len(raw))
	var fails map[int]failure
	targets := make([]mapTarget, len(raw)) // callees of db and http spans, for the map
	for i := range raw {
		r := &raw[i]
		s := &spans[i]
		*s = span{
			traceID: r.traceID, spanID: r.spanID, parentID: r.parentID,
			service: r.service, name: r.name, start: r.start, end: r.end,
			attrs: r.attrs, err: r.errored,
		}
		s.kind = mapKind(r)
		s.status = httpStatus(r.attrs)
		if s.kind == KindServer && s.status >= 500 {
			s.err = true
		}
		if r.errored && r.statusMsg != "" {
			s.attrs = keepAttr(s.attrs, "exception.message", truncUTF8(r.statusMsg, 512))
		}
		if e := r.exc; e != nil {
			if e.message != "" {
				s.attrs = keepAttr(s.attrs, "exception.message", e.message)
			}
			if e.typ != "" {
				s.attrs = keepAttr(s.attrs, "exception.type", e.typ)
			}
			if e.stack != "" {
				s.attrs = keepAttr(s.attrs, "exception.stacktrace", e.stack)
			}
		}
		s.op = opName(s.kind, s.name, s.attrs)
		if s.kind == KindDB || s.kind == KindHTTP {
			targets[i], _ = callTarget(s)
		}
		if f, ok := spanFailure(s, r); ok {
			if fails == nil {
				fails = map[int]failure{}
			}
			fails[i] = f
		}
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evict(now)
	for i := range spans {
		s := &spans[i]
		if s.traceID == "" || s.spanID == "" {
			continue
		}
		c.ingested++
		c.services[s.service] = true
		c.countOp(s)
		c.mapSpan(s, targets[i])
		if f, ok := fails[i]; ok {
			c.countError(s, f)
		}
		if s.attrs["exception.stacktrace"] != "" || s.attrs["lt.stack"] != "" {
			c.internStacks(s.attrs)
		}
		t := c.traces[s.traceID]
		if t == nil {
			t = &traceEntry{firstAt: now}
			c.traces[s.traceID] = t
			c.order = append(c.order, s.traceID)
		}
		if len(t.spans) >= MaxSpansPerTrace {
			t.dropped++
		} else {
			t.spans = append(t.spans, s)
			c.nspans++
			if t.byID != nil {
				t.byID[s.spanID] = s
			} else if len(t.spans) == indexFrom {
				t.byID = make(map[string]*span, 2*indexFrom)
				for _, x := range t.spans {
					t.byID[x.spanID] = x
				}
			}
		}
		if s.kind == KindServer {
			c.classify(s, now)
		}
	}
	c.expirePending(now)
	c.evict(now)
}

// classify decides whether a server span is a transaction entry point: the
// first span of its service in the trace. When its parent has not arrived
// yet the decision waits (see pendingWait).
func (c *Collector) classify(s *span, now time.Time) {
	if s.parentID == "" {
		c.resolved(s, nil)
		return
	}
	if p := c.findSpan(s.traceID, s.parentID); p != nil {
		c.resolved(s, p)
		return
	}
	if len(c.pending) >= maxQueued {
		return // the operation stats still count it
	}
	c.pending = append(c.pending, pending{s: s, at: now})
}

// expirePending settles the server spans that waited long enough for
// their parent (the oldest are first). c.mu held.
func (c *Collector) expirePending(now time.Time) {
	n := 0
	for n < len(c.pending) && now.Sub(c.pending[n].at) >= pendingWait {
		p := c.pending[n].s
		c.resolved(p, c.findSpan(p.traceID, p.parentID))
		c.pending[n] = pending{}
		n++
	}
	if n > 0 {
		c.pending = c.pending[n:]
	}
}

// resolvePending settles every waiting server span whose parent has
// arrived or that waited long enough (all of them when force). c.mu held.
func (c *Collector) resolvePending(now time.Time, force bool) {
	keep := c.pending[:0]
	for _, p := range c.pending {
		par := c.findSpan(p.s.traceID, p.s.parentID)
		switch {
		case par != nil:
			c.resolved(p.s, par)
		case force || now.Sub(p.at) >= pendingWait:
			c.resolved(p.s, nil)
		default:
			keep = append(keep, p)
		}
	}
	clear(c.pending[len(keep):])
	c.pending = keep
}

func (c *Collector) findSpan(traceID, spanID string) *span {
	t := c.traces[traceID]
	if t == nil {
		return nil
	}
	if t.byID != nil {
		return t.byID[spanID]
	}
	for i := len(t.spans) - 1; i >= 0; i-- {
		if t.spans[i].spanID == spanID {
			return t.spans[i]
		}
	}
	return nil
}

// newOpHist is an operation's latency histogram (µs). Two significant
// digits instead of metrics.NewHist's three keep it at ~26KB, not ~190KB.
func newOpHist() *hdr.Histogram { return hdr.New(1, 3_600_000_000, 2) }

func (c *Collector) countOp(s *span) {
	k := opKey{s.service, s.kind, s.op}
	o := c.ops[k]
	if o == nil {
		if len(c.ops) >= MaxOperations {
			k.name = "(other)"
			o = c.ops[k]
		}
		if o == nil {
			o = &opStat{hist: newOpHist()}
			c.ops[k] = o
		}
	}
	d := s.durUs()
	o.count++
	if s.err {
		o.errors++
	}
	o.totalUs += d
	if d > o.maxUs {
		o.maxUs = d
	}
	_ = o.hist.RecordValue(max(d, 1))
}

// evict drops the oldest traces while over the window, count or span bounds.
func (c *Collector) evict(now time.Time) {
	for c.head < len(c.order) {
		id := c.order[c.head]
		t := c.traces[id]
		if t != nil && len(c.traces) <= MaxTraces && c.nspans <= MaxSpans && now.Sub(t.firstAt) < TraceWindow {
			break
		}
		if t != nil {
			c.nspans -= len(t.spans)
			delete(c.traces, id)
		}
		c.order[c.head] = ""
		c.head++
	}
	if c.head > 1024 && c.head*2 > len(c.order) {
		c.order = append(c.order[:0:0], c.order[c.head:]...)
		c.head = 0
	}
}

// Snapshot implements run.Sink.
func (c *Collector) Snapshot(*metrics.Snapshot, *metrics.Accumulator) {}

// Done implements run.Sink.
func (c *Collector) Done(*run.Result) {}

func errKind(s *span) string {
	switch {
	case s.status >= 500:
		return "status:" + strconv.Itoa(s.status)
	case s.err:
		return "error"
	}
	return ""
}

// Decorate implements run.Decorator: it adds the server transactions that
// finished since the previous call (as reported, so typically up to a
// second or two late) as the snapshot's APM hitmap, plus server series
// "apm tps", "apm mean" (ms) and "apm errors", per service when there are
// several.
func (c *Collector) Decorate(s *metrics.Snapshot) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolvePending(now, false)
	done := c.done
	c.done = nil
	for _, t := range done {
		c.rootSvcs[t.service] = true
	}
	if len(c.rootSvcs) == 0 {
		return
	}
	type agg struct {
		n, errs int
		totalUs int64
	}
	per := map[string]*agg{}
	for svc := range c.rootSvcs {
		per[svc] = &agg{}
	}
	var hits *metrics.Hits
	for _, t := range done {
		if hits == nil {
			hits = &metrics.Hits{}
		}
		e := errKind(t)
		hits.Add(metrics.HitSample{
			Key:     t.service + "|" + t.op,
			Off:     int((t.end / 1e6) % 1000),
			Latency: t.durUs(),
			Err:     e,
			TraceID: t.traceID,
		}, c.rnd)
		a := per[t.service]
		a.n++
		a.totalUs += t.durUs()
		if e != "" {
			a.errs++
		}
	}
	s.APMHits = hits
	if s.Server == nil {
		s.Server = map[string]float64{}
	}
	multi := len(c.rootSvcs) > 1
	for svc, a := range per {
		prefix := "apm "
		if multi {
			prefix = svc + " apm "
		}
		c.set(s, prefix+"tps", "/s", float64(a.n))
		c.set(s, prefix+"errors", "/s", float64(a.errs))
		if a.n > 0 {
			c.set(s, prefix+"mean", "ms", float64(a.totalUs)/float64(a.n)/1000)
		}
	}
}

func (c *Collector) set(s *metrics.Snapshot, name, unit string, v float64) {
	s.Server[name] = v
	c.series[name] = unit
}

// Series implements run.SeriesLister.
func (c *Collector) Series() []run.ServerSeries {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]run.ServerSeries, 0, len(c.series))
	for n, u := range c.series {
		out = append(out, run.ServerSeries{Name: n, Unit: u})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Services lists the services seen so far.
func (c *Collector) Services() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.servicesLocked()
}

func (c *Collector) servicesLocked() []string {
	out := make([]string, 0, len(c.services))
	for s := range c.services {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Operations returns every operation since start, busiest (by total time) first.
func (c *Collector) Operations() []run.OpStat {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.operationsLocked()
}

func (c *Collector) operationsLocked() []run.OpStat {
	out := make([]run.OpStat, 0, len(c.ops))
	for k, o := range c.ops {
		st := run.OpStat{
			Service: k.service, Kind: k.kind, Name: k.name,
			Count: o.count, Errors: o.errors,
			TotalMs: float64(o.totalUs) / 1000,
			MaxMs:   float64(o.maxUs) / 1000,
			P95Ms:   float64(o.hist.ValueAtQuantile(95)) / 1000,
		}
		if o.count > 0 {
			st.MeanMs = st.TotalMs / float64(o.count)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalMs != out[j].TotalMs {
			return out[i].TotalMs > out[j].TotalMs
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Trace returns a stored trace, or nil.
func (c *Collector) Trace(id string) *run.Trace {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.traceLocked(strings.ToLower(id))
}

func (c *Collector) traceLocked(id string) *run.Trace {
	t := c.traces[id]
	if t == nil || len(t.spans) == 0 {
		return nil
	}
	origin := t.spans[0].start
	for _, s := range t.spans {
		origin = min(origin, s.start)
	}
	out := &run.Trace{TraceID: id, Spans: make([]run.Span, 0, len(t.spans))}
	for _, s := range t.spans {
		out.Spans = append(out.Spans, run.Span{
			SpanID: s.spanID, ParentID: s.parentID, Service: s.service,
			Name: s.name, Kind: s.kind,
			StartUs: (s.start - origin) / 1000, DurUs: s.durUs(),
			Error: s.err, Attrs: s.attrs, // attrs are never changed after ingest
		})
	}
	sort.SliceStable(out.Spans, func(i, j int) bool { return out.Spans[i].StartUs < out.Spans[j].StartUs })
	return out
}

// Stats are the collector's own counters.
type Stats struct {
	Spans, StoredSpans, Traces, BadRequests int64
	LastError                               string
}

// Stats reports what the collector holds.
func (c *Collector) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Spans: c.ingested, StoredSpans: int64(c.nspans), Traces: int64(len(c.traces)), BadRequests: c.badReqs, LastError: c.lastError}
}

// Finish implements run.Finisher: it adds the APM report with the
// operations and the traces of the requests the report points at.
func (c *Collector) Finish(r *run.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ingested == 0 && len(c.services) == 0 {
		return
	}
	c.resolvePending(c.now(), true)
	rep := &run.APMReport{Services: c.servicesLocked(), Spans: c.ingested}
	rep.Operations = c.operationsLocked()
	rep.Map = c.mapLocked()
	errs := c.errorsLocked()
	if r.APM != nil { // keep what others (logs) put there
		rep.Errors = append(rep.Errors, r.APM.Errors...)
	}
	rep.Errors = append(rep.Errors, errs...)
	if len(rep.Operations) > MaxReportOps {
		rep.Operations = rep.Operations[:MaxReportOps]
	}
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.ToLower(id)
		if id == "" || seen[id] || len(rep.Traces) >= MaxReportTraces {
			return
		}
		seen[id] = true
		if t := c.traceLocked(id); t != nil {
			rep.Traces = append(rep.Traces, *t)
		}
	}
	if r.Summary != nil {
		for _, k := range r.Summary.Keys {
			for _, e := range k.Failed {
				add(e.TraceID)
			}
			for _, e := range k.Slowest {
				add(e.TraceID)
			}
		}
	}
	for _, g := range errs {
		add(g.TraceID)
	}
	// Then hitmap samples, failed and slow ones first.
	var samples []metrics.HitSample
	for _, s := range r.Snapshots {
		if s == nil {
			continue
		}
		if s.APMHits != nil {
			samples = append(samples, s.APMHits.Samples...)
		}
		if s.Hits != nil {
			samples = append(samples, s.Hits.Samples...)
		}
	}
	sort.SliceStable(samples, func(i, j int) bool {
		if (samples[i].Err != "") != (samples[j].Err != "") {
			return samples[i].Err != ""
		}
		return samples[i].Latency > samples[j].Latency
	})
	for _, s := range samples {
		add(s.TraceID)
	}
	r.APM = rep
}

// Active implements run.ActiveSource: the transactions agents reported in
// flight within the last 5s, longest running first. limit <= 0 means all.
func (c *Collector) Active(limit int) []metrics.ActiveTx {
	now := c.now()
	c.mu.Lock()
	var out []metrics.ActiveTx
	for k, e := range c.active {
		age := now.Sub(e.at)
		if age > ActiveTTL {
			delete(c.active, k)
			continue
		}
		svc, inst, _ := strings.Cut(k, "\x00")
		for _, a := range e.list {
			out = append(out, metrics.ActiveTx{
				Source: svc, Name: a.Name, Detail: a.Detail,
				ElapsedMs: a.ElapsedMs + age.Milliseconds(),
				Who:       inst, TraceID: a.TraceID, Stack: a.Stack,
			})
		}
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ElapsedMs > out[j].ElapsedMs })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

var (
	_ run.Sink         = (*Collector)(nil)
	_ run.Decorator    = (*Collector)(nil)
	_ run.Finisher     = (*Collector)(nil)
	_ run.SeriesLister = (*Collector)(nil)
	_ run.ActiveSource = (*Collector)(nil)
)
