package convert

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// spec wraps the document root for $ref lookups.
type spec struct {
	root *yaml.Node
}

// get returns the value for key in a mapping node, or nil.
func get(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func getStr(n *yaml.Node, key string) string {
	if v := get(n, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// deref follows local $refs (with a hop limit).
func (sp *spec) deref(n *yaml.Node) *yaml.Node {
	for hop := 0; n != nil && hop < 16; hop++ {
		ref := getStr(n, "$ref")
		if ref == "" {
			return n
		}
		n = sp.lookup(ref)
	}
	return n
}

func (sp *spec) lookup(ref string) *yaml.Node {
	if !strings.HasPrefix(ref, "#/") {
		return nil // remote refs are not followed
	}
	n := sp.root
	for _, p := range strings.Split(ref[2:], "/") {
		p, _ = url.PathUnescape(p)
		p = strings.NewReplacer("~1", "/", "~0", "~").Replace(p)
		n = get(n, p)
	}
	return n
}

func clone(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = nil
	for _, x := range n.Content {
		c.Content = append(c.Content, clone(x))
	}
	resetStyle(&c)
	c.HeadComment, c.LineComment, c.FootComment = "", "", ""
	return &c
}

var methodOrder = []string{"get", "post", "put", "patch", "delete", "head", "options", "trace"}

type param struct {
	name, in string
	required bool
	node     *yaml.Node
}

// OpenAPI converts an OpenAPI 3.x or Swagger 2.0 spec (JSON or YAML).
func OpenAPI(data []byte, opt Options) (*Scenario, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("openapi: empty document")
	}
	sp := &spec{root: doc.Content[0]}
	swagger := getStr(sp.root, "swagger") != ""
	if !swagger && getStr(sp.root, "openapi") == "" {
		return nil, fmt.Errorf("openapi: not an OpenAPI or Swagger document")
	}
	s := newScenario(opt)

	base, baseNote := sp.baseURL(swagger)
	if opt.BaseURL != "" {
		base, baseNote = opt.BaseURL, ""
	}
	s.Vars = append(s.Vars, KV{"baseUrl", strings.TrimSuffix(base, "/")})
	varSet := map[string]bool{"baseUrl": true}
	addVar := func(name string, v any) {
		if !varSet[name] {
			varSet[name] = true
			s.Vars = append(s.Vars, KV{name, v})
		}
	}

	var ops []string
	deprecated := 0
	paths := get(sp.root, "paths")
	if paths == nil || paths.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("openapi: no paths")
	}
	globalSec := get(sp.root, "security")
	for i := 0; i+1 < len(paths.Content); i += 2 {
		p := paths.Content[i].Value
		item := sp.deref(paths.Content[i+1])
		for _, m := range methodOrder {
			op := get(item, m)
			if op == nil {
				continue
			}
			if getStr(op, "deprecated") == "true" && !opt.All {
				deprecated++
				continue
			}
			st := Step{Method: strings.ToUpper(m)}
			if st.Method == "GET" {
				st.Method = ""
			}
			params := sp.params(get(item, "parameters"), get(op, "parameters"))

			// Path: {id} -> {{ id }} with a var.
			urlPath := pathParam.ReplaceAllStringFunc(p, func(seg string) string {
				name := seg[1 : len(seg)-1]
				v := varName(name)
				var val any = "1"
				for _, pr := range params {
					if pr.in == "path" && pr.name == name {
						val = sp.paramValue(pr, true)
					}
				}
				addVar(v, val)
				return "{{ " + v + " }}"
			})
			st.URL = "{{ baseUrl }}" + urlPath

			for _, pr := range params {
				if !pr.required {
					continue
				}
				switch pr.in {
				case "query":
					st.Query = append(st.Query, KV{pr.name, sp.paramValue(pr, false)})
				case "header":
					st.Headers = append(st.Headers, KV{pr.name, fmt.Sprint(sp.paramValue(pr, false))})
				}
			}
			sec := get(op, "security")
			if sec == nil {
				sec = globalSec
			}
			for _, h := range sp.securityHeaders(sec, swagger) {
				st.Headers = appendHeader(st.Headers, h.header, h.value)
				addVar(h.varName, "changeme")
			}

			note := sp.body(&st, op, params, swagger)
			st.Status = successStatus(get(op, "responses"))
			if id := getStr(op, "operationId"); id != "" {
				st.Name = id
			}
			desc := strings.ToUpper(m) + " " + p
			if sum := getStr(op, "summary"); sum != "" {
				desc += " — " + oneLine(sum)
			}
			if note != "" {
				desc += "\nnote: " + note
			}
			st.Comment = desc
			ops = append(ops, strings.ToUpper(m)+" "+p)
			s.Steps = append(s.Steps, st)
		}
	}
	if len(s.Steps) == 0 {
		return nil, fmt.Errorf("openapi: no operations (deprecated skipped: %d; use --all)", deprecated)
	}

	title := getStr(get(sp.root, "info"), "title")
	if title != "" {
		title = " (" + oneLine(title) + ")"
	}
	s.Comments = append(s.Comments, fmt.Sprintf("Converted from OpenAPI%s by lt convert: %d operation(s).", title, len(ops)))
	for _, o := range ops {
		s.Comments = append(s.Comments, "  "+o)
	}
	if deprecated > 0 {
		s.Comments = append(s.Comments, fmt.Sprintf("skipped %d deprecated operation(s) (use --all to keep)", deprecated))
	}
	if baseNote != "" {
		s.Comments = append(s.Comments, baseNote)
	}
	s.Comments = append(s.Comments, "Values are examples or placeholders; set real ones with -e name=value, data or extract.")
	return s, nil
}

var pathParam = regexp.MustCompile(`\{[^{}]+\}`)

func varName(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '_'
		}
	}
	return string(b)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func (sp *spec) baseURL(swagger bool) (string, string) {
	const def = "http://localhost:8080"
	if swagger {
		host := getStr(sp.root, "host")
		if host == "" {
			return def + getStr(sp.root, "basePath"), "spec has no host; baseUrl is a guess (override with --base-url or -e baseUrl=...)"
		}
		scheme := "https"
		if sc := get(sp.root, "schemes"); sc != nil && len(sc.Content) > 0 {
			scheme = sc.Content[0].Value
		}
		return scheme + "://" + host + getStr(sp.root, "basePath"), ""
	}
	servers := get(sp.root, "servers")
	if servers == nil || len(servers.Content) == 0 {
		return def, "spec has no servers; baseUrl is a guess (override with --base-url or -e baseUrl=...)"
	}
	srv := servers.Content[0]
	u := getStr(srv, "url")
	vars := get(srv, "variables")
	u = pathParam.ReplaceAllStringFunc(u, func(seg string) string {
		if d := getStr(get(vars, seg[1:len(seg)-1]), "default"); d != "" {
			return d
		}
		return seg
	})
	if !strings.Contains(u, "://") {
		return def + "/" + strings.TrimPrefix(u, "/"), "server URL is relative; baseUrl assumes localhost:8080 (override with --base-url)"
	}
	return u, ""
}

