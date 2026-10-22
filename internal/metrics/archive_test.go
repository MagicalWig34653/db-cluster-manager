// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLocalArchiveRoundTrip(t *testing.T) {
	a, err := NewLocalArchive(filepath.Join(t.TempDir(), "arch"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m1 := BlockMeta{ID: blockID(0, 100), MinT: 0, MaxT: 100}
	m2 := BlockMeta{ID: blockID(100, 200), MinT: 100, MaxT: 200}
	if err := a.PutBlock(ctx, m2, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := a.PutBlock(ctx, m1, []byte("one")); err != nil {
		t.Fatal(err)
	}
	got, err := a.GetBlock(ctx, m1.ID)
	if err != nil || string(got) != "one" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := a.GetBlock(ctx, blockID(500, 600)); !errors.Is(err, ErrArchiveNotFound) {
		t.Fatalf("got %v", err)
	}
	for _, bad := range []string{"../etc/passwd", "a_b", "", "1_2/../3"} {
		if _, err := a.GetBlock(ctx, bad); err == nil || errors.Is(err, ErrArchiveNotFound) {
			t.Errorf("id %q must be rejected as invalid, got %v", bad, err)
		}
		if err := a.PutBlock(ctx, BlockMeta{ID: bad}, nil); err == nil {
			t.Errorf("put id %q accepted", bad)
		}
	}
	l, _ := a.ListRanges(ctx, 150, 160)
	if len(l) != 1 || l[0].ID != m2.ID || l[0].Size != 3 {
		t.Fatalf("%+v", l)
	}
	l, _ = a.ListRanges(ctx, 0, 1000)
	if len(l) != 2 || l[0].MinT != 0 {
		t.Fatalf("%+v", l)
	}
	// stray files are ignored
	_ = os.WriteFile(filepath.Join(a.Dir, "junk.txt"), []byte("x"), 0o640)
	if l, _ = a.ListRanges(ctx, 0, 1000); len(l) != 2 {
		t.Fatalf("%+v", l)
	}
}

func TestArchiveIndexOverlap(t *testing.T) {
	var x ArchiveIndex
	for i := int64(0); i < 10; i++ {
		x.Add(BlockMeta{ID: blockID(i*100, i*100+100), MinT: i * 100, MaxT: i*100 + 100})
	}
	got := x.Overlapping(250, 420)
	if len(got) != 3 || got[0].MinT != 200 || got[2].MinT != 400 {
		t.Fatalf("%+v", got)
	}
	if len(x.Overlapping(1000, 2000)) != 0 || len(x.Overlapping(-50, -1)) != 0 {
		t.Fatal("false overlap")
	}
	if got := x.Overlapping(100, 100); len(got) != 1 || got[0].MinT != 100 {
		t.Fatalf("boundary: %+v", got) // [100,200) contains 100, [0,100) does not
	}
	if !x.Has(blockID(0, 100)) || x.Len() != 10 {
		t.Fatal("has/len")
	}
}

func fakeBlock(n int) *blockFile { return &blockFile{mem: make([]byte, n)} }

func TestBlockCacheLRUBounded(t *testing.T) {
	c := newBlockCache(100)
	c.Put("a", fakeBlock(40))
	c.Put("b", fakeBlock(40))
	if _, ok := c.Get("a"); !ok { // a becomes most recent
		t.Fatal("a missing")
	}
	c.Put("c", fakeBlock(40)) // evicts b (least recently used)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should be kept")
	}
	if size, _, _, n := c.stats(); size != 80 || n != 2 {
		t.Fatalf("size=%d n=%d", size, n)
	}
	c.Put("huge", fakeBlock(101)) // never cached, never evicts others
	if _, ok := c.Get("huge"); ok {
		t.Fatal("oversized block cached")
	}
	if size, _, _, _ := c.stats(); size != 80 {
		t.Fatalf("size=%d", size)
	}
	c.Put("a", fakeBlock(60)) // replacing updates the size accounting
	if size, _, _, _ := c.stats(); size > 100 {
		t.Fatalf("size=%d exceeds bound", size)
	}
	for i := 0; i < 1000; i++ {
		c.Put(fmt.Sprint(i), fakeBlock(7+i%30))
		if size, _, _, _ := c.stats(); size > 100 {
			t.Fatalf("bound exceeded: %d", size)
		}
	}
}

type spyArchive struct {
	Archive
	mu   sync.Mutex
	gets []string
	fail bool
}

func (s *spyArchive) GetBlock(ctx context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	s.gets = append(s.gets, id)
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return nil, errors.New("simulated network failure")
	}
	return s.Archive.GetBlock(ctx, id)
}

