package apm

import (
	"net/url"
	"strconv"
	"strings"
)

// Kinds as shown in reports (run.Span.Kind, run.OpStat.Kind).
const (
	KindServer   = "server"
	KindDB       = "db"
	KindHTTP     = "http"
	KindInternal = "internal"
)

// mapKind turns an OTel span kind and its attributes into a report kind.
func mapKind(s *rawSpan) string {
	switch s.kind {
	case kindServer, kindConsumer:
		return KindServer
	case kindClient, kindProducer:
		if s.attrs["db.system"] != "" || s.attrs["db.statement"] != "" || s.attrs["db.query.text"] != "" {
			return KindDB
		}
		return KindHTTP
	}
	return KindInternal
}

// httpStatus is the span's HTTP status code, or 0.
func httpStatus(attrs map[string]string) int {
	v := attrs["http.response.status_code"]
	if v == "" {
		v = attrs["http.status_code"]
	}
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func httpMethod(attrs map[string]string) string {
	if m := attrs["http.request.method"]; m != "" {
		return m
	}
	return attrs["http.method"]
}

func statement(attrs map[string]string) string {
	if q := attrs["db.query.text"]; q != "" {
		return q
	}
	return attrs["db.statement"]
}

// opName is the operation a span is counted under.
func opName(kind, name string, attrs map[string]string) string {
	switch kind {
	case KindServer:
		if route, m := attrs["http.route"], httpMethod(attrs); route != "" && m != "" {
			return m + " " + route
		}
	case KindDB:
		if q := statement(attrs); q != "" {
			return NormalizeSQL(q)
		}
	case KindHTTP:
		m := httpMethod(attrs)
		host := attrs["server.address"]
		if host == "" {
			host = attrs["net.peer.name"]
		}
		if host == "" {
			if u, err := url.Parse(first(attrs["url.full"], attrs["http.url"])); err == nil {
				host = u.Hostname()
			}
		}
		if host == "" {
			host = attrs["peer.service"]
		}
		if m != "" && host != "" {
			if p := first(attrs["server.port"], attrs["net.peer.port"]); p != "" && p != "80" && p != "443" {
				host += ":" + p
			}
			return m + " " + host
		}
	}
	if name == "" {
		return "(unnamed)"
	}
	return name
}

const maxOpName = 200

// NormalizeSQL turns a statement into its shape: comments dropped, string
// and number literals replaced by ?, lists of ? collapsed, whitespace
// collapsed and the result cut to 200 characters.
func NormalizeSQL(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	space := false
	emit := func(c byte) {
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
	}
	prevIdent := false // previous byte was part of an identifier
	for i := 0; i < len(q) && b.Len() < maxOpName*2; i++ {
		c := q[i]
		switch {
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			for i < len(q) && q[i] != '\n' {
				i++
			}
			space = true
			prevIdent = false
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			j := strings.Index(q[i+2:], "*/")
			if j < 0 {
				i = len(q)
			} else {
				i += j + 3
			}
			space = true
			prevIdent = false
		case c == '\'':
			for i++; i < len(q); i++ {
				if q[i] == '\'' {
					if i+1 < len(q) && q[i+1] == '\'' {
						i++
						continue
					}
					break
				}
				if q[i] == '\\' {
					i++
				}
			}
			emit('?')
			prevIdent = false
		case c >= '0' && c <= '9' && !prevIdent:
			for i+1 < len(q) && (isDigit(q[i+1]) || q[i+1] == '.' || q[i+1] == 'e' || q[i+1] == 'E' || q[i+1] == 'x' || isHex(q[i+1])) {
				i++
			}
			emit('?')
			prevIdent = false
		case c == '$' && i+1 < len(q) && isDigit(q[i+1]): // $1 placeholders
			for i+1 < len(q) && isDigit(q[i+1]) {
				i++
			}
			emit('?')
			prevIdent = false
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			space = true
			prevIdent = false
		default:
			emit(c)
			prevIdent = isIdent(c)
		}
	}
	s := collapseLists(b.String())
	if len(s) > maxOpName {
		s = truncUTF8(s, maxOpName-3) + "..."
	}
	return s
}

// collapseLists turns "(?, ?, ?)" into "(?)" so IN lists of any length match.
func collapseLists(s string) string {
	if !strings.Contains(s, "?,") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteByte(s[i])
		if s[i] != '?' {
			continue
		}
		j := i + 1
		for {
			k := j
			if k < len(s) && s[k] == ' ' {
				k++
			}
			if k < len(s) && s[k] == ',' {
				k++
			} else {
				break
			}
			if k < len(s) && s[k] == ' ' {
				k++
			}
			if k < len(s) && s[k] == '?' {
				j = k + 1
				continue
			}
			break
		}
		i = j - 1
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isHex(c byte) bool   { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }
func isIdent(c byte) bool {
	return c == '_' || isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
