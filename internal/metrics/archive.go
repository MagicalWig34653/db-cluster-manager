// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"container/list"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// BlockMeta describes one archived block. The archive unit is the aggregated
// (rollup) file of a block; ID is derived from the block window.
type BlockMeta struct {
	ID         string
	MinT, MaxT int64 // block window [MinT, MaxT) in ms
	Size       int64
}

// Archive is the long-term tier (U05). Implementations for S3 and SFTP belong
// to the backup backends; this package only defines the contract and a local
// directory implementation. Implementations must be safe for concurrent use,
// make PutBlock atomic (no partial object visible) and idempotent.
type Archive interface {
	PutBlock(ctx context.Context, meta BlockMeta, data []byte) error
	GetBlock(ctx context.Context, id string) ([]byte, error) // ErrArchiveNotFound if absent
	// ListRanges returns blocks overlapping [minT, maxT] sorted by MinT.
	ListRanges(ctx context.Context, minT, maxT int64) ([]BlockMeta, error)
}

var idRE = regexp.MustCompile(`^[0-9]{1,20}_[0-9]{1,20}$`)

func blockID(minT, maxT int64) string { return fmt.Sprintf("%020d_%020d", minT, maxT) }

func parseBlockID(id string) (BlockMeta, bool) {
	if !idRE.MatchString(id) {
		return BlockMeta{}, false
	}
	a, b, _ := strings.Cut(id, "_")
	mn, err1 := strconv.ParseInt(a, 10, 64)
	mx, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || mx <= mn {
		return BlockMeta{}, false
	}
	return BlockMeta{ID: id, MinT: mn, MaxT: mx}, true
}

// LocalArchive stores archived blocks as files "<minT>_<maxT>.blk" in a
// directory. The time range is encoded in the name, so listing needs no file
// reads.
type LocalArchive struct{ Dir string }

// NewLocalArchive creates the directory if needed.
func NewLocalArchive(dir string) (*LocalArchive, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &LocalArchive{Dir: dir}, nil
}

func (a *LocalArchive) PutBlock(ctx context.Context, meta BlockMeta, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := parseBlockID(meta.ID); !ok {
		return fmt.Errorf("metrics: invalid archive block id %q", meta.ID)
	}
	return writeFileAtomic(filepath.Join(a.Dir, meta.ID+".blk"), data)
}

func (a *LocalArchive) GetBlock(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := parseBlockID(id); !ok {
		return nil, fmt.Errorf("metrics: invalid archive block id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(a.Dir, id+".blk"))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrArchiveNotFound, id)
	}
	return b, err
}

func (a *LocalArchive) ListRanges(ctx context.Context, minT, maxT int64) ([]BlockMeta, error) {
	ents, err := os.ReadDir(a.Dir)
	if err != nil {
		return nil, err
	}
	var out []BlockMeta
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".blk")
		if !ok {
			continue
		}
		m, ok := parseBlockID(name)
		if !ok || m.MaxT <= minT || m.MinT > maxT {
			continue
		}
		if info, err := e.Info(); err == nil {
			m.Size = info.Size()
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MinT < out[j].MinT })
	return out, nil
}

// ArchiveIndex is an in-memory, time-sorted index of archived blocks. Queries
// use it to decide which blocks to fetch.
type ArchiveIndex struct {
	mu    sync.RWMutex
	metas []BlockMeta
}

// Replace swaps the index contents.
func (x *ArchiveIndex) Replace(m []BlockMeta) {
	c := append([]BlockMeta(nil), m...)
	sort.Slice(c, func(i, j int) bool { return c[i].MinT < c[j].MinT })
	x.mu.Lock()
	x.metas = c
	x.mu.Unlock()
}

// Add inserts or replaces one entry.
func (x *ArchiveIndex) Add(m BlockMeta) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for i := range x.metas {
		if x.metas[i].ID == m.ID {
			x.metas[i] = m
			return
		}
	}
	x.metas = append(x.metas, m)
	sort.Slice(x.metas, func(i, j int) bool { return x.metas[i].MinT < x.metas[j].MinT })
}

// Overlapping returns the blocks whose window intersects [minT, maxT].
func (x *ArchiveIndex) Overlapping(minT, maxT int64) []BlockMeta {
	x.mu.RLock()
	defer x.mu.RUnlock()
	i := sort.Search(len(x.metas), func(i int) bool { return x.metas[i].MaxT > minT })
	var out []BlockMeta
	for ; i < len(x.metas) && x.metas[i].MinT <= maxT; i++ {
		out = append(out, x.metas[i])
	}
	return out
}

// Has reports whether the id is indexed.
func (x *ArchiveIndex) Has(id string) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for _, m := range x.metas {
		if m.ID == id {
			return true
		}
	}
	return false
}

// Len returns the number of indexed blocks.
func (x *ArchiveIndex) Len() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.metas)
}

// blockCache is a size-limited LRU of parsed archive blocks.
type blockCache struct {
	mu           sync.Mutex
	max, size    int64
	ll           *list.List
	items        map[string]*list.Element
	hits, misses int64
}

type cacheItem struct {
	id string
	b  *blockFile
}

func newBlockCache(max int64) *blockCache {
	return &blockCache{max: max, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *blockCache) Get(id string) (*blockFile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[id]; ok {
		c.ll.MoveToFront(e)
		c.hits++
		return e.Value.(*cacheItem).b, true
	}
	c.misses++
	return nil, false
}

// Put caches b; blocks larger than the whole cache are not cached.
func (c *blockCache) Put(id string, b *blockFile) {
	n := int64(len(b.mem))
	if n > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[id]; ok {
		c.size -= int64(len(e.Value.(*cacheItem).b.mem))
		c.ll.Remove(e)
		delete(c.items, id)
	}
	c.items[id] = c.ll.PushFront(&cacheItem{id, b})
	c.size += n
	for c.size > c.max {
		last := c.ll.Back()
		it := last.Value.(*cacheItem)
		c.size -= int64(len(it.b.mem))
		c.ll.Remove(last)
		delete(c.items, it.id)
	}
}

func (c *blockCache) stats() (size, hits, misses int64, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size, c.hits, c.misses, len(c.items)
}
