// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type hseries struct {
	id      uint64
	labels  Labels
	samples []Sample
}

type block struct {
	minT, maxT int64 // window [minT, maxT)
	raw, agg   *blockFile
	rawBad     bool // file present but unreadable
	aggBad     bool
	rawSize    int64
	aggSize    int64
}

func (b *block) bytes() int64 { return b.rawSize + b.aggSize }

type counters struct {
	appended, rejTooOld, rejTooNew, rejLimits, rejInvalid, rejOther atomic.Int64
	queries, queriesBusy, queriesFailed, querySamples               atomic.Int64
	walTruncated, archiveFetchFail, archivePut                      atomic.Int64
}

// Store is the metric store. It is safe for concurrent use.
type Store struct {
	opts Options
	dir  string

	mu           sync.RWMutex
	closed       bool
	wal          *os.File
	walSize      int64
	series       map[string]*hseries
	byID         map[uint64]*hseries
	nextID       uint64
	labelVals    map[string]map[string]int
	headSamples  int
	headMin      int64
	headMax      int64
	hasHead      bool
	sealedUpTo   int64
	blocks       []*block
	expiredUntil int64
	blockBytes   int64

	archIdx ArchiveIndex
	cache   *blockCache
	sem     chan struct{}
	c       counters
}

// Stats is a snapshot of the store's own state and counters.
type Stats struct {
	ActiveSeries, HeadSamples, Blocks   int
	BlockBytes, WALBytes, MaxBytes      int64
	Appended, RejectedTooOld            int64
	RejectedTooNew, RejectedLimits      int64
	RejectedInvalid, RejectedOther      int64
	Queries, QueriesBusy, QueriesFailed int64
	QuerySamplesLoaded                  int64
	WALTruncatedBytes                   int64
	ExpiredBefore                       int64
	ArchiveIndexBlocks                  int
	ArchiveCacheBytes                   int64
	ArchiveCacheItems                   int
	ArchiveCacheHits                    int64
	ArchiveCacheMisses                  int64
	ArchiveFetchFailures                int64
	ArchiveBlocksPut                    int64
}

const noTime = math.MinInt64

// Open opens or creates a store in opts.Dir and recovers from the WAL.
func Open(opts Options) (*Store, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	s := &Store{
		opts: opts, dir: opts.Dir,
		series: map[string]*hseries{}, byID: map[uint64]*hseries{}, labelVals: map[string]map[string]int{},
		nextID: 1, headMax: noTime, headMin: math.MaxInt64, sealedUpTo: noTime,
		cache: newBlockCache(max64(opts.ArchiveCacheBytes, 1)),
		sem:   make(chan struct{}, opts.MaxConcurrentQueries),
	}
	if err := os.MkdirAll(s.blocksDir(), 0o750); err != nil {
		return nil, err
	}
	if err := s.loadBlocks(); err != nil {
		return nil, err
	}
	s.loadMeta()
	if err := s.recoverWAL(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	err := s.sealLocked()
	s.mu.Unlock()
	if err != nil {
		_ = s.wal.Close()
		return nil, err
	}
	return s, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Store) blocksDir() string { return filepath.Join(s.dir, "blocks") }
func (s *Store) walPath() string   { return filepath.Join(s.dir, "head.wal") }
func (s *Store) blockPath(minT int64, ext string) string {
	return filepath.Join(s.blocksDir(), fmt.Sprintf("%020d.%s", minT, ext))
}

func (s *Store) loadMeta() {
	b, err := os.ReadFile(filepath.Join(s.dir, "meta.json"))
	if err != nil {
		return
	}
	var m struct {
		ExpiredUntil int64 `json:"expiredUntil"`
	}
	if json.Unmarshal(b, &m) == nil {
		s.expiredUntil = m.ExpiredUntil
	}
}

func (s *Store) saveMeta() error {
	b, _ := json.Marshal(struct {
		ExpiredUntil int64 `json:"expiredUntil"`
	}{s.expiredUntil})
	return writeFileAtomic(filepath.Join(s.dir, "meta.json"), b)
}

func (s *Store) loadBlocks() error {
	ents, err := os.ReadDir(s.blocksDir())
	if err != nil {
		return err
	}
	byMin := map[int64]*block{}
	blockDur := s.opts.BlockDuration.Milliseconds()
	for _, e := range ents {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(s.blocksDir(), name))
			continue
		}
		stem, ext, ok := strings.Cut(name, ".")
		if !ok || (ext != "raw" && ext != "agg") {
			continue
		}
		minT, err := strconv.ParseInt(stem, 10, 64)
		if err != nil {
			continue
		}
		b := byMin[minT]
		if b == nil {
			b = &block{minT: minT, maxT: minT + blockDur}
			byMin[minT] = b
		}
		p := filepath.Join(s.blocksDir(), name)
		info, err := e.Info()
		if err != nil {
			return err
		}
		bf, perr := openBlockPath(p)
		if perr == nil {
			b.maxT = bf.winMax
		}
		if ext == "raw" {
			b.rawSize, b.raw, b.rawBad = info.Size(), bf, perr != nil
		} else {
			b.aggSize, b.agg, b.aggBad = info.Size(), bf, perr != nil
		}
		s.blockBytes += info.Size()
	}
	for _, b := range byMin {
		if b.agg == nil && !b.aggBad && b.raw != nil {
			s.rebuildAgg(b) // crash between raw and agg write, or lost agg
		}
		s.blocks = append(s.blocks, b)
		if b.maxT > s.sealedUpTo {
			s.sealedUpTo = b.maxT
		}
	}
	sort.Slice(s.blocks, func(i, j int) bool { return s.blocks[i].minT < s.blocks[j].minT })
	return nil
}

