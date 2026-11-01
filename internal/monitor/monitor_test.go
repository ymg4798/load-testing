package monitor

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/scenario"
)

const text = `# HELP http_requests_total Requests.
# TYPE http_requests_total counter
http_requests_total{code="200",path="/a"} 10
http_requests_total{code="500",path="/a"} 2
http_requests_total{code="200",path="/b, \"c\""} 5
go_goroutines 42
temp_celsius{room="x"} NaN
weird_total 1e3 1700000000000
`

func TestParseText(t *testing.T) {
	ss, types, err := parseText(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if types["http_requests_total"] != "counter" || len(ss) != 6 {
		t.Fatalf("types %v, %d samples", types, len(ss))
	}
	if ss[2].labels["path"] != `/b, "c"` || ss[5].value != 1000 || !math.IsNaN(ss[4].value) {
		t.Fatalf("parsed %+v", ss)
	}
	sum := func(expr string) float64 {
		sel, err := parseSelector(expr)
		if err != nil {
			t.Fatal(err)
		}
		var v float64
		for _, s := range ss {
			if sel.match(s) {
				v += s.value
			}
		}
		return v
	}
	for expr, want := range map[string]float64{
		"http_requests_total":                        17,
		`http_requests_total{code="500"}`:            2,
		`http_requests_total{code!="500"}`:           15,
		`http_requests_total{path=~"/b.*"}`:          5,
		`http_requests_total{code=~"2..",path="/a"}`: 10,
		"go_goroutines":                              42,
	} {
		if got := sum(expr); got != want {
			t.Errorf("%s = %v, want %v", expr, got, want)
		}
	}
	if _, err := parseSelector(`x{a=b}`); err == nil {
		t.Error("unquoted value should fail")
	}
}

func TestScrapeRatesAndGauges(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Key") != "k" {
			w.WriteHeader(401)
			return
		}
		c := n.Add(100)
		fmt.Fprintf(w, "process_cpu_seconds_total %g\nreqs_total %d\ngo_goroutines 7\nprocess_resident_memory_bytes 1048576\n", float64(c)/1000, c)
	}))
	defer srv.Close()
	m, err := New([]scenario.MonitorTarget{{
		URL:     "{{ base }}/metrics",
		Headers: map[string]string{"X-Key": "{{ key }}"},
		Metrics: []string{"process_cpu_seconds_total", "reqs_total", "go_goroutines", "process_resident_memory_bytes"},
	}}, map[string]any{"base": srv.URL, "key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	time.Sleep(2300 * time.Millisecond)
	m.Close()
	s := &metrics.Snapshot{}
	m.Decorate(s)
	if s.Server["goroutines"] != 7 || s.Server["memory"] != 1048576 {
		t.Fatalf("gauges: %v", s.Server)
	}
	// 100 per second → reqs 100/s, cpu 0.1 s/s = 10%.
	if r := s.Server["reqs_total"]; r < 70 || r > 130 {
		t.Fatalf("rate: %v", s.Server)
	}
	if c := s.Server["cpu"]; c < 7 || c > 13 {
		t.Fatalf("cpu: %v", s.Server)
	}
	units := map[string]string{}
	for _, x := range m.Series() {
		units[x.Name] = x.Unit
	}
	if units["cpu"] != "%" || units["memory"] != "bytes" || units["reqs_total"] != "/s" {
		t.Fatalf("units %v", units)
	}
}

func TestParseCPUTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:01.50":    1500 * time.Millisecond,
		"01:02:03":   time.Hour + 2*time.Minute + 3*time.Second,
		"2-00:00:01": 48*time.Hour + time.Second,
		"12:34.00":   12*time.Minute + 34*time.Second,
	} {
		got, err := parseCPUTime(in)
		if err != nil || got != want {
			t.Errorf("%s = %v %v, want %v", in, got, err, want)
		}
	}
}
