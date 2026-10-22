// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

// Errors of the Store.
var (
	// ErrConflict is metastore.ErrVersionConflict: a compare-and-set lost.
	ErrConflict = metastore.ErrVersionConflict
	// ErrNotLeader is metastore.ErrNotLeader: writes and consistent reads
	// need the leader.
	ErrNotLeader = metastore.ErrNotLeader
	// ErrNotFound is returned by Update callers that require an existing
	// document.
	ErrNotFound = errors.New("manager: document not found")
)

// Store is a typed view of the replicated document store of one metastore
// node. Writes go through Raft (leader only). Reads are local and may lag
// behind the leader unless the Consistent option is given.
type Store struct {
	node           *metastore.Node
	barrierTimeout time.Duration
}

// NewStore returns a Store on node.
func NewStore(node *metastore.Node) *Store {
	return &Store{node: node, barrierTimeout: 5 * time.Second}
}

// Node returns the underlying metastore node.
func (s *Store) Node() *metastore.Node { return s.node }

type readOpts struct{ consistent bool }

// ReadOption modifies a read.
type ReadOption func(*readOpts)

// Consistent makes a read run a Raft barrier first, so it observes every
// write committed before the call. It only works on the leader; on a
// follower the read fails with ErrNotLeader (followers cannot serve
// linearizable reads with this library).
func Consistent() ReadOption { return func(o *readOpts) { o.consistent = true } }

func (s *Store) prepareRead(opts []ReadOption) error {
	var o readOpts
	for _, f := range opts {
		f(&o)
	}
	if o.consistent {
		return s.node.Barrier(s.barrierTimeout)
	}
	return nil
}

// Raw returns a document.
func (s *Store) Raw(coll, key string, opts ...ReadOption) (metastore.Document, bool, error) {
	if err := s.prepareRead(opts); err != nil {
		return metastore.Document{}, false, err
	}
	d, ok := s.node.Doc(coll, key)
	return d, ok, nil
}

// RawList returns a collection sorted by key.
func (s *Store) RawList(coll string, opts ...ReadOption) ([]metastore.Document, error) {
	if err := s.prepareRead(opts); err != nil {
		return nil, err
	}
	return s.node.Docs(coll), nil
}

// Txn applies ops atomically (see metastore.DocOp for IfVersion).
func (s *Store) Txn(ops ...metastore.DocOp) ([]metastore.Document, error) {
	return s.node.ApplyDocs(ops...)
}

// Versioned is a decoded document with its version.
type Versioned[T any] struct {
	Key     string
	Version uint64
	Value   T
}

// IfVersion returns a pointer for metastore.DocOp.IfVersion. 0 means "must
// not exist".
func IfVersion(v uint64) *uint64 { return &v }

// Get decodes one document. ok is false when it does not exist.
func Get[T any](s *Store, coll, key string, opts ...ReadOption) (v Versioned[T], ok bool, err error) {
	d, ok, err := s.Raw(coll, key, opts...)
	if err != nil || !ok {
		return v, false, err
	}
	if err := json.Unmarshal(d.Value, &v.Value); err != nil {
		return v, false, fmt.Errorf("manager: decode %s/%s: %w", coll, key, err)
	}
	v.Key, v.Version = key, d.Version
	return v, true, nil
}

// List decodes a collection sorted by key.
func List[T any](s *Store, coll string, opts ...ReadOption) ([]Versioned[T], error) {
	docs, err := s.RawList(coll, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]Versioned[T], 0, len(docs))
	for _, d := range docs {
		var v T
		if err := json.Unmarshal(d.Value, &v); err != nil {
			return nil, fmt.Errorf("manager: decode %s/%s: %w", coll, d.Key, err)
		}
		out = append(out, Versioned[T]{Key: d.Key, Version: d.Version, Value: v})
	}
	return out, nil
}

// PutOp encodes v into a write operation.
func PutOp(coll, key string, v any, ifVersion *uint64) (metastore.DocOp, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return metastore.DocOp{}, fmt.Errorf("manager: encode %s/%s: %w", coll, key, err)
	}
	return metastore.DocOp{Collection: coll, Key: key, Value: b, IfVersion: ifVersion}, nil
}

// Put writes v (conditionally with ifVersion) and returns the new version.
func Put(s *Store, coll, key string, v any, ifVersion *uint64) (uint64, error) {
	op, err := PutOp(coll, key, v, ifVersion)
	if err != nil {
		return 0, err
	}
	res, err := s.Txn(op)
	if err != nil {
		return 0, err
	}
	return res[0].Version, nil
}

// Delete removes a document (conditionally with ifVersion).
func Delete(s *Store, coll, key string, ifVersion *uint64) error {
	_, err := s.Txn(metastore.DocOp{Collection: coll, Key: key, Delete: true, IfVersion: ifVersion})
	return err
}

// maxUpdateAttempts bounds the compare-and-set retries of Update.
const maxUpdateAttempts = 8

// Update reads a document consistently, applies fn and writes the result
// with compare-and-set, retrying on conflicts. fn receives the zero value
// and exists=false for a missing document; returning an error aborts.
func Update[T any](s *Store, coll, key string, fn func(cur T, exists bool) (T, error)) (T, uint64, error) {
	var zero T
	for range maxUpdateAttempts {
		cur, ok, err := Get[T](s, coll, key, Consistent())
		if err != nil {
			return zero, 0, err
		}
		next, err := fn(cur.Value, ok)
		if err != nil {
			return zero, 0, err
		}
		ver, err := Put(s, coll, key, next, IfVersion(cur.Version))
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return zero, 0, err
		}
		return next, ver, nil
	}
	return zero, 0, fmt.Errorf("%w: %s/%s: too many concurrent updates", ErrConflict, coll, key)
}
