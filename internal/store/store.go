// Package store keeps watch-mode history on disk: one snapshot per second,
// append-only, crash-safe, browsable from an hour to a week back.
//
// Layout of dir (all times UTC):
//
//	2026110115.seg   every second of 15:00–15:59 on 2026-11-01
//	2026110115.min   the same hour pre-aggregated per minute (an index)
//
// Both files are sequences of CRC-checked, DEFLATE-compressed blocks (see
// block.go) holding records (see agg.go). The .seg file is the source of
// truth; the .min sidecar is derived from it and rebuilt on Open for the
// newest segments, so a crash can never leave the two disagreeing for
// long. Range reads .min when its buckets are a minute or wider, which
// makes a 7-day read touch 10,080 records instead of 604,800.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ymg4798/lt/internal/metrics"
)

// Options tunes a Store. The zero value is usable.
type Options struct {
	// Retention is how long segments are kept (default 7 days). Files
	// whose whole hour is older are deleted on Open and then hourly.
	Retention time.Duration
	// FlushEvery is how many seconds are buffered before a block is
	// written (default 5); a crash loses at most this much. A block is
	// also written at every minute boundary and on Close.
	FlushEvery int
	// NoSync skips fsync after each block (tests, benchmarks). Without
	// fsync a process crash still loses nothing written; an OS crash or
	// power loss may lose more.
	NoSync bool
}

// ErrOutOfOrder is returned by Append for a second at or before the last
// one stored; the snapshot is dropped.
var ErrOutOfOrder = errors.New("store: snapshot at or before the last stored second")

// ErrClosed is returned after Close.
var ErrClosed = errors.New("store: closed")

// Store is an on-disk snapshot history. Append is meant to be called from
// one goroutine; Range, Latest and Names may be called concurrently with
// it and with each other.
type Store struct {
	dir string
	opt Options
	now func() time.Time // for tests

	mu     sync.Mutex
	closed bool

	hour             int64 // unix start of the open segment hour; -1 = none
	seg, min         *os.File
	segSize, minSize int64 // bytes of complete blocks written so far

	secW, minW blockWriter
	enc        []byte
	pending    []*agg // seconds written to secW but not yet to disk
	cur        *agg   // the current minute so far
	curMin     int64
	lastAt     int64
	hasLast    bool
	lastPrune  time.Time
}

const segLayout = "2006010215"

// Open opens (creating if needed) the store in dir. It deletes expired
// segments, truncates a torn tail off the newest segment and rebuilds the
// minute index of the newest segments from their seconds.
func Open(dir string, opt Options) (*Store, error) {
	if opt.Retention <= 0 {
		opt.Retention = 7 * 24 * time.Hour
	}
	if opt.FlushEvery <= 0 {
		opt.FlushEvery = 5
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, opt: opt, now: time.Now, hour: -1}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) open() error {
	s.prune()
	hours, err := s.hours()
	if err != nil {
		return err
	}
	for i, h := range hours {
		newest := i >= len(hours)-2 // the only ones a crash can leave behind
		if !newest {
			if _, err := os.Stat(s.path(h, ".min")); err == nil {
				continue
			}
		}
		last, ok, err := s.rebuild(h, i == len(hours)-1)
		if err != nil {
			return fmt.Errorf("store: repair %s: %w", filepath.Base(s.path(h, ".seg")), err)
		}
		if ok && (!s.hasLast || last > s.lastAt) {
			s.lastAt, s.hasLast = last, true
		}
	}
	return nil
}

// rebuild re-derives h's .min from its .seg, optionally cutting a damaged
// tail off the .seg, and reports the newest second found.
func (s *Store) rebuild(h int64, truncate bool) (last int64, found bool, err error) {
	segPath := s.path(h, ".seg")
	data, err := os.ReadFile(segPath)
	if err != nil {
		return 0, false, err
	}
	mins := map[int64]*agg{}
	dec := decoder{intern: map[string]string{}}
	var buf []byte
	good := scanBlocks(data, func(bh blockHeader, payload []byte) bool {
		if bh.kind != kindSeconds {
			return true
		}
		buf, _ = inflate(payload, buf, func(rec []byte) {
			a, err := dec.decode(rec)
			if err != nil {
				return
			}
			m := a.at - mod(a.at, 60)
			x := mins[m]
			if x == nil {
				x = &agg{}
				mins[m] = x
			}
			x.merge(a, minuteSamples)
			if !found || a.at > last {
				last, found = a.at, true
			}
		})
		return true
	})
	if truncate && good < len(data) {
		if err := os.Truncate(segPath, int64(good)); err != nil {
			return 0, false, err
		}
	}
	keys := make([]int64, 0, len(mins))
	for m := range mins {
		keys = append(keys, m)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var w blockWriter
	var enc []byte
	for _, m := range keys {
		a := mins[m]
		a.finish()
		enc = a.encode(enc[:0])
		w.add(a.at, enc)
	}
	tmp := s.path(h, ".min.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return 0, false, err
	}
	if !w.empty() {
		_, err = f.Write(w.build(kindMinutes))
	}
	if err == nil && !s.opt.NoSync {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.path(h, ".min"))
	}
	if err != nil {
		os.Remove(tmp)
		return 0, false, err
	}
	return last, found, nil
}

