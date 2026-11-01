// Package output prints progress and results to the terminal.
package output

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/run"
)

// Console prints one line per second and a summary at the end.
type Console struct {
	W      io.Writer
	Quiet  bool // no per-second lines
	color  bool
	header bool
}

// NewConsole writes to stdout, with color when stdout is a terminal.
func NewConsole(quiet bool) *Console {
	c := &Console{W: os.Stdout, Quiet: quiet}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "" {
		c.color = true
	}
	return c
}

func (c *Console) paint(code, s string) string {
	if !c.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func ms(us int64) string { return metrics.FormatMicros(float64(us)) }

// Snapshot prints the per-second progress line.
func (c *Console) Snapshot(s *metrics.Snapshot, _ *metrics.Accumulator) {
	if c.Quiet {
		return
	}
	if !c.header {
		c.header = true
		fmt.Fprintln(c.W, c.paint("2", fmt.Sprintf("%6s %9s %9s %9s %9s %9s %9s %7s %6s %8s %5s", "time", "target/s", "iter/s", "req/s", "p50", "p95", "p99", "err%", "drop", "inflight", "cpu")))
	}
	n, errs, st := s.Overall()
	errPct := 0.0
	if n > 0 {
		errPct = float64(errs) / float64(n) * 100
	}
	target := "-"
	if s.TargetRate > 0 {
		target = fmt.Sprintf("%.0f", s.TargetRate)
	}
	errS := fmt.Sprintf("%6.2f%%", errPct)
	if errs > 0 {
		errS = c.paint("31", errS)
	}
	drop := fmt.Sprintf("%6d", s.Dropped)
	if s.Dropped > 0 {
		drop = c.paint("33", drop)
	}
	cpu := fmt.Sprintf("%4.0f%%", s.Gen.CPU)
	if s.Gen.CPU > 85 {
		cpu = c.paint("33", cpu)
	}
	inflight := s.InFlight
	if s.Users > inflight {
		inflight = s.Users
	}
	var iters int64
	for k, ks := range s.Keys {
		if _, st := metrics.SplitKey(k); st == metrics.IterationStep {
			iters += ks.Count
		}
	}
	p50, p95, p99 := ms(st.P50), ms(st.P95), ms(st.P99)
	if n == 0 {
		p50, p95, p99 = "-", "-", "-"
	}
	fmt.Fprintf(c.W, "%5ds %9s %9d %9d %9s %9s %9s %s %s %8d %s\n",
		s.T, target, iters, n, p50, p95, p99, errS, drop, inflight, cpu)
}

// Done implements run.Sink. The summary is printed by Summary once the
// caller has added everything (baseline comparison) to the result.
func (c *Console) Done(*run.Result) {}

// Summary prints the end-of-test summary.
func (c *Console) Summary(r *run.Result) { PrintSummary(c.W, r, c.color) }

// PrintSummary prints a result the way `lt run` ends.
func PrintSummary(w io.Writer, r *run.Result, color bool) {
	c := &Console{W: w, color: color}
	s := r.Summary
	fmt.Fprintln(w)
	agents := ""
	if r.Meta.Agents > 1 {
		agents = fmt.Sprintf("   agents %d", r.Meta.Agents)
	}
	fmt.Fprintf(w, "%s   %s%s\n\n", c.paint("1", r.Meta.Name), (time.Duration(s.Duration * float64(time.Second))).Round(100*time.Millisecond), agents)
	if r.Stopped != nil {
		fmt.Fprintf(w, "  %s at %ds: %s\n\n", c.paint("31;1", "stopped"), r.Stopped.T, r.Stopped.Reason)
	}
	fmt.Fprintf(w, "  requests %d (%.1f/s)   errors %.2f%% (%d)   iterations %d   dropped %d\n\n",
		s.Requests, s.RPS, s.ErrorRate*100, s.Errors, s.Iterations, s.Dropped)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "  step\tcount\trps\terr%\tmin\tp50\tp90\tp95\tp99\tp99.9\tmax\t")
	row := func(name string, count int64, rps, er float64, st metrics.Stats) {
		fmt.Fprintf(tw, "  %s\t%d\t%.1f\t%.2f\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", name, count, rps, er*100,
			ms(st.Min), ms(st.P50), ms(st.P90), ms(st.P95), ms(st.P99), ms(st.P999), ms(st.Max))
	}
	multi := len(r.Meta.Scenarios) > 1
	for _, k := range s.Keys {
		if metrics.IsInternal(k.Key) {
			continue
		}
		name := k.Step
		if multi {
			name = k.Scenario + "/" + k.Step
		}
		row(name, k.Count, k.RPS, k.ErrorRate, k.Stats)
	}
	row("all requests", s.Requests, s.RPS, s.ErrorRate, s.Latency)
	for _, k := range s.Keys {
		if k.Step == metrics.IterationStep {
			name := "iteration"
			if multi {
				name = k.Scenario + "/iteration"
			}
			row(name, k.Count, k.RPS, k.ErrorRate, k.Stats)
		}
	}
	tw.Flush()

	type ek struct {
		key, kind string
		n         int64
	}
	var errs []ek
	for _, k := range s.Keys {
		for kind, n := range k.ErrKinds {
			name := k.Step
			if k.Step == metrics.IterationStep {
				name = "iteration"
			}
			if multi {
				name = k.Scenario + "/" + name
			}
			errs = append(errs, ek{name, kind, n})
		}
	}
	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool { return errs[i].n > errs[j].n })
		fmt.Fprintln(w, "\n  errors")
		for i, e := range errs {
			if i == 15 {
				fmt.Fprintf(w, "    ... %d more\n", len(errs)-15)
				break
			}
			fmt.Fprintf(w, "    %-28s %-22s %d\n", e.key, e.kind, e.n)
		}
	}

	if len(r.Thresholds) > 0 {
		fmt.Fprintln(w, "\n  thresholds")
		for _, t := range r.Thresholds {
			mark := c.paint("32", "✓")
			if !t.Pass {
				mark = c.paint("31", "✗")
			}
			fmt.Fprintf(w, "    %s %-28s %-18s actual %s\n", mark, t.Metric, t.Expr, t.Actual)
		}
	}
	if ex := slowest(r, 5); len(ex) > 0 {
		fmt.Fprintln(w, "\n  slowest traced requests")
		for _, e := range ex {
			fmt.Fprintf(w, "    %-28s %9s  at %4ds  trace %s\n", e.step, ms(e.Latency), e.T, e.TraceID)
		}
		if r.Meta.TraceURL != "" {
			fmt.Fprintf(w, "    open: %s\n", strings.ReplaceAll(r.Meta.TraceURL, "{traceId}", ex[0].TraceID))
		}
	}
	if cmp := r.Compare; cmp != nil {
		fmt.Fprintf(w, "\n  vs baseline %s\n", cmp.Base)
		for _, row := range cmp.Rows {
			if row.Base == nil || row.New == nil {
				continue
			}
			mark := c.paint("32", "✓")
			if len(row.Regressed) > 0 {
				mark = c.paint("31", "✗ "+strings.Join(row.Regressed, ","))
			}
			fmt.Fprintf(w, "    %-28s p95 %9s %8s   p99 %9s %8s   err %+.2fpp  %s\n", row.Name,
				ms(row.New.P95), pctChange(float64(row.Base.P95), float64(row.New.P95)),
				ms(row.New.P99), pctChange(float64(row.Base.P99), float64(row.New.P99)),
				(row.New.ErrorRate-row.Base.ErrorRate)*100, mark)
		}
	}
	if len(r.Insights) > 0 {
		fmt.Fprintln(w, "\n  findings")
		for _, in := range r.Insights {
			mark := c.paint("34", "•")
			switch in.Level {
			case "critical":
				mark = c.paint("31", "●")
			case "warn":
				mark = c.paint("33", "●")
			}
			fmt.Fprintf(w, "    %s %s\n", mark, in.Title)
			if in.Detail != "" {
				fmt.Fprintf(w, "      %s\n", c.paint("2", wrap(in.Detail, 90, "      ")))
			}
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "\n  warnings")
		for _, msg := range r.Warnings {
			fmt.Fprintf(w, "    %s %s\n", c.paint("33", "!"), wrap(msg, 90, "      "))
		}
	}
	fmt.Fprintf(w, "\n  generator: max cpu %.0f%%, max scheduling lag p99 %s\n", s.Gen.MaxCPU, ms(s.Gen.MaxLagP99))
	if len(r.Thresholds) > 0 || r.Compare != nil || r.Stopped != nil {
		v := Verdict(r)
		code := "31;1"
		if v == "PASS" {
			code = "32;1"
		}
		fmt.Fprintln(w, "\n  "+c.paint(code, v))
	}
}

func wrap(s string, width int, indent string) string {
	var b strings.Builder
	n := 0
	for i, word := range strings.Fields(s) {
		if i > 0 {
			if n+len(word)+1 > width {
				b.WriteString("\n" + indent)
				n = 0
			} else {
				b.WriteByte(' ')
				n++
			}
		}
		b.WriteString(word)
		n += len(word)
	}
	return b.String()
}
