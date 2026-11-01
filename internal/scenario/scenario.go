// Package scenario loads and validates scenario files.
package scenario

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ymg4798/load-testing/internal/protocol"
	"github.com/ymg4798/load-testing/internal/yamlx"
)

// File is a whole scenario file.
type File struct {
	Name       string               `yaml:"name"`
	Vars       map[string]any       `yaml:"vars"`
	Data       map[string]*DataSpec `yaml:"data"`
	Setup      string               `yaml:"setup"`
	Options    Options              `yaml:"options"`
	Scenarios  map[string]*Scenario `yaml:"scenarios"`
	Thresholds map[string][]string  `yaml:"thresholds"`
	// Limits use the threshold syntax, judged over the last few seconds
	// (options.limitWindow) while the test runs; the test stops at the
	// first one that fails. Use them to find a breaking point.
	Limits map[string][]string `yaml:"limits"`
	// Monitor collects the target's own metrics every second during the
	// test, so reports show server and load side by side.
	Monitor []MonitorTarget `yaml:"monitor"`
	// APM listens for traces from the target (lt agent or OpenTelemetry).
	APM APM `yaml:"apm"`
	// Logs are followed during the run (and in watch mode) and linked to
	// requests by trace ID.
	Logs []LogSpec `yaml:"logs"`
	// Alerts notify while the test runs, without stopping it.
	Alerts []AlertRule `yaml:"alerts"`

	// Dir is the folder the file was loaded from; relative paths resolve against it.
	Dir string `yaml:"-"`
	// Path is the file's own path.
	Path string `yaml:"-"`
}

// Options apply to every scenario in the file.
type Options struct {
	Timeout         yamlx.Duration `yaml:"timeout"`         // per request, default 30s
	Insecure        bool           `yaml:"insecure"`        // skip TLS verification
	HTTP2           *bool          `yaml:"http2"`           // default true
	MaxConnsPerHost int            `yaml:"maxConnsPerHost"` // 0 = no limit
	NoKeepAlive     bool           `yaml:"noKeepAlive"`
	Cookies         bool           `yaml:"cookies"`      // keep a cookie jar per VU
	GracefulStop    yamlx.Duration `yaml:"gracefulStop"` // wait for in-flight iterations, default 30s
	// CorrectLag adds each arrival's scheduling lag to its first request's
	// latency, measuring from when the request was due rather than sent
	// (wrk2-style). Off by default: in the open model the target cannot
	// delay sends, so lag comes from the generator and is reported apart.
	CorrectLag bool `yaml:"correctLag"`
	// LimitWindow is how many recent seconds limits are judged over (default 5s).
	LimitWindow yamlx.Duration `yaml:"limitWindow"`
	// Tracing sends a trace context with every HTTP and gRPC request.
	Tracing Tracing `yaml:"tracing"`
}

// Tracing is `tracing: true` or a map with the details.
type Tracing struct {
	Enabled    bool   `yaml:"enabled"`
	Propagator string `yaml:"propagator"` // w3c (default), b3, jaeger
	Sampled    *bool  `yaml:"sampled"`    // default true
	// URL links trace IDs in reports, e.g. http://localhost:16686/trace/{traceId}
	URL string `yaml:"url"`
}

// UnmarshalYAML accepts a bool or a map. A map turns tracing on unless it says enabled: false.
func (t *Tracing) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&t.Enabled)
	}
	type plain Tracing
	p := plain{Enabled: true}
	if err := n.Decode(&p); err != nil {
		return err
	}
	*t = Tracing(p)
	return nil
}

// Protocol converts the file setting for protocol plugins.
func (t Tracing) Protocol() protocol.Tracing {
	return protocol.Tracing{Enabled: t.Enabled, Propagator: t.Propagator, Sampled: t.Sampled == nil || *t.Sampled}
}

