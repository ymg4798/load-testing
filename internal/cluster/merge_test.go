package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/scenario"
)

// fakeAgent streams the given seconds in order, each with 10 requests.
func fakeAgent(t *testing.T, seconds []int, pause time.Duration) string {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/run", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) })
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		for _, sec := range seconds {
			h := metrics.NewHist()
			for i := 0; i < 10; i++ {
				_ = h.RecordValue(1000)
			}
			hist, _ := metrics.EncodeHist(h)
			_ = enc.Encode(streamMsg{Snap: &metrics.Snapshot{T: sec, Keys: map[string]*metrics.KeySnap{
				"s|hit": {Count: 10, Stats: metrics.StatsOf(h), Hist: hist},
			}}})
			w.(http.Flusher).Flush()
			time.Sleep(pause)
		}
		_ = enc.Encode(streamMsg{Done: true, Elapsed: float64(len(seconds))})
	})
	mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// A lagging agent's second that arrives after the controller moved on must
// still be counted, not dropped.
func TestLateSnapshotIsCounted(t *testing.T) {
	fast := fakeAgent(t, []int{1, 2, 3, 4, 5, 6, 7, 8}, 0)
	// Second 1 arrives last: after the others are 3+ seconds ahead.
	slow := fakeAgent(t, []int{2, 3, 4, 5, 6, 7, 8, 1}, 20*time.Millisecond)
	dir := t.TempDir()
	p := filepath.Join(dir, "s.yaml")
	_ = os.WriteFile(p, []byte("scenarios:\n  s:\n    rate: 1\n    duration: 8s\n    flow:\n      - http: http://localhost/\n"), 0o644)
	f, err := scenario.Load(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Agents: []string{fast, slow}, Token: "t", StartDelay: 10 * time.Millisecond}
	res, err := c.Run(context.Background(), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Requests != 160 {
		t.Fatalf("requests %d, want 160 (2 agents × 8 s × 10)", res.Summary.Requests)
	}
	for i, s := range res.Snapshots {
		if i > 0 && s.T <= res.Snapshots[i-1].T {
			t.Fatalf("snapshots out of order: %d after %d", s.T, res.Snapshots[i-1].T)
		}
	}
}