// params merges path-level and operation-level parameters.
func (sp *spec) params(lists ...*yaml.Node) []param {
	var out []param
	for _, l := range lists {
		if l == nil {
			continue
		}
		for _, n := range l.Content {
			n = sp.deref(n)
			pr := param{name: getStr(n, "name"), in: getStr(n, "in"), required: getStr(n, "required") == "true", node: n}
			if pr.in == "path" {
				pr.required = true
			}
			replaced := false
			for i := range out {
				if out[i].name == pr.name && out[i].in == pr.in {
					out[i], replaced = pr, true
				}
			}
			if !replaced {
				out = append(out, pr)
			}
		}
	}
	return out
}

// paramValue returns an example value for a parameter.
func (sp *spec) paramValue(pr param, path bool) any {
	n := pr.node
	if ex := get(n, "example"); ex != nil {
		return scalarValue(clone(ex))
	}
	if exs := sp.deref(get(n, "examples")); exs != nil && len(exs.Content) >= 2 {
		if v := get(sp.deref(exs.Content[1]), "value"); v != nil {
			return scalarValue(clone(v))
		}
	}
	schema := get(n, "schema")
	if schema == nil { // Swagger 2: type etc. live on the parameter
		schema = n
	}
	v := sp.example(schema, 0, nil)
	if path && v.Kind == yaml.ScalarNode && v.Value == "0" {
		return 1 // ids rarely start at 0
	}
	return scalarValue(v)
}

