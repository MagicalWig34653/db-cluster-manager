// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"os"
)

// WAL record framing: u32 payload length, u32 CRC32C of the payload, payload.
// Payload: type byte, then
//
//	1 (series): uvarint id, labels
//	2 (samples): uvarint n, n x (uvarint id, varint t, u64 value bits)
//
// Replay stops at the first short, oversized or corrupt record; the file is
// truncated there so that appends continue after the last good record.
const (
	walSeries  = 1
	walSamples = 2
	walMaxRec  = 64 << 20
)

func frameRecord(dst, payload []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(payload)))
	dst = binary.LittleEndian.AppendUint32(dst, crc32.Checksum(payload, crcTable))
	return append(dst, payload...)
}

func seriesRecord(dst []byte, id uint64, l Labels) []byte {
	p := []byte{walSeries}
	p = binary.AppendUvarint(p, id)
	p = putLabels(p, l)
	return frameRecord(dst, p)
}

type walSample struct {
	id uint64
	s  Sample
}

func samplesRecord(dst []byte, ss []walSample) []byte {
	p := []byte{walSamples}
	p = binary.AppendUvarint(p, uint64(len(ss)))
	for _, x := range ss {
		p = binary.AppendUvarint(p, x.id)
		p = binary.AppendVarint(p, x.s.T)
		p = binary.LittleEndian.AppendUint64(p, math.Float64bits(x.s.V))
	}
	return frameRecord(dst, p)
}

// replayWAL reads path, calls the handlers per good record and returns the
// number of good bytes. A handler error is treated like a corrupt record.
func replayWAL(path string, onSeries func(uint64, Labels) error, onSamples func([]walSample) error) (good, total int64, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	total = st.Size()
	hdr := make([]byte, 8)
	for good+8 <= total {
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		n := int64(binary.LittleEndian.Uint32(hdr))
		if n == 0 || n > walMaxRec || good+8+n > total {
			break
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(f, p); err != nil {
			break
		}
		if crc32.Checksum(p, crcTable) != binary.LittleEndian.Uint32(hdr[4:]) {
			break
		}
		if !applyWALRecord(p, onSeries, onSamples) {
			break
		}
		good += 8 + n
	}
	return good, total, nil
}

func applyWALRecord(p []byte, onSeries func(uint64, Labels) error, onSamples func([]walSample) error) bool {
	d := &decoder{b: p[1:]}
	switch p[0] {
	case walSeries:
		id := d.uvarint()
		l := d.labels()
		return d.err == nil && onSeries(id, l) == nil
	case walSamples:
		n := d.uvarint()
		if d.err != nil || n > uint64(len(p)) {
			return false
		}
		ss := make([]walSample, 0, n)
		for i := uint64(0); i < n; i++ {
			id := d.uvarint()
			t := d.varint()
			vb := d.bytes(8)
			if d.err != nil {
				return false
			}
			ss = append(ss, walSample{id, Sample{t, math.Float64frombits(binary.LittleEndian.Uint64(vb))}})
		}
		return onSamples(ss) == nil
	}
	return false
}
