package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymg4798/load-testing/internal/apm"
	"github.com/ymg4798/load-testing/internal/cluster"
	"github.com/ymg4798/load-testing/internal/compare"
	"github.com/ymg4798/load-testing/internal/engine"
	"github.com/ymg4798/load-testing/internal/history"
	"github.com/ymg4798/load-testing/internal/logs"
	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/monitor"
	"github.com/ymg4798/load-testing/internal/notify"
	"github.com/ymg4798/load-testing/internal/output"
	"github.com/ymg4798/load-testing/internal/preflight"
	"github.com/ymg4798/load-testing/internal/prom"
	"github.com/ymg4798/load-testing/internal/run"
	"github.com/ymg4798/load-testing/internal/scenario"
	"github.com/ymg4798/load-testing/internal/web"
)

func parseVars(list []string) (map[string]string, error) {
	m := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("-e expects name=value, got %q", kv)
		}
		m[k] = v
	}
	return m, nil
}

func cmdRun(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	out := fs.String("o", "", "write the result file (JSON) here")
	html := fs.String("html", "", "write an HTML report here")
	dash := fs.String("dashboard", "", "serve a live dashboard on this address, e.g. :5665")
	promAddr := fs.String("prometheus", "", "serve Prometheus metrics on this address, e.g. :9464")
	agents := fs.String("agents", "", "comma-separated agents (host:port) to run on instead of this machine")
	token := fs.String("token", os.Getenv("LT_TOKEN"), "agent token (default $LT_TOKEN)")
	quiet := fs.Bool("q", false, "no per-second lines")
	label := fs.String("label", "", "a note stored in the result (e.g. a commit)")
	keep := fs.Bool("keep", false, "keep the dashboard running after the test until Ctrl+C")
	save := fs.Bool("save", false, "store the result in the history folder (see lt history)")
	histDir := fs.String("history-dir", "", "history folder (default $LT_HISTORY or "+history.DefaultDir+")")
	baseline := fs.String("baseline", "", "compare with a baseline: last, pinned (from history) or a result file; regression exits 1")
	lat := fs.Float64("max-latency", 10, "with --baseline: allowed p95/p99 increase, percent")
	errPts := fs.Float64("max-errors", 1, "with --baseline: allowed error-rate increase, percentage points")
	floor := fs.Duration("noise", time.Millisecond, "with --baseline: ignore latency changes smaller than this")
	md := fs.String("md", "", "write a Markdown summary here (also appended to $GITHUB_STEP_SUMMARY when set)")
	notifyOn := fs.String("notify-on", "always", "when to send --notify: always or fail")
	apmAddr := fs.String("apm", "", "receive the target's traces here (lt agent or OpenTelemetry OTLP/HTTP), e.g. :4318; also apm.listen in the file")
	grafanaURL := fs.String("grafana", os.Getenv("LT_GRAFANA_URL"), "mark the test on Grafana dashboards, e.g. http://localhost:3000 (token: $LT_GRAFANA_TOKEN)")
	var vars, notifyURLs multiFlag
	fs.Var(&vars, "e", "set a variable, name=value (repeatable)")
	fs.Var(&notifyURLs, "notify", "post the outcome to a webhook: Slack, Discord or any JSON URL (repeatable; also $LT_NOTIFY)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: lt run [flags] <scenario.yaml>")
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
	f, err := scenario.Load(pos[0], ov)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if _, err := metrics.ParseThresholds(f.Thresholds); err != nil {
		return fail(exitUsage, "%v", err)
	}
	if _, err := metrics.ParseThresholds(f.Limits); err != nil {
		return fail(exitUsage, "limits: %v", err)
	}
	if *notifyOn != "always" && *notifyOn != "fail" {
		return fail(exitUsage, "--notify-on must be always or fail")
	}
	if env := os.Getenv("LT_NOTIFY"); env != "" {
		for _, u := range strings.Split(env, ",") {
			if u = strings.TrimSpace(u); u != "" {
				notifyURLs = append(notifyURLs, u)
			}
		}
	}
	hdir := history.Dir(*histDir)
	testName := run.MetaFor(f, 1).Name
	// Resolve the baseline before running, so "last" means the previous run.
	var base *run.Result
	var baseName string
	if *baseline != "" {
		path, err := history.Baseline(hdir, testName, *baseline)
		if err != nil {
			return fail(exitUsage, "baseline: %v", err)
		}
		if base, err = run.LoadResult(path); err != nil {
			return fail(exitUsage, "baseline: %v", err)
		}
		baseName = path
		if *baseline == "last" || *baseline == "pinned" {
			baseName = *baseline + ": " + filepath.Base(path)
		}
	}
	var agentList []string
	for _, a := range strings.Split(*agents, ",") {
		if a = strings.TrimSpace(a); a != "" {
			agentList = append(agentList, a)
		}
	}
	nAgents := len(agentList)
	if nAgents == 0 {
		nAgents = 1
		warnLimits()
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	meta := run.MetaFor(f, nAgents)
	console := output.NewConsole(*quiet)
	var sinks []run.Sink
	mon, err := monitor.New(f.Monitor, f.Vars)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if mon != nil {
		mon.Start()
		defer mon.Close()
		sinks = append(sinks, mon) // first, so others see its series
	}
	if *apmAddr == "" {
		*apmAddr = f.APM.Listen
	}
	var col *apm.Collector
	if *apmAddr != "" {
		c, err := apm.New(*apmAddr)
		if err != nil {
			return fail(exitError, "apm: %v", err)
		}
		col = c
		defer c.Close()
		sinks = append(sinks, lateSpans{c}) // after monitor: both write server series
		fmt.Fprintf(os.Stderr, "apm: send traces to http://%s (OTLP/HTTP or lt agent)\n", c.Addr())
	}
	logStore, stopLogs, err := startLogs(f, col)
	if err != nil {
		return fail(exitUsage, "logs: %v", err)
	}
	defer stopLogs()
	if logStore != nil {
		sinks = append(sinks, logStore) // after the collector: it adds to the APM report
	}
	sinks = append(sinks, console)
	limiter, err := run.NewLimiter(f.Limits, int(f.Options.LimitWindow.D().Seconds()), cancelRun)
	if err != nil {
		return fail(exitUsage, "limits: %v", err)
	}
	if limiter != nil {
		sinks = append(sinks, limiter)
	}
	alerter, err := run.NewAlerter(f.Alerts, alertNotifier(meta.Name, notifyURLs))
	if err != nil {
		return fail(exitUsage, "alerts: %v", err)
	}
	if alerter != nil {
		sinks = append(sinks, alerter)
		if logStore != nil {
			alerter.SetLogs(logStore)
		}
	}
	var dashboard *web.Dashboard
	if *dash != "" {
		d, err := web.NewDashboard(*dash, meta)
		if err != nil {
			return fail(exitError, "dashboard: %v", err)
		}
		dashboard = d
		defer d.Close()
		if alerter != nil {
			d.SetAlerts(alerter.Events)
		}
		if mon != nil {
			d.SetTopSQL(mon.TopSQL)
		}
		if col != nil {
			d.AddActive(col)
			d.SetAPM(col)
		}
		if logStore != nil {
			d.SetLogs(logStore)
		}
		sinks = append(sinks, d)
		fmt.Fprintf(os.Stderr, "dashboard: %s\n", d.URL())
	}
	if *promAddr != "" {
		p, err := prom.New(*promAddr, meta.Name)
		if err != nil {
			return fail(exitError, "prometheus: %v", err)
		}
		defer p.Close()
		sinks = append(sinks, p)
		fmt.Fprintf(os.Stderr, "prometheus: http://%s/metrics\n", p.Addr())
	}

	planned := time.Duration(meta.Planned * float64(time.Second))
	where := "this machine"
	if len(agentList) > 0 {
		where = fmt.Sprintf("%d agents", len(agentList))
	}
	fmt.Fprintf(os.Stderr, "running %s for %s on %s (Ctrl+C to stop)\n", meta.Name, planned, where)

	var gf *notify.Grafana
	if *grafanaURL != "" {
		gf = &notify.Grafana{URL: *grafanaURL, Token: os.Getenv("LT_GRAFANA_TOKEN")}
		if err := gf.Start(ctx, meta.Name, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "warning: grafana annotation: %v\n", err)
			gf = nil
		}
	}

	var res *run.Result
	if len(agentList) > 0 {
		c := &cluster.Controller{Agents: agentList, Token: *token}
		res, err = c.Run(runCtx, f, ov, sinks...)
	} else {
		res, err = run.LocalWith(runCtx, f, func(eng run.ActiveSource) {
			if dashboard != nil {
				dashboard.AddActive(eng)
			}
		}, sinks...)
	}
	if res == nil {
		return fail(exitError, "%v", err)
	}
	if err != nil && !(res.Stopped != nil && errors.Is(err, context.Canceled)) {
		fmt.Fprintf(os.Stderr, "lt: %v\n", err)
	}
	res.Meta.Label = *label
	if base != nil {
		rep := compare.Compare(base, res, compare.Limits{LatencyPct: *lat / 100, ErrorPoints: *errPts / 100, MinMicros: floor.Microseconds()})
		res.Compare = rep.Comparison(base, baseName)
	}
	console.Summary(res)
	if *out != "" {
		if err := run.Save(*out, res); err != nil {
			return fail(exitError, "write %s: %v", *out, err)
		}
		fmt.Fprintf(os.Stderr, "result: %s\n", *out)
	}
	if *html != "" {
		if err := writeReport(*html, res); err != nil {
			return fail(exitError, "write %s: %v", *html, err)
		}
		fmt.Fprintf(os.Stderr, "report: %s\n", *html)
	}
	if *save {
		path, err := history.Save(hdir, res)
		if err != nil {
			return fail(exitError, "save history: %v", err)
		}
		fmt.Fprintf(os.Stderr, "saved: %s\n", path)
	}
	if *md != "" {
		if err := writeMarkdown(*md, res, false); err != nil {
			return fail(exitError, "write %s: %v", *md, err)
		}
	}
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		if err := writeMarkdown(p, res, true); err != nil {
			fmt.Fprintf(os.Stderr, "warning: GITHUB_STEP_SUMMARY: %v\n", err)
		}
	}
	verdict := output.Verdict(res)
	if ctx.Err() == nil {
		// The test is over; give slow webhooks their own time.
		nctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if gf != nil {
			if err := gf.End(nctx, output.OneLine(res), verdict); err != nil {
				fmt.Fprintf(os.Stderr, "warning: grafana annotation: %v\n", err)
			}
		}
		if len(notifyURLs) > 0 && (*notifyOn == "always" || verdict != "PASS") {
			text := output.OneLine(res)
			for _, u := range notifyURLs {
				if err := notify.Webhook(nctx, u, text, res); err != nil {
					fmt.Fprintf(os.Stderr, "warning: notify: %v\n", err)
				}
			}
		}
		cancel()
	}
	if dashboard != nil && *keep && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "dashboard still at %s — Ctrl+C to exit\n", dashboard.URL())
		<-ctx.Done()
	}
	if ctx.Err() != nil {
		return exitError
	}
	if verdict != "PASS" {
		return exitThresholds
	}
	return exitOK
}

