package protocol

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ymg4798/lt/internal/jsonpath"
	"github.com/ymg4798/lt/internal/tmpl"
	"github.com/ymg4798/lt/internal/yamlx"
)

// CheckSpec is the YAML form of a response check.
//
//	check:
//	  status: 200            # or [200, 201] or "2xx"
//	  bodyContains: "ok"
//	  bodyNotContains: "error"
//	  json: { "$.ok": true }
//	  header: { Content-Type: json }
//	  maxLatency: 500ms
type CheckSpec struct {
	Status          any               `yaml:"status"`
	BodyContains    string            `yaml:"bodyContains"`
	BodyNotContains string            `yaml:"bodyNotContains"`
	JSON            map[string]any    `yaml:"json"`
	Header          map[string]string `yaml:"header"`
	MaxLatency      yamlx.Duration    `yaml:"maxLatency"`
}

// Check is a compiled CheckSpec.
type Check struct {
	status      func(int) bool
	hasStatus   bool
	contains    *tmpl.String
	notContains *tmpl.String
	json        []jsonCheck
	header      map[string]*tmpl.String
	maxLatency  time.Duration
}

type jsonCheck struct {
	path *jsonpath.Path
	want tmpl.Value
}

// CompileCheck compiles a check. A nil spec gives a check that only fails
// on status >= 400 (for protocols that have statuses).
func CompileCheck(spec *CheckSpec) (*Check, error) {
	c := &Check{}
	if spec == nil {
		return c, nil
	}
	if spec.Status != nil {
		f, err := statusMatcher(spec.Status)
		if err != nil {
			return nil, err
		}
		c.status, c.hasStatus = f, true
	}
	var err error
	if spec.BodyContains != "" {
		if c.contains, err = tmpl.Compile(spec.BodyContains); err != nil {
			return nil, err
		}
	}
	if spec.BodyNotContains != "" {
		if c.notContains, err = tmpl.Compile(spec.BodyNotContains); err != nil {
			return nil, err
		}
	}
	for p, want := range spec.JSON {
		jp, err := jsonpath.Compile(p)
		if err != nil {
			return nil, err
		}
		w, err := tmpl.CompileValue(want)
		if err != nil {
			return nil, err
		}
		c.json = append(c.json, jsonCheck{jp, w})
	}
	if len(spec.Header) > 0 {
		c.header = map[string]*tmpl.String{}
		for k, v := range spec.Header {
			t, err := tmpl.Compile(v)
			if err != nil {
				return nil, err
			}
			c.header[k] = t
		}
	}
	c.maxLatency = spec.MaxLatency.D()
	return c, nil
}

func statusMatcher(v any) (func(int) bool, error) {
	var parts []func(int) bool
	add := func(x any) error {
		switch s := x.(type) {
		case int:
			parts = append(parts, func(c int) bool { return c == s })
		case float64:
			n := int(s)
			parts = append(parts, func(c int) bool { return c == n })
		case string:
			s = strings.TrimSpace(strings.ToLower(s))
			if len(s) == 3 && strings.HasSuffix(s, "xx") && s[0] >= '1' && s[0] <= '5' {
				d := int(s[0] - '0')
				parts = append(parts, func(c int) bool { return c/100 == d })
				return nil
			}
			n, err := strconv.Atoi(s)
			if err != nil {
				return fmt.Errorf("check.status: bad value %q", s)
			}
			parts = append(parts, func(c int) bool { return c == n })
		default:
			return fmt.Errorf("check.status: bad value %v", x)
		}
		return nil
	}
	if list, ok := v.([]any); ok {
		for _, x := range list {
			if err := add(x); err != nil {
				return nil, err
			}
		}
	} else if err := add(v); err != nil {
		return nil, err
	}
	return func(c int) bool {
		for _, p := range parts {
			if p(c) {
				return true
			}
		}
		return false
	}, nil
}

