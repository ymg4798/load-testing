// Package monitor collects the target's own metrics while a test runs, so
// a report shows server CPU, memory or any Prometheus metric next to the
// load that caused it. Sources are Prometheus text endpoints (raw metrics
// or a preset for the usual exporters), local processes (by name or pid),
// Docker containers (Engine API) and direct PostgreSQL, MySQL and Redis
// connections, which also report the statements that took the most time
// during the test (Top SQL).
package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/run"
	"github.com/ymg4798/lt/internal/scenario"
	"github.com/ymg4798/lt/internal/tmpl"
)

// Defaults when a Prometheus target lists no metrics; missing ones are skipped.
var defaultMetrics = []string{
	"process_cpu_seconds_total",
	"process_resident_memory_bytes",
	"go_memstats_heap_inuse_bytes",
	"go_goroutines",
}

type series struct {
	display string
	unit    string // "%", "bytes", "/s" or ""
	sel     *selector
	cpu     bool // process_cpu_seconds_total: shown as % of one core

	prev   float64
	prevAt time.Time
	have   bool
}

type target struct {
	spec   scenario.MonitorTarget
	prefix string
	series []*series

	// process targets
	procPrev   procUsage
	procPrevAt time.Time

	preset presetFunc       // url targets with preset:
	rates  map[string]point // counter state for presets and databases

	// direct database connections
	kind   string // "postgres", "mysql" or "redis"
	source string // TopQuery.Source
	db     dbSource
	topMu  sync.Mutex
	base   map[string]stmtStat // statement totals when the test started
	noTop  bool                // the server has no statement statistics

	docker *dockerSource // docker engine targets

	warnMu sync.Mutex
	warned map[string]bool
}

// Monitor scrapes every target once a second and keeps the latest values.
type Monitor struct {
	targets []*target
	client  *http.Client

	mu     sync.Mutex
	latest map[string]float64
	units  map[string]string
	stop   context.CancelFunc
	done   sync.WaitGroup
	closed sync.Once
}

// New prepares a monitor; it returns nil when there is nothing to watch.
// URLs, headers, process names and DSNs may use {{ var }} from the file's
// vars. Database connections are opened lazily, on the first scrape.
func New(specs []scenario.MonitorTarget, vars map[string]any) (*Monitor, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	render := func(s string) (string, error) {
		t, err := tmpl.Compile(s)
		if err != nil {
			return "", err
		}
		return t.RenderString(tmpl.MapScope(vars))
	}
	m := &Monitor{client: &http.Client{Timeout: 900 * time.Millisecond}, latest: map[string]float64{}, units: map[string]string{}}
	ok := false
	defer func() {
		if !ok {
			m.closeSources()
		}
	}()
	for _, sp := range specs {
		var err error
		if sp.URL, err = render(sp.URL); err != nil {
			return nil, fmt.Errorf("monitor url: %w", err)
		}
		if sp.Process, err = render(sp.Process); err != nil {
			return nil, fmt.Errorf("monitor process: %w", err)
		}
		hs := map[string]string{}
		for k, v := range sp.Headers {
			if hs[k], err = render(v); err != nil {
				return nil, fmt.Errorf("monitor header %s: %w", k, err)
			}
		}
		sp.Headers = hs
		for _, f := range []*string{&sp.Postgres, &sp.MySQL, &sp.Redis, &sp.Docker} {
			if *f, err = render(*f); err != nil {
				return nil, fmt.Errorf("monitor dsn: %w", err)
			}
		}
		t := &target{spec: sp, prefix: sp.Name, rates: map[string]point{}, warned: map[string]bool{}}
		m.targets = append(m.targets, t)
		if kind := dbKind(sp); kind != "" {
			if err := t.openDB(kind, specs); err != nil {
				return nil, err
			}
			continue
		}
		if sp.Docker != "" {
			// Series are "<container> cpu" etc.; the target's name, if
			// any, goes in front.
			if t.docker, err = newDocker(sp.Docker, sp.Containers); err != nil {
				return nil, fmt.Errorf("monitor: %w", err)
			}
			continue
		}
		if sp.Preset != "" {
			if t.preset, err = lookupPreset(sp.Preset); err != nil {
				return nil, fmt.Errorf("monitor %s: %w", sp.URL, err)
			}
		}
		if t.prefix == "" && len(specs) > 1 {
			if sp.Process != "" {
				t.prefix = sp.Process
			} else if u, err := url.Parse(sp.URL); err == nil {
				t.prefix = u.Host
			}
		}
		if sp.URL != "" {
			list := sp.Metrics
			if len(list) == 0 && t.preset == nil {
				list = defaultMetrics
			}
			for _, expr := range list {
				sel, err := parseSelector(expr)
				if err != nil {
					return nil, fmt.Errorf("monitor %s: %w", sp.URL, err)
				}
				s := &series{sel: sel, display: expr}
				switch {
				case sel.name == "process_cpu_seconds_total" && len(sel.matchers) == 0:
					s.display, s.unit, s.cpu = "cpu", "%", true
				case sel.name == "process_resident_memory_bytes" && len(sel.matchers) == 0:
					s.display, s.unit = "memory", "bytes"
				case sel.name == "go_memstats_heap_inuse_bytes" && len(sel.matchers) == 0:
					s.display, s.unit = "heap", "bytes"
				case sel.name == "go_goroutines" && len(sel.matchers) == 0:
					s.display = "goroutines"
				case isCounterName(sel.name):
					s.unit = "/s"
				case strings.HasSuffix(sel.name, "_bytes"):
					s.unit = "bytes"
				}
				s.display = t.name(s.display)
				t.series = append(t.series, s)
			}
		}
	}
	ok = true
	return m, nil
}

