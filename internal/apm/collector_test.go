package apm

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/run"
)

// ---- protobuf builders ----

func pbKV(k string, v any) []byte {
	var val []byte
	switch x := v.(type) {
	case string:
		val = protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), x)
	case bool:
		val = protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), protowire.EncodeBool(x))
	case int:
		val = protowire.AppendVarint(protowire.AppendTag(nil, 3, protowire.VarintType), uint64(x))
	case float64:
		val = protowire.AppendFixed64(protowire.AppendTag(nil, 4, protowire.Fixed64Type), math.Float64bits(x))
	}
	b := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), k)
	return protowire.AppendBytes(protowire.AppendTag(b, 2, protowire.BytesType), val)
}

type pbSpan struct {
	trace, id, parent string
	name              string
	kind              int
	start, end        uint64
	attrs             [][]byte
	statusErr         bool
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func (s pbSpan) encode() []byte {
	b := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), mustHex(s.trace))
	b = protowire.AppendBytes(protowire.AppendTag(b, 2, protowire.BytesType), mustHex(s.id))
	b = protowire.AppendString(protowire.AppendTag(b, 3, protowire.BytesType), "rojo=1")
	if s.parent != "" {
		b = protowire.AppendBytes(protowire.AppendTag(b, 4, protowire.BytesType), mustHex(s.parent))
	}
	b = protowire.AppendString(protowire.AppendTag(b, 5, protowire.BytesType), s.name)
	b = protowire.AppendVarint(protowire.AppendTag(b, 6, protowire.VarintType), uint64(s.kind))
	b = protowire.AppendFixed64(protowire.AppendTag(b, 7, protowire.Fixed64Type), s.start)
	b = protowire.AppendFixed64(protowire.AppendTag(b, 8, protowire.Fixed64Type), s.end)
	for _, a := range s.attrs {
		b = protowire.AppendBytes(protowire.AppendTag(b, 9, protowire.BytesType), a)
	}
	// an event, which the decoder must skip
	b = protowire.AppendBytes(protowire.AppendTag(b, 11, protowire.BytesType), protowire.AppendString(protowire.AppendTag(nil, 2, protowire.BytesType), "evt"))
	if s.statusErr {
		st := protowire.AppendString(protowire.AppendTag(nil, 2, protowire.BytesType), "boom")
		st = protowire.AppendVarint(protowire.AppendTag(st, 3, protowire.VarintType), 2)
		b = protowire.AppendBytes(protowire.AppendTag(b, 15, protowire.BytesType), st)
	}
	b = protowire.AppendFixed32(protowire.AppendTag(b, 16, protowire.Fixed32Type), 1)
	return b
}

func pbRequest(service string, spans ...pbSpan) []byte {
	res := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), pbKV("service.name", service))
	res = protowire.AppendBytes(protowire.AppendTag(res, 1, protowire.BytesType), pbKV("host.name", "h1"))
	scope := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "lib"))
	for _, s := range spans {
		scope = protowire.AppendBytes(protowire.AppendTag(scope, 2, protowire.BytesType), s.encode())
	}
	rs := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), res)
	rs = protowire.AppendBytes(protowire.AppendTag(rs, 2, protowire.BytesType), scope)
	rs = protowire.AppendString(protowire.AppendTag(rs, 3, protowire.BytesType), "https://opentelemetry.io/schemas/1.26.0")
	return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), rs)
}

func post(t testing.TB, c *Collector, path, ct string, body []byte, gz bool) *httptest.ResponseRecorder {
	t.Helper()
	if gz {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = buf.Bytes()
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	w := httptest.NewRecorder()
	c.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	return w
}

const (
	tr1 = "0af7651916cd43dd8448eb211c80319c"
	sp1 = "b7ad6b7169203331"
	sp2 = "00f067aa0ba902b7"
	sp3 = "1111111111111111"
)

func testClock(c *Collector) *time.Time {
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }
	return &now
}

