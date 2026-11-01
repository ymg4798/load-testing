// Package script runs JavaScript functions from scenario flows with goja.
//
//   - script: hooks.js#pickItem
//
// hooks.js:
//
//	export function pickItem(ctx) {
//	  const items = ctx.last.json.items;
//	  ctx.vars.itemId = items[Math.floor(Math.random() * items.length)].id;
//	}
//
// ctx.vars is the VU's live variable map, ctx.last the previous response
// ({status, body, headers, json, latencyMs}), plus ctx.vu, ctx.iter and
// ctx.scenario. Returning false or calling fail("why") fails the iteration.
// Each VU gets its own JS runtime, so scripts can keep state in globals.
package script

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dop251/goja"

	"github.com/ymg4798/load-testing/internal/protocol"
)

func init() { protocol.Register("script", compileStep) }

var exportRe = regexp.MustCompile(`(?m)^(\s*)export\s+(default\s+)?(async\s+function|function|const|let|var|class)\b`)

// Load compiles a script file. `export` keywords are dropped so files can
// be written as modules.
func Load(path string) (*goja.Program, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	code := exportRe.ReplaceAllString(string(src), "$1$3")
	return goja.Compile(filepath.Base(path), code, false)
}

// SplitRef splits "file.js#fn" into its parts.
func SplitRef(ref string) (file, fn string, err error) {
	file, fn, ok := strings.Cut(ref, "#")
	if !ok || file == "" || fn == "" {
		return "", "", fmt.Errorf("script %q: expected file.js#function", ref)
	}
	return file, fn, nil
}

type failErr struct{ msg string }

func (e *failErr) Error() string { return e.msg }

// newRuntime creates a runtime with the helper globals and the program loaded.
func newRuntime(prog *goja.Program, logPrefix string) (*goja.Runtime, error) {
	rt := goja.New()
	rt.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	console := rt.NewObject()
	logf := func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		fmt.Fprintln(os.Stderr, logPrefix+strings.Join(parts, " "))
		return goja.Undefined()
	}
	_ = console.Set("log", logf)
	_ = console.Set("error", logf)
	_ = rt.Set("console", console)
	_ = rt.Set("fail", func(msg string) { panic(rt.NewGoError(&failErr{msg})) })
	if _, err := rt.RunProgram(prog); err != nil {
		return nil, err
	}
	return rt, nil
}

type step struct {
	name string
	fn   string
	prog *goja.Program
	key  string
}

func compileStep(raw any, env *protocol.Env) (protocol.Step, error) {
	ref, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("script: expected file.js#function")
	}
	file, fn, err := SplitRef(ref)
	if err != nil {
		return nil, err
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(env.BaseDir, file)
	}
	p, err := env.Shared("js:"+path, func() (any, error) { return Load(path) })
	if err != nil {
		return nil, fmt.Errorf("script %s: %w", file, err)
	}
	// Check that the function exists now rather than at the first iteration.
	rt, err := newRuntime(p.(*goja.Program), "")
	if err != nil {
		return nil, fmt.Errorf("script %s: %w", file, err)
	}
	if _, ok := goja.AssertFunction(rt.Get(fn)); !ok {
		return nil, fmt.Errorf("script %s: no function %q", file, fn)
	}
	return &step{name: "_script:" + fn, fn: fn, prog: p.(*goja.Program), key: "js:" + path}, nil
}

type vuRuntime struct {
	rt  *goja.Runtime
	fns map[string]goja.Callable
}

func (s *step) Run(ctx context.Context, vu *protocol.VU) error {
	start := time.Now()
	st := vu.State(s.key, func() any {
		rt, err := newRuntime(s.prog, fmt.Sprintf("[vu %d] ", vu.ID))
		if err != nil {
			return err
		}
		return &vuRuntime{rt: rt, fns: map[string]goja.Callable{}}
	})
	if err, ok := st.(error); ok {
		return s.fail(vu, start, "script", err.Error())
	}
	r := st.(*vuRuntime)
	f, ok := r.fns[s.fn]
	if !ok {
		f, _ = goja.AssertFunction(r.rt.Get(s.fn))
		r.fns[s.fn] = f
	}
	stop := context.AfterFunc(ctx, func() { r.rt.Interrupt("canceled") })
	defer stop()
	res, err := f(goja.Undefined(), r.rt.ToValue(ctxObject(r.rt, vu)))
	r.rt.ClearInterrupt()
	if err != nil {
		var fe *failErr
		if ge, ok := err.(*goja.Exception); ok {
			if inner, ok := ge.Value().Export().(error); ok && asFail(inner, &fe) {
				return s.fail(vu, start, "script:fail", fe.msg)
			}
		}
		return s.fail(vu, start, "script:error", err.Error())
	}
	if b, ok := res.Export().(bool); ok && !b {
		return s.fail(vu, start, "script:false", "")
	}
	return nil
}

