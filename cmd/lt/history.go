package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/ymg4798/load-testing/internal/history"
	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/web"
)

func cmdHistory(args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	dir := fs.String("dir", "", "history folder (default $LT_HISTORY or "+history.DefaultDir+")")
	html := fs.String("html", "", "write a trend chart (HTML) of the test's runs here")
	last := fs.Int("n", 30, "show at most this many recent runs")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage:
  lt history                     list tests with stored runs
  lt history <test> [--html f]   list a test's runs, optionally as a trend chart
  lt history pin <result file>   make a stored run the baseline (--baseline pinned)

Runs are stored by "lt run --save".`)
		fs.PrintDefaults()
	}
	pos, err := parse(fs, args)
	if err != nil {
		if isHelp(err) {
			return exitOK
		}
		return exitUsage
	}
	d := history.Dir(*dir)
	switch {
	case len(pos) == 2 && pos[0] == "pin":
		if err := history.Pin(pos[1]); err != nil {
			return fail(exitError, "%v", err)
		}
		fmt.Printf("pinned %s as the baseline\n", pos[1])
		return exitOK
	case len(pos) == 0:
		tests, err := history.Tests(d)
		if err != nil {
			return fail(exitError, "%v", err)
		}
		if len(tests) == 0 {
			fmt.Printf("no stored runs in %s (use lt run --save)\n", d)
			return exitOK
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "test\truns\tlast run")
		for _, t := range tests {
			list, err := history.List(d, t)
			if err != nil || len(list) == 0 {
				continue
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\n", t, len(list), list[len(list)-1].Started.Format("2006-01-02 15:04"))
		}
		tw.Flush()
		return exitOK
	case len(pos) == 1:
	default:
		fs.Usage()
		return exitUsage
	}

	list, err := history.List(d, pos[0])
	if err != nil {
		return fail(exitError, "%v", err)
	}
	if len(list) > *last {
		list = list[len(list)-*last:]
	}
	f := func(v int64) string { return metrics.FormatMicros(float64(v)) }
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "file\tstarted\tlabel\trps\tp50\tp95\tp99\terr%\tresult\t\t")
	for _, e := range list {
		res := "pass"
		switch {
		case e.Stopped != nil:
			res = "stopped"
		case !e.Passed:
			res = "fail"
		case e.Thresholds == 0:
			res = "-"
		}
		pin := ""
		if e.Pinned {
			pin = "★ baseline"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f\t%s\t%s\t%s\t%.2f\t%s\t%s\t\n", e.File, e.Started.Format("01-02 15:04"), e.Label,
			e.RPS, f(e.P50), f(e.P95), f(e.P99), e.ErrorRate*100, res, pin)
	}
	tw.Flush()
	if *html != "" {
		out, err := os.Create(*html)
		if err != nil {
			return fail(exitError, "%v", err)
		}
		if err := web.WriteTrend(out, pos[0], list); err != nil {
			out.Close()
			return fail(exitError, "%v", err)
		}
		if err := out.Close(); err != nil {
			return fail(exitError, "%v", err)
		}
		fmt.Fprintf(os.Stderr, "trend: %s\n", *html)
	}
	return exitOK
}
