// Package jsonpath reads values from decoded JSON with a small subset of
// JSONPath: $.a.b, $.list[0], $['key with space'], $.list[*].id, $.list.length
package jsonpath

import (
	"fmt"
	"strconv"
	"strings"
)

type seg struct {
	key   string
	index int
	all   bool
	isIdx bool
}

// Path is a compiled expression.
type Path struct {
	src  string
	segs []seg
}

// Compile parses a path. The leading "$" is optional.
func Compile(src string) (*Path, error) {
	s := strings.TrimSpace(src)
	s = strings.TrimPrefix(s, "$")
	p := &Path{src: src}
	for len(s) > 0 {
		switch s[0] {
		case '.':
			s = s[1:]
			n := strings.IndexAny(s, ".[")
			if n < 0 {
				n = len(s)
			}
			if n == 0 {
				return nil, fmt.Errorf("jsonpath %q: empty key", src)
			}
			p.segs = append(p.segs, seg{key: s[:n]})
			s = s[n:]
		case '[':
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return nil, fmt.Errorf("jsonpath %q: missing ]", src)
			}
			in := strings.TrimSpace(s[1:end])
			s = s[end+1:]
			switch {
			case in == "*":
				p.segs = append(p.segs, seg{all: true})
			case len(in) >= 2 && (in[0] == '\'' || in[0] == '"'):
				p.segs = append(p.segs, seg{key: in[1 : len(in)-1]})
			default:
				i, err := strconv.Atoi(in)
				if err != nil {
					return nil, fmt.Errorf("jsonpath %q: bad index %q", src, in)
				}
				p.segs = append(p.segs, seg{index: i, isIdx: true})
			}
		default:
			// allow "a.b" without the leading "$."
			if len(p.segs) == 0 {
				s = "." + s
				continue
			}
			return nil, fmt.Errorf("jsonpath %q: unexpected %q", src, s[0])
		}
	}
	return p, nil
}

// String returns the source text.
func (p *Path) String() string { return p.src }

// Get evaluates the path. The bool is false when nothing matched.
func (p *Path) Get(doc any) (any, bool) { return get(doc, p.segs) }

func get(cur any, segs []seg) (any, bool) {
	for i, s := range segs {
		switch {
		case s.all:
			list, ok := cur.([]any)
			if !ok {
				return nil, false
			}
			var out []any
			for _, item := range list {
				if v, ok := get(item, segs[i+1:]); ok {
					out = append(out, v)
				}
			}
			return out, true
		case s.isIdx:
			list, ok := cur.([]any)
			if !ok {
				return nil, false
			}
			idx := s.index
			if idx < 0 {
				idx += len(list)
			}
			if idx < 0 || idx >= len(list) {
				return nil, false
			}
			cur = list[idx]
		default:
			switch c := cur.(type) {
			case map[string]any:
				v, ok := c[s.key]
				if !ok {
					return nil, false
				}
				cur = v
			case []any:
				if s.key == "length" {
					cur = float64(len(c))
					continue
				}
				return nil, false
			case string:
				if s.key == "length" {
					cur = float64(len(c))
					continue
				}
				return nil, false
			default:
				return nil, false
			}
		}
	}
	return cur, true
}
