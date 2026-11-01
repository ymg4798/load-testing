package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/apm"
	"github.com/ymg4798/lt/internal/metrics"
)

// waitInCheckout is the "slow code" active stacks must point at.
func waitInCheckout(ch chan struct{}) {
	<-ch
}

// panicInPricing panics with a runtime error.
func panicInPricing(i int) int {
	table := []int{1, 2, 3}
	return table[i]
}

func startFeatures(t *testing.T, h http.Handler) (*apm.Collector, *Agent, *httptest.Server) {
	t.Helper()
	col, err := apm.New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := Start(Config{Service: "shop", Endpoint: "http://" + col.Addr(), Instance: "i1", FlushInterval: 20 * time.Millisecond, StackAfter: 100 * time.Millisecond})
	srv := httptest.NewUnstartedServer(Middleware(h))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the re-raised panic
	srv.Start()
	t.Cleanup(func() {
		_ = a.Shutdown(context.Background())
		srv.Close()
		col.Close()
	})
	return col, a, srv
}

func TestActiveStackCapture(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /checkout", func(w http.ResponseWriter, r *http.Request) {
		waitInCheckout(release)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /fast", func(w http.ResponseWriter, r *http.Request) {})
	col, a, srv := startFeatures(t, mux)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Get(srv.URL + "/checkout")
		if err == nil {
			resp.Body.Close()
		}
	}()
	// A fast request in between must not get a stack.
	if resp, err := http.Get(srv.URL + "/fast"); err == nil {
		resp.Body.Close()
	}
	var got metrics.ActiveTx
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if act := col.Active(10); len(act) == 1 && act[0].Stack != "" {
			got = act[0]
			break
		}
	}
	close(release)
	<-done
	if !strings.Contains(got.Stack, "agent.waitInCheckout") || !strings.Contains(got.Stack, "features_test.go:") {
		t.Fatalf("active %+v", got)
	}
	for _, noise := range []string{"runtime.", "agent.Middleware", "net/http.(*conn).serve", "runtime/pprof"} {
		if strings.Contains(got.Stack, noise) {
			t.Errorf("stack keeps %q:\n%s", noise, got.Stack)
		}
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr := col.Trace(got.TraceID)
	if tr == nil {
		t.Fatal("trace missing")
	}
	if st := tr.Spans[0].Attrs["lt.stack"]; !strings.Contains(st, "agent.waitInCheckout") {
		t.Errorf("lt.stack %q", st)
	}
}

func TestStacksOff(t *testing.T) {
	a := Start(Config{Service: "x", Endpoint: "http://127.0.0.1:1", StackAfter: -1, FlushInterval: time.Hour})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	}()
	var labels string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		labels = "none"
		_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
		if strings.Contains(buf.String(), labelKey) {
			labels = "found"
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if labels != "none" {
		t.Errorf("labels with StackAfter < 0: %s", labels)
	}
}

func TestParseLabels(t *testing.T) {
	got := parseLabels(`{"a":"1", "lt_tx":"00f067aa0ba902b7", "q":"x\"y, z"}`)
	if got["lt_tx"] != "00f067aa0ba902b7" || got["a"] != "1" || got["q"] != `x"y, z` || len(got) != 3 {
		t.Errorf("%v", got)
	}
	if got := parseLabels(`{}`); len(got) != 0 {
		t.Errorf("%v", got)
	}
	if got := parseLabels(`{"broken`); len(got) != 0 {
		t.Errorf("%v", got)
	}
}