func TestProtobufDecode(t *testing.T) {
	for _, gz := range []bool{false, true} {
		c := newCollector()
		start := uint64(1_800_000_000_000_000_000)
		body := pbRequest("checkout",
			pbSpan{trace: tr1, id: sp1, parent: "aaaaaaaaaaaaaaaa", name: "GET /cart/{id}", kind: 2, start: start, end: start + 30e6,
				attrs: [][]byte{pbKV("http.request.method", "GET"), pbKV("http.route", "/cart/{id}"), pbKV("http.response.status_code", 503), pbKV("ignored.attr", "x")}},
			pbSpan{trace: tr1, id: sp2, parent: sp1, name: "SELECT", kind: 3, start: start + 5e6, end: start + 25e6, statusErr: true,
				attrs: [][]byte{pbKV("db.system", "postgresql"), pbKV("db.statement", "SELECT * FROM cart WHERE id = 42 AND name = 'bob'"), pbKV("db.ok", true), pbKV("ratio", 0.5)}},
			pbSpan{trace: tr1, id: sp3, parent: sp1, name: "HTTP GET", kind: 3, start: start + 26e6, end: start + 28e6,
				attrs: [][]byte{pbKV("http.request.method", "GET"), pbKV("server.address", "inventory"), pbKV("server.port", 8080)}},
		)
		w := post(t, c, "/v1/traces", "application/x-protobuf", body, gz)
		if ct := w.Header().Get("Content-Type"); ct != "application/x-protobuf" || w.Body.Len() != 0 {
			t.Fatalf("response %q %q", ct, w.Body.String())
		}
		tr := c.Trace(strings.ToUpper(tr1))
		if tr == nil || len(tr.Spans) != 3 {
			t.Fatalf("trace: %+v", tr)
		}
		srv, db, out := tr.Spans[0], tr.Spans[1], tr.Spans[2]
		if srv.Kind != KindServer || srv.Service != "checkout" || !srv.Error || srv.StartUs != 0 || srv.DurUs != 30_000 || srv.Attrs["http.route"] != "/cart/{id}" || srv.Attrs["http.response.status_code"] != "503" {
			t.Errorf("server span %+v", srv)
		}
		if _, ok := srv.Attrs["ignored.attr"]; ok {
			t.Errorf("unwanted attr kept")
		}
		if db.Kind != KindDB || db.ParentID != sp1 || !db.Error || db.StartUs != 5000 || db.Attrs["db.system"] != "postgresql" || db.Attrs["exception.message"] != "boom" {
			t.Errorf("db span %+v", db)
		}
		if out.Kind != KindHTTP {
			t.Errorf("http span %+v", out)
		}
		ops := map[string]run.OpStat{}
		for _, o := range c.Operations() {
			ops[o.Kind+" "+o.Name] = o
		}
		for _, want := range []string{"server GET /cart/{id}", "db SELECT * FROM cart WHERE id = ? AND name = ?", "http GET inventory:8080"} {
			if _, ok := ops[want]; !ok {
				t.Errorf("missing op %q in %v", want, ops)
			}
		}
		if o := ops["server GET /cart/{id}"]; o.Count != 1 || o.Errors != 1 || o.MaxMs != 30 || math.Abs(o.P95Ms-30) > 0.5 {
			t.Errorf("server op %+v", o)
		}
	}
}

func TestProtobufTruncated(t *testing.T) {
	c := newCollector()
	body := pbRequest("x", pbSpan{trace: tr1, id: sp1, name: "a", kind: 2, start: 1, end: 2})
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body[:len(body)-3]))
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	c.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code %d", w.Code)
	}
}

