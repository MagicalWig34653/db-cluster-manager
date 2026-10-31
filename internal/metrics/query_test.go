// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func q1(t *testing.T, s *Store, q Query) *Result {
	t.Helper()
	if q.Authorizer == nil {
		q.Authorizer = AllowAll
	}
	res, err := s.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9*math.Max(1, math.Abs(b)) }

func TestAggregationsOnRawHead(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	l := MustLabels("g")
	// values 1..10 at 10s spacing starting at base+10s
	for i := 1; i <= 10; i++ {
		appendOK(t, s, e(l, base+int64(i)*10000, float64(i)))
	}
	at := base + 100000
	cases := map[Func]float64{FuncAvg: 5.5, FuncMin: 1, FuncMax: 10, FuncSum: 55, FuncCount: 10, FuncLast: 10}
	for f, want := range cases {
		res := q1(t, s, Query{Selector: mustSel(t, "g"), Start: at, End: at, Step: 100000, Window: 100000, Func: f})
		if got := res.Series[0].Points[0]; got.State != StateOK || !near(got.V, want) {
			t.Errorf("%s: %+v want %v", f, got, want)
		}
	}
	res := q1(t, s, Query{Selector: mustSel(t, "g"), Start: at, End: at, Step: 100000, Window: 100000, Func: FuncQuantile, Quantile: 0.5})
	if !near(res.Series[0].Points[0].V, 5.5) {
		t.Errorf("median %v", res.Series[0].Points[0].V)
	}
	res = q1(t, s, Query{Selector: mustSel(t, "g"), Start: at, End: at, Step: 100000, Window: 100000, Func: FuncQuantile, Quantile: 0.9})
	if !near(res.Series[0].Points[0].V, 9.1) {
		t.Errorf("p90 %v", res.Series[0].Points[0].V)
	}
	// rate: 1 per 10s => 0.1/s
	res = q1(t, s, Query{Selector: mustSel(t, "g"), Start: at, End: at, Step: 100000, Window: 100000, Func: FuncRate})
	if !near(res.Series[0].Points[0].V, 0.1) {
		t.Errorf("rate %v", res.Series[0].Points[0].V)
	}
}

func TestRateHandlesCounterReset(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	l := MustLabels("c")
	vals := []float64{100, 110, 120, 5, 15, 25} // reset after 120
	for i, v := range vals {
		appendOK(t, s, e(l, base+int64(i)*10000, v))
	}
	res := q1(t, s, Query{Selector: mustSel(t, "c"), Start: base + 60000, End: base + 60000, Step: 60000, Window: 70000, Func: FuncRate})
	// increase = 10+10+5+10+10 = 45 over 50s
	if p := res.Series[0].Points[0]; !near(p.V, 0.9) {
		t.Fatalf("rate %+v", p)
	}
}

func TestStepAlignmentAndNoData(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	l := MustLabels("m")
	appendOK(t, s, e(l, base+10*min1, 1), e(l, base+40*min1, 2))
	res := q1(t, s, Query{Selector: mustSel(t, "m"), Start: base + 7*min1 + 123, End: base + 50*min1, Step: 10 * min1, Func: FuncLast})
	if res.Start != base+0*min1 || res.Start%(10*min1) != 0 {
		t.Fatalf("start not aligned: %d", res.Start)
	}
	var states []PointState
	for _, p := range res.Series[0].Points {
		states = append(states, p.State)
		if p.T%(10*min1) != 0 {
			t.Fatalf("unaligned point %d", p.T)
		}
	}
	// base, +10, +20, +30, +40, +50 -> ndata, ok, nodata, nodata, ok, nodata
	want := []PointState{StateNoData, StateOK, StateNoData, StateNoData, StateOK, StateNoData}
	if len(states) != len(want) {
		t.Fatalf("%v", states)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states %v want %v", states, want)
		}
	}
	if len(res.Gaps) != 0 {
		t.Fatalf("no-data must not be reported as unavailable: %+v", res.Gaps)
	}
}

