package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Docker monitoring talks to the Engine API over HTTP (unix socket or TCP)
// without the SDK: the running containers are listed every few seconds and
// each one's stats are read once a second, one-shot, so the rates come from
// our own previous sample.

const (
	maxContainers   = 50
	dockerListEvery = 5 * time.Second
	dockerParallel  = 25 // stats calls mostly wait for the daemon, so many may be open
	// One-shot stats may still wait for the daemon's next collection
	// (once a second), so a call can take about 1s; rates use the
	// daemon's own read time, so the timing does not skew them.
	dockerStatTimeout = 2500 * time.Millisecond
)

type dockerSource struct {
	endpoint string // as resolved, for messages
	base     string // http://docker or http://host:port
	client   *http.Client
	globs    []string

	listedAt   time.Time
	containers []dockerContainer
	prev       map[string]dockerSample // by container id
	shown      map[string][]string     // series emitted per container id, to drop when it goes
}

type dockerContainer struct {
	id, name string
	restarts int
}

// dockerSample is what one stats call gives, reduced to the counters.
type dockerSample struct {
	read      time.Time
	cpu       uint64 // total usage, ns
	system    uint64 // host cpu, ns (0 when the daemon does not report it)
	rx, tx    uint64
	blkR, blk uint64 // read, write bytes
}

// newDocker resolves the endpoint: "local" (DOCKER_HOST, then the usual
// sockets), unix:///path, tcp://host:port or http(s)://host:port.
func newDocker(endpoint string, globs []string) (*dockerSource, error) {
	ep := strings.TrimSpace(endpoint)
	if ep == "local" || ep == "" {
		ep = localDockerEndpoint()
	}
	d := &dockerSource{endpoint: ep, globs: globs, prev: map[string]dockerSample{}, shown: map[string][]string{}}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("docker endpoint %q: %w", endpoint, err)
	}
	tr := &http.Transport{MaxIdleConnsPerHost: dockerParallel, IdleConnTimeout: 30 * time.Second}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		if sock == "" {
			sock = u.Host
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		}
		d.base = "http://docker"
	case "tcp", "http":
		d.base = "http://" + u.Host
	case "https":
		d.base = "https://" + u.Host
	default:
		return nil, fmt.Errorf("docker endpoint %q: use local, unix:///path or tcp://host:port", endpoint)
	}
	for _, g := range globs {
		if _, err := path.Match(g, ""); err != nil {
			return nil, fmt.Errorf("docker containers %q: %w", g, err)
		}
	}
	d.client = &http.Client{Transport: tr}
	return d, nil
}

func localDockerEndpoint() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	cands := []string{"/var/run/docker.sock"}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands,
			filepath.Join(home, ".docker", "run", "docker.sock"),     // Docker Desktop (macOS)
			filepath.Join(home, ".docker", "desktop", "docker.sock"), // Docker Desktop (Linux)
			filepath.Join(home, ".colima", "default", "docker.sock"), // colima
			filepath.Join(home, ".orbstack", "run", "docker.sock"),   // OrbStack
			filepath.Join(home, ".rd", "docker.sock"),                // Rancher Desktop
			filepath.Join(home, "Library", "Containers", "com.docker.docker", "Data", "docker.raw.sock"),
		)
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return "unix://" + c
		}
	}
	return "unix:///var/run/docker.sock"
}

func (d *dockerSource) get(ctx context.Context, p string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+p, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: status %d %s", p, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v)
}

func (d *dockerSource) close() { d.client.CloseIdleConnections() }

// matches reports whether a container name passes the globs.
func (d *dockerSource) matches(name string) bool {
	if len(d.globs) == 0 {
		return true
	}
	for _, g := range d.globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

type apiContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
}

