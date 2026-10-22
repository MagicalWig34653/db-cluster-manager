// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func benchOpts(b *testing.B, sync bool) Options {
	o := DefaultOptions(b.TempDir())
	o.SyncWAL = sync
	o.MaxFutureSkew = 1000 * 24 * time.Hour
	o.MaxHeadSamples = 50_000_000
	clk := &fakeClock{}
	clk.Set(base + 26*hour)
	o.Now = clk.Now // keep the 2023-based test data inside retention
	return o
}

func benchLabels(n int) []Labels {
	out := make([]Labels, n)
	for i := range out {
		out[i] = MustLabels("node_cpu_seconds_total", "host", fmt.Sprintf("h%03d", i%100), "cpu", fmt.Sprint(i/100), "mode", "user")
	}
	return out
}

// BenchmarkAppend measures batched ingestion of 1000 series x 1 sample per
// batch (one scrape of 1000 series). Reported ns/op is per sample.
func benchmarkAppend(b *testing.B, syncWAL bool) {
	s, err := Open(benchOpts(b, syncWAL))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ls := benchLabels(1000)
	batch := make([]Entry, len(ls))
	b.ResetTimer()
	ts := base
	for i := 0; i < b.N; i += len(ls) {
		for j := range ls {
			batch[j] = Entry{ls[j], Sample{ts, float64(i + j)}}
		}
		if _, err := s.Append(batch); err != nil {
			b.Fatal(err)
		}
		ts += 15000
	}
	b.StopTimer()
	b.ReportMetric(float64(s.Stats().BlockBytes+s.Stats().WALBytes)/float64(b.N), "disk-B/sample")
}

func BenchmarkAppendNoSync(b *testing.B) { benchmarkAppend(b, false) }
func BenchmarkAppendFsync(b *testing.B)  { benchmarkAppend(b, true) }

func benchStore(b *testing.B) *Store {
	s, err := Open(benchOpts(b, false))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	ls := benchLabels(100)
	batch := make([]Entry, 0, 100)
	for ts := base; ts < base+24*hour; ts += 15000 { // 1 day, 15s scrape, 100 series
		batch = batch[:0]
		for j, l := range ls {
			batch = append(batch, Entry{l, Sample{ts, float64(ts/1000) + float64(j)}})
		}
		if _, err := s.Append(batch); err != nil {
			b.Fatal(err)
		}
	}
	return s
}

// BenchmarkQueryRaw: 100 series, 24 h, avg over 5 min windows at 5 min steps
// from sealed raw blocks (576,000 samples decoded per query).
func BenchmarkQueryRaw(b *testing.B) {
	s := benchStore(b)
	sel, _ := ParseSelector("node_cpu_seconds_total")
	q := Query{Selector: sel, Start: base, End: base + 23*hour, Step: 5 * min1, Window: 5 * min1, Func: FuncAvg, Authorizer: AllowAll}
	res, err := s.Query(context.Background(), q)
	if err != nil || len(res.Series) != 100 || len(res.Gaps) != 0 {
		b.Fatalf("bad setup: err=%v series=%d gaps=%d", err, len(res.Series), len(res.Gaps))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Query(context.Background(), q); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkQueryRollup: the same query forced onto the 15 min rollup.
func BenchmarkQueryRollup(b *testing.B) {
	s := benchStore(b)
	sel, _ := ParseSelector("node_cpu_seconds_total")
	q := Query{Selector: sel, Start: base, End: base + 23*hour, Step: 15 * min1, Window: 15 * min1, Func: FuncAvg, Resolution: 15 * min1, Authorizer: AllowAll}
	if res, err := s.Query(context.Background(), q); err != nil || len(res.Series) != 100 || len(res.Gaps) != 0 {
		b.Fatalf("bad setup: err=%v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Query(context.Background(), q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeDecodeRegular(b *testing.B) {
	n := 1000
	ts := make([]int64, n)
	vs := make([]float64, n)
	for i := range ts {
		ts[i] = base + int64(i)*15000
		vs[i] = float64(i) * 1.5
	}
	b.ResetTimer()
	var size int
	for i := 0; i < b.N; i++ {
		te, fe := encodeTimes(ts), encodeFloats(vs)
		size = len(te) + len(fe)
		if _, err := decodeTimes(te, n); err != nil {
			b.Fatal(err)
		}
		if _, err := decodeFloats(fe, n); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(size)/float64(n), "B/sample")
}

func TestConcurrentAppendQuery(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	ls := []Labels{MustLabels("m", "i", "1"), MustLabels("m", "i", "2")}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for ts := base; ts < base+6*hour; ts += 30000 {
			if _, err := s.Append([]Entry{e(ls[0], ts, 1), e(ls[1], ts, 2)}); err != nil {
				t.Error(err)
				return
			}
		}
		close(stop)
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.Query(context.Background(), withAuth(Query{Selector: mustSel(t, "m"), Start: base, End: base + 6*hour, Step: 30 * min1, Func: FuncAvg})); err != nil {
				t.Error(err)
				return
			}
			_ = s.Stats()
		}
	}()
	wg.Wait()
}
