package output

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/run"
)

// Verdict is a one-word outcome: PASS, FAIL, REGRESSED or STOPPED.
func Verdict(r *run.Result) string {
	switch {
	case r.Stopped != nil:
		return "STOPPED"
	case !r.Passed:
		return "FAIL"
	case r.Compare != nil && r.Compare.Regressed:
		return "REGRESSED"
	}
	return "PASS"
}

func verdictIcon(v string) string {
	switch v {
	case "PASS":
		return "✅"
	case "STOPPED":
		return "🛑"
	}
	return "❌"
}

// OneLine is a short summary for chat notifications.
func OneLine(r *run.Result) string {
	s := r.Summary
	v := Verdict(r)
	line := fmt.Sprintf("%s lt %s: %s — %d requests (%.1f/s), errors %.2f%%, p95 %s, p99 %s",
		verdictIcon(v), r.Meta.Name, v, s.Requests, s.RPS, s.ErrorRate*100, ms(s.Latency.P95), ms(s.Latency.P99))
	if r.Meta.Label != "" {
		line += " [" + r.Meta.Label + "]"
	}
	for _, t := range r.Thresholds {
		if !t.Pass {
			line += fmt.Sprintf("\n• threshold failed: %s %s (actual %s)", t.Metric, t.Expr, t.Actual)
		}
	}
	if r.Compare != nil && r.Compare.Regressed {
		for _, row := range r.Compare.Rows {
			if len(row.Regressed) > 0 {
				line += fmt.Sprintf("\n• regressed vs baseline: %s %v", row.Name, row.Regressed)
			}
		}
	}
	n := 0
	for _, in := range r.Insights {
		if in.Level != "info" && n < 3 {
			line += "\n• " + in.Title
			n++
		}
	}
	return line
}

