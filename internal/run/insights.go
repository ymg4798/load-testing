package run

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/ymg4798/load-testing/internal/metrics"
)

// Insight is a finding about the target, read from the per-second record.
type Insight struct {
	Level  string `json:"level"` // info, warn, critical
	Kind   string `json:"kind"`  // latency_knee, error_onset, saturation, plateau, spike, stopped, slowest_step, error_hotspot, server
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	T      int    `json:"t,omitempty"` // second it starts, when it has one
}

type second struct {
	t                  int
	load               float64 // target rate, or users
	reqs, errs, iters  int64
	dropped            int64
	p95, p99           int64
	topErrKind, topKey string
}

func seconds(r *Result) []second {
	out := make([]second, 0, len(r.Snapshots))
	for _, s := range r.Snapshots {
		n, e, st := s.Overall()
		x := second{t: s.T, load: loadOf(s), reqs: n, errs: e, dropped: s.Dropped, p95: st.P95, p99: st.P99}
		var worst int64
		for k, ks := range s.Keys {
			if _, step := metrics.SplitKey(k); step == metrics.IterationStep {
				x.iters += ks.Count
				continue
			}
			if metrics.IsInternal(k) {
				continue
			}
			for kind, c := range ks.ErrKinds {
				if c > worst {
					worst, x.topErrKind, x.topKey = c, kind, k
				}
			}
		}
		out = append(out, x)
	}
	return out
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

func fmtLoad(r *Result, v float64) string {
	for _, s := range r.Meta.Scenarios {
		if s.Executor == "arrival-rate" {
			return fmt.Sprintf("%.0f/s", v)
		}
	}
	return fmt.Sprintf("%.0f users", v)
}

func us(v int64) string { return metrics.FormatMicros(float64(v)) }

func stepName(key string) string {
	sc, st := metrics.SplitKey(key)
	return sc + "/" + st
}

// Insights reads the per-second record and says, in plain words, where the
// target started to struggle. It looks only at the target; the generator's
// own health is covered by Warnings.
func Insights(r *Result) []Insight {
	ss := seconds(r)
	var out []Insight
	add := func(i Insight) { out = append(out, i) }

	if st := r.Stopped; st != nil {
		add(Insight{Level: "critical", Kind: "stopped", T: st.T,
			Title:  fmt.Sprintf("Stopped at %ds: limit %s", st.T, st.Reason),
			Detail: fmt.Sprintf("Load was %s when it failed. The last window where every limit held ended at %s, so that is roughly the most this target sustains.", fmtLoad(r, st.Rate), fmtLoad(r, st.LastGood)),
		})
	}

	// Latency knee: p95 sustained at 2x its early level (and at least +20ms).
	// Seconds after the plan ended (load 0, in-flight work draining) are
	// left out: their latencies say nothing about a given load.
	planned := ss[:0:0]
	for _, s := range ss {
		if s.load > 0 || closedOnly(r) && s.reqs > 0 {
			planned = append(planned, s)
		}
	}
	if len(planned) == 0 {
		planned = ss
	}
	var loaded []second
	for _, s := range planned {
		if s.reqs > 0 {
			loaded = append(loaded, s)
		}
	}
	if len(loaded) >= 6 {
		n := len(loaded) / 5
		if n < 3 {
			n = 3
		}
		var early []int64
		for _, s := range loaded[:n] {
			early = append(early, s.p95)
		}
		base := median(early)
		bar := base * 2
		if bar < base+20_000 {
			bar = base + 20_000
		}
		for i := n; i+2 < len(loaded); i++ {
			a, b, c := loaded[i], loaded[i+1], loaded[i+2]
			if a.p95 > bar && b.p95 > bar && c.p95 > bar {
				add(Insight{Level: "warn", Kind: "latency_knee", T: a.t,
					Title:  fmt.Sprintf("Latency rose from %ds at %s", a.t, fmtLoad(r, a.load)),
					Detail: fmt.Sprintf("p95 went from %s early in the test to %s and stayed there. Load at that point: %s, %.0f req/s.", us(base), us(max(a.p95, b.p95, c.p95)), fmtLoad(r, a.load), float64(a.reqs)),
				})
				break
			}
		}
	}

	// Error onset: error rate above 1% for two seconds in a row.
	for i := 0; i+1 < len(planned); i++ {
		a, b := planned[i], planned[i+1]
		if a.reqs > 0 && b.reqs > 0 && float64(a.errs)/float64(a.reqs) > 0.01 && float64(b.errs)/float64(b.reqs) > 0.01 {
			d := fmt.Sprintf("%.1f%% of requests failed that second", float64(a.errs)/float64(a.reqs)*100)
			if a.topErrKind != "" {
				d += fmt.Sprintf(", mostly %s at %s", a.topErrKind, stepName(a.topKey))
			}
			add(Insight{Level: "critical", Kind: "error_onset", T: a.t,
				Title:  fmt.Sprintf("Errors started at %ds at %s", a.t, fmtLoad(r, a.load)),
				Detail: d + ".",
			})
			break
		}
	}

	// Saturation (open model): arrivals dropped because maxInFlight was full.
	for _, s := range ss {
		if s.dropped > 0 {
			add(Insight{Level: "critical", Kind: "saturation", T: s.t,
				Title:  fmt.Sprintf("Target could not keep up from %ds at %s", s.t, fmtLoad(r, s.load)),
				Detail: "Iterations piled up until maxInFlight was reached and new arrivals were dropped. Responses were slower than the arrival rate allows.",
			})
			break
		}
	}

	// Plateau (closed model): users grew a lot, throughput did not.
	if closedOnly(r) && len(ss) >= 10 {
		q := len(ss) / 4
		lo, hi := ss[q], ss[len(ss)-q-1]
		var rpsLo, rpsHi float64
		for _, s := range ss[q-1 : q+2] {
			rpsLo += float64(s.reqs) / 3
		}
		for _, s := range ss[len(ss)-q-2 : len(ss)-q+1] {
			rpsHi += float64(s.reqs) / 3
		}
		if lo.load > 0 && hi.load >= lo.load*1.5 && rpsLo > 0 && rpsHi < rpsLo*1.1 {
			add(Insight{Level: "warn", Kind: "plateau", T: lo.t,
				Title:  fmt.Sprintf("Throughput stayed near %.0f req/s while users grew %.0f → %.0f", rpsHi, lo.load, hi.load),
				Detail: "More users only made each request wait longer, which means the target was already at its limit.",
			})
		}
	}

	// Spikes: a single second whose p99 jumps far above the test's usual p99.
	// Seconds after the target started to give out are not spikes.
	trouble := troubleStart(out)
	var p99s []int64
	for _, s := range loaded {
		p99s = append(p99s, s.p99)
	}
	if typical := median(p99s); typical > 0 && len(loaded) >= 5 {
		var spikes []string
		first := 0
		for _, s := range loaded {
			if trouble > 0 && s.t >= trouble-1 {
				break
			}
			if s.p99 > typical*5 && s.p99-typical > 50_000 {
				if first == 0 {
					first = s.t
				}
				if len(spikes) < 5 {
					spikes = append(spikes, fmt.Sprintf("%ds (%s)", s.t, us(s.p99)))
				}
			}
		}
		if len(spikes) > 0 {
			add(Insight{Level: "warn", Kind: "spike", T: first,
				Title:  fmt.Sprintf("Short latency spikes: p99 above %s", us(typical*5)),
				Detail: fmt.Sprintf("Usual p99 was %s; at %s it jumped. Look for GC pauses, lock contention, cold caches or a dependency hiccup at those moments.", us(typical), strings.Join(spikes, ", ")),
			})
		}
	}

	if s := r.Summary; s != nil {
		// Where errors come from.
		if s.Errors > 0 {
			var best *metrics.KeySummary
			for _, k := range s.Keys {
				if !metrics.IsInternal(k.Key) && (best == nil || k.Errors > best.Errors) {
					best = k
				}
			}
			if best != nil && float64(best.Errors) >= 0.8*float64(s.Errors) && len(nonInternal(s)) > 1 {
				kind, n := "", int64(0)
				for k, c := range best.ErrKinds {
					if c > n {
						kind, n = k, c
					}
				}
				add(Insight{Level: "warn", Kind: "error_hotspot",
					Title:  fmt.Sprintf("%.0f%% of errors came from %s", float64(best.Errors)/float64(s.Errors)*100, stepName(best.Key)),
					Detail: fmt.Sprintf("Most common: %s (%d times).", kind, n),
				})
			}
		}
		// Which step dominates an iteration.
		if ks := nonInternal(s); len(ks) > 1 {
			var total float64
			for _, k := range ks {
				total += k.Mean * float64(k.Count)
			}
			sort.Slice(ks, func(i, j int) bool { return ks[i].Mean*float64(ks[i].Count) > ks[j].Mean*float64(ks[j].Count) })
			if top := ks[0]; total > 0 {
				share := top.Mean * float64(top.Count) / total
				if share >= 0.4 {
					add(Insight{Level: "info", Kind: "slowest_step",
						Title:  fmt.Sprintf("%s takes %.0f%% of request time", stepName(top.Key), share*100),
						Detail: fmt.Sprintf("p95 %s, p99 %s. Speeding this one up helps the most.", us(top.P95), us(top.P99)),
					})
				}
			}
		}
	}

	out = append(out, serverInsights(r, out)...)
	out = append(out, apmInsights(r)...)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].Level) < rank(out[j].Level) })
	return out
}

