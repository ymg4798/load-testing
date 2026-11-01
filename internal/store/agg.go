package store

import (
	"cmp"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"sort"

	"github.com/ymg4798/lt/internal/metrics"
)

// agg is the unit stored on disk and merged on read: the sum of n seconds.
// A second record is an agg with n == 1; a minute record (in the .min
// sidecar) is the agg of up to 60 seconds. Because merging aggs is
// associative, seconds, minutes and downsampled buckets all share it.
type agg struct {
	at int64 // unix second of the first second merged
	n  int   // seconds merged
	t  int   // largest Snapshot.T merged

	dropped, inFlight, users, goroutines int64   // sums
	targetRate, cpu                      float64 // sums

	server map[string]series

	hits, apm *metrics.Hits // OK/Err sums; samples kept separately below
	hitS      []metrics.HitSample
	apmS      []metrics.HitSample

	active []int64 // sums
	keys   map[string]*keyCount
}

// series is one server series inside an agg: average = sum / n.
type series struct {
	sum float64
	n   int64
}

type keyCount struct{ count, errors, bytes int64 }

// Sample caps: per stored second, per stored minute, per returned bucket.
const (
	secondSamples = 20
	minuteSamples = 60
	bucketSamples = 60
)

// fromSnapshot turns one second into an agg, keeping the most
// interesting samples only.
func fromSnapshot(at int64, s *metrics.Snapshot) *agg {
	a := &agg{at: at, n: 1, t: s.T,
		dropped: s.Dropped, inFlight: s.InFlight, users: s.Users,
		goroutines: int64(s.Gen.Goroutines), targetRate: s.TargetRate, cpu: s.Gen.CPU,
	}
	if len(s.Server) > 0 {
		a.server = make(map[string]series, len(s.Server))
		for k, v := range s.Server {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			a.server[k] = series{v, 1}
		}
	}
	if s.Hits != nil {
		a.hits = &metrics.Hits{OK: clone32(s.Hits.OK), Err: clone32(s.Hits.Err)}
		a.hitS = pickSamples(append([]metrics.HitSample(nil), s.Hits.Samples...), secondSamples)
	}
	if s.APMHits != nil {
		a.apm = &metrics.Hits{OK: clone32(s.APMHits.OK), Err: clone32(s.APMHits.Err)}
		a.apmS = pickSamples(append([]metrics.HitSample(nil), s.APMHits.Samples...), secondSamples)
	}
	if len(s.Active) > 0 {
		a.active = append([]int64(nil), s.Active...)
	}
	for k, ks := range s.Keys {
		if ks == nil {
			continue
		}
		if a.keys == nil {
			a.keys = map[string]*keyCount{}
		}
		a.keys[k] = &keyCount{ks.Count, ks.Errors, ks.Bytes}
	}
	return a
}

func clone32(v []int32) []int32 {
	if len(v) == 0 {
		return nil
	}
	return append([]int32(nil), v...)
}