func TestJSONDecode(t *testing.T) {
	c := newCollector()
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"py-api"}}]},
	 "scopeSpans":[{"scope":{"name":"x"},"spans":[
	  {"traceId":"` + strings.ToUpper(tr1) + `","spanId":"` + sp1 + `","name":"GET /users","kind":"SPAN_KIND_SERVER",
	   "startTimeUnixNano":"1800000000000000000","endTimeUnixNano":"1800000000012000000",
	   "attributes":[{"key":"http.method","value":{"stringValue":"GET"}},{"key":"http.status_code","value":{"intValue":"200"}},{"key":"http.target","value":{"stringValue":"/users?x=1"}}],
	   "status":{}},
	  {"traceId":"` + tr1 + `","spanId":"` + sp2 + `","parentSpanId":"` + sp1 + `","name":"redis GET","kind":3,
	   "startTimeUnixNano":1800000000001000000,"endTimeUnixNano":"1800000000002000000",
	   "attributes":[{"key":"db.system","value":{"stringValue":"redis"}},{"key":"db.query.text","value":{"stringValue":"GET user:1"}},{"key":"n","value":{"doubleValue":1.5}},{"key":"b","value":{"boolValue":true}}],
	   "status":{"code":"STATUS_CODE_ERROR","message":"timeout"}}
	 ]}]}]}`
	w := post(t, c, "/v1/traces", "application/json", []byte(body), false)
	if w.Body.String() != "{}" {
		t.Fatalf("response %q", w.Body.String())
	}
	// snake_case field names and base64 IDs
	b64 := "CvdlGRbNQ92ESOshHIAxnA==" // tr1
	snake := `{"resource_spans":[{"resource":{"attributes":[{"key":"service.name","value":{"string_value":"py-api"}}]},
	 "scope_spans":[{"spans":[{"trace_id":"` + b64 + `","span_id":"` + sp3 + `","parent_span_id":"` + sp1 + `","name":"internal","kind":1,
	 "start_time_unix_nano":"1800000000003000000","end_time_unix_nano":"1800000000004000000"}]}]}]}`
	post(t, c, "/v1/traces", "application/json", []byte(snake), true)

	tr := c.Trace(tr1)
	if tr == nil || len(tr.Spans) != 3 {
		t.Fatalf("trace %+v", tr)
	}
	if s := tr.Spans[0]; s.Kind != KindServer || s.Service != "py-api" || s.Error || s.DurUs != 12000 || s.Attrs["http.status_code"] != "200" {
		t.Errorf("server %+v", s)
	}
	if s := tr.Spans[1]; s.Kind != KindDB || !s.Error || s.StartUs != 1000 {
		t.Errorf("db %+v", s)
	}
	if s := tr.Spans[2]; s.Kind != KindInternal || s.ParentID != sp1 {
		t.Errorf("internal %+v", s)
	}
	names := map[string]bool{}
	for _, o := range c.Operations() {
		names[o.Name] = true
	}
	if !names["GET /users"] || !names["GET user:?"] {
		t.Errorf("ops %v", names)
	}
	if st := c.Stats(); st.Spans != 3 || st.Traces != 1 {
		t.Errorf("stats %+v", st)
	}
}

func jsonSpanBody(service string, spans ...string) []byte {
	return []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"` + service + `"}}]},"scopeSpans":[{"spans":[` + strings.Join(spans, ",") + `]}]}]}`)
}

func tSpan(trace, id, parent, name string, kind int, startMs, endMs int64, status int) string {
	base := int64(1_800_000_000_000)
	return fmt.Sprintf(`{"traceId":%q,"spanId":%q,"parentSpanId":%q,"name":%q,"kind":%d,"startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":[{"key":"http.response.status_code","value":{"intValue":"%d"}}]}`,
		trace, id, parent, name, kind, (base+startMs)*1e6, (base+endMs)*1e6, status)
}

func id16(n int) string { return fmt.Sprintf("%016x", n) }
func id32(n int) string { return fmt.Sprintf("%032x", n) }

