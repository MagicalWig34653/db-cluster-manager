// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendOutOfOrderAndTooOld(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	l := MustLabels("m")
	appendOK(t, s, e(l, base+10*min1, 1))
	// within the 5 minute window, out of order: accepted
	appendOK(t, s, e(l, base+7*min1, 2))
	// too old
	n, err := s.Append([]Entry{e(l, base+4*min1, 3), e(l, base+11*min1, 4)})
	if n != 1 || !errors.Is(err, ErrTooOld) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	st := s.Stats()
	if st.RejectedTooOld != 1 || st.HeadSamples != 3 {
		t.Fatalf("stats %+v", st)
	}
	res, err := s.Query(context.Background(), Query{Selector: mustSel(t, "m"), Start: base, End: base + 12*min1, Step: min1, Func: FuncLast, Authorizer: AllowAll})
	if err != nil {
		t.Fatal(err)
	}
	if got := valueAt(res.Series[0], base+7*min1); got != 2 {
		t.Fatalf("out-of-order sample missing: %v", got)
	}
}

func mustSel(t *testing.T, s string) Selector {
	t.Helper()
	sel, err := ParseSelector(s)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func valueAt(sr SeriesResult, ts int64) float64 {
	for _, p := range sr.Points {
		if p.T == ts {
			return p.V
		}
	}
	return -12345
}

func TestAppendDuplicateAndFuture(t *testing.T) {
	o, clk := testOpts(t, func(o *Options) { o.MaxFutureSkew = 5 * time.Minute })
	s := openT(t, o)
	l := MustLabels("m")
	now := clk.Now().UnixMilli()
	appendOK(t, s, e(l, now, 1))
	if n, err := s.Append([]Entry{e(l, now, 1)}); n != 0 || err != nil {
		t.Fatalf("idempotent duplicate should be silently ignored: n=%d err=%v", n, err)
	}
	if _, err := s.Append([]Entry{e(l, now, 2)}); !errors.Is(err, ErrDuplicateSample) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Append([]Entry{e(l, now+int64(time.Hour/time.Millisecond), 2)}); !errors.Is(err, ErrTooNew) {
		t.Fatalf("got %v", err)
	}
}

func TestCardinalityLimits(t *testing.T) {
	o, _ := testOpts(t, func(o *Options) { o.MaxSeries = 3; o.MaxValuesPerLabel = 2 })
	s := openT(t, o)
	appendOK(t, s,
		e(MustLabels("a", "k", "1"), base, 1),
		e(MustLabels("a", "k", "2"), base, 1))
	_, err := s.Append([]Entry{e(MustLabels("a", "k", "3"), base, 1)})
	if !errors.Is(err, ErrMaxLabelValues) {
		t.Fatalf("want ErrMaxLabelValues, got %v", err)
	}
	appendOK(t, s, e(MustLabels("a", "k", "1", "x", "y"), base, 1)) // third series
	_, err = s.Append([]Entry{e(MustLabels("a", "k", "2", "x", "y"), base, 1)})
	if !errors.Is(err, ErrMaxSeries) {
		t.Fatalf("want ErrMaxSeries, got %v", err)
	}
	// Existing series still accept samples at the limit.
	appendOK(t, s, e(MustLabels("a", "k", "1"), base+1000, 2))
	if s.Stats().RejectedLimits != 2 {
		t.Fatalf("stats %+v", s.Stats())
	}
}

func TestBatchAndHeadLimits(t *testing.T) {
	o, _ := testOpts(t, func(o *Options) { o.MaxBatch = 2; o.MaxHeadSamples = 2 })
	s := openT(t, o)
	l := MustLabels("m")
	if _, err := s.Append([]Entry{e(l, base, 1), e(l, base+1, 1), e(l, base+2, 1)}); !errors.Is(err, ErrBatchTooLarge) {
		t.Fatalf("got %v", err)
	}
	appendOK(t, s, e(l, base, 1), e(l, base+1, 1))
	if _, err := s.Append([]Entry{e(l, base+2, 1)}); !errors.Is(err, ErrHeadFull) {
		t.Fatalf("got %v", err)
	}
}

func TestInvalidLabelsRejected(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	if _, err := s.Append([]Entry{e(MustLabels("1x"), base, 1)}); !errors.Is(err, ErrInvalidLabels) {
		t.Fatalf("got %v", err)
	}
}

func TestSealCreatesBlocksAndSurvivesRestart(t *testing.T) {
	o, _ := testOpts(t)
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	l := MustLabels("cpu", "host", "a")
	fill(t, s, l, base, base+5*hour, 15000, func(ts int64) float64 { return float64(ts-base) / 1000 })
	st := s.Stats()
	if st.Blocks != 2 {
		t.Fatalf("expected 2 sealed blocks (windows ending before newest-5m), got %d", st.Blocks)
	}
	if st.HeadSamples == 0 || st.BlockBytes == 0 {
		t.Fatalf("stats %+v", st)
	}
	before := queryAll(t, s, "cpu", base, base+5*hour-15000, 15000)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openT(t, o)
	after := queryAll(t, s2, "cpu", base, base+5*hour-15000, 15000)
	if before != after || before == 0 {
		t.Fatalf("restart changed data: %v vs %v", before, after)
	}
	// Samples in sealed windows are now too old.
	if _, err := s2.Append([]Entry{e(l, base+1000, 5)}); !errors.Is(err, ErrTooOld) {
		t.Fatalf("got %v", err)
	}
}

// queryAll returns the number of OK points of the first series.
func queryAll(t *testing.T, s *Store, name string, from, to, step int64) int {
	t.Helper()
	res, err := s.Query(context.Background(), Query{Selector: mustSel(t, name), Start: from, End: to, Step: step, Func: FuncLast, Authorizer: AllowAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) == 0 {
		return 0
	}
	n := 0
	for _, p := range res.Series[0].Points {
		if p.State == StateOK {
			n++
		}
	}
	return n
}

func TestRetentionByAgeDropsRawThenBlock(t *testing.T) {
	o, clk := testOpts(t, func(o *Options) { o.RawRetention = 24 * time.Hour; o.AggRetention = 72 * time.Hour })
	s := openT(t, o)
	l := MustLabels("m")
	fill(t, s, l, base, base+7*hour, min1, func(ts int64) float64 { return 1 })
	if s.Stats().Blocks == 0 {
		t.Fatal("no blocks")
	}
	files := func(ext string) int {
		m, _ := filepath.Glob(filepath.Join(o.Dir, "blocks", "*."+ext))
		return len(m)
	}
	nRaw, nAgg := files("raw"), files("agg")
	if nRaw == 0 || nRaw != nAgg {
		t.Fatalf("raw=%d agg=%d", nRaw, nAgg)
	}
	clk.Set(base + 2*day) // blocks are now >24h old, <72h
	if err := s.Seal(); err != nil {
		t.Fatal(err)
	}
	if files("raw") != 0 || files("agg") != nAgg {
		t.Fatalf("raw=%d agg=%d", files("raw"), files("agg"))
	}
	clk.Set(base + 10*day)
	if err := s.Seal(); err != nil {
		t.Fatal(err)
	}
	if files("agg") != 0 || s.Stats().Blocks != 0 {
		t.Fatalf("agg=%d blocks=%d", files("agg"), s.Stats().Blocks)
	}
	if s.Stats().ExpiredBefore == 0 {
		t.Fatal("expiredBefore not recorded")
	}
	// The floor survives a restart.
	exp := s.Stats().ExpiredBefore
	_ = s.Close()
	s2 := openT(t, o)
	if s2.Stats().ExpiredBefore != exp {
		t.Fatalf("expiredBefore lost: %d vs %d", s2.Stats().ExpiredBefore, exp)
	}
}

func TestRetentionByBytesDropsOldestWholeBlocks(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	l := MustLabels("m", "h", "a")
	r := func(ts int64) float64 { return float64(ts%977) * 1.37 }
	fill(t, s, l, base, base+13*hour, 5000, r)
	full := s.Stats()
	if full.Blocks < 5 {
		t.Fatalf("blocks=%d", full.Blocks)
	}
	_ = s.Close()
	o.MaxBytes = full.BlockBytes/2 + full.WALBytes
	s2 := openT(t, o)
	if err := s2.Seal(); err != nil {
		t.Fatal(err)
	}
	st := s2.Stats()
	if st.BlockBytes+st.WALBytes > o.MaxBytes || st.Blocks == 0 || st.Blocks >= full.Blocks {
		t.Fatalf("budget not enforced: %+v (was %+v)", st, full)
	}
	// Oldest dropped first: the first remaining block starts after base.
	s2.mu.RLock()
	first := s2.blocks[0].minT
	s2.mu.RUnlock()
	if first <= base {
		t.Fatalf("oldest block not dropped first: %d", first)
	}
	// Every remaining block file is intact (never partial).
	for _, b := range s2.blocks {
		if b.raw == nil || b.agg == nil {
			t.Fatal("partial block left behind")
		}
	}
	res, _ := s2.Query(context.Background(), Query{Selector: mustSel(t, "m"), Start: base, End: base + 13*hour - 5000, Step: 5 * min1, Authorizer: AllowAll})
	if len(res.Gaps) == 0 || res.Gaps[0].Reason != "expired by retention" {
		t.Fatalf("expired range not reported: %+v", res.Gaps)
	}
}

func TestDiskBudgetRejectsAppend(t *testing.T) {
	o, _ := testOpts(t, func(o *Options) { o.MaxBytes = 4096 })
	s := openT(t, o)
	l := MustLabels("m")
	var lastErr error
	for i := 0; i < 2000 && lastErr == nil; i++ {
		_, lastErr = s.Append([]Entry{e(l, base+int64(i)*1000, float64(i)*0.37)})
	}
	if !errors.Is(lastErr, ErrDiskBudget) {
		t.Fatalf("expected ErrDiskBudget, got %v", lastErr)
	}
}

func TestStatsAndClosed(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	appendOK(t, s, e(MustLabels("m"), base, 1))
	st := s.Stats()
	if st.ActiveSeries != 1 || st.Appended != 1 || st.WALBytes == 0 || st.MaxBytes != o.MaxBytes {
		t.Fatalf("%+v", st)
	}
	_ = s.Close()
	if _, err := s.Append([]Entry{e(MustLabels("m"), base+1, 1)}); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestOptionsValidate(t *testing.T) {
	o, _ := testOpts(t)
	o.Resolutions = []time.Duration{7 * time.Minute}
	if _, err := Open(o); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("got %v", err)
	}
	o, _ = testOpts(t)
	o.MaxSeries = 0
	if _, err := Open(o); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("got %v", err)
	}
}

func TestMissingAggRebuiltFromRaw(t *testing.T) {
	o, _ := testOpts(t)
	s, _ := Open(o)
	fill(t, s, MustLabels("m"), base, base+4*hour, min1, func(ts int64) float64 { return 2 })
	_ = s.Close()
	m, _ := filepath.Glob(filepath.Join(o.Dir, "blocks", "*.agg"))
	if len(m) == 0 {
		t.Fatal("no agg files")
	}
	_ = os.Remove(m[0])
	s2 := openT(t, o)
	if _, err := os.Stat(m[0]); err != nil {
		t.Fatalf("agg not rebuilt: %v", err)
	}
	_ = fmt.Sprint(s2)
}
