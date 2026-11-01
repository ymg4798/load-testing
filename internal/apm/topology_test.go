package apm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/ymg4798/load-testing/internal/run"
)

// xSpan is a JSON span with arbitrary attributes, status and exception event.
type xSpan struct {
	trace, id, parent, name string
	kind                    int
	startMs, endMs          int64
	attrs                   map[string]any
	statusErr               string // status message; "" = ok
	exc                     map[string]any
}

func (s xSpan) json() string {
	base := int64(1_800_000_000_000)
	kvs := func(m map[string]any) string {
		var out []string
		for k, v := range m {
			var val string
			switch x := v.(type) {
			case int:
				val = fmt.Sprintf(`{"intValue":"%d"}`, x)
			case bool:
				val = fmt.Sprintf(`{"boolValue":%t}`, x)
			default:
				b, _ := json.Marshal(fmt.Sprint(x))
				val = `{"stringValue":` + string(b) + `}`
			}
			out = append(out, fmt.Sprintf(`{"key":%q,"value":%s}`, k, val))
		}
		return "[" + strings.Join(out, ",") + "]"
	}
	j := fmt.Sprintf(`{"traceId":%q,"spanId":%q,"parentSpanId":%q,"name":%q,"kind":%d,"startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":%s`,
		s.trace, s.id, s.parent, s.name, s.kind, (base+s.startMs)*1e6, (base+s.endMs)*1e6, kvs(s.attrs))
	if s.statusErr != "" {
		j += fmt.Sprintf(`,"status":{"code":2,"message":%q}`, s.statusErr)
	}
	if s.exc != nil {
		j += `,"events":[{"name":"log","attributes":[]},{"name":"exception","timeUnixNano":"1","attributes":` + kvs(s.exc) + `}]`
	}
	return j + "}"
}

func sendX(t *testing.T, c *Collector, svc string, spans ...xSpan) {
	t.Helper()
	js := make([]string, len(spans))
	for i, s := range spans {
		js[i] = s.json()
	}
	post(t, c, "/v1/traces", "application/json", jsonSpanBody(svc, js...), false)
}

