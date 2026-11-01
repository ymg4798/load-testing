package cluster

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ymg4798/load-testing/internal/metrics"
	"github.com/ymg4798/load-testing/internal/run"
	"github.com/ymg4798/load-testing/internal/scenario"
)

// Bundle limits.
const (
	maxFile   = 20 << 20
	maxBundle = 100 << 20
)

// Controller drives a distributed run.
type Controller struct {
	Agents []string // host:port or http(s)://host:port
	Token  string
	// StartDelay gives every agent time to receive the start signal
	// before the agreed moment. Default 3s.
	StartDelay time.Duration
	Client     *http.Client
}

func (c *Controller) base(agent string) string {
	if strings.HasPrefix(agent, "http://") || strings.HasPrefix(agent, "https://") {
		return strings.TrimSuffix(agent, "/")
	}
	return "http://" + agent
}

func (c *Controller) do(ctx context.Context, method, agent, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base(agent)+path, rd)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.Client.Do(req)
}

// Bundle collects the scenario's folder: the file itself plus data,
// scripts and proto files it may reference. Hidden folders and
// node_modules are skipped.
func Bundle(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFile {
			return nil // large files (results, binaries) are not scenario inputs
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext == ".html" || (ext == ".json" && looksLikeResult(p)) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		total += len(b)
		if total > maxBundle {
			return fmt.Errorf("scenario folder is larger than %d MB; move unrelated files out", maxBundle>>20)
		}
		rel, _ := filepath.Rel(dir, p)
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	return files, err
}

func looksLikeResult(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 32)
	n, _ := f.Read(head)
	return bytes.HasPrefix(bytes.TrimSpace(head[:n]), []byte(`{"tool":"lt"`))
}

type agentMsg struct {
	idx int
	msg streamMsg
	err error
}

// Run executes f on every agent and returns the merged result.
func (c *Controller) Run(ctx context.Context, f *scenario.File, vars map[string]string, sinks ...run.Sink) (*run.Result, error) {
	if len(c.Agents) == 0 {
		return nil, fmt.Errorf("no agents")
	}
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	if c.StartDelay == 0 {
		c.StartDelay = 3 * time.Second
	}
	ths, err := metrics.ParseThresholds(f.Thresholds)
	if err != nil {
		return nil, err
	}
	files, err := Bundle(f.Dir)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(f.Dir, f.Path)
	if err != nil {
		return nil, err
	}
	idb := make([]byte, 6)
	_, _ = rand.Read(idb)
	id := hex.EncodeToString(idb)
	n := len(c.Agents)

	// Arm every agent. Each compiles the scenario and reports errors now.
	armCtx, cancelArm := context.WithTimeout(ctx, 60*time.Second)
	defer cancelArm()
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, a := range c.Agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := RunRequest{ID: id, Files: files, Scenario: filepath.ToSlash(rel), Vars: vars, Index: i, Count: n, StartDelay: c.StartDelay}
			resp, err := c.do(armCtx, http.MethodPost, a, "/v1/run", req)
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				var e struct{ Error string }
				_ = json.NewDecoder(resp.Body).Decode(&e)
				if e.Error == "" {
					e.Error = resp.Status
				}
				errs[i] = fmt.Errorf("%s", e.Error)
			}
		}()
	}
	wg.Wait()
	var bad []string
	for i, err := range errs {
		if err != nil {
			bad = append(bad, fmt.Sprintf("agent %s: %v", c.Agents[i], err))
		}
	}
	if len(bad) > 0 {
		c.stopAll()
		return nil, fmt.Errorf("could not start:\n  %s", strings.Join(bad, "\n  "))
	}

	// Stream from every agent.
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	msgs := make(chan agentMsg, 64)
	var swg sync.WaitGroup
	for i, a := range c.Agents {
		swg.Add(1)
		go func() {
			defer swg.Done()
			c.stream(streamCtx, i, a, id, msgs)
		}()
	}
	go func() { swg.Wait(); close(msgs) }()
	stopOnce := sync.Once{}
	go func() {
		select {
		case <-ctx.Done():
			stopOnce.Do(c.stopAll)
		case <-streamCtx.Done():
		}
	}()

	merged := make(chan *metrics.Snapshot, 16)
	var elapsed time.Duration
	var agentErrs []string
	go func() {
		defer close(merged)
		pending := map[int][]*metrics.Snapshot{}
		reported := map[int]int{} // agents that reported each second (late folds not counted)
		active := n
		next, maxT := 1, 0
		flush := func(all bool) {
			for {
				group, ok := pending[next]
				if !ok {
					if !all || next > maxT {
						return
					}
					next++
					continue
				}
				// Emit when every live agent reported this second, or when
				// one is lagging by more than 3 seconds.
				if !all && reported[next] < active && maxT < next+3 {
					return
				}
				m, err := metrics.Merge(group)
				if err == nil {
					m.T = next
					merged <- m
				}
				delete(pending, next)
				delete(reported, next)
				next++
			}
		}
		for m := range msgs {
			switch {
			case m.err != nil:
				agentErrs = append(agentErrs, fmt.Sprintf("agent %s: %v", c.Agents[m.idx], m.err))
				active--
			case m.msg.Done:
				if m.msg.Error != "" {
					agentErrs = append(agentErrs, fmt.Sprintf("agent %s: %s", c.Agents[m.idx], m.msg.Error))
				}
				if d := time.Duration(m.msg.Elapsed * float64(time.Second)); d > elapsed {
					elapsed = d
				}
				active--
			case m.msg.Snap != nil:
				s := m.msg.Snap
				if s.T < next {
					// Its second already went out without this agent (it lagged
					// more than 3s). Count it with the next second instead of
					// losing it: totals stay exact, timing shifts a little.
					pending[next] = append(pending[next], s)
					if next > maxT {
						maxT = next
					}
					break
				}
				pending[s.T] = append(pending[s.T], s)
				reported[s.T]++
				if s.T > maxT {
					maxT = s.T
				}
			}
			flush(false)
		}
		flush(true)
	}()

	meta := run.MetaFor(f, n)
	meta.Started = time.Now().Add(c.StartDelay)
	res, err := run.Collect(meta, merged, ths, func() time.Duration { return elapsed }, sinks...)
	if len(agentErrs) > 0 {
		sort.Strings(agentErrs)
		res.Warnings = append(res.Warnings, agentErrs...)
	}
	return res, err
}

func (c *Controller) stream(ctx context.Context, idx int, agent, id string, out chan<- agentMsg) {
	resp, err := c.do(ctx, http.MethodGet, agent, "/v1/stream?id="+id, nil)
	if err != nil {
		out <- agentMsg{idx: idx, err: err}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out <- agentMsg{idx: idx, err: fmt.Errorf("stream: %s", resp.Status)}
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var m streamMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			out <- agentMsg{idx: idx, err: err}
			return
		}
		out <- agentMsg{idx: idx, msg: m}
		if m.Done {
			return
		}
	}
	err = sc.Err()
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	out <- agentMsg{idx: idx, err: fmt.Errorf("stream ended: %w", err)}
}

func (c *Controller) stopAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, a := range c.Agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := c.do(ctx, http.MethodPost, a, "/v1/stop", nil); err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}
