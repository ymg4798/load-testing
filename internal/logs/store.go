// Package logs keeps the target's recent log lines during a run (and in
// watch mode): lines tailed from files and OTLP logs, parsed into records,
// searchable, counted per second, grouped into recurring errors and linked
// to requests by trace ID.
package logs

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
	"github.com/ymg4798/lt/internal/run"
)

// Default limits of a Store.
const (
	DefaultMaxLines = 200_000
	DefaultMaxBytes = 64 << 20
	// MaxReportLines caps the lines a result keeps.
	MaxReportLines = 2000

	keepFirstErrors = 500  // errors kept from the start of the run
	keepLastErrors  = 2000 // and the most recent ones
	bucketSeconds   = 3600 // per-second counters kept for Count
	recOverhead     = 160  // rough bytes per record besides its strings
)

// Level indexes for the per-second counters.
var levelIndex = map[string]int{"": 0, "debug": 1, "info": 2, "warn": 3, "error": 4, "fatal": 5}
var levelNames = [6]string{"", "debug", "info", "warn", "error", "fatal"}

// Options set a Store's limits; zero values take the defaults.
type Options struct {
	MaxLines int
	MaxBytes int64
}

type rec struct {
	line run.LogLine
	at   time.Time // arrival
	seq  uint64
	size int
}

type kept struct {
	seq  uint64
	line run.LogLine
}

type bucket struct {
	sec int64
	n   [6]int64
}

// Store holds recent log lines in a bounded ring, plus counters for the
// whole run. It is safe for concurrent use. It implements run.LogSource,
// run.Sink, run.Decorator, run.SeriesLister and run.Finisher.
type Store struct {
	now      func() time.Time
	maxLines int
	maxBytes int64

	mu    sync.RWMutex
	ring  []rec
	head  int // oldest
	n     int
	bytes int64
	seq   uint64

	total   int64
	byLevel [6]int64
	buckets [bucketSeconds]bucket

	firstErrs []kept
	lastErrs  []kept // ring
	lastHead  int

	groups        map[groupKey]*group
	groupsDropped int64

	// Decorate state.
	decTotal, decErrs int64
	decAt             time.Time
	emitted           bool

	dropped atomic.Int64 // lines lost before parsing (backlog skipped)
}