// scalarValue turns a scalar node into a Go value; other nodes stay as nodes.
func scalarValue(n *yaml.Node) any {
	if n.Kind != yaml.ScalarNode {
		return n
	}
	var v any
	if err := n.Decode(&v); err == nil && v != nil {
		switch v.(type) {
		case string, bool, int, int64, float64:
			return v
		}
	}
	return n.Value
}

type secHeader struct{ header, value, varName string }

// securityHeaders returns auth headers for the first security requirement.
func (sp *spec) securityHeaders(sec *yaml.Node, swagger bool) []secHeader {
	if sec == nil || len(sec.Content) == 0 {
		return nil
	}
	defs := get(get(sp.root, "components"), "securitySchemes")
	if swagger {
		defs = get(sp.root, "securityDefinitions")
	}
	var out []secHeader
	bearer := secHeader{"Authorization", "Bearer {{ token }}", "token"}
	req := sec.Content[0] // first alternative
	for i := 0; i+1 < len(req.Content); i += 2 {
		d := sp.deref(get(defs, req.Content[i].Value))
		switch getStr(d, "type") {
		case "http":
			if strings.EqualFold(getStr(d, "scheme"), "bearer") {
				out = append(out, bearer)
			}
		case "oauth2", "openIdConnect":
			out = append(out, bearer)
		case "apiKey":
			if getStr(d, "in") == "header" {
				v := varName(req.Content[i].Value)
				out = append(out, secHeader{getStr(d, "name"), "{{ " + v + " }}", v})
			}
		}
	}
	return out
}

// body sets the request body from the spec and returns a note if it couldn't.
func (sp *spec) body(st *Step, op *yaml.Node, params []param, swagger bool) string {
	if swagger {
		var form []KV
		for _, pr := range params {
			switch pr.in {
			case "body":
				st.JSON = compact(sp.example(get(pr.node, "schema"), 0, nil))
				return ""
			case "formData":
				if pr.required {
					form = append(form, KV{pr.name, sp.paramValue(pr, false)})
				}
			}
		}
		if form != nil {
			st.Form = form
		}
		return ""
	}
	rb := sp.deref(get(op, "requestBody"))
	content := get(rb, "content")
	if content == nil || len(content.Content) < 2 {
		return ""
	}
	// Prefer JSON, then form, then whatever comes first.
	pick := -1
	for _, want := range []string{"json", "x-www-form-urlencoded", ""} {
		for i := 0; i+1 < len(content.Content); i += 2 {
			if strings.Contains(content.Content[i].Value, want) {
				pick = i
				break
			}
		}
		if pick >= 0 {
			break
		}
	}
	ct, media := content.Content[pick].Value, content.Content[pick+1]
	ex := sp.mediaExample(media)
	switch {
	case strings.Contains(ct, "json"):
		st.JSON = ex
		if !isDefaultJSON(ct) {
			st.Headers = appendHeader(st.Headers, "Content-Type", ct)
		}
	case strings.Contains(ct, "x-www-form-urlencoded") && ex.Kind == yaml.MappingNode:
		var form []KV
		for i := 0; i+1 < len(ex.Content); i += 2 {
			if v := ex.Content[i+1]; v.Kind == yaml.ScalarNode {
				form = append(form, KV{ex.Content[i].Value, scalarValue(v)})
			}
		}
		st.Form = form
		if form == nil {
			st.Form = []KV{}
		}
	case strings.HasPrefix(ct, "multipart/"):
		return "multipart body (" + ct + ") not generated"
	default:
		st.Headers = appendHeader(st.Headers, "Content-Type", ct)
		if ex.Kind == yaml.ScalarNode {
			st.Body = ex.Value
		} else {
			b, _ := yaml.Marshal(ex)
			st.Body = string(b)
		}
		if st.Body == "" {
			st.Body = "string"
		}
	}
	return ""
}

func (sp *spec) mediaExample(media *yaml.Node) *yaml.Node {
	if ex := get(media, "example"); ex != nil {
		return clone(ex)
	}
	if exs := get(media, "examples"); exs != nil && len(exs.Content) >= 2 {
		if v := get(sp.deref(exs.Content[1]), "value"); v != nil {
			return clone(v)
		}
	}
	return sp.example(get(media, "schema"), 0, nil)
}

