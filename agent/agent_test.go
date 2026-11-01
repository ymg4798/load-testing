package agent

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/apm"
	"github.com/ymg4798/lt/internal/metrics"
)

// testDriver runs no SQL; a query containing "block" waits until release
// is closed. Only the deprecated-free context interfaces are implemented
// on the conn, and the stmt is the plain driver.Stmt, so both the direct
// and the prepared paths of the wrapper are exercised.
type testDriver struct{}

// release unblocks "block" queries; TestActiveList replaces it per run.
var release = make(chan struct{})

func (testDriver) Open(string) (driver.Conn, error) { return testConn{}, nil }

type testConn struct{}

func (testConn) Prepare(q string) (driver.Stmt, error) { return testStmt{q}, nil }
func (testConn) Close() error                          { return nil }
func (testConn) Begin() (driver.Tx, error)             { return testTx{}, nil }
func (testConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "block") {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if strings.Contains(q, "fail") {
		return nil, errors.New("syntax error")
	}
	time.Sleep(2 * time.Millisecond)
	return &testRows{}, nil
}

type testStmt struct{ q string }

func (testStmt) Close() error  { return nil }
func (testStmt) NumInput() int { return -1 }
func (testStmt) Exec([]driver.Value) (driver.Result, error) {
	time.Sleep(time.Millisecond)
	return driver.RowsAffected(1), nil
}
func (testStmt) Query([]driver.Value) (driver.Rows, error) { return &testRows{}, nil }

type testTx struct{}

func (testTx) Commit() error   { return nil }
func (testTx) Rollback() error { return nil }

type testRows struct{ done bool }

func (*testRows) Columns() []string { return []string{"v"} }
func (*testRows) Close() error      { return nil }
func (r *testRows) Next(d []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	d[0] = int64(7)
	return nil
}

func init() { sql.Register("agenttest", testDriver{}) }

type fixture struct {
	col  *apm.Collector
	a    *Agent
	srv  *httptest.Server
	stop func()
}

func setup(t *testing.T) *fixture {
	t.Helper()
	col, err := apm.New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := Start(Config{Service: "shop", Endpoint: "http://" + col.Addr() + "/", Instance: "test-1", FlushInterval: 20 * time.Millisecond})
	db, err := OpenDB("agenttest", "")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: Transport(nil)}
	mux := http.NewServeMux()
	f := &fixture{col: col, a: a}
	mux.HandleFunc("GET /stock", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /order/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var v int64
		if err := db.QueryRowContext(ctx, "SELECT /* q1 */ v FROM orders WHERE id = "+r.PathValue("id")).Scan(&v); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if _, err := db.ExecContext(ctx, "UPDATE orders SET seen = 1 WHERE id = ?", 5); err != nil { // prepared path
			http.Error(w, err.Error(), 500)
			return
		}
		ctx2, sp := StartSpan(ctx, "render", KindInternal)
		sp.SetAttr("items", 3)
		req, _ := http.NewRequestWithContext(ctx2, "GET", f.srv.URL+"/stock?sku=1", nil)
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		resp.Body.Close()
		sp.End(nil)
		_, _ = w.Write([]byte("done"))
	})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), "SELECT block FROM big_table")
		if err == nil {
			rows.Close()
		}
	})
	mux.HandleFunc("GET /bad", func(w http.ResponseWriter, r *http.Request) {
		_, err := db.QueryContext(r.Context(), "SELECT fail")
		http.Error(w, err.Error(), 500)
	})
	legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetRoute(r.Context(), "/legacy/{n}")
		w.WriteHeader(201)
	})
	root := http.NewServeMux()
	root.Handle("/legacy/", Middleware(legacy))
	root.Handle("/", Middleware(mux))
	f.srv = httptest.NewServer(root)
	f.stop = func() {
		f.srv.Close()
		_ = db.Close()
		col.Close()
	}
	t.Cleanup(func() {
		_ = a.Shutdown(context.Background())
		f.stop()
	})
	return f
}

