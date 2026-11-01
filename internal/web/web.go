// Package web renders results as HTML: a self-contained report file and a
// live dashboard served while a test runs. Both use the same page and
// scripts; the report embeds its data, the dashboard streams it.
package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"io"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/run"
)

var (
	//go:embed assets/uPlot.iife.min.js
	uplotJS string
	//go:embed assets/uPlot.min.css
	uplotCSS string
	//go:embed assets/app.js
	appJS string
	//go:embed assets/app.css
	appCSS string
	//go:embed assets/page.html
	pageHTML string
)

var page = template.Must(template.New("page").Parse(pageHTML))

// KeyPoint is one key's numbers for one second, in microseconds.
type KeyPoint struct {
	Count  int64 `json:"n"`
	Errors int64 `json:"e"`
	P50    int64 `json:"p50"`
	P95    int64 `json:"p95"`
	P99    int64 `json:"p99"`
	Max    int64 `json:"max"`
}

// Point is one second of the test, trimmed for charts.
type Point struct {
	T        int                 `json:"t"`
	Seconds  int                 `json:"sec,omitempty"` // history points: seconds merged into this one
	Target   float64             `json:"target"`
	Done     int64               `json:"done"`
	Iters    int64               `json:"iters"`
	Errors   int64               `json:"errors"`
	Dropped  int64               `json:"dropped"`
	InFlight int64               `json:"inFlight"`
	Users    int64               `json:"users"`
	CPU      float64             `json:"cpu"`
	LagP99   int64               `json:"lagP99"`
	P50      int64               `json:"p50"`
	P95      int64               `json:"p95"`
	P99      int64               `json:"p99"`
	Max      int64               `json:"max"`
	Keys     map[string]KeyPoint `json:"keys"`
	Server   map[string]float64  `json:"server,omitempty"`
	Hits     *metrics.Hits       `json:"hits,omitempty"`
	APMHits  *metrics.Hits       `json:"apmHits,omitempty"`
	Active   []int64             `json:"active,omitempty"`
}

// PointOf trims a snapshot.
func PointOf(s *metrics.Snapshot) Point {
	n, e, st := s.Overall()
	p := Point{
		T: s.T, Target: s.TargetRate, Done: n, Errors: e, Dropped: s.Dropped,
		InFlight: s.InFlight, Users: s.Users, CPU: s.Gen.CPU, LagP99: s.Gen.LagP99,
		P50: st.P50, P95: st.P95, P99: st.P99, Max: st.Max,
		Keys: make(map[string]KeyPoint, len(s.Keys)), Server: s.Server,
		Hits: s.Hits, APMHits: s.APMHits, Active: s.Active,
	}
	for k, ks := range s.Keys {
		if _, st := metrics.SplitKey(k); st == metrics.IterationStep {
			p.Iters += ks.Count
		}
		p.Keys[k] = KeyPoint{Count: ks.Count, Errors: ks.Errors, P50: ks.P50, P95: ks.P95, P99: ks.P99, Max: ks.Max}
	}
	return p
}

// View is the data the page renders.
type View struct {
	Meta       run.Meta                  `json:"meta"`
	Summary    *metrics.Summary          `json:"summary"`
	Thresholds []metrics.ThresholdResult `json:"thresholds"`
	Passed     bool                      `json:"passed"`
	Warnings   []string                  `json:"warnings"`
	Insights   []run.Insight             `json:"insights"`
	Stopped    *run.Stop                 `json:"stopped"`
	Compare    *run.Comparison           `json:"compare"`
	Alerts     []run.AlertEvent          `json:"alerts"`
	TopSQL     []run.TopQuery            `json:"topSql"`
	APM        *run.APMReport            `json:"apm"`
	Logs       *run.LogReport            `json:"logs"`
	Points     []Point                   `json:"points"`
	Live       bool                      `json:"live"`
	Done       bool                      `json:"done"`
	// Watch is the always-on monitoring console (lt watch): no load test.
	Watch bool `json:"watch,omitempty"`
	// History says stored history can be browsed (/api/range).
	History bool `json:"history,omitempty"`
}

// ViewOf builds the page data for a finished result.
func ViewOf(r *run.Result) *View {
	v := &View{Meta: r.Meta, Summary: r.Summary, Thresholds: r.Thresholds, Passed: r.Passed, Warnings: r.Warnings,
		Insights: r.Insights, Stopped: r.Stopped, Compare: r.Compare, Alerts: r.Alerts, TopSQL: r.TopSQL, APM: r.APM, Logs: r.Logs, Done: true}
	for _, s := range r.Snapshots {
		v.Points = append(v.Points, PointOf(s))
	}
	return v
}

type pageData struct {
	Title    string
	UplotCSS template.CSS
	AppCSS   template.CSS
	UplotJS  template.JS
	AppJS    template.JS
	Data     template.JS
}

func marshal(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(true) // keeps "</script>" out of the inline data
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func render(w io.Writer, title string, v *View) error {
	data, err := marshal(v)
	if err != nil {
		return err
	}
	return page.Execute(w, pageData{
		Title:    title,
		UplotCSS: template.CSS(uplotCSS),
		AppCSS:   template.CSS(appCSS),
		UplotJS:  template.JS(uplotJS),
		AppJS:    template.JS(appJS),
		Data:     template.JS(data),
	})
}

// WriteReport writes a self-contained HTML report.
func WriteReport(w io.Writer, r *run.Result) error {
	return render(w, r.Meta.Name+" · lt report", ViewOf(r))
}
