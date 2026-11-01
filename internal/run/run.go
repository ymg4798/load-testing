// Package run ties an engine (local or remote) to its outputs: it folds
// snapshots into a result, checks thresholds and fans snapshots out to
// sinks such as the terminal, the live dashboard and Prometheus.
package run

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ymg4798/lt/internal/engine"
	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/scenario"
)

// FormatVersion is bumped when the result file layout changes.
const FormatVersion = 1

// Result is everything written to a result file.
type Result struct {
	Tool       string                    `json:"tool"`
	Version    int                       `json:"version"`
	Meta       Meta                      `json:"meta"`
	Summary    *metrics.Summary          `json:"summary"`
	Thresholds []metrics.ThresholdResult `json:"thresholds"`
	Passed     bool                      `json:"passed"`
	Warnings   []string                  `json:"warnings,omitempty"`
	Insights   []Insight                 `json:"insights,omitempty"`
	Stopped    *Stop                     `json:"stopped,omitempty"`
	Compare    *Comparison               `json:"compare,omitempty"`
	Alerts     []AlertEvent              `json:"alerts,omitempty"`
	TopSQL     []TopQuery                `json:"topSql,omitempty"`
	APM        *APMReport                `json:"apm,omitempty"`
	Logs       *LogReport                `json:"logs,omitempty"`
	Snapshots  []*metrics.Snapshot       `json:"snapshots,omitempty"`
}

// Meta describes the test that produced a result.
type Meta struct {
	Name      string         `json:"name"`
	File      string         `json:"file"`
	Started   time.Time      `json:"started"`
	Planned   float64        `json:"planned"` // seconds
	Agents    int            `json:"agents"`
	Label     string         `json:"label,omitempty"`
	Scenarios []ScenarioInfo `json:"scenarios"`
	// TraceURL links trace IDs in reports; "{traceId}" is replaced.
	TraceURL string `json:"traceUrl,omitempty"`
	// Server lists the monitored server metrics and their units.
	Server []ServerSeries `json:"server,omitempty"`
}

// ServerSeries describes one monitored server metric.
type ServerSeries struct {
	Name string `json:"name"`
	Unit string `json:"unit,omitempty"` // "%", "bytes", "/s" or ""
}

// ScenarioInfo is a scenario's shape, for reports.
type ScenarioInfo struct {
	Name        string           `json:"name"`
	Executor    string           `json:"executor"`
	Start       float64          `json:"start"`
	Stages      []scenario.Stage `json:"-"`
	StageList   []StageInfo      `json:"stages"`
	MaxInFlight int              `json:"maxInFlight,omitempty"`
}

// StageInfo is a stage in seconds.
type StageInfo struct {
	Duration float64 `json:"duration"`
	Target   float64 `json:"target"`
}

// MetaFor describes a scenario file.
func MetaFor(f *scenario.File, agents int) Meta {
	m := Meta{Name: f.Name, File: filepath.Base(f.Path), Started: time.Now(), Agents: agents}
	if f.Options.Tracing.Enabled {
		m.TraceURL = f.Options.Tracing.URL
	}
	if m.Name == "" {
		m.Name = m.File
	}
	for _, n := range f.ScenarioNames() {
		s := f.Scenarios[n]
		info := ScenarioInfo{Name: n, Executor: s.Executor, MaxInFlight: s.MaxInFlight, Start: s.StartRate}
		if s.Executor != scenario.ArrivalRate {
			info.Start = float64(s.StartUsers)
			info.MaxInFlight = 0
		}
		for _, st := range s.Stages {
			info.StageList = append(info.StageList, StageInfo{Duration: st.Duration.D().Seconds(), Target: st.Target})
		}
		if d := s.TotalDuration().Seconds(); d > m.Planned {
			m.Planned = d
		}
		m.Scenarios = append(m.Scenarios, info)
	}
	return m
}

// Sink receives snapshots as they are produced and the result at the end.
type Sink interface {
	Snapshot(s *metrics.Snapshot, acc *metrics.Accumulator)
	Done(r *Result)
}

// Decorator adds to each snapshot before sinks see it (server metrics).
type Decorator interface {
	Decorate(s *metrics.Snapshot)
}

// Finisher is a sink that adds to the result before findings are computed
// (top SQL from databases, APM traces).
type Finisher interface {
	Finish(r *Result)
}

// ActiveSource lists transactions in flight right now (the engine, the APM collector).
type ActiveSource interface {
	Active(limit int) []metrics.ActiveTx
}

// SeriesLister is a sink that knows the server series it collected (monitor).
type SeriesLister interface {
	Series() []ServerSeries
}

// Stopper is a sink that may end the test early (limits).
type Stopper interface {
	Stopped() *Stop
}

