package convert

import (
	"strings"
	"testing"
	"time"
)

func TestFromRecording(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, err := FromRecording([]Recorded{
		{Started: t0, Duration: 20 * time.Millisecond, Method: "GET", URL: "http://app.test/", Status: 200, ResponseType: "text/html"},
		{Started: t0.Add(30 * time.Millisecond), Method: "GET", URL: "http://app.test/app.js", Status: 200, ResponseType: "application/javascript"},
		{Started: t0.Add(2 * time.Second), Duration: 10 * time.Millisecond, Method: "POST", URL: "http://app.test/api/login",
			Headers: []KV{{"Content-Type", "application/json"}, {"X-Token", "t"}, {"User-Agent", "x"}},
			Body:    `{"user":"kim"}`, ContentType: "application/json", Status: 200, ResponseType: "application/json",
			ResponseHeaders: []KV{{"Set-Cookie", "sid=1"}}},
	}, Options{Name: "rec"})
	if err != nil {
		t.Fatal(err)
	}
	y, err := s.YAML()
	if err != nil {
		t.Fatal(err)
	}
	out := string(y)
	for _, want := range []string{"# Recorded by lt record: 2 request(s).", "skipped 1 static", "baseUrl: http://app.test",
		"sleep: 2s", `url: "{{ baseUrl }}/api/login"`, "json: {user: kim}", "X-Token: t", "cookies: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "app.js") || strings.Contains(out, "User-Agent") {
		t.Errorf("static asset or noise header kept:\n%s", out)
	}
	load(t, y) // valid scenario
	if _, err := FromRecording(nil, Options{}); err == nil {
		t.Error("want error for no entries")
	}
}
