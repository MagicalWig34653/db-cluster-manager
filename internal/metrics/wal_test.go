// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func headCount(t *testing.T, s *Store) int {
	t.Helper()
	return s.Stats().HeadSamples
}

func walFile(o Options) string { return filepath.Join(o.Dir, "head.wal") }

func TestWALRecoversToLastGoodRecordAfterTruncation(t *testing.T) {
	const n = 10
	for cut := 1; cut <= 40; cut += 3 {
		o, _ := testOpts(t)
		s, _ := Open(o)
		l := MustLabels("m")
		var sizes []int64
		for i := 0; i < n; i++ {
			appendOK(t, s, e(l, base+int64(i)*1000, float64(i)))
			sizes = append(sizes, s.Stats().WALBytes)
		}
		_ = s.Close()
		// Truncate somewhere inside record 7 (index 6).
		target := sizes[6] - int64(cut)
		if target <= sizes[5] {
			continue
		}
		if err := os.Truncate(walFile(o), target); err != nil {
			t.Fatal(err)
		}
		s2 := openT(t, o)
		if got := headCount(t, s2); got != 6 {
			t.Fatalf("cut=%d: recovered %d samples, want 6", cut, got)
		}
		if s2.Stats().WALTruncatedBytes != target-sizes[5] {
			t.Fatalf("cut=%d: truncated bytes %d", cut, s2.Stats().WALTruncatedBytes)
		}
		// New appends after recovery are durable and the file is clean.
		appendOK(t, s2, e(l, base+100000, 99))
		_ = s2.Close()
		s3 := openT(t, o)
		if got := headCount(t, s3); got != 7 || s3.Stats().WALTruncatedBytes != 0 {
			t.Fatalf("cut=%d: after second restart %d samples, stats %+v", cut, got, s3.Stats())
		}
	}
}

func TestWALRecoversFromCorruptRecordAndGarbageTail(t *testing.T) {
	o, _ := testOpts(t)
	s, _ := Open(o)
	l := MustLabels("m")
	var sizes []int64
	for i := 0; i < 8; i++ {
		appendOK(t, s, e(l, base+int64(i)*1000, float64(i)))
		sizes = append(sizes, s.Stats().WALBytes)
	}
	_ = s.Close()
	b, _ := os.ReadFile(walFile(o))
	b[sizes[4]+12] ^= 0xff // inside record 6 (index 5)
	if err := os.WriteFile(walFile(o), b, 0o640); err != nil {
		t.Fatal(err)
	}
	s2 := openT(t, o)
	if got := headCount(t, s2); got != 5 {
		t.Fatalf("recovered %d, want 5", got)
	}
	_ = s2.Close()

	// Garbage appended after a clean file is cut off as well.
	f, _ := os.OpenFile(walFile(o), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write([]byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9})
	_ = f.Close()
	s3 := openT(t, o)
	if got := headCount(t, s3); got != 5 || s3.Stats().WALTruncatedBytes != 11 {
		t.Fatalf("got %d, stats %+v", got, s3.Stats())
	}
}

func TestCrashBetweenBlockWriteAndWALRewrite(t *testing.T) {
	o, _ := testOpts(t)
	s, _ := Open(o)
	l := MustLabels("m")
	fill(t, s, l, base, base+2*hour+5*min1-1000, 60000, func(ts int64) float64 { return 1 })
	if s.Stats().Blocks != 0 {
		t.Fatal("sealed too early")
	}
	old, _ := os.ReadFile(walFile(o))
	appendOK(t, s, e(l, base+2*hour+5*min1, 1)) // seals window 0 and rewrites the WAL
	if s.Stats().Blocks != 1 {
		t.Fatal("expected a sealed block")
	}
	_ = s.Close()
	// Simulate the crash: block files exist, but the old WAL is still there.
	if err := os.WriteFile(walFile(o), old, 0o640); err != nil {
		t.Fatal(err)
	}
	s2 := openT(t, o)
	if s2.Stats().Blocks != 1 || s2.Stats().HeadSamples != 5 /* only 2h..2h5m, not the sealed window */ {
		t.Fatalf("replay must skip sealed samples: %+v", s2.Stats())
	}
	res, err := s2.Query(context.Background(), Query{Selector: mustSel(t, "m"), Start: base, End: base + 2*hour - 1, Step: 10 * min1, Func: FuncCount, Window: 10 * min1, Authorizer: AllowAll})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range res.Series[0].Points {
		if i == 0 {
			continue // window starts before the first sample
		}
		if p.State != StateOK || p.V != 10 {
			t.Fatalf("point %+v: duplicated or missing samples after crash recovery", p)
		}
	}
}

func TestCorruptBlockChunkFallsBackToRollupsAndReportsGap(t *testing.T) {
	o, _ := testOpts(t)
	s, _ := Open(o)
	l := MustLabels("m")
	fill(t, s, l, base, base+5*hour, 15000, func(ts int64) float64 { return 3 })
	_ = s.Close()
	rawFiles, _ := filepath.Glob(filepath.Join(o.Dir, "blocks", "*.raw"))
	b, _ := os.ReadFile(rawFiles[0])
	b[len(blockMagic)+2] ^= 0xff // inside the first chunk
	_ = os.WriteFile(rawFiles[0], b, 0o640)
	s2 := openT(t, o)
	res, err := s2.Query(context.Background(), Query{Selector: mustSel(t, "m"), Start: base, End: base + 2*hour - 1, Step: 15 * min1, Window: 15 * min1, Func: FuncAvg, Authorizer: AllowAll})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range res.Series[0].Points {
		if i == 0 {
			continue // window before the first sample
		}
		if p.State != StateOK || p.V != 3 {
			t.Fatalf("rollup fallback failed: %+v", p)
		}
	}
	// A quantile needs raw samples: the corrupt range is explicitly unavailable.
	res, err = s2.Query(context.Background(), Query{Selector: mustSel(t, "m"), Start: base, End: base + 3*hour, Step: 15 * min1, Window: 15 * min1, Func: FuncQuantile, Quantile: 0.5, Authorizer: AllowAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps) != 1 || res.Gaps[0].From > base+1 {
		t.Fatalf("gaps: %+v", res.Gaps)
	}
	var unavailable, ok int
	for _, p := range res.Series[0].Points {
		switch p.State {
		case StateUnavailable:
			unavailable++
		case StateOK:
			ok++
		}
	}
	if unavailable == 0 || ok == 0 {
		t.Fatalf("unavailable=%d ok=%d", unavailable, ok)
	}
}