// Collect consumes snapshots until the channel closes and builds the result.
// elapsed, when not nil, gives the measured test length once it is over.
func Collect(meta Meta, snaps <-chan *metrics.Snapshot, ths []*metrics.Threshold, elapsed func() time.Duration, sinks ...Sink) (*Result, error) {
	acc := metrics.NewAccumulator()
	var firstErr error
	for s := range snaps {
		for _, k := range sinks {
			if d, ok := k.(Decorator); ok {
				d.Decorate(s)
			}
		}
		if err := acc.Add(s); err != nil && firstErr == nil {
			firstErr = err
		}
		for _, k := range sinks {
			k.Snapshot(s, acc)
		}
	}
	if elapsed != nil {
		acc.SetDuration(elapsed().Seconds())
	}
	r := &Result{
		Tool: "lt", Version: FormatVersion, Meta: meta,
		Summary:   acc.Summary(),
		Snapshots: acc.Snapshots,
	}
	r.Thresholds = metrics.Evaluate(ths, acc)
	r.Passed = true
	for _, t := range r.Thresholds {
		if !t.Pass {
			r.Passed = false
		}
	}
	for _, k := range sinks {
		if st, ok := k.(Stopper); ok && st.Stopped() != nil {
			r.Stopped = st.Stopped()
		}
		if sl, ok := k.(SeriesLister); ok {
			r.Meta.Server = append(r.Meta.Server, sl.Series()...)
		}
	}
	for _, k := range sinks {
		if f, ok := k.(Finisher); ok {
			f.Finish(r)
		}
	}
	r.Warnings = Warnings(r)
	r.Insights = Insights(r)
	for _, k := range sinks {
		k.Done(r)
	}
	return r, firstErr
}

// Local runs a scenario file on this machine.
func Local(ctx context.Context, f *scenario.File, sinks ...Sink) (*Result, error) {
	return LocalWith(ctx, f, nil, sinks...)
}

// LocalWith is Local that hands the running engine to onStart, for live
// views such as the active-transaction list.
func LocalWith(ctx context.Context, f *scenario.File, onStart func(ActiveSource), sinks ...Sink) (*Result, error) {
	ths, err := metrics.ParseThresholds(f.Thresholds)
	if err != nil {
		return nil, err
	}
	snaps := make(chan *metrics.Snapshot, 16)
	eng, err := engine.New(f, snaps)
	if err != nil {
		return nil, err
	}
	if onStart != nil {
		onStart(eng)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- eng.Run(ctx) }()
	res, err := Collect(MetaFor(f, 1), snaps, ths, eng.Elapsed, sinks...)
	if e := <-runErr; e != nil && err == nil && ctx.Err() == nil {
		err = e
	}
	return res, err
}

// Warnings explains measurements that may not be trustworthy.
func Warnings(r *Result) []string {
	var w []string
	s := r.Summary
	if s.Gen.MaxCPU > 85 {
		w = append(w, fmt.Sprintf("Load generator CPU reached %.0f%%. Latencies may include time spent waiting for the generator itself; use more agents or a lower rate.", s.Gen.MaxCPU))
	}
	if s.Gen.MaxLagP99 > 25_000 {
		w = append(w, fmt.Sprintf("Iterations started up to %s late (p99 of the worst second), so the offered rate was uneven. Usually the generator is busy (CPU) or the OS woke timers late; use more agents or a lower rate.", metrics.FormatMicros(float64(s.Gen.MaxLagP99))))
	}
	if s.Dropped > 0 {
		w = append(w, fmt.Sprintf("%d iterations were dropped because maxInFlight was reached. The target could not keep up with the offered rate; raise maxInFlight to queue more, or treat this as saturation.", s.Dropped))
	}
	kinds := map[string]int64{}
	for _, k := range s.Keys {
		for e, n := range k.ErrKinds {
			kinds[e] += n
		}
	}
	if kinds["no_local_port"] > 0 {
		w = append(w, "Ran out of local ports (EADDRNOTAVAIL). Keep connections alive, widen the ephemeral port range, or spread the load over more agents. See `lt preflight`.")
	}
	if kinds["too_many_files"] > 0 {
		w = append(w, "Hit the open-file limit (EMFILE). Raise `ulimit -n`. See `lt preflight`.")
	}
	if s.Requests == 0 {
		w = append(w, "No requests completed.")
	}
	return w
}