func TestParseGoroutineProfile(t *testing.T) {
	prof := "goroutine profile: total 4\n" +
		"2 @ 0x1 0x2\n" +
		"#\t0x1\tmain.idle+0x10\t/src/main.go:5\n\n" +
		"1 @ 0x1 0x2 0x3\n" +
		"# labels: {\"lt_tx\":\"aaaa\"}\n" +
		"#\t0x1\tsync.runtime_SemacquireMutex+0x1\t/go/src/runtime/sema.go:95\n" +
		"#\t0x2\tinternal/sync.(*Mutex).lockSlow+0x1\t/go/src/internal/sync/mutex.go:149\n" +
		"#\t0x2\tsync.(*Mutex).Lock+0x1\t\t\t/go/src/sync/mutex.go:46\n" +
		"#\t0x3\tmain.chargeCard+0x33\t/src/main.go:12\n" +
		"#\t0x3\tgithub.com/ymg4798/lt/agent.Middleware.func1+0x1\t/mod/agent/http.go:80\n" +
		"#\t0x3\tnet/http.HandlerFunc.ServeHTTP+0x1\t/go/src/net/http/server.go:2300\n" +
		"#\t0x3\tnet/http.(*conn).serve+0x1\t/go/src/net/http/server.go:2100\n\n" +
		"1 @ 0x1\n" +
		"# labels: {\"lt_tx\":\"aaaa\"}\n" + // a goroutine the handler started: the handler's wins
		"#\t0x1\tmain.background+0x1\t/src/main.go:30\n\n" +
		"1 @ 0x1\n" +
		"# labels: {\"lt_tx\":\"bbbb\"}\n" +
		"#\t0x1\tmain.other+0x1\t/src/main.go:40\n\n"
	got := parseGoroutineProfile([]byte(prof), map[string]bool{"aaaa": true})
	want := "sync.runtime_SemacquireMutex\n\t/go/src/runtime/sema.go:95\n" +
		"internal/sync.(*Mutex).lockSlow\n\t/go/src/internal/sync/mutex.go:149\n" +
		"sync.(*Mutex).Lock\n\t/go/src/sync/mutex.go:46\n" +
		"main.chargeCard\n\t/src/main.go:12\n"
	if len(got) != 1 || got["aaaa"] != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestStackCap(t *testing.T) {
	fs := make([]frame, 500)
	for i := range fs {
		fs[i] = frame{fn: "main.recurse", loc: "/src/main.go:1"}
	}
	s := formatStack(fs)
	if len(s) > maxStackBytes || !strings.HasSuffix(s, " more\n") {
		t.Errorf("len %d, tail %q", len(s), s[len(s)-20:])
	}
}

func TestPanicRecorded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /price", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{byte(panicInPricing(len(r.URL.Path)))})
	})
	col, a, srv := startFeatures(t, mux)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req, _ := http.NewRequest("GET", srv.URL+"/price", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || string(body) != "Internal Server Error\n" {
		t.Errorf("client got %d %q", resp.StatusCode, body)
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr := col.Trace(traceID)
	if tr == nil {
		t.Fatal("no trace")
	}
	s := tr.Spans[0]
	if !s.Error || s.Attrs["http.response.status_code"] != "500" || s.Attrs["exception.message"] != "runtime error: index out of range [6] with length 3" {
		t.Errorf("span %+v", s)
	}
	st := s.Attrs["exception.stacktrace"]
	if !strings.HasPrefix(st, "github.com/ymg4798/lt/agent.panicInPricing\n") || strings.Contains(st, "runtime.") || strings.Contains(st, "debug.Stack") {
		t.Errorf("stack:\n%s", st)
	}
	gs := col.Errors()
	if len(gs) != 1 || gs[0].Source != "panic" || gs[0].Message != "runtime error: index out of range [?] with length ?" || gs[0].TraceID != traceID || gs[0].Stack != st {
		t.Errorf("groups %+v", gs)
	}
}

func TestFailRecordsException(t *testing.T) {
	col, err := apm.New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer col.Close()
	a := Start(Config{Service: "svc", Endpoint: "http://" + col.Addr(), FlushInterval: time.Hour})
	_, s1 := StartSpan(context.Background(), "charge", KindInternal)
	s1.Fail("card declined")
	s1.End(errors.New("later error is not the exception"))
	_, s2 := StartSpan(context.Background(), "load", KindInternal)
	s2.End(&json.SyntaxError{})
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	a1 := col.Trace(s1.TraceID()).Spans[0].Attrs
	a2 := col.Trace(s2.TraceID()).Spans[0].Attrs
	if a1["exception.message"] != "card declined" {
		t.Errorf("fail: %v", a1)
	}
	if a2["exception.type"] != "*json.SyntaxError" {
		t.Errorf("end(err): %v", a2)
	}
}

func TestSlogHandler(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs" {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, b)
			mu.Unlock()
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer sink.Close()
	a := Start(Config{Service: "shop", Endpoint: sink.URL, Instance: "i1", FlushInterval: time.Hour})
	var out bytes.Buffer
	lg := slog.New(SlogHandler(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx, sp := StartSpan(context.Background(), "order", KindServer)
	lg.With("order", 42).WithGroup("db").ErrorContext(ctx, "slow query", "ms", 200, slog.Group("conn", "host", "db1"), "err", errors.New("timeout"))
	lg.DebugContext(context.Background(), "no span")
	sp.End(nil)
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"trace_id":"`+sp.TraceID()+`"`) || !strings.Contains(lines[0], `"span_id":"`+sp.SpanID()+`"`) || strings.Contains(lines[1], "trace_id") {
		t.Errorf("next handler output:\n%s", out.String())
	}
	if a.SentLogs() != 2 || a.DroppedLogs() != 0 {
		t.Errorf("sent %d dropped %d", a.SentLogs(), a.DroppedLogs())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("%d posts", len(bodies))
	}
	var req struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []struct {
					Key   string
					Value struct{ StringValue string }
				}
			}
			ScopeLogs []struct {
				LogRecords []struct {
					TimeUnixNano   string
					SeverityNumber int
					SeverityText   string
					Body           struct{ StringValue string }
					TraceID        string `json:"traceId"`
					SpanID         string `json:"spanId"`
					Attributes     []struct {
						Key   string
						Value map[string]any
					}
				}
			}
		}
	}
	if err := json.Unmarshal(bodies[0], &req); err != nil {
		t.Fatal(err)
	}
	rl := req.ResourceLogs[0]
	if rl.Resource.Attributes[0].Key != "service.name" || rl.Resource.Attributes[0].Value.StringValue != "shop" {
		t.Errorf("resource %+v", rl.Resource)
	}
	recs := rl.ScopeLogs[0].LogRecords
	if len(recs) != 2 {
		t.Fatalf("records %+v", recs)
	}
	r := recs[0]
	if r.SeverityNumber != 17 || r.SeverityText != "ERROR" || r.Body.StringValue != "slow query" || r.TraceID != sp.TraceID() || r.SpanID != sp.SpanID() || r.TimeUnixNano == "" {
		t.Errorf("record %+v", r)
	}
	attrs := map[string]any{}
	for _, at := range r.Attributes {
		for _, v := range at.Value {
			attrs[at.Key] = v
		}
	}
	if attrs["order"] != "42" || attrs["db.ms"] != "200" || attrs["db.conn.host"] != "db1" || attrs["db.err"] != "timeout" {
		t.Errorf("attrs %v", attrs)
	}
	if recs[1].SeverityNumber != 5 || recs[1].TraceID != "" {
		t.Errorf("debug record %+v", recs[1])
	}
}

func TestSlogHandlerNoAgentNeverBlocks(t *testing.T) {
	lg := slog.New(SlogHandler(nil))
	lg.Info("dropped quietly") // no agent: nothing to do
	a := Start(Config{Service: "q", Endpoint: "http://127.0.0.1:1", LogQueueSize: 3, FlushInterval: time.Hour})
	for i := 0; i < 10; i++ {
		lg.Warn("x", "i", i)
	}
	if a.DroppedLogs() != 7 {
		t.Errorf("dropped %d", a.DroppedLogs())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.Shutdown(ctx)
}

func TestSeverity(t *testing.T) {
	for l, want := range map[slog.Level]int{slog.LevelDebug: 5, slog.LevelInfo: 9, slog.LevelWarn: 13, slog.LevelError: 17, slog.LevelError + 100: 24, -100: 1} {
		if got := severity(l); got != want {
			t.Errorf("%v: %d", l, got)
		}
	}
}

func TestPanicStackRepanic(t *testing.T) {
	st := "goroutine 7 [running]:\n" +
		"runtime/debug.Stack()\n\t/go/src/runtime/debug/stack.go:26 +0x64\n" +
		"github.com/ymg4798/lt/agent.Middleware.func1.1()\n\t/mod/agent/http.go:90 +0x1\n" +
		"panic({0x1, 0x2})\n\t/go/src/runtime/panic.go:860 +0x1\n" +
		"main.handler.func1()\n\t/src/main.go:20 +0x1\n" +
		"panic({0x1, 0x2})\n\t/go/src/runtime/panic.go:860 +0x1\n" +
		"runtime.goPanicIndex(0x6, 0x3)\n\t/go/src/runtime/panic.go:115 +0x1\n" +
		"main.applyDiscount(...)\n\t/src/main.go:42\n" +
		"main.(*shop).price(0x1400)\n\t/src/main.go:50 +0x1\n" +
		"net/http.HandlerFunc.ServeHTTP(0x1, {0x2, 0x3}, 0x4)\n\t/go/src/net/http/server.go:2358 +0x1\n" +
		"net/http.(*conn).serve(0x1, {0x2, 0x3})\n\t/go/src/net/http/server.go:2100 +0x1\n" +
		"created by net/http.(*Server).Serve in goroutine 1\n\t/go/src/net/http/server.go:3454 +0x1\n"
	want := "main.applyDiscount\n\t/src/main.go:42\nmain.(*shop).price\n\t/src/main.go:50\n"
	if got := panicStack([]byte(st)); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
