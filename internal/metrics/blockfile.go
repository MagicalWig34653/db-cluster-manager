// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Block file layout (all integers little endian unless varint):
//
//	magic "MTBLK001"
//	blobs (one per series)
//	index: uvarint n, then per entry: labels, uvarint off, uvarint len,
//	       u32 crc of the blob, uvarint sample count, varint minT, varint maxT
//	footer (40 bytes): u64 indexOff, u32 indexLen, u32 indexCRC,
//	       i64 windowMin, i64 windowMax, magic "MTEND001"
const (
	blockMagic  = "MTBLK001"
	footerMagic = "MTEND001"
	footerSize  = 40
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type entry struct {
	labels     Labels
	off, size  uint64
	crc        uint32
	count      int
	minT, maxT int64
}

// blockFile is the parsed index of one block file. The data is read through
// open, which yields either a local file or an in-memory archive block.
type blockFile struct {
	path       string
	mem        []byte // set for archive blocks
	winMin     int64
	winMax     int64
	entries    []entry
	size       int64
	indexBytes int
}

type blobItem struct {
	labels     Labels
	blob       []byte
	count      int
	minT, maxT int64
}

func putString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func putLabels(b []byte, l Labels) []byte {
	b = binary.AppendUvarint(b, uint64(len(l)))
	for _, x := range l {
		b = putString(b, x.Name)
		b = putString(b, x.Value)
	}
	return b
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = errors.New("bad uvarint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.err = errors.New("bad varint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) bytes(n uint64) []byte {
	if d.err != nil {
		return nil
	}
	if n > uint64(len(d.b)) {
		d.err = errShortStream
		return nil
	}
	out := d.b[:n]
	d.b = d.b[n:]
	return out
}

func (d *decoder) str() string { return string(d.bytes(d.uvarint())) }

func (d *decoder) u32() uint32 {
	b := d.bytes(4)
	if d.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (d *decoder) labels() Labels {
	n := d.uvarint()
	if d.err != nil || n > 1024 {
		if d.err == nil {
			d.err = errors.New("too many labels")
		}
		return nil
	}
	l := make(Labels, 0, n)
	for i := uint64(0); i < n; i++ {
		name := d.str()
		l = append(l, Label{name, d.str()})
	}
	return l
}

func buildBlockBytes(winMin, winMax int64, items []blobItem) []byte {
	sort.Slice(items, func(i, j int) bool { return items[i].labels.Key() < items[j].labels.Key() })
	out := []byte(blockMagic)
	idx := binary.AppendUvarint(nil, uint64(len(items)))
	for _, it := range items {
		off := uint64(len(out))
		out = append(out, it.blob...)
		idx = putLabels(idx, it.labels)
		idx = binary.AppendUvarint(idx, off)
		idx = binary.AppendUvarint(idx, uint64(len(it.blob)))
		idx = binary.LittleEndian.AppendUint32(idx, crc32.Checksum(it.blob, crcTable))
		idx = binary.AppendUvarint(idx, uint64(it.count))
		idx = binary.AppendVarint(idx, it.minT)
		idx = binary.AppendVarint(idx, it.maxT)
	}
	indexOff := uint64(len(out))
	out = append(out, idx...)
	out = binary.LittleEndian.AppendUint64(out, indexOff)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(idx)))
	out = binary.LittleEndian.AppendUint32(out, crc32.Checksum(idx, crcTable))
	out = binary.LittleEndian.AppendUint64(out, uint64(winMin))
	out = binary.LittleEndian.AppendUint64(out, uint64(winMax))
	return append(out, footerMagic...)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// writeFileAtomic writes data to path via a temporary file and rename.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func parseBlock(r io.ReaderAt, size int64) (*blockFile, error) {
	if size < int64(len(blockMagic))+footerSize {
		return nil, fmt.Errorf("%w: file too short", ErrCorruptBlock)
	}
	foot := make([]byte, footerSize)
	if _, err := r.ReadAt(foot, size-footerSize); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
	}
	if string(foot[32:]) != footerMagic {
		return nil, fmt.Errorf("%w: bad footer magic", ErrCorruptBlock)
	}
	indexOff := binary.LittleEndian.Uint64(foot[0:])
	indexLen := binary.LittleEndian.Uint32(foot[8:])
	indexCRC := binary.LittleEndian.Uint32(foot[12:])
	if indexOff > uint64(size) || uint64(indexLen) > uint64(size)-indexOff {
		return nil, fmt.Errorf("%w: index out of range", ErrCorruptBlock)
	}
	idx := make([]byte, indexLen)
	if _, err := r.ReadAt(idx, int64(indexOff)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
	}
	if crc32.Checksum(idx, crcTable) != indexCRC {
		return nil, fmt.Errorf("%w: index checksum mismatch", ErrCorruptBlock)
	}
	head := make([]byte, len(blockMagic))
	if _, err := r.ReadAt(head, 0); err != nil || string(head) != blockMagic {
		return nil, fmt.Errorf("%w: bad magic", ErrCorruptBlock)
	}
	b := &blockFile{
		winMin: int64(binary.LittleEndian.Uint64(foot[16:])), winMax: int64(binary.LittleEndian.Uint64(foot[24:])),
		size: size, indexBytes: int(indexLen),
	}
	d := &decoder{b: idx}
	n := d.uvarint()
	if d.err != nil || n > uint64(indexLen) {
		return nil, fmt.Errorf("%w: bad index", ErrCorruptBlock)
	}
	b.entries = make([]entry, 0, n)
	for i := uint64(0); i < n; i++ {
		var e entry
		e.labels = d.labels()
		e.off = d.uvarint()
		e.size = d.uvarint()
		e.crc = d.u32()
		e.count = int(d.uvarint())
		e.minT = d.varint()
		e.maxT = d.varint()
		if d.err != nil || e.off > indexOff || e.size > indexOff-e.off {
			return nil, fmt.Errorf("%w: bad index entry", ErrCorruptBlock)
		}
		b.entries = append(b.entries, e)
	}
	return b, nil
}

