package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/run"
)

// Dashboard serves the live page and streams one event per second over
// Server-Sent Events. It is a run.Sink.
type Dashboard struct {
	srv  *http.Server
	ln   net.Listener
	meta run.Meta

	mu      sync.Mutex
	view    *View
	clients map[chan []byte]struct{}

	// Live sources, set before or during the run.
	srcMu     sync.Mutex
	actives   []run.ActiveSource
	apm       APMSource
	topSQL    func() []run.TopQuery
	alerts    func() []run.AlertEvent
	logs      run.LogSource
	history   func(from, to time.Time, n int) ([]Point, error)
	maxPoints int
}

// SetHistory lets the page browse stored history (watch mode).
func (d *Dashboard) SetHistory(f func(from, to time.Time, n int) ([]Point, error)) {
	d.srcMu.Lock()
	d.history = f
	d.srcMu.Unlock()
	d.mu.Lock()
	d.view.History = true
	d.mu.Unlock()
}

func (d *Dashboard) apiRange(w http.ResponseWriter, r *http.Request) {
	d.srcMu.Lock()
	h := d.history
	d.srcMu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	n, _ := strconv.Atoi(q.Get("points"))
	if to <= 0 {
		to = time.Now().Unix()
	}
	if n <= 0 || n > 5000 {
		n = 1000
	}
	ps, err := h(time.Unix(from, 0), time.Unix(to, 0), n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ps == nil {
		ps = []Point{}
	}
	writeJSON(w, map[string]any{"points": ps})
}

// MapSource and ErrorSource are optional extras of the APM collector.
type MapSource interface{ Map() *run.ServiceMap }
type ErrorSource interface{ Errors() []run.ErrorGroup }

// SetLogs lets the page search logs live.
func (d *Dashboard) SetLogs(l run.LogSource) { d.srcMu.Lock(); d.logs = l; d.srcMu.Unlock() }

func (d *Dashboard) apiLogs(w http.ResponseWriter, r *http.Request) {
	d.srcMu.Lock()
	l := d.logs
	d.srcMu.Unlock()
	limit := 300
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 5000 {
		limit = n
	}
	lines := []run.LogLine{}
	if l != nil {
		lines = l.Search(r.URL.Query().Get("q"), limit)
	} else {
		d.mu.Lock()
		if d.view.Logs != nil {
			lines = searchLines(d.view.Logs.Lines, r.URL.Query().Get("q"), limit)
		}
		d.mu.Unlock()
	}
	writeJSON(w, map[string]any{"lines": lines, "live": l != nil})
}

// searchLines is a small fallback for finished runs: substring or trace: filter.
func searchLines(all []run.LogLine, q string, limit int) []run.LogLine {
	var out []run.LogLine
	q = strings.ToLower(strings.TrimSpace(q))
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		l := all[i]
		if t, ok := strings.CutPrefix(q, "trace:"); ok {
			if strings.EqualFold(l.TraceID, t) {
				out = append(out, l)
			}
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(l.Message), q) {
			out = append(out, l)
		}
	}
	return out
}

// APMSource is what the dashboard asks the APM collector while live.
type APMSource interface {
	Trace(id string) *run.Trace
	Operations() []run.OpStat
}

// AddActive adds a source of in-flight transactions (engine, APM collector).
func (d *Dashboard) AddActive(s run.ActiveSource) {
	d.srcMu.Lock()
	d.actives = append(d.actives, s)
	d.srcMu.Unlock()
}

// SetAPM lets the page fetch traces and operations live.
func (d *Dashboard) SetAPM(a APMSource) { d.srcMu.Lock(); d.apm = a; d.srcMu.Unlock() }

// SetTopSQL lets the page fetch the databases' busiest statements live.
func (d *Dashboard) SetTopSQL(f func() []run.TopQuery) {
	d.srcMu.Lock()
	d.topSQL = f
	d.srcMu.Unlock()
}

// SetAlerts lets the page fetch alert events live.
func (d *Dashboard) SetAlerts(f func() []run.AlertEvent) {
	d.srcMu.Lock()
	d.alerts = f
	d.srcMu.Unlock()
}

// SetWatch turns the page into the always-on console that keeps the last
// n seconds.
func (d *Dashboard) SetWatch(n int) {
	d.mu.Lock()
	d.view.Watch = true
	d.maxPoints = n
	d.mu.Unlock()
}

// SetServerSeries updates the series list (names and units) the page
// knows about; watch mode learns them as sources report.
func (d *Dashboard) SetServerSeries(list []run.ServerSeries) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.view.Meta.Server = list
	d.meta.Server = list
	d.broadcast("meta", d.view.Meta)
}

func writeJSON(w http.ResponseWriter, v any) {
	data, err := marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(data))
}

func (d *Dashboard) apiActive(w http.ResponseWriter, _ *http.Request) {
	d.srcMu.Lock()
	srcs := append([]run.ActiveSource(nil), d.actives...)
	d.srcMu.Unlock()
	out := []metrics.ActiveTx{}
	for _, s := range srcs {
		out = append(out, s.Active(200)...)
	}
	writeJSON(w, map[string]any{"items": out, "at": time.Now()})
}

