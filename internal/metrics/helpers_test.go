// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct{ ms atomic.Int64 }

func (c *fakeClock) Now() time.Time { return time.UnixMilli(c.ms.Load()) }
func (c *fakeClock) Set(ms int64)   { c.ms.Store(ms) }

const (
	min1 = int64(60000)
	hour = 60 * min1
	day  = 24 * hour
)

// base is a block-aligned origin (a multiple of 2h).
const base = int64(1_700_000_000_000) - int64(1_700_000_000_000)%(2*hour)

func testOpts(t *testing.T, mut ...func(*Options)) (Options, *fakeClock) {
	t.Helper()
	clk := &fakeClock{}
	clk.Set(base + day)
	o := DefaultOptions(t.TempDir())
	o.SyncWAL = false
	o.Now = clk.Now
	o.MaxFutureSkew = 1000 * 24 * time.Hour
	for _, m := range mut {
		m(&o)
	}
	return o, clk
}

func openT(t *testing.T, o Options) *Store {
	t.Helper()
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func appendOK(t *testing.T, s *Store, es ...Entry) {
	t.Helper()
	n, err := s.Append(es)
	if err != nil || n != len(es) {
		t.Fatalf("append: n=%d err=%v", n, err)
	}
}

func e(l Labels, ts int64, v float64) Entry { return Entry{l, Sample{ts, v}} }

// fill appends one sample every step ms in [from, to) with value f(t).
func fill(t *testing.T, s *Store, l Labels, from, to, step int64, f func(int64) float64) {
	t.Helper()
	var batch []Entry
	for ts := from; ts < to; ts += step {
		batch = append(batch, e(l, ts, f(ts)))
		if len(batch) == 5000 {
			appendOK(t, s, batch...)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		appendOK(t, s, batch...)
	}
}
