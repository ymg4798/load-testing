// Package httpstep is the "http" step: one HTTP request per step.
//
//   - http:
//     name: login
//     method: POST
//     url: "{{ baseUrl }}/login"
//     headers: { X-Trace: "{{ uuid() }}" }
//     query: { page: 1 }
//     json: { id: "{{ users.id }}" }   # or body: "...", or form: {...}
//     timeout: 5s
//     check: { status: 200 }
//     extract: { token: $.token }
package httpstep

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
	"net/url"
	"strings"
	"time"

	"github.com/ymg4798/lt/internal/protocol"
	"github.com/ymg4798/lt/internal/tmpl"
	"github.com/ymg4798/lt/internal/yamlx"
)

func init() { protocol.Register("http", compile) }

// maxBody caps how much of a response is kept for checks and scripts.
const maxBody = 10 << 20

type spec struct {
	Name    string              `yaml:"name"`
	Method  string              `yaml:"method"`
	URL     string              `yaml:"url"`
	Headers map[string]string   `yaml:"headers"`
	Query   map[string]any      `yaml:"query"`
	JSON    any                 `yaml:"json"`
	Body    string              `yaml:"body"`
	Form    map[string]any      `yaml:"form"`
	Timeout yamlx.Duration      `yaml:"timeout"`
	Check   *protocol.CheckSpec `yaml:"check"`
	Extract map[string]string   `yaml:"extract"`
}

type step struct {
	tracing  protocol.Tracing
	name     string
	method   string
	url      *tmpl.String
	headers  map[string]*tmpl.String
	query    map[string]tmpl.Value
	json     tmpl.Value
	body     *tmpl.String
	form     map[string]tmpl.Value
	timeout  time.Duration
	check    *protocol.Check
	extract  *protocol.Extract
	needBody bool
	keepBody bool
	tr       *http.Transport
	cookies  bool
}

func compile(raw any, env *protocol.Env) (protocol.Step, error) {
	var sp spec
	if s, ok := raw.(string); ok { // shorthand: - http: http://host/path
		sp.URL = s
	} else if err := yamlx.Decode(raw, &sp); err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	if sp.URL == "" {
		return nil, fmt.Errorf("http: url is required")
	}
	s := &step{method: strings.ToUpper(sp.Method), timeout: sp.Timeout.D(), cookies: env.Options.Cookies, tracing: env.Options.Tracing}
	if s.method == "" {
		s.method = http.MethodGet
		if sp.JSON != nil || sp.Body != "" || sp.Form != nil {
			s.method = http.MethodPost
		}
	}
	if s.timeout == 0 {
		s.timeout = env.Options.Timeout
	}
	var err error
	if s.url, err = tmpl.Compile(sp.URL); err != nil {
		return nil, err
	}
	s.name = sp.Name
	if s.name == "" {
		s.name = s.method + " " + pathOf(sp.URL)
	}
	s.headers = map[string]*tmpl.String{}
	for k, v := range sp.Headers {
		if s.headers[k], err = tmpl.Compile(v); err != nil {
			return nil, err
		}
	}
	if s.query, err = compileMap(sp.Query); err != nil {
		return nil, err
	}
	if s.form, err = compileMap(sp.Form); err != nil {
		return nil, err
	}
	n := 0
	if sp.JSON != nil {
		n++
		if s.json, err = tmpl.CompileValue(sp.JSON); err != nil {
			return nil, err
		}
	}
	if sp.Body != "" {
		n++
		if s.body, err = tmpl.Compile(sp.Body); err != nil {
			return nil, err
		}
	}
	if sp.Form != nil {
		n++
	}
	if n > 1 {
		return nil, fmt.Errorf("http %s: use only one of json, body, form", s.name)
	}
	if s.check, err = protocol.CompileCheck(sp.Check); err != nil {
		return nil, fmt.Errorf("http %s: %w", s.name, err)
	}
	if s.extract, err = protocol.CompileExtract(sp.Extract); err != nil {
		return nil, fmt.Errorf("http %s: %w", s.name, err)
	}
	s.keepBody = env.HasScript
	s.needBody = s.keepBody || s.check.NeedsBody() || s.extract.NeedsBody()

	tr, err := env.Shared("http.transport", func() (any, error) {
		t := newTransport(env.Options)
		env.OnClose(t.CloseIdleConnections)
		return t, nil
	})
	if err != nil {
		return nil, err
	}
	s.tr = tr.(*http.Transport)
	return s, nil
}

func compileMap(m map[string]any) (map[string]tmpl.Value, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]tmpl.Value, len(m))
	for k, v := range m {
		c, err := tmpl.CompileValue(v)
		if err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, nil
}

// pathOf gives a readable default name from a URL template.
func pathOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if j := strings.IndexByte(u, '/'); j >= 0 {
			u = u[j:]
		} else {
			u = "/"
		}
	} else if strings.HasPrefix(u, "{{") {
		if j := strings.Index(u, "}}"); j >= 0 {
			u = u[j+2:]
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

func newTransport(o protocol.Options) *http.Transport {
	idle := o.MaxInFlight
	if idle < 100 {
		idle = 100
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		MaxIdleConns:          0,
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

func (s *step) client(vu *protocol.VU) *http.Client {
	return vu.State("http.client", func() any {
		c := &http.Client{Transport: s.tr}
		if s.cookies {
			c.Jar, _ = cookiejar.New(nil)
		}
		return c
	}).(*http.Client)
}

func (s *step) build(ctx context.Context, vu *protocol.VU, tr *protocol.Trace) (*http.Request, error) {
	u, err := s.url.RenderString(vu)
	if err != nil {
		return nil, err
	}
	if len(s.query) > 0 {
		pu, err := url.Parse(u)
		if err != nil {
			return nil, err
		}
		q := pu.Query()
		for k, v := range s.query {
			r, err := v.Render(vu)
			if err != nil {
				return nil, err
			}
			q.Set(k, tmpl.ToString(r))
		}
		pu.RawQuery = q.Encode()
		u = pu.String()
	}
	var body io.Reader
	contentType := ""
	switch {
	case s.json != nil:
		v, err := s.json.Render(vu)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		body, contentType = bytes.NewReader(b), "application/json"
	case s.body != nil:
		b, err := s.body.RenderString(vu)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(b)
	case s.form != nil:
		f := url.Values{}
		for k, v := range s.form {
			r, err := v.Render(vu)
			if err != nil {
				return nil, err
			}
			f.Set(k, tmpl.ToString(r))
		}
		body, contentType = strings.NewReader(f.Encode()), "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequestWithContext(ctx, s.method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
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
	var data []byte
	var n int64
	if s.needBody {
		data, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
		n = int64(len(data))
		if err == nil {
			m, _ := io.Copy(io.Discard, resp.Body)
			n += m
		}
	} else {
		n, err = io.Copy(io.Discard, resp.Body)
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
