package logs

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"math"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func msg(fs ...[]byte) []byte { return bytes.Join(fs, nil) }

func fBytes(n protowire.Number, v []byte) []byte {
	b := protowire.AppendTag(nil, n, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func fStr(n protowire.Number, s string) []byte { return fBytes(n, []byte(s)) }

func fVarint(n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, n, protowire.VarintType), v)
}

func fFixed64(n protowire.Number, v uint64) []byte {
	return protowire.AppendFixed64(protowire.AppendTag(nil, n, protowire.Fixed64Type), v)
}

func fFixed32(n protowire.Number, v uint32) []byte {
	return protowire.AppendFixed32(protowire.AppendTag(nil, n, protowire.Fixed32Type), v)
}

func anyStr(s string) []byte { return fStr(1, s) }

func kv(k string, anyValue []byte) []byte { return msg(fStr(1, k), fBytes(2, anyValue)) }

func TestOTLPProto(t *testing.T) {
	trace, _ := hex.DecodeString("4bf92f3577b34da6a3ce929d0e0e4736")
	span, _ := hex.DecodeString("00f067aa0ba902b7")
	kvlist := fBytes(6, msg( // KeyValueList
		fBytes(1, kv("event", anyStr("login"))),
		fBytes(1, kv("n", fVarint(3, 7))),
		fBytes(1, kv("ok", fVarint(2, 1))),
		fBytes(1, kv("ratio", fFixed64(4, math.Float64bits(0.5)))),
		fBytes(1, kv("tags", fBytes(5, msg(fBytes(1, anyStr("a")), fBytes(1, anyStr("b")))))),
	))
	rec1 := msg(
		fFixed64(1, 1793527200_000000000),
		fVarint(2, 17), // ERROR
		fStr(3, "Error"),
		fBytes(5, anyStr("payment failed for order 42")),
		fBytes(6, kv("exception.type", anyStr("PaymentError"))),
		fBytes(6, kv("exception.message", anyStr("card declined"))),
		fBytes(6, kv("exception.stacktrace", anyStr("PaymentError: card declined\n  at pay (pay.js:10)"))),
		fBytes(6, kv("http.route", anyStr("/pay"))),
		fFixed32(8, 1),
		fBytes(9, trace),
		fBytes(10, span),
	)
	rec2 := msg(
		fFixed64(11, 1793527201_000000000), // observed time only
		fVarint(2, 9),
		fBytes(5, kvlist),
	)
	req := fBytes(1, msg( // ResourceLogs
		fBytes(1, fBytes(1, kv("service.name", anyStr("shop")))),
		fBytes(2, msg(fBytes(1, fStr(1, "scope")), fBytes(2, rec1), fBytes(2, rec2))),
	))
	req = append(req, fBytes(1, fBytes(2, fBytes(2, fVarint(2, 13))))...) // no service, warn, empty body

	lines, err := DecodeOTLPProto(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	a := lines[0]
	if a.Source != "shop" || a.Level != "error" || a.Message != "payment failed for order 42" ||
		a.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || a.Time.Unix() != 1793527200 ||
		a.Attrs["error"] != "PaymentError: card declined" || !strings.Contains(a.Attrs["stack"], "pay.js:10") || a.Attrs["http.route"] != "/pay" {
		t.Errorf("rec1 %+v", a)
	}
	b := lines[1]
	if b.Level != "info" || b.Time.Unix() != 1793527201 || b.Message != `{"event":"login","n":7,"ok":true,"ratio":0.5,"tags":["a","b"]}` {
		t.Errorf("rec2 %+v", b)
	}
	if lines[2].Source != "unknown_service" || lines[2].Level != "warn" {
		t.Errorf("rec3 %+v", lines[2])
	}
	if _, err := DecodeOTLPProto([]byte{0x0a, 0x05, 0x01}); err == nil {
		t.Error("truncated accepted")
	}
}

const otlpJSON = `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}}]},
 "scopeLogs":[{"scope":{"name":"x"},"logRecords":[
  {"timeUnixNano":"1793527200000000000","severityNumber":"SEVERITY_NUMBER_FATAL","body":{"stringValue":"out of memory"},
   "traceId":"0AF7651916CD43DD8448EB211C80319C","attributes":[{"key":"pid","value":{"intValue":"12"}}]},
  {"observedTimeUnixNano":1793527201000000000,"severityText":"WARNING","body":{"kvlistValue":{"values":[{"key":"msg","value":{"stringValue":"slow"}}]}}}
 ]}]},
 {"resource_logs_ignored":1,"scope_logs":[{"log_records":[{"severity_number":5,"body":{"string_value":"snake"}}]}]}]}`

func TestOTLPJSON(t *testing.T) {
	lines, err := DecodeOTLPJSON([]byte(otlpJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("%d lines: %+v", len(lines), lines)
	}
	if l := lines[0]; l.Source != "api" || l.Level != "fatal" || l.Message != "out of memory" || l.TraceID != "0af7651916cd43dd8448eb211c80319c" || l.Attrs["pid"] != "12" || l.Time.Unix() != 1793527200 {
		t.Errorf("0: %+v", l)
	}
	if l := lines[1]; l.Level != "warn" || l.Message != `{"msg":"slow"}` || l.Time.Unix() != 1793527201 {
		t.Errorf("1: %+v", l)
	}
	if l := lines[2]; l.Level != "debug" || l.Message != "snake" || l.Source != "unknown_service" {
		t.Errorf("2: %+v", l)
	}
	if _, err := DecodeOTLPJSON([]byte("{")); err == nil {
		t.Error("bad json accepted")
	}
}

func TestOTLPServe(t *testing.T) {
	st := NewStore(Options{})
	srv, err := Serve("127.0.0.1:0", st)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	url := "http://" + srv.Addr() + "/v1/logs"

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(otlpJSON))
	zw.Close()
	req, _ := http.NewRequest(http.MethodPost, url, &gz)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("json status %d", resp.StatusCode)
	}
	pb := fBytes(1, msg(fBytes(1, fBytes(1, kv("service.name", anyStr("pb")))), fBytes(2, fBytes(2, msg(fVarint(2, 17), fBytes(5, anyStr("proto line")))))))
	resp, err = http.Post(url, "application/x-protobuf", bytes.NewReader(pb))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("proto status %d", resp.StatusCode)
	}
	resp, _ = http.Post(url, "application/x-protobuf", strings.NewReader("\x0a\x09xx"))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("bad body status %d", resp.StatusCode)
	}
	resp, _ = http.Get(url)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status %d", resp.StatusCode)
	}
	if got := st.Search("source:pb level:error", 10); len(got) != 1 || got[0].Message != "proto line" {
		t.Errorf("proto ingested %+v", got)
	}
	if st.Stats().Total != 4 || len(st.ErrorGroups()) != 2 {
		t.Errorf("stats %+v groups %+v", st.Stats(), st.ErrorGroups())
	}
}
