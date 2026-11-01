package logs

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ymg4798/lt/internal/run"
)

// Limits on what one record keeps.
const (
	maxMessage   = 4096
	maxStack     = 16 << 10
	maxStackLine = 200
	maxAttrs     = 16
	maxAttrValue = 256
)

// normLevel maps the many level spellings to debug, info, warn, error,
// fatal, or "" when unknown.
func normLevel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "trace", "debug", "dbg", "verbose", "finest", "finer", "fine", "d", "t", "v", "10", "20":
		return "debug"
	case "info", "information", "informational", "notice", "inf", "i", "config", "30":
		return "info"
	case "warn", "warning", "wrn", "w", "40":
		return "warn"
	case "error", "err", "eror", "e", "severe", "50":
		return "error"
	case "fatal", "critical", "crit", "crt", "alert", "emerg", "emergency", "panic", "dpanic", "f", "c", "60":
		return "fatal"
	}
	// "ERROR2" (OTel severity text), "warn:" and the like.
	for _, p := range []struct{ pre, lvl string }{{"trace", "debug"}, {"debug", "debug"}, {"info", "info"}, {"warn", "warn"}, {"error", "error"}, {"fatal", "fatal"}} {
		if strings.HasPrefix(s, p.pre) && len(s) <= len(p.pre)+2 {
			return p.lvl
		}
	}
	return ""
}

// severityLevel maps an OTLP SeverityNumber.
func severityLevel(n int) string {
	switch {
	case n <= 0:
		return ""
	case n <= 8:
		return "debug"
	case n <= 12:
		return "info"
	case n <= 16:
		return "warn"
	case n <= 20:
		return "error"
	}
	return "fatal"
}

// isError reports whether a level counts as an error.
func isError(l string) bool { return l == "error" || l == "fatal" }

// Format is how a source's lines are parsed.
type Format int

// Formats.
const (
	Auto Format = iota // JSON when a line is a JSON object, else logfmt or text
	JSON
	Text // plain text (and logfmt), never JSON
)

// ParseFormat reads the file setting: "", auto, json, text.
func ParseFormat(s string) Format {
	switch strings.ToLower(s) {
	case "json":
		return JSON
	case "text":
		return Text
	}
	return Auto
}

// Parser turns raw lines from one source into records, joining stack
// traces and indented continuation lines into the record before them. It
// is not safe for concurrent use.
type Parser struct {
	Source string
	Format Format
	// Now gives the time for records without a timestamp (default time.Now).
	Now func() time.Time

	pending *record
	inStack bool // pending has a stack under way (Python/Go/Java)
}

type record struct {
	line  run.LogLine
	stack []string
	size  int
}

// Feed parses one line (without its newline) and returns the records it
// completed, usually the previous one.
func (p *Parser) Feed(raw string) []run.LogLine {
	raw = strings.TrimRight(raw, "\r")
	if p.pending != nil && p.Format != JSON && p.continues(raw) {
		p.appendStack(raw)
		return nil
	}
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := p.Flush()
	r := p.parse(raw)
	if r == nil {
		return out
	}
	if r.multi {
		p.pending = &record{line: r.line}
		p.inStack = startsStack(r.line.Message)
		if r.stackLines != nil {
			p.pending.stack = r.stackLines
		}
		return out
	}
	return append(out, r.line)
}

// Flush returns the record waiting for continuation lines, if any.
func (p *Parser) Flush() []run.LogLine {
	if p.pending == nil {
		return nil
	}
	r := p.pending
	p.pending, p.inStack = nil, false
	if len(r.stack) > 0 {
		st := strings.Join(r.stack, "\n")
		if r.line.Attrs == nil {
			r.line.Attrs = map[string]string{}
		}
		if old := r.line.Attrs["stack"]; old != "" {
			st = old + "\n" + st
		}
		r.line.Attrs["stack"] = truncUTF8(st, maxStack)
		if r.line.Level == "" && looksLikeTrace(st) {
			r.line.Level = "error"
		}
	}
	return []run.LogLine{r.line}
}

// Pending reports whether a record is waiting for continuation lines.
func (p *Parser) Pending() bool { return p.pending != nil }

