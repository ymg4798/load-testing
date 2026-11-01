// Package graphqlstep is the "graphql" step: one GraphQL operation sent as
// an HTTP POST with a JSON body.
//
//   - graphql:
//     name: user                     # default: "GraphQL <operation name>"
//     url: "{{ baseUrl }}/graphql"
//     query: "query User($id: ID!) { user(id: $id) { id name } }"
//     variables: { id: "{{ users.id }}" }
//     operationName: User
//     headers: { Authorization: "Bearer {{ token }}" }
//     timeout: 5s
//     check: { status: 200, json: { "$.data.user.name": kim }, allowErrors: false }
//     extract: { uid: $.data.user.id }
//
// A response whose "errors" array is not empty fails the step with kind
// graphql:error unless check.allowErrors is true. The step shares the HTTP
// connection pool (and, with options.cookies, the per-VU cookie jar) with
// http steps and sends the trace headers when tracing is on.
package graphqlstep

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"time"

	"github.com/ymg4798/lt/internal/protocol"
	"github.com/ymg4798/lt/internal/tmpl"
	"github.com/ymg4798/lt/internal/yamlx"
)

func init() { protocol.Register("graphql", compile) }

const maxBody = 10 << 20

type checkSpec struct {
	Status          any               `yaml:"status"`
	BodyContains    string            `yaml:"bodyContains"`
	BodyNotContains string            `yaml:"bodyNotContains"`
	JSON            map[string]any    `yaml:"json"`
	Header          map[string]string `yaml:"header"`
	MaxLatency      yamlx.Duration    `yaml:"maxLatency"`
	AllowErrors     bool              `yaml:"allowErrors"`
}

type spec struct {
	Name          string            `yaml:"name"`
	URL           string            `yaml:"url"`
	Query         string            `yaml:"query"`
	Variables     map[string]any    `yaml:"variables"`
	OperationName string            `yaml:"operationName"`
	Headers       map[string]string `yaml:"headers"`
	Timeout       yamlx.Duration    `yaml:"timeout"`
	Check         *checkSpec        `yaml:"check"`
	Extract       map[string]string `yaml:"extract"`
}

type step struct {
	tracing     protocol.Tracing
	name        string
	url         *tmpl.String
	query       *tmpl.String
	variables   tmpl.Value
	opName      string
	headers     map[string]*tmpl.String
	timeout     time.Duration
	check       *protocol.Check
	allowErrors bool
	extract     *protocol.Extract
	tr          *http.Transport
	cookies     bool
}

var opNameRe = regexp.MustCompile(`^\s*(?:query|mutation|subscription)\s+([_A-Za-z][_0-9A-Za-z]*)`)

func compile(raw any, env *protocol.Env) (protocol.Step, error) {
	var sp spec
	if err := yamlx.Decode(raw, &sp); err != nil {
		return nil, fmt.Errorf("graphql: %w", err)
	}
	if sp.URL == "" || strings.TrimSpace(sp.Query) == "" {
		return nil, fmt.Errorf("graphql: url and query are required")
	}
	s := &step{name: sp.Name, opName: sp.OperationName, timeout: sp.Timeout.D(), tracing: env.Options.Tracing, cookies: env.Options.Cookies}
	if s.timeout == 0 {
		s.timeout = env.Options.Timeout
	}
	if s.name == "" {
		op := s.opName
		if op == "" {
			if m := opNameRe.FindStringSubmatch(stripComments(sp.Query)); m != nil {
				op = m[1]
			}
		}
		if op == "" {
			op = "query"
			if strings.HasPrefix(strings.TrimSpace(stripComments(sp.Query)), "mutation") {
				op = "mutation"
			}
		}
		s.name = "GraphQL " + op
	}
	var err error
	if s.url, err = tmpl.Compile(sp.URL); err != nil {
		return nil, err
	}
	if s.query, err = tmpl.Compile(sp.Query); err != nil {
		return nil, fmt.Errorf("graphql %s: query: %w", s.name, err)
	}
	if sp.Variables != nil {
		if s.variables, err = tmpl.CompileValue(sp.Variables); err != nil {
			return nil, fmt.Errorf("graphql %s: variables: %w", s.name, err)
		}
	}
	s.headers = map[string]*tmpl.String{}
	for k, v := range sp.Headers {
		if s.headers[k], err = tmpl.Compile(v); err != nil {
			return nil, err
		}
	}
	var cs *protocol.CheckSpec
	if c := sp.Check; c != nil {
		cs = &protocol.CheckSpec{Status: c.Status, BodyContains: c.BodyContains, BodyNotContains: c.BodyNotContains,
			JSON: c.JSON, Header: c.Header, MaxLatency: c.MaxLatency}
		s.allowErrors = c.AllowErrors
	}
	if s.check, err = protocol.CompileCheck(cs); err != nil {
		return nil, fmt.Errorf("graphql %s: %w", s.name, err)
	}
	if s.extract, err = protocol.CompileExtract(sp.Extract); err != nil {
		return nil, fmt.Errorf("graphql %s: %w", s.name, err)
	}
	// Same key and type as the http step, so both share one pool.
	tr, err := env.Shared("http.transport", func() (any, error) {
		t := newTransport(env.Options)
		env.OnClose(t.CloseIdleConnections)
		return t, nil
	})
	if err != nil {
		return nil, err
	}
	var ok bool
	if s.tr, ok = tr.(*http.Transport); !ok {
		return nil, fmt.Errorf("graphql: shared http transport has unexpected type %T", tr)
	}
	return s, nil
}

