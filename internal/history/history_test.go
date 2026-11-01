package history

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/run"
)

func result(name string, at time.Time, p95 int64) *run.Result {
	return &run.Result{Tool: "lt", Version: run.FormatVersion, Passed: true,
		Meta:    run.Meta{Name: name, Started: at},
		Summary: &metrics.Summary{Requests: 10, RPS: 1, Latency: metrics.Stats{P95: p95}}}
}

func TestSaveListBaseline(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	if _, err := Baseline(dir, "my test", "last"); err == nil {
		t.Fatal("no history should be an error")
	}
	p1, err := Save(dir, result("my test", t0, 100))
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := Save(dir, result("my test", t0, 200)) // same second: gets a suffix
	if p1 == p2 || filepath.Base(filepath.Dir(p1)) != "my-test" {
		t.Fatalf("paths %s %s", p1, p2)
	}
	list, err := List(dir, "my test")
	if err != nil || len(list) != 2 {
		t.Fatalf("list %v %v", list, err)
	}
	last, _ := Baseline(dir, "my test", "last")
	if last != p2 {
		t.Fatalf("last = %s, want %s", last, p2)
	}
	if _, err := Baseline(dir, "my test", "pinned"); err == nil {
		t.Fatal("nothing pinned yet")
	}
	if err := Pin(p1); err != nil {
		t.Fatal(err)
	}
	pinned, _ := Baseline(dir, "my test", "pinned")
	if pinned != p1 {
		t.Fatalf("pinned = %s", pinned)
	}
	r, err := run.LoadResult(pinned) // gzipped
	if err != nil || r.Summary.Latency.P95 != 100 {
		t.Fatalf("load %v %v", r, err)
	}
	if tests, _ := Tests(dir); len(tests) != 1 || tests[0] != "my-test" {
		t.Fatalf("tests %v", tests)
	}
}