func (p *Parser) appendStack(raw string) {
	r := p.pending
	if len(r.stack) >= maxStackLine || r.size > maxStack {
		return
	}
	if strings.TrimSpace(raw) == "" {
		if !p.inStack {
			return
		}
		raw = ""
	}
	r.stack = append(r.stack, raw)
	r.size += len(raw) + 1
	if startsStack(raw) {
		p.inStack = true
	}
}

var (
	javaException = regexp.MustCompile(`^(?:Exception in thread "[^"]*" )?(?:[a-zA-Z_$][\w$]*\.)+[A-Z][\w$]*(?:Exception|Error|Throwable|Fault)\b(?::|$)`)
	pyException   = regexp.MustCompile(`^[A-Za-z_][\w.]*(?:Error|Exception|Warning|Exit|Interrupt|Iteration)(?::|$)`)
	goFrame       = regexp.MustCompile(`^(?:created by )?[\w./*%()-]+(?:\.[\w*%()-]+)*\(.*\)(?: in goroutine \d+)?$`)
)

// continues reports whether raw belongs to the pending record.
func (p *Parser) continues(raw string) bool {
	if raw == "" || strings.TrimSpace(raw) == "" {
		return p.inStack // blank lines inside a Go panic dump
	}
	switch raw[0] {
	case ' ', '\t':
		return true
	}
	switch {
	case strings.HasPrefix(raw, "Caused by:"), strings.HasPrefix(raw, "Suppressed:"),
		strings.HasPrefix(raw, "Traceback (most recent call last)"),
		strings.HasPrefix(raw, "During handling of the above exception"),
		strings.HasPrefix(raw, "The above exception was the direct cause"),
		strings.HasPrefix(raw, "goroutine "), strings.HasPrefix(raw, "..."):
		return true
	case raw[0] == '{' || raw[0] == '[' && len(raw) > 1 && raw[1] >= '0' && raw[1] <= '9':
		return false // a JSON line, a bracketed timestamp
	case javaException.MatchString(raw):
		return true
	case p.inStack && pyException.MatchString(raw):
		return true
	case p.inStack && goFrame.MatchString(raw):
		return true
	}
	return false
}

func startsStack(s string) bool {
	return strings.HasPrefix(s, "Traceback (most recent call last)") ||
		strings.HasPrefix(s, "goroutine ") || strings.HasPrefix(s, "panic: ") ||
		strings.HasPrefix(s, "fatal error: ")
}

func looksLikeTrace(st string) bool {
	return strings.Contains(st, "Traceback (most recent call last)") ||
		strings.Contains(st, "\tat ") || strings.Contains(st, "goroutine ") ||
		strings.Contains(st, "Exception") || strings.Contains(st, "Error")
}

type parsed struct {
	line       run.LogLine
	multi      bool // may take continuation lines (text records)
	stackLines []string
}

func (p *Parser) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Parser) parse(raw string) *parsed {
	if p.Format != Text {
		if t := strings.TrimLeft(raw, " \t"); len(t) > 1 && t[0] == '{' {
			if l, ok := parseJSONLine(t); ok {
				p.fill(&l)
				return &parsed{line: l}
			}
		}
	}
	if l, ok := parseLogfmt(raw); ok {
		p.fill(&l)
		return &parsed{line: l, multi: p.Format != JSON}
	}
	l := parseText(raw)
	p.fill(&l)
	return &parsed{line: l, multi: p.Format != JSON}
}

func (p *Parser) fill(l *run.LogLine) {
	if l.Source == "" {
		l.Source = p.Source
	}
	if l.Time.IsZero() {
		l.Time = p.now()
	}
	l.TraceID = strings.ToLower(l.TraceID)
	l.Message = truncUTF8(l.Message, maxMessage)
}

// ---- JSON ----

var (
	timeKeys  = []string{"time", "ts", "timestamp", "@timestamp", "datetime", "date", "t", "eventTime"}
	levelKeys = []string{"level", "lvl", "severity", "log.level", "loglevel", "levelname", "severity_text", "severityText", "@level", "le"}
	msgKeys   = []string{"msg", "message", "@message", "event", "body", "text", "log"}
	traceKeys = []string{"trace_id", "traceId", "traceID", "trace.id", "traceid", "dd.trace_id", "logging.googleapis.com/trace", "otelTraceID"}
	errKeys   = []string{"error.message", "error", "err", "exception.message", "exception", "exc"}
	stackKeys = []string{"stack", "stacktrace", "stack_trace", "exception.stacktrace", "error.stack_trace", "error.stack", "exc_info", "trace"}
)

