package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

// Middleware traces each request: a server span named "METHOD route"
// (continuing the caller's trace), its status code, and an entry in the
// active list while it runs. The span is in the request's context, so
// database calls and outgoing calls made with it become its children.
//
// The handler runs with the pprof goroutine label lt_tx=<span id>, which
// lets the agent show where a long request is stuck (see
// Config.StackAfter); the last stack captured is kept on the span as
// lt.stack.
//
// A panic in the handler is recorded on the span as an exception event
// (exception.message, exception.stacktrace); if nothing was written yet the
// client gets a 500, and the panic then continues up the stack as usual.
func Middleware(next http.Handler) http.Handler {
	mux, _ := next.(*http.ServeMux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current.Load() == nil {
			next.ServeHTTP(w, r)
			return
		}
		route := ""
		if mux != nil {
			if _, p := mux.Handler(r); p != "" {
				route = patternRoute(p)
			}
		}
		name := r.Method + " " + r.URL.Path
		if route != "" {
			name = r.Method + " " + route
		}
		s := newSpan(r.Context(), extract(r.Header.Get), name, KindServer)
		if s == nil {
			next.ServeHTTP(w, r)
			return
		}
		s.method = r.Method
		s.attrs = append(s.attrs,
			attr{"http.request.method", r.Method},
			attr{"url.path", r.URL.Path},
			attr{"server.address", r.Host})
		if route != "" {
			s.attrs = append(s.attrs, attr{"http.route", route})
		}
		t := &tx{span: s, start: s.start, traceID: s.TraceID()}
		t.name.Store(&name)
		s.tx = t
		outerPattern := r.Pattern
		outerCtx := r.Context()
		ctx := context.WithValue(context.WithValue(outerCtx, spanKey{}, s), txKey{}, t)
		if s.a.cfg.StackAfter > 0 {
			t.label = s.SpanID()
			ctx = pprof.WithLabels(ctx, pprof.Labels(labelKey, t.label))
			pprof.SetGoroutineLabels(ctx)
		}
		s.a.addTx(t)
		r = r.WithContext(ctx)
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			rec := recover()
			if t.label != "" {
				pprof.SetGoroutineLabels(outerCtx)
			}
			s.a.removeTx(t)
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			if rec != nil && rec != http.ErrAbortHandler {
				s.mu.Lock()
				s.recordLocked(&exception{at: time.Now(), message: fmt.Sprint(rec), typ: fmt.Sprintf("%T", rec),
					stack: panicStack(debug.Stack()), escaped: true})
				s.mu.Unlock()
				if !sw.wrote {
					status = http.StatusInternalServerError
					writePanic500(sw)
				}
			}
			if st := t.stack.Load(); st != nil {
				s.SetAttr("lt.stack", *st)
			}
			// A ServeMux inside next sets the pattern on our copy of the request.
			s.mu.Lock()
			routed := s.routed
			s.mu.Unlock()
			if route == "" && !routed && r.Pattern != "" && r.Pattern != outerPattern {
				s.setRoute(patternRoute(r.Pattern))
			}
			s.SetAttr("http.response.status_code", status)
			switch {
			case rec != nil:
				s.finish(true, fmt.Sprintf("panic: %v", rec), nil)
			case status >= 500:
				s.finish(true, http.StatusText(status), nil)
			default:
				s.finish(false, "", nil)
			}
			if rec != nil {
				panic(rec)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// writePanic500 answers a request whose handler panicked before writing
// anything: a complete 500 (with a length, flushed) so the client gets it
// even though the panic then closes the connection.
func writePanic500(w *statusWriter) {
	defer func() { _ = recover() }() // a broken connection must not mask the panic
	const body = "Internal Server Error\n"
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Connection", "close")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, body)
	w.Flush()
}

// patternRoute turns a ServeMux pattern ("GET example.com/items/{id}") into a route.
func patternRoute(p string) string {
	if i := strings.IndexByte(p, ' '); i >= 0 {
		p = strings.TrimLeft(p[i+1:], " \t")
	}
	if i := strings.IndexByte(p, '/'); i > 0 {
		p = p[i:]
	}
	return p
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	if code >= 200 {
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		w.wrote = true
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("agent: response writer cannot be hijacked")
	}
	c, rw, err := h.Hijack()
	if err == nil {
		w.status, w.wrote = http.StatusSwitchingProtocols, true
	}
	return c, rw, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Transport traces outgoing requests made with the returned RoundTripper
// (base, or http.DefaultTransport when nil): a client span per request,
// a traceparent header so the called service continues the trace, and
// "GET host/path" as the caller's current activity while it waits.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base}
}

type transport struct{ base http.RoundTripper }

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	s := newSpan(ctx, nil, req.Method+" "+req.URL.Host, KindClient)
	if s == nil {
		return t.base.RoundTrip(req)
	}
	u := *req.URL
	u.RawQuery, u.User, u.Fragment = "", nil, ""
	s.attrs = append(s.attrs,
		attr{"http.request.method", req.Method},
		attr{"url.full", u.String()},
		attr{"server.address", req.URL.Hostname()})
	if p := req.URL.Port(); p != "" {
		s.attrs = append(s.attrs, attr{"server.port", p})
	}
	out := req.Clone(ctx)
	out.Header.Set("traceparent", s.traceparent())
	restore := setActivity(ctx, req.Method+" "+req.URL.Host+req.URL.Path)
	resp, err := t.base.RoundTrip(out)
	restore()
	if err == nil {
		s.SetAttr("http.response.status_code", resp.StatusCode)
		if resp.StatusCode >= 500 {
			s.Fail(resp.Status)
		}
	}
	s.End(err)
	return resp, err
}
