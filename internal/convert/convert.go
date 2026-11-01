// Package convert turns recordings and API descriptions (HAR files, curl
// commands, OpenAPI specs) into lt scenario files.
package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/textproto"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Input formats.
const (
	FormatHAR     = "har"
	FormatCurl    = "curl"
	FormatOpenAPI = "openapi"
)

// Options control a conversion.
type Options struct {
	Name     string   // scenario name; default from the input file name
	Rate     float64  // iterations per second, default 1
	Duration string   // default 30s
	All      bool     // keep static assets (HAR) and deprecated operations (OpenAPI)
	Hosts    []string // HAR/curl: keep only these hosts
	NoSleep  bool     // HAR: don't turn pauses into sleep steps
	BaseURL  string   // value for vars.baseUrl
}

// KV is an ordered key/value pair.
type KV struct {
	Key   string
	Value any // string, number, bool or *yaml.Node
}

// Step is one flow step: an HTTP request or a sleep.
type Step struct {
	Sleep   string // when set, the step is "sleep: <Sleep>"
	Comment string // optional comment above the step

	Name    string
	Method  string
	URL     string
	Headers []KV
	Query   []KV
	JSON    *yaml.Node
	Form    []KV
	Body    string
	Status  any // check.status; nil = none
}

// Scenario is a generated scenario file with one arrival-rate scenario.
type Scenario struct {
	Name     string
	Comments []string // header lines, without "# "
	Vars     []KV
	Insecure bool
	Cookies  bool
	Rate     float64
	Duration string
	Steps    []Step
}

// Convert reads input in the given format ("" = detect) and returns scenario YAML.
// fileName is used for detection and the default scenario name.
func Convert(format, fileName string, data []byte, opt Options) ([]byte, error) {
	if format == "" {
		var err error
		if format, err = Detect(fileName, data); err != nil {
			return nil, err
		}
	}
	if opt.Name == "" {
		opt.Name = nameFromFile(fileName)
	}
	var (
		s   *Scenario
		err error
	)
	switch format {
	case FormatHAR:
		s, err = HAR(data, opt)
	case FormatCurl:
		s, err = Curl(data, opt)
	case FormatOpenAPI:
		s, err = OpenAPI(data, opt)
	default:
		return nil, fmt.Errorf("unknown input format %q (use har, curl or openapi)", format)
	}
	if err != nil {
		return nil, err
	}
	return s.YAML()
}

// Detect guesses the input format from the file name and content.
func Detect(fileName string, data []byte) (string, error) {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".har":
		return FormatHAR, nil
	case ".sh", ".curl":
		return FormatCurl, nil
	}
	t := bytes.TrimSpace(data)
	if bytes.HasPrefix(t, []byte("curl ")) || bytes.HasPrefix(t, []byte("#")) && bytes.Contains(t, []byte("curl ")) {
		return FormatCurl, nil
	}
	var top map[string]any
	if yaml.Unmarshal(t, &top) == nil {
		if lg, ok := top["log"].(map[string]any); ok && lg["entries"] != nil {
			return FormatHAR, nil
		}
		if top["openapi"] != nil || top["swagger"] != nil {
			return FormatOpenAPI, nil
		}
	}
	if bytes.Contains(t, []byte("curl ")) {
		return FormatCurl, nil
	}
	return "", fmt.Errorf("can't tell the input format; use --from har|curl|openapi")
}

