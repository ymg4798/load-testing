package logs

import (
	"strings"
	"testing"
	"time"

	"github.com/ymg4798/load-testing/internal/run"
)

func TestQuery(t *testing.T) {
	lines := []run.LogLine{
		{Source: "api", Level: "error", Message: "Connection refused to db:5432", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"},
		{Source: "api", Level: "info", Message: "GET /items 200", Attrs: map[string]string{"user": "Kim"}},
		{Source: "web-1", Level: "warn", Message: "slow deadline exceeded"},
		{Source: "worker", Level: "fatal", Message: "panic: nil map"},
	}
	cases := map[string][]int{
		"":                                       {0, 1, 2, 3},
		"connection":                             {0},
		"CONNECTION refused":                     {0},
		`"refused to"`:                           {0},
		`"to refused"`:                           nil,
		"kim":                                    {1}, // attributes are searched
		"/dead(lock|line)/":                      {2},
		"/DEAD/":                                 nil,
		"/DEAD/i":                                {2},
		"/GET \\/items/":                         {1},
		"level:error":                            {0},
		"level=error":                            {0},
		"level:warn,error":                       {0, 2},
		"level:err,fatal":                        {0, 3},
		"trace:4bf92f35":                         {0},
		"trace:4BF92F3577B34DA6A3CE929D0E0E4736": {0},
		"source:api":                             {0, 1},
		"source:web-*":                           {2},
		"source:API level:info":                  {1},
		"-level:info source:api":                 {0},
		"-refused":                               {1, 2, 3},
		"level:error nothing-here":               nil,
		"/unclosed":                              nil, // plain word
	}
	for q, want := range cases {
		pq, err := ParseQuery(q)
		if err != nil {
			t.Errorf("%q: %v", q, err)
			continue
		}
		var got []int
		for i := range lines {
			if pq.Match(&lines[i]) {
				got = append(got, i)
			}
		}
		if !equalInts(got, want) {
			t.Errorf("%q matched %v, want %v", q, got, want)
		}
	}
	if _, err := ParseQuery("/(/"); err == nil {
		t.Error("bad regex accepted")
	}
	q, _ := ParseQuery("level:error -level:fatal")
	if !q.levelOnly() || !q.levelOK("error") || q.levelOK("fatal") || q.levelOK("info") {
		t.Error("level-only query")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func feedAll(p *Parser, text string) []run.LogLine {
	var out []run.LogLine
	for _, l := range strings.Split(text, "\n") {
		out = append(out, p.Feed(l)...)
	}
	return append(out, p.Flush()...)
}

func TestParseJSON(t *testing.T) {
	p := &Parser{Source: "app.log"}
	out := feedAll(p, strings.Join([]string{
		`{"time":"2026-11-01T10:00:00.5Z","level":"ERROR","msg":"db failed","trace_id":"4BF92F3577B34DA6A3CE929D0E0E4736","user":"u1","error":"timeout","stack":"goroutine 1 [running]:\nmain.main()"}`,
		`{"@timestamp":"2026-11-01T10:00:01Z","log.level":"warn","message":"slow","trace":{"id":"0af7651916cd43dd8448eb211c80319c"}}`,
		`{"ts":1793527200.25,"severity":"info","event":"started","traceparent":"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}`,
		`{"level":50,"time":1793527200123,"msg":"pino error"}`,
		`{"level":"info","msg":"nested","error":{"message":"boom","stack_trace":"java.lang.X\n\tat a.b"}}`,
	}, "\n"))
	if len(out) != 5 {
		t.Fatalf("%d records: %+v", len(out), out)
	}
	a := out[0]
	if a.Level != "error" || a.Message != "db failed" || a.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" ||
		a.Attrs["error"] != "timeout" || !strings.Contains(a.Attrs["stack"], "main.main()") || a.Attrs["user"] != "u1" || a.Source != "app.log" ||
		!a.Time.Equal(time.Date(2026, 11, 1, 10, 0, 0, 5e8, time.UTC)) {
		t.Errorf("json 0: %+v", a)
	}
	if out[1].Level != "warn" || out[1].Message != "slow" || out[1].TraceID != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("json 1: %+v", out[1])
	}
	if out[2].Level != "info" || out[2].Message != "started" || out[2].TraceID != "0af7651916cd43dd8448eb211c80319c" || out[2].Time.UnixMilli() != 1793527200250 {
		t.Errorf("json 2: %+v", out[2])
	}
	if out[3].Level != "error" || out[3].Time.UnixMilli() != 1793527200123 {
		t.Errorf("pino: %+v", out[3])
	}
	if out[4].Attrs["error"] != "boom" || !strings.Contains(out[4].Attrs["stack"], "at a.b") {
		t.Errorf("nested error: %+v", out[4])
	}
}

func TestParseLogfmt(t *testing.T) {
	p := &Parser{Source: "x"}
	out := feedAll(p, `time=2026-11-01T10:00:00Z level=ERROR msg="query failed: \"users\"" trace_id=4bf92f3577b34da6a3ce929d0e0e4736 err="conn reset" dur=12ms`)
	if len(out) != 1 {
		t.Fatalf("%+v", out)
	}
	l := out[0]
	if l.Level != "error" || l.Message != `query failed: "users"` || l.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || l.Attrs["error"] != "conn reset" || l.Attrs["dur"] != "12ms" || l.Time.Year() != 2026 {
		t.Errorf("logfmt: %+v", l)
	}
	// Prose with an = is not logfmt.
	out = feedAll(&Parser{}, "set x=1 then retry")
	if out[0].Message != "set x=1 then retry" || out[0].Attrs != nil {
		t.Errorf("prose: %+v", out[0])
	}
}

func TestParseTextAndStacks(t *testing.T) {
	in := `2026-11-01 10:00:00,123 INFO  [main] app started
2026-11-01 10:00:01.500 ERROR [http-1] c.e.Shop - checkout failed trace_id=4bf92f3577b34da6a3ce929d0e0e4736
java.lang.IllegalStateException: stock < 0
	at com.example.Shop.buy(Shop.java:42)
	at com.example.Api.handle(Api.java:10)
Caused by: java.sql.SQLException: deadlock
	... 12 more
2026-11-01 10:00:02 WARNING:root:disk 91%
2026-11-01 10:00:03 ERROR:root:task failed
Traceback (most recent call last):
  File "app.py", line 3, in <module>
    run()
ValueError: bad input
[2026-11-01T10:00:04Z] [error] upstream timed out
2026/11/01 10:00:05 user 12 logged in
panic: runtime error: index out of range [5] with length 3

goroutine 1 [running]:
main.main()
	/tmp/x.go:8 +0x1d
exit status 2
E1101 10:00:06.000001   123 server.go:42] glog failure
plain line with traceId 0AF7651916CD43DD8448EB211C80319C inside
request id 0af7651916cd43dd8448eb211c80319c here`
	out := feedAll(&Parser{Source: "s"}, in)
	type want struct{ level, msgPrefix, stackHas string }
	wants := []want{
		{"info", "INFO  [main] app started", ""},
		{"error", "ERROR [http-1] c.e.Shop - checkout failed", "Caused by: java.sql.SQLException: deadlock"},
		{"warn", "WARNING:root:disk", ""},
		{"error", "ERROR:root:task failed", "ValueError: bad input"},
		{"error", "[error] upstream timed out", ""},
		{"", "user 12 logged in", ""},
		{"fatal", "panic: runtime error", "/tmp/x.go:8"},
		{"", "exit status 2", ""},
		{"error", "glog failure", ""},
		{"", "plain line", ""},
		{"", "request id", ""},
	}
	if len(out) != len(wants) {
		for _, l := range out {
			t.Logf("%q %q stack=%q", l.Level, l.Message, l.Attrs["stack"])
		}
		t.Fatalf("%d records, want %d", len(out), len(wants))
	}
	for i, w := range wants {
		l := out[i]
		if l.Level != w.level || !strings.HasPrefix(l.Message, w.msgPrefix) || (w.stackHas != "" && !strings.Contains(l.Attrs["stack"], w.stackHas)) || (w.stackHas == "" && l.Attrs["stack"] != "") {
			t.Errorf("record %d: level %q msg %q stack %q; want %+v", i, l.Level, l.Message, l.Attrs["stack"], w)
		}
	}
	if !out[0].Time.Equal(time.Date(2026, 11, 1, 10, 0, 0, 123e6, time.Local)) {
		t.Errorf("time 0: %v", out[0].Time)
	}
	if out[4].Time.Unix() != time.Date(2026, 11, 1, 10, 0, 4, 0, time.UTC).Unix() {
		t.Errorf("bracketed time: %v", out[4].Time)
	}
	if out[1].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || out[9].TraceID != "0af7651916cd43dd8448eb211c80319c" || out[10].TraceID != "" {
		t.Errorf("trace ids %q %q %q", out[1].TraceID, out[9].TraceID, out[10].TraceID)
	}
	if !strings.Contains(out[6].Attrs["stack"], "goroutine 1 [running]:\nmain.main()") {
		t.Errorf("go stack %q", out[6].Attrs["stack"])
	}
}

func TestParseFormatJSONOnly(t *testing.T) {
	p := &Parser{Format: JSON}
	out := feedAll(p, "{\"msg\":\"a\"}\n  indented is its own line\n{bad json")
	if len(out) != 3 || out[1].Message != "  indented is its own line" {
		t.Errorf("%+v", out)
	}
	out = feedAll(&Parser{Format: Text}, `{"msg":"a"}`)
	if out[0].Message != `{"msg":"a"}` {
		t.Errorf("text format parsed json: %+v", out[0])
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"user 1234 not found":                                         "user <n> not found",
		"order 550e8400-e29b-41d4-a716-446655440000 failed":           "order <uuid> failed",
		`no such table "orders_2026"`:                                 `no such table "…"`,
		"tx 0xdeadbeef rolled back after 1.5s":                        "tx <id> rolled back after <n>",
		"span 4bf92f3577b34da6 timed out":                             "span <id> timed out",
		"dial tcp 10.0.0.12:5432: connect: refused":                   "dial tcp <n>: connect: refused",
		"cafe is open":                                                "cafe is open",
		"http2 stream    error":                                       "http2 stream error",
		"index out of range [5] with length 3\ngoroutine 1 [running]": "index out of range [<n>] with length <n>",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLevels(t *testing.T) {
	for in, want := range map[string]string{"WARNING": "warn", "Err": "error", "CRITICAL": "fatal", "notice": "info", "ERROR2": "error", "trace": "debug", "60": "fatal", "x": ""} {
		if got := normLevel(in); got != want {
			t.Errorf("normLevel(%q) = %q, want %q", in, got, want)
		}
	}
	for n, want := range map[int]string{0: "", 1: "debug", 9: "info", 13: "warn", 17: "error", 21: "fatal"} {
		if got := severityLevel(n); got != want {
			t.Errorf("severity %d = %q", n, got)
		}
	}
}