func rank(level string) int {
	switch level {
	case "critical":
		return 0
	case "warn":
		return 1
	}
	return 2
}

func closedOnly(r *Result) bool {
	if len(r.Meta.Scenarios) == 0 {
		return false
	}
	for _, s := range r.Meta.Scenarios {
		if s.Executor == "arrival-rate" {
			return false
		}
	}
	return true
}

func nonInternal(s *metrics.Summary) []*metrics.KeySummary {
	var out []*metrics.KeySummary
	for _, k := range s.Keys {
		if !metrics.IsInternal(k.Key) && k.Count > 0 {
			out = append(out, k)
		}
	}
	return out
}

// serverInsights compares server metrics before and after the first
// problem the load caused (knee, errors, saturation, stop).
func serverInsights(r *Result, found []Insight) []Insight {
	t := troubleStart(found)
	units := map[string]string{}
	for _, s := range r.Meta.Server {
		units[s.Name] = s.Unit
	}
	names := map[string]bool{}
	for _, s := range r.Snapshots {
		for n := range s.Server {
			names[n] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	var out []Insight
	keys := make([]string, 0, len(names))
	for n := range names {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		var before, after, all []float64
		for _, s := range r.Snapshots {
			v, ok := s.Server[n]
			if !ok || math.IsNaN(v) {
				continue
			}
			all = append(all, v)
			if t > 0 && s.T < t {
				before = append(before, v)
			} else if t > 0 {
				after = append(after, v)
			}
		}
		if t == 0 || len(before) < 3 || len(after) < 3 {
			continue
		}
		b, a := mean(before), mean(after)
		if b > 0 && a/b >= 1.5 || b == 0 && a > 0 {
			out = append(out, Insight{Level: "info", Kind: "server", T: t,
				Title:  fmt.Sprintf("Server: %s went %s → %s when the trouble started", n, fmtUnit(b, units[n]), fmtUnit(a, units[n])),
				Detail: fmt.Sprintf("Average before %ds vs from %ds on; peak %s.", t, t, fmtUnit(maxOf(all), units[n])),
			})
		}
	}
	return out
}

// troubleStart is the first second where the target gave out, or 0.
func troubleStart(found []Insight) int {
	t := 0
	for _, i := range found {
		switch i.Kind {
		case "latency_knee", "error_onset", "saturation", "stopped":
			if t == 0 || i.T < t {
				t = i.T
			}
		}
	}
	return t
}

func fmtUnit(v float64, unit string) string {
	switch unit {
	case "%":
		return fmt.Sprintf("%.0f%%", v)
	case "bytes":
		u := []string{"B", "KB", "MB", "GB", "TB"}
		i := 0
		for math.Abs(v) >= 1024 && i < len(u)-1 {
			v /= 1024
			i++
		}
		return fmt.Sprintf("%.1f %s", v, u[i])
	case "/s":
		return fmtNum(v) + "/s"
	case "ms":
		return us(int64(v * 1000))
	}
	return fmtNum(v)
}

func mean(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func maxOf(v []float64) float64 {
	m := math.Inf(-1)
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

func fmtNum(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return fmt.Sprintf("%.2fG", v/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.2fM", v/1e6)
	case a >= 1e4:
		return fmt.Sprintf("%.1fk", v/1e3)
	case a >= 100:
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.3g", v)
}

// apmInsights reads the server's own traces: where its time went.
func apmInsights(r *Result) []Insight {
	if r.APM == nil {
		return nil
	}
	var server float64
	var top *OpStat
	var slow *OpStat
	for i := range r.APM.Operations {
		o := &r.APM.Operations[i]
		switch o.Kind {
		case "server":
			server += o.TotalMs
		case "db", "http":
			if top == nil || o.TotalMs > top.TotalMs {
				top = o
			}
			if o.Count >= 10 && (slow == nil || o.P95Ms > slow.P95Ms) {
				slow = o
			}
		}
	}
	var out []Insight
	if top != nil && server > 0 && top.TotalMs/server >= 0.3 {
		what := "SQL"
		if top.Kind == "http" {
			what = "the outgoing call"
		}
		out = append(out, Insight{Level: "info", Kind: "apm_hotspot",
			Title:  fmt.Sprintf("%.0f%% of server time went to %s %s", top.TotalMs/server*100, what, shorten(top.Name, 70)),
			Detail: fmt.Sprintf("%d calls, mean %s, p95 %s (from %s's own traces).", top.Count, us(int64(top.MeanMs*1000)), us(int64(top.P95Ms*1000)), top.Service),
		})
	}
	if slow != nil && slow != top && slow.P95Ms >= 100 {
		out = append(out, Insight{Level: "info", Kind: "apm_slow",
			Title:  fmt.Sprintf("Slowest %s: %s", map[string]string{"db": "query", "http": "outgoing call"}[slow.Kind], shorten(slow.Name, 70)),
			Detail: fmt.Sprintf("p95 %s, max %s over %d calls.", us(int64(slow.P95Ms*1000)), us(int64(slow.MaxMs*1000)), slow.Count),
		})
	}
	return out
}

func shorten(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
