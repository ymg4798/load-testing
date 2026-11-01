package store

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
	"sync"
)

// Block framing. A segment (.seg) and its minute sidecar (.min) are both a
// plain sequence of blocks, appended and never rewritten in place:
//
//	offset size
//	0      4    magic "LTB1"
//	4      4    payload length (compressed bytes), little endian
//	8      4    CRC32-Castagnoli of bytes 12..end of payload
//	12     1    kind: 'S' seconds, 'M' minutes
//	13     8    first: smallest record time (unix s)
//	21     8    last:  largest record time (unix s)
//	29     4    record count
//	33     len  raw DEFLATE of: { uvarint(len) record }*
//
// The CRC covers the header fields after it and the payload, so a block
// is either wholly valid or rejected. A reader that meets a short, torn or
// corrupt block skips forward to the next magic and carries on, so a torn
// tail (crash mid-write) costs only that block. first/last let a reader
// skip blocks outside the range without inflating them.

const (
	blockMagic  = "LTB1"
	headerSize  = 33
	maxBlockLen = 64 << 20
	kindSeconds = 'S'
	kindMinutes = 'M'
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type blockHeader struct {
	kind        byte
	first, last int64
	count       uint32
	length      int
}

// blockWriter builds compressed blocks; not safe for concurrent use.
type blockWriter struct {
	raw bytes.Buffer
	cmp bytes.Buffer
	fw  *flate.Writer
	n   uint32
	lo  int64
	hi  int64
}

func (w *blockWriter) add(at int64, rec []byte) {
	if w.n == 0 || at < w.lo {
		w.lo = at
	}
	if w.n == 0 || at > w.hi {
		w.hi = at
	}
	w.n++
	var l [binary.MaxVarintLen64]byte
	w.raw.Write(l[:binary.PutUvarint(l[:], uint64(len(rec)))])
	w.raw.Write(rec)
}

func (w *blockWriter) empty() bool { return w.n == 0 }

// build returns the framed block and resets the writer.
func (w *blockWriter) build(kind byte) []byte {
	w.cmp.Reset()
	w.cmp.Write(make([]byte, headerSize))
	if w.fw == nil {
		w.fw, _ = flate.NewWriter(&w.cmp, flate.DefaultCompression)
	} else {
		w.fw.Reset(&w.cmp)
	}
	w.fw.Write(w.raw.Bytes())
	w.fw.Close()
	b := w.cmp.Bytes()
	copy(b[0:4], blockMagic)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-headerSize))
	b[12] = kind
	binary.LittleEndian.PutUint64(b[13:], uint64(w.lo))
	binary.LittleEndian.PutUint64(b[21:], uint64(w.hi))
	binary.LittleEndian.PutUint32(b[29:], w.n)
	binary.LittleEndian.PutUint32(b[8:], crc32.Checksum(b[12:], crcTable))
	w.raw.Reset()
	w.n = 0
	return b
}

// parseHeader validates the block at b[0:]. It returns ok=false when the
// bytes there are not a complete, intact block.
func parseHeader(b []byte) (blockHeader, bool) {
	if len(b) < headerSize || string(b[:4]) != blockMagic {
		return blockHeader{}, false
	}
	n := int(binary.LittleEndian.Uint32(b[4:]))
	if n > maxBlockLen || headerSize+n > len(b) {
		return blockHeader{}, false
	}
	if crc32.Checksum(b[12:headerSize+n], crcTable) != binary.LittleEndian.Uint32(b[8:]) {
		return blockHeader{}, false
	}
	return blockHeader{
		kind:   b[12],
		first:  int64(binary.LittleEndian.Uint64(b[13:])),
		last:   int64(binary.LittleEndian.Uint64(b[21:])),
		count:  binary.LittleEndian.Uint32(b[29:]),
		length: n,
	}, true
}

// scanBlocks calls fn for every valid block in data, resynchronising on
// the next magic after damage. It returns the end offset of the last
// valid block (what a writer may truncate a damaged tail to).
func scanBlocks(data []byte, fn func(h blockHeader, payload []byte) bool) (goodEnd int) {
	off := 0
	for off < len(data) {
		h, ok := parseHeader(data[off:])
		if !ok {
			i := bytes.Index(data[off+1:], []byte(blockMagic))
			if i < 0 {
				return goodEnd
			}
			off += 1 + i
			continue
		}
		end := off + headerSize + h.length
		goodEnd = end
		if fn != nil && !fn(h, data[off+headerSize:end]) {
			return goodEnd
		}
		off = end
	}
	return goodEnd
}

var inflaters = sync.Pool{New: func() any { return flate.NewReader(bytes.NewReader(nil)) }}

// inflate decompresses a block payload into dst and splits it into records.
func inflate(payload []byte, dst []byte, fn func(rec []byte)) ([]byte, error) {
	r := inflaters.Get().(io.ReadCloser)
	defer inflaters.Put(r)
	if err := r.(flate.Resetter).Reset(bytes.NewReader(payload), nil); err != nil {
		return dst, err
	}
	buf := bytes.NewBuffer(dst[:0])
	if _, err := buf.ReadFrom(r); err != nil {
		return buf.Bytes(), err
	}
	b := buf.Bytes()
	for len(b) > 0 {
		l, n := binary.Uvarint(b)
		if n <= 0 || uint64(len(b)-n) < l {
			return buf.Bytes(), errCorrupt
		}
		fn(b[n : n+int(l)])
		b = b[n+int(l):]
	}
	return buf.Bytes(), nil
}