func parseJSONLine(s string) (run.LogLine, bool) {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return run.LogLine{}, false
	}
	flat := map[string]any{}
	flatten("", m, flat, 0)
	var l run.LogLine
	used := map[string]bool{}
	take := func(keys []string) (string, any) {
		for _, k := range keys {
			if v, ok := flat[k]; ok && v != nil {
				used[k] = true
				return k, v
			}
		}
		return "", nil
	}
	if _, v := take(timeKeys); v != nil {
		l.Time = jsonTime(v)
	}
	if _, v := take(levelKeys); v != nil {
		l.Level = normLevel(textOf(v))
	}
	if _, v := take(msgKeys); v != nil {
		l.Message = textOf(v)
	}
	if _, v := take(traceKeys); v != nil {
		l.TraceID = traceOf(textOf(v))
	}
	if l.TraceID == "" {
		if v, ok := flat["traceparent"]; ok {
			used["traceparent"] = true
			l.TraceID = traceOf(textOf(v))
		}
	}
	attrs := map[string]string{}
	if _, v := take(errKeys); v != nil {
		attrs["error"] = truncUTF8(textOf(v), maxAttrValue*4)
		if l.Message == "" {
			l.Message = textOf(v)
		}
	}
	for _, k := range stackKeys {
		if v, ok := flat[k]; ok && v != nil {
			if s := textOf(v); strings.Contains(s, "\n") || k != "trace" {
				used[k] = true
				attrs["stack"] = truncUTF8(s, maxStack)
				break
			}
		}
	}
	if l.Level == "" && (attrs["stack"] != "" || attrs["error"] != "") {
		l.Level = "error"
	}
	if l.Message == "" {
		l.Message = s
	}
	// The remaining fields, in a stable order, capped.
	keys := make([]string, 0, len(flat))
	for k := range flat {
		if !used[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(attrs) >= maxAttrs {
			break
		}
		attrs[k] = truncUTF8(textOf(flat[k]), maxAttrValue)
	}
	if len(attrs) > 0 {
		l.Attrs = attrs
	}
	return l, true
}

// flatten turns nested objects into dotted keys ({"trace":{"id":1}} → trace.id).
func flatten(prefix string, m map[string]any, out map[string]any, depth int) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok && depth < 3 {
			flatten(key, sub, out, depth+1)
			continue
		}
		out[key] = v
	}
}

func textOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	case []any:
		// exc_info and some loggers write stacks as a list of lines.
		allStr := true
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				allStr = false
				break
			}
			parts = append(parts, s)
		}
		if allStr {
			return strings.Join(parts, "\n")
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonTime(v any) time.Time {
	switch x := v.(type) {
	case string:
		if t, ok := parseTimestamp(x); ok {
			return t
		}
		if n, err := strconv.ParseFloat(x, 64); err == nil {
			return unixAny(n)
		}
	case json.Number:
		if n, err := x.Float64(); err == nil {
			return unixAny(n)
		}
	}
	return time.Time{}
}

// unixAny reads seconds, milliseconds, microseconds or nanoseconds by size.
func unixAny(n float64) time.Time {
	switch {
	case n <= 0 || math.IsNaN(n) || math.IsInf(n, 0):
		return time.Time{}
	case n < 1e11:
		s, f := math.Modf(n)
		return time.Unix(int64(s), int64(f*1e9))
	case n < 1e14:
		return time.UnixMilli(int64(n))
	case n < 1e17:
		return time.UnixMicro(int64(n))
	}
	return time.Unix(0, int64(n))
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999 -0700",
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999",
	"2006/01/02 15:04:05.999999999",
	time.RFC1123Z, time.RFC1123,
	"02/Jan/2006:15:04:05 -0700",
}

// parseTimestamp reads the usual timestamp spellings (a comma before the
// fraction too, as Python and log4j write it). Zoneless times are local.
func parseTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 8 {
		return time.Time{}, false
	}
	if i := strings.IndexByte(s, ','); i == 19 {
		s = s[:i] + "." + s[i+1:]
	}
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// traceOf extracts a 32-hex trace ID from an ID or a traceparent.
func traceOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "/traces/"); i >= 0 { // GCP: projects/p/traces/<id>
		s = s[i+len("/traces/"):]
	}
	if len(s) == 55 && s[2] == '-' && s[35] == '-' { // traceparent
		s = s[3:35]
	}
	if isHex(s) && (len(s) == 32 || len(s) == 16) {
		return strings.ToLower(s)
	}
	if n, err := strconv.ParseUint(s, 10, 64); err == nil && n > 0 { // Datadog decimal IDs
		return strings.ToLower(strconv.FormatUint(n, 16))
	}
	return strings.ToLower(s)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// ---- logfmt ----

