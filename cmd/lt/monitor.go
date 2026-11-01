package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/notify"
	"github.com/ymg4798/load-testing/internal/run"
	"github.com/ymg4798/load-testing/internal/scenario"
)

// cmdMonitor runs each scenario's flow a few times on a schedule — a
// synthetic check that reuses load-test scenarios — and alerts when the
// outcome changes between passing and failing.
func cmdMonitor(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	every := fs.Duration("every", time.Minute, "time between checks")
	iters := fs.Int("iterations", 1, "flow runs per scenario per check")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up on a check after this long")
	count := fs.Int("count", 0, "stop after this many checks (0 = run until Ctrl+C)")
	promAddr := fs.String("prometheus", "", "serve check results as Prometheus metrics on this address, e.g. :9465")
	var vars, notifyURLs multiFlag
	fs.Var(&vars, "e", "set a variable, name=value (repeatable)")
	fs.Var(&notifyURLs, "notify", "webhook for failures and recoveries: Slack, Discord or any JSON URL (repeatable; also $LT_NOTIFY)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: lt monitor [flags] <scenario.yaml>

Runs every scenario's flow --iterations times, one user each, every --every.
A check fails when a request fails or a threshold fails. Rates, stages and
users in the file are ignored. Alerts go out when the state changes.`)
		fs.PrintDefaults()
	}
	pos, err := parse(fs, args)
	if err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if len(pos) != 1 {
		fs.Usage()
		return exitUsage
	}
	ov, err := parseVars(vars)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if env := os.Getenv("LT_NOTIFY"); env != "" {
		for _, u := range strings.Split(env, ",") {
			if u = strings.TrimSpace(u); u != "" {
				notifyURLs = append(notifyURLs, u)
			}
		}
	}
	load := func() (*scenario.File, error) {
		f, err := scenario.Load(pos[0], ov)
		if err != nil {
			return nil, err
		}
		for _, s := range f.Scenarios {
			s.Executor, s.Iterations, s.Users, s.Duration = scenario.Iterations, *iters, 1, 0
			s.Stages, s.StartAfter, s.Rate, s.StartRate, s.StartUsers = nil, 0, 0, 0, 0
		}
		f.Limits = nil
		f.Monitor = nil
		if f.Options.GracefulStop.D() > *timeout {
			f.Options.GracefulStop = 0
		}
		return f, f.Validate()
	}
	if _, err := load(); err != nil {
		return fail(exitUsage, "%v", err)
	}

	st := &monitorState{name: pos[0]}
	if *promAddr != "" {
		ln, err := net.Listen("tcp", *promAddr)
		if err != nil {
			return fail(exitError, "prometheus: %v", err)
		}
		srv := &http.Server{Handler: st, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
		fmt.Fprintf(os.Stderr, "prometheus: http://%s/metrics\n", ln.Addr())
	}

	fmt.Fprintf(os.Stderr, "monitoring %s every %s (Ctrl+C to stop)\n", pos[0], *every)
	prevOK, first := true, true
	for n := 1; ; n++ {
		f, err := load() // re-read, so edits apply without a restart
		var res *run.Result
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, *timeout)
			res, err = run.Local(cctx, f)
			cancel()
		}
		if ctx.Err() != nil {
			return exitOK
		}
		ok, line := judge(res, err)
		fmt.Printf("%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
		st.record(res, ok)
		if ok != prevOK || (first && !ok) {
			text := "✅ lt monitor: " + nameOf(res, pos[0]) + " recovered\n" + line
			if !ok {
				text = "❌ lt monitor: " + nameOf(res, pos[0]) + " failing\n" + line
			}
			nctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			for _, u := range notifyURLs {
				r := res
				if r == nil {
					r = &run.Result{Tool: "lt", Meta: run.Meta{Name: nameOf(nil, pos[0])}}
				}
				if err := notify.Webhook(nctx, u, text, r); err != nil {
					fmt.Fprintf(os.Stderr, "warning: notify: %v\n", err)
				}
			}
			cancel()
		}
		prevOK, first = ok, false
		if *count > 0 && n >= *count {
			if !ok {
				return exitThresholds
			}
			return exitOK
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(*every):
		}
	}
}

func nameOf(r *run.Result, path string) string {
	if r != nil && r.Meta.Name != "" {
		return r.Meta.Name
	}
	return path
}

// judge turns one check into pass/fail and a line of text.
func judge(r *run.Result, err error) (bool, string) {
	if r == nil {
		return false, "FAIL  " + err.Error()
	}
	s := r.Summary
	ok := s.Errors == 0 && r.Passed && s.Requests > 0
	var parts []string
	for _, k := range s.Keys {
		if metrics.IsInternal(k.Key) {
			continue
		}
		p := fmt.Sprintf("%s %s", k.Step, metrics.FormatMicros(k.Mean))
		if k.Errors > 0 {
			var kinds []string
			for kind := range k.ErrKinds {
				kinds = append(kinds, kind)
			}
			sort.Strings(kinds)
			p += " (" + strings.Join(kinds, ",") + ")"
		}
		parts = append(parts, p)
	}
	for _, t := range r.Thresholds {
		if !t.Pass {
			parts = append(parts, fmt.Sprintf("threshold %s %s: %s", t.Metric, t.Expr, t.Actual))
		}
	}
	if s.Requests == 0 {
		parts = append(parts, "no requests completed")
	}
	if err != nil {
		parts = append(parts, err.Error())
	}
	word := "OK  "
	if !ok {
		word = "FAIL"
	}
	return ok, word + "  " + strings.Join(parts, " · ")
}

// monitorState serves the latest check as Prometheus metrics.
type monitorState struct {
	mu       sync.Mutex
	name     string
	up       bool
	checks   int64
	failures int64
	last     time.Time
	steps    map[string]float64 // mean seconds
}

func (m *monitorState) record(r *run.Result, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks++
	if !ok {
		m.failures++
	}
	m.up, m.last = ok, time.Now()
	m.steps = map[string]float64{}
	if r != nil {
		m.name = r.Meta.Name
		for _, k := range r.Summary.Keys {
			if !metrics.IsInternal(k.Key) {
				m.steps[k.Step] = k.Mean / 1e6
			}
		}
	}
}

func (m *monitorState) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace
	t := esc(m.name)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	up := 0
	if m.up {
		up = 1
	}
	fmt.Fprintf(w, "# HELP lt_monitor_up 1 when the last check passed.\n# TYPE lt_monitor_up gauge\nlt_monitor_up{test=\"%s\"} %d\n", t, up)
	fmt.Fprintf(w, "# HELP lt_monitor_checks_total Checks run.\n# TYPE lt_monitor_checks_total counter\nlt_monitor_checks_total{test=\"%s\"} %d\n", t, m.checks)
	fmt.Fprintf(w, "# HELP lt_monitor_failures_total Checks that failed.\n# TYPE lt_monitor_failures_total counter\nlt_monitor_failures_total{test=\"%s\"} %d\n", t, m.failures)
	if !m.last.IsZero() {
		fmt.Fprintf(w, "# HELP lt_monitor_last_check_timestamp_seconds When the last check finished.\n# TYPE lt_monitor_last_check_timestamp_seconds gauge\nlt_monitor_last_check_timestamp_seconds{test=\"%s\"} %d\n", t, m.last.Unix())
	}
	fmt.Fprintln(w, "# HELP lt_monitor_step_seconds Mean latency of each step in the last check.\n# TYPE lt_monitor_step_seconds gauge")
	steps := make([]string, 0, len(m.steps))
	for s := range m.steps {
		steps = append(steps, s)
	}
	sort.Strings(steps)
	for _, s := range steps {
		fmt.Fprintf(w, "lt_monitor_step_seconds{test=\"%s\",step=\"%s\"} %g\n", t, esc(s), m.steps[s])
	}
}
