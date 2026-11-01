// Package tmpl renders the {{ ... }} placeholders used in scenario files.
//
// A placeholder holds either a variable path (users.id, env.HOME, vu, iter)
// or a function call: uuid(), randInt(1,100), randStr(8), now(), isoNow().
// A string that is exactly one placeholder keeps the value's type, so
// `count: "{{ n }}"` sends a number in JSON.
package tmpl

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// Scope resolves variable paths while rendering.
type Scope interface {
	Lookup(path []string) (any, bool)
}

// MapScope is a Scope over a plain map, with env.* taken from the environment.
type MapScope map[string]any

func (m MapScope) Lookup(path []string) (any, bool) { return LookupPath(m, path) }

// LookupPath walks nested maps and lists. "env.X" reads environment variables.
func LookupPath(root map[string]any, path []string) (any, bool) {
	if len(path) == 0 {
		return nil, false
	}
	if path[0] == "env" && len(path) == 2 {
		if v, ok := root["env"]; ok {
			if m, ok := v.(map[string]any); ok {
				if x, ok := m[path[1]]; ok {
					return x, true
				}
			}
		}
		v, ok := os.LookupEnv(path[1])
		return v, ok
	}
	var cur any = root
	for _, p := range path {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[p]
			if !ok {
				return nil, false
			}
			cur = v
		case map[string]string:
			v, ok := c[p]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// Value is a compiled scenario value that may contain placeholders.
type Value interface {
	Render(s Scope) (any, error)
}

// String is a compiled string template.
type String struct {
	parts []part
	raw   string
}

type part struct {
	lit  string
	expr *expr // nil for literal parts
}

type expr struct {
	src  string
	path []string
	fn   string
	args []string
}

// Compile parses a template string.
func Compile(s string) (*String, error) {
	t := &String{raw: s}
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			if rest != "" {
				t.parts = append(t.parts, part{lit: rest})
			}
			return t, nil
		}
		if i > 0 {
			t.parts = append(t.parts, part{lit: rest[:i]})
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("template %q: missing }}", s)
		}
		e, err := parseExpr(strings.TrimSpace(rest[i+2 : i+j]))
		if err != nil {
			return nil, fmt.Errorf("template %q: %w", s, err)
		}
		t.parts = append(t.parts, part{expr: e})
		rest = rest[i+j+2:]
	}
}

// MustCompile is Compile for literals known to be valid.
func MustCompile(s string) *String {
	t, err := Compile(s)
	if err != nil {
		panic(err)
	}
	return t
}

func parseExpr(src string) (*expr, error) {
	if src == "" {
		return nil, fmt.Errorf("empty {{ }}")
	}
	e := &expr{src: src}
	if i := strings.IndexByte(src, '('); i >= 0 {
		if !strings.HasSuffix(src, ")") {
			return nil, fmt.Errorf("bad call %q", src)
		}
		e.fn = strings.TrimSpace(src[:i])
		if inner := strings.TrimSpace(src[i+1 : len(src)-1]); inner != "" {
			for _, a := range strings.Split(inner, ",") {
				e.args = append(e.args, strings.Trim(strings.TrimSpace(a), `"'`))
			}
		}
		switch e.fn {
		case "uuid", "randInt", "randStr", "now", "isoNow", "randFloat":
		default:
			return nil, fmt.Errorf("unknown function %q", e.fn)
		}
		return e, nil
	}
	e.path = strings.Split(src, ".")
	return e, nil
}

// IsStatic reports whether the string has no placeholders.
func (t *String) IsStatic() bool {
	for _, p := range t.parts {
		if p.expr != nil {
			return false
		}
	}
	return true
}

// Raw returns the source text.
func (t *String) Raw() string { return t.raw }

// Render returns the typed value when the template is a single placeholder,
// otherwise the rendered string.
func (t *String) Render(s Scope) (any, error) {
	if len(t.parts) == 1 && t.parts[0].expr != nil {
		return t.parts[0].expr.eval(s)
	}
	return t.RenderString(s)
}

// RenderString always returns a string.
func (t *String) RenderString(s Scope) (string, error) {
	if len(t.parts) == 1 && t.parts[0].expr == nil {
		return t.parts[0].lit, nil
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.expr == nil {
			b.WriteString(p.lit)
			continue
		}
		v, err := p.expr.eval(s)
		if err != nil {
			return "", err
		}
		b.WriteString(ToString(v))
	}
	return b.String(), nil
}

// ToString formats a value for insertion into text.
func ToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func (e *expr) eval(s Scope) (any, error) {
	if e.fn == "" {
		v, ok := s.Lookup(e.path)
		if !ok {
			return nil, fmt.Errorf("unknown variable {{ %s }}", e.src)
		}
		return v, nil
	}
	switch e.fn {
	case "uuid":
		var b [16]byte
		_, _ = rand.Read(b[:])
		b[6] = b[6]&0x0f | 0x40
		b[8] = b[8]&0x3f | 0x80
		h := hex.EncodeToString(b[:])
		return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
	case "randInt":
		lo, hi := int64(0), int64(100)
		if len(e.args) == 2 {
			var err1, err2 error
			lo, err1 = strconv.ParseInt(e.args[0], 10, 64)
			hi, err2 = strconv.ParseInt(e.args[1], 10, 64)
			if err1 != nil || err2 != nil || hi < lo {
				return nil, fmt.Errorf("randInt(min, max): bad arguments")
			}
		}
		return lo + mrand.Int64N(hi-lo+1), nil
	case "randFloat":
		return mrand.Float64(), nil
	case "randStr":
		n := 8
		if len(e.args) == 1 {
			if v, err := strconv.Atoi(e.args[0]); err == nil && v > 0 {
				n = v
			}
		}
		const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
		b := make([]byte, n)
		for i := range b {
			b[i] = letters[mrand.IntN(len(letters))]
		}
		return string(b), nil
	case "now":
		return time.Now().UnixMilli(), nil
	case "isoNow":
		return time.Now().UTC().Format(time.RFC3339Nano), nil
	}
	return nil, fmt.Errorf("unknown function %q", e.fn)
}

// CompileValue compiles every string inside a YAML value (maps, lists, scalars).
func CompileValue(v any) (Value, error) {
	switch x := v.(type) {
	case string:
		t, err := Compile(x)
		if err != nil {
			return nil, err
		}
		if t.IsStatic() {
			return static{x}, nil
		}
		return t, nil
	case map[string]any:
		m := mapValue{}
		for k, vv := range x {
			c, err := CompileValue(vv)
			if err != nil {
				return nil, err
			}
			m[k] = c
		}
		return m, nil
	case []any:
		l := make(listValue, len(x))
		for i, vv := range x {
			c, err := CompileValue(vv)
			if err != nil {
				return nil, err
			}
			l[i] = c
		}
		return l, nil
	default:
		return static{x}, nil
	}
}

type static struct{ v any }

func (s static) Render(Scope) (any, error) { return s.v, nil }

type mapValue map[string]Value

func (m mapValue) Render(s Scope) (any, error) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		r, err := v.Render(s)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}

type listValue []Value

func (l listValue) Render(s Scope) (any, error) {
	out := make([]any, len(l))
	for i, v := range l {
		r, err := v.Render(s)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}
