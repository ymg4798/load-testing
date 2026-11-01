package logs

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ymg4798/lt/internal/run"
)

// Query is a parsed search. All parts must match (AND):
//
//	timeout                  free text: case-insensitive substring of the message, stack or attributes
//	"connection refused"     a quoted phrase is one substring
//	/dead(lock|line)/        a regular expression (/…/i for case-insensitive)
//	level:error              level, also level=error; a list: level:warn,error
//	trace:4bf92f35…          trace ID (prefix allowed)
//	source:api               source (file name or service); globs allowed: source:web-*
//	-word, -level:debug      a leading - negates a part
type Query struct {
	parts []part
}

type part struct {
	neg    bool
	text   string         // lower-case substring
	re     *regexp.Regexp // /…/
	levels map[string]bool
	trace  string
	source string
}

// ParseQuery parses q; an empty query matches everything.
func ParseQuery(q string) (*Query, error) {
	out := &Query{}
	for _, tok := range tokenize(q) {
		p := part{}
		if len(tok.s) > 1 && tok.s[0] == '-' && !tok.quoted {
			p.neg = true
			tok.s = tok.s[1:]
		}
		s := tok.s
		switch {
		case tok.quoted:
			p.text = strings.ToLower(s)
		case len(s) >= 2 && s[0] == '/' && (strings.HasSuffix(s, "/") || strings.HasSuffix(s, "/i")):
			body, flags := s[1:len(s)-1], ""
			if strings.HasSuffix(s, "/i") && len(s) >= 3 {
				body, flags = s[1:len(s)-2], "(?i)"
			}
			re, err := regexp.Compile(flags + body)
			if err != nil {
				return nil, fmt.Errorf("query %s: %w", s, err)
			}
			p.re = re
		default:
			k, v, ok := cutField(s)
			switch {
			case ok && k == "level":
				p.levels = map[string]bool{}
				for _, l := range strings.Split(v, ",") {
					if l = normLevel(l); l != "" {
						p.levels[l] = true
					}
				}
			case ok && k == "trace":
				p.trace = strings.ToLower(v)
			case ok && k == "source":
				p.source = strings.ToLower(v)
			default:
				p.text = strings.ToLower(s)
			}
		}
		if p.text == "" && p.re == nil && p.levels == nil && p.trace == "" && p.source == "" {
			continue
		}
		out.parts = append(out.parts, p)
	}
	return out, nil
}

// cutField splits "level:error" and "level=error".
func cutField(s string) (k, v string, ok bool) {
	i := strings.IndexAny(s, ":=")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	k = strings.ToLower(s[:i])
	switch k {
	case "level", "lvl", "severity":
		k = "level"
	case "trace", "traceid", "trace_id":
		k = "trace"
	case "source", "service", "src":
		k = "source"
	default:
		return "", "", false
	}
	return k, strings.Trim(s[i+1:], `"'`), true
}

type token struct {
	s      string
	quoted bool
}

// tokenize splits on spaces, keeping "quoted phrases" and /regex with spaces/.
func tokenize(q string) []token {
	var out []token
	i := 0
	for i < len(q) {
		for i < len(q) && (q[i] == ' ' || q[i] == '\t') {
			i++
		}
		if i >= len(q) {
			break
		}
		switch {
		case q[i] == '"':
			j := strings.IndexByte(q[i+1:], '"')
			if j < 0 {
				out = append(out, token{s: q[i+1:], quoted: true})
				return out
			}
			out = append(out, token{s: q[i+1 : i+1+j], quoted: true})
			i += j + 2
		case q[i] == '/' || (q[i] == '-' && i+1 < len(q) && q[i+1] == '/'):
			start := i
			if q[i] == '-' {
				i++
			}
			j := i + 1
			for j < len(q) && q[j] != '/' {
				if q[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(q) { // no closing slash: plain word
				end := strings.IndexAny(q[start:], " \t")
				if end < 0 {
					end = len(q) - start
				}
				out = append(out, token{s: q[start : start+end]})
				i = start + end
				continue
			}
			j++
			if j < len(q) && q[j] == 'i' {
				j++
			}
			out = append(out, token{s: q[start:j]})
			i = j
		default:
			j := i
			for j < len(q) && q[j] != ' ' && q[j] != '\t' {
				j++
			}
			out = append(out, token{s: q[i:j]})
			i = j
		}
	}
	return out
}

// levelOnly reports whether the query only filters on level, so counts
// can come from the per-second counters.
func (q *Query) levelOnly() bool {
	for _, p := range q.parts {
		if p.levels == nil {
			return false
		}
	}
	return true
}

// levelOK reports whether a line of level l passes the level parts.
func (q *Query) levelOK(l string) bool {
	for _, p := range q.parts {
		if p.levels != nil && p.levels[l] == p.neg {
			return false
		}
	}
	return true
}

// Match reports whether a line matches.
func (q *Query) Match(l *run.LogLine) bool {
	for i := range q.parts {
		if q.parts[i].match(l) == q.parts[i].neg {
			return false
		}
	}
	return true
}

func (p *part) match(l *run.LogLine) bool {
	switch {
	case p.levels != nil:
		return p.levels[l.Level]
	case p.trace != "":
		return l.TraceID != "" && strings.HasPrefix(l.TraceID, p.trace)
	case p.source != "":
		src := strings.ToLower(l.Source)
		if strings.ContainsAny(p.source, "*?[") {
			ok, _ := path.Match(p.source, src)
			return ok
		}
		return src == p.source
	case p.re != nil:
		if p.re.MatchString(l.Message) {
			return true
		}
		for _, v := range l.Attrs {
			if p.re.MatchString(v) {
				return true
			}
		}
		return false
	default:
		if containsFold(l.Message, p.text) {
			return true
		}
		for _, v := range l.Attrs {
			if containsFold(v, p.text) {
				return true
			}
		}
		return false
	}
}

// containsFold is a case-insensitive strings.Contains; sub is lower-case.
// ASCII needles avoid allocating.
func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i < len(sub); i++ {
		if sub[i] >= utf8.RuneSelf {
			return strings.Contains(strings.ToLower(s), sub)
		}
	}
	n := len(sub)
	c0 := sub[0]
	c1 := c0
	if 'a' <= c0 && c0 <= 'z' {
		c1 = c0 - 'a' + 'A'
	}
	for i := 0; i+n <= len(s); i++ {
		if s[i] != c0 && s[i] != c1 {
			continue
		}
		ok := true
		for j := 1; j < n; j++ {
			c := s[i+j]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
