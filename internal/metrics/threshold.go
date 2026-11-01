package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Threshold is one pass/fail rule, e.g. metric "latency{step:login}", expr "p95 < 300ms".
type Threshold struct {
	Metric  string            `json:"metric"`
	Expr    string            `json:"expr"`
	base    string            // latency, iteration, errors, dropped, rps
	filter  map[string]string // scenario / step
	stat    string
	op      string
	value   float64
	unitDur bool
}

// ThresholdResult is a Threshold checked against a finished test.
type ThresholdResult struct {
	Metric string  `json:"metric"`
	Expr   string  `json:"expr"`
	Actual string  `json:"actual"`
	Value  float64 `json:"value"`
	Pass   bool    `json:"pass"`
}

var ops = []string{"<=", ">=", "==", "!=", "<", ">"}

// ParseThresholds parses the thresholds section of a scenario file.
func ParseThresholds(m map[string][]string) ([]*Threshold, error) {
	var names []string
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []*Threshold
	for _, name := range names {
		for _, expr := range m[name] {
			t, err := parseThreshold(name, expr)
			if err != nil {
				return nil, fmt.Errorf("threshold %q: %q: %w", name, expr, err)
			}
			out = append(out, t)
		}
	}
	return out, nil
}

func parseThreshold(metric, expr string) (*Threshold, error) {
	t := &Threshold{Metric: metric, Expr: expr, filter: map[string]string{}}
	base := metric
	if i := strings.IndexByte(metric, '{'); i >= 0 {
		if !strings.HasSuffix(metric, "}") {
			return nil, fmt.Errorf("missing }")
		}
		base = metric[:i]
		for _, kv := range strings.Split(metric[i+1:len(metric)-1], ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), ":")
			if !ok || (k != "step" && k != "scenario") {
				return nil, fmt.Errorf("filter must be step:<name> or scenario:<name>, got %q", kv)
			}
			t.filter[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	t.base = strings.TrimSpace(base)

	e := strings.TrimSpace(expr)
	var lhs, rhs string
	for _, op := range ops {
		if i := strings.Index(e, op); i >= 0 {
			lhs, rhs, t.op = strings.TrimSpace(e[:i]), strings.TrimSpace(e[i+len(op):]), op
			break
		}
	}
	if t.op == "" {
		return nil, fmt.Errorf("no comparison (<, <=, >, >=, ==, !=)")
	}
	t.stat = lhs
	switch t.base {
	case "latency", "iteration":
		if t.stat == "" {
			return nil, fmt.Errorf("say which statistic, e.g. p95 < 300ms")
		}
		if _, ok := quantileOf(t.stat); !ok {
			return nil, fmt.Errorf("unknown statistic %q (use min, avg, med, max, p90, p95, p99, p99.9 ...)", t.stat)
		}
		d, err := parseDurationValue(rhs)
		if err != nil {
			return nil, err
		}
		t.value, t.unitDur = float64(d.Microseconds()), true
	case "errors", "dropped":
		if t.stat == "" {
			t.stat = "rate"
		}
		if t.stat != "rate" && t.stat != "count" {
			return nil, fmt.Errorf("%s supports rate or count", t.base)
		}
		v, err := parseNumber(rhs)
		if err != nil {
			return nil, err
		}
		t.value = v
	case "rps":
		v, err := parseNumber(rhs)
		if err != nil {
			return nil, err
		}
		t.value = v
	default:
		return nil, fmt.Errorf("unknown metric %q (use latency, iteration, errors, dropped, rps)", t.base)
	}
	return t, nil
}

// quantileOf maps a statistic name to a percentile (or a negative code for min/avg/max).
func quantileOf(stat string) (float64, bool) {
	switch stat {
	case "min":
		return -1, true
	case "avg", "mean":
		return -2, true
	case "max":
		return -3, true
	case "med":
		return 50, true
	}
	if strings.HasPrefix(stat, "p") {
		q, err := strconv.ParseFloat(stat[1:], 64)
		if err == nil && q > 0 && q <= 100 {
			return q, true
		}
	}
	return 0, false
}

func parseDurationValue(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(v * float64(time.Millisecond)), nil // bare numbers are milliseconds
	}
	return 0, fmt.Errorf("bad duration %q", s)
}

func parseNumber(s string) (float64, error) {
	if strings.HasSuffix(s, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		return v / 100, err
	}
	return strconv.ParseFloat(s, 64)
}

func (t *Threshold) matches(key string) bool {
	sc, st := SplitKey(key)
	if t.base == "iteration" {
		if st != IterationStep {
			return false
		}
	} else if IsInternal(key) {
		return false
	}
	if v, ok := t.filter["scenario"]; ok && v != sc {
		return false
	}
	if v, ok := t.filter["step"]; ok && v != st {
		return false
	}
	return true
}

// Evaluate checks every threshold against the accumulated test.
func Evaluate(ts []*Threshold, acc *Accumulator) []ThresholdResult {
	var out []ThresholdResult
	sum := acc.Summary()
	for _, t := range ts {
		var actual float64
		var shown string
		switch t.base {
		case "latency", "iteration":
			h, _, _ := acc.HistFor(t.matches)
			q, _ := quantileOf(t.stat)
			switch q {
			case -1:
				actual = float64(h.Min())
			case -2:
				actual = h.Mean()
			case -3:
				actual = float64(h.Max())
			default:
				actual = float64(h.ValueAtQuantile(q))
			}
			if h.TotalCount() == 0 {
				actual = math.NaN()
			}
			shown = FormatMicros(actual)
		case "errors":
			_, count, errs := acc.HistFor(t.matches)
			if t.stat == "count" {
				actual, shown = float64(errs), strconv.FormatInt(errs, 10)
			} else {
				if count > 0 {
					actual = float64(errs) / float64(count)
				}
				shown = fmt.Sprintf("%.2f%%", actual*100)
			}
		case "dropped":
			if t.stat == "count" {
				actual, shown = float64(sum.Dropped), strconv.FormatInt(sum.Dropped, 10)
			} else {
				if n := sum.Dropped + sum.Iterations; n > 0 {
					actual = float64(sum.Dropped) / float64(n)
				}
				shown = fmt.Sprintf("%.2f%%", actual*100)
			}
		case "rps":
			_, count, _ := acc.HistFor(t.matches)
			if d := acc.Duration(); d > 0 {
				actual = float64(count) / d
			}
			shown = fmt.Sprintf("%.1f/s", actual)
		}
		out = append(out, ThresholdResult{
			Metric: t.Metric, Expr: t.Expr, Actual: shown, Value: actual, Pass: compare(actual, t.op, t.value),
		})
	}
	return out
}

func compare(a float64, op string, b float64) bool {
	if math.IsNaN(a) {
		return false // nothing measured: never silently pass
	}
	switch op {
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	case "==":
		return a == b
	case "!=":
		return a != b
	}
	return false
}

// FormatMicros prints a microsecond value the way people read latencies.
func FormatMicros(us float64) string {
	switch {
	case math.IsNaN(us):
		return "n/a"
	case us < 1000:
		return fmt.Sprintf("%.0fµs", us)
	case us < 1_000_000:
		return fmt.Sprintf("%.2fms", us/1000)
	default:
		return fmt.Sprintf("%.2fs", us/1_000_000)
	}
}
