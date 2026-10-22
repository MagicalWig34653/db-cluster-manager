// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// Func is the per-step aggregation over the window (t-Window, t].
type Func string

const (
	FuncLast     Func = "last"
	FuncAvg      Func = "avg"
	FuncMin      Func = "min"
	FuncMax      Func = "max"
	FuncSum      Func = "sum"
	FuncCount    Func = "count"
	FuncRate     Func = "rate" // per second, counter resets handled
	FuncQuantile Func = "quantile"
)

// ParseFunc validates a function name.
func ParseFunc(s string) (Func, error) {
	switch f := Func(s); f {
	case FuncLast, FuncAvg, FuncMin, FuncMax, FuncSum, FuncCount, FuncRate, FuncQuantile:
		return f, nil
	}
	return "", fmt.Errorf("%w: unknown function %q", ErrInvalidQuery, s)
}

// PointState says why a step has (or has not) a value.
type PointState uint8

const (
	// StateOK: V is valid.
	StateOK PointState = iota
	// StateNoData: the interval was readable but contains no samples.
	StateNoData
	// StateUnavailable: data may exist but cannot be read (expired by
	// retention, corrupt block, archive unreachable, or rollups cannot answer
	// the function). V is NaN.
	StateUnavailable
)

func (s PointState) String() string {
	return [...]string{"ok", "no_data", "unavailable"}[s]
}

// Point is one result step.
type Point struct {
	T     int64
	V     float64
	State PointState
}

// SeriesResult is the result for one series.
type SeriesResult struct {
	Labels Labels
	Points []Point
}

// Gap is a time range [From, To] (ms) that could not be read.
type Gap struct {
	From, To int64
	Reason   string
}

// Result of a range query. Every series has exactly one Point per step; Gaps
// lists the unavailable intervals independent of series.
type Result struct {
	Start, End, Step, Window int64
	Series                   []SeriesResult
	Gaps                     []Gap
}

// Query is a range query. Times are milliseconds. Start is aligned down to a
// multiple of Step so that repeated dashboard refreshes hit the same steps.
type Query struct {
	Selector Selector
	Start    int64
	End      int64
	Step     int64
	Window   int64 // default: Step
	Func     Func  // default: last
	Quantile float64
	// Resolution forces a rollup resolution (ms) wherever rollups exist;
	// 0 uses raw samples when available. Quantile cannot use rollups.
	Resolution int64
	// Authorizer is mandatory. It is applied to every candidate series before
	// any of its samples are read.
	Authorizer Authorizer
}

type slotKind int

const (
	slotEmpty slotKind = iota
	slotRaw
	slotAgg
	slotUnavail
)

type slot struct {
	start, end int64
	kind       slotKind
	reason     string
	blk        *block
	remote     *BlockMeta
	head       bool
}

type acc struct {
	labels  Labels
	samples []Sample
	buckets []Bucket
}

type snapshot struct {
	blocks     map[int64]*block
	sealedUpTo int64
	expired    int64
	head       []acc
}

// Query runs a range query. It enforces the sample, step, concurrency and time
// limits of the store options.
func (s *Store) Query(ctx context.Context, q Query) (*Result, error) {
	if q.Authorizer == nil {
		return nil, ErrNoAuthorizer
	}
	if len(q.Selector) == 0 || q.Step <= 0 || q.End < q.Start {
		return nil, fmt.Errorf("%w: selector, positive step and end >= start are required", ErrInvalidQuery)
	}
	if q.Func == "" {
		q.Func = FuncLast
	}
	if _, err := ParseFunc(string(q.Func)); err != nil {
		return nil, err
	}
	if q.Func == FuncQuantile && (q.Quantile < 0 || q.Quantile > 1 || math.IsNaN(q.Quantile)) {
		return nil, fmt.Errorf("%w: quantile must be in [0,1]", ErrInvalidQuery)
	}
	if q.Window <= 0 {
		q.Window = q.Step
	}
	q.Start -= mod(q.Start, q.Step)
	steps := (q.End-q.Start)/q.Step + 1
	if steps > int64(s.opts.MaxQuerySteps) {
		return nil, fmt.Errorf("%w: %d steps, limit %d", ErrTooManySteps, steps, s.opts.MaxQuerySteps)
	}
	resolutions := s.resMs()
	aggRes := q.Resolution
	if aggRes != 0 {
		found := false
		for _, r := range resolutions {
			found = found || r == aggRes
		}
		if !found || q.Func == FuncQuantile {
			return nil, fmt.Errorf("%w: resolution %dms not configured or not usable for %s", ErrInvalidQuery, aggRes, q.Func)
		}
	} else if q.Func != FuncQuantile {
		for _, r := range resolutions {
			if r <= q.Window {
				aggRes = r
			}
		}
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.c.queriesBusy.Add(1)
		return nil, ErrBusy
	}
	s.c.queries.Add(1)
	ctx, cancel := context.WithTimeout(ctx, s.opts.QueryTimeout)
	defer cancel()
	res, err := s.runQuery(ctx, q, aggRes, steps)
	if err != nil {
		s.c.queriesFailed.Add(1)
	}
	return res, err
}