const maxDepth = 8

// example generates an example value for a schema.
func (sp *spec) example(n *yaml.Node, depth int, refs []string) *yaml.Node {
	if n == nil || depth > maxDepth {
		return scalar("!!null", "null")
	}
	if ref := getStr(n, "$ref"); ref != "" {
		for _, r := range refs {
			if r == ref { // recursive schema
				return scalar("!!null", "null")
			}
		}
		return sp.example(sp.lookup(ref), depth+1, append(refs, ref))
	}
	for _, k := range []string{"example", "default"} {
		if v := get(n, k); v != nil {
			return clone(v)
		}
	}
	if v := get(n, "examples"); v != nil && v.Kind == yaml.SequenceNode && len(v.Content) > 0 {
		return clone(v.Content[0])
	}
	if v := get(n, "enum"); v != nil && len(v.Content) > 0 {
		return clone(v.Content[0])
	}
	if v := get(n, "const"); v != nil {
		return clone(v)
	}
	if all := get(n, "allOf"); all != nil {
		out := mapping()
		for _, sub := range all.Content {
			if e := sp.example(sub, depth+1, refs); e.Kind == yaml.MappingNode {
				out.Content = append(out.Content, e.Content...)
			}
		}
		return out
	}
	for _, k := range []string{"oneOf", "anyOf"} {
		if v := get(n, k); v != nil && len(v.Content) > 0 {
			return sp.example(v.Content[0], depth+1, refs)
		}
	}
	typ := get(n, "type")
	t := ""
	if typ != nil {
		t = typ.Value
		if typ.Kind == yaml.SequenceNode { // 3.1: [string, "null"]
			for _, c := range typ.Content {
				if c.Value != "null" {
					t = c.Value
					break
				}
			}
		}
	}
	if t == "" {
		switch {
		case get(n, "properties") != nil:
			t = "object"
		case get(n, "items") != nil:
			t = "array"
		}
	}
	switch t {
	case "object":
		out := mapping()
		props := get(n, "properties")
		if props != nil {
			for i := 0; i+1 < len(props.Content); i += 2 {
				p := props.Content[i+1]
				if getStr(sp.deref(p), "readOnly") == "true" {
					continue
				}
				e := sp.example(p, depth+1, refs)
				if e.Tag == "!!null" && get(sp.deref(p), "example") == nil {
					continue // recursive or unknown schema
				}
				out.Content = append(out.Content, str(props.Content[i].Value), e)
			}
		}
		return out
	case "array":
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		seq.Content = append(seq.Content, sp.example(get(n, "items"), depth+1, refs))
		return seq
	case "integer", "number":
		if m := getStr(n, "minimum"); m != "" {
			return scalar("", m)
		}
		return scalar("!!int", "0")
	case "boolean":
		return scalar("!!bool", "true")
	case "string":
		return str(stringExample(getStr(n, "format")))
	}
	return scalar("!!null", "null")
}

func stringExample(format string) string {
	switch format {
	case "date-time":
		return "2024-01-01T00:00:00Z"
	case "date":
		return "2024-01-01"
	case "time":
		return "00:00:00"
	case "uuid":
		return "00000000-0000-0000-0000-000000000000"
	case "email":
		return "user@example.com"
	case "uri", "url":
		return "https://example.com"
	case "hostname":
		return "example.com"
	case "ipv4":
		return "127.0.0.1"
	case "ipv6":
		return "::1"
	case "byte":
		return "c3RyaW5n"
	case "password":
		return "password"
	}
	return "string"
}

// successStatus returns the first 2xx response code, or nil.
func successStatus(responses *yaml.Node) any {
	if responses == nil {
		return nil
	}
	for i := 0; i+1 < len(responses.Content); i += 2 {
		code := responses.Content[i].Value
		if len(code) == 3 && code[0] == '2' {
			if strings.EqualFold(code, "2XX") {
				return "2xx"
			}
			var n int
			if _, err := fmt.Sscanf(code, "%d", &n); err == nil {
				return n
			}
		}
	}
	return nil
}