// rebuildAgg regenerates the rollup file from the raw file.
func (s *Store) rebuildAgg(b *block) {
	r, closeFn, err := b.raw.reader()
	if err != nil {
		return
	}
	defer closeFn()
	var items []blobItem
	for _, e := range b.raw.entries {
		blob, err := readBlob(r, e)
		if err != nil {
			return
		}
		smp, err := decodeRawChunk(blob)
		if err != nil || len(smp) == 0 {
			return
		}
		items = append(items, blobItem{e.labels, encodeAggChunk(smp, s.resMs()), len(smp), smp[0].T, smp[len(smp)-1].T})
	}
	data := buildBlockBytes(b.minT, b.maxT, items)
	p := s.blockPath(b.minT, "agg")
	if writeFileAtomic(p, data) != nil {
		return
	}
	if bf, err := openBlockPath(p); err == nil {
		b.agg, b.aggSize = bf, int64(len(data))
		s.blockBytes += b.aggSize
	}
}

func (s *Store) resMs() []int64 {
	out := make([]int64, len(s.opts.Resolutions))
	for i, r := range s.opts.Resolutions {
		out[i] = r.Milliseconds()
	}
	return out
}

func (s *Store) recoverWAL() error {
	good, total, err := replayWAL(s.walPath(),
		func(id uint64, l Labels) error {
			if err := l.validate(&s.opts); err != nil {
				return err
			}
			if _, dup := s.byID[id]; dup {
				return errors.New("duplicate series id")
			}
			s.registerSeries(id, l)
			return nil
		},
		func(ss []walSample) error {
			for _, x := range ss {
				hs := s.byID[x.id]
				if hs == nil {
					return errors.New("unknown series id")
				}
				if x.s.T < s.sealedUpTo {
					continue
				}
				s.insertHead(hs, x.s)
			}
			return nil
		})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.walPath(), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	if good < total {
		if err := f.Truncate(good); err != nil {
			_ = f.Close()
			return err
		}
		s.c.walTruncated.Add(total - good)
	}
	if _, err := f.Seek(good, 0); err != nil {
		_ = f.Close()
		return err
	}
	s.wal, s.walSize = f, good
	return nil
}