// Verify returns an error kind ("" when the response passes) and a message.
// When useStatus is true and no status rule was given, status >= 400 fails.
func (c *Check) Verify(r *Response, s tmpl.Scope, useStatus bool) (kind, msg string) {
	if useStatus {
		if c.hasStatus {
			if !c.status(r.Status) {
				return "status:" + strconv.Itoa(r.Status), ""
			}
		} else if r.Status >= 400 {
			return "status:" + strconv.Itoa(r.Status), ""
		}
	}
	if c.maxLatency > 0 && r.Latency > c.maxLatency {
		return "check:latency", fmt.Sprintf("%s > %s", r.Latency, c.maxLatency)
	}
	if c.contains != nil {
		want, err := c.contains.RenderString(s)
		if err != nil {
			return "check:template", err.Error()
		}
		if !strings.Contains(string(r.Body), want) {
			return "check:body", fmt.Sprintf("body does not contain %q", want)
		}
	}
	if c.notContains != nil {
		bad, err := c.notContains.RenderString(s)
		if err != nil {
			return "check:template", err.Error()
		}
		if strings.Contains(string(r.Body), bad) {
			return "check:body", fmt.Sprintf("body contains %q", bad)
		}
	}
	for k, t := range c.header {
		want, err := t.RenderString(s)
		if err != nil {
			return "check:template", err.Error()
		}
		got, ok := r.Header(k)
		if !ok || !strings.Contains(got, want) {
			return "check:header", fmt.Sprintf("header %s=%q, want %q", k, got, want)
		}
	}
	if len(c.json) > 0 {
		doc, err := r.JSON()
		if err != nil {
			return "check:json", "body is not JSON"
		}
		for _, jc := range c.json {
			got, ok := jc.path.Get(doc)
			if !ok {
				return "check:json", fmt.Sprintf("%s missing", jc.path)
			}
			want, err := jc.want.Render(s)
			if err != nil {
				return "check:template", err.Error()
			}
			if !looseEqual(got, want) {
				return "check:json", fmt.Sprintf("%s = %v, want %v", jc.path, got, want)
			}
		}
	}
	return "", ""
}

// NeedsBody reports whether the check reads the body.
func (c *Check) NeedsBody() bool {
	return c.contains != nil || c.notContains != nil || len(c.json) > 0
}

// looseEqual compares JSON values, treating numbers of any type alike and
// comparing scalars by text otherwise.
func looseEqual(a, b any) bool {
	if fa, ok := toFloat(a); ok {
		if fb, ok := toFloat(b); ok {
			return fa == fb
		}
	}
	if reflect.DeepEqual(a, b) {
		return true
	}
	return tmpl.ToString(a) == tmpl.ToString(b)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}

// Extract pulls values out of a response into VU variables.
//
//	extract:
//	  token: $.token           # JSONPath
//	  session: header:Set-Cookie
//	  csrf: regex:name="csrf" value="([^"]+)"
//	  code: status
//	  raw: body
type Extract struct {
	items []extractItem
}

type extractItem struct {
	name   string
	kind   string // json, header, regex, status, body
	path   *jsonpath.Path
	header string
	re     *regexp.Regexp
}

// CompileExtract compiles an extract map.
func CompileExtract(m map[string]string) (*Extract, error) {
	e := &Extract{}
	for name, spec := range m {
		it := extractItem{name: name}
		switch {
		case spec == "status":
			it.kind = "status"
		case spec == "body":
			it.kind = "body"
		case strings.HasPrefix(spec, "header:"):
			it.kind, it.header = "header", strings.TrimSpace(strings.TrimPrefix(spec, "header:"))
		case strings.HasPrefix(spec, "regex:"):
			re, err := regexp.Compile(strings.TrimPrefix(spec, "regex:"))
			if err != nil {
				return nil, fmt.Errorf("extract %s: %w", name, err)
			}
			it.kind, it.re = "regex", re
		default:
			p, err := jsonpath.Compile(spec)
			if err != nil {
				return nil, fmt.Errorf("extract %s: %w", name, err)
			}
			it.kind, it.path = "json", p
		}
		e.items = append(e.items, it)
	}
	return e, nil
}

// Empty reports whether nothing is extracted.
func (e *Extract) Empty() bool { return e == nil || len(e.items) == 0 }

// NeedsBody reports whether extraction reads the body.
func (e *Extract) NeedsBody() bool {
	if e == nil {
		return false
	}
	for _, it := range e.items {
		if it.kind == "json" || it.kind == "regex" || it.kind == "body" {
			return true
		}
	}
	return false
}

// Apply stores extracted values in vars. It fails on the first value that
// cannot be found.
func (e *Extract) Apply(r *Response, vars map[string]any) (kind, msg string) {
	if e == nil {
		return "", ""
	}
	for _, it := range e.items {
		switch it.kind {
		case "status":
			vars[it.name] = r.Status
		case "body":
			vars[it.name] = string(r.Body)
		case "header":
			v, ok := r.Header(it.header)
			if !ok {
				return "extract:" + it.name, "header " + it.header + " missing"
			}
			vars[it.name] = v
		case "regex":
			m := it.re.FindSubmatch(r.Body)
			if m == nil {
				return "extract:" + it.name, "regex did not match"
			}
			if len(m) > 1 {
				vars[it.name] = string(m[1])
			} else {
				vars[it.name] = string(m[0])
			}
		case "json":
			doc, err := r.JSON()
			if err != nil {
				return "extract:" + it.name, "body is not JSON"
			}
			v, ok := it.path.Get(doc)
			if !ok {
				return "extract:" + it.name, it.path.String() + " missing"
			}
			vars[it.name] = v
		}
	}
	return "", ""
}