func dbKind(sp scenario.MonitorTarget) string {
	switch {
	case sp.Postgres != "":
		return "postgres"
	case sp.MySQL != "":
		return "mysql"
	case sp.Redis != "":
		return "redis"
	}
	return ""
}

// openDB prepares a direct database target. Its series are already named
// "pg …", "mysql …" or "redis …"; the target's name, or its host when
// several targets are of the same kind, goes in front.
func (t *target) openDB(kind string, specs []scenario.MonitorTarget) error {
	var host string
	var err error
	switch kind {
	case "postgres":
		var s *pgSource
		s, host, err = newPostgres(t.spec.Postgres)
		if err == nil {
			t.db = s
		}
	case "mysql":
		var s *mysqlSource
		s, host, err = newMySQL(t.spec.MySQL)
		if err == nil {
			t.db = s
		}
	case "redis":
		var s *redisSource
		s, host, err = newRedis(t.spec.Redis)
		if err == nil {
			t.db = s
		}
	}
	if err != nil {
		return fmt.Errorf("monitor %s: %w", kind, err)
	}
	t.kind = kind
	same := 0
	for _, sp := range specs {
		if dbKind(sp) == kind {
			same++
		}
	}
	if t.prefix == "" && same > 1 {
		t.prefix = host
	}
	t.source = t.spec.Name
	if t.source == "" {
		t.source = kind
		if t.prefix != "" {
			t.source += " " + t.prefix
		}
	}
	return nil
}

func (t *target) name(s string) string {
	if t.prefix == "" {
		return s
	}
	return t.prefix + " " + s
}

func isCounterName(n string) bool {
	for _, suf := range []string{"_total", "_count", "_sum", "_bucket"} {
		if strings.HasSuffix(n, suf) {
			return true
		}
	}
	return false
}

// Start begins scraping in the background until Close.
func (m *Monitor) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.stop = cancel
	for _, t := range m.targets {
		m.done.Add(1)
		go func() {
			defer m.done.Done()
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				if t.db != nil {
					m.baseline(ctx, t)
				}
				m.scrape(ctx, t)
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}()
	}
}

// Close stops scraping and closes database connections.
func (m *Monitor) Close() {
	m.closed.Do(func() {
		if m.stop != nil {
			m.stop()
			m.done.Wait()
		}
		m.closeSources()
	})
}

func (m *Monitor) closeSources() {
	for _, t := range m.targets {
		if t.db != nil {
			t.db.close()
		}
		if t.docker != nil {
			t.docker.close()
		}
	}
}

func (m *Monitor) set(name, unit string, v float64) {
	m.mu.Lock()
	m.latest[name] = v
	m.units[name] = unit
	m.mu.Unlock()
}

// drop forgets the latest values of series that went away (a stopped
// container); they stay listed in Series.
func (m *Monitor) drop(names []string) {
	m.mu.Lock()
	for _, n := range names {
		delete(m.latest, n)
	}
	m.mu.Unlock()
}

func (m *Monitor) warn(t *target, err error) {
	m.warnOnce(t, "scrape", fmt.Sprintf("%v (will keep trying)", err))
}

// warnOnce prints msg the first time key comes up for t.
func (m *Monitor) warnOnce(t *target, key, msg string) {
	t.warnMu.Lock()
	defer t.warnMu.Unlock()
	if !t.warned[key] {
		t.warned[key] = true
		fmt.Fprintf(os.Stderr, "monitor: %s\n", msg)
	}
}

