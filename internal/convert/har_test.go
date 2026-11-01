package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

type hEntry struct {
	start, method, url, rtype, mime, body, bodyMime string
	status                                          int
	headers                                         map[string]string
	params                                          map[string]string
}

func makeHAR(t *testing.T, es ...hEntry) string {
	t.Helper()
	var entries []any
	for _, e := range es {
		var hs []map[string]string
		for k, v := range e.headers {
			hs = append(hs, map[string]string{"name": k, "value": v})
		}
		req := map[string]any{"method": e.method, "url": e.url, "headers": hs, "cookies": []any{}}
		if e.body != "" || e.params != nil {
			var ps []map[string]string
			for k, v := range e.params {
				ps = append(ps, map[string]string{"name": k, "value": v})
			}
			req["postData"] = map[string]any{"mimeType": e.bodyMime, "text": e.body, "params": ps}
		}
		if e.status == 0 {
			e.status = 200
		}
		entries = append(entries, map[string]any{
			"startedDateTime": e.start, "time": 10, "_resourceType": e.rtype,
			"request":  req,
			"response": map[string]any{"status": e.status, "headers": []any{}, "content": map[string]any{"mimeType": e.mime}},
		})
	}
	b, _ := json.Marshal(map[string]any{"log": map[string]any{"entries": entries}})
	return string(b)
}

func TestHAR(t *testing.T) {
	har := makeHAR(t,
		hEntry{start: "2026-01-01T00:00:00.000Z", method: "GET", url: "https://app.test/", rtype: "document", mime: "text/html"},
		hEntry{start: "2026-01-01T00:00:00.100Z", method: "GET", url: "https://app.test/main.js", rtype: "script"},
		hEntry{start: "2026-01-01T00:00:00.100Z", method: "GET", url: "https://cdn.test/logo", mime: "image/png"},
		hEntry{start: "2026-01-01T00:00:00.100Z", method: "GET", url: "https://app.test/font.woff2"},
		hEntry{start: "2026-01-01T00:00:00.200Z", method: "GET", url: "https://track.test/p?e=view", rtype: "ping"},
		hEntry{start: "2026-01-01T00:00:03.600Z", method: "POST", url: "https://app.test/api/login", rtype: "fetch",
			body: `{"user":"a","n":1}`, bodyMime: "application/json",
			headers: map[string]string{":authority": "app.test", "content-type": "application/json", "Cookie": "sid=1", "x-csrf-token": "t", "sec-fetch-mode": "cors"}},
		hEntry{start: "2026-01-01T00:00:03.700Z", method: "POST", url: "https://app.test/form", rtype: "document",
			bodyMime: "application/x-www-form-urlencoded", params: map[string]string{"q": "a b"}, status: 302},
		hEntry{start: "2026-01-01T00:00:03.800Z", method: "GET", url: "https://app.test/api/list?tag=a&tag=b", rtype: "xhr", status: 304,
			headers: map[string]string{"if-none-match": "x", "Authorization": "Bearer abc"}},
		hEntry{start: "2026-01-01T00:00:03.900Z", method: "GET", url: "https://app.test/", rtype: "document"},
		hEntry{start: "2026-01-01T00:00:04.000Z", method: "GET", url: "wss://app.test/ws", rtype: "websocket"},
	)
	y, f := convertOK(t, FormatHAR, har, Options{})
	for _, want := range []string{"skipped 3 static", "skipped 1 non-HTTP", "cookies: true", "sleep: 3s"} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in:\n%s", want, y)
		}
	}
	eq(t, f.Vars["baseUrl"], "https://app.test")
	if !f.Options.Cookies {
		t.Error("cookies option not set")
	}
	hs := flowHTTP(t, f)
	if len(hs) != 6 {
		t.Fatalf("want 6 http steps:\n%s", y)
	}
	eq(t, hs[0]["url"], "{{ baseUrl }}/")
	eq(t, hs[0]["check"].(map[string]any)["status"], 200)
	eq(t, hs[1]["url"], "https://track.test/p")
	login := hs[2]
	eq(t, login["method"], "POST")
	eq(t, login["json"].(map[string]any)["n"], 1)
	eq(t, login["headers"], map[string]any{"X-Csrf-Token": "t"})
	eq(t, hs[3]["form"], map[string]any{"q": "a b"})
	eq(t, hs[3]["check"].(map[string]any)["status"], 302)
	list := hs[4]
	eq(t, list["url"], "{{ baseUrl }}/api/list?tag=a&tag=b") // repeated keys stay in the URL
	eq(t, list["headers"], map[string]any{"Authorization": "Bearer abc"})
	eq(t, list["check"].(map[string]any)["status"], 200)
	eq(t, hs[5]["name"], "GET / #2")
}

func TestHAROptions(t *testing.T) {
	har := makeHAR(t,
		hEntry{start: "2026-01-01T00:00:00Z", method: "GET", url: "https://a.test/x.css", mime: "text/css"},
		hEntry{start: "2026-01-01T00:00:00Z", method: "GET", url: "https://b.test/api"},
		hEntry{start: "2026-01-01T00:00:05Z", method: "GET", url: "https://a.test:8443/api"},
	)
	y, f := convertOK(t, FormatHAR, har, Options{All: true, NoSleep: true})
	if strings.Contains(y, "sleep") {
		t.Errorf("unexpected sleep:\n%s", y)
	}
	eq(t, len(flowHTTP(t, f)), 3)

	_, f = convertOK(t, FormatHAR, har, Options{Hosts: []string{"a.test"}})
	hs := flowHTTP(t, f)
	eq(t, len(hs), 1)
	eq(t, f.Vars["baseUrl"], "https://a.test:8443")

	if _, err := HAR([]byte(har), Options{Hosts: []string{"none.test"}}); err == nil {
		t.Error("expected error when nothing is left")
	}
	if _, err := HAR([]byte("not json"), Options{}); err == nil {
		t.Error("expected parse error")
	}
}