func stripComments(q string) string {
	var b strings.Builder
	for _, l := range strings.Split(q, "\n") {
		if i := strings.IndexByte(l, '#'); i >= 0 {
			l = l[:i]
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// newTransport mirrors httpstep's transport settings.
func newTransport(o protocol.Options) *http.Transport {
	idle := o.MaxInFlight
	if idle < 100 {
		idle = 100
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		MaxIdleConnsPerHost:   idle,
		MaxConnsPerHost:       o.MaxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableKeepAlives:     o.NoKeepAlive,
		DisableCompression:    true,
		ForceAttemptHTTP2:     o.HTTP2,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: o.Insecure}, //nolint:gosec // opt-in for test targets
	}
}

// client is the per-VU client; the state key matches httpstep's so that
// both steps share a cookie jar.
func (s *step) client(vu *protocol.VU) *http.Client {
	return vu.State("http.client", func() any {
		c := &http.Client{Transport: s.tr}
		if s.cookies {
			c.Jar, _ = cookiejar.New(nil)
		}
		return c
	}).(*http.Client)
}

type request struct {
	Query         string `json:"query"`
	Variables     any    `json:"variables,omitempty"`
	OperationName string `json:"operationName,omitempty"`
}

func (s *step) build(ctx context.Context, vu *protocol.VU, tr *protocol.Trace) (*http.Request, error) {
	u, err := s.url.RenderString(vu)
	if err != nil {
		return nil, err
	}
	body := request{OperationName: s.opName}
	if body.Query, err = s.query.RenderString(vu); err != nil {
		return nil, err
	}
	if s.variables != nil {
		if body.Variables, err = s.variables.Render(vu); err != nil {
			return nil, err
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/graphql-response+json, application/json")
	req.Header.Set("User-Agent", "lt/1")
	if tr != nil {
		for _, h := range tr.Headers {
			req.Header.Set(h[0], h[1])
		}
	}
	for k, t := range s.headers {
		v, err := t.RenderString(vu)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(k, "host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

func (s *step) Run(ctx context.Context, vu *protocol.VU) error {
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	tr := s.tracing.NewTrace(vu)
	traceID := ""
	if tr != nil {
		traceID = tr.ID
	}
	req, err := s.build(ctx, vu, tr)
	if err != nil {
		return protocol.Fail(s.name, "template", err.Error())
	}
	start := time.Now()
	resp, err := s.client(vu).Do(req)
	if err != nil {
		kind := protocol.ClassifyErr(err)
		vu.Record(protocol.Result{Step: s.name, Latency: time.Since(start), Err: kind, TraceID: traceID})
		return protocol.Fail(s.name, kind, err.Error())
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	n := int64(len(data))
	if err == nil {
		m, _ := io.Copy(io.Discard, resp.Body)
		n += m
	}
	resp.Body.Close()
	lat := time.Since(start)
	if err != nil {
		kind := protocol.ClassifyErr(err)
		vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: n, Err: kind, TraceID: traceID})
		return protocol.Fail(s.name, kind, err.Error())
	}
	r := &protocol.Response{Status: resp.StatusCode, Headers: resp.Header, Body: data, Latency: lat}
	kind, msg := s.check.Verify(r, vu, true)
	if kind == "" && !s.allowErrors {
		kind, msg = graphqlErrors(r)
	}
	if kind == "" {
		kind, msg = s.extract.Apply(r, vu.Vars)
	}
	vu.Last = r
	vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: n, Err: kind, TraceID: traceID})
	if kind != "" {
		return protocol.Fail(s.name, kind, msg)
	}
	return nil
}

// graphqlErrors fails when the body has a non-empty "errors" array.
func graphqlErrors(r *protocol.Response) (string, string) {
	doc, err := r.JSON()
	if err != nil {
		return "graphql:invalid", "response is not JSON"
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return "graphql:invalid", "response is not a JSON object"
	}
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return "", ""
	}
	msg := fmt.Sprintf("%d error(s)", len(errs))
	if e, ok := errs[0].(map[string]any); ok {
		if s, ok := e["message"].(string); ok {
			msg += ": " + s
		}
	}
	return "graphql:error", msg
}