func TestDecorateHits(t *testing.T) {
	c := newCollector()
	now := testClock(c)
	send := func(svc string, spans ...string) {
		post(t, c, "/v1/traces", "application/json", jsonSpanBody(svc, spans...), false)
	}

	// a: root (no parent), 20ms, ok
	send("web", tSpan(id32(1), id16(1), "", "GET /a", 2, 0, 20, 200))
	// b: parent is lt's span, never reported: waits, then counts; fails with 503
	send("web", tSpan(id32(2), id16(2), id16(99), "GET /b", 2, 0, 300, 503))
	// c: server span of "api" whose parent is a client span of "web" already seen
	send("web", tSpan(id32(3), id16(30), "", "GET /c", 2, 0, 50, 200), tSpan(id32(3), id16(31), id16(30), "call api", 3, 5, 40, 200))
	send("api", tSpan(id32(3), id16(32), id16(31), "GET /api", 2, 10, 35, 200))
	// d: self call: server span arrives before its parent client span of the same service
	send("web", tSpan(id32(4), id16(42), id16(41), "GET /fast", 2, 2, 4, 200))

	var s metrics.Snapshot
	c.Decorate(&s)
	if s.APMHits == nil {
		t.Fatal("no hits")
	}
	count := func(h *metrics.Hits) (ok, bad int32) {
		for _, v := range h.OK {
			ok += v
		}
		for _, v := range h.Err {
			bad += v
		}
		return
	}
	// a and web /c have no parent; api /api has a known parent in another
	// service. b and the self call wait for their parents.
	if ok, bad := count(s.APMHits); ok != 3 || bad != 0 {
		t.Fatalf("first decorate ok=%d bad=%d samples=%+v", ok, bad, s.APMHits.Samples)
	}
	if s.Server["web apm tps"] != 2 || s.Server["api apm tps"] != 1 || s.Server["web apm errors"] != 0 {
		t.Errorf("series %v", s.Server)
	}
	if got := s.Server["web apm mean"]; got != 35 {
		t.Errorf("web mean %v", got)
	}

	// the self call's parent arrives: same service, so not a transaction
	send("web", tSpan(id32(4), id16(41), id16(40), "GET 127.0.0.1", 3, 1, 5, 200), tSpan(id32(4), id16(40), "", "GET /order", 2, 0, 10, 200))
	*now = now.Add(time.Second)
	s = metrics.Snapshot{Server: map[string]float64{"cpu": 12}}
	c.Decorate(&s)
	ok, bad := count(s.APMHits)
	if ok != 1 || bad != 1 {
		t.Fatalf("second decorate ok=%d bad=%d %+v", ok, bad, s.APMHits.Samples)
	}
	keys := map[string]metrics.HitSample{}
	for _, h := range s.APMHits.Samples {
		keys[h.Key] = h
	}
	if h := keys["web|GET /b"]; h.Err != "status:503" || h.Latency != 300_000 || h.TraceID != id32(2) || h.Off != 300 {
		t.Errorf("sample b %+v", h)
	}
	if _, ok := keys["web|GET /order"]; !ok {
		t.Errorf("GET /order missing: %v", keys)
	}
	if _, ok := keys["web|GET /fast"]; ok {
		t.Errorf("self call counted as a transaction")
	}
	if s.Server["cpu"] != 12 || s.Server["web apm errors"] != 1 || s.Server["api apm tps"] != 0 {
		t.Errorf("series %v", s.Server)
	}
	series := map[string]string{}
	for _, x := range c.Series() {
		series[x.Name] = x.Unit
	}
	if series["web apm tps"] != "/s" || series["web apm mean"] != "ms" || series["api apm errors"] != "/s" {
		t.Errorf("Series %v", series)
	}

	// nothing new: no hitmap, zero rates
	s = metrics.Snapshot{}
	c.Decorate(&s)
	if s.APMHits != nil || s.Server["web apm tps"] != 0 {
		t.Errorf("idle decorate %+v %v", s.APMHits, s.Server)
	}
	if _, ok := s.Server["web apm mean"]; ok {
		t.Errorf("mean without transactions")
	}
}

func TestDecorateSingleService(t *testing.T) {
	c := newCollector()
	post(t, c, "/v1/traces", "application/json", jsonSpanBody("only", tSpan(id32(1), id16(1), "", "GET /", 2, 0, 8, 200)), false)
	var s metrics.Snapshot
	c.Decorate(&s)
	if s.Server["apm tps"] != 1 || s.Server["apm mean"] != 8 {
		t.Errorf("%v", s.Server)
	}
}

func TestFinishSelectsTraces(t *testing.T) {
	c := newCollector()
	var spans []string
	for i := 1; i <= 6; i++ {
		spans = append(spans, tSpan(id32(i), id16(i), "", fmt.Sprintf("GET /%d", i), 2, 0, int64(i), 200))
	}
	post(t, c, "/v1/traces", "application/json", jsonSpanBody("svc", spans...), false)
	r := &run.Result{
		Summary: &metrics.Summary{Keys: []*metrics.KeySummary{{
			Slowest: []metrics.Exemplar{{TraceID: id32(1)}, {TraceID: strings.ToUpper(id32(2))}},
			Failed:  []metrics.Exemplar{{TraceID: id32(3)}, {TraceID: id32(1)}, {TraceID: id32(77)}},
		}}},
		Snapshots: []*metrics.Snapshot{
			{Hits: &metrics.Hits{Samples: []metrics.HitSample{{TraceID: id32(4)}, {}}}},
			nil,
			{APMHits: &metrics.Hits{Samples: []metrics.HitSample{{TraceID: id32(5)}}}},
		},
	}
	c.Finish(r)
	if r.APM == nil {
		t.Fatal("no report")
	}
	got := map[string]bool{}
	for _, tr := range r.APM.Traces {
		got[tr.TraceID] = true
	}
	if len(r.APM.Traces) != 5 || got[id32(6)] || !got[id32(2)] || !got[id32(5)] {
		t.Errorf("traces %v", got)
	}
	if r.APM.Spans != 6 || len(r.APM.Services) != 1 || len(r.APM.Operations) != 6 || r.APM.Operations[0].Name != "GET /6" {
		t.Errorf("report %+v", r.APM)
	}

	empty := newCollector()
	r2 := &run.Result{Summary: &metrics.Summary{}}
	empty.Finish(r2)
	if r2.APM != nil {
		t.Errorf("empty collector made a report")
	}
}

