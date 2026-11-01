package cluster

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/scenario"
)

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestDistributedRun(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var agents []string
	for i := 0; i < 3; i++ {
		addr := freeAddr(t)
		a := &Agent{Token: "t"}
		go func() { _ = a.Serve(ctx, addr) }()
		agents = append(agents, addr)
	}
	time.Sleep(200 * time.Millisecond)

	dir := t.TempDir()
	p := filepath.Join(dir, "s.yaml")
	_ = os.WriteFile(p, []byte(`
name: dist
scenarios:
  a:
    rate: 90
    duration: 2s
    flow:
      - http: { name: hit, url: "{{ base }}/" }
  b:
    users: 4
    duration: 2s
    flow:
      - http: { name: user, url: "{{ base }}/u" }
      - sleep: 100ms
`), 0o644)
	vars := map[string]string{"base": srv.URL}
	f, err := scenario.Load(p, vars)
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Agents: agents, Token: "t", StartDelay: 500 * time.Millisecond}
	res, err := c.Run(ctx, f, vars)
	if err != nil {
		t.Fatal(err)
	}
	var arrivals, users int64
	for _, k := range res.Summary.Keys {
		switch k.Step {
		case "hit":
			arrivals += k.Count
		case "user":
			users += k.Count
		}
	}
	if arrivals < 178 || arrivals > 182 {
		t.Fatalf("arrival-rate requests %d, want ~180", arrivals)
	}
	if res.Meta.Agents != 3 || res.Summary.Gen.Agents != 3 {
		t.Fatalf("agents meta %d / gen %d", res.Meta.Agents, res.Summary.Gen.Agents)
	}
	if users < 60 || users > 90 { // 4 users, ~10 iterations/s each, 2s
		t.Fatalf("user requests %d", users)
	}

	bad := &Controller{Agents: agents[:1], Token: "wrong"}
	if _, err := bad.Run(ctx, f, vars); err == nil {
		t.Fatal("wrong token should fail")
	}
}

func TestBundleSkips(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "s.yaml"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "old.json"), []byte(`{"tool":"lt","version":1}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "data.json"), []byte(`[{"a":1}]`), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("x"), 0o644)
	files, err := Bundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["s.yaml"]; !ok || len(files) != 2 {
		t.Fatalf("bundle = %v", keys(files))
	}
	if _, ok := files["old.json"]; ok {
		t.Fatal("result files should be skipped")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