func nameFromFile(p string) string {
	if p == "" || p == "-" {
		return "converted"
	}
	b := filepath.Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

func newScenario(opt Options) *Scenario {
	s := &Scenario{Name: opt.Name, Rate: opt.Rate, Duration: opt.Duration}
	if s.Name == "" {
		s.Name = "converted"
	}
	if s.Rate <= 0 {
		s.Rate = 1
	}
	if s.Duration == "" {
		s.Duration = "30s"
	}
	return s
}

// ---- recorded requests (HAR, curl) ----

// request is a concrete recorded request with an absolute URL.
type request struct {
	Method      string
	URL         *url.URL
	Headers     []KV // string values, as recorded
	Body        string
	FormParams  []KV // HAR postData.params for form bodies, if given
	Status      int  // recorded response status, 0 if unknown
	Comment     string
	contentType string
}

// dropHeaders are headers that are noise in a load test or set by the client.
var dropHeaders = map[string]bool{
	"user-agent": true, "accept-encoding": true, "accept-language": true, "content-length": true,
	"connection": true, "host": true, "origin": true, "referer": true, "cache-control": true,
	"pragma": true, "priority": true, "dnt": true, "upgrade-insecure-requests": true, "te": true,
	"keep-alive": true, "if-none-match": true, "if-modified-since": true, "x-client-data": true,
	"purpose": true, "sec-purpose": true,
}

func keepHeader(name, value string) bool {
	n := strings.ToLower(name)
	switch {
	case strings.HasPrefix(n, ":"), strings.HasPrefix(n, "sec-"), dropHeaders[n]:
		return false
	case n == "accept" && value == "*/*":
		return false
	}
	return true
}

func headerValue(h []KV, name string) string {
	for _, kv := range h {
		if strings.EqualFold(kv.Key, name) {
			return fmt.Sprint(kv.Value)
		}
	}
	return ""
}

// buildSteps turns recorded requests into http steps against {{ baseUrl }}.
// keepCookies keeps Cookie headers (curl); HAR relies on the cookie jar instead.
func buildSteps(s *Scenario, reqs []request, keepCookies bool, baseURL string) {
	// The most common origin becomes baseUrl.
	count := map[string]int{}
	var origins []string
	for _, r := range reqs {
		o := r.URL.Scheme + "://" + r.URL.Host
		if count[o] == 0 {
			origins = append(origins, o)
		}
		count[o]++
	}
	base := ""
	for _, o := range origins {
		if base == "" || count[o] > count[base] {
			base = o
		}
	}
	if base != "" {
		v := base
		if baseURL != "" {
			v = strings.TrimSuffix(baseURL, "/")
		}
		s.Vars = append(s.Vars, KV{"baseUrl", v})
	}
	seen := map[string]int{}
	for _, r := range reqs {
		st := Step{Method: r.Method, Comment: r.Comment}
		u := *r.URL
		u.Fragment = ""
		if q, ok := splitQuery(u.RawQuery); ok {
			st.Query = q
			u.RawQuery = ""
		}
		if u.Path == "" {
			u.Path = "/"
		}
		if u.Scheme+"://"+u.Host == base {
			st.URL = "{{ baseUrl }}" + strings.TrimPrefix(u.String(), base)
		} else {
			st.URL = u.String()
		}
		ct := r.contentType
		if ct == "" {
			ct = headerValue(r.Headers, "Content-Type")
		}
		bodyKind := setBody(&st, r, ct)
		for _, h := range r.Headers {
			v := fmt.Sprint(h.Value)
			lk := strings.ToLower(h.Key)
			if !keepHeader(h.Key, v) || lk == "cookie" && !keepCookies {
				continue
			}
			if lk == "content-type" && (bodyKind == "json" && isDefaultJSON(v) || bodyKind == "form") {
				continue // lt sets these itself
			}
			if lk == "content-type" && bodyKind == "" {
				continue // no body
			}
			st.Headers = appendHeader(st.Headers, textproto.CanonicalMIMEHeaderKey(h.Key), v)
		}
		if r.Status == 304 {
			st.Status = 200 // conditional headers are dropped
		} else if r.Status > 0 {
			st.Status = r.Status
		}
		def := st.Method + " " + pathOnly(st.URL)
		if st.Method == "" {
			def = "GET " + pathOnly(st.URL)
		}
		seen[def]++
		if n := seen[def]; n > 1 {
			st.Name = fmt.Sprintf("%s #%d", def, n)
		}
		if st.Method == "GET" {
			st.Method = "" // lt's default
		}
		s.Steps = append(s.Steps, st)
	}
}

func isDefaultJSON(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.EqualFold(strings.TrimSpace(mt), "application/json")
}

func appendHeader(h []KV, k, v string) []KV {
	for i := range h {
		if strings.EqualFold(h[i].Key, k) {
			h[i].Value = v
			return h
		}
	}
	return append(h, KV{k, v})
}

// setBody picks json, form or body for the request and reports which.
func setBody(st *Step, r request, ct string) string {
	if r.Body == "" && len(r.FormParams) == 0 {
		return ""
	}
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	if strings.Contains(mt, "json") {
		if n, err := jsonNode(r.Body); err == nil {
			st.JSON = n
			return "json"
		}
	}
	if mt == "application/x-www-form-urlencoded" {
		params := r.FormParams
		ok := len(params) > 0
		if !ok {
			params, ok = splitQuery(r.Body)
		}
		if ok && uniqueKeys(params) {
			st.Form = params
			return "form"
		}
	}
	body := r.Body
	if body == "" { // params only
		v := url.Values{}
		for _, p := range r.FormParams {
			v.Add(p.Key, fmt.Sprint(p.Value))
		}
		body = v.Encode()
	}
	st.Body = body
	return "body"
}

func uniqueKeys(kv []KV) bool {
	seen := map[string]bool{}
	for _, p := range kv {
		if seen[p.Key] {
			return false
		}
		seen[p.Key] = true
	}
	return true
}

// splitQuery parses a query string in order. It fails on repeated keys or
// bad escapes, since lt's query map can't express those.
func splitQuery(raw string) ([]KV, bool) {
	if raw == "" {
		return nil, false
	}
	var out []KV
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k, err1 := url.QueryUnescape(k)
		v, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil || k == "" {
			return nil, false
		}
		out = append(out, KV{k, v})
	}
	if len(out) == 0 || !uniqueKeys(out) {
		return nil, false
	}
	return out, true
}