func TestSelectorsInQuery(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	appendOK(t, s,
		e(MustLabels("cpu", "host", "a", "mode", "user"), base, 1),
		e(MustLabels("cpu", "host", "a", "mode", "idle"), base, 2),
		e(MustLabels("cpu", "host", "b", "mode", "user"), base, 3),
		e(MustLabels("mem", "host", "a"), base, 4))
	count := func(sel string) int {
		return len(q1(t, s, Query{Selector: mustSel(t, sel), Start: base, End: base, Step: min1}).Series)
	}
	if count(`cpu`) != 3 || count(`cpu{host="a"}`) != 2 || count(`cpu{mode!="idle"}`) != 2 ||
		count(`{host=~"a|b"}`) != 4 || count(`cpu{host!~"a"}`) != 1 {
		t.Fatal("selector counts wrong")
	}
}

// Rollups are used after raw data expired.
func TestRollupQueryAfterRawRetention(t *testing.T) {
	o, clk := testOpts(t, func(o *Options) { o.RawRetention = 12 * time.Hour; o.AggRetention = 90 * 24 * time.Hour })
	s := openT(t, o)
	l := MustLabels("g")
	fill(t, s, l, base, base+7*hour, 10000, func(ts int64) float64 { return float64((ts - base) / 1000) })
	clk.Set(base + 3*day)
	if err := s.Seal(); err != nil {
		t.Fatal(err)
	}
	if s.Stats().Blocks == 0 {
		t.Fatal("no blocks")
	}
	for _, b := range s.blocks {
		if b.raw != nil {
			t.Fatal("raw should have been dropped")
		}
	}
	// 15 minute window on a 15 minute step: exactly one 15m rollup bucket per step.
	q := Query{Selector: mustSel(t, "g"), Start: base + 15*min1, End: base + 90*min1, Step: 15 * min1, Window: 15 * min1}
	q.Func = FuncAvg
	res := q1(t, s, q)
	for _, p := range res.Series[0].Points {
		if p.State != StateOK {
			t.Fatalf("%+v", p)
		}
		// samples at seconds [t-15m, t) => avg of values at those seconds
		first := float64((p.T - 15*min1 - base) / 1000)
		want := first + (89.0*10)/2 // 90 samples, step 10s
		if !near(p.V, want) {
			t.Fatalf("avg at %d: got %v want %v", p.T, p.V, want)
		}
	}
	q.Func = FuncMax
	if v := q1(t, s, q).Series[0].Points[0].V; !near(v, float64((15*min1-10000)/1000)) {
		t.Fatalf("max %v", v)
	}
	q.Func = FuncCount
	if v := q1(t, s, q).Series[0].Points[1].V; v != 90 {
		t.Fatalf("count %v", v)
	}
	// Counter: value increases 10 per 10s => 1/s from rollup increments.
	q.Func = FuncRate
	for _, p := range q1(t, s, q).Series[0].Points[1:] {
		if math.Abs(p.V-1) > 0.02 {
			t.Fatalf("rollup rate %+v", p)
		}
	}
	// Quantile cannot be answered from rollups: explicit unavailable.
	q.Func, q.Quantile = FuncQuantile, 0.5
	res = q1(t, s, q)
	if len(res.Gaps) == 0 {
		t.Fatal("quantile on rolled-up range must report a gap")
	}
	for _, p := range res.Series {
		for _, pt := range p.Points {
			if pt.State == StateOK {
				t.Fatalf("unexpected ok point %+v", pt)
			}
		}
	}
	// A window shorter than the smallest rollup cannot use rollups either.
	q.Func, q.Window, q.Step = FuncAvg, 30000, 30000
	q.End = q.Start + 5*30000
	res = q1(t, s, q)
	if len(res.Gaps) == 0 {
		t.Fatal("expected gap for sub-resolution window")
	}
}

func TestForcedResolutionAndRates(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	l := MustLabels("g")
	fill(t, s, l, base, base+5*hour, 10000, func(ts int64) float64 { return 2 })
	q := Query{Selector: mustSel(t, "g"), Start: base + 30*min1, End: base + 90*min1, Step: hour, Window: hour, Func: FuncSum, Resolution: 15 * min1}
	forced := q1(t, s, q)
	q.Resolution = 0
	raw := q1(t, s, q)
	if forced.Series[0].Points[1].V != raw.Series[0].Points[1].V {
		t.Fatalf("rollup %v != raw %v", forced.Series[0].Points[1].V, raw.Series[0].Points[1].V)
	}
	q.Resolution, q.Func = 7*min1, FuncSum
	if _, err := s.Query(context.Background(), withAuth(q)); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("got %v", err)
	}
}