func (s *Store) registerSeries(id uint64, l Labels) *hseries {
	hs := &hseries{id: id, labels: l}
	s.series[l.Key()] = hs
	s.byID[id] = hs
	for _, x := range l {
		m := s.labelVals[x.Name]
		if m == nil {
			m = map[string]int{}
			s.labelVals[x.Name] = m
		}
		m[x.Value]++
	}
	if id >= s.nextID {
		s.nextID = id + 1
	}
	return hs
}

func (s *Store) unregisterSeries(hs *hseries) {
	delete(s.series, hs.labels.Key())
	delete(s.byID, hs.id)
	for _, x := range hs.labels {
		m := s.labelVals[x.Name]
		if m[x.Value]--; m[x.Value] <= 0 {
			delete(m, x.Value)
		}
		if len(m) == 0 {
			delete(s.labelVals, x.Name)
		}
	}
}

// insertHead inserts keeping order. It returns false for an exact duplicate
// and ErrDuplicateSample for a conflicting value.
func (s *Store) insertHead(hs *hseries, x Sample) (bool, error) {
	ss := hs.samples
	i := len(ss)
	for i > 0 && ss[i-1].T > x.T {
		i--
	}
	if i > 0 && ss[i-1].T == x.T {
		if math.Float64bits(ss[i-1].V) == math.Float64bits(x.V) {
			return false, nil
		}
		return false, ErrDuplicateSample
	}
	ss = append(ss, Sample{})
	copy(ss[i+1:], ss[i:])
	ss[i] = x
	hs.samples = ss
	s.headSamples++
	if x.T < s.headMin {
		s.headMin = x.T
	}
	if !s.hasHead || x.T > s.headMax {
		s.headMax, s.hasHead = x.T, true
	}
	return true, nil
}

func (s *Store) totalBytesLocked() int64 { return s.blockBytes + s.walSize }