func pathOnly(u string) string {
	if strings.HasPrefix(u, "{{ baseUrl }}") {
		u = strings.TrimPrefix(u, "{{ baseUrl }}")
	} else if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if j := strings.IndexByte(u, '/'); j >= 0 {
			u = u[j:]
		} else {
			u = "/"
		}
	}
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	if u == "" {
		u = "/"
	}
	return u
}

func hostAllowed(u *url.URL, hosts []string) bool {
	if len(hosts) == 0 {
		return true
	}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == strings.ToLower(u.Host) || h == strings.ToLower(u.Hostname()) {
			return true
		}
	}
	return false
}

// ---- YAML nodes ----

// jsonNode parses JSON into an ordered YAML node.
func jsonNode(s string) (*yaml.Node, error) {
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("invalid JSON")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(s), &doc); err == nil && len(doc.Content) == 1 {
		n := doc.Content[0]
		resetStyle(n)
		return n, nil
	}
	// yaml.v3 rejects a few valid JSON texts; fall back to sorted keys.
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return valueNode(v), nil
}

func resetStyle(n *yaml.Node) {
	n.Style = 0
	n.Line, n.Column = 0, 0
	for _, c := range n.Content {
		resetStyle(c)
	}
}

func scalar(tag, v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v}
}

func str(s string) *yaml.Node {
	n := scalar("!!str", s)
	if strings.Contains(s, "{{") {
		n.Style = yaml.DoubleQuotedStyle
	} else if strings.Contains(s, "\n") {
		n.Style = yaml.LiteralStyle
	}
	return n
}

func mapping(kv ...*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: kv}
}

func valueNode(v any) *yaml.Node {
	switch x := v.(type) {
	case *yaml.Node:
		return x
	case nil:
		return scalar("!!null", "null")
	case string:
		return str(x)
	case bool:
		return scalar("!!bool", strconv.FormatBool(x))
	case int:
		return scalar("!!int", strconv.Itoa(x))
	case int64:
		return scalar("!!int", strconv.FormatInt(x, 10))
	case float64:
		if x == float64(int64(x)) {
			return scalar("!!int", strconv.FormatInt(int64(x), 10))
		}
		return scalar("!!float", strconv.FormatFloat(x, 'g', -1, 64))
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range x {
			n.Content = append(n.Content, valueNode(e))
		}
		return n
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := mapping()
		for _, k := range keys {
			n.Content = append(n.Content, str(k), valueNode(x[k]))
		}
		return n
	case []KV:
		return kvNode(x)
	}
	return str(fmt.Sprint(v))
}

