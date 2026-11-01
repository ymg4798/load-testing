package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/ymg4798/load-testing/internal/scenario"
)

func target(t *testing.T) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-" + in["id"], "items": []int{7, 8}})
	})
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"pick":` + r.URL.Query().Get("pick") + `}`))
	})
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(503) })
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, m)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func load(t *testing.T, yaml string, files map[string]string, vars map[string]string) *scenario.File {
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "s.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := scenario.Load(p, vars)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func key(r *Result, step string) (count, errs int64) {
	for _, k := range r.Summary.Keys {
		if k.Step == step {
			count += k.Count
			errs += k.Errors
		}
	}
	return
}

func TestFlowEndToEnd(t *testing.T) {
	srv, hits := target(t)
	f := load(t, `
name: e2e
data:
  users: { rows: [{id: a}, {id: b}] }
scenarios:
  main:
    rate: 50
    duration: 2s
    flow:
      - http:
          name: login
          method: POST
          url: "{{ base }}/login"
          json: { id: "{{ users.id }}" }
          check: { status: 200, json: { "$.token": "tok-{{ users.id }}" } }
          extract: { token: $.token }
      - script: h.js#pick
      - http:
          name: me
          url: "{{ base }}/me?pick={{ pick }}"
          headers: { Authorization: "Bearer {{ token }}" }
          check: { json: { "$.ok": true, "$.pick": "{{ pick }}" } }
      - ws:
          name: sock
          url: "{{ wsBase }}/ws"
          actions:
            - send: "hello {{ users.id }}"
            - expect: { name: echo, contains: "hello {{ users.id }}", timeout: 2s }
thresholds:
  errors: ["rate == 0"]
  "latency{step:me}": ["p99 < 1s"]
`, map[string]string{"h.js": `export function pick(ctx) { ctx.vars.pick = ctx.last.json.items[1]; }`},
		map[string]string{"base": srv.URL, "wsBase": "ws" + strings.TrimPrefix(srv.URL, "http")})

	res, err := Local(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("thresholds failed: %+v", res.Thresholds)
	}
	n, e := key(res, "me")
	if n != 100 || e != 0 {
		t.Fatalf("me: %d requests, %d errors; want 100, 0", n, e)
	}
	if n, _ := key(res, "sock:echo"); n != 100 {
		t.Fatalf("ws echo count %d", n)
	}
	if hits.Load() != 200 {
		t.Fatalf("server saw %d hits, want 200", hits.Load())
	}
}

func TestFailuresAbortIteration(t *testing.T) {
	srv, _ := target(t)
	f := load(t, `
scenarios:
  main:
    executor: constant-users
    users: 2
    duration: 1s
    flow:
      - http: { name: bad, url: "{{ base }}/fail" }
      - http: { name: never, url: "{{ base }}/me" }
      - sleep: 50ms
thresholds:
  errors: ["rate < 1%"]
`, nil, map[string]string{"base": srv.URL})
	res, err := Local(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("thresholds should fail")
	}
	n, e := key(res, "bad")
	if n == 0 || n != e {
		t.Fatalf("bad: %d requests %d errors", n, e)
	}
	if n, _ := key(res, "never"); n != 0 {
		t.Fatalf("step after a failure ran %d times", n)
	}
	for _, k := range res.Summary.Keys {
		if k.Step == "bad" && k.ErrKinds["status:503"] != k.Errors {
			t.Fatalf("error kinds %v", k.ErrKinds)
		}
	}
}

func TestDroppedWhenSaturated(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	f := load(t, `
options: { gracefulStop: 100ms, timeout: 5s }
scenarios:
  main:
    rate: 100
    duration: 1s
    maxInFlight: 10
    flow:
      - http: "{{ base }}/"
`, nil, map[string]string{"base": srv.URL})
	res, err := Local(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Dropped < 85 || res.Summary.Dropped > 90 {
		t.Fatalf("dropped %d, want 90 (100 arrivals, 10 in flight)", res.Summary.Dropped)
	}
}