// Append ingests a batch. Valid entries are stored even if others are
// rejected; the returned error joins (at most eight) rejection reasons and can
// be inspected with errors.Is. The int is the number of stored samples. An
// error that is not a rejection (WAL write failure) means the stored samples
// are queryable but not durable.
func (s *Store) Append(entries []Entry) (int, error) {
	if len(entries) > s.opts.MaxBatch {
		return 0, fmt.Errorf("%w: %d entries, limit %d", ErrBatchTooLarge, len(entries), s.opts.MaxBatch)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	now := s.opts.Now()
	if s.totalBytesLocked() >= s.opts.MaxBytes {
		_ = s.enforceRetentionLocked(now)
		if s.totalBytesLocked() >= s.opts.MaxBytes {
			s.c.rejOther.Add(int64(len(entries)))
			return 0, fmt.Errorf("%w: %d of %d bytes used", ErrDiskBudget, s.totalBytesLocked(), s.opts.MaxBytes)
		}
	}
	var (
		errs    []error
		nerr    int
		walBuf  []byte
		samples []walSample
		ok      int
	)
	reject := func(c *atomic.Int64, l Labels, err error) {
		c.Add(1)
		nerr++
		if len(errs) < 8 {
			errs = append(errs, fmt.Errorf("%s: %w", l, err))
		}
	}
	maxFuture := now.Add(s.opts.MaxFutureSkew).UnixMilli()
	ooo := s.opts.OutOfOrderWindow.Milliseconds()
	for _, e := range entries {
		if err := e.Labels.validate(&s.opts); err != nil {
			reject(&s.c.rejInvalid, e.Labels, err)
			continue
		}
		t := e.Sample.T
		if t > maxFuture {
			reject(&s.c.rejTooNew, e.Labels, fmt.Errorf("%w: %d > %d", ErrTooNew, t, maxFuture))
			continue
		}
		if t < s.sealedUpTo || (s.hasHead && t < s.headMax-ooo) {
			reject(&s.c.rejTooOld, e.Labels, ErrTooOld)
			continue
		}
		if s.headSamples >= s.opts.MaxHeadSamples {
			reject(&s.c.rejLimits, e.Labels, ErrHeadFull)
			continue
		}
		key := e.Labels.Key()
		hs := s.series[key]
		if hs == nil {
			if err := s.checkNewSeries(e.Labels); err != nil {
				reject(&s.c.rejLimits, e.Labels, err)
				continue
			}
			hs = s.registerSeries(s.nextID, e.Labels)
			walBuf = seriesRecord(walBuf, hs.id, hs.labels)
		}
		added, err := s.insertHead(hs, e.Sample)
		if err != nil {
			reject(&s.c.rejOther, e.Labels, err)
			continue
		}
		if added {
			samples = append(samples, walSample{hs.id, e.Sample})
			ok++
		}
	}
	if nerr > len(errs) {
		errs = append(errs, fmt.Errorf("... and %d more rejections", nerr-len(errs)))
	}
	if len(samples) > 0 {
		walBuf = samplesRecord(walBuf, samples)
	}
	s.c.appended.Add(int64(ok))
	if len(walBuf) > 0 {
		if _, err := s.wal.Write(walBuf); err != nil {
			return ok, errors.Join(append(errs, fmt.Errorf("metrics: wal write: %w", err))...)
		}
		s.walSize += int64(len(walBuf))
		if s.opts.SyncWAL {
			if err := s.wal.Sync(); err != nil {
				return ok, errors.Join(append(errs, fmt.Errorf("metrics: wal sync: %w", err))...)
			}
		}
	}
	if err := s.sealLocked(); err != nil {
		errs = append(errs, err)
	}
	return ok, errors.Join(errs...)
}

func (s *Store) checkNewSeries(l Labels) error {
	if len(s.series) >= s.opts.MaxSeries {
		return fmt.Errorf("%w: %d active series", ErrMaxSeries, s.opts.MaxSeries)
	}
	for _, x := range l {
		m := s.labelVals[x.Name]
		if _, known := m[x.Value]; !known && len(m) >= s.opts.MaxValuesPerLabel {
			return fmt.Errorf("%w: label %q already has %d values", ErrMaxLabelValues, x.Name, len(m))
		}
	}
	return nil
}

func (s *Store) windowStart(t int64) int64 {
	d := s.opts.BlockDuration.Milliseconds()
	return t - mod(t, d)
}

// sealLocked seals every completed window: one whose end is at or before
// (newest sample - OutOfOrderWindow), so that no accepted sample can still
// land in it.
func (s *Store) sealLocked() error {
	d := s.opts.BlockDuration.Milliseconds()
	for s.headSamples > 0 && s.hasHead {
		ws := s.windowStart(s.headMin)
		we := ws + d
		if we > s.headMax-s.opts.OutOfOrderWindow.Milliseconds() {
			return nil
		}
		if err := s.sealWindowLocked(ws, we); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) sealWindowLocked(ws, we int64) error {
	var rawItems, aggItems []blobItem
	res := s.resMs()
	for _, hs := range s.series {
		n := sort.Search(len(hs.samples), func(i int) bool { return hs.samples[i].T >= we })
		if n == 0 {
			continue
		}
		part := hs.samples[:n]
		rawItems = append(rawItems, blobItem{hs.labels, encodeRawChunk(part), n, part[0].T, part[n-1].T})
		aggItems = append(aggItems, blobItem{hs.labels, encodeAggChunk(part, res), n, part[0].T, part[n-1].T})
	}
	b := &block{minT: ws, maxT: we}
	if len(rawItems) > 0 {
		aggData := buildBlockBytes(ws, we, aggItems)
		rawData := buildBlockBytes(ws, we, rawItems)
		// Raw first: a crash in between leaves a raw-only block, whose
		// rollups loadBlocks rebuilds.
		if err := writeFileAtomic(s.blockPath(ws, "raw"), rawData); err != nil {
			return fmt.Errorf("metrics: write raw block: %w", err)
		}
		if err := writeFileAtomic(s.blockPath(ws, "agg"), aggData); err != nil {
			return fmt.Errorf("metrics: write agg block: %w", err)
		}
		var err error
		if b.agg, err = openBlockPath(s.blockPath(ws, "agg")); err != nil {
			return err
		}
		if b.raw, err = openBlockPath(s.blockPath(ws, "raw")); err != nil {
			return err
		}
		b.aggSize, b.rawSize = int64(len(aggData)), int64(len(rawData))
		s.blockBytes += b.bytes()
		s.blocks = append(s.blocks, b)
		sort.Slice(s.blocks, func(i, j int) bool { return s.blocks[i].minT < s.blocks[j].minT })
	}
	s.sealedUpTo = we
	s.headMin = math.MaxInt64
	for k, hs := range s.series {
		n := sort.Search(len(hs.samples), func(i int) bool { return hs.samples[i].T >= we })
		s.headSamples -= n
		hs.samples = append([]Sample(nil), hs.samples[n:]...)
		if len(hs.samples) == 0 {
			s.unregisterSeries(hs)
			_ = k
			continue
		}
		if hs.samples[0].T < s.headMin {
			s.headMin = hs.samples[0].T
		}
	}
	if err := s.rewriteWALLocked(); err != nil {
		return err
	}
	return s.enforceRetentionLocked(s.opts.Now())
}

func (s *Store) rewriteWALLocked() error {
	var buf []byte
	ids := make([]uint64, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var batch []walSample
	flush := func() {
		if len(batch) > 0 {
			buf = samplesRecord(buf, batch)
			batch = batch[:0]
		}
	}
	for _, id := range ids {
		hs := s.byID[id]
		buf = seriesRecord(buf, id, hs.labels)
		for _, x := range hs.samples {
			batch = append(batch, walSample{id, x})
			if len(batch) >= 1000 {
				flush()
			}
		}
	}
	flush()
	if err := writeFileAtomic(s.walPath(), buf); err != nil {
		return fmt.Errorf("metrics: rewrite wal: %w", err)
	}
	_ = s.wal.Close()
	f, err := os.OpenFile(s.walPath(), os.O_RDWR, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, 2); err != nil {
		return err
	}
	s.wal, s.walSize = f, int64(len(buf))
	return nil
}

// Seal seals completed windows and applies retention. Safe to call often.
func (s *Store) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.sealLocked(); err != nil {
		return err
	}
	return s.enforceRetentionLocked(s.opts.Now())
}

func (s *Store) removeBlockFiles(b *block, raw, agg bool) {
	if raw && b.rawSize > 0 {
		_ = os.Remove(s.blockPath(b.minT, "raw"))
		s.blockBytes -= b.rawSize
		b.raw, b.rawSize, b.rawBad = nil, 0, false
	}
	if agg && b.aggSize > 0 {
		_ = os.Remove(s.blockPath(b.minT, "agg"))
		s.blockBytes -= b.aggSize
		b.agg, b.aggSize, b.aggBad = nil, 0, false
	}
}

// enforceRetentionLocked drops whole files, never part of one: raw files older
// than RawRetention, whole blocks older than AggRetention, then the oldest
// blocks while the disk budget is exceeded.
func (s *Store) enforceRetentionLocked(now time.Time) error {
	changed := false
	nowMs := now.UnixMilli()
	kept := s.blocks[:0:0]
	for _, b := range s.blocks {
		age := nowMs - b.maxT
		switch {
		case age > s.opts.AggRetention.Milliseconds():
			s.removeBlockFiles(b, true, true)
			s.expiredUntil, changed = max64(s.expiredUntil, b.maxT), true
			continue
		case age > s.opts.RawRetention.Milliseconds():
			s.removeBlockFiles(b, true, false)
		}
		kept = append(kept, b)
	}
	s.blocks = kept
	for s.totalBytesLocked() > s.opts.MaxBytes && len(s.blocks) > 0 {
		b := s.blocks[0]
		s.removeBlockFiles(b, true, true)
		s.expiredUntil, changed = max64(s.expiredUntil, b.maxT), true
		s.blocks = s.blocks[1:]
	}
	if changed {
		return s.saveMeta()
	}
	return nil
}

// Stats returns a snapshot of counters and sizes.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	st := Stats{
		ActiveSeries: len(s.series), HeadSamples: s.headSamples, Blocks: len(s.blocks),
		BlockBytes: s.blockBytes, WALBytes: s.walSize, MaxBytes: s.opts.MaxBytes, ExpiredBefore: s.expiredUntil,
	}
	s.mu.RUnlock()
	cs, ch, cm, cn := s.cache.stats()
	st.Appended, st.RejectedTooOld, st.RejectedTooNew = s.c.appended.Load(), s.c.rejTooOld.Load(), s.c.rejTooNew.Load()
	st.RejectedLimits, st.RejectedInvalid, st.RejectedOther = s.c.rejLimits.Load(), s.c.rejInvalid.Load(), s.c.rejOther.Load()
	st.Queries, st.QueriesBusy, st.QueriesFailed = s.c.queries.Load(), s.c.queriesBusy.Load(), s.c.queriesFailed.Load()
	st.QuerySamplesLoaded, st.WALTruncatedBytes = s.c.querySamples.Load(), s.c.walTruncated.Load()
	st.ArchiveIndexBlocks, st.ArchiveCacheBytes, st.ArchiveCacheItems = s.archIdx.Len(), cs, cn
	st.ArchiveCacheHits, st.ArchiveCacheMisses = ch, cm
	st.ArchiveFetchFailures, st.ArchiveBlocksPut = s.c.archiveFetchFail.Load(), s.c.archivePut.Load()
	return st
}