func withAuth(q Query) Query { q.Authorizer = AllowAll; return q }

func TestAuthorizerAppliedBeforeRead(t *testing.T) {
	o, _ := testOpts(t)
	s, _ := Open(o)
	a, b := MustLabels("m", "tenant", "a"), MustLabels("m", "tenant", "b")
	for ts := base; ts < base+5*hour; ts += 15000 {
		appendOK(t, s, e(a, ts, 1), e(b, ts, 1)) // out-of-order tolerance is store-wide
	}
	_ = s.Close()
	s = openT(t, o) // blocks plus a head tail
	only := func(l Labels) bool { return l.Get("tenant") == "a" }
	var asked []string
	spy := func(l Labels) bool { asked = append(asked, l.Get("tenant")); return only(l) }
	before := s.Stats().QuerySamplesLoaded
	res := q1(t, s, Query{Selector: mustSel(t, "m"), Start: base, End: base + 5*hour, Step: 30 * min1, Window: 30 * min1, Func: FuncCount, Authorizer: spy})
	if len(res.Series) != 1 || res.Series[0].Labels.Get("tenant") != "a" {
		t.Fatalf("series: %+v", res.Series)
	}
	loaded := s.Stats().QuerySamplesLoaded - before
	all := q1(t, s, Query{Selector: mustSel(t, "m"), Start: base, End: base + 5*hour, Step: 30 * min1, Window: 30 * min1, Func: FuncCount})
	loadedAll := s.Stats().QuerySamplesLoaded - before - loaded
	if len(all.Series) != 2 || loaded*2 != loadedAll {
		t.Fatalf("denied series were read: loaded=%d all=%d", loaded, loadedAll)
	}
	if len(asked) == 0 {
		t.Fatal("authorizer not consulted")
	}
	if _, err := s.Query(context.Background(), Query{Selector: mustSel(t, "m"), Start: base, End: base, Step: 1}); !errors.Is(err, ErrNoAuthorizer) {
		t.Fatalf("got %v", err)
	}
}

func TestQueryLimits(t *testing.T) {
	o, _ := testOpts(t, func(o *Options) { o.MaxQuerySamples = 100; o.MaxQuerySteps = 10; o.MaxConcurrentQueries = 1 })
	s, _ := Open(o)
	l := MustLabels("m")
	fill(t, s, l, base, base+5*hour, 15000, func(ts int64) float64 { return 1 })
	_ = s.Close()
	s = openT(t, o)
	q := withAuth(Query{Selector: mustSel(t, "m"), Start: base, End: base + 5*hour, Step: hour, Func: FuncLast})
	if _, err := s.Query(context.Background(), q); !errors.Is(err, ErrTooManySamples) {
		t.Fatalf("samples: %v", err)
	}
	q.Step = min1
	if _, err := s.Query(context.Background(), q); !errors.Is(err, ErrTooManySteps) {
		t.Fatalf("steps: %v", err)
	}
	s.sem <- struct{}{}
	q.Step = hour
	if _, err := s.Query(context.Background(), q); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	<-s.sem
	st := s.Stats()
	if st.QueriesBusy != 1 || st.QueriesFailed != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestQueryTimeout(t *testing.T) {
	o, _ := testOpts(t, func(o *Options) { o.QueryTimeout = time.Nanosecond })
	s := openT(t, o)
	appendOK(t, s, e(MustLabels("m"), base, 1))
	time.Sleep(time.Millisecond)
	_, err := s.Query(context.Background(), withAuth(Query{Selector: mustSel(t, "m"), Start: base, End: base + 10*hour, Step: min1, Func: FuncLast}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestInvalidQueries(t *testing.T) {
	o, _ := testOpts(t)
	s := openT(t, o)
	sel := mustSel(t, "m")
	for _, q := range []Query{
		{Selector: nil, Start: 0, End: 1, Step: 1},
		{Selector: sel, Start: 0, End: 1, Step: 0},
		{Selector: sel, Start: 5, End: 1, Step: 1},
		{Selector: sel, Start: 0, End: 1, Step: 1, Func: "bogus"},
		{Selector: sel, Start: 0, End: 1, Step: 1, Func: FuncQuantile, Quantile: 2},
	} {
		if _, err := s.Query(context.Background(), withAuth(q)); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%+v: got %v", q, err)
		}
	}
}
