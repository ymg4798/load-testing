package record

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/convert"
)

func app(t *testing.T, tls bool) *httptest.Server {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			b, _ := io.ReadAll(r.Body)
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "1"})
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"echo":` + string(b) + `}`))
		case "/logo.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		case "/missing":
			http.NotFound(w, r)
		default:
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<h1>" + r.Host + r.URL.RequestURI() + "</h1>"))
		}
	})
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

// start runs the recorder on a random port and returns its URL and a stop func.
func start(t *testing.T, opt Options) (*Recorder, string, func()) {
	t.Helper()
	rec, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rec.Serve(ctx, ln, 0) }()
	return rec, "http://" + ln.Addr().String(), func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func get(t *testing.T, c *http.Client, method, u, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, u, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Api-Key", "k1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestReverseProxy(t *testing.T) {
	srv := app(t, false)
	rec, proxy, stop := start(t, Options{Target: srv.URL})
	c := &http.Client{}
	if code, body := get(t, c, "GET", proxy+"/home?x=1", ""); code != 200 || !strings.Contains(body, "/home?x=1") {
		t.Fatalf("GET: %d %q", code, body)
	}
	get(t, c, "GET", proxy+"/logo.png", "")
	if code, body := get(t, c, "POST", proxy+"/login", `{"user":"kim"}`); code != 200 || !strings.Contains(body, `"user":"kim"`) {
		t.Fatalf("POST: %d %q", code, body)
	}
	if code, _ := get(t, c, "GET", proxy+"/missing", ""); code != 404 {
		t.Fatalf("404 not passed through: %d", code)
	}
	stop()
	es := rec.Entries()
	if len(es) != 4 {
		t.Fatalf("%d entries", len(es))
	}
	if es[2].Method != "POST" || es[2].URL != srv.URL+"/login" || es[2].Body != `{"user":"kim"}` || es[2].Status != 200 {
		t.Errorf("login entry: %+v", es[2])
	}
	y, err := rec.Scenario(convert.Options{Name: "rec"})
	if err != nil {
		t.Fatal(err)
	}
	out := string(y)
	for _, want := range []string{"Recorded by lt record: 3 request(s)", "skipped 1 static", "baseUrl: " + srv.URL,
		`url: "{{ baseUrl }}/home"`, "x: \"1\"", "json: {user: kim}", "X-Api-Key: k1", "status: 404", "cookies: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestForwardProxyHTTP(t *testing.T) {
	srv := app(t, false)
	other := app(t, false)
	host := strings.TrimPrefix(srv.URL, "http://")
	rec, proxy, stop := start(t, Options{})
	pu, _ := url.Parse(proxy)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	if code, body := get(t, c, "GET", srv.URL+"/a", ""); code != 200 || !strings.Contains(body, host+"/a") {
		t.Fatalf("GET via proxy: %d %q", code, body)
	}
	get(t, c, "GET", other.URL+"/b", "")
	// Direct (non-proxy) requests are refused with a hint.
	if code, body := get(t, &http.Client{}, "GET", proxy+"/x", ""); code != 400 || !strings.Contains(body, "--target") {
		t.Fatalf("direct: %d %q", code, body)
	}
	stop()
	if n := len(rec.Entries()); n != 2 {
		t.Fatalf("%d entries", n)
	}
	// Host filter at conversion: other's port differs, so filter by host:port.
	y, err := rec.Scenario(convert.Options{Hosts: []string{host}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(y), "{{ baseUrl }}/a") || strings.Contains(string(y), "/b") {
		t.Errorf("host filter:\n%s", y)
	}
}

func TestConnectTunnelAndMITM(t *testing.T) {
	srv := app(t, true)
	dir := t.TempDir()
	ca, created, err := LoadOrCreateCA(dir)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if ca2, created2, err := LoadOrCreateCA(dir); err != nil || created2 || !bytes.Equal(ca2.Cert.Raw, ca.Cert.Raw) {
		t.Fatalf("reload: %v %v", err, created2)
	}

	// Without --mitm: the tunnel passes through; the client sees the real server cert.
	rec, proxy, stop := start(t, Options{})
	pu, _ := url.Parse(proxy)
	direct := srv.Client().Transport.(*http.Transport).Clone()
	direct.Proxy = http.ProxyURL(pu)
	c := &http.Client{Transport: direct}
	if code, _ := get(t, c, "GET", srv.URL+"/tunnel", ""); code != 200 {
		t.Fatalf("tunnel: %d", code)
	}
	stop()
	if len(rec.Entries()) != 0 || rec.Tunnels() != 1 {
		t.Fatalf("tunnel recorded: %d entries, %d tunnels", len(rec.Entries()), rec.Tunnels())
	}

	// With --mitm: the client trusts our CA; upstream is the test server (self-signed → Insecure).
	rec, proxy, stop = start(t, Options{MITM: true, CA: ca, Insecure: true})
	pu, _ = url.Parse(proxy)
	c = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}}}
	for range 2 { // second request reuses the intercepted connection
		if code, body := get(t, c, "GET", srv.URL+"/secure?q=1", ""); code != 200 || !strings.Contains(body, "/secure?q=1") {
			t.Fatalf("mitm GET: %d %q", code, body)
		}
	}
	if code, _ := get(t, c, "POST", srv.URL+"/login", `{"a":1}`); code != 200 {
		t.Fatal("mitm POST")
	}
	stop()
	es := rec.Entries()
	if len(es) != 3 || !strings.HasPrefix(es[0].URL, "https://127.0.0.1:") || es[2].Body != `{"a":1}` {
		t.Fatalf("mitm entries: %+v", es)
	}
	y, err := rec.Scenario(convert.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(y), "insecure: true") || !strings.Contains(string(y), "baseUrl: https://127.0.0.1:") {
		t.Errorf("mitm scenario:\n%s", y)
	}

	// --host limits interception: other hosts are tunneled even with --mitm.
	rec, proxy, stop = start(t, Options{MITM: true, CA: ca, Insecure: true, Hosts: []string{"example.invalid"}})
	pu, _ = url.Parse(proxy)
	direct = srv.Client().Transport.(*http.Transport).Clone()
	direct.Proxy = http.ProxyURL(pu)
	if code, _ := get(t, &http.Client{Transport: direct}, "GET", srv.URL+"/x", ""); code != 200 {
		t.Fatal("filtered tunnel")
	}
	stop()
	if len(rec.Entries()) != 0 || rec.Tunnels() != 1 {
		t.Fatalf("filtered host was intercepted")
	}
}

func TestLeafCert(t *testing.T) {
	ca, _, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"app.example.com", "127.0.0.1"} {
		l, err := ca.Leaf(h)
		if err != nil {
			t.Fatal(err)
		}
		if l2, _ := ca.Leaf(h); l2 != l {
			t.Error("leaf not cached")
		}
		cert, err := parseLeaf(l)
		if err != nil {
			t.Fatal(err)
		}
		if err := cert.VerifyHostname(h); err != nil {
			t.Error(err)
		}
	}
}

func TestHostMatches(t *testing.T) {
	for _, c := range []struct {
		host  string
		hosts []string
		ok    bool
	}{
		{"a.com", nil, true}, {"a.com", []string{"a.com"}, true}, {"api.a.com", []string{"a.com"}, true},
		{"ba.com", []string{"a.com"}, false}, {"A.COM:443", []string{"a.com:443"}, true}, {"a.com:80", []string{"a.com:443"}, false},
		{"api.a.com:8443", []string{"a.com"}, true},
	} {
		if HostMatches(c.host, c.hosts) != c.ok {
			t.Errorf("%s %v != %v", c.host, c.hosts, c.ok)
		}
	}
}

func TestServeDuration(t *testing.T) {
	rec, _ := New(Options{})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	start := time.Now()
	if err := rec.Serve(context.Background(), ln, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("duration ignored")
	}
}

func parseLeaf(l *tls.Certificate) (*x509.Certificate, error) {
	return x509.ParseCertificate(l.Certificate[0])
}
