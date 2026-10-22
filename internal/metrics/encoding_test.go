// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"math"
	"math/rand"
	"testing"
)

func TestTimesRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cases := [][]int64{
		nil,
		{5},
		{5, 6},
		{0, 15000, 30000, 45000, 60000},
		{math.MinInt64, 0, math.MaxInt64}, // wrap-around exercises the 64-bit escape
	}
	for i := 0; i < 200; i++ {
		n := rng.Intn(500) + 1
		ts := make([]int64, n)
		cur := rng.Int63n(1 << 40)
		for j := range ts {
			switch rng.Intn(4) {
			case 0:
				cur += 15000
			case 1:
				cur += 15000 + int64(rng.Intn(200)-100)
			case 2:
				cur += int64(rng.Intn(5000000))
			default:
				cur += int64(rng.Intn(3)) // includes zero deltas
			}
			ts[j] = cur
		}
		cases = append(cases, ts)
	}
	for i, ts := range cases {
		enc := encodeTimes(ts)
		got, err := decodeTimes(enc, len(ts))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		for j := range ts {
			if got[j] != ts[j] {
				t.Fatalf("case %d idx %d: got %d want %d", i, j, got[j], ts[j])
			}
		}
	}
}

func TestFloatsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	cases := [][]float64{
		nil,
		{1.5},
		{0, 0, 0, 0},
		{math.NaN(), math.Inf(1), math.Inf(-1), -0.0, math.MaxFloat64, math.SmallestNonzeroFloat64},
	}
	for i := 0; i < 200; i++ {
		n := rng.Intn(500) + 1
		vs := make([]float64, n)
		cur := rng.Float64() * 1000
		for j := range vs {
			switch rng.Intn(4) {
			case 0:
				cur += 1
			case 1:
				cur = rng.NormFloat64() * 1e6
			case 2:
				cur = math.Float64frombits(rng.Uint64())
			default: // repeated value
			}
			vs[j] = cur
		}
		cases = append(cases, vs)
	}
	for i, vs := range cases {
		enc := encodeFloats(vs)
		got, err := decodeFloats(enc, len(vs))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		for j := range vs {
			if math.Float64bits(got[j]) != math.Float64bits(vs[j]) {
				t.Fatalf("case %d idx %d: got %v want %v", i, j, got[j], vs[j])
			}
		}
	}
}

func TestDecodeTruncatedFails(t *testing.T) {
	ts := []int64{1000, 2000, 3500, 9000, 9001}
	enc := encodeTimes(ts)
	if _, err := decodeTimes(enc[:len(enc)-3], len(ts)); err == nil {
		t.Fatal("expected error for truncated timestamps")
	}
	vs := []float64{1, 2.5, 3.25, 100}
	fe := encodeFloats(vs)
	if _, err := decodeFloats(fe[:3], len(vs)); err == nil {
		t.Fatal("expected error for truncated floats")
	}
}

func TestCompressionRatioRegularScrape(t *testing.T) {
	n := 480
	ts := make([]int64, n)
	vs := make([]float64, n)
	for i := range ts {
		ts[i] = 1700000000000 + int64(i)*15000
		vs[i] = float64(i * 3)
	}
	size := len(encodeTimes(ts)) + len(encodeFloats(vs))
	if size > n*16/4 {
		t.Fatalf("regular series compresses poorly: %d bytes for %d samples", size, n)
	}
}
