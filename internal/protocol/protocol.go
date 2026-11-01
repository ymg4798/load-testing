// Package protocol defines what a scenario step is and the shared pieces
// every protocol plugin uses: the virtual user, checks, extraction and
// error classification. Plugins register themselves by name ("http", "ws",
// "grpc", ...) and the engine looks them up when it compiles a flow.
package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/ymg4798/lt/internal/tmpl"
)

// Result is one measurement a step reports.
type Result struct {
	Step    string
	Latency time.Duration
	Bytes   int64
	Err     string // "" when it succeeded, otherwise an error kind such as "timeout" or "status:503"
	TraceID string // set when the request carried a trace context
}

// Step is one compiled entry of a flow.
type Step interface {
	// Run executes the step for one virtual user. It reports measurements
	// through vu.Record and returns an error when the iteration should be
	// treated as failed.
	Run(ctx context.Context, vu *VU) error
}

// Options are the scenario-file settings protocols read.
type Options struct {
	Timeout         time.Duration
	Insecure        bool
	HTTP2           bool
	MaxConnsPerHost int
	NoKeepAlive     bool
	Cookies         bool
	MaxInFlight     int
	Tracing         Tracing
}

// Env is what a plugin gets while compiling a step. One Env exists per
// scenario per run; Shared holds resources that VUs share (connection
// pools, compiled scripts).
type Env struct {
	BaseDir  string
	Scenario string
	Options  Options
	// HasScript is true when the flow contains script steps, which read
	// ctx.last, so response bodies must be kept.
	HasScript bool

	mu      sync.Mutex // guards shared
	shared  map[string]any
	closeMu sync.Mutex // guards closers; separate so create funcs may call OnClose
	closers []func()
}

// Shared returns the shared resource stored under key, creating it once.
func (e *Env) Shared(key string, create func() (any, error)) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shared == nil {
		e.shared = map[string]any{}
	}
	if v, ok := e.shared[key]; ok {
		return v, nil
	}
	v, err := create()
	if err != nil {
		return nil, err
	}
	e.shared[key] = v
	return v, nil
}

// OnClose registers cleanup for the end of the run.
func (e *Env) OnClose(f func()) {
	e.closeMu.Lock()
	e.closers = append(e.closers, f)
	e.closeMu.Unlock()
}

// Close runs cleanup registered with OnClose.
func (e *Env) Close() {
	e.closeMu.Lock()
	cs := e.closers
	e.closers = nil
	e.closeMu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}

// Factory compiles the YAML body of a step.
type Factory func(raw any, env *Env) (Step, error)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register makes a step kind available to scenario files.
func Register(kind string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[kind] = f
}

// Lookup returns the factory for a step kind.
func Lookup(kind string) (Factory, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	f, ok := registry[kind]
	return f, ok
}

// Kinds lists registered step kinds.
func Kinds() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	var out []string
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Response is the last response a VU received, for checks, extraction and scripts.
type Response struct {
	Status  int
	Headers map[string][]string
	Body    []byte
	Latency time.Duration

	parsed  bool
	json    any
	jsonErr error
}

// JSON decodes the body once.
func (r *Response) JSON() (any, error) {
	if !r.parsed {
		r.parsed = true
		r.jsonErr = json.Unmarshal(r.Body, &r.json)
	}
	return r.json, r.jsonErr
}

// Header returns the first value of a header (case-insensitive).
func (r *Response) Header(name string) (string, bool) {
	for k, v := range r.Headers {
		if equalFold(k, name) && len(v) > 0 {
			return v[0], true
		}
	}
	return "", false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// VU is one virtual user. A VU runs one iteration at a time, so its fields
// need no locking.
type VU struct {
	ID       int
	Iter     int64
	Scenario string
	Vars     map[string]any
	Last     *Response
	Rand     *rand.Rand

	// Record reports one measurement. Set by the engine.
	Record func(Result)

	state    map[string]any
	iterEnd  []func()
	closeFns []func()
}

// NewVU creates a VU with a copy of the global variables.
func NewVU(id int, scenario string, globals map[string]any, record func(Result)) *VU {
	vars := make(map[string]any, len(globals)+8)
	for k, v := range globals {
		vars[k] = v
	}
	return &VU{
		ID: id, Scenario: scenario, Vars: vars, Record: record,
		Rand:  rand.New(rand.NewPCG(uint64(id)+1, uint64(time.Now().UnixNano()))),
		state: map[string]any{},
	}
}

// State returns per-VU protocol state stored under key, creating it on first use.
func (vu *VU) State(key string, create func() any) any {
	if v, ok := vu.state[key]; ok {
		return v
	}
	v := create()
	vu.state[key] = v
	return v
}

// SetState stores or (with nil) removes per-VU state.
func (vu *VU) SetState(key string, v any) {
	if v == nil {
		delete(vu.state, key)
		return
	}
	vu.state[key] = v
}

// GetState reads per-VU state.
func (vu *VU) GetState(key string) (any, bool) {
	v, ok := vu.state[key]
	return v, ok
}

// AtIterationEnd runs f when the current iteration finishes (closing
// connections opened during it, for example).
func (vu *VU) AtIterationEnd(f func()) { vu.iterEnd = append(vu.iterEnd, f) }

// AtClose runs f when the VU is retired at the end of the test.
func (vu *VU) AtClose(f func()) { vu.closeFns = append(vu.closeFns, f) }

// EndIteration runs AtIterationEnd callbacks. Called by the engine.
func (vu *VU) EndIteration() {
	fs := vu.iterEnd
	vu.iterEnd = nil
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

// Close runs AtClose callbacks. Called by the engine.
func (vu *VU) Close() {
	vu.EndIteration()
	for i := len(vu.closeFns) - 1; i >= 0; i-- {
		vu.closeFns[i]()
	}
	vu.closeFns = nil
}

// Lookup implements tmpl.Scope: vu, iter, scenario, last.*, then variables.
func (vu *VU) Lookup(path []string) (any, bool) {
	switch path[0] {
	case "vu":
		if len(path) == 1 {
			return vu.ID, true
		}
	case "iter":
		if len(path) == 1 {
			return vu.Iter, true
		}
	case "scenario":
		if len(path) == 1 {
			return vu.Scenario, true
		}
	case "last":
		if vu.Last == nil {
			return nil, false
		}
		if len(path) == 2 {
			switch path[1] {
			case "status":
				return vu.Last.Status, true
			case "body":
				return string(vu.Last.Body), true
			}
		}
		if len(path) >= 2 && path[1] == "json" {
			doc, err := vu.Last.JSON()
			if err != nil {
				return nil, false
			}
			if len(path) == 2 {
				return doc, true
			}
			if m, ok := doc.(map[string]any); ok {
				return tmpl.LookupPath(m, path[2:])
			}
			return nil, false
		}
	}
	return tmpl.LookupPath(vu.Vars, path)
}

// StepError marks an iteration as failed with a short reason.
type StepError struct {
	Step string
	Kind string
	Msg  string
}

func (e *StepError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%s: %s: %s", e.Step, e.Kind, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Step, e.Kind)
}

// Fail returns a StepError.
func Fail(step, kind, msg string) error { return &StepError{Step: step, Kind: kind, Msg: msg} }