func TestServiceMap(t *testing.T) {
	c := newCollector()
	now := testClock(c)
	const ltParent = "aaaaaaaaaaaaaaaa" // lt's request: never reported
	for i := 1; i <= 4; i++ {
		tr := id32(i)
		srv, db, pay, stockCall, stockSrv, self, selfSrv := id16(i*10+1), id16(i*10+2), id16(i*10+3), id16(i*10+4), id16(i*10+5), id16(i*10+6), id16(i*10+7)
		status := 200
		if i == 4 {
			status = 503
		}
		// stock reports first: its parent (shop's client span) is not there yet.
		sendX(t, c, "stock", xSpan{trace: tr, id: stockSrv, parent: stockCall, name: "GET /stock", kind: 2, startMs: 12, endMs: 18,
			attrs: map[string]any{"http.request.method": "GET", "http.route": "/stock", "http.response.status_code": 200}})
		sendX(t, c, "shop",
			xSpan{trace: tr, id: srv, parent: ltParent, name: "GET /order/{id}", kind: 2, startMs: 0, endMs: 40,
				attrs: map[string]any{"http.request.method": "GET", "http.route": "/order/{id}", "http.response.status_code": status}},
			xSpan{trace: tr, id: db, parent: srv, name: "SELECT", kind: 3, startMs: 1, endMs: 1 + int64(i),
				attrs: map[string]any{"db.system": "postgres", "server.address": "db1", "db.name": "shop", "db.query.text": "SELECT 1"}},
			xSpan{trace: tr, id: pay, parent: srv, name: "POST", kind: 3, startMs: 5, endMs: 10,
				attrs: map[string]any{"http.request.method": "POST", "url.full": "https://pay.example/charge"}},
			xSpan{trace: tr, id: stockCall, parent: srv, name: "GET", kind: 3, startMs: 11, endMs: 20,
				attrs: map[string]any{"http.request.method": "GET", "server.address": "stock", "server.port": 8080}},
			xSpan{trace: tr, id: self, parent: srv, name: "GET", kind: 3, startMs: 21, endMs: 23,
				attrs: map[string]any{"http.request.method": "GET", "server.address": "127.0.0.1", "server.port": 8080}},
			xSpan{trace: tr, id: selfSrv, parent: self, name: "GET /fast", kind: 2, startMs: 21, endMs: 22,
				attrs: map[string]any{"http.request.method": "GET", "http.route": "/fast", "http.response.status_code": 200}},
		)
	}
	*now = now.Add(time.Second) // lt's parents never come
	m := c.Map()
	if m == nil {
		t.Fatal("no map")
	}
	nodes := map[string]run.MapNode{}
	var ids []string
	for _, n := range m.Nodes {
		nodes[n.ID] = n
		ids = append(ids, n.ID)
	}
	want := []string{"client:lt", "service:shop", "service:stock", "db:postgres@db1/shop", "http:pay.example"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Errorf("nodes %v, want %v", ids, want)
	}
	if n := nodes["service:shop"]; n.Count != 8 || n.Errors != 1 || n.Kind != "service" { // 4 orders + 4 /fast
		t.Errorf("shop %+v", n)
	}
	if n := nodes["db:postgres@db1/shop"]; n.Count != 4 || n.Name != "postgres@db1/shop" || n.Kind != "db" || n.MeanMs != 2.5 {
		t.Errorf("db %+v", n)
	}
	if n := nodes["client:lt"]; n.Count != 4 || n.Errors != 1 {
		t.Errorf("lt %+v", n)
	}
	edges := map[string]run.MapEdge{}
	for _, e := range m.Edges {
		edges[e.From+">"+e.To] = e
	}
	if len(edges) != 5 {
		t.Errorf("edges %+v", m.Edges)
	}
	if e := edges["client:lt>service:shop"]; e.Count != 4 || e.Errors != 1 || e.MeanMs != 40 || e.P95Ms < 39 || e.P95Ms > 41 {
		t.Errorf("lt edge %+v", e)
	}
	if e := edges["service:shop>service:stock"]; e.Count != 4 || e.MeanMs != 9 { // the caller's view of the call
		t.Errorf("stock edge %+v (all %v)", e, edges)
	}
	if e := edges["service:shop>service:shop"]; e.Count != 4 {
		t.Errorf("self call %+v", e)
	}
	if e := edges["service:shop>http:pay.example"]; e.Count != 4 || e.MeanMs != 5 {
		t.Errorf("pay edge %+v", e)
	}
	if e := edges["service:shop>db:postgres@db1/shop"]; e.Count != 4 || e.P95Ms < 3.9 || e.P95Ms > 4.1 {
		t.Errorf("db edge %+v", e)
	}

	var r run.Result
	c.Finish(&r)
	if r.APM == nil || r.APM.Map == nil || len(r.APM.Map.Edges) != 5 {
		t.Fatalf("finish map %+v", r.APM)
	}
}