func (s *Store) runQuery(ctx context.Context, q Query, aggRes, steps int64) (*Result, error) {
	lo0 := q.Start - q.Window + 1
	hi0 := q.End
	budget := s.opts.MaxQuerySamples
	allowed := map[string]bool{}
	authorize := func(l Labels) bool {
		k := l.Key()
		v, ok := allowed[k]
		if !ok {
			v = q.Selector.Matches(l) && q.Authorizer(l)
			allowed[k] = v
		}
		return v
	}

	// Snapshot under the read lock; the authorizer runs here, before any
	// sample is copied.
	s.mu.RLock()
	snap := snapshot{blocks: make(map[int64]*block, len(s.blocks)), sealedUpTo: s.sealedUpTo, expired: s.expiredUntil}
	for _, b := range s.blocks {
		snap.blocks[b.minT] = b
	}
	for _, hs := range s.series {
		if !authorize(hs.labels) {
			continue
		}
		i := sort.Search(len(hs.samples), func(i int) bool { return hs.samples[i].T >= lo0 })
		j := sort.Search(len(hs.samples), func(i int) bool { return hs.samples[i].T > hi0 })
		if i >= j {
			continue
		}
		budget -= j - i
		if budget < 0 {
			s.mu.RUnlock()
			return nil, fmt.Errorf("%w: limit %d", ErrTooManySamples, s.opts.MaxQuerySamples)
		}
		snap.head = append(snap.head, acc{labels: hs.labels, samples: append([]Sample(nil), hs.samples[i:j]...)})
	}
	s.mu.RUnlock()

	// Classify time slots.
	d := s.opts.BlockDuration.Milliseconds()
	var slots []*slot
	sealed := snap.sealedUpTo
	for st := lo0 - mod(lo0, d); st <= hi0; st += d {
		sl := &slot{start: st, end: st + d}
		b := snap.blocks[st]
		var remote *BlockMeta
		if s.opts.Archive != nil {
			for _, m := range s.archIdx.Overlapping(st, st) {
				if m.MinT == st {
					m := m
					remote = &m
				}
			}
		}
		switch {
		case st < snap.expired && b == nil && remote == nil:
			sl.kind, sl.reason = slotUnavail, "expired by retention"
		case sealed != noTime && st >= sealed && b == nil:
			sl.kind, sl.head = slotRaw, true
		case b != nil:
			sl.blk = b
			switch {
			case b.raw != nil && !(q.Resolution > 0 && b.agg != nil):
				sl.kind = slotRaw
			case b.agg != nil:
				sl.kind = slotAgg
			case remote != nil:
				sl.kind, sl.remote = slotAgg, remote
			default:
				sl.kind, sl.reason = slotUnavail, "block unreadable"
			}
		case remote != nil:
			sl.kind, sl.remote = slotAgg, remote
		case sealed == noTime:
			sl.kind, sl.head = slotRaw, true
		}
		if sl.kind == slotAgg && aggRes == 0 {
			sl.kind, sl.reason = slotUnavail, "raw data expired and rollups cannot answer this query"
		}
		slots = append(slots, sl)
	}

	accs := map[string]*acc{}
	get := func(l Labels) *acc {
		k := l.Key()
		a := accs[k]
		if a == nil {
			a = &acc{labels: l}
			accs[k] = a
		}
		return a
	}
	for _, sl := range slots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch sl.kind {
		case slotRaw:
			if sl.blk != nil {
				if err := s.loadRaw(ctx, sl, lo0, hi0, authorize, get, &budget); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					if isLimit(err) {
						return nil, err
					}
					rawErr := err
					sl.kind = slotUnavail
					sl.reason = "raw block unreadable: " + rawErr.Error()
					if sl.blk.agg != nil && aggRes > 0 {
						// Fall back to the rollups of the same block.
						sl.kind, sl.reason = slotAgg, ""
						if err := s.loadAgg(ctx, sl, aggRes, lo0, hi0, authorize, get, &budget); err != nil {
							if isLimit(err) {
								return nil, err
							}
							sl.kind, sl.reason = slotUnavail, "block unreadable: "+err.Error()
						}
					}
				}
			}
		case slotAgg:
			if err := s.loadAgg(ctx, sl, aggRes, lo0, hi0, authorize, get, &budget); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if isLimit(err) {
					return nil, err
				}
				sl.kind, sl.reason = slotUnavail, "rollup block unavailable: "+err.Error()
			}
		}
	}
	for _, h := range snap.head {
		a := get(h.labels)
		a.samples = append(a.samples, h.samples...)
	}
	s.c.querySamples.Add(int64(s.opts.MaxQuerySamples - budget))

	res := &Result{Start: q.Start, End: q.End, Step: q.Step, Window: q.Window}
	// Per-step availability, shared by all series.
	stepBad := make([]bool, steps)
	for i := int64(0); i < steps; i++ {
		t := q.Start + i*q.Step
		for _, sl := range slots {
			if sl.kind == slotUnavail && sl.end > t-q.Window+1 && sl.start <= t {
				stepBad[i] = true
				break
			}
		}
	}
	for _, sl := range slots {
		if sl.kind != slotUnavail {
			continue
		}
		g := Gap{From: max64(sl.start, lo0), To: min64(sl.end-1, hi0), Reason: sl.reason}
		if n := len(res.Gaps); n > 0 && res.Gaps[n-1].To+1 == g.From && res.Gaps[n-1].Reason == g.Reason {
			res.Gaps[n-1].To = g.To
		} else {
			res.Gaps = append(res.Gaps, g)
		}
	}
	keys := make([]string, 0, len(accs))
	for k := range accs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := accs[k]
		sr := SeriesResult{Labels: a.labels, Points: make([]Point, steps)}
		any := false
		for i := int64(0); i < steps; i++ {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			t := q.Start + i*q.Step
			p := Point{T: t, V: math.NaN()}
			if stepBad[i] {
				p.State = StateUnavailable
			} else if v, ok := evalStep(a, q, t-q.Window, t, aggRes); ok {
				p.V, p.State = v, StateOK
				any = true
			} else {
				p.State = StateNoData
			}
			sr.Points[i] = p
		}
		if any {
			res.Series = append(res.Series, sr)
		}
	}
	return res, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

