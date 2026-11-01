package run

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/scenario"
)

// Alerter evaluates alert rules every second and reports when one starts
// or stops holding. Unlike limits it never stops the test.
type Alerter struct {
	rules  []*alertRule
	notify func(AlertEvent, []string)
	logs   LogSource // for log{...} rules

	mu     sync.Mutex
	events []AlertEvent
}

type alertRule struct {
	spec   scenario.AlertRule
	window int
	hold   int // seconds the condition must hold

	load   *metrics.Threshold // load-test metric rule
	server *serverCond        // server{name} rule
	log    *logCond           // log{query} rule

	since  int // second the condition started holding (0 = not)
	firing bool
}

type logCond struct {
	query string
	op    string
	value float64
}

type serverCond struct {
	name  string
	op    string
	value float64
}

var logRe = regexp.MustCompile(`^log\{([^}]*)\}\s*(<=|>=|==|!=|<|>)\s*([0-9.]+)$`)

var serverRe = regexp.MustCompile(`^server\{([^}]+)\}\s*(<=|>=|==|!=|<|>)\s*([0-9.]+)\s*([A-Za-z%/]*)$`)

// NewAlerter parses rules; notify (may be nil) is called for each event
// with the rule's own webhooks.
func NewAlerter(rules []scenario.AlertRule, notify func(AlertEvent, []string)) (*Alerter, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	a := &Alerter{notify: notify}
	for i, sp := range rules {
		r := &alertRule{spec: sp, window: int(sp.Window.D().Seconds()), hold: int(sp.For.D().Seconds())}
		if r.window <= 0 {
			r.window = 10
		}
		if r.spec.Name == "" {
			r.spec.Name = sp.When
		}
		when := strings.TrimSpace(sp.When)
		if m := logRe.FindStringSubmatch(when); m != nil {
			v, err := strconv.ParseFloat(m[3], 64)
			if err != nil {
				return nil, fmt.Errorf("alert %d: %q: %w", i+1, when, err)
			}
			r.log = &logCond{query: strings.TrimSpace(m[1]), op: m[2], value: v}
		} else if m := serverRe.FindStringSubmatch(when); m != nil {
			v, err := strconv.ParseFloat(m[3], 64)
			if err != nil {
				return nil, fmt.Errorf("alert %d: %q: %w", i+1, when, err)
			}
			switch strings.ToUpper(m[4]) {
			case "KB":
				v *= 1 << 10
			case "MB":
				v *= 1 << 20
			case "GB":
				v *= 1 << 30
			}
			r.server = &serverCond{name: strings.TrimSpace(m[1]), op: m[2], value: v}
		} else {
			metric, expr, ok := strings.Cut(when, " ")
			if !ok {
				return nil, fmt.Errorf("alert %d: %q: want \"<metric> <condition>\", e.g. \"latency p95 > 500ms\"", i+1, when)
			}
			ts, err := metrics.ParseThresholds(map[string][]string{metric: {strings.TrimSpace(expr)}})
			if err != nil {
				return nil, fmt.Errorf("alert %d: %w", i+1, err)
			}
			r.load = ts[0]
		}
		a.rules = append(a.rules, r)
	}
	return a, nil
}

// SetLogs gives log{...} rules something to count.
func (a *Alerter) SetLogs(l LogSource) {
	a.mu.Lock()
	a.logs = l
	a.mu.Unlock()
}

// Snapshot implements Sink.
func (a *Alerter) Snapshot(s *metrics.Snapshot, acc *metrics.Accumulator) {
	a.Observe(s.T, acc.Snapshots)
}

// Observe evaluates every rule against the latest snapshots; t is the
// current second. Watch mode calls it directly.
func (a *Alerter) Observe(t int, snaps []*metrics.Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.rules {
		var holds bool
		var shown string
		if r.log != nil {
			if a.logs == nil {
				continue
			}
			n := float64(a.logs.Count(r.log.query, time.Now().Add(-time.Duration(r.window)*time.Second)))
			holds, shown = cmp(n, r.log.op, r.log.value), strconv.FormatFloat(n, 'f', 0, 64)+" lines"
		} else {
			holds, shown = r.eval(snaps)
		}
		switch {
		case holds && r.since == 0:
			r.since = t
		case !holds:
			r.since = 0
		}
		if holds && !r.firing && t-r.since+1 >= max(r.hold, 1) {
			r.firing = true
			a.emit(r, t, "firing", shown)
		} else if !holds && r.firing {
			if shown == "" {
				shown = "no data"
			}
			r.firing = false
			a.emit(r, t, "resolved", shown)
		}
	}
}

func (a *Alerter) emit(r *alertRule, t int, state, value string) {
	ev := AlertEvent{T: t, At: time.Now(), Name: r.spec.Name, Severity: r.spec.Severity, State: state, Value: value, Rule: r.spec.When}
	a.events = append(a.events, ev)
	if a.notify != nil {
		go a.notify(ev, r.spec.Notify)
	}
}

// eval says whether the condition holds over the rule's window and the
// value it saw ("" when there was nothing to judge).
func (r *alertRule) eval(snaps []*metrics.Snapshot) (bool, string) {
	if len(snaps) == 0 {
		return false, ""
	}
	win := snaps[max(0, len(snaps)-r.window):]
	if r.server != nil {
		var sum float64
		n := 0
		for _, s := range win {
			if v, ok := s.Server[r.server.name]; ok && !math.IsNaN(v) {
				sum += v
				n++
			}
		}
		if n == 0 {
			return false, ""
		}
		v := sum / float64(n)
		return cmp(v, r.server.op, r.server.value), strconv.FormatFloat(v, 'g', 4, 64)
	}
	acc := metrics.NewAccumulator()
	for _, s := range win {
		if acc.Add(s) != nil {
			return false, ""
		}
	}
	res := metrics.Evaluate([]*metrics.Threshold{r.load}, acc)[0]
	if math.IsNaN(res.Value) {
		return false, ""
	}
	return res.Pass, res.Actual
}

func cmp(a float64, op string, b float64) bool {
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

// Done implements Sink.
func (a *Alerter) Done(*Result) {}

// Finish implements Finisher: the result keeps every event.
func (a *Alerter) Finish(r *Result) { r.Alerts = a.Events() }

// Events returns the events so far.
func (a *Alerter) Events() []AlertEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AlertEvent(nil), a.events...)
}

// Firing lists the rules firing now.
func (a *Alerter) Firing() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.rules {
		if r.firing {
			out = append(out, r.spec.Name)
		}
	}
	return out
}
