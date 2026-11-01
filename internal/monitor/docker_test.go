package monitor

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/scenario"
)

// fakeDocker serves the Engine API endpoints the monitor uses. Each stats
// call advances the counters by one second's worth: web uses 1.5 cores,
// db 0.25.
type fakeDocker struct {
	mu    sync.Mutex
	calls map[string]int
	gone  atomic.Bool // db stopped
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/containers/json":
		list := `[{"Id":"aaa111","Names":["/web-1"]},{"Id":"ccc333","Names":["/other"]}`
		if !f.gone.Load() {
			list += `,{"Id":"bbb222","Names":["/db"]}`
		}
		fmt.Fprint(w, list+"]")
	case strings.HasSuffix(p, "/json"):
		n := 0
		if strings.Contains(p, "bbb222") {
			n = 3
		}
		fmt.Fprintf(w, `{"RestartCount":%d}`, n)
	case strings.HasSuffix(p, "/stats"):
		if r.URL.Query().Get("one-shot") != "true" || r.URL.Query().Get("stream") != "false" {
			http.Error(w, "want one-shot", 400)
			return
		}
		id := strings.Split(p, "/")[2]
		f.calls[id]++
		k := uint64(f.calls[id])
		cores := 1.5
		if id == "bbb222" {
			cores = 0.25
		}
		read := time.Date(2026, 1, 1, 0, 0, int(k), 0, time.UTC).Format(time.RFC3339Nano)
		fmt.Fprintf(w, `{"read":%q,
			"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":4},
			"memory_stats":{"usage":%d,"limit":%d,"stats":{"inactive_file":%d}},
			"networks":{"eth0":{"rx_bytes":%d,"tx_bytes":%d},"eth1":{"rx_bytes":%d,"tx_bytes":0}},
			"blkio_stats":{"io_service_bytes_recursive":[{"major":8,"op":"read","value":%d},{"major":8,"op":"write","value":%d}]}}`,
			read, uint64(cores*1e9)*k, 4*uint64(1e9)*k,
			300<<20, 1<<30, 100<<20,
			1000*k, 500*k, 24*k,
			4096*k, 8192*k)
	default:
		http.NotFound(w, r)
	}
}

func TestDockerStats(t *testing.T) {
	fake := &fakeDocker{calls: map[string]int{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	m, err := New([]scenario.MonitorTarget{{
		Docker:     strings.Replace(srv.URL, "http://", "tcp://", 1),
		Containers: []string{"web-*", "db"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	tg := m.targets[0]
	now := time.Now()
	ctx := t.Context()
	m.scrape(ctx, tg)
	m.scrape(ctx, tg)
	s := &metrics.Snapshot{Server: map[string]float64{"keep": 1}}
	m.Decorate(s)
	near := func(name string, want float64) {
		t.Helper()
		got, ok := s.Server[name]
		if !ok || math.Abs(got-want) > want*1e-6+1e-9 {
			t.Errorf("%s = %v (present %v), want %v", name, got, ok, want)
		}
	}
	near("keep", 1) // Decorate merges, it does not replace
	near("web-1 cpu", 150)
	near("db cpu", 25)
	near("web-1 memory", 200<<20)
	near("web-1 memory %", 200.0/1024*100)
	near("web-1 net rx bytes", 1024)
	near("web-1 net tx bytes", 500)
	near("web-1 disk read bytes", 4096)
	near("web-1 disk write bytes", 8192)
	near("db restarts", 3)
	near("web-1 restarts", 0)
	if _, ok := s.Server["other cpu"]; ok {
		t.Error("container outside the globs was watched")
	}
	units := map[string]string{}
	for _, se := range m.Series() {
		units[se.Name] = se.Unit
	}
	for n, u := range map[string]string{"web-1 cpu": "%", "web-1 memory": "bytes", "web-1 memory %": "%", "web-1 net rx bytes": "/s", "db restarts": ""} {
		if units[n] != u {
			t.Errorf("unit of %s = %q, want %q", n, units[n], u)
		}
	}

	// db goes away: its series stop at the next list refresh.
	fake.gone.Store(true)
	tg.docker.listedAt = now.Add(-time.Minute)
	m.scrape(ctx, tg)
	s = &metrics.Snapshot{}
	m.Decorate(s)
	if _, ok := s.Server["db cpu"]; ok {
		t.Error("stopped container still reported")
	}
	if _, ok := s.Server["web-1 cpu"]; !ok {
		t.Error("web-1 lost")
	}
}

func TestDockerMemAndCPUMath(t *testing.T) {
	st := &apiStats{}
	st.MemoryStats.Usage = 1000
	st.MemoryStats.Stats = map[string]uint64{"total_inactive_file": 300, "inactive_file": 100, "cache": 50}
	if st.memUsed() != 700 {
		t.Errorf("v1 mem %d", st.memUsed())
	}
	delete(st.MemoryStats.Stats, "total_inactive_file")
	if st.memUsed() != 900 {
		t.Errorf("v2 mem %d", st.memUsed())
	}
	st.MemoryStats.Stats = map[string]uint64{"inactive_file": 5000}
	if st.memUsed() != 1000 {
		t.Errorf("inactive > usage should not subtract: %d", st.memUsed())
	}
	t0 := time.Unix(100, 0)
	a := dockerSample{read: t0, cpu: 0}
	b := dockerSample{read: t0.Add(2 * time.Second), cpu: 1e9} // no host counter (Windows): wall time
	if v, ok := cpuPercent(a, b, 0, time.Time{}, time.Time{}); !ok || math.Abs(v-50) > 1e-9 {
		t.Errorf("wall cpu %v %v", v, ok)
	}
	if _, ok := cpuPercent(b, a, 2, time.Time{}, time.Time{}); ok {
		t.Error("counter reset should give no value")
	}
}

func TestDockerEndpoint(t *testing.T) {
	for _, ep := range []string{"unix:///var/run/docker.sock", "tcp://h:2375", "local"} {
		if _, err := newDocker(ep, nil); err != nil {
			t.Errorf("%s: %v", ep, err)
		}
	}
	if _, err := newDocker("ftp://x", nil); err == nil {
		t.Error("ftp accepted")
	}
	if _, err := newDocker("local", []string{"["}); err == nil {
		t.Error("bad glob accepted")
	}
}

// TestDockerReal reads a real daemon: LT_DOCKER_TEST=<container name glob>
// (start e.g. `docker run --rm -d --name lt-burn alpine sh -c 'while :; do :; done'`).
func TestDockerReal(t *testing.T) {
	glob := os.Getenv("LT_DOCKER_TEST")
	if glob == "" {
		t.Skip("LT_DOCKER_TEST not set")
	}
	m, err := New([]scenario.MonitorTarget{{Docker: "local", Containers: []string{glob}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Start()
	time.Sleep(3500 * time.Millisecond)
	s := &metrics.Snapshot{}
	m.Decorate(s)
	if len(s.Server) == 0 {
		t.Fatal("no docker series")
	}
	for k, v := range s.Server {
		t.Logf("%s = %.1f", k, v)
	}
}