func (m *Monitor) scrape(ctx context.Context, t *target) {
	now := time.Now()
	if t.db != nil {
		hctx, cancel := context.WithTimeout(ctx, healthTimeout)
		defer cancel()
		if err := t.db.health(hctx, newCalc(t.rates, now, m.set, t.name)); err != nil && ctx.Err() == nil {
			m.warn(t, fmt.Errorf("%s: %w", t.source, err))
		}
		return
	}
	if t.docker != nil {
		err := t.docker.scrape(ctx, now, dockerEmit{
			set: m.set, drop: m.drop, name: t.name,
			warn: func(key, msg string) { m.warnOnce(t, key, msg) },
		})
		if err != nil && ctx.Err() == nil {
			m.warn(t, err)
		}
		return
	}
	if t.spec.Process != "" {
		u, err := readProcess(ctx, t.spec.Process)
		if err != nil {
			if ctx.Err() == nil {
				m.warn(t, err)
			}
			return
		}
		if !t.procPrevAt.IsZero() && u.cpu >= t.procPrev.cpu {
			dt := now.Sub(t.procPrevAt).Seconds()
			m.set(t.name("cpu"), "%", (u.cpu-t.procPrev.cpu).Seconds()/dt*100)
		}
		m.set(t.name("memory"), "bytes", float64(u.rss))
		t.procPrev, t.procPrevAt = u, now
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.spec.URL, nil)
	if err != nil {
		m.warn(t, err)
		return
	}
	for k, v := range t.spec.Headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			m.warn(t, fmt.Errorf("%s: %w", t.spec.URL, err))
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		m.warn(t, fmt.Errorf("%s: status %d", t.spec.URL, resp.StatusCode))
		return
	}
	samples, types, err := parseText(resp.Body)
	if err != nil {
		m.warn(t, fmt.Errorf("%s: %w", t.spec.URL, err))
		return
	}
	if t.preset != nil {
		c := newCalc(t.rates, now, m.set, t.name)
		c.fam = groupSamples(samples)
		t.preset(c)
	}
	for _, s := range t.series {
		var sum float64
		found := false
		for _, x := range samples {
			if s.sel.match(x) && !math.IsNaN(x.value) {
				sum += x.value
				found = true
			}
		}
		if !found {
			continue
		}
		counter := s.cpu || s.unit == "/s" || types[s.sel.name] == "counter"
		if !counter {
			m.set(s.display, s.unit, sum)
			continue
		}
		if s.have && sum >= s.prev {
			v := (sum - s.prev) / now.Sub(s.prevAt).Seconds()
			unit := s.unit
			if s.cpu {
				v *= 100
			} else if unit == "" {
				unit = "/s"
			}
			m.set(s.display, unit, v)
		}
		s.prev, s.prevAt, s.have = sum, now, true
	}
}

// Decorate implements run.Decorator: it attaches the latest values.
func (m *Monitor) Decorate(s *metrics.Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.latest) == 0 {
		return
	}
	if s.Server == nil {
		s.Server = make(map[string]float64, len(m.latest))
	}
	for k, v := range m.latest {
		s.Server[k] = v
	}
}

// Snapshot implements run.Sink.
func (m *Monitor) Snapshot(*metrics.Snapshot, *metrics.Accumulator) {}

// Done implements run.Sink.
func (m *Monitor) Done(*run.Result) {}

// Series lists the series seen so far, sorted by name.
func (m *Monitor) Series() []run.ServerSeries {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []run.ServerSeries
	for n, u := range m.units {
		out = append(out, run.ServerSeries{Name: n, Unit: u})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// baseline records the statement statistics the test starts from; it is
// retried each second until it works or the server turns out to have none.
func (m *Monitor) baseline(ctx context.Context, t *target) {
	t.topMu.Lock()
	need := t.base == nil && !t.noTop
	t.topMu.Unlock()
	if !need {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, stmtTimeout)
	defer cancel()
	cur, err := t.db.statements(sctx)
	t.topMu.Lock()
	defer t.topMu.Unlock()
	if err != nil {
		m.topErr(t, err, ctx)
		return
	}
	t.base = cur
}

// topErr is called with t.topMu held.
func (m *Monitor) topErr(t *target, err error, ctx context.Context) {
	var none noStatementsError
	switch {
	case errors.As(err, &none):
		t.noTop = true
		m.warnOnce(t, "top", t.source+": "+none.msg)
	case ctx.Err() == nil:
		m.warnOnce(t, "top", fmt.Sprintf("%s: top sql: %v (will keep trying)", t.source, err))
	}
}

// TopSQL returns the statements that took the most time since the monitor
// started: the top 20 of each database by total time, busiest first.
// It queries the databases, so call it at most every few seconds.
func (m *Monitor) TopSQL() []run.TopQuery {
	var (
		mu  sync.Mutex
		out []run.TopQuery
		wg  sync.WaitGroup
	)
	for _, t := range m.targets {
		if t.db == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := m.topOf(t)
			mu.Lock()
			out = append(out, q...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sortTop(out)
	return out
}

func (m *Monitor) topOf(t *target) []run.TopQuery {
	t.topMu.Lock()
	base, off := t.base, t.noTop
	t.topMu.Unlock()
	if base == nil || off {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), stmtTimeout)
	defer cancel()
	cur, err := t.db.statements(ctx)
	if err != nil {
		t.topMu.Lock()
		m.topErr(t, err, ctx)
		t.topMu.Unlock()
		return nil
	}
	return topDelta(t.source, base, cur, topPerSource)
}

// Finish implements run.Finisher: it adds the test's Top SQL to the result.
func (m *Monitor) Finish(r *run.Result) {
	q := m.TopSQL()
	if len(q) == 0 {
		return
	}
	r.TopSQL = append(r.TopSQL, q...)
	sortTop(r.TopSQL)
}
