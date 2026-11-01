package logs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ymg4798/load-testing/internal/run"
	"github.com/ymg4798/load-testing/internal/scenario"
	"github.com/ymg4798/load-testing/internal/tmpl"
)

// Tailing parameters.
var (
	pollEvery  = 200 * time.Millisecond
	globEvery  = time.Second
	flushAfter = 300 * time.Millisecond // a record waiting for stack lines is emitted after this much quiet
	// maxBacklog is how far a reader may fall behind a file before it skips
	// ahead to the newest quarter of that (counting the skipped lines as
	// dropped) rather than lag further.
	maxBacklog   int64 = 32 << 20
	readChunk          = 1 << 20
	maxReadPoll        = 8 << 20 // per file per poll, so one busy file does not starve the rest
	maxPartial         = 1 << 20 // a "line" longer than this is cut
	partialFlush       = time.Second
)

// Follower tails log files into a store until Close.
type Follower struct {
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// FollowFiles tails every file matching each spec, like tail -F: files
// that exist now are read from their end, files that appear later from
// the start; rotation (rename or re-create) and truncation are followed.
// File paths may use {{ var }} and globs; relative paths resolve against
// dir (the scenario file's folder; "" for the working directory).
func FollowFiles(st *Store, specs []scenario.LogSpec, dir string, vars map[string]any) (*Follower, error) {
	f := &Follower{stop: make(chan struct{})}
	var ws []*watcher
	for _, sp := range specs {
		p := sp.File
		t, err := tmpl.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("logs %s: %w", sp.File, err)
		}
		if p, err = t.RenderString(tmpl.MapScope(vars)); err != nil {
			return nil, fmt.Errorf("logs %s: %w", sp.File, err)
		}
		if dir != "" && !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := filepath.Match(p, ""); err != nil {
			return nil, fmt.Errorf("logs %s: %w", sp.File, err)
		}
		ws = append(ws, &watcher{st: st, pattern: p, name: sp.Name, format: ParseFormat(sp.Format), files: map[string]*tailed{}})
	}
	for _, w := range ws {
		w.glob(true)
		if len(w.files) == 0 {
			fmt.Fprintf(os.Stderr, "logs: no file matches %s yet; waiting for it\n", w.pattern)
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			w.loop(f.stop)
		}()
	}
	return f, nil
}

// Close stops tailing and emits records still waiting for stack lines.
func (f *Follower) Close() {
	f.once.Do(func() {
		close(f.stop)
		f.wg.Wait()
	})
}

type watcher struct {
	st      *Store
	pattern string
	name    string
	format  Format
	files   map[string]*tailed
	globAt  time.Time
}

type tailed struct {
	path    string
	f       *os.File
	info    os.FileInfo
	off     int64
	partial []byte
	skipNL  bool // after skipping a backlog: drop up to the next newline
	parser  *Parser
	dataAt  time.Time
}

func (w *watcher) loop(stop <-chan struct{}) {
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	defer w.closeAll()
	for {
		select {
		case <-stop:
			w.poll() // what was written up to now
			return
		case <-tick.C:
		}
		w.poll()
	}
}

func (w *watcher) closeAll() {
	for _, t := range w.files {
		w.finish(t)
	}
}

// finish emits what a file still holds and closes it.
func (w *watcher) finish(t *tailed) {
	var out []run.LogLine
	if len(t.partial) > 0 && !t.skipNL {
		out = append(out, t.parser.Feed(string(t.partial))...)
		t.partial = nil
	}
	out = append(out, t.parser.Flush()...)
	w.st.AddBatch(out)
	if t.f != nil {
		t.f.Close()
		t.f = nil
	}
}

func (w *watcher) glob(initial bool) {
	w.globAt = time.Now()
	matches, _ := filepath.Glob(w.pattern)
	sort.Strings(matches)
	for _, p := range matches {
		if _, ok := w.files[p]; ok {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		t := &tailed{path: p, parser: &Parser{Source: w.sourceName(p), Format: w.format}}
		if err := t.open(initial); err != nil {
			continue
		}
		w.files[p] = t
	}
}

func (w *watcher) sourceName(p string) string {
	if w.name != "" {
		return w.name
	}
	return filepath.Base(p)
}

// open opens the path, at its end when atEnd.
func (t *tailed) open(atEnd bool) error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	t.f, t.info, t.off, t.partial, t.skipNL = f, fi, 0, nil, false
	if atEnd {
		if t.off, err = f.Seek(0, io.SeekEnd); err != nil {
			f.Close()
			return err
		}
	}
	return nil
}