func TestActiveTTL(t *testing.T) {
	c := newCollector()
	now := testClock(c)
	post(t, c, "/v1/lt/active", "application/json", []byte(`{"service":"web","instance":"h1:1","active":[{"name":"GET /a","elapsedMs":100,"detail":"SELECT 1","traceId":"abc"},{"name":"GET /b","elapsedMs":2500}]}`), false)
	post(t, c, "/v1/lt/active", "application/json", []byte(`{"service":"api","instance":"h2:1","active":[{"name":"POST /x","elapsedMs":900}]}`), false)
	*now = now.Add(2 * time.Second)
	post(t, c, "/v1/lt/active", "application/json", []byte(`{"service":"api","instance":"h2:1","active":[{"name":"POST /y","elapsedMs":10}]}`), false)

	got := c.Active(0)
	if len(got) != 3 || got[0].Name != "GET /b" || got[0].ElapsedMs != 4500 || got[0].Source != "web" || got[0].Who != "h1:1" {
		t.Fatalf("active %+v", got)
	}
	if got[1].Detail != "SELECT 1" || got[1].TraceID != "abc" || got[1].ElapsedMs != 2100 {
		t.Errorf("detail %+v", got[1])
	}
	if l := c.Active(1); len(l) != 1 {
		t.Errorf("limit: %d", len(l))
	}
	*now = now.Add(4 * time.Second) // web's report is 6s old, api's 4s
	got = c.Active(10)
	if len(got) != 1 || got[0].Name != "POST /y" {
		t.Errorf("after TTL %+v", got)
	}
	if svcs := c.Services(); len(svcs) != 2 {
		t.Errorf("services %v", svcs)
	}
}

func TestEviction(t *testing.T) {
	c := newCollector()
	now := testClock(c)
	post(t, c, "/v1/traces", "application/json", jsonSpanBody("s", tSpan(id32(1), id16(1), "", "old", 1, 0, 1, 0)), false)
	*now = now.Add(TraceWindow + time.Second)
	post(t, c, "/v1/traces", "application/json", jsonSpanBody("s", tSpan(id32(2), id16(2), "", "new", 1, 0, 1, 0)), false)
	if c.Trace(id32(1)) != nil || c.Trace(id32(2)) == nil {
		t.Errorf("window eviction")
	}
	// spans per trace cap
	var spans []string
	for i := 0; i < MaxSpansPerTrace+20; i++ {
		spans = append(spans, tSpan(id32(3), id16(1000+i), "", "x", 1, 0, 1, 0))
	}
	post(t, c, "/v1/traces", "application/json", jsonSpanBody("s", spans...), false)
	if n := len(c.Trace(id32(3)).Spans); n != MaxSpansPerTrace {
		t.Errorf("spans kept %d", n)
	}
	// trace count cap
	c2 := newCollector()
	for i := 0; i < MaxTraces+10; i += 500 {
		var batch []string
		for j := i; j < i+500; j++ {
			batch = append(batch, tSpan(id32(j+1), id16(1), "", "x", 1, 0, 1, 0))
		}
		post(t, c2, "/v1/traces", "application/json", jsonSpanBody("s", batch...), false)
	}
	if st := c2.Stats(); st.Traces != MaxTraces || c2.Trace(id32(1)) != nil {
		t.Errorf("trace cap: %+v", st)
	}
}