// NewStore makes an empty store.
func NewStore(o Options) *Store {
	if o.MaxLines <= 0 {
		o.MaxLines = DefaultMaxLines
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	return &Store{
		now:      time.Now,
		maxLines: o.MaxLines,
		maxBytes: o.MaxBytes,
		groups:   map[groupKey]*group{},
	}
}

func lineSize(l *run.LogLine) int {
	n := recOverhead + len(l.Message) + len(l.TraceID)
	for k, v := range l.Attrs {
		n += len(k) + len(v) + 32
	}
	return n
}

// Add stores one line. Missing times are set to now and levels and trace
// IDs are normalized.
func (s *Store) Add(l run.LogLine) {
	s.AddBatch([]run.LogLine{l})
}

// AddBatch stores several lines under one lock.
func (s *Store) AddBatch(ls []run.LogLine) {
	if len(ls) == 0 {
		return
	}
	now := s.now()
	norms := make([]string, len(ls))
	for i := range ls {
		l := &ls[i]
		if l.Time.IsZero() {
			l.Time = now
		}
		if _, ok := levelIndex[l.Level]; !ok {
			l.Level = normLevel(l.Level)
		}
		l.TraceID = strings.ToLower(l.TraceID)
		if isError(l.Level) {
			norms[i] = groupMessage(l)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sec := now.Unix()
	b := &s.buckets[sec%bucketSeconds]
	if b.sec != sec {
		*b = bucket{sec: sec}
	}
	for i := range ls {
		l := ls[i]
		s.seq++
		r := rec{line: l, at: now, seq: s.seq, size: lineSize(&l)}
		s.push(r)
		li := levelIndex[l.Level]
		s.total++
		s.byLevel[li]++
		b.n[li]++
		if isError(l.Level) {
			k := kept{seq: r.seq, line: l}
			if len(s.firstErrs) < keepFirstErrors {
				s.firstErrs = append(s.firstErrs, k)
			} else if len(s.lastErrs) < keepLastErrors {
				s.lastErrs = append(s.lastErrs, k)
			} else {
				s.lastErrs[s.lastHead] = k
				s.lastHead = (s.lastHead + 1) % keepLastErrors
			}
			s.addGroup(&l, norms[i])
		}
	}
}

// push appends to the ring, evicting the oldest past the limits.
func (s *Store) push(r rec) {
	for s.n > 0 && (s.n >= s.maxLines || s.bytes+int64(r.size) > s.maxBytes) {
		old := &s.ring[s.head]
		s.bytes -= int64(old.size)
		*old = rec{}
		s.head = (s.head + 1) % len(s.ring)
		s.n--
	}
	if s.n == len(s.ring) { // full but under maxLines: grow
		out := make([]rec, min(s.maxLines, max(2*len(s.ring), 1024)))
		for i := range s.n {
			out[i] = s.ring[(s.head+i)%len(s.ring)]
		}
		s.ring, s.head = out, 0
	}
	s.ring[(s.head+s.n)%len(s.ring)] = r
	s.n++
	s.bytes += int64(r.size)
}

// at returns the i-th oldest record.
func (s *Store) at(i int) *rec { return &s.ring[(s.head+i)%len(s.ring)] }

// addDropped counts lines lost because a reader fell too far behind.
func (s *Store) addDropped(n int64) { s.dropped.Add(n) }

// Stats are the store's own counters.
type Stats struct {
	Total   int64 // lines added since start
	Held    int   // lines in memory now
	Bytes   int64 // their approximate size
	Dropped int64 // lines (or, for skipped file backlogs, an estimate) never parsed
	Groups  int   // distinct error groups
}

// Stats reports what the store holds.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Stats{Total: s.total, Held: s.n, Bytes: s.bytes, Dropped: s.dropped.Load(), Groups: len(s.groups)}
}

// Search implements run.LogSource: the newest lines matching query, newest
// first. A query that does not parse (a bad /regex/) is searched as text.
func (s *Store) Search(query string, limit int) []run.LogLine {
	q, err := ParseQuery(query)
	if err != nil {
		q = &Query{parts: []part{{text: strings.ToLower(query)}}}
	}
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []run.LogLine
	for i := s.n - 1; i >= 0 && len(out) < limit; i-- {
		r := s.at(i)
		if q.Match(&r.line) {
			out = append(out, r.line)
		}
	}
	return out
}

// Count implements run.LogSource: lines matching query that arrived since
// t. Level-only queries count from per-second counters (exact for the last
// hour, even for lines no longer held); others scan the held lines.
func (s *Store) Count(query string, since time.Time) int {
	q, err := ParseQuery(query)
	if err != nil {
		q = &Query{parts: []part{{text: strings.ToLower(query)}}}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if q.levelOnly() && s.now().Sub(since) < (bucketSeconds-1)*time.Second {
		from := since.Unix()
		if since.Nanosecond() > 0 {
			from++ // whole seconds after since; the counters have no finer grain
		}
		var n int64
		for i := range s.buckets {
			b := &s.buckets[i]
			if b.sec < from || b.sec == 0 {
				continue
			}
			for li, c := range b.n {
				if c > 0 && q.levelOK(levelNames[li]) {
					n += c
				}
			}
		}
		return int(n)
	}
	n := 0
	for i := s.n - 1; i >= 0; i-- {
		r := s.at(i)
		if r.at.Before(since) {
			break
		}
		if q.Match(&r.line) {
			n++
		}
	}
	return n
}

// Snapshot implements run.Sink.
func (s *Store) Snapshot(*metrics.Snapshot, *metrics.Accumulator) {}

// Done implements run.Sink.
func (s *Store) Done(*run.Result) {}

// Series names.
const (
	SeriesLines  = "logs"
	SeriesErrors = "log errors"
)

// Decorate implements run.Decorator: "logs" (lines/s) and "log errors"
// (error and fatal lines/s) since the previous call, once any line came in.
func (s *Store) Decorate(snap *metrics.Snapshot) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	errs := s.byLevel[4] + s.byLevel[5]
	dl, de := s.total-s.decTotal, errs-s.decErrs
	secs := 1.0
	if !s.decAt.IsZero() {
		secs = now.Sub(s.decAt).Seconds()
	}
	s.decTotal, s.decErrs, s.decAt = s.total, errs, now
	if s.total == 0 {
		return
	}
	if secs < 0.5 {
		secs = 1 // calls close together (catching up): count, not rate
	}
	s.emitted = true
	if snap.Server == nil {
		snap.Server = map[string]float64{}
	}
	snap.Server[SeriesLines] = float64(dl) / secs
	snap.Server[SeriesErrors] = float64(de) / secs
}

// Series implements run.SeriesLister.
func (s *Store) Series() []run.ServerSeries {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.emitted {
		return nil
	}
	return []run.ServerSeries{{Name: SeriesErrors, Unit: "/s"}, {Name: SeriesLines, Unit: "/s"}}
}

// Finish implements run.Finisher: it adds the log report (totals and the
// lines that matter) and appends the log error groups to the APM report's
// errors. Run it after the APM collector's Finish, which sets r.APM.
func (s *Store) Finish(r *run.Result) {
	traces := reportTraces(r)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.total == 0 {
		return
	}
	rep := &run.LogReport{Total: s.total, ByLevel: map[string]int{}}
	for i, c := range s.byLevel {
		if c > 0 {
			name := levelNames[i]
			if name == "" {
				name = "unknown"
			}
			rep.ByLevel[name] = int(c)
		}
	}
	// Lines of the traces the report points at.
	var traced []kept
	if len(traces) > 0 {
		for i := 0; i < s.n; i++ {
			rc := s.at(i)
			if rc.line.TraceID != "" && traces[rc.line.TraceID] {
				traced = append(traced, kept{seq: rc.seq, line: rc.line})
			}
		}
	}
	errs := make([]kept, 0, len(s.firstErrs)+len(s.lastErrs))
	errs = append(errs, s.firstErrs...)
	errs = append(errs, s.lastErrs[s.lastHead:]...)
	errs = append(errs, s.lastErrs[:s.lastHead]...)
	// Share the cap: traced lines get at least half, errors the rest.
	tracedCap := min(len(traced), MaxReportLines/2)
	errs = spread(errs, MaxReportLines-tracedCap)
	traced = spread(traced, MaxReportLines-len(errs))
	seen := make(map[uint64]bool, len(errs)+len(traced))
	all := make([]kept, 0, len(errs)+len(traced))
	for _, list := range [][]kept{errs, traced} {
		for _, k := range list {
			if !seen[k.seq] {
				seen[k.seq] = true
				all = append(all, k)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seq < all[j].seq })
	rep.Lines = make([]run.LogLine, len(all))
	for i, k := range all {
		rep.Lines[i] = k.line
	}
	r.Logs = rep

	if len(s.groups) == 0 {
		return
	}
	gs := make([]run.ErrorGroup, 0, len(s.groups))
	for _, g := range s.groups {
		gs = append(gs, g.ErrorGroup)
	}
	sortGroups(gs)
	if len(gs) > maxReportGroups {
		gs = gs[:maxReportGroups]
	}
	if r.APM == nil {
		r.APM = &run.APMReport{}
	}
	r.APM.Errors = append(r.APM.Errors, gs...)
}

// spread keeps at most n items, evenly spaced (the first and the last
// among them), when there are more.
func spread(ks []kept, n int) []kept {
	if n <= 0 {
		return nil
	}
	if len(ks) <= n {
		return ks
	}
	if n == 1 {
		return ks[len(ks)-1:]
	}
	out := make([]kept, n) // first and last included
	for i := range n {
		out[i] = ks[i*(len(ks)-1)/(n-1)]
	}
	return out
}

// reportTraces gathers the trace IDs the result points at: slowest and
// failed exemplars, hitmap samples and the APM traces.
func reportTraces(r *run.Result) map[string]bool {
	out := map[string]bool{}
	add := func(id string) {
		if id != "" {
			out[strings.ToLower(id)] = true
		}
	}
	if r.Summary != nil {
		for _, k := range r.Summary.Keys {
			for _, e := range k.Slowest {
				add(e.TraceID)
			}
			for _, e := range k.Failed {
				add(e.TraceID)
			}
		}
	}
	for _, sn := range r.Snapshots {
		if sn == nil {
			continue
		}
		for _, h := range []*metrics.Hits{sn.Hits, sn.APMHits} {
			if h != nil {
				for _, x := range h.Samples {
					add(x.TraceID)
				}
			}
		}
		for _, ks := range sn.Keys {
			if ks == nil {
				continue
			}
			if ks.Slow != nil {
				add(ks.Slow.TraceID)
			}
			for _, e := range ks.Failed {
				add(e.TraceID)
			}
		}
	}
	if r.APM != nil {
		for _, t := range r.APM.Traces {
			add(t.TraceID)
		}
	}
	return out
}
