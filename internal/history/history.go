// Package history keeps past results so runs can be compared over time:
// one folder per test name, a gzipped result file per run and a small
// index for listing and trend charts.
//
//	.lt/runs/<test>/index.jsonl
//	.lt/runs/<test>/20261101-153012.json.gz
//	.lt/runs/<test>/pinned          (file name of the baseline run)
package history

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ymg4798/load-testing/internal/run"
)

// DefaultDir is used when neither a flag nor $LT_HISTORY says otherwise.
const DefaultDir = ".lt/runs"

// Dir resolves the history folder.
func Dir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("LT_HISTORY"); v != "" {
		return v
	}
	return DefaultDir
}

// Entry is one run in the index.
type Entry struct {
	File       string    `json:"file"` // name inside the test folder
	Name       string    `json:"name"`
	Started    time.Time `json:"started"`
	Label      string    `json:"label,omitempty"`
	Duration   float64   `json:"duration"`
	Requests   int64     `json:"requests"`
	RPS        float64   `json:"rps"`
	ErrorRate  float64   `json:"errorRate"`
	P50        int64     `json:"p50"`
	P95        int64     `json:"p95"`
	P99        int64     `json:"p99"`
	Passed     bool      `json:"passed"`
	Thresholds int       `json:"thresholds"`
	Stopped    *run.Stop `json:"stopped,omitempty"`
	Pinned     bool      `json:"-"`
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Slug turns a test name into a folder name.
func Slug(name string) string {
	s := strings.Trim(unsafe.ReplaceAllString(name, "-"), "-.")
	if s == "" {
		return "test"
	}
	return s
}

// Save stores a result and returns its path.
func Save(dir string, r *run.Result) (string, error) {
	folder := filepath.Join(dir, Slug(r.Meta.Name))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", err
	}
	base := r.Meta.Started.Format("20060102-150405")
	name := base + ".json.gz"
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(folder, name)); errors.Is(err, os.ErrNotExist) {
			break
		}
		name = fmt.Sprintf("%s-%d.json.gz", base, i)
	}
	path := filepath.Join(folder, name)
	if err := writeGz(path, r); err != nil {
		return "", err
	}
	s := r.Summary
	e := Entry{
		File: name, Name: r.Meta.Name, Started: r.Meta.Started, Label: r.Meta.Label,
		Duration: s.Duration, Requests: s.Requests, RPS: s.RPS, ErrorRate: s.ErrorRate,
		P50: s.Latency.P50, P95: s.Latency.P95, P99: s.Latency.P99,
		Passed: r.Passed, Thresholds: len(r.Thresholds), Stopped: r.Stopped,
	}
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(filepath.Join(folder, "index.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

func writeGz(path string, r *run.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if err := json.NewEncoder(zw).Encode(r); err != nil {
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Tests lists the test names that have history.
func Tests(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "index.jsonl")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// List returns the runs of one test, oldest first.
func List(dir, test string) ([]Entry, error) {
	folder := filepath.Join(dir, Slug(test))
	f, err := os.Open(filepath.Join(folder, "index.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no history for %q in %s", test, dir)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pinned := pinnedName(folder)
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(folder, e.File)); err != nil {
			continue // deleted by hand
		}
		e.Pinned = e.File == pinned
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, sc.Err()
}

func pinnedName(folder string) string {
	b, err := os.ReadFile(filepath.Join(folder, "pinned"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Pin marks a stored run as the baseline of its test.
func Pin(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	folder, file := filepath.Split(path)
	if _, err := os.Stat(filepath.Join(folder, "index.jsonl")); err != nil {
		return fmt.Errorf("%s is not inside a history folder", path)
	}
	return os.WriteFile(filepath.Join(folder, "pinned"), []byte(file+"\n"), 0o644)
}

// Baseline resolves --baseline: "last" (the latest stored run of the
// test), "pinned" (the run marked with Pin), or a file path.
func Baseline(dir, test, which string) (string, error) {
	folder := filepath.Join(dir, Slug(test))
	switch which {
	case "last":
		list, err := List(dir, test)
		if err != nil {
			return "", err
		}
		if len(list) == 0 {
			return "", fmt.Errorf("no stored runs of %q yet (use --save)", test)
		}
		return filepath.Join(folder, list[len(list)-1].File), nil
	case "pinned":
		p := pinnedName(folder)
		if p == "" {
			return "", fmt.Errorf("no pinned run for %q (use: lt history pin <file>)", test)
		}
		return filepath.Join(folder, p), nil
	}
	return which, nil
}