func TestErrorGroups(t *testing.T) {
	c := newCollector()
	testClock(c)
	stack := "main.applyDiscount\n\t/src/main.go:42\nmain.main.func9\n\t/src/main.go:200\n"
	sendX(t, c, "shop",
		xSpan{trace: id32(1), id: id16(1), name: "SELECT", kind: 3, startMs: 0, endMs: 1, statusErr: "order 42 not found",
			attrs: map[string]any{"db.system": "postgres"}},
		xSpan{trace: id32(2), id: id16(2), name: "SELECT", kind: 3, startMs: 5, endMs: 6, statusErr: "order 7 not found",
			attrs: map[string]any{"db.system": "postgres"}},
		xSpan{trace: id32(3), id: id16(3), name: "GET /status", kind: 2, startMs: 0, endMs: 1,
			attrs: map[string]any{"http.response.status_code": 503}},
		xSpan{trace: id32(4), id: id16(4), name: "GET /status", kind: 2, startMs: 0, endMs: 1, statusErr: "Service Unavailable",
			attrs: map[string]any{"http.response.status_code": 503}},
		xSpan{trace: id32(5), id: id16(5), name: "GET /panic", kind: 2, startMs: 0, endMs: 3, statusErr: "panic: runtime error: index out of range [5] with length 3",
			attrs: map[string]any{"http.response.status_code": 500},
			exc:   map[string]any{"exception.message": "runtime error: index out of range [5] with length 3", "exception.type": "runtime.boundsError", "exception.stacktrace": stack, "exception.escaped": true}},
		xSpan{trace: id32(6), id: id16(6), name: "GET /panic", kind: 2, startMs: 10, endMs: 13, statusErr: "panic: runtime error: index out of range [7] with length 3",
			attrs: map[string]any{"http.response.status_code": 500},
			exc:   map[string]any{"exception.message": "runtime error: index out of range [7] with length 3", "exception.stacktrace": stack, "exception.escaped": true}},
		xSpan{trace: id32(7), id: id16(7), name: "render", kind: 1, startMs: 0, endMs: 1, // exception event, status unset
			exc: map[string]any{"exception.message": "template: missing key \"price\""}},
		xSpan{trace: id32(8), id: id16(8), name: "GET /ok", kind: 2, startMs: 0, endMs: 1, attrs: map[string]any{"http.response.status_code": 200}},
	)
	got := map[string]run.ErrorGroup{}
	for _, g := range c.Errors() {
		got[g.Service+"|"+g.Source+"|"+g.Message] = g
	}
	if len(got) != 4 {
		t.Errorf("groups %v", got)
	}
	g := got["shop|span|order ? not found"]
	if g.Count != 2 || g.TraceID != id32(2) || !g.First.Before(g.Last) {
		t.Errorf("db group %+v", g)
	}
	if g := got["shop|span|HTTP 503"]; g.Count != 2 {
		t.Errorf("503 group %+v", g)
	}
	if g := got["shop|panic|runtime error: index out of range [?] with length ?"]; g.Count != 2 || g.Stack != stack {
		t.Errorf("panic group %+v", g)
	}
	if g := got[`shop|span|template: missing key "?"`]; g.Count != 1 {
		t.Errorf("event group %+v", got)
	}
	tr := c.Trace(id32(5))
	if a := tr.Spans[0].Attrs; a["exception.message"] != "runtime error: index out of range [5] with length 3" || a["exception.stacktrace"] != stack || a["exception.type"] != "runtime.boundsError" {
		t.Errorf("panic span attrs %v", a)
	}

	// Finish appends to errors already in the report (from logs).
	r := run.Result{APM: &run.APMReport{Errors: []run.ErrorGroup{{Service: "shop", Source: "log", Message: "x", Count: 3}}}}
	c.Finish(&r)
	if len(r.APM.Errors) != 5 || r.APM.Errors[0].Source != "log" {
		t.Errorf("finish errors %+v", r.APM.Errors)
	}
	found := false
	for _, tr := range r.APM.Traces {
		found = found || tr.TraceID == id32(6)
	}
	if !found {
		t.Error("example trace of the panic group not in the report")
	}
}

func TestErrorGroupBound(t *testing.T) {
	c := newCollector()
	testClock(c)
	var spans []xSpan
	for i := 0; i < MaxErrorGroups+20; i++ {
		spans = append(spans, xSpan{trace: id32(i + 1), id: id16(i + 1), name: "x", kind: 1, endMs: 1, statusErr: "failure " + string(rune('a'+i%26)) + string(rune('a'+i/26%26))})
	}
	sendX(t, c, "s", spans...)
	gs := c.Errors()
	if len(gs) != MaxErrorGroups+1 {
		t.Fatalf("%d groups", len(gs))
	}
	other := false
	for _, g := range gs {
		other = other || (g.Message == "(other errors)" && g.Count == 20)
	}
	if !other {
		t.Error("no (other errors) group")
	}
}