// MonitorTarget is one source of server metrics: a Prometheus endpoint
// or a local process.
type MonitorTarget struct {
	Name    string            `yaml:"name"`    // prefix for its series; default from the URL host or process
	URL     string            `yaml:"url"`     // Prometheus text format, e.g. http://localhost:8080/metrics
	Preset  string            `yaml:"preset"`  // with url: node, mysql, postgres, redis (the usual exporters)
	Metrics []string          `yaml:"metrics"` // e.g. go_goroutines, http_requests_total{code="500"}
	Headers map[string]string `yaml:"headers"`
	Process string            `yaml:"process"` // local process name or pid: CPU % and memory
	// Direct database connections: health metrics every second and the
	// busiest statements of the test (Top SQL).
	Postgres string `yaml:"postgres"` // postgres://user:pass@host:5432/db
	MySQL    string `yaml:"mysql"`    // user:pass@tcp(host:3306)/db
	Redis    string `yaml:"redis"`    // redis://[:pass@]host:6379[/db]
	// Docker engine: per-container CPU, memory, network and disk. Value is
	// the endpoint ("unix:///var/run/docker.sock", "tcp://host:2375") or "local".
	Docker string `yaml:"docker"`
	// Containers limits docker monitoring to names matching these globs.
	Containers []string `yaml:"containers"`
}

// LogSpec is one log source: a file to follow. OTLP logs arrive on the
// apm listener (/v1/logs) without configuration.
type LogSpec struct {
	Name   string `yaml:"name"`   // shown as the source; default the file name
	File   string `yaml:"file"`   // path or glob; followed like tail -F
	Format string `yaml:"format"` // auto (default), json, text
}

// AlertRule notifies when a condition holds for a while.
//
//   - name: slow checkout
//     when: "latency{step:checkout} p95 > 500ms"   # or "errors rate > 1%", "server{cpu} > 90"
//     for: 30s          # how long it must hold (default 0: at once)
//     window: 10s       # recent seconds the value is computed over (default 10s)
//     severity: critical
type AlertRule struct {
	Name     string         `yaml:"name"`
	When     string         `yaml:"when"`
	For      yamlx.Duration `yaml:"for"`
	Window   yamlx.Duration `yaml:"window"`
	Severity string         `yaml:"severity"`
	Notify   []string       `yaml:"notify"` // webhooks; default: the run's --notify
}

// APM receives the target's own traces while the test runs.
type APM struct {
	Listen string `yaml:"listen"` // OTLP/HTTP and lt agent endpoint, e.g. :4318
}

// Executors.
const (
	ArrivalRate   = "arrival-rate"
	ConstantUsers = "constant-users"
	RampingUsers  = "ramping-users"
	Iterations    = "iterations"
)

// Scenario is one workload with its own executor and flow.
type Scenario struct {
	Executor string `yaml:"executor"`

	// arrival-rate: iterations per second. Either rate+duration or stages.
	Rate      float64 `yaml:"rate"`
	StartRate float64 `yaml:"startRate"`
	// constant-users / ramping-users: concurrent users.
	Users      int `yaml:"users"`
	StartUsers int `yaml:"startUsers"`
	// iterations: run the flow this many times in total, shared by users
	// (default 1), giving up after duration (default 10m).
	Iterations int `yaml:"iterations"`

	Duration yamlx.Duration `yaml:"duration"`
	Stages   []Stage        `yaml:"stages"`

	// MaxInFlight caps concurrent iterations for arrival-rate. When it is
	// reached, new arrivals are dropped and counted, never delayed.
	MaxInFlight int            `yaml:"maxInFlight"`
	StartAfter  yamlx.Duration `yaml:"startAfter"`
	OnError     string         `yaml:"onError"` // abort (default) or continue

	Flow []map[string]any `yaml:"flow"`
}

// Stage moves the target (rate or users) linearly to Target over Duration.
type Stage struct {
	Duration yamlx.Duration `yaml:"duration"`
	Target   float64        `yaml:"target"`
}

// Load reads and validates a scenario file. overrides replace entries of vars.
func Load(path string, overrides map[string]string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)
	return Parse(b, filepath.Dir(abs), abs, overrides)
}