func (d *Dashboard) apiTrace(w http.ResponseWriter, r *http.Request) {
	d.srcMu.Lock()
	a := d.apm
	d.srcMu.Unlock()
	var t *run.Trace
	if a != nil {
		t = a.Trace(r.URL.Query().Get("id"))
	}
	if t == nil {
		d.mu.Lock()
		if d.view.APM != nil {
			for i := range d.view.APM.Traces {
				if d.view.APM.Traces[i].TraceID == r.URL.Query().Get("id") {
					t = &d.view.APM.Traces[i]
				}
			}
		}
		d.mu.Unlock()
	}
	if t == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, t)
}

func (d *Dashboard) apiLive(w http.ResponseWriter, _ *http.Request) {
	d.srcMu.Lock()
	a, top, al, lg := d.apm, d.topSQL, d.alerts, d.logs
	d.srcMu.Unlock()
	out := map[string]any{}
	if lg != nil {
		out["logs"] = map[string]any{"live": true}
	}
	if a != nil {
		out["operations"] = a.Operations()
		if m, ok := a.(MapSource); ok {
			out["map"] = m.Map()
		}
		if e, ok := a.(ErrorSource); ok {
			out["errors"] = e.Errors()
		}
	}
	if eg, ok := lg.(interface{ ErrorGroups() []run.ErrorGroup }); ok {
		groups := eg.ErrorGroups()
		if prev, ok := out["errors"].([]run.ErrorGroup); ok {
			groups = append(prev, groups...)
		}
		if len(groups) > 0 {
			out["errors"] = groups
		}
	}
	if top != nil {
		out["topSql"] = top()
	}
	if al != nil {
		out["alerts"] = al()
	}
	writeJSON(w, out)
}

// NewDashboard starts listening on addr (e.g. ":5665").
func NewDashboard(addr string, meta run.Meta) (*Dashboard, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	d := &Dashboard{ln: ln, meta: meta, clients: map[chan []byte]struct{}{},
		view: &View{Meta: meta, Live: true, Summary: &metrics.Summary{}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.page)
	mux.HandleFunc("GET /events", d.events)
	mux.HandleFunc("GET /api/active", d.apiActive)
	mux.HandleFunc("GET /api/trace", d.apiTrace)
	mux.HandleFunc("GET /api/live", d.apiLive)
	mux.HandleFunc("GET /api/logs", d.apiLogs)
	mux.HandleFunc("GET /api/range", d.apiRange)
	d.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = d.srv.Serve(ln) }()
	return d, nil
}

// URL is where the dashboard can be opened.
func (d *Dashboard) URL() string {
	addr := d.ln.Addr().(*net.TCPAddr)
	host := addr.IP.String()
	if addr.IP.IsUnspecified() {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s/", net.JoinHostPort(host, fmt.Sprint(addr.Port)))
}

func (d *Dashboard) page(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	v := *d.view
	if !v.Done {
		v.Points = nil // the page gets everything from /events
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = render(w, d.meta.Name+" · lt live", &v)
}

func (d *Dashboard) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan []byte, 64)
	d.mu.Lock()
	initial, _ := marshal(d.view)
	d.clients[ch] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.clients, ch)
		d.mu.Unlock()
	}()
	fmt.Fprintf(w, "event: init\ndata: %s\n", initial)
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write(msg)
			fl.Flush()
		}
	}
}

func (d *Dashboard) broadcast(event string, v any) {
	data, err := marshal(v)
	if err != nil {
		return
	}
	msg := []byte("event: " + event + "\ndata: " + data + "\n")
	for ch := range d.clients {
		select {
		case ch <- msg:
		default: // slow client: skip this update rather than stall the test
		}
	}
}

// Snapshot implements run.Sink.
func (d *Dashboard) Snapshot(s *metrics.Snapshot, acc *metrics.Accumulator) {
	p := PointOf(s)
	sum := acc.Summary()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.view.Points = append(d.view.Points, p)
	if d.maxPoints > 0 && len(d.view.Points) > d.maxPoints {
		d.view.Points = append([]Point(nil), d.view.Points[len(d.view.Points)-d.maxPoints:]...)
	}
	d.view.Summary = sum
	d.broadcast("point", map[string]any{"point": p, "summary": sum})
}

// Done implements run.Sink.
func (d *Dashboard) Done(r *run.Result) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := ViewOf(r)
	v.Live = true
	d.view = v
	d.broadcast("done", map[string]any{"meta": v.Meta, "summary": v.Summary, "thresholds": v.Thresholds, "passed": v.Passed,
		"warnings": v.Warnings, "insights": v.Insights, "stopped": v.Stopped, "compare": v.Compare,
		"alerts": v.Alerts, "topSql": v.TopSQL, "apm": v.APM, "logs": v.Logs})
}

// Close stops the server.
func (d *Dashboard) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.mu.Lock()
	for ch := range d.clients {
		close(ch)
		delete(d.clients, ch)
	}
	d.mu.Unlock()
	if err := d.srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_ = d.srv.Close()
	}
}
