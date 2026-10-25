// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Replicated documents.
//
// Besides the operation journal the state machine holds a generic document
// store: collections of JSON documents addressed by (collection, key). The
// manager keeps its desired state and inventory there (package
// internal/manager). Every write is a Raft command of kind KindDocs that
// carries one or more DocOps applied atomically: either all of them apply or
// none does.
//
// Versions are global revisions: the store keeps one counter that advances
// by one per successful KindDocs command, and every document written by the
// command gets that revision as its Version. A deleted and re-created
// document therefore never reuses an earlier version, so compare-and-set on
// Version is free of the ABA problem.
//
// Backward compatibility: commands without Docs decode and apply exactly as
// before. A snapshot of a state without documents is written in the
// opjournal snapshot format (as before this extension), and a snapshot in
// that format restores with an empty document store. A state with documents
// is written in the envelope format snapshotFormatV2.

// KindDocs is the Command kind of a document transaction.
const KindDocs = "docs"

var (
	// ErrVersionConflict is returned when a DocOp's IfVersion does not match.
	// Nothing of the transaction was applied.
	ErrVersionConflict = errors.New("metastore: document version conflict")
	// ErrInvalidDoc is returned for a malformed transaction.
	ErrInvalidDoc = errors.New("metastore: invalid document operation")
)

// Document is one stored document.
type Document struct {
	Collection string          `json:"collection"`
	Key        string          `json:"key"`
	Version    uint64          `json:"version"`
	Value      json.RawMessage `json:"value"`
}