// LoadWatch reads a file for lt watch: monitor, apm and alerts, with no
// scenarios required.
func LoadWatch(path string, overrides map[string]string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(path)
	f, err := decode(b, filepath.Dir(abs), abs, overrides)
	if err != nil {
		return nil, err
	}
	if len(f.Monitor) == 0 && f.APM.Listen == "" && len(f.Logs) == 0 {
		return nil, fmt.Errorf("%s: nothing to watch: add monitor targets or apm.listen", filepath.Base(path))
	}
	if err := f.validateWatch(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return f, nil
}

func decode(b []byte, dir, path string, overrides map[string]string) (*File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	f.Dir, f.Path = dir, path
	if f.Vars == nil {
		f.Vars = map[string]any{}
	}
	for k, v := range overrides {
		f.Vars[k] = v
	}
	return &f, nil
}

// Parse is Load for bytes already in memory.
func Parse(b []byte, dir, path string, overrides map[string]string) (*File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	f.Dir, f.Path = dir, path
	if f.Vars == nil {
		f.Vars = map[string]any{}
	}
	for k, v := range overrides {
		f.Vars[k] = v
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return &f, nil
}

// ScenarioNames returns scenario names in a stable order.
func (f *File) ScenarioNames() []string {
	var out []string
	for k := range f.Scenarios {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Validate checks the file and fills defaults.
func (f *File) Validate() error {
	if len(f.Scenarios) == 0 {
		return fmt.Errorf("no scenarios")
	}
	if err := protocol.ValidPropagator(f.Options.Tracing.Propagator); err != nil {
		return err
	}
	if err := f.validateWatch(); err != nil {
		return err
	}
	return f.validateRun()
}

// validateWatch checks the parts shared with lt watch.
func (f *File) validateWatch() error {
	for i, m := range f.Monitor {
		n := 0
		for _, v := range []string{m.URL, m.Process, m.Postgres, m.MySQL, m.Redis, m.Docker} {
			if v != "" {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("monitor %d: set exactly one of url, process, postgres, mysql, redis, docker", i+1)
		}
		if m.Preset != "" && m.URL == "" {
			return fmt.Errorf("monitor %d: preset needs url (the exporter's /metrics)", i+1)
		}
	}
	for i, l := range f.Logs {
		if l.File == "" {
			return fmt.Errorf("logs %d: file is required", i+1)
		}
		switch l.Format {
		case "", "auto", "json", "text":
		default:
			return fmt.Errorf("logs %d: format must be auto, json or text", i+1)
		}
	}
	for i, a := range f.Alerts {
		if a.When == "" {
			return fmt.Errorf("alert %d: when is required", i+1)
		}
		if a.Severity == "" {
			f.Alerts[i].Severity = "warning"
		} else if a.Severity != "warning" && a.Severity != "critical" {
			return fmt.Errorf("alert %d: severity must be warning or critical", i+1)
		}
	}
	return nil
}

// validateRun fills defaults and checks data and scenarios.
func (f *File) validateRun() error {
	if f.Options.Timeout == 0 {
		f.Options.Timeout = yamlx.Duration(30 * time.Second)
	}
	if f.Options.GracefulStop == 0 {
		f.Options.GracefulStop = yamlx.Duration(30 * time.Second)
	}
	if f.Options.LimitWindow <= 0 {
		f.Options.LimitWindow = yamlx.Duration(5 * time.Second)
	}
	if f.Options.HTTP2 == nil {
		t := true
		f.Options.HTTP2 = &t
	}
	for name, d := range f.Data {
		if err := d.validate(name); err != nil {
			return err
		}
	}
	for _, name := range f.ScenarioNames() {
		if err := f.Scenarios[name].validate(); err != nil {
			return fmt.Errorf("scenario %s: %w", name, err)
		}
	}
	return nil
}

func (s *Scenario) validate() error {
	if s.Executor == "" {
		switch {
		case s.Users > 0 && len(s.Stages) == 0:
			s.Executor = ConstantUsers
		default:
			s.Executor = ArrivalRate
		}
	}
	if s.OnError == "" {
		s.OnError = "abort"
	}
	if s.OnError != "abort" && s.OnError != "continue" {
		return fmt.Errorf("onError must be abort or continue")
	}
	if len(s.Flow) == 0 {
		return fmt.Errorf("flow is empty")
	}
	for i, st := range s.Flow {
		kinds := 0
		for k := range st {
			if k != "once" {
				kinds++
			}
		}
		if kinds != 1 {
			return fmt.Errorf("flow step %d: each step needs exactly one kind (http, ws, grpc, script, sleep, set)", i+1)
		}
	}
	for i, st := range s.Stages {
		if st.Duration <= 0 {
			return fmt.Errorf("stage %d: duration must be > 0", i+1)
		}
		if st.Target < 0 {
			return fmt.Errorf("stage %d: target must be >= 0", i+1)
		}
	}
	switch s.Executor {
	case ArrivalRate:
		if len(s.Stages) == 0 {
			if s.Rate <= 0 || s.Duration <= 0 {
				return fmt.Errorf("arrival-rate needs rate and duration, or stages")
			}
			s.StartRate = s.Rate
			s.Stages = []Stage{{Duration: s.Duration, Target: s.Rate}}
		}
		if s.MaxInFlight == 0 {
			peak := s.StartRate
			for _, st := range s.Stages {
				if st.Target > peak {
					peak = st.Target
				}
			}
			// Enough for every arrival of 10 seconds to be in flight at once.
			s.MaxInFlight = int(peak*10) + 100
		}
	case ConstantUsers:
		if s.Users <= 0 || s.Duration <= 0 {
			return fmt.Errorf("constant-users needs users and duration")
		}
		s.StartUsers = s.Users
		s.Stages = []Stage{{Duration: s.Duration, Target: float64(s.Users)}}
	case RampingUsers:
		if len(s.Stages) == 0 {
			return fmt.Errorf("ramping-users needs stages")
		}
	case Iterations:
		if s.Iterations <= 0 {
			return fmt.Errorf("iterations executor needs iterations > 0")
		}
		if s.Users <= 0 {
			s.Users = 1
		}
		if s.Duration <= 0 {
			s.Duration = yamlx.Duration(10 * time.Minute)
		}
		s.StartUsers = s.Users
		s.Stages = []Stage{{Duration: s.Duration, Target: float64(s.Users)}}
	default:
		return fmt.Errorf("unknown executor %q (use %s, %s, %s, %s)", s.Executor, ArrivalRate, ConstantUsers, RampingUsers, Iterations)
	}
	return nil
}

// TotalDuration is how long the scenario runs, including StartAfter.
func (s *Scenario) TotalDuration() time.Duration {
	d := s.StartAfter.D()
	for _, st := range s.Stages {
		d += st.Duration.D()
	}
	return d
}

// Partition scales the file for agent index i of n, so that n agents
// together produce the load the file describes.
func (f *File) Partition(i, n int) {
	if n <= 1 {
		return
	}
	for _, s := range f.Scenarios {
		switch s.Executor {
		case ArrivalRate:
			s.Rate /= float64(n)
			s.StartRate /= float64(n)
			for j := range s.Stages {
				s.Stages[j].Target /= float64(n)
			}
			s.MaxInFlight = (s.MaxInFlight + n - 1) / n
		default:
			s.Iterations = splitInt(s.Iterations, i, n)
			s.Users = splitInt(s.Users, i, n)
			s.StartUsers = splitInt(s.StartUsers, i, n)
			for j := range s.Stages {
				s.Stages[j].Target = float64(splitInt(int(s.Stages[j].Target), i, n))
			}
		}
	}
	for _, d := range f.Data {
		d.partIndex, d.partCount = i, n
	}
}

// splitInt gives agent i its share of total, spreading the remainder over
// the first agents.
func splitInt(total, i, n int) int {
	share := total / n
	if i < total%n {
		share++
	}
	return share
}