func kvNode(kv []KV) *yaml.Node {
	n := mapping()
	for _, p := range kv {
		n.Content = append(n.Content, str(p.Key), valueNode(p.Value))
	}
	return n
}

// compact uses flow style for short maps and lists of scalars.
func compact(n *yaml.Node) *yaml.Node {
	for _, c := range n.Content {
		compact(c)
	}
	if n.Kind != yaml.MappingNode && n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		return n
	}
	width := 0
	for _, c := range n.Content {
		if c.Kind != yaml.ScalarNode || c.Style == yaml.LiteralStyle {
			return n
		}
		width += len(c.Value) + 4
	}
	if width <= 70 {
		n.Style = yaml.FlowStyle
	}
	return n
}

// YAML renders the scenario file.
func (s *Scenario) YAML() ([]byte, error) {
	root := mapping(str("name"), str(s.Name))
	if len(s.Vars) > 0 {
		root.Content = append(root.Content, str("vars"), kvNode(s.Vars))
	}
	if s.Insecure || s.Cookies {
		opts := mapping()
		if s.Insecure {
			opts.Content = append(opts.Content, str("insecure"), scalar("!!bool", "true"))
		}
		if s.Cookies {
			opts.Content = append(opts.Content, str("cookies"), scalar("!!bool", "true"))
		}
		root.Content = append(root.Content, str("options"), opts)
	}
	flow := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, st := range s.Steps {
		var item *yaml.Node
		if st.Sleep != "" {
			item = mapping(str("sleep"), str(st.Sleep))
		} else {
			item = mapping(str("http"), stepNode(st))
		}
		if st.Comment != "" {
			item.HeadComment = commentLines(st.Comment)
		}
		flow.Content = append(flow.Content, item)
	}
	main := mapping(
		str("rate"), valueNode(s.Rate),
		str("duration"), str(s.Duration),
		str("flow"), flow,
	)
	root.Content = append(root.Content, str("scenarios"), mapping(str("main"), main))

	var buf bytes.Buffer
	for _, c := range s.Comments {
		if c == "" {
			buf.WriteString("#\n")
		} else {
			buf.WriteString("# " + c + "\n")
		}
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return spaceTopLevel(buf.Bytes()), nil
}

func commentLines(c string) string {
	lines := strings.Split(c, "\n")
	for i, l := range lines {
		lines[i] = "# " + l
	}
	return strings.Join(lines, "\n")
}

func stepNode(st Step) *yaml.Node {
	n := mapping()
	add := func(k string, v *yaml.Node) { n.Content = append(n.Content, str(k), v) }
	if st.Name != "" {
		add("name", str(st.Name))
	}
	if st.Method != "" {
		add("method", str(st.Method))
	}
	add("url", str(st.URL))
	if len(st.Headers) > 0 {
		add("headers", kvNode(st.Headers))
	}
	if len(st.Query) > 0 {
		add("query", compact(kvNode(st.Query)))
	}
	switch {
	case st.JSON != nil:
		add("json", compact(st.JSON))
	case st.Form != nil:
		add("form", compact(kvNode(st.Form)))
	case st.Body != "":
		add("body", str(st.Body))
	}
	if st.Status != nil {
		c := mapping(str("status"), valueNode(st.Status))
		c.Style = yaml.FlowStyle
		add("check", c)
	}
	return n
}

// spaceTopLevel puts a blank line before each top-level key after the first.
func spaceTopLevel(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	var out []string
	started := false
	for _, l := range lines {
		top := l != "" && l[0] != ' ' && l[0] != '#' && l[0] != '-'
		if top && started {
			out = append(out, "")
		}
		if top {
			started = true
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}