// DocOp is one write of a document transaction.
type DocOp struct {
	Collection string `json:"collection"`
	Key        string `json:"key"`
	// IfVersion makes the write conditional: nil writes unconditionally, 0
	// requires that the document does not exist, any other value requires
	// the document's current Version to equal it.
	IfVersion *uint64 `json:"if_version,omitempty"`
	// Delete removes the document; Value is then ignored. Deleting a
	// missing document without IfVersion is not an error.
	Delete bool            `json:"delete,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
}

func validateDocOps(ops []DocOp) error {
	if len(ops) == 0 {
		return fmt.Errorf("%w: empty transaction", ErrInvalidDoc)
	}
	for _, op := range ops {
		if op.Collection == "" || op.Key == "" {
			return fmt.Errorf("%w: collection and key required", ErrInvalidDoc)
		}
		if !op.Delete && (len(op.Value) == 0 || !json.Valid(op.Value)) {
			return fmt.Errorf("%w: %s/%s: value must be valid JSON", ErrInvalidDoc, op.Collection, op.Key)
		}
	}
	return nil
}

// docStore is the document part of the state machine. It is guarded by the
// fsm lock.
type docStore struct {
	rev  uint64
	docs map[string]map[string]Document
}

func newDocStore() *docStore { return &docStore{docs: map[string]map[string]Document{}} }

func (s *docStore) get(coll, key string) (Document, bool) {
	d, ok := s.docs[coll][key]
	return d, ok
}

// apply applies a transaction atomically and returns the written documents
// (deleted ones with their last version and no value).
func (s *docStore) apply(ops []DocOp) ([]Document, error) {
	if err := validateDocOps(ops); err != nil {
		return nil, err
	}
	// Check every condition against the state as it evolves within the
	// transaction, without mutating, then apply.
	type pending struct {
		doc     Document
		deleted bool
	}
	view := map[[2]string]pending{}
	cur := func(coll, key string) (Document, bool) {
		if p, ok := view[[2]string{coll, key}]; ok {
			return p.doc, !p.deleted
		}
		return s.get(coll, key)
	}
	next := s.rev + 1
	out := make([]Document, 0, len(ops))
	for _, op := range ops {
		d, exists := cur(op.Collection, op.Key)
		if op.IfVersion != nil {
			want := *op.IfVersion
			switch {
			case want == 0 && exists:
				return nil, fmt.Errorf("%w: %s/%s exists (version %d)", ErrVersionConflict, op.Collection, op.Key, d.Version)
			case want != 0 && !exists:
				return nil, fmt.Errorf("%w: %s/%s does not exist", ErrVersionConflict, op.Collection, op.Key)
			case want != 0 && d.Version != want:
				return nil, fmt.Errorf("%w: %s/%s is at version %d, not %d", ErrVersionConflict, op.Collection, op.Key, d.Version, want)
			}
		}
		k := [2]string{op.Collection, op.Key}
		if op.Delete {
			view[k] = pending{doc: d, deleted: true}
			out = append(out, Document{Collection: op.Collection, Key: op.Key, Version: d.Version})
			continue
		}
		nd := Document{Collection: op.Collection, Key: op.Key, Version: next, Value: bytes.Clone(op.Value)}
		view[k] = pending{doc: nd}
		out = append(out, nd)
	}
	for k, p := range view {
		if p.deleted {
			delete(s.docs[k[0]], k[1])
			if len(s.docs[k[0]]) == 0 {
				delete(s.docs, k[0])
			}
			continue
		}
		if s.docs[k[0]] == nil {
			s.docs[k[0]] = map[string]Document{}
		}
		s.docs[k[0]][k[1]] = p.doc
	}
	s.rev = next
	return out, nil
}

func (s *docStore) list(coll string) []Document {
	m := s.docs[coll]
	out := make([]Document, 0, len(m))
	for _, d := range m {
		d.Value = bytes.Clone(d.Value)
		out = append(out, d)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Key < out[b].Key })
	return out
}

func (s *docStore) empty() bool { return s.rev == 0 && len(s.docs) == 0 }

// snapshotFormatV2 names the envelope snapshot format.
const snapshotFormatV2 = "dcm-metastore/2"

type snapshotEnvelope struct {
	Format   string          `json:"format"`
	Journal  json.RawMessage `json:"journal"`
	Revision uint64          `json:"revision"`
	Docs     []Document      `json:"docs"`
}

func (s *docStore) export() (uint64, []Document) {
	colls := make([]string, 0, len(s.docs))
	for c := range s.docs {
		colls = append(colls, c)
	}
	sort.Strings(colls)
	var out []Document
	for _, c := range colls {
		out = append(out, s.list(c)...)
	}
	if out == nil {
		out = []Document{}
	}
	return s.rev, out
}

func restoreDocStore(rev uint64, docs []Document) (*docStore, error) {
	s := newDocStore()
	s.rev = rev
	for _, d := range docs {
		if d.Collection == "" || d.Key == "" || d.Version == 0 || d.Version > rev || !json.Valid(d.Value) {
			return nil, fmt.Errorf("metastore: invalid document %s/%s in snapshot", d.Collection, d.Key)
		}
		if _, dup := s.get(d.Collection, d.Key); dup {
			return nil, fmt.Errorf("metastore: duplicate document %s/%s in snapshot", d.Collection, d.Key)
		}
		if s.docs[d.Collection] == nil {
			s.docs[d.Collection] = map[string]Document{}
		}
		s.docs[d.Collection][d.Key] = d
	}
	return s, nil
}

// isEnvelope reports whether data is an envelope snapshot (it has a
// "format" member) rather than a legacy opjournal snapshot.
func isEnvelope(data []byte) (string, bool) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", false
	}
	raw, ok := probe["format"]
	if !ok {
		return "", false
	}
	var f string
	_ = json.Unmarshal(raw, &f)
	return f, true
}

// ApplyDocs replicates a document transaction and returns the written
// documents. It returns ErrNotLeader on a follower, ErrVersionConflict or
// ErrInvalidDoc when the transaction was rejected (nothing applied), and an
// error wrapping ErrOutcomeUnknown when the commit could not be confirmed.
func (n *Node) ApplyDocs(ops ...DocOp) ([]Document, error) {
	if err := validateDocOps(ops); err != nil {
		return nil, err
	}
	res, err := n.apply(Command{Kind: KindDocs, Docs: ops})
	if err != nil {
		return nil, err
	}
	return res.docs, res.err
}

// Doc returns the local replica of one document. Like Journal, reads are
// local and may lag behind the leader; the leader calls Barrier first when
// it needs every committed write.
func (n *Node) Doc(collection, key string) (Document, bool) {
	n.fsm.mu.Lock()
	defer n.fsm.mu.Unlock()
	d, ok := n.fsm.docs.get(collection, key)
	d.Value = bytes.Clone(d.Value)
	return d, ok
}

// Docs returns the local replica of a collection sorted by key.
func (n *Node) Docs(collection string) []Document { return n.fsm.docList(collection) }

// Revision returns the local document revision (the version of the latest
// applied document transaction).
func (n *Node) Revision() uint64 { return n.fsm.revision() }

// AllDocs returns a coherent local copy of every document, sorted by collection
// and key. Call Barrier first for leader-consistent maintenance scans.
func (n *Node) AllDocs() []Document {
	n.fsm.mu.Lock()
	defer n.fsm.mu.Unlock()
	_, docs := n.fsm.docs.export()
	for i := range docs {
		docs[i].Value = bytes.Clone(docs[i].Value)
	}
	return docs
}
