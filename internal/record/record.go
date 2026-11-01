// Package record is `lt record`: an HTTP proxy that captures the traffic
// passing through it and turns it into a scenario.
//
// Two modes:
//
//   - forward proxy (default): set it as the browser or system HTTP(S)
//     proxy. Plain HTTP is recorded. HTTPS arrives as CONNECT tunnels, which
//     pass through unrecorded unless MITM is on: then the proxy terminates
//     TLS with per-host certificates signed by a local CA the user trusts.
//   - reverse proxy (Target set): point the client at the proxy instead of
//     the server; every request is forwarded to Target and recorded.
//
// The recording goes through internal/convert (the HAR path), so static
// assets, noisy headers, cookies and pauses are handled the same way.
package record

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ymg4798/lt/internal/convert"
)

// maxBody caps how much of a request body is recorded and forwarded in one
// piece. Larger bodies are forwarded but recorded without the body.
const maxBody = 8 << 20

// Options configure a Recorder.
type Options struct {
	Target   string   // reverse-proxy mode: forward everything here
	MITM     bool     // intercept HTTPS CONNECT tunnels (needs CA)
	CA       *CA      // signs per-host certificates for MITM
	Hosts    []string // record only these hosts (others pass through)
	Insecure bool     // don't verify upstream TLS certificates
	Log      io.Writer
}

// Recorder is an http.Handler that proxies and records.
type Recorder struct {
	opt    Options
	target *url.URL
	proxy  *httputil.ReverseProxy
	tr     *http.Transport

	mu      sync.Mutex
	entries []convert.Recorded

	tunnels   atomic.Int64 // CONNECT tunnels passed through unrecorded
	innerMu   sync.Mutex
	inner     map[*http.Server]struct{} // MITM servers, one per tunnel
	blind     map[net.Conn]net.Conn     // pass-through tunnels: client -> upstream
	innerDone sync.WaitGroup
	closed    atomic.Bool
}

type ctxKey struct{}

// pending is the part of an entry known before the response.
type pending struct {
	entry convert.Recorded
}

// New creates a Recorder.
func New(opt Options) (*Recorder, error) {
	r := &Recorder{opt: opt, inner: map[*http.Server]struct{}{}, blind: map[net.Conn]net.Conn{}}
	if opt.Log == nil {
		r.opt.Log = io.Discard
	}
	if opt.Target != "" {
		u, err := url.Parse(opt.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("--target must be an http:// or https:// URL, got %q", opt.Target)
		}
		r.target = u
	}
	if opt.MITM && opt.CA == nil {
		return nil, fmt.Errorf("record: MITM needs a CA")
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	r.tr = &http.Transport{
		Proxy:                 nil, // never loop back through HTTP(S)_PROXY, which may point at us
		DialContext:           d.DialContext,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: opt.Insecure}, //nolint:gosec // opt-in
	}
	r.proxy = &httputil.ReverseProxy{
		Rewrite:        r.rewrite,
		Transport:      r.tr,
		FlushInterval:  -1, // stream (SSE, long polls)
		ModifyResponse: r.onResponse,
		ErrorHandler:   r.onError,
	}
	return r, nil
}

// Entries returns what was recorded so far, in request order.
func (r *Recorder) Entries() []convert.Recorded {
	r.mu.Lock()
	out := append([]convert.Recorded(nil), r.entries...)
	r.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Tunnels counts CONNECT tunnels that passed through without recording.
func (r *Recorder) Tunnels() int64 { return r.tunnels.Load() }

// Scenario converts the recording into scenario YAML. Hosts (from opt, or
// else the recorder's) keep matching hosts and their subdomains.
func (r *Recorder) Scenario(opt convert.Options) ([]byte, error) {
	hosts := opt.Hosts
	if hosts == nil {
		hosts = r.opt.Hosts
	}
	opt.Hosts = nil
	var keep []convert.Recorded
	skipped := 0
	for _, e := range r.Entries() {
		u, err := url.Parse(e.URL)
		if err == nil && !HostMatches(u.Host, hosts) {
			skipped++
			continue
		}
		keep = append(keep, e)
	}
	if len(keep) == 0 && skipped > 0 {
		return nil, fmt.Errorf("record: all %d request(s) were to hosts other than %s", skipped, strings.Join(hosts, ", "))
	}
	s, err := convert.FromRecording(keep, opt)
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		s.Comments = append(s.Comments[:1:1], append([]string{fmt.Sprintf("skipped %d request(s) to other hosts (--host)", skipped)}, s.Comments[1:]...)...)
	}
	if r.opt.Insecure {
		s.Insecure = true
	}
	return s.YAML()
}