// Close flushes and closes the WAL. Unsealed samples stay in the WAL.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.wal.Sync(); err != nil {
		_ = s.wal.Close()
		return err
	}
	return s.wal.Close()
}

// RefreshArchive reloads the archive index from the configured Archive.
func (s *Store) RefreshArchive(ctx context.Context) error {
	if s.opts.Archive == nil {
		return nil
	}
	m, err := s.opts.Archive.ListRanges(ctx, math.MinInt64, math.MaxInt64)
	if err != nil {
		return err
	}
	s.archIdx.Replace(m)
	return nil
}

// ArchiveBlocks uploads the rollup file of every sealed block that is not yet
// in the archive index and returns how many were uploaded. Schedule it more
// often than AggRetention so that expiring data is archived first.
func (s *Store) ArchiveBlocks(ctx context.Context) (int, error) {
	if s.opts.Archive == nil {
		return 0, nil
	}
	s.mu.RLock()
	var todo []*block
	for _, b := range s.blocks {
		if b.agg != nil && !s.archIdx.Has(blockID(b.minT, b.maxT)) {
			todo = append(todo, b)
		}
	}
	s.mu.RUnlock()
	var errs []error
	n := 0
	for _, b := range todo {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		data, err := os.ReadFile(b.agg.path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		meta := BlockMeta{ID: blockID(b.minT, b.maxT), MinT: b.minT, MaxT: b.maxT, Size: int64(len(data))}
		if err := s.opts.Archive.PutBlock(ctx, meta, data); err != nil {
			errs = append(errs, fmt.Errorf("archive block %s: %w", meta.ID, err))
			continue
		}
		s.archIdx.Add(meta)
		s.c.archivePut.Add(1)
		n++
	}
	return n, errors.Join(errs...)
}

// Maintain seals, archives and applies retention, in that order.
func (s *Store) Maintain(ctx context.Context) error {
	err := s.Seal()
	_, aerr := s.ArchiveBlocks(ctx)
	s.mu.Lock()
	rerr := s.enforceRetentionLocked(s.opts.Now())
	s.mu.Unlock()
	return errors.Join(err, aerr, rerr)
}
