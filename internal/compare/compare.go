// Package compare puts two results side by side and flags regressions.
package compare

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/run"
)

// Limits decide what counts as a regression.
type Limits struct {
	LatencyPct  float64 // e.g. 0.10: p95/p99 may get 10% slower
	ErrorPoints float64 // e.g. 0.01: error rate may rise by 1 percentage point
	MinMicros   int64   // ignore latency changes smaller than this (noise floor)
}

// Row is one step in both results.
type Row struct {
	Name      string
	Base, New *metrics.KeySummary
	Regressed []string
}

// Report is the whole comparison.
type Report struct {
	Rows      []Row
	Regressed bool
}

// Compare matches steps by key. "all requests" is always the last row.
func Compare(base, cur *run.Result, lim Limits) *Report {
	idx := func(r *run.Result) map[string]*metrics.KeySummary {
		m := map[string]*metrics.KeySummary{}
		for _, k := range r.Summary.Keys {
			if !metrics.IsInternal(k.Key) || k.Step == metrics.IterationStep {
				m[k.Key] = k
			}
		}
		return m
	}
	b, c := idx(base), idx(cur)
	var keys []string
	seen := map[string]bool{}
	for k := range b {
		keys, seen[k] = append(keys, k), true
	}
	for k := range c {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	rep := &Report{}
	add := func(name string, x, y *metrics.KeySummary) {
		row := Row{Name: name, Base: x, New: y}
		if x != nil && y != nil {
			row.Regressed = check(x, y, lim)
		}
		if len(row.Regressed) > 0 {
			rep.Regressed = true
		}
		rep.Rows = append(rep.Rows, row)
	}
	for _, k := range keys {
		sc, st := metrics.SplitKey(k)
		name := st
		if st == metrics.IterationStep {
			name = "iteration"
		}
		if len(base.Meta.Scenarios) > 1 || len(cur.Meta.Scenarios) > 1 {
			name = sc + "/" + name
		}
		add(name, b[k], c[k])
	}
	total := func(s *metrics.Summary) *metrics.KeySummary {
		return &metrics.KeySummary{Count: s.Requests, Errors: s.Errors, ErrorRate: s.ErrorRate, RPS: s.RPS, Stats: s.Latency}
	}
	add("all requests", total(base.Summary), total(cur.Summary))
	return rep
}

func check(b, c *metrics.KeySummary, lim Limits) []string {
	var out []string
	for _, q := range []struct {
		name string
		x, y int64
	}{{"p95", b.P95, c.P95}, {"p99", b.P99, c.P99}} {
		if q.x > 0 && q.y-q.x > lim.MinMicros && float64(q.y-q.x)/float64(q.x) > lim.LatencyPct {
			out = append(out, q.name)
		}
	}
	if c.ErrorRate-b.ErrorRate > lim.ErrorPoints {
		out = append(out, "errors")
	}
	return out
}

func pct(a, b float64) string {
	if a == 0 {
		if b == 0 {
			return "0%"
		}
		return "new"
	}
	return fmt.Sprintf("%+.1f%%", (b-a)/a*100)
}

// Print writes the comparison as a table.
func (r *Report) Print(w io.Writer, baseName, newName string) {
	fmt.Fprintf(w, "base: %s\nnew:  %s\n\n", baseName, newName)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "step\trps\tΔ\tp50\tΔ\tp95\tΔ\tp99\tΔ\terr%\tΔpp\t\t")
	f := metrics.FormatMicros
	for _, row := range r.Rows {
		if row.Base == nil || row.New == nil {
			which := "only in new"
			if row.New == nil {
				which = "only in base"
			}
			fmt.Fprintf(tw, "%s\t%s\t\t\t\t\t\t\t\t\t\t\t\n", row.Name, which)
			continue
		}
		b, c := row.Base, row.New
		mark := ""
		if len(row.Regressed) > 0 {
			mark = fmt.Sprintf("REGRESSED %v", row.Regressed)
		}
		fmt.Fprintf(tw, "%s\t%.1f\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%+.2f\t%s\t\n",
			row.Name, c.RPS, pct(b.RPS, c.RPS),
			f(float64(c.P50)), pct(float64(b.P50), float64(c.P50)),
			f(float64(c.P95)), pct(float64(b.P95), float64(c.P95)),
			f(float64(c.P99)), pct(float64(b.P99), float64(c.P99)),
			c.ErrorRate*100, (c.ErrorRate-b.ErrorRate)*100, mark)
	}
	tw.Flush()
	if r.Regressed {
		fmt.Fprintln(w, "\nregression found")
	} else {
		fmt.Fprintln(w, "\nno regression")
	}
}

// Comparison converts the report for storing in a result file.
func (r *Report) Comparison(base *run.Result, baseName string) *run.Comparison {
	c := &run.Comparison{Base: baseName, BaseStarted: base.Meta.Started, Regressed: r.Regressed}
	for _, row := range r.Rows {
		c.Rows = append(c.Rows, run.ComparisonRow{Name: row.Name, Base: row.Base, New: row.New, Regressed: row.Regressed})
	}
	return c
}
