// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"errors"
	"math"
	"math/bits"
)

// The sample encoding follows the Gorilla scheme (Pelkonen et al., VLDB 2015):
// timestamps as delta-of-delta with prefix-coded size classes, values as the
// XOR with the previous value storing only the meaningful bit window. The
// element count is stored by the caller, not in the stream.

var errShortStream = errors.New("metrics: encoded stream is truncated")

type bitWriter struct {
	buf  []byte
	free uint8 // unused low bits in the last byte
}

func (w *bitWriter) writeBit(b bool) {
	if w.free == 0 {
		w.buf = append(w.buf, 0)
		w.free = 8
	}
	w.free--
	if b {
		w.buf[len(w.buf)-1] |= 1 << w.free
	}
}

// writeBits writes the low n bits of v, most significant first.
func (w *bitWriter) writeBits(v uint64, n int) {
	for n > 0 {
		if w.free == 0 {
			w.buf = append(w.buf, 0)
			w.free = 8
		}
		take := int(w.free)
		if take > n {
			take = n
		}
		chunk := (v >> uint(n-take)) & (1<<uint(take) - 1)
		w.buf[len(w.buf)-1] |= byte(chunk << (w.free - uint8(take)))
		w.free -= uint8(take)
		n -= take
	}
}

type bitReader struct {
	buf  []byte
	pos  int // byte index
	used uint8
}

func (r *bitReader) readBit() (bool, error) {
	v, err := r.readBits(1)
	return v == 1, err
}

func (r *bitReader) readBits(n int) (uint64, error) {
	var out uint64
	for n > 0 {
		if r.pos >= len(r.buf) {
			return 0, errShortStream
		}
		avail := 8 - int(r.used)
		take := avail
		if take > n {
			take = n
		}
		b := uint64(r.buf[r.pos])
		chunk := (b >> uint(avail-take)) & (1<<uint(take) - 1)
		out = out<<uint(take) | chunk
		r.used += uint8(take)
		if r.used == 8 {
			r.used = 0
			r.pos++
		}
		n -= take
	}
	return out, nil
}

// encodeTimes encodes timestamps (any int64 sequence, normally ascending).
func encodeTimes(ts []int64) []byte {
	if len(ts) == 0 {
		return nil
	}
	w := &bitWriter{}
	w.writeBits(uint64(ts[0]), 64)
	if len(ts) == 1 {
		return w.buf
	}
	prevDelta := ts[1] - ts[0]
	w.writeBits(uint64(prevDelta), 64)
	for i := 2; i < len(ts); i++ {
		delta := ts[i] - ts[i-1]
		dod := delta - prevDelta
		prevDelta = delta
		switch {
		case dod == 0:
			w.writeBit(false)
		case dod >= -63 && dod <= 64:
			w.writeBits(0b10, 2)
			w.writeBits(uint64(dod+63), 7)
		case dod >= -255 && dod <= 256:
			w.writeBits(0b110, 3)
			w.writeBits(uint64(dod+255), 9)
		case dod >= -2047 && dod <= 2048:
			w.writeBits(0b1110, 4)
			w.writeBits(uint64(dod+2047), 12)
		default:
			w.writeBits(0b1111, 4)
			w.writeBits(uint64(dod), 64)
		}
	}
	return w.buf
}

func decodeTimes(b []byte, n int) ([]int64, error) {
	if n == 0 {
		return nil, nil
	}
	r := &bitReader{buf: b}
	out := make([]int64, 0, n)
	v, err := r.readBits(64)
	if err != nil {
		return nil, err
	}
	out = append(out, int64(v))
	if n == 1 {
		return out, nil
	}
	d, err := r.readBits(64)
	if err != nil {
		return nil, err
	}
	prevDelta := int64(d)
	out = append(out, out[0]+prevDelta)
	for i := 2; i < n; i++ {
		var dod int64
		size := 0
		for size < 4 {
			bit, err := r.readBit()
			if err != nil {
				return nil, err
			}
			if !bit {
				break
			}
			size++
		}
		switch size {
		case 0:
		case 1:
			x, err := r.readBits(7)
			if err != nil {
				return nil, err
			}
			dod = int64(x) - 63
		case 2:
			x, err := r.readBits(9)
			if err != nil {
				return nil, err
			}
			dod = int64(x) - 255
		case 3:
			x, err := r.readBits(12)
			if err != nil {
				return nil, err
			}
			dod = int64(x) - 2047
		default:
			x, err := r.readBits(64)
			if err != nil {
				return nil, err
			}
			dod = int64(x)
		}
		prevDelta += dod
		out = append(out, out[i-1]+prevDelta)
	}
	return out, nil
}

// encodeFloats encodes float64 values with XOR compression. NaN payloads and
// signed zeros are preserved bit-exactly.
func encodeFloats(vs []float64) []byte {
	if len(vs) == 0 {
		return nil
	}
	w := &bitWriter{}
	prev := math.Float64bits(vs[0])
	w.writeBits(prev, 64)
	prevLead, prevTrail := -1, 0
	for _, f := range vs[1:] {
		cur := math.Float64bits(f)
		x := cur ^ prev
		prev = cur
		if x == 0 {
			w.writeBit(false)
			continue
		}
		w.writeBit(true)
		lead := bits.LeadingZeros64(x)
		if lead > 31 {
			lead = 31
		}
		trail := bits.TrailingZeros64(x)
		if prevLead >= 0 && lead >= prevLead && trail >= prevTrail {
			w.writeBit(false)
			w.writeBits(x>>uint(prevTrail), 64-prevLead-prevTrail)
			continue
		}
		w.writeBit(true)
		sig := 64 - lead - trail
		w.writeBits(uint64(lead), 5)
		w.writeBits(uint64(sig-1), 6)
		w.writeBits(x>>uint(trail), sig)
		prevLead, prevTrail = lead, trail
	}
	return w.buf
}

func decodeFloats(b []byte, n int) ([]float64, error) {
	if n == 0 {
		return nil, nil
	}
	r := &bitReader{buf: b}
	out := make([]float64, 0, n)
	prev, err := r.readBits(64)
	if err != nil {
		return nil, err
	}
	out = append(out, math.Float64frombits(prev))
	lead, trail := -1, 0
	for i := 1; i < n; i++ {
		nz, err := r.readBit()
		if err != nil {
			return nil, err
		}
		if !nz {
			out = append(out, math.Float64frombits(prev))
			continue
		}
		newWin, err := r.readBit()
		if err != nil {
			return nil, err
		}
		if newWin {
			l, err := r.readBits(5)
			if err != nil {
				return nil, err
			}
			s, err := r.readBits(6)
			if err != nil {
				return nil, err
			}
			lead = int(l)
			sig := int(s) + 1
			trail = 64 - lead - sig
			if trail < 0 {
				return nil, errors.New("metrics: corrupt float stream")
			}
		} else if lead < 0 {
			return nil, errors.New("metrics: corrupt float stream")
		}
		x, err := r.readBits(64 - lead - trail)
		if err != nil {
			return nil, err
		}
		prev ^= x << uint(trail)
		out = append(out, math.Float64frombits(prev))
	}
	return out, nil
}