type limitErr struct{ error }

func (l limitErr) Unwrap() error { return l.error }

func isLimit(err error) bool { _, ok := err.(limitErr); return ok }

func (s *Store) loadRaw(ctx context.Context, sl *slot, lo, hi int64, authorize func(Labels) bool, get func(Labels) *acc, budget *int) error {
	bf := sl.blk.raw
	r, closeFn, err := bf.reader()
	if err != nil {
		return err
	}
	defer closeFn()
	type part struct {
		l Labels
		s []Sample
	}
	var parts []part
	for _, e := range bf.entries {
		if e.maxT < lo || e.minT > hi || !authorize(e.labels) {
			continue
		}
		if *budget -= e.count; *budget < 0 {
			return limitErr{fmt.Errorf("%w: limit %d", ErrTooManySamples, s.opts.MaxQuerySamples)}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		blob, err := readBlob(r, e)
		if err != nil {
			return err
		}
		smp, err := decodeRawChunk(blob)
		if err != nil {
			return err
		}
		p := part{l: e.labels}
		for _, x := range smp {
			if x.T >= lo && x.T <= hi {
				p.s = append(p.s, x)
			}
		}
		parts = append(parts, p)
	}
	// Commit only after the whole block read cleanly, so that a fallback to
	// rollups never mixes partial raw data with buckets.
	for _, p := range parts {
		a := get(p.l)
		a.samples = append(a.samples, p.s...)
	}
	return nil
}

func (s *Store) loadAgg(ctx context.Context, sl *slot, res, lo, hi int64, authorize func(Labels) bool, get func(Labels) *acc, budget *int) error {
	var bf *blockFile
	if sl.remote != nil {
		var err error
		if bf, err = s.fetchRemote(ctx, *sl.remote); err != nil {
			s.c.archiveFetchFail.Add(1)
			return err
		}
	} else {
		bf = sl.blk.agg
	}
	r, closeFn, err := bf.reader()
	if err != nil {
		return err
	}
	defer closeFn()
	for _, e := range bf.entries {
		if e.maxT < lo || e.minT > hi || !authorize(e.labels) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		blob, err := readBlob(r, e)
		if err != nil {
			return err
		}
		bs, err := decodeAggChunk(blob, res)
		if err != nil {
			return err
		}
		if *budget -= len(bs); *budget < 0 {
			return limitErr{fmt.Errorf("%w: limit %d", ErrTooManySamples, s.opts.MaxQuerySamples)}
		}
		a := get(e.labels)
		for _, b := range bs {
			if b.T+res > lo && b.T <= hi {
				a.buckets = append(a.buckets, b)
			}
		}
	}
	return nil
}

func (s *Store) fetchRemote(ctx context.Context, m BlockMeta) (*blockFile, error) {
	if bf, ok := s.cache.Get(m.ID); ok {
		return bf, nil
	}
	if m.Size > s.opts.MaxArchiveBlockBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrArchiveBlockLarge, m.Size)
	}
	data, err := s.opts.Archive.GetBlock(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.opts.MaxArchiveBlockBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrArchiveBlockLarge, len(data))
	}
	bf, err := parseBlockBytes(data)
	if err != nil {
		return nil, err
	}
	s.cache.Put(m.ID, bf)
	return bf, nil
}