// startLogs follows the file's log sources and, with APM on, takes OTLP
// logs on the same listener. It returns nil when there is nothing to read.
func startLogs(f *scenario.File, col *apm.Collector) (*logs.Store, func(), error) {
	if len(f.Logs) == 0 && col == nil {
		return nil, func() {}, nil
	}
	st := logs.NewStore(logs.Options{})
	stop := func() {}
	if len(f.Logs) > 0 {
		fol, err := logs.FollowFiles(st, f.Logs, f.Dir, f.Vars)
		if err != nil {
			return nil, nil, err
		}
		stop = fol.Close
	}
	if col != nil {
		col.Mount("/v1/logs", st.OTLPHandler())
	}
	return st, stop, nil
}

// lateSpans gives agents a moment to flush their last spans before the
// collector builds the report (agents send once a second).
type lateSpans struct{ *apm.Collector }

func (l lateSpans) Finish(r *run.Result) {
	time.Sleep(1500 * time.Millisecond)
	l.Collector.Finish(r)
}

// alertNotifier prints alert events and posts them to the rule's webhooks,
// or the run's --notify webhooks when the rule names none.
func alertNotifier(test string, fallback []string) func(run.AlertEvent, []string) {
	return func(e run.AlertEvent, urls []string) {
		icon := "🔴"
		if e.State == "resolved" {
			icon = "✅"
		} else if e.Severity == "warning" {
			icon = "🟠"
		}
		text := fmt.Sprintf("%s lt %s: alert %s — %s (%s, value %s)", icon, test, e.State, e.Name, e.Rule, e.Value)
		fmt.Fprintln(os.Stderr, text)
		if len(urls) == 0 {
			urls = fallback
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, u := range urls {
			if err := notify.Text(ctx, u, text); err != nil {
				fmt.Fprintf(os.Stderr, "warning: alert notify: %v\n", err)
			}
		}
	}
}

func writeMarkdown(path string, r *run.Result, appendTo bool) error {
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	output.WriteMarkdown(f, r)
	return f.Close()
}

// warnLimits prints OS limits that are low enough to cap any sizable test.
// (maxInFlight is only a ceiling, so it is not used to estimate connections.)
func warnLimits() {
	for _, it := range preflight.Check(0) {
		if !it.OK {
			fmt.Fprintf(os.Stderr, "warning: %s is %s; %s\n", it.Name, it.Value, it.Advice)
		}
	}
}

func writeReport(path string, r *run.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := web.WriteReport(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	var vars multiFlag
	fs.Var(&vars, "e", "set a variable, name=value (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if len(pos) != 1 {
		return fail(exitUsage, "usage: lt validate <scenario.yaml>")
	}
	ov, err := parseVars(vars)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	f, err := scenario.Load(pos[0], ov)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if _, err := metrics.ParseThresholds(f.Thresholds); err != nil {
		return fail(exitUsage, "%v", err)
	}
	if err := validateFlows(f); err != nil {
		return fail(exitUsage, "%v", err)
	}
	m := run.MetaFor(f, 1)
	fmt.Printf("%s: ok — %d scenario(s), %s\n", filepath.Base(pos[0]), len(m.Scenarios), time.Duration(m.Planned*float64(time.Second)))
	for _, s := range m.Scenarios {
		fmt.Printf("  %-16s %-15s %d step(s)\n", s.Name, s.Executor, len(f.Scenarios[s.Name].Flow))
	}
	return exitOK
}

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	out := fs.String("o", "", "output HTML file (default: <result>.html)")
	pos, err := parse(fs, args)
	if err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if len(pos) != 1 {
		return fail(exitUsage, "usage: lt report <result.json> [-o report.html]")
	}
	r, err := run.LoadResult(pos[0])
	if err != nil {
		return fail(exitError, "%v", err)
	}
	path := *out
	if path == "" {
		path = strings.TrimSuffix(pos[0], filepath.Ext(pos[0])) + ".html"
	}
	if err := writeReport(path, r); err != nil {
		return fail(exitError, "%v", err)
	}
	fmt.Println(path)
	return exitOK
}

func cmdCompare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	lat := fs.Float64("max-latency", 10, "allowed p95/p99 increase, percent")
	errPts := fs.Float64("max-errors", 1, "allowed error-rate increase, percentage points")
	floor := fs.Duration("noise", time.Millisecond, "ignore latency changes smaller than this")
	pos, err := parse(fs, args)
	if err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if len(pos) != 2 {
		return fail(exitUsage, "usage: lt compare <base.json> <new.json>")
	}
	base, err := run.LoadResult(pos[0])
	if err != nil {
		return fail(exitError, "%v", err)
	}
	cur, err := run.LoadResult(pos[1])
	if err != nil {
		return fail(exitError, "%v", err)
	}
	rep := compare.Compare(base, cur, compare.Limits{LatencyPct: *lat / 100, ErrorPoints: *errPts / 100, MinMicros: floor.Microseconds()})
	rep.Print(os.Stdout, pos[0], pos[1])
	if rep.Regressed {
		return exitThresholds
	}
	return exitOK
}

func cmdAgent(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	addr := fs.String("listen", ":7700", "listen address")
	token := fs.String("token", os.Getenv("LT_TOKEN"), "require this bearer token (default $LT_TOKEN)")
	if _, err := parse(fs, args); err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "warning: no --token; anyone who can reach this port can run scenarios (including scripts) here")
	}
	a := &cluster.Agent{Token: *token, Version: version, Log: log.New(os.Stderr, "", log.LstdFlags)}
	if err := a.Serve(ctx, *addr); err != nil {
		return fail(exitError, "%v", err)
	}
	return exitOK
}

func cmdPreflight(args []string) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	conns := fs.Int("conns", 0, "connections you expect to hold open")
	if _, err := parse(fs, args); err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	bad := 0
	for _, it := range preflight.Check(*conns) {
		mark := "ok  "
		if !it.OK {
			mark = "WARN"
			bad++
		}
		fmt.Printf("%s %-28s %s\n", mark, it.Name, it.Value)
		if it.Advice != "" {
			fmt.Printf("     %s\n", it.Advice)
		}
	}
	if bad > 0 {
		return exitThresholds
	}
	return exitOK
}

func validateFlows(f *scenario.File) error { return engine.Validate(f) }