func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

func (s *Store) path(hour int64, ext string) string {
	return filepath.Join(s.dir, time.Unix(hour, 0).UTC().Format(segLayout)+ext)
}

// hours lists the segment hours on disk, oldest first.
func (s *Store) hours() ([]int64, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".seg")
		if !ok || len(name) != len(segLayout) {
			continue
		}
		t, err := time.ParseInLocation(segLayout, name, time.UTC)
		if err != nil {
			continue
		}
		out = append(out, t.Unix())
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// prune deletes segments whose whole hour is older than the retention.
func (s *Store) prune() {
	now := s.now()
	s.lastPrune = now
	cutoff := now.Add(-s.opt.Retention).Unix()
	hours, err := s.hours()
	if err != nil {
		return
	}
	for _, h := range hours {
		if h+3600 > cutoff || h == s.hour {
			continue
		}
		os.Remove(s.path(h, ".seg"))
		os.Remove(s.path(h, ".min"))
	}
}

// Append stores the snapshot of the second at (truncated to the second).
// Seconds must increase; a repeated or earlier one returns ErrOutOfOrder.
func (s *Store) Append(at time.Time, snap *metrics.Snapshot) error {
	if snap == nil {
		return nil
	}
	sec := at.Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.hasLast && sec <= s.lastAt {
		return ErrOutOfOrder
	}
	hour := sec - mod(sec, 3600)
	minute := sec - mod(sec, 60)
	if hour != s.hour {
		if err := s.rotate(hour); err != nil {
			return err
		}
	} else if s.cur != nil && minute != s.curMin {
		if err := s.flushMinute(); err != nil {
			return err
		}
	}
	a := fromSnapshot(sec, snap)
	s.enc = a.encode(s.enc[:0])
	s.secW.add(sec, s.enc)
	s.pending = append(s.pending, a)
	if s.cur == nil {
		s.cur, s.curMin = &agg{}, minute
	}
	s.cur.merge(a, minuteSamples)
	s.lastAt, s.hasLast = sec, true

	var err error
	if len(s.pending) >= s.opt.FlushEvery {
		err = s.flushSeconds()
	}
	if s.now().Sub(s.lastPrune) >= time.Hour {
		s.prune()
	}
	return err
}

// flushSeconds writes the buffered seconds as one block. Caller holds mu.
func (s *Store) flushSeconds() error {
	if s.secW.empty() {
		return nil
	}
	b := s.secW.build(kindSeconds)
	s.pending = nil
	return s.write(s.seg, &s.segSize, b)
}

// flushMinute writes the buffered seconds and then the finished (or, on
// Close, partial) minute. Caller holds mu.
func (s *Store) flushMinute() error {
	err := s.flushSeconds()
	if s.cur == nil {
		return err
	}
	s.cur.finish()
	s.enc = s.cur.encode(s.enc[:0])
	s.minW.add(s.cur.at, s.enc)
	s.cur = nil
	if werr := s.write(s.min, &s.minSize, s.minW.build(kindMinutes)); err == nil {
		err = werr
	}
	return err
}

func (s *Store) write(f *os.File, size *int64, b []byte) error {
	if f == nil {
		return ErrClosed
	}
	_, err := f.Write(b)
	if err == nil && !s.opt.NoSync {
		err = f.Sync()
	}
	if err != nil {
		// A short write leaves a torn block; readers skip it, but the
		// size readers trust must match the file.
		if st, serr := f.Stat(); serr == nil {
			*size = st.Size()
		}
		return err
	}
	*size += int64(len(b))
	return nil
}

// rotate finishes the open hour and opens (appending to) another.
func (s *Store) rotate(hour int64) error {
	err := s.closeFiles()
	seg, e1 := openAppend(s.path(hour, ".seg"))
	if e1 != nil {
		return e1
	}
	min, e2 := openAppend(s.path(hour, ".min"))
	if e2 != nil {
		seg.Close()
		return e2
	}
	s.seg, s.min, s.hour = seg, min, hour
	s.segSize, s.minSize = fileSize(seg), fileSize(min)
	return err
}

func openAppend(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
}

func fileSize(f *os.File) int64 {
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	return st.Size()
}

func (s *Store) closeFiles() error {
	if s.seg == nil {
		return nil
	}
	err := s.flushMinute()
	for _, f := range []*os.File{s.seg, s.min} {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	s.seg, s.min, s.hour = nil, nil, -1
	s.segSize, s.minSize = 0, 0
	return err
}

// Close writes everything buffered and closes the files. The store can be
// reopened with Open and appended to again.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.closeFiles()
}