// parseLogfmt reads key=value pairs; it needs the line to start with a
// pair and to have at least two, or a msg/level key, so prose is left alone.
func parseLogfmt(s string) (run.LogLine, bool) {
	pairs, ok := logfmtPairs(s)
	if !ok {
		return run.LogLine{}, false
	}
	known := 0
	for _, kv := range pairs {
		switch kv[0] {
		case "msg", "message", "level", "lvl", "time", "ts":
			known++
		}
	}
	if len(pairs) < 2 || known == 0 {
		return run.LogLine{}, false
	}
	var l run.LogLine
	attrs := map[string]string{}
	get := func(keys ...string) string {
		for _, k := range keys {
			for i := range pairs {
				if pairs[i][0] == k && pairs[i][2] == "" {
					pairs[i][2] = "used"
					return pairs[i][1]
				}
			}
		}
		return ""
	}
	if v := get("time", "ts", "timestamp", "t"); v != "" {
		l.Time, _ = parseTimestamp(v)
		if l.Time.IsZero() {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				l.Time = unixAny(n)
			}
		}
	}
	l.Level = normLevel(get("level", "lvl", "severity"))
	l.Message = get("msg", "message")
	if v := get("trace_id", "traceId", "traceID", "trace.id", "traceparent", "trace"); v != "" {
		l.TraceID = traceOf(v)
	}
	if v := get("error", "err"); v != "" {
		attrs["error"] = truncUTF8(v, maxAttrValue*4)
		if l.Level == "" {
			l.Level = "error"
		}
	}
	if v := get("stack", "stacktrace"); v != "" {
		attrs["stack"] = truncUTF8(strings.ReplaceAll(v, `\n`, "\n"), maxStack)
	}
	for _, kv := range pairs {
		if kv[2] == "" && len(attrs) < maxAttrs {
			attrs[kv[0]] = truncUTF8(kv[1], maxAttrValue)
		}
	}
	if l.Message == "" {
		l.Message = s
	}
	if len(attrs) > 0 {
		l.Attrs = attrs
	}
	return l, true
}

// logfmtPairs splits a logfmt line into [key, value, used] triples.
func logfmtPairs(s string) ([][3]string, bool) {
	var out [][3]string
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		k := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' {
			i++
		}
		if i >= len(s) || s[i] != '=' || i == k {
			return out, false
		}
		key := s[k:i]
		i++
		var val string
		if i < len(s) && s[i] == '"' {
			j := i + 1
			var b strings.Builder
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
					switch s[j] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(s[j])
					}
				} else {
					b.WriteByte(s[j])
				}
				j++
			}
			val = b.String()
			i = j + 1
		} else {
			j := i
			for j < len(s) && s[j] != ' ' {
				j++
			}
			val = s[i:j]
			i = j
		}
		out = append(out, [3]string{key, val, ""})
	}
	return out, len(out) > 0
}

// ---- plain text ----

var (
	leadingTime = regexp.MustCompile(`^\[?(\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?(?: ?(?:Z|[+-]\d{2}:?\d{2}))?)\]?\s*`)
	clfTime     = regexp.MustCompile(`\[(\d{2}/[A-Z][a-z]{2}/\d{4}:\d{2}:\d{2}:\d{2} [+-]\d{4})\]`)
	glogPrefix  = regexp.MustCompile(`^([IWEF])(\d{4} \d{2}:\d{2}:\d{2}\.\d{6})\s+\d+\s+[\w.-]+:\d+\]\s*`)
)