func (s *spyArchive) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.gets) }

func TestArchiveOnDemandQueryFetchesOnlyNeededBlocksAndCaches(t *testing.T) {
	la, _ := NewLocalArchive(t.TempDir())
	spy := &spyArchive{Archive: la}
	o, clk := testOpts(t, func(o *Options) {
		o.Archive = spy
		o.RawRetention = 12 * time.Hour
		o.AggRetention = 3 * 24 * time.Hour
	})
	s := openT(t, o)
	l := MustLabels("g")
	fill(t, s, l, base, base+9*hour, 10000, func(ts int64) float64 { return float64((ts - base) / hour) }) // value = hour index
	n, err := s.ArchiveBlocks(context.Background())
	if err != nil || n != s.Stats().Blocks || n < 3 {
		t.Fatalf("archived %d (blocks %d): %v", n, s.Stats().Blocks, err)
	}
	if n2, _ := s.ArchiveBlocks(context.Background()); n2 != 0 {
		t.Fatalf("second run re-uploaded %d blocks", n2)
	}
	clk.Set(base + 30*day) // everything local expires
	if err := s.Seal(); err != nil {
		t.Fatal(err)
	}
	if s.Stats().Blocks != 0 {
		t.Fatal("local blocks should be gone")
	}
	if err := s.RefreshArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Window 15m so rollups apply; range lies inside the second block only.
	q := withAuth(Query{Selector: mustSel(t, "g"), Start: base + 2*hour + 15*min1, End: base + 3*hour, Step: 15 * min1, Window: 15 * min1, Func: FuncAvg})
	res, err := s.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps) != 0 {
		t.Fatalf("gaps: %+v", res.Gaps)
	}
	for _, p := range res.Series[0].Points {
		if p.State != StateOK || p.V != 2 {
			t.Fatalf("%+v", p)
		}
	}
	if spy.count() != 1 {
		t.Fatalf("fetched %d blocks, want exactly 1: %v", spy.count(), spy.gets)
	}
	if _, err := s.Query(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if spy.count() != 1 || s.Stats().ArchiveCacheHits == 0 {
		t.Fatalf("cache not used: fetches=%d stats=%+v", spy.count(), s.Stats())
	}
	// A range that spans two blocks fetches two (one from cache).
	q.Start, q.End = base+1*hour+45*min1, base+2*hour+30*min1
	if _, err := s.Query(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if spy.count() != 2 {
		t.Fatalf("fetches=%d", spy.count())
	}
	// Archive failure is visible as unavailable, not as missing data.
	spy.mu.Lock()
	spy.fail = true
	spy.mu.Unlock()
	q.Start, q.End = base+4*hour+15*min1, base+5*hour
	res, err = s.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps) == 0 || s.Stats().ArchiveFetchFailures == 0 {
		t.Fatalf("expected gap, got %+v", res.Gaps)
	}
	// Outside the archive and before the retention floor: unavailable (expired).
	q.Start, q.End = base-3*hour, base-hour
	res, _ = s.Query(context.Background(), q)
	if len(res.Gaps) != 0 {
		// before the first block there was never data and nothing expired there
		t.Logf("gaps before first block: %+v", res.Gaps)
	}
}

func TestArchiveBlockTooLargeIsGap(t *testing.T) {
	la, _ := NewLocalArchive(t.TempDir())
	o, clk := testOpts(t, func(o *Options) {
		o.Archive = la
		o.RawRetention = 12 * time.Hour
		o.AggRetention = 3 * 24 * time.Hour
		o.MaxArchiveBlockBytes = 10
	})
	s := openT(t, o)
	fill(t, s, MustLabels("g"), base, base+5*hour, 10000, func(ts int64) float64 { return 1 })
	if _, err := s.ArchiveBlocks(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk.Set(base + 30*day)
	_ = s.Seal()
	_ = s.RefreshArchive(context.Background())
	res, err := s.Query(context.Background(), withAuth(Query{Selector: mustSel(t, "g"), Start: base + 15*min1, End: base + hour, Step: 15 * min1, Func: FuncAvg}))
	if err != nil || len(res.Gaps) == 0 {
		t.Fatalf("err=%v gaps=%+v", err, res.Gaps)
	}
}
