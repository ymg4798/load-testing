package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/run"
)

func TestMarkdown(t *testing.T) {
	k := &metrics.KeySummary{Key: "s|lo|gin", Scenario: "s", Step: "lo|gin", Count: 10, Stats: metrics.Stats{P95: 1500},
		Slowest: []metrics.Exemplar{{T: 3, Latency: 9000, TraceID: "abc"}}}
	r := &run.Result{
		Meta:       run.Meta{Name: "demo", TraceURL: "http://j/{traceId}", Scenarios: []run.ScenarioInfo{{Name: "s"}}},
		Summary:    &metrics.Summary{Requests: 10, Keys: []*metrics.KeySummary{k}},
		Thresholds: []metrics.ThresholdResult{{Metric: "latency", Expr: "p95 < 1ms", Actual: "1.50ms"}},
		Insights:   []run.Insight{{Level: "warn", Title: "Latency rose"}},
		Compare: &run.Comparison{Base: "last: x.json.gz", Regressed: true,
			Rows: []run.ComparisonRow{{Name: "lo|gin", Base: k, New: k, Regressed: []string{"p95"}}}},
	}
	var b bytes.Buffer
	WriteMarkdown(&b, r)
	md := b.String()
	for _, want := range []string{"❌ lt: demo — FAIL", `lo\|gin`, "❌ `latency` p95 < 1ms", "🟠 **Latency rose**",
		"Compared with baseline", "[`abc`](http://j/abc)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	r.Thresholds[0].Pass, r.Passed = true, true
	if Verdict(r) != "REGRESSED" {
		t.Errorf("verdict %s", Verdict(r))
	}
	if !strings.Contains(OneLine(r), "regressed vs baseline: lo|gin [p95]") {
		t.Errorf("one line: %s", OneLine(r))
	}
}