func mdEscape(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// WriteMarkdown writes the result as GitHub-flavored Markdown, for
// $GITHUB_STEP_SUMMARY, pull request comments or chat.
func WriteMarkdown(w io.Writer, r *run.Result) {
	s := r.Summary
	v := Verdict(r)
	fmt.Fprintf(w, "## %s lt: %s — %s\n\n", verdictIcon(v), mdEscape(r.Meta.Name), v)
	meta := []string{(time.Duration(s.Duration * float64(time.Second))).Round(100 * time.Millisecond).String()}
	if r.Meta.Label != "" {
		meta = append([]string{"`" + r.Meta.Label + "`"}, meta...)
	}
	if r.Meta.Agents > 1 {
		meta = append(meta, fmt.Sprintf("%d agents", r.Meta.Agents))
	}
	meta = append(meta,
		fmt.Sprintf("%d requests (%.1f/s)", s.Requests, s.RPS),
		fmt.Sprintf("errors %.2f%%", s.ErrorRate*100),
		fmt.Sprintf("p95 %s", ms(s.Latency.P95)),
		fmt.Sprintf("p99 %s", ms(s.Latency.P99)))
	if s.Dropped > 0 {
		meta = append(meta, fmt.Sprintf("dropped %d", s.Dropped))
	}
	fmt.Fprintln(w, strings.Join(meta, " · "))
	fmt.Fprintln(w)

	if r.Stopped != nil {
		fmt.Fprintf(w, "> 🛑 Stopped at %ds: %s\n\n", r.Stopped.T, mdEscape(r.Stopped.Reason))
	}

	fmt.Fprintln(w, "| step | count | rps | err% | p50 | p95 | p99 | max |")
	fmt.Fprintln(w, "|---|--:|--:|--:|--:|--:|--:|--:|")
	multi := len(r.Meta.Scenarios) > 1
	row := func(name string, k *metrics.KeySummary) {
		fmt.Fprintf(w, "| %s | %d | %.1f | %.2f | %s | %s | %s | %s |\n", mdEscape(name), k.Count, k.RPS, k.ErrorRate*100,
			ms(k.P50), ms(k.P95), ms(k.P99), ms(k.Max))
	}
	for _, k := range s.Keys {
		if metrics.IsInternal(k.Key) {
			continue
		}
		name := k.Step
		if multi {
			name = k.Scenario + "/" + k.Step
		}
		row(name, k)
	}
	row("**all requests**", &metrics.KeySummary{Count: s.Requests, RPS: s.RPS, ErrorRate: s.ErrorRate, Stats: s.Latency})
	fmt.Fprintln(w)

	if len(r.Thresholds) > 0 {
		fmt.Fprintln(w, "### Thresholds")
		fmt.Fprintln(w)
		for _, t := range r.Thresholds {
			mark := "✅"
			if !t.Pass {
				mark = "❌"
			}
			fmt.Fprintf(w, "- %s `%s` %s — actual %s\n", mark, t.Metric, mdEscape(t.Expr), t.Actual)
		}
		fmt.Fprintln(w)
	}

	if c := r.Compare; c != nil {
		fmt.Fprintf(w, "### Compared with baseline\n\n`%s` (%s)\n\n", c.Base, c.BaseStarted.Format("2006-01-02 15:04"))
		fmt.Fprintln(w, "| step | rps | p95 | Δ | p99 | Δ | err% | Δpp | |")
		fmt.Fprintln(w, "|---|--:|--:|--:|--:|--:|--:|--:|---|")
		for _, row := range c.Rows {
			if row.Base == nil || row.New == nil {
				which := "only in new"
				if row.New == nil {
					which = "only in baseline"
				}
				fmt.Fprintf(w, "| %s | %s | | | | | | | |\n", mdEscape(row.Name), which)
				continue
			}
			b, n := row.Base, row.New
			mark := ""
			if len(row.Regressed) > 0 {
				mark = "❌ " + strings.Join(row.Regressed, ", ")
			}
			fmt.Fprintf(w, "| %s | %.1f | %s | %s | %s | %s | %.2f | %+.2f | %s |\n", mdEscape(row.Name), n.RPS,
				ms(n.P95), pctChange(float64(b.P95), float64(n.P95)), ms(n.P99), pctChange(float64(b.P99), float64(n.P99)),
				n.ErrorRate*100, (n.ErrorRate-b.ErrorRate)*100, mark)
		}
		fmt.Fprintln(w)
	}

	if len(r.Insights) > 0 {
		fmt.Fprintln(w, "### Findings")
		fmt.Fprintln(w)
		for _, in := range r.Insights {
			icon := map[string]string{"critical": "🔴", "warn": "🟠"}[in.Level]
			if icon == "" {
				icon = "🔵"
			}
			fmt.Fprintf(w, "- %s **%s**", icon, mdEscape(in.Title))
			if in.Detail != "" {
				fmt.Fprintf(w, " — %s", mdEscape(in.Detail))
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
	}

	if ex := slowest(r, 5); len(ex) > 0 {
		fmt.Fprintln(w, "### Slowest traced requests")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| step | latency | at | trace |")
		fmt.Fprintln(w, "|---|--:|--:|---|")
		for _, e := range ex {
			fmt.Fprintf(w, "| %s | %s | %ds | %s |\n", mdEscape(e.step), ms(e.Latency), e.T, traceLink(r, e.TraceID, true))
		}
		fmt.Fprintln(w)
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "### Warnings")
		fmt.Fprintln(w)
		for _, m := range r.Warnings {
			fmt.Fprintf(w, "- ⚠️ %s\n", mdEscape(m))
		}
		fmt.Fprintln(w)
	}
}

func pctChange(a, b float64) string {
	if a == 0 {
		if b == 0 {
			return "0%"
		}
		return "new"
	}
	return fmt.Sprintf("%+.1f%%", (b-a)/a*100)
}

type stepExemplar struct {
	step string
	metrics.Exemplar
}

// slowest returns the n slowest traced requests over every step.
func slowest(r *run.Result, n int) []stepExemplar {
	var all []stepExemplar
	for _, k := range r.Summary.Keys {
		for _, e := range k.Slowest {
			all = append(all, stepExemplar{k.Scenario + "/" + k.Step, e})
		}
	}
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].Latency > all[j-1].Latency; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// traceLink shows a trace ID, as a Markdown link when a trace URL is set.
func traceLink(r *run.Result, id string, md bool) string {
	if r.Meta.TraceURL == "" || !md {
		return "`" + id + "`"
	}
	return fmt.Sprintf("[`%s`](%s)", id, strings.ReplaceAll(r.Meta.TraceURL, "{traceId}", id))
}