// cmpSample orders samples best first: errors, then slowest; the rest
// only makes the order total so results do not depend on merge order.
func cmpSample(a, b metrics.HitSample) int {
	if ea, eb := a.Err != "", b.Err != ""; ea != eb {
		if ea {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(b.Latency, a.Latency); c != 0 {
		return c
	}
	return cmp.Or(cmp.Compare(a.Key, b.Key), cmp.Compare(a.Off, b.Off),
		cmp.Compare(a.Err, b.Err), cmp.Compare(a.TraceID, b.TraceID))
}

// pushSamples adds src to dst, which is kept as a heap of at most max
// samples with the worst at the root: once full, most candidates are
// rejected with one comparison. dst must be a slice pushSamples built.
func pushSamples(dst, src []metrics.HitSample, max int) []metrics.HitSample {
	if dst == nil && len(src) > 0 && max > 0 {
		dst = make([]metrics.HitSample, 0, max)
	}
	for _, x := range src {
		if len(dst) < max {
			dst = append(dst, x)
			for i := len(dst) - 1; i > 0; { // sift up
				p := (i - 1) / 2
				if cmpSample(dst[p], dst[i]) >= 0 {
					break
				}
				dst[p], dst[i] = dst[i], dst[p]
				i = p
			}
			continue
		}
		if max == 0 || cmpSample(x, dst[0]) >= 0 {
			continue
		}
		dst[0] = x
		for i := 0; ; { // sift down
			w, l, r := i, 2*i+1, 2*i+2
			if l < len(dst) && cmpSample(dst[l], dst[w]) > 0 {
				w = l
			}
			if r < len(dst) && cmpSample(dst[r], dst[w]) > 0 {
				w = r
			}
			if w == i {
				break
			}
			dst[i], dst[w] = dst[w], dst[i]
			i = w
		}
	}
	return dst
}

// pickSamples returns the best max samples of s, best first.
func pickSamples(s []metrics.HitSample, max int) []metrics.HitSample {
	out := pushSamples(nil, s, max)
	slices.SortFunc(out, cmpSample)
	return out
}

// merge adds b into a, keeping the best sampleCap samples (a's sample
// lists are heaps until finish).
func (a *agg) merge(b *agg, sampleCap int) {
	if a.n == 0 || b.at < a.at {
		a.at = b.at
	}
	a.n += b.n
	if b.t > a.t {
		a.t = b.t
	}
	a.dropped += b.dropped
	a.inFlight += b.inFlight
	a.users += b.users
	a.goroutines += b.goroutines
	a.targetRate += b.targetRate
	a.cpu += b.cpu
	if len(b.server) > 0 && a.server == nil {
		a.server = make(map[string]series, len(b.server))
	}
	for k, v := range b.server {
		x := a.server[k]
		x.sum += v.sum
		x.n += v.n
		a.server[k] = x
	}
	if b.hits != nil {
		a.hits = metrics.MergeHits(a.hits, &metrics.Hits{OK: b.hits.OK, Err: b.hits.Err})
	}
	if b.apm != nil {
		a.apm = metrics.MergeHits(a.apm, &metrics.Hits{OK: b.apm.OK, Err: b.apm.Err})
	}
	a.hitS = pushSamples(a.hitS, b.hitS, sampleCap)
	a.apmS = pushSamples(a.apmS, b.apmS, sampleCap)
	if len(b.active) > len(a.active) {
		a.active = append(a.active, make([]int64, len(b.active)-len(a.active))...)
	}
	for i, v := range b.active {
		a.active[i] += v
	}
	if len(b.keys) > 0 && a.keys == nil {
		a.keys = map[string]*keyCount{}
	}
	for k, v := range b.keys {
		x := a.keys[k]
		if x == nil {
			x = &keyCount{}
			a.keys[k] = x
		}
		x.count += v.count
		x.errors += v.errors
		x.bytes += v.bytes
	}
}

// finish orders the samples best first; a must not be merged into after.
func (a *agg) finish() {
	slices.SortFunc(a.hitS, cmpSample)
	slices.SortFunc(a.apmS, cmpSample)
}

// snapshot renders the agg: gauges averaged over the seconds that had
// them, counters (hits, keys, dropped) summed.
func (a *agg) snapshot() *metrics.Snapshot {
	n := float64(a.n)
	if n == 0 {
		n = 1
	}
	s := &metrics.Snapshot{
		T:          a.t,
		Keys:       map[string]*metrics.KeySnap{},
		Dropped:    a.dropped,
		InFlight:   int64(math.Round(float64(a.inFlight) / n)),
		Users:      int64(math.Round(float64(a.users) / n)),
		TargetRate: a.targetRate / n,
	}
	s.Gen.CPU = a.cpu / n
	s.Gen.Goroutines = int(math.Round(float64(a.goroutines) / n))
	if len(a.server) > 0 {
		s.Server = make(map[string]float64, len(a.server))
		for k, v := range a.server {
			if v.n > 0 {
				s.Server[k] = v.sum / float64(v.n)
			}
		}
	}
	if a.hits != nil || len(a.hitS) > 0 {
		s.Hits = &metrics.Hits{Samples: a.hitS}
		if a.hits != nil {
			s.Hits.OK, s.Hits.Err = a.hits.OK, a.hits.Err
		}
	}
	if a.apm != nil || len(a.apmS) > 0 {
		s.APMHits = &metrics.Hits{Samples: a.apmS}
		if a.apm != nil {
			s.APMHits.OK, s.APMHits.Err = a.apm.OK, a.apm.Err
		}
	}
	if len(a.active) > 0 {
		s.Active = make([]int64, len(a.active))
		for i, v := range a.active {
			s.Active[i] = int64(math.Round(float64(v) / n))
		}
	}
	for k, v := range a.keys {
		s.Keys[k] = &metrics.KeySnap{Count: v.count, Errors: v.errors, Bytes: v.bytes}
	}
	return s
}

// ---- encoding ----
//
// record := uvarint(version=1) varint(at) uvarint(n) varint(t)
//           varint(dropped) varint(inFlight) varint(users) varint(goroutines)
//           f64(targetRate) f64(cpu)
//           uvarint(#series) { str(name) f64(sum) uvarint(n) }
//           hits(client) hits(apm)
//           uvarint(#active) { varint }
//           uvarint(#keys) { str(key) varint(count) varint(errors) varint(bytes) }
// hits   := uvarint(#ok) {uvarint} uvarint(#err) {uvarint}
//           uvarint(#samples) { str(key) varint(off) varint(lat) str(err) str(trace) }
// str    := uvarint(len) bytes;  f64 := 8 bytes little endian
// Arrays are trimmed of trailing zeros before writing.

const recVersion = 1

func (a *agg) encode(b []byte) []byte {
	b = binary.AppendUvarint(b, recVersion)
	b = binary.AppendVarint(b, a.at)
	b = binary.AppendUvarint(b, uint64(a.n))
	b = binary.AppendVarint(b, int64(a.t))
	b = binary.AppendVarint(b, a.dropped)
	b = binary.AppendVarint(b, a.inFlight)
	b = binary.AppendVarint(b, a.users)
	b = binary.AppendVarint(b, a.goroutines)
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(a.targetRate))
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(a.cpu))
	names := make([]string, 0, len(a.server))
	for k := range a.server {
		names = append(names, k)
	}
	sort.Strings(names) // stable order compresses better
	b = binary.AppendUvarint(b, uint64(len(names)))
	for _, k := range names {
		v := a.server[k]
		b = appendStr(b, k)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v.sum))
		b = binary.AppendUvarint(b, uint64(v.n))
	}
	b = appendHits(b, a.hits, a.hitS)
	b = appendHits(b, a.apm, a.apmS)
	act := trim64(a.active)
	b = binary.AppendUvarint(b, uint64(len(act)))
	for _, v := range act {
		b = binary.AppendVarint(b, v)
	}
	keys := make([]string, 0, len(a.keys))
	for k := range a.keys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b = binary.AppendUvarint(b, uint64(len(keys)))
	for _, k := range keys {
		v := a.keys[k]
		b = appendStr(b, k)
		b = binary.AppendVarint(b, v.count)
		b = binary.AppendVarint(b, v.errors)
		b = binary.AppendVarint(b, v.bytes)
	}
	return b
}