// Save writes a result file.
func Save(path string, r *Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// LoadResult reads a result file, plain or gzipped.
func LoadResult(path string) (*Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if b, err = io.ReadAll(zr); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Tool != "lt" {
		return nil, fmt.Errorf("%s: not an lt result file", path)
	}
	return &r, nil
}

// Comparison is the result compared against a baseline run (lt run --baseline).
type Comparison struct {
	Base        string          `json:"base"` // file or label of the baseline
	BaseStarted time.Time       `json:"baseStarted"`
	Regressed   bool            `json:"regressed"`
	Rows        []ComparisonRow `json:"rows"`
}

// ComparisonRow is one step in both runs. Base or New is nil when the
// step exists in only one of them.
type ComparisonRow struct {
	Name      string              `json:"name"`
	Base      *metrics.KeySummary `json:"base,omitempty"`
	New       *metrics.KeySummary `json:"new,omitempty"`
	Regressed []string            `json:"regressed,omitempty"`
}

// AlertEvent is an alert rule firing or resolving during a run.
type AlertEvent struct {
	T        int       `json:"t"` // second of the test
	At       time.Time `json:"at"`
	Name     string    `json:"name"`
	Severity string    `json:"severity"` // warning, critical
	State    string    `json:"state"`    // firing, resolved
	Value    string    `json:"value"`    // the measured value, formatted
	Rule     string    `json:"rule"`     // the condition as written
}

// TopQuery is one statement from a database's own statistics: the change
// over the test (or, in watch mode, over the recent window).
type TopQuery struct {
	Source  string  `json:"source"` // monitor target name
	Query   string  `json:"query"`  // normalized statement text
	Calls   int64   `json:"calls"`
	TotalMs float64 `json:"totalMs"`
	MeanMs  float64 `json:"meanMs"`
	Rows    int64   `json:"rows,omitempty"`
}

// APMReport is what the APM collector saw from the target's own tracing
// (lt agent or OpenTelemetry) during the run.
type APMReport struct {
	Services   []string `json:"services"`
	Spans      int64    `json:"spans"`
	Operations []OpStat `json:"operations"` // busiest first
	// Traces holds full traces for the requests the report points at
	// (slowest, failed and hitmap samples), capped.
	Traces []Trace `json:"traces,omitempty"`
	// Map is who called whom: services, databases, outside hosts.
	Map *ServiceMap `json:"map,omitempty"`
	// Errors groups failed spans by service and message.
	Errors []ErrorGroup `json:"errors,omitempty"`
}

// ServiceMap is the topology seen in traces. Node IDs are "kind:name",
// e.g. "service:shop", "db:postgres", "http:api.stripe.com", "client:lt".
type ServiceMap struct {
	Nodes []MapNode `json:"nodes"`
	Edges []MapEdge `json:"edges"`
}

// MapNode is one box on the map.
type MapNode struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Kind   string  `json:"kind"` // client, service, db, http
	Count  int64   `json:"count"`
	Errors int64   `json:"errors"`
	MeanMs float64 `json:"meanMs"`
}

// MapEdge is calls from one node to another.
type MapEdge struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Count  int64   `json:"count"`
	Errors int64   `json:"errors"`
	MeanMs float64 `json:"meanMs"`
	P95Ms  float64 `json:"p95Ms"`
}

// ErrorGroup is the same failure seen many times (from spans or logs).
type ErrorGroup struct {
	Service string    `json:"service"`
	Source  string    `json:"source"` // span, log, panic
	Message string    `json:"message"`
	Count   int64     `json:"count"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
	TraceID string    `json:"traceId,omitempty"` // one example
	Stack   string    `json:"stack,omitempty"`
}

// LogLine is one log record, from a tailed file or OTLP logs.
type LogLine struct {
	Time    time.Time         `json:"time"`
	Source  string            `json:"source"` // file name or service
	Level   string            `json:"level"`  // debug, info, warn, error, fatal ("" if unknown)
	Message string            `json:"message"`
	TraceID string            `json:"traceId,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// LogReport is what the run keeps of the logs: counts and the lines that
// matter (errors and lines of the traces the report points at), capped.
type LogReport struct {
	Total   int64          `json:"total"`
	ByLevel map[string]int `json:"byLevel"`
	Lines   []LogLine      `json:"lines"`
}

// LogSource is what live views and alerts ask the log store.
type LogSource interface {
	// Search returns the newest lines matching query (substring or
	// /regex/, plus optional level= and trace= filters), newest first.
	Search(query string, limit int) []LogLine
	// Count counts lines matching query since t.
	Count(query string, since time.Time) int
}

// OpStat is one operation (endpoint, SQL statement, outgoing call) summed over the run.
type OpStat struct {
	Service string  `json:"service"`
	Kind    string  `json:"kind"` // server, db, http, internal
	Name    string  `json:"name"`
	Count   int64   `json:"count"`
	Errors  int64   `json:"errors"`
	TotalMs float64 `json:"totalMs"`
	MeanMs  float64 `json:"meanMs"`
	P95Ms   float64 `json:"p95Ms"`
	MaxMs   float64 `json:"maxMs"`
}

// Trace is one distributed trace.
type Trace struct {
	TraceID string `json:"traceId"`
	Spans   []Span `json:"spans"`
}

// Span is one timed operation inside a trace. StartUs is relative to the
// earliest span of the trace.
type Span struct {
	SpanID   string            `json:"spanId"`
	ParentID string            `json:"parentId,omitempty"`
	Service  string            `json:"service"`
	Name     string            `json:"name"`
	Kind     string            `json:"kind"` // server, db, http, internal
	StartUs  int64             `json:"startUs"`
	DurUs    int64             `json:"durUs"`
	Error    bool              `json:"error,omitempty"`
	Attrs    map[string]string `json:"attrs,omitempty"`
}