// evalStep evaluates the window (lo, hi]. ok is false when there is no data.
func evalStep(a *acc, q Query, lo, hi, res int64) (float64, bool) {
	i := sort.Search(len(a.samples), func(i int) bool { return a.samples[i].T > lo })
	j := sort.Search(len(a.samples), func(i int) bool { return a.samples[i].T > hi })
	smp := a.samples[i:j]
	var bk []Bucket
	if res > 0 && len(a.buckets) > 0 {
		bi := sort.Search(len(a.buckets), func(i int) bool { return a.buckets[i].T >= lo })
		for k := bi; k < len(a.buckets) && a.buckets[k].T+res <= hi+1; k++ {
			bk = append(bk, a.buckets[k])
		}
	}
	if len(smp) == 0 && len(bk) == 0 {
		return 0, false
	}
	var count, sum float64
	mn, mx := math.Inf(1), math.Inf(-1)
	var last float64
	lastT := int64(math.MinInt64)
	for _, b := range bk {
		count += b.Count
		sum += b.Sum
		mn, mx = math.Min(mn, b.Min), math.Max(mx, b.Max)
		last, lastT = b.Last, b.T
	}
	for _, x := range smp {
		count++
		sum += x.V
		mn, mx = math.Min(mn, x.V), math.Max(mx, x.V)
	}
	if len(smp) > 0 && smp[len(smp)-1].T >= lastT {
		last = smp[len(smp)-1].V
	}
	switch q.Func {
	case FuncLast:
		return last, true
	case FuncAvg:
		return sum / count, true
	case FuncMin:
		return mn, true
	case FuncMax:
		return mx, true
	case FuncSum:
		return sum, true
	case FuncCount:
		return count, true
	case FuncRate:
		var inc float64
		for _, b := range bk {
			inc += b.Inc
		}
		if len(bk) > 0 {
			// Rollup based: increase over the covered window.
			for k := 1; k < len(smp); k++ {
				inc += counterStep(smp[k-1].V, smp[k].V)
			}
			return inc / (float64(q.Window) / 1000), true
		}
		if len(smp) < 2 {
			return 0, false
		}
		for k := 1; k < len(smp); k++ {
			inc += counterStep(smp[k-1].V, smp[k].V)
		}
		return inc / (float64(smp[len(smp)-1].T-smp[0].T) / 1000), true
	case FuncQuantile:
		v := make([]float64, len(smp))
		for k, x := range smp {
			v[k] = x.V
		}
		return quantile(q.Quantile, v), true
	}
	return 0, false
}

func counterStep(prev, cur float64) float64 {
	if cur >= prev {
		return cur - prev
	}
	return cur // counter reset: the new value counts from zero
}

func quantile(phi float64, v []float64) float64 {
	sort.Float64s(v)
	if len(v) == 1 {
		return v[0]
	}
	rank := phi * float64(len(v)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	return v[lo] + (v[hi]-v[lo])*(rank-float64(lo))
}

// Window helpers for callers that think in durations.
func Millis(d time.Duration) int64 { return d.Milliseconds() }
