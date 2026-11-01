package protocol

import (
	"regexp"
	"testing"
)

func TestTraceHeaders(t *testing.T) {
	vu := NewVU(1, "s", nil, nil)
	if (Tracing{}).NewTrace(vu) != nil {
		t.Fatal("tracing off must send nothing")
	}
	for prop, re := range map[string]string{
		"":       `^traceparent=00-[0-9a-f]{32}-[0-9a-f]{16}-01$`,
		"w3c":    `^traceparent=00-[0-9a-f]{32}-[0-9a-f]{16}-01$`,
		"b3":     `^b3=[0-9a-f]{32}-[0-9a-f]{16}-1$`,
		"jaeger": `^uber-trace-id=[0-9a-f]{32}:[0-9a-f]{16}:0:1$`,
	} {
		tr := Tracing{Enabled: true, Propagator: prop, Sampled: true}.NewTrace(vu)
		h := tr.Headers[0][0] + "=" + tr.Headers[0][1]
		if !regexp.MustCompile(re).MatchString(h) || len(tr.ID) != 32 {
			t.Errorf("%s: %s", prop, h)
		}
	}
	tr := Tracing{Enabled: true}.NewTrace(vu)
	if tr.Headers[0][1][53:] != "00" {
		t.Errorf("unsampled flag: %s", tr.Headers[0][1])
	}
	if ValidPropagator("zipkin") == nil {
		t.Error("unknown propagator accepted")
	}
}