// ServeHTTP proxies one request.
func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.Method == http.MethodConnect:
		r.connect(w, req)
	case req.URL.IsAbs():
		r.forward(w, req, req.URL)
	case r.target != nil:
		u := *r.target
		u.Path = joinPath(r.target.Path, req.URL.Path)
		u.RawPath = ""
		u.RawQuery = req.URL.RawQuery
		r.forward(w, req, &u)
	default:
		http.Error(w, "lt record: this is an HTTP proxy. Set it as your HTTP/HTTPS proxy, or start it with --target http://your-app to use it as a reverse proxy.", http.StatusBadRequest)
	}
}

func joinPath(a, b string) string {
	if a == "" || a == "/" {
		return b
	}
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}

// forward sends req to out through the reverse proxy, recording it.
func (r *Recorder) forward(w http.ResponseWriter, req *http.Request, out *url.URL) {
	p := &pending{entry: convert.Recorded{Started: time.Now(), Method: req.Method, URL: out.String()}}
	for _, k := range sortedKeys(req.Header) {
		if hopHeader(k, req.Header) {
			continue
		}
		for _, v := range req.Header[k] {
			p.entry.Headers = append(p.entry.Headers, convert.KV{Key: k, Value: v})
		}
	}
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
		if err != nil {
			http.Error(w, "lt record: reading request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(b) <= maxBody {
			p.entry.Body = string(b)
			req.Body = io.NopCloser(bytes.NewReader(b))
		} else {
			fmt.Fprintf(r.opt.Log, "  body of %s %s is over %d MB: forwarded, not recorded\n", req.Method, out, maxBody>>20)
			req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), req.Body))
		}
		p.entry.ContentType = req.Header.Get("Content-Type")
	}
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, p))
	req.URL = out
	r.proxy.ServeHTTP(w, req)
}

func (r *Recorder) rewrite(pr *httputil.ProxyRequest) {
	if r.target != nil && pr.In.URL.Host == r.target.Host {
		pr.Out.Host = "" // reverse mode: Host is the target's
	} else {
		pr.Out.Host = pr.In.Host
		if pr.Out.Host == "" {
			pr.Out.Host = pr.In.URL.Host
		}
	}
	pr.Out.Header.Del("Proxy-Connection")
	pr.Out.Header.Del("Proxy-Authorization")
}

func (r *Recorder) onResponse(resp *http.Response) error {
	p, _ := resp.Request.Context().Value(ctxKey{}).(*pending)
	if p == nil {
		return nil
	}
	e := p.entry
	e.Duration = time.Since(e.Started)
	e.Status = resp.StatusCode
	e.ResponseType = resp.Header.Get("Content-Type")
	for _, k := range sortedKeys(resp.Header) {
		if strings.EqualFold(k, "Set-Cookie") || strings.EqualFold(k, "Content-Type") || strings.EqualFold(k, "Location") {
			for _, v := range resp.Header[k] {
				e.ResponseHeaders = append(e.ResponseHeaders, convert.KV{Key: k, Value: v})
			}
		}
	}
	r.add(e)
	return nil
}

func (r *Recorder) onError(w http.ResponseWriter, req *http.Request, err error) {
	if p, _ := req.Context().Value(ctxKey{}).(*pending); p != nil {
		e := p.entry
		e.Duration = time.Since(e.Started)
		r.add(e) // Status 0: the converter reports it as "got no response"
	}
	fmt.Fprintf(r.opt.Log, "  %s %s: %v\n", req.Method, req.URL, err)
	if !errors.Is(err, context.Canceled) {
		w.WriteHeader(http.StatusBadGateway)
	}
}

func (r *Recorder) add(e convert.Recorded) {
	r.mu.Lock()
	r.entries = append(r.entries, e)
	n := len(r.entries)
	r.mu.Unlock()
	fmt.Fprintf(r.opt.Log, "  #%-4d %3d %-6s %s\n", n, e.Status, e.Method, e.URL)
}

