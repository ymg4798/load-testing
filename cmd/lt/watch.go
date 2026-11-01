package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ymg4798/load-testing/internal/apm"
	"github.com/ymg4798/load-testing/internal/history"
	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/monitor"
	"github.com/ymg4798/load-testing/internal/prom"
	"github.com/ymg4798/load-testing/internal/run"
	"github.com/ymg4798/load-testing/internal/scenario"
	"github.com/ymg4798/load-testing/internal/store"
	"github.com/ymg4798/load-testing/internal/web"
)

// cmdWatch is the always-on console: server and database metrics, APM
// traces from the target, alerts — with no load test running.
func cmdWatch(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	dash := fs.String("dashboard", ":5665", "serve the console on this address")
	apmAddr := fs.String("apm", "", "receive traces here (lt agent or OTLP/HTTP), e.g. :4318; also apm.listen in the file")
	promAddr := fs.String("prometheus", "", "serve the watched series as Prometheus metrics, e.g. :9464")
	keep := fs.Duration("keep", time.Hour, "how much history the console keeps in memory (live view)")
	dataDir := fs.String("data", "", "where history is stored on disk (default .lt/watch/<name>; \"off\" to keep nothing)")
	retention := fs.Duration("retention", 7*24*time.Hour, "how long stored history is kept")
	var vars, notifyURLs multiFlag
	fs.Var(&vars, "e", "set a variable, name=value (repeatable)")
	fs.Var(&notifyURLs, "notify", "webhook for alerts: Slack, Discord or any JSON URL (repeatable; also $LT_NOTIFY)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: lt watch [flags] <watch.yaml>

Watches servers and databases (monitor), receives traces from the target
(apm) and evaluates alerts, continuously, with a live console. The file uses
the same monitor, apm and alerts sections as a scenario file; scenarios are
not needed.`)
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
	f, err := scenario.LoadWatch(pos[0], ov)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	name := f.Name
	if name == "" {
		name = strings.TrimSuffix(strings.TrimSuffix(pos[0], ".yaml"), ".yml")
	}

	mon, err := monitor.New(f.Monitor, f.Vars)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if mon != nil {
		mon.Start()
		defer mon.Close()
	}
	if *apmAddr == "" {
		*apmAddr = f.APM.Listen
	}
	var col *apm.Collector
	if *apmAddr != "" {
		if col, err = apm.New(*apmAddr); err != nil {
			return fail(exitError, "apm: %v", err)
		}
		defer col.Close()
		fmt.Fprintf(os.Stderr, "apm: send traces to http://%s (OTLP/HTTP or lt agent)\n", col.Addr())
	}
	alerter, err := run.NewAlerter(f.Alerts, alertNotifier(name, notifyURLs))
	if err != nil {
		return fail(exitUsage, "alerts: %v", err)
	}
	logStore, stopLogs, err := startLogs(f, col)
	if err != nil {
		return fail(exitUsage, "logs: %v", err)
	}
	defer stopLogs()
	if alerter != nil && logStore != nil {
		alerter.SetLogs(logStore)
	}

	var st *store.Store
	if *dataDir != "off" {
		dir := *dataDir
		if dir == "" {
			dir = filepath.Join(".lt", "watch", history.Slug(name))
		}
		if st, err = store.Open(dir, store.Options{Retention: *retention}); err != nil {
			return fail(exitError, "history: %v", err)
		}
		defer st.Close()
		fmt.Fprintf(os.Stderr, "history: %s (kept %s)\n", dir, *retention)
	}

	meta := run.Meta{Name: name, File: pos[0], Started: time.Now()}
	d, err := web.NewDashboard(*dash, meta)
	if err != nil {
		return fail(exitError, "dashboard: %v", err)
	}
	defer d.Close()
	window := int(keep.Seconds())
	d.SetWatch(window)
	if col != nil {
		d.AddActive(col)
		d.SetAPM(col)
	}
	if mon != nil {
		d.SetTopSQL(mon.TopSQL)
	}
	if alerter != nil {
		d.SetAlerts(alerter.Events)
	}
	if logStore != nil {
		d.SetLogs(logStore)
	}
	if st != nil {
		d.SetHistory(func(from, to time.Time, n int) ([]web.Point, error) {
			ps, err := st.Range(from, to, n)
			if err != nil {
				return nil, err
			}
			out := make([]web.Point, 0, len(ps))
			for _, p := range ps {
				wp := web.PointOf(p.Snapshot)
				wp.T = int(p.At.Unix())
				wp.Seconds = p.Seconds
				out = append(out, wp)
			}
			return out, nil
		})
		// Start the live view with what was recorded before a restart.
		if ps, err := st.Range(time.Now().Add(-*keep), time.Now(), window); err == nil {
			for _, p := range ps {
				p.Snapshot.T = int(p.At.Unix())
				d.Snapshot(p.Snapshot, metrics.NewAccumulator())
			}
		}
	}
	var promSink *prom.Exporter
	if *promAddr != "" {
		if promSink, err = prom.New(*promAddr, name); err != nil {
			return fail(exitError, "prometheus: %v", err)
		}
		defer promSink.Close()
	}
	fmt.Fprintf(os.Stderr, "watching %s — console at %s (Ctrl+C to stop)\n", name, d.URL())

	var ring []*metrics.Snapshot
	seen := ""
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for t := 1; ; t++ {
		select {
		case <-ctx.Done():
			return exitOK
		case <-tick.C:
		}
		now := time.Now()
		// T is wall-clock seconds in watch mode, so stored and live points line up.
		s := &metrics.Snapshot{T: int(now.Unix()), Keys: map[string]*metrics.KeySnap{}}
		if mon != nil {
			mon.Decorate(s) // first: it replaces the server map
		}
		if col != nil {
			col.Decorate(s)
		}
		if logStore != nil {
			logStore.Decorate(s)
		}
		ring = append(ring, s)
		if len(ring) > window {
			ring = ring[len(ring)-window:]
		}
		if alerter != nil {
			alerter.Observe(s.T, ring)
		}
		if st != nil {
			if err := st.Append(now, s); err != nil && err != store.ErrOutOfOrder {
				fmt.Fprintf(os.Stderr, "warning: history: %v\n", err)
			}
		}
		acc := metrics.NewAccumulator()
		d.Snapshot(s, acc)
		if promSink != nil {
			promSink.Snapshot(s, acc)
		}
		// Tell the page about new series (names and units) as sources report.
		var series []run.ServerSeries
		if mon != nil {
			series = append(series, mon.Series()...)
		}
		if col != nil {
			series = append(series, col.Series()...)
		}
		if logStore != nil {
			series = append(series, logStore.Series()...)
		}
		sort.Slice(series, func(i, j int) bool { return series[i].Name < series[j].Name })
		var b strings.Builder
		for _, x := range series {
			b.WriteString(x.Name + "\x00" + x.Unit + "\x00")
		}
		if b.String() != seen {
			seen = b.String()
			d.SetServerSeries(series)
		}
		if t%30 == 0 {
			fmt.Fprintf(os.Stderr, "%s  %s\n", time.Now().Format("15:04:05"), watchLine(s))
		}
	}
}

// watchLine is a short status line for the terminal.
func watchLine(s *metrics.Snapshot) string {
	keys := make([]string, 0, len(s.Server))
	for k := range s.Server {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		if len(parts) == 6 {
			parts = append(parts, fmt.Sprintf("+%d more", len(keys)-6))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.4g", k, s.Server[k]))
	}
	if len(parts) == 0 {
		return "waiting for data…"
	}
	return strings.Join(parts, " · ")
}