func (w *watcher) poll() {
	if time.Since(w.globAt) >= globEvery {
		w.glob(false)
	}
	for p, t := range w.files {
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			// Gone (deleted, or renamed by rotation): read what is left.
			w.read(t)
			w.finish(t)
			delete(w.files, p)
			continue
		case !os.SameFile(fi, t.info):
			// Replaced (rotation by rename + create): drain the old file, start the new one.
			w.read(t)
			w.finish(t)
			if err := t.open(false); err != nil {
				delete(w.files, p)
				continue
			}
		case fi.Size() < t.off:
			// Truncated (copytruncate, or "> file"): start over.
			if _, err := t.f.Seek(0, io.SeekStart); err == nil {
				t.off, t.partial, t.skipNL = 0, nil, false
			}
		}
		w.read(t)
	}
}

// read consumes what was appended since the last read.
func (w *watcher) read(t *tailed) {
	if t.f == nil {
		return
	}
	now := time.Now()
	if fi, err := t.f.Stat(); err == nil && fi.Size()-t.off > maxBacklog {
		// Too far behind: keep only the newest part, so lag stays bounded.
		to := fi.Size() - maxBacklog/4
		if _, err := t.f.Seek(to, io.SeekStart); err == nil {
			w.st.addDropped(countLines(t.f, t.off, to))
			// Mid-line unless the byte before is a newline: drop the rest of that line.
			last := make([]byte, 1)
			_, err := t.f.ReadAt(last, to-1)
			t.off, t.partial, t.skipNL = to, nil, err != nil || last[0] != '\n'
			if t.skipNL {
				w.st.addDropped(1) // the line cut in two
			}
		}
	}
	buf := make([]byte, readChunk)
	var out []run.LogLine
	got := 0
	for got < maxReadPoll {
		n, err := t.f.Read(buf)
		if n > 0 {
			got += n
			t.off += int64(n)
			out = w.consume(t, buf[:n], out)
		}
		if err != nil || n == 0 { // io.EOF: caught up
			break
		}
	}
	if got > 0 {
		t.dataAt = now
	} else {
		quiet := now.Sub(t.dataAt)
		if len(t.partial) > 0 && quiet >= partialFlush && !t.skipNL {
			out = append(out, t.parser.Feed(string(t.partial))...)
			t.partial = nil
		}
		if t.parser.Pending() && quiet >= flushAfter {
			out = append(out, t.parser.Flush()...)
		}
	}
	w.st.AddBatch(out)
}

// countLines counts the newlines in [from, to): much cheaper than parsing.
func countLines(f *os.File, from, to int64) int64 {
	buf := make([]byte, readChunk)
	var n int64
	for from < to {
		k, err := f.ReadAt(buf[:min(int64(len(buf)), to-from)], from)
		n += int64(bytes.Count(buf[:k], []byte{'\n'}))
		from += int64(k)
		if err != nil || k == 0 {
			break
		}
	}
	return n
}

func (w *watcher) consume(t *tailed, b []byte, out []run.LogLine) []run.LogLine {
	if len(t.partial) > 0 {
		b = append(t.partial, b...)
		t.partial = nil
	}
	lines, rest := splitLines(b)
	for _, ln := range lines {
		if t.skipNL {
			t.skipNL = false
			continue
		}
		out = append(out, t.parser.Feed(string(ln))...)
	}
	if len(rest) > maxPartial {
		if !t.skipNL {
			out = append(out, t.parser.Feed(string(rest[:maxPartial]))...)
		}
		rest = nil
		t.skipNL = true
	}
	if len(rest) > 0 {
		t.partial = append([]byte(nil), rest...)
	}
	return out
}