func parseText(raw string) run.LogLine {
	l := run.LogLine{Message: raw}
	rest := raw
	if m := glogPrefix.FindStringSubmatch(raw); m != nil {
		l.Level = normLevel(m[1])
		if t, err := time.ParseInLocation("0102 15:04:05.000000", m[2], time.Local); err == nil {
			now := time.Now()
			l.Time = t.AddDate(now.Year(), 0, 0)
		}
		l.Message = raw[len(m[0]):]
	} else {
		if m := leadingTime.FindStringSubmatch(raw); m != nil {
			if t, ok := parseTimestamp(strings.Replace(m[1], " ", "T", 1)); ok {
				l.Time = t
			} else if t, ok := parseTimestamp(m[1]); ok {
				l.Time = t
			}
			rest = raw[len(m[0]):]
			l.Message = rest
		} else if m := clfTime.FindStringSubmatch(raw); m != nil {
			l.Time, _ = parseTimestamp(m[1])
		}
		l.Level = textLevel(rest)
	}
	if strings.HasPrefix(raw, "panic: ") || strings.HasPrefix(raw, "fatal error: ") {
		l.Level = "fatal"
	}
	if l.Level == "" && (javaException.MatchString(raw) || strings.HasPrefix(raw, "Traceback (most recent call last)")) {
		l.Level = "error"
	}
	l.TraceID = textTrace(raw)
	return l
}

// textLevel finds a level word near the start of a line: an upper-case
// word (ERROR, WARN), any case in brackets ([error], <warn>), or a
// "level: x" pair.
func textLevel(s string) string {
	head := s
	if len(head) > 120 {
		head = head[:120]
	}
	i := 0
	for i < len(head) {
		c := head[i]
		if !isLetter(c) {
			i++
			continue
		}
		j := i
		for j < len(head) && isLetter(head[j]) {
			j++
		}
		w := head[i:j]
		before := byte(' ')
		if i > 0 {
			before = head[i-1]
		}
		after := byte(' ')
		if j < len(head) {
			after = head[j]
		}
		bracketed := (before == '[' || before == '<' || before == '(' || before == '|') && (after == ']' || after == '>' || after == ')' || after == '|')
		upper := strings.ToUpper(w) == w && len(w) >= 3
		if (upper || bracketed) && (before == ' ' || before == '[' || before == '<' || before == '(' || before == '|' || before == '\t' || i == 0) {
			if lvl := wordLevel(w); lvl != "" {
				return lvl
			}
		}
		if strings.EqualFold(w, "level") || strings.EqualFold(w, "severity") {
			k := j
			for k < len(head) && (head[k] == '=' || head[k] == ':' || head[k] == ' ' || head[k] == '"') {
				k++
			}
			e := k
			for e < len(head) && isLetter(head[e]) {
				e++
			}
			if lvl := normLevel(head[k:e]); lvl != "" {
				return lvl
			}
		}
		i = j
	}
	return ""
}

func wordLevel(w string) string {
	switch strings.ToUpper(w) {
	case "TRACE", "DEBUG", "DBG", "INFO", "INF", "NOTICE", "WARN", "WARNING", "WRN", "ERROR", "ERR", "SEVERE", "FATAL", "CRITICAL", "CRIT", "PANIC", "EMERG", "ALERT":
		return normLevel(w)
	}
	return ""
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// textTrace finds a 32-hex trace ID with "trace" shortly before it, or a
// traceparent.
func textTrace(s string) string {
	low := ""
	for i := 0; i+32 <= len(s); i++ {
		if !isHexByte(s[i]) || (i > 0 && isHexByte(s[i-1])) {
			continue
		}
		j := i
		for j < len(s) && isHexByte(s[j]) {
			j++
		}
		n := j - i
		if n != 32 {
			i = j
			continue
		}
		if i >= 3 && s[i-1] == '-' && s[i-3:i-1] == "00" { // traceparent 00-<id>-<span>-01
			return strings.ToLower(s[i:j])
		}
		if low == "" {
			low = strings.ToLower(s)
		}
		from := max(0, i-24)
		if strings.Contains(low[from:i], "trace") {
			return strings.ToLower(s[i:j])
		}
		i = j
	}
	return ""
}

func isHexByte(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// splitLines is bytes.Split on '\n' that also reports the unfinished tail.
func splitLines(b []byte) (lines [][]byte, rest []byte) {
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return lines, b
		}
		lines = append(lines, b[:i])
		b = b[i+1:]
	}
}
