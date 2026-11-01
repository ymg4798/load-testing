// Package cluster runs one test on several machines. Each machine runs
// `lt agent`; the machine that runs `lt run --agents ...` is the
// controller. The controller ships the scenario folder to every agent,
// gives each a share of the load, starts them together and merges their
// per-second snapshots, so the result looks like one big generator.
//
// Protocol (HTTP + JSON, optional bearer token):
//
//	POST /v1/run     RunRequest → {"planned": seconds}   compile and arm a run
//	GET  /v1/stream  NDJSON: {"snap": Snapshot} per second, then {"done": true, ...}
//	POST /v1/stop    stop the current run
//	GET  /v1/status  {"busy": bool, "version": "..."}
package cluster

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ymg4798/lt/internal/engine"
	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/scenario"
)

// RunRequest is what the controller sends to arm an agent.
type RunRequest struct {
	ID         string            `json:"id"`
	Files      map[string][]byte `json:"files"`    // path relative to the scenario folder → contents
	Scenario   string            `json:"scenario"` // the scenario file's path within Files
	Vars       map[string]string `json:"vars"`
	Index      int               `json:"index"`
	Count      int               `json:"count"`
	StartDelay time.Duration     `json:"startDelay"` // from receipt of the request
}

// streamMsg is one NDJSON line of /v1/stream.
type streamMsg struct {
	Snap    *metrics.Snapshot `json:"snap,omitempty"`
	Done    bool              `json:"done,omitempty"`
	Error   string            `json:"error,omitempty"`
	Elapsed float64           `json:"elapsed,omitempty"`
}

// Agent serves runs for a controller.
type Agent struct {
	Token   string
	Version string
	Log     *log.Logger

	mu  sync.Mutex
	cur *agentRun
}

type agentRun struct {
	id     string
	dir    string
	cancel context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	history []*metrics.Snapshot
	done    bool
	err     string
	elapsed time.Duration
}

func (a *Agent) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log.Printf(format, args...)
	}
}

// Serve listens on addr until ctx ends.
func (a *Agent) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/run", a.auth(a.handleRun))
	mux.HandleFunc("GET /v1/stream", a.auth(a.handleStream))
	mux.HandleFunc("POST /v1/stop", a.auth(a.handleStop))
	mux.HandleFunc("GET /v1/status", a.auth(a.handleStatus))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		a.stopCurrent()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	a.logf("agent listening on %s", ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a *Agent) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) != 1 {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (a *Agent) handleStatus(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	busy := a.cur != nil && !a.cur.isDone()
	a.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"busy": busy, "version": a.Version})
}

func (r *agentRun) isDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

// safeJoin keeps bundle paths inside dir.
func safeJoin(dir, rel string) (string, error) {
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if p != dir && !strings.HasPrefix(p, dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("bad path %q", rel)
	}
	return p, nil
}

func (a *Agent) handleRun(w http.ResponseWriter, r *http.Request) {
	received := time.Now()
	var req RunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 200<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	a.mu.Lock()
	if a.cur != nil && !a.cur.isDone() {
		a.mu.Unlock()
		writeErr(w, http.StatusConflict, fmt.Errorf("agent is busy with run %s", a.cur.id))
		return
	}
	a.mu.Unlock()

	dir, err := os.MkdirTemp("", "lt-agent-")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for rel, data := range req.Files {
		p, err := safeJoin(dir, rel)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(p), 0o755)
		}
		if err == nil {
			err = os.WriteFile(p, data, 0o644)
		}
		if err != nil {
			os.RemoveAll(dir)
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	path, err := safeJoin(dir, req.Scenario)
	if err != nil {
		os.RemoveAll(dir)
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	f, err := scenario.Load(path, req.Vars)
	if err != nil {
		os.RemoveAll(dir)
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	f.Partition(req.Index, req.Count)
	snaps := make(chan *metrics.Snapshot, 16)
	eng, err := engine.New(f, snaps)
	if err != nil {
		os.RemoveAll(dir)
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	run := &agentRun{id: req.ID, dir: dir, cancel: cancel}
	run.cond = sync.NewCond(&run.mu)
	a.mu.Lock()
	a.cur = run
	a.mu.Unlock()
	a.logf("run %s armed: agent %d of %d, %s, starts in %s", req.ID, req.Index+1, req.Count, filepath.Base(req.Scenario), req.StartDelay)

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for s := range snaps {
			run.mu.Lock()
			run.history = append(run.history, s)
			run.cond.Broadcast()
			run.mu.Unlock()
		}
	}()
	go func() {
		defer os.RemoveAll(dir)
		var runErr error
		select {
		case <-time.After(time.Until(received.Add(req.StartDelay))):
			runErr = eng.Run(ctx)
		case <-ctx.Done():
			close(snaps)
		}
		<-drained
		run.mu.Lock()
		run.done = true
		run.elapsed = eng.Elapsed()
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			run.err = runErr.Error()
		}
		run.cond.Broadcast()
		run.mu.Unlock()
		a.logf("run %s finished after %s", req.ID, eng.Elapsed().Round(time.Millisecond))
	}()
	_ = json.NewEncoder(w).Encode(map[string]any{"planned": eng.Duration().Seconds()})
}

func (a *Agent) handleStream(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	run := a.cur
	a.mu.Unlock()
	if run == nil || run.id != r.URL.Query().Get("id") {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such run"))
		return
	}
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	// Wake the waiting loop below if the controller goes away.
	stop := context.AfterFunc(r.Context(), func() {
		run.mu.Lock()
		run.cond.Broadcast()
		run.mu.Unlock()
	})
	defer stop()
	sent := 0
	for {
		run.mu.Lock()
		for sent >= len(run.history) && !run.done && r.Context().Err() == nil {
			run.cond.Wait()
		}
		batch := run.history[sent:]
		done, errMsg, elapsed := run.done, run.err, run.elapsed
		run.mu.Unlock()
		if r.Context().Err() != nil {
			return
		}
		for _, s := range batch {
			if err := enc.Encode(streamMsg{Snap: s}); err != nil {
				return
			}
		}
		sent += len(batch)
		if done && sent >= len(run.history) {
			_ = enc.Encode(streamMsg{Done: true, Error: errMsg, Elapsed: elapsed.Seconds()})
			if fl != nil {
				fl.Flush()
			}
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

func (a *Agent) stopCurrent() {
	a.mu.Lock()
	run := a.cur
	a.mu.Unlock()
	if run != nil {
		run.cancel()
	}
}

func (a *Agent) handleStop(w http.ResponseWriter, _ *http.Request) {
	a.stopCurrent()
	w.WriteHeader(http.StatusNoContent)
}
