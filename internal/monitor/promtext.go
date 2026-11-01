package monitor

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// sample is one line of the Prometheus text format.
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// parseText reads the Prometheus text exposition format. It returns the
// samples and the declared type of each metric family (# TYPE lines).
func parseText(r io.Reader) ([]sample, map[string]string, error) {
	var out []sample
	types := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			f := strings.Fields(line)
			if len(f) >= 4 && f[1] == "TYPE" {
				types[f[2]] = f[3]
			}
			continue
		}
		s, err := parseLine(line)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, s)
	}
	return out, types, sc.Err()
}

func parseLine(line string) (sample, error) {
	s := sample{}
	i := strings.IndexAny(line, "{ \t")
	if i < 0 {
		return s, fmt.Errorf("bad metric line %q", line)
	}
	s.name = line[:i]
	rest := line[i:]
	if rest[0] == '{' {
		labels, n, err := parseLabels(rest)
		if err != nil {
			return s, fmt.Errorf("%w in %q", err, line)
		}
		s.labels = labels
		rest = rest[n:]
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return s, fmt.Errorf("no value in %q", line)
	}
	v, err := parseValue(f[0])
	if err != nil {
		return s, fmt.Errorf("bad value in %q", line)
	}
	s.value = v
	return s, nil
}

// parseLabels reads `{a="x",b="y"}` and returns how many bytes it used.
func parseLabels(s string) (map[string]string, int, error) {
	m := map[string]string{}
	i := 1
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated labels")
		}
		if s[i] == '}' {
			return m, i + 1, nil
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, 0, fmt.Errorf("bad label")
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			return nil, 0, fmt.Errorf("label value must be quoted")
		}
		i++
		var b strings.Builder
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(s[i])
				}
				continue
			}
			b.WriteByte(s[i])
		}
		if i >= len(s) {
			return nil, 0, fmt.Errorf("unterminated label value")
		}
		i++
		m[key] = b.String()
	}
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

// selector is `name` or `name{label="v",other!="w",x=~"re"}`.
type selector struct {
	name     string
	matchers []matcher
}

type matcher struct {
	key, op, val string
	re           *regexp.Regexp
}

func parseSelector(expr string) (*selector, error) {
	expr = strings.TrimSpace(expr)
	i := strings.IndexByte(expr, '{')
	if i < 0 {
		if expr == "" {
			return nil, fmt.Errorf("empty metric")
		}
		return &selector{name: expr}, nil
	}
	if !strings.HasSuffix(expr, "}") {
		return nil, fmt.Errorf("%q: missing }", expr)
	}
	sel := &selector{name: strings.TrimSpace(expr[:i])}
	body := expr[i+1 : len(expr)-1]
	for _, part := range splitTop(body) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var m matcher
		for _, op := range []string{"!=", "=~", "!~", "="} {
			if j := strings.Index(part, op); j > 0 {
				m.key, m.op = strings.TrimSpace(part[:j]), op
				v := strings.TrimSpace(part[j+len(op):])
				uq, err := strconv.Unquote(v)
				if err != nil {
					return nil, fmt.Errorf("%q: label value must be quoted", expr)
				}
				m.val = uq
				break
			}
		}
		if m.op == "" {
			return nil, fmt.Errorf("%q: bad matcher %q", expr, part)
		}
		if m.op == "=~" || m.op == "!~" {
			re, err := regexp.Compile("^(?:" + m.val + ")$")
			if err != nil {
				return nil, fmt.Errorf("%q: %w", expr, err)
			}
			m.re = re
		}
		sel.matchers = append(sel.matchers, m)
	}
	return sel, nil
}

// splitTop splits on commas outside quotes.
func splitTop(s string) []string {
	var out []string
	start, inQ := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			inQ = !inQ
		case ',':
			if !inQ {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func (sel *selector) match(s sample) bool {
	if s.name != sel.name {
		return false
	}
	for _, m := range sel.matchers {
		v := s.labels[m.key]
		var ok bool
		switch m.op {
		case "=":
			ok = v == m.val
		case "!=":
			ok = v != m.val
		case "=~":
			ok = m.re.MatchString(v)
		case "!~":
			ok = !m.re.MatchString(v)
		}
		if !ok {
			return false
		}
	}
	return true
}