func TestNormalizeSQL(t *testing.T) {
	cases := map[string]string{
		"SELECT /* 12ms */ *  FROM t\n WHERE id = 42 AND s = 'it''s'":  "SELECT * FROM t WHERE id = ? AND s = ?",
		"select a1, b from t2 where x in (1, 2, 3) -- note\n limit 10": "select a1, b from t2 where x in (?) limit ?",
		"UPDATE t SET v = $1 WHERE id = $2":                            "UPDATE t SET v = ? WHERE id = ?",
		"INSERT INTO t (a,b) VALUES (1.5e3,'x'),(2,'y')":               "INSERT INTO t (a,b) VALUES (?),(?)",
		"SELECT * FROM t WHERE h = 0xFF":                               "SELECT * FROM t WHERE h = ?",
	}
	for in, want := range cases {
		if got := NormalizeSQL(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
	long := "SELECT " + strings.Repeat("col, ", 100) + "x FROM t"
	if got := NormalizeSQL(long); len(got) != maxOpName || !strings.HasSuffix(got, "...") {
		t.Errorf("long: %d %q", len(got), got)
	}
}

func TestListen(t *testing.T) {
	c, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	body := jsonSpanBody("s", tSpan(id32(1), id16(1), "", "GET /", 2, 0, 1, 200))
	resp, err := http.Post("http://"+c.Addr()+"/v1/traces", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || c.Trace(id32(1)) == nil {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp, _ = http.Get("http://" + c.Addr() + "/v1/traces")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status %d", resp.StatusCode)
	}
}

func TestConcurrent(t *testing.T) {
	c := newCollector()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				n := g*1000 + i + 1
				post(t, c, "/v1/traces", "application/x-protobuf", benchRequest(n, 10), false)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				var s metrics.Snapshot
				c.Decorate(&s)
				c.Operations()
				c.Active(10)
				c.Trace(id32(1001))
			}
		}
	}()
	wg.Wait()
	close(done)
	if st := c.Stats(); st.Spans != 8*50*10 {
		t.Errorf("spans %d", st.Spans)
	}
}

// benchRequest is a protobuf request with one trace of n spans: a server
// span and n-1 db and http children.
func benchRequest(trace, n int) []byte {
	start := uint64(1_800_000_000_000_000_000)
	var tid [16]byte
	binary.BigEndian.PutUint64(tid[8:], uint64(trace))
	tid[0] = 1
	th := hex.EncodeToString(tid[:])
	root := id16(trace<<8 | 1)
	spans := []pbSpan{{trace: th, id: root, parent: "aaaaaaaaaaaaaaaa", name: "GET /order/{id}", kind: 2, start: start, end: start + 20e6,
		attrs: [][]byte{pbKV("http.request.method", "GET"), pbKV("http.route", "/order/{id}"), pbKV("http.response.status_code", 200)}}}
	for i := 1; i < n; i++ {
		s := pbSpan{trace: th, id: id16(trace<<8 | (i + 1)), parent: root, kind: 3, start: start + uint64(i)*1e6, end: start + uint64(i)*1e6 + 5e5}
		if i%3 == 0 {
			s.name = "GET"
			s.attrs = [][]byte{pbKV("http.request.method", "GET"), pbKV("server.address", "stock")}
		} else {
			s.name = "SELECT"
			s.attrs = [][]byte{pbKV("db.system", "mysql"), pbKV("db.statement", fmt.Sprintf("SELECT * FROM items WHERE order_id = %d AND kind = 'x'", trace))}
		}
		spans = append(spans, s)
	}
	return pbRequest("bench", spans...)
}

func BenchmarkIngest(b *testing.B) {
	const perReq = 50
	reqs := make([][]byte, 256)
	for i := range reqs {
		reqs[i] = benchRequest(i+1, perReq)
	}
	c := newCollector()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		post(b, c, "/v1/traces", "application/x-protobuf", reqs[i%len(reqs)], false)
		if i%1000 == 0 { // about once a second at this rate
			var s metrics.Snapshot
			c.Decorate(&s)
		}
	}
	b.ReportMetric(float64(b.N*perReq)/b.Elapsed().Seconds(), "spans/s")
}

func BenchmarkIngestParallel(b *testing.B) {
	const perReq = 50
	reqs := make([][]byte, 4096)
	for i := range reqs {
		reqs[i] = benchRequest(i+1, perReq)
	}
	c := newCollector()
	b.ReportAllocs()
	b.ResetTimer()
	var mu sync.Mutex
	n := 0
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			i := n
			n++
			mu.Unlock()
			post(b, c, "/v1/traces", "application/x-protobuf", reqs[i%len(reqs)], false)
		}
	})
	b.ReportMetric(float64(b.N*perReq)/b.Elapsed().Seconds(), "spans/s")
}