// list refreshes the running containers that match, with restart counts.
// It returns how many matched before the cap.
func (d *dockerSource) list(ctx context.Context) (int, error) {
	var cs []apiContainer
	if err := d.get(ctx, "/containers/json", &cs); err != nil {
		return 0, err
	}
	var out []dockerContainer
	for _, c := range cs {
		name := c.ID
		if len(name) > 12 {
			name = name[:12]
		}
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if d.matches(name) {
			out = append(out, dockerContainer{id: c.ID, name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	matched := len(out)
	if len(out) > maxContainers {
		out = out[:maxContainers]
	}
	parallel(len(out), func(i int) {
		var ins struct {
			RestartCount int `json:"RestartCount"`
		}
		if err := d.get(ctx, "/containers/"+out[i].id+"/json", &ins); err == nil {
			out[i].restarts = ins.RestartCount
		}
	})
	d.containers = out
	return matched, nil
}

// parallel runs f(0..n-1), at most dockerParallel at a time.
func parallel(n int, f func(i int)) {
	sem := make(chan struct{}, dockerParallel)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			f(i)
		}()
	}
	wg.Wait()
}

// apiStats is the part of GET /containers/{id}/stats we use.
type apiStats struct {
	Read     time.Time `json:"read"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IOServiceBytes []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

func (st *apiStats) sample() dockerSample {
	s := dockerSample{read: st.Read, cpu: st.CPUStats.CPUUsage.TotalUsage, system: st.CPUStats.SystemUsage}
	for _, n := range st.Networks {
		s.rx += n.RxBytes
		s.tx += n.TxBytes
	}
	for _, e := range st.BlkioStats.IOServiceBytes {
		switch strings.ToLower(e.Op) {
		case "read":
			s.blkR += e.Value
		case "write":
			s.blk += e.Value
		}
	}
	return s
}

// memUsed is usage minus the reclaimable page cache, as `docker stats` shows.
func (st *apiStats) memUsed() uint64 {
	m := st.MemoryStats
	sub := func(k string) (uint64, bool) {
		v, ok := m.Stats[k]
		return v, ok && v < m.Usage
	}
	if v, ok := sub("total_inactive_file"); ok { // cgroup v1
		return m.Usage - v
	}
	if v, ok := sub("inactive_file"); ok { // cgroup v2
		return m.Usage - v
	}
	if v, ok := sub("cache"); ok { // old daemons
		return m.Usage - v
	}
	return m.Usage
}

// cpuPercent is the container's CPU between two samples, one core = 100.
// Docker's formula (container delta / host delta × cpus) when the host
// counter moved, else container time over wall time.
func cpuPercent(prev, cur dockerSample, cpus int, now, prevAt time.Time) (float64, bool) {
	if cur.cpu < prev.cpu {
		return 0, false
	}
	dc := float64(cur.cpu - prev.cpu)
	if cur.system > prev.system && cpus > 0 {
		return dc / float64(cur.system-prev.system) * float64(cpus) * 100, true
	}
	dt := cur.read.Sub(prev.read)
	if prev.read.IsZero() || cur.read.IsZero() || dt <= 0 {
		dt = now.Sub(prevAt)
	}
	if dt <= 0 {
		return 0, false
	}
	return dc / float64(dt.Nanoseconds()) * 100, true
}

// dockerEmit is how the source reports values (Monitor.set and drop).
type dockerEmit struct {
	set  func(name, unit string, v float64)
	drop func(names []string)
	name func(string) string // target prefix
	warn func(key, msg string)
}

// scrape reads every container's stats once.
func (d *dockerSource) scrape(ctx context.Context, now time.Time, e dockerEmit) error {
	if d.listedAt.IsZero() || now.Sub(d.listedAt) >= dockerListEvery {
		matched, err := d.list(ctx)
		if err != nil {
			return fmt.Errorf("docker %s: %w", d.endpoint, err)
		}
		d.listedAt = now
		if matched > maxContainers {
			e.warn("docker-cap", fmt.Sprintf("docker: %d containers match; watching the first %d by name (narrow it with containers:)", matched, maxContainers))
		}
	}
	type res struct {
		st  apiStats
		err error
	}
	cs := d.containers
	results := make([]res, len(cs))
	parallel(len(cs), func(i int) {
		sctx, cancel := context.WithTimeout(ctx, dockerStatTimeout)
		defer cancel()
		results[i].err = d.get(sctx, "/containers/"+cs[i].id+"/stats?stream=false&one-shot=true", &results[i].st)
	})
	live := map[string]bool{}
	for i, c := range cs {
		live[c.id] = true
		r := &results[i]
		if r.err != nil {
			continue
		}
		cur := r.st.sample()
		if cur.read.IsZero() {
			cur.read = now
		}
		n := func(s string) string { return e.name(c.name + " " + s) }
		emitted := []string{n("memory"), n("restarts")}
		e.set(n("memory"), "bytes", float64(r.st.memUsed()))
		if lim := r.st.MemoryStats.Limit; lim > 0 {
			e.set(n("memory %"), "%", float64(r.st.memUsed())/float64(lim)*100)
			emitted = append(emitted, n("memory %"))
		}
		e.set(n("restarts"), "", float64(c.restarts))
		if p, ok := d.prev[c.id]; ok {
			if v, ok := cpuPercent(p, cur, r.st.CPUStats.OnlineCPUs, now, p.read); ok {
				e.set(n("cpu"), "%", v)
				emitted = append(emitted, n("cpu"))
			}
			secs := cur.read.Sub(p.read).Seconds()
			if secs > 0 {
				rate := func(name string, a, b uint64) {
					if b >= a {
						e.set(n(name), "/s", float64(b-a)/secs)
						emitted = append(emitted, n(name))
					}
				}
				rate("net rx bytes", p.rx, cur.rx)
				rate("net tx bytes", p.tx, cur.tx)
				rate("disk read bytes", p.blkR, cur.blkR)
				rate("disk write bytes", p.blk, cur.blk)
			}
		}
		d.prev[c.id] = cur
		d.shown[c.id] = mergeNames(d.shown[c.id], emitted)
	}
	for id, names := range d.shown {
		if !live[id] {
			e.drop(names)
			delete(d.shown, id)
			delete(d.prev, id)
		}
	}
	if len(cs) > 0 {
		var failed []string
		for i := range results {
			if results[i].err != nil && ctx.Err() == nil {
				failed = append(failed, cs[i].name+": "+results[i].err.Error())
			}
		}
		if len(failed) == len(cs) {
			return fmt.Errorf("docker stats: %s", failed[0])
		}
	}
	return nil
}

func mergeNames(have, add []string) []string {
	for _, a := range add {
		found := false
		for _, h := range have {
			if h == a {
				found = true
				break
			}
		}
		if !found {
			have = append(have, a)
		}
	}
	return have
}