func (f *fixture) get(t *testing.T, path string, h map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", f.srv.URL+path, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestEndToEnd(t *testing.T) {
	f := setup(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const parent = "00f067aa0ba902b7"
	if code := f.get(t, "/order/42", map[string]string{"traceparent": "00-" + traceID + "-" + parent + "-01"}); code != 200 {
		t.Fatalf("status %d", code)
	}
	if err := f.a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	tr := f.col.Trace(traceID)
	if tr == nil {
		t.Fatalf("trace not collected (sent %d, dropped %d, stats %+v)", f.a.Sent(), f.a.Dropped(), f.col.Stats())
	}
	byName := map[string]int{}
	for i, s := range tr.Spans {
		byName[s.Kind+" "+s.Name] = i
		if s.Service != "shop" {
			t.Errorf("service %q", s.Service)
		}
	}
	span := func(key string) (s struct{ ID, Parent string }) {
		i, ok := byName[key]
		if !ok {
			t.Fatalf("no span %q in %v", key, byName)
		}
		return struct{ ID, Parent string }{tr.Spans[i].SpanID, tr.Spans[i].ParentID}
	}
	srv := span("server GET /order/{id}")
	sel := span("db SELECT")
	upd := span("db UPDATE")
	render := span("internal render")
	call := span("http GET " + strings.TrimPrefix(f.srv.URL, "http://"))
	stock := span("server GET /stock")
	if srv.Parent != parent {
		t.Errorf("server parent %q, want the incoming span", srv.Parent)
	}
	if sel.Parent != srv.ID || upd.Parent != srv.ID || render.Parent != srv.ID {
		t.Errorf("db/internal parents: %v %v %v, server %v", sel, upd, render, srv.ID)
	}
	if call.Parent != render.ID || stock.Parent != call.ID {
		t.Errorf("call parent %q (render %q), stock parent %q (call %q)", call.Parent, render.ID, stock.Parent, call.ID)
	}
	s := tr.Spans[byName["db SELECT"]]
	if s.Attrs["db.system"] != "agenttest" || !strings.Contains(s.Attrs["db.query.text"], "WHERE id = 42") {
		t.Errorf("db attrs %v", s.Attrs)
	}
	if a := tr.Spans[byName["server GET /order/{id}"]].Attrs; a["http.response.status_code"] != "200" || a["http.route"] != "/order/{id}" {
		t.Errorf("server attrs %v", a)
	}
	if a := tr.Spans[byName["http GET "+strings.TrimPrefix(f.srv.URL, "http://")]].Attrs; !strings.HasSuffix(a["url.full"], "/stock") {
		t.Errorf("client attrs %v", a)
	}

	// The collector counts one transaction: /stock is a call within the service.
	var snap metrics.Snapshot
	f.col.Decorate(&snap)
	time.Sleep(600 * time.Millisecond) // let a parent-less server span stop waiting
	f.col.Decorate(&snap)
	ops := map[string]int64{}
	for _, o := range f.col.Operations() {
		ops[o.Kind+" "+o.Name] = o.Count
	}
	if ops["db SELECT /* q1 */ v FROM orders WHERE id = ?"] != 0 || ops["db SELECT v FROM orders WHERE id = ?"] != 1 || ops["db UPDATE orders SET seen = ? WHERE id = ?"] != 1 {
		t.Errorf("ops %v", ops)
	}
}

func TestHitsAndErrors(t *testing.T) {
	f := setup(t)
	f.get(t, "/order/1", nil)
	if code := f.get(t, "/bad", map[string]string{"b3": "80f198ee56343ba864fe8b2a57d3eff7-e457b5a2e4d86bd1-1"}); code != 500 {
		t.Fatalf("bad: %d", code)
	}
	f.get(t, "/legacy/7", map[string]string{"uber-trace-id": "5af7183fb1d4cf5f%3A6e2b4a3d2c1b0a99%3A0%3A1"})
	if err := f.a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	var snap metrics.Snapshot
	f.col.Decorate(&snap)
	if snap.APMHits == nil {
		t.Fatal("no hits")
	}
	got := map[string]metrics.HitSample{}
	for _, s := range snap.APMHits.Samples {
		got[s.Key] = s
	}
	if len(got) != 3 {
		t.Fatalf("samples %+v", snap.APMHits.Samples)
	}
	if s := got["shop|GET /bad"]; s.Err != "status:500" || s.TraceID != "80f198ee56343ba864fe8b2a57d3eff7" {
		t.Errorf("bad sample %+v", s)
	}
	if s := got["shop|GET /legacy/{n}"]; s.TraceID != "00000000000000005af7183fb1d4cf5f" {
		t.Errorf("legacy sample %+v (all %v)", s, got)
	}
	tr := f.col.Trace("80f198ee56343ba864fe8b2a57d3eff7")
	failed := false
	for _, s := range tr.Spans {
		if s.Kind == "db" && s.Error && s.Attrs["exception.message"] == "syntax error" {
			failed = true
		}
	}
	if !failed {
		t.Errorf("failed db span not marked: %+v", tr.Spans)
	}
	if snap.Server["apm tps"] != 3 || snap.Server["apm errors"] != 1 {
		t.Errorf("series %v", snap.Server)
	}
}

func TestActiveList(t *testing.T) {
	release = make(chan struct{})
	f := setup(t)
	done := make(chan int)
	go func() { done <- f.get(t, "/slow", nil) }()
	deadline := time.Now().Add(5 * time.Second)
	var act []metrics.ActiveTx
	for time.Now().Before(deadline) {
		if act = f.col.Active(10); len(act) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(release)
	<-done
	if len(act) != 1 {
		t.Fatalf("active %+v", act)
	}
	if a := act[0]; a.Source != "shop" || a.Who != "test-1" || a.Name != "GET /slow" || a.Detail != "SELECT block FROM big_table" || len(a.TraceID) != 32 {
		t.Errorf("active %+v", a)
	}
	if err := f.a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if act := f.col.Active(10); len(act) != 0 { // shutdown reports an empty list
		t.Errorf("after shutdown %+v", act)
	}
}

func TestNoAgent(t *testing.T) {
	ctx, s := StartSpan(context.Background(), "x", KindInternal)
	s.SetAttr("a", 1)
	s.End(nil)
	if s != nil || SpanFromContext(ctx) != nil {
		t.Fatal("span without an agent")
	}
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
}

func TestQueueFull(t *testing.T) {
	a := Start(Config{Service: "q", Endpoint: "http://127.0.0.1:1", QueueSize: 2, FlushInterval: time.Hour})
	for i := 0; i < 5; i++ {
		_, s := StartSpan(context.Background(), "x", KindInternal)
		s.End(nil)
	}
	if a.Dropped() != 3 {
		t.Errorf("dropped %d", a.Dropped())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.Shutdown(ctx)
}

func TestExtract(t *testing.T) {
	cases := []struct {
		h           map[string]string
		trace, span string
	}{
		{map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}, "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"},
		{map[string]string{"traceparent": "00-00000000000000000000000000000000-00f067aa0ba902b7-01"}, "", ""},
		{map[string]string{"b3": "a3ce929d0e0e4736-00f067aa0ba902b7-1-05e3ac9a4f6e3b90"}, "0000000000000000a3ce929d0e0e4736", "00f067aa0ba902b7"},
		{map[string]string{"X-B3-TraceId": "4bf92f3577b34da6a3ce929d0e0e4736", "X-B3-SpanId": "00f067aa0ba902b7"}, "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"},
		{map[string]string{"uber-trace-id": "4bf92f3577b34da6a3ce929d0e0e4736:f067aa0ba902b7:0:1"}, "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"},
		{map[string]string{"b3": "0"}, "", ""},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.h {
			h.Set(k, v)
		}
		r := extract(h.Get)
		if c.trace == "" {
			if r != nil {
				t.Errorf("%v: got %x", c.h, r.traceID)
			}
			continue
		}
		if r == nil || hexID(r.traceID[:]) != c.trace || hexID(r.spanID[:]) != c.span {
			t.Errorf("%v: got %+v", c.h, r)
		}
	}
}

func TestSQLVerb(t *testing.T) {
	for in, want := range map[string]string{"  select * from t": "SELECT", "/* 5ms */ UPDATE t": "UPDATE", "": "SQL", "with(x)": "WITH"} {
		if got := sqlVerb(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