func appendStr(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendHits(b []byte, h *metrics.Hits, samples []metrics.HitSample) []byte {
	var ok, er []int32
	if h != nil {
		ok, er = trim32(h.OK), trim32(h.Err)
	}
	for _, v := range [][]int32{ok, er} {
		b = binary.AppendUvarint(b, uint64(len(v)))
		for _, x := range v {
			b = binary.AppendUvarint(b, uint64(uint32(x)))
		}
	}
	b = binary.AppendUvarint(b, uint64(len(samples)))
	for _, s := range samples {
		b = appendStr(b, s.Key)
		b = binary.AppendVarint(b, int64(s.Off))
		b = binary.AppendVarint(b, s.Latency)
		b = appendStr(b, s.Err)
		b = appendStr(b, s.TraceID)
	}
	return b
}

func trim32(v []int32) []int32 {
	n := len(v)
	for n > 0 && v[n-1] == 0 {
		n--
	}
	return v[:n]
}

func trim64(v []int64) []int64 {
	n := len(v)
	for n > 0 && v[n-1] == 0 {
		n--
	}
	return v[:n]
}

var errCorrupt = errors.New("store: corrupt record")

// decoder reads records; it interns strings so a range read does not
// allocate a new name for every series of every second.
type decoder struct {
	b      []byte
	err    bool
	intern map[string]string

	scratch agg
	hitsBuf [2]metrics.Hits
}

func (d *decoder) uvarint() uint64 {
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = true
		d.b = nil
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) varint() int64 {
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.err = true
		d.b = nil
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) f64() float64 {
	if len(d.b) < 8 {
		d.err = true
		d.b = nil
		return 0
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(d.b))
	d.b = d.b[8:]
	return v
}

// count reads a length and sanity-checks it against what is left.
func (d *decoder) count() int {
	v := d.uvarint()
	if v > uint64(len(d.b)) {
		d.err = true
		d.b = nil
		return 0
	}
	return int(v)
}

func (d *decoder) str() string {
	n := d.count()
	if n == 0 {
		return ""
	}
	raw := d.b[:n]
	d.b = d.b[n:]
	if s, ok := d.intern[string(raw)]; ok { // no allocation for the lookup
		return s
	}
	s := string(raw)
	if d.intern != nil && len(d.intern) < 1<<16 {
		d.intern[s] = s
	}
	return s
}

// decode parses one record into the decoder's scratch agg, reusing its
// maps and slices: the result is valid only until the next decode, so
// callers merge it into an agg of their own (merge copies).
func (d *decoder) decode(rec []byte) (*agg, error) {
	d.b, d.err = rec, false
	if d.uvarint() != recVersion {
		return nil, errCorrupt
	}
	a := &d.scratch
	server, keys := a.server, a.keys
	*a = agg{hitS: a.hitS[:0], apmS: a.apmS[:0], active: a.active[:0]}
	a.at = d.varint()
	a.n = int(d.uvarint())
	a.t = int(d.varint())
	a.dropped = d.varint()
	a.inFlight = d.varint()
	a.users = d.varint()
	a.goroutines = d.varint()
	a.targetRate = d.f64()
	a.cpu = d.f64()
	if ns := d.count(); ns > 0 {
		if server == nil {
			server = make(map[string]series, ns)
		}
		clear(server)
		a.server = server
		for range ns {
			k := d.str()
			sum := d.f64()
			server[k] = series{sum, int64(d.uvarint())}
		}
	}
	a.hits, a.hitS = d.hits(&d.hitsBuf[0], a.hitS)
	a.apm, a.apmS = d.hits(&d.hitsBuf[1], a.apmS)
	for range d.count() {
		a.active = append(a.active, d.varint())
	}
	if nk := d.count(); nk > 0 {
		if keys == nil {
			keys = make(map[string]*keyCount, nk)
		}
		clear(keys)
		a.keys = keys
		for range nk {
			k := d.str()
			keys[k] = &keyCount{d.varint(), d.varint(), d.varint()}
		}
	}
	if d.err {
		return nil, errCorrupt
	}
	return a, nil
}

func (d *decoder) hits(h *metrics.Hits, ss []metrics.HitSample) (*metrics.Hits, []metrics.HitSample) {
	arr := [2][]int32{h.OK[:0], h.Err[:0]}
	for i := range arr {
		for range d.count() {
			arr[i] = append(arr[i], int32(uint32(d.uvarint())))
		}
	}
	h.OK, h.Err = arr[0], arr[1]
	for range d.count() {
		ss = append(ss, metrics.HitSample{Key: d.str(), Off: int(d.varint()), Latency: d.varint(), Err: d.str(), TraceID: d.str()})
	}
	if len(h.OK) == 0 && len(h.Err) == 0 {
		return nil, ss
	}
	return h, ss
}