// connect handles a CONNECT request: a blind tunnel, or TLS interception.
func (r *Recorder) connect(w http.ResponseWriter, req *http.Request) {
	hostport := req.Host
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		hostport = net.JoinHostPort(hostport, "443")
	}
	host, port, _ := net.SplitHostPort(hostport)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT not supported", http.StatusInternalServerError)
		return
	}
	intercept := r.opt.MITM && (HostMatches(host, r.opt.Hosts) || HostMatches(hostport, r.opt.Hosts))
	var upstream net.Conn
	if !intercept {
		var err error
		upstream, err = net.DialTimeout("tcp", hostport, 10*time.Second)
		if err != nil {
			http.Error(w, "lt record: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	c, buf, err := hj.Hijack()
	if err != nil {
		if upstream != nil {
			upstream.Close()
		}
		return
	}
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		c.Close()
		if upstream != nil {
			upstream.Close()
		}
		return
	}
	if !intercept {
		r.tunnels.Add(1)
		r.innerMu.Lock()
		if r.closed.Load() {
			r.innerMu.Unlock()
			c.Close()
			upstream.Close()
			return
		}
		r.blind[c] = upstream
		r.innerMu.Unlock()
		tunnel(c, buf, upstream)
		r.innerMu.Lock()
		delete(r.blind, c)
		r.innerMu.Unlock()
		return
	}
	r.mitm(c, host, port)
}

func tunnel(client net.Conn, buf io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, buf) // buf holds anything the client sent early
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
	client.Close()
	upstream.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = c.Close()
	}
}

// mitm terminates TLS on the hijacked connection and serves the requests
// inside it through the recording proxy.
func (r *Recorder) mitm(c net.Conn, host, port string) {
	cfg := &tls.Config{
		GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hi.ServerName
			if name == "" {
				name = host
			}
			return r.opt.CA.Leaf(name)
		},
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	}
	tc := tls.Server(c, cfg)
	authority := host
	if port != "443" {
		authority = net.JoinHostPort(host, port)
	}
	ln := newOneConnListener(tc)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			u := *req.URL
			u.Scheme, u.Host = "https", authority
			if req.Host == "" {
				req.Host = authority
			}
			r.forward(w, req, &u)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				ln.Close()
			}
		},
	}
	r.innerMu.Lock()
	if r.closed.Load() {
		r.innerMu.Unlock()
		c.Close()
		return
	}
	r.inner[srv] = struct{}{}
	r.innerDone.Add(1)
	r.innerMu.Unlock()
	_ = srv.Serve(ln)
	r.innerMu.Lock()
	delete(r.inner, srv)
	r.innerMu.Unlock()
	r.innerDone.Done()
}

// Close stops intercepted connections and idle upstream connections.
func (r *Recorder) Close() {
	r.innerMu.Lock()
	r.closed.Store(true)
	for s := range r.inner {
		_ = s.Close()
	}
	for c, up := range r.blind {
		_ = c.Close()
		_ = up.Close()
	}
	r.innerMu.Unlock()
	r.innerDone.Wait()
	r.tr.CloseIdleConnections()
}

// HostMatches reports whether host (with or without a port) is one of
// hosts or a subdomain of one. A filter with a port ("localhost:8080")
// must match host:port exactly. An empty list matches everything.
func HostMatches(host string, hosts []string) bool {
	if len(hosts) == 0 {
		return true
	}
	host = strings.ToLower(host)
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.TrimSuffix(name, ".")
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if _, _, err := net.SplitHostPort(h); err == nil {
			if h == host {
				return true
			}
			continue
		}
		if name == h || strings.HasSuffix(name, "."+h) {
			return true
		}
	}
	return false
}

var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authorization": true,
	"Proxy-Authenticate": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

func hopHeader(k string, h http.Header) bool {
	if hopHeaders[http.CanonicalHeaderKey(k)] {
		return true
	}
	for _, v := range h["Connection"] {
		for _, f := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), k) {
				return true
			}
		}
	}
	return false
}

func sortedKeys(h http.Header) []string {
	ks := make([]string, 0, len(h))
	for k := range h {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// oneConnListener hands one connection to http.Server and then blocks
// until closed.
type oneConnListener struct {
	mu   sync.Mutex
	c    net.Conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

func newOneConnListener(c net.Conn) *oneConnListener {
	return &oneConnListener{c: c, done: make(chan struct{}), addr: c.LocalAddr()}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.c
	l.c = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.addr }

// Serve runs the proxy on ln until ctx is done or, when d > 0, d has
// passed. It then stops accepting, gives in-flight requests a moment to
// finish and closes intercepted connections.
func (r *Recorder) Serve(ctx context.Context, ln net.Listener, d time.Duration) error {
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 30 * time.Second}
	if d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		r.Close()
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx) // hijacked tunnels are not waited for
	r.Close()
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
