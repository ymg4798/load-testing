package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ymg4798/load-testing/internal/scenario"
)

func fastPolls(t *testing.T) {
	pe, ge, fa, pf := pollEvery, globEvery, flushAfter, partialFlush
	pollEvery, globEvery, flushAfter, partialFlush = 10*time.Millisecond, 30*time.Millisecond, 40*time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { pollEvery, globEvery, flushAfter, partialFlush = pe, ge, fa, pf })
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// waitFor polls the store until query has n matches.
func waitFor(t *testing.T, s *Store, query string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := len(s.Search(query, 100000)); got >= n {
			if got > n {
				t.Fatalf("%q: %d lines, want %d", query, got, n)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q: %d lines, want %d", query, len(s.Search(query, 100000)), n)
}

func TestTailRotationTruncation(t *testing.T) {
	fastPolls(t)
	dir := t.TempDir()
	app := filepath.Join(dir, "app.log")
	appendFile(t, app, "old line before start\n")
	st := NewStore(Options{})
	f, err := FollowFiles(st, []scenario.LogSpec{{File: "{{ name }}.log"}}, dir, map[string]any{"name": "app"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	appendFile(t, app, "first INFO line\nsecond ")
	time.Sleep(30 * time.Millisecond)
	appendFile(t, app, "half\n")
	waitFor(t, st, "line", 1)
	waitFor(t, st, `"second half"`, 1)
	if len(st.Search("old", 10)) != 0 {
		t.Error("read from the start of an existing file")
	}

	// Rotation by rename: lines written to the old file just before still count.
	appendFile(t, app, "before rotate\n")
	if err := os.Rename(app, app+".1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, app+".1", "late write to rotated\n")
	appendFile(t, app, "after rotate\n")
	waitFor(t, st, "after rotate", 1)
	waitFor(t, st, "before rotate", 1)
	waitFor(t, st, "late write", 1)

	// Truncation (copytruncate).
	if err := os.Truncate(app, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	appendFile(t, app, "after truncate\n")
	waitFor(t, st, "after truncate", 1)

	// Recreated after delete.
	os.Remove(app)
	time.Sleep(50 * time.Millisecond)
	appendFile(t, app, "recreated\n")
	waitFor(t, st, "recreated", 1)
	if src := st.Search("recreated", 1)[0].Source; src != "app.log" {
		t.Errorf("source %q", src)
	}
}

func TestTailGlobAndStacks(t *testing.T) {
	fastPolls(t)
	dir := t.TempDir()
	st := NewStore(Options{})
	f, err := FollowFiles(st, []scenario.LogSpec{{Name: "svc", File: filepath.Join(dir, "*.log")}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A file appearing later is read from its start.
	appendFile(t, filepath.Join(dir, "b.log"), "2026-11-01 10:00:00 ERROR boom\njava.lang.RuntimeException: x\n\tat a.B.c(B.java:1)\n")
	waitFor(t, st, "level:error source:svc", 1)
	l := st.Search("boom", 1)[0]
	if !strings.Contains(l.Attrs["stack"], "at a.B.c") {
		t.Errorf("stack not joined: %+v", l)
	}
	// Unterminated last line is taken after a quiet spell; Close flushes the rest.
	appendFile(t, filepath.Join(dir, "c.log"), `{"level":"warn","msg":"no newline"}`)
	waitFor(t, st, "no newline", 1)
	appendFile(t, filepath.Join(dir, "b.log"), "WARN pending at close\n")
	time.Sleep(30 * time.Millisecond)
	f.Close()
	waitFor(t, st, "pending at close", 1)
	if _, err := FollowFiles(st, []scenario.LogSpec{{File: "[bad"}}, "", nil); err == nil {
		t.Error("bad glob accepted")
	}
}

func TestTailThroughput(t *testing.T) {
	fastPolls(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "busy.log")
	appendFile(t, path, "")
	st := NewStore(Options{})
	f, err := FollowFiles(st, []scenario.LogSpec{{File: path}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	const n = 50_000
	start := time.Now()
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"time":"2026-11-01T10:00:00Z","level":"info","msg":"req %d","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}`+"\n", i)
		if i%1000 == 999 {
			w.WriteString(b.String())
			b.Reset()
		}
	}
	w.Close()
	deadline := time.Now().Add(10 * time.Second)
	for st.Stats().Total < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := st.Stats().Total; got != n {
		t.Fatalf("ingested %d of %d", got, n)
	}
	t.Logf("%d JSON lines in %v", n, time.Since(start))

	// Falling far behind skips to the end and counts what it dropped.
	f.Close()
	old := maxBacklog
	maxBacklog = 1 << 20
	defer func() { maxBacklog = old }()
	st2 := NewStore(Options{})
	f2, _ := FollowFiles(st2, []scenario.LogSpec{{File: path}}, "", nil)
	defer f2.Close()
	appendFile(t, path, strings.Repeat("filler line that nobody reads\n", 80_000)) // ~2.4MB in one go
	appendFile(t, path, "tail end\n")
	waitFor(t, st2, "tail end", 1)
	if d, kept := st2.Stats().Dropped, int64(len(st2.Search("filler", 100_000))); d+kept != 80_000 {
		t.Errorf("dropped %d + kept %d, want 80000", d, kept)
	}
	if n := len(st2.Search("filler", 100_000)); n == 0 || n > 10_000 {
		t.Errorf("kept %d filler lines, want the newest ~8.7k", n)
	}
}