func openBlockPath(path string) (*blockFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b, err := parseBlock(f, st.Size())
	if err != nil {
		return nil, err
	}
	b.path = path
	return b, nil
}

func parseBlockBytes(data []byte) (*blockFile, error) {
	b, err := parseBlock(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	b.mem = data
	return b, nil
}

// reader gives access to blob data; the returned function releases it.
func (b *blockFile) reader() (io.ReaderAt, func(), error) {
	if b.mem != nil {
		return bytes.NewReader(b.mem), func() {}, nil
	}
	f, err := os.Open(b.path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

func readBlob(r io.ReaderAt, e entry) ([]byte, error) {
	buf := make([]byte, e.size)
	if _, err := r.ReadAt(buf, int64(e.off)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
	}
	if crc32.Checksum(buf, crcTable) != e.crc {
		return nil, fmt.Errorf("%w: chunk checksum mismatch for %s", ErrCorruptBlock, e.labels)
	}
	return buf, nil
}

// Raw chunk: uvarint n, uvarint timesLen, times, floats.
func encodeRawChunk(s []Sample) []byte {
	ts := make([]int64, len(s))
	vs := make([]float64, len(s))
	for i, x := range s {
		ts[i], vs[i] = x.T, x.V
	}
	te := encodeTimes(ts)
	out := binary.AppendUvarint(nil, uint64(len(s)))
	out = binary.AppendUvarint(out, uint64(len(te)))
	out = append(out, te...)
	return append(out, encodeFloats(vs)...)
}

func decodeRawChunk(b []byte) ([]Sample, error) {
	d := &decoder{b: b}
	n := d.uvarint()
	tl := d.uvarint()
	te := d.bytes(tl)
	if d.err != nil || n > 1<<24 {
		return nil, fmt.Errorf("%w: bad chunk header", ErrCorruptBlock)
	}
	ts, err := decodeTimes(te, int(n))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
	}
	vs, err := decodeFloats(d.b, int(n))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
	}
	out := make([]Sample, n)
	for i := range out {
		out[i] = Sample{ts[i], vs[i]}
	}
	return out, nil
}

// Bucket is one rollup interval. Inc is the counter increase (reset aware)
// contributed by the samples of the bucket, including the step from the
// previous sample of the same block.
type Bucket struct {
	T                               int64
	Count, Sum, Min, Max, Last, Inc float64
}

func buildBuckets(s []Sample, res int64) []Bucket {
	var out []Bucket
	var prev float64
	for i, x := range s {
		start := x.T - mod(x.T, res)
		if len(out) == 0 || out[len(out)-1].T != start {
			out = append(out, Bucket{T: start, Min: x.V, Max: x.V})
		}
		b := &out[len(out)-1]
		b.Count++
		b.Sum += x.V
		if x.V < b.Min {
			b.Min = x.V
		}
		if x.V > b.Max {
			b.Max = x.V
		}
		b.Last = x.V
		if i > 0 {
			if x.V >= prev {
				b.Inc += x.V - prev
			} else {
				b.Inc += x.V
			}
		}
		prev = x.V
	}
	return out
}

func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

// Agg chunk: uvarint nres, then per resolution: uvarint resMs, uvarint nb,
// then 7 length-prefixed streams (times, count, sum, min, max, last, inc).
func encodeAggChunk(s []Sample, resMs []int64) []byte {
	out := binary.AppendUvarint(nil, uint64(len(resMs)))
	for _, r := range resMs {
		bs := buildBuckets(s, r)
		out = binary.AppendUvarint(out, uint64(r))
		out = binary.AppendUvarint(out, uint64(len(bs)))
		ts := make([]int64, len(bs))
		cols := [6][]float64{}
		for i, b := range bs {
			ts[i] = b.T
			for k, v := range [6]float64{b.Count, b.Sum, b.Min, b.Max, b.Last, b.Inc} {
				cols[k] = append(cols[k], v)
			}
		}
		te := encodeTimes(ts)
		out = binary.AppendUvarint(out, uint64(len(te)))
		out = append(out, te...)
		for k := range cols {
			e := encodeFloats(cols[k])
			out = binary.AppendUvarint(out, uint64(len(e)))
			out = append(out, e...)
		}
	}
	return out
}

func decodeAggChunk(b []byte, resMs int64) ([]Bucket, error) {
	d := &decoder{b: b}
	nres := d.uvarint()
	for i := uint64(0); i < nres && d.err == nil; i++ {
		r := int64(d.uvarint())
		nb := d.uvarint()
		streams := make([][]byte, 7)
		for k := range streams {
			streams[k] = d.bytes(d.uvarint())
		}
		if d.err != nil {
			break
		}
		if r != resMs {
			continue
		}
		if nb > 1<<24 {
			return nil, fmt.Errorf("%w: bad bucket count", ErrCorruptBlock)
		}
		ts, err := decodeTimes(streams[0], int(nb))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
		}
		var cols [6][]float64
		for k := 0; k < 6; k++ {
			if cols[k], err = decodeFloats(streams[k+1], int(nb)); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrCorruptBlock, err)
			}
		}
		out := make([]Bucket, nb)
		for i := range out {
			out[i] = Bucket{ts[i], cols[0][i], cols[1][i], cols[2][i], cols[3][i], cols[4][i], cols[5][i]}
		}
		return out, nil
	}
	if d.err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptBlock, d.err)
	}
	return nil, fmt.Errorf("%w: resolution %dms not in block", ErrCorruptBlock, resMs)
}