func asFail(err error, out **failErr) bool {
	for err != nil {
		if fe, ok := err.(*failErr); ok {
			*out = fe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (s *step) fail(vu *protocol.VU, start time.Time, kind, msg string) error {
	vu.Record(protocol.Result{Step: s.name, Latency: time.Since(start), Err: kind})
	return protocol.Fail(s.name, kind, msg)
}

func ctxObject(rt *goja.Runtime, vu *protocol.VU) *goja.Object {
	o := rt.NewObject()
	_ = o.Set("vars", vu.Vars)
	_ = o.Set("vu", vu.ID)
	_ = o.Set("iter", vu.Iter)
	_ = o.Set("scenario", vu.Scenario)
	if vu.Last != nil {
		_ = o.Set("last", responseObject(rt, vu.Last, false))
	} else {
		_ = o.Set("last", goja.Null())
	}
	return o
}

func responseObject(rt *goja.Runtime, r *protocol.Response, jsonFunc bool) *goja.Object {
	o := rt.NewObject()
	_ = o.Set("status", r.Status)
	_ = o.Set("body", string(r.Body))
	_ = o.Set("latencyMs", float64(r.Latency.Microseconds())/1000)
	h := map[string]any{}
	for k, v := range r.Headers {
		if len(v) > 0 {
			h[strings.ToLower(k)] = v[0]
		}
	}
	_ = o.Set("headers", h)
	parse := func() any {
		v, err := r.JSON()
		if err != nil {
			return nil
		}
		return v
	}
	if jsonFunc {
		_ = o.Set("json", parse)
	} else {
		_ = o.DefineAccessorProperty("json", rt.ToValue(parse), nil, goja.FLAG_FALSE, goja.FLAG_TRUE)
	}
	return o
}

// RunSetup runs a setup function once before the test. It receives
// ctx.vars (the global variables) and an http helper; the object it
// returns is merged into the global variables.
//
//	export function setup(ctx) {
//	  const r = http.post(ctx.vars.baseUrl + "/login", { json: { id: "admin" } });
//	  return { adminToken: r.json().token };
//	}
func RunSetup(baseDir, ref string, vars map[string]any) (map[string]any, error) {
	file, fn, err := SplitRef(ref)
	if err != nil {
		return nil, err
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, file)
	}
	prog, err := Load(path)
	if err != nil {
		return nil, fmt.Errorf("setup %s: %w", file, err)
	}
	rt, err := newRuntime(prog, "[setup] ")
	if err != nil {
		return nil, fmt.Errorf("setup %s: %w", file, err)
	}
	installHTTP(rt)
	f, ok := goja.AssertFunction(rt.Get(fn))
	if !ok {
		return nil, fmt.Errorf("setup %s: no function %q", file, fn)
	}
	o := rt.NewObject()
	_ = o.Set("vars", vars)
	res, err := f(goja.Undefined(), o)
	if err != nil {
		return nil, fmt.Errorf("setup %s#%s: %w", file, fn, err)
	}
	out := map[string]any{}
	if res != nil && !goja.IsUndefined(res) && !goja.IsNull(res) {
		m, ok := res.Export().(map[string]any)
		if !ok {
			return nil, fmt.Errorf("setup %s#%s: must return an object", file, fn)
		}
		out = m
	}
	return out, nil
}

// installHTTP adds a small synchronous http client for setup code:
// http.get(url, opts), http.post(url, opts), http.request(method, url, opts)
// with opts {headers, body, json}; the result has status, body, headers, json().
func installHTTP(rt *goja.Runtime) {
	client := &http.Client{Timeout: 30 * time.Second}
	do := func(method, url string, opts map[string]any) (*goja.Object, error) {
		var body io.Reader
		hdr := http.Header{}
		if opts != nil {
			if j, ok := opts["json"]; ok {
				b, err := json.Marshal(j)
				if err != nil {
					return nil, err
				}
				body = strings.NewReader(string(b))
				hdr.Set("Content-Type", "application/json")
			} else if b, ok := opts["body"].(string); ok {
				body = strings.NewReader(b)
			}
			if h, ok := opts["headers"].(map[string]any); ok {
				for k, v := range h {
					hdr.Set(k, fmt.Sprint(v))
				}
			}
		}
		req, err := http.NewRequest(method, url, body)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		r := &protocol.Response{Status: resp.StatusCode, Headers: resp.Header, Body: data, Latency: time.Since(start)}
		return responseObject(rt, r, true), nil
	}
	h := rt.NewObject()
	_ = h.Set("request", func(method, url string, opts map[string]any) (*goja.Object, error) {
		return do(strings.ToUpper(method), url, opts)
	})
	_ = h.Set("get", func(url string, opts map[string]any) (*goja.Object, error) { return do("GET", url, opts) })
	_ = h.Set("post", func(url string, opts map[string]any) (*goja.Object, error) { return do("POST", url, opts) })
	_ = h.Set("put", func(url string, opts map[string]any) (*goja.Object, error) { return do("PUT", url, opts) })
	_ = h.Set("del", func(url string, opts map[string]any) (*goja.Object, error) { return do("DELETE", url, opts) })
	_ = rt.Set("http", h)
}