func TestNormalizeError(t *testing.T) {
	for in, want := range map[string]string{
		"order 42 not found":                             "order ? not found",
		"dial tcp 10.0.0.12:5432: connect: refused":      "dial tcp ?: connect: refused",
		"user 3f2c9a1b-1d2e-4f5a-8b9c-0d1e2f3a4b5c gone": "user ? gone",
		"bad token 'abc' at 0x1400012ab":                 "bad token '?' at ?",
		"object deadbeef12 missing\nsecond line":         "object ? missing",
		"   spaced    out  ":                             "spaced out",
		"":                                               "error",
	} {
		if got := NormalizeError(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestExceptionEventProto(t *testing.T) {
	c := newCollector()
	ev := protowire.AppendString(protowire.AppendTag(nil, 2, protowire.BytesType), "exception")
	ev = protowire.AppendBytes(protowire.AppendTag(ev, 3, protowire.BytesType), pbKV("exception.message", "boom 12"))
	ev = protowire.AppendBytes(protowire.AppendTag(ev, 3, protowire.BytesType), pbKV("exception.stacktrace", strings.Repeat("x", 20_000)))
	s := pbSpan{trace: tr1, id: sp1, name: "work", kind: 1, start: 1, end: 2}
	b := protowire.AppendBytes(protowire.AppendTag(s.encode(), 11, protowire.BytesType), ev)
	post(t, c, "/v1/traces", "application/x-protobuf", pbRequestRaw("svc", b), false)
	tr := c.Trace(tr1)
	if tr == nil {
		t.Fatal("no trace")
	}
	a := tr.Spans[0].Attrs
	if a["exception.message"] != "boom 12" || len(a["exception.stacktrace"]) != maxStack {
		t.Errorf("attrs %v / stack %d", a["exception.message"], len(a["exception.stacktrace"]))
	}
	if gs := c.Errors(); len(gs) != 1 || gs[0].Message != "boom ?" || len(gs[0].Stack) != maxStack {
		t.Errorf("groups %+v", gs)
	}
}

// pbRequestRaw wraps one encoded span in a request.
func pbRequestRaw(service string, span []byte) []byte {
	res := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), pbKV("service.name", service))
	scope := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), span)
	rs := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), res)
	rs = protowire.AppendBytes(protowire.AppendTag(rs, 2, protowire.BytesType), scope)
	return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), rs)
}

func TestStackInterning(t *testing.T) {
	c := newCollector()
	testClock(c)
	st := strings.Repeat("main.f\n\t/x.go:1\n", 10)
	for i := 0; i < 3; i++ {
		sendX(t, c, "s", xSpan{trace: id32(i + 1), id: id16(i + 1), name: "GET /lock", kind: 2, endMs: 2000, attrs: map[string]any{"lt.stack": st, "http.response.status_code": 200}})
	}
	if len(c.stacks) != 1 {
		t.Errorf("%d distinct stacks", len(c.stacks))
	}
	if a := c.Trace(id32(3)).Spans[0].Attrs; a["lt.stack"] != st {
		t.Errorf("lt.stack %q", a["lt.stack"])
	}
}

func TestActiveStack(t *testing.T) {
	c := newCollector()
	testClock(c)
	big := strings.Repeat("y", 10_000)
	post(t, c, "/v1/lt/active", "application/json", []byte(`{"service":"shop","instance":"i1","active":[{"name":"GET /lock","elapsedMs":2500,"stack":"main.chargeCard\n\t/src/main.go:9\n"},{"name":"GET /x","elapsedMs":1200,"stack":"`+big+`"}]}`), false)
	act := c.Active(0)
	if len(act) != 2 || act[0].Stack != "main.chargeCard\n\t/src/main.go:9\n" || len(act[1].Stack) != maxStack {
		t.Errorf("active %+v", act)
	}
}

func TestMount(t *testing.T) {
	c, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	get := func(path string) (int, string) {
		resp, err := http.Post("http://"+c.Addr()+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/v1/logs"); code != 404 {
		t.Errorf("before mount: %d", code)
	}
	c.Mount("/v1/logs", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "logs") }))
	c.Mount("/extra/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "extra "+r.URL.Path) }))
	if code, body := get("/v1/logs"); code != 200 || body != "logs" {
		t.Errorf("mounted: %d %q", code, body)
	}
	if code, body := get("/extra/a/b"); code != 200 || body != "extra /extra/a/b" {
		t.Errorf("prefix: %d %q", code, body)
	}
	if code, _ := get("/v1/traces"); code != 200 { // built-ins still work
		t.Errorf("traces: %d", code)
	}
	c.Mount("/v1/logs", nil)
	if code, _ := get("/v1/logs"); code != 404 {
		t.Errorf("after unmount: %d", code)
	}
	defer func() {
		if recover() == nil {
			t.Error("mounting over /v1/traces did not panic")
		}
	}()
	c.Mount("/v1/traces", http.NotFoundHandler())
}

func TestMountConcurrent(t *testing.T) {
	c := newCollector()
	done := make(chan bool)
	go func() {
		for i := 0; i < 200; i++ {
			c.Mount(fmt.Sprintf("/m%d", i), http.NotFoundHandler())
		}
		close(done)
	}()
	for {
		select {
		case <-done:
			return
		default:
			post(t, c, "/v1/traces", "application/json", jsonSpanBody("s"), false)
		}
	}
}
