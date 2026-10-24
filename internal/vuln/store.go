// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

// Persistence.
//
// Everything of this package lives in the replicated metastore document
// store (collections below). KV is the minimal view of it; MetastoreKV
// adapts a *metastore.Node (or anything with the same methods) and
// MemoryKV is the test fake. Store is the typed layer on top. Writes that
// belong together go into one transaction (all or nothing).

// Collections.
const (
	CollAdvisories = "vuln_advisories"
	CollFindings   = "vuln_findings"
	CollFeeds      = "vuln_feeds"
	CollKEV        = "vuln_kev"
	CollProposals  = "vuln_proposals"
	CollPolicy     = "vuln_policy"
	CollProviders  = "vuln_ai_providers"
	CollAudit      = "vuln_audit"
)

// KVDoc is one stored document.
type KVDoc struct {
	Key     string
	Version uint64
	Value   json.RawMessage
}

// KVPut is one write of a transaction. IfVersion follows metastore.DocOp:
// nil unconditional, 0 must not exist, otherwise must equal.
type KVPut struct {
	Collection string
	Key        string
	Value      json.RawMessage
	Delete     bool
	IfVersion  *uint64
}

// ErrConflict is returned when a conditional write lost a race.
var ErrConflict = errors.New("vuln: version conflict")

// KV is the document store seen by this package.
type KV interface {
	Get(coll, key string) (KVDoc, bool, error)
	List(coll string) ([]KVDoc, error)
	Txn(puts ...KVPut) error
}

// DocNode is the subset of *metastore.Node used by MetastoreKV.
type DocNode interface {
	Doc(collection, key string) (metastore.Document, bool)
	Docs(collection string) []metastore.Document
	ApplyDocs(ops ...metastore.DocOp) ([]metastore.Document, error)
}

// MetastoreKV stores documents in the Raft metastore. Writes go through
// the leader (metastore.ErrNotLeader on a follower); reads are local.
type MetastoreKV struct{ Node DocNode }

// Get implements KV.
func (m MetastoreKV) Get(coll, key string) (KVDoc, bool, error) {
	d, ok := m.Node.Doc(coll, key)
	if !ok {
		return KVDoc{}, false, nil
	}
	return KVDoc{Key: d.Key, Version: d.Version, Value: d.Value}, true, nil
}

// List implements KV.
func (m MetastoreKV) List(coll string) ([]KVDoc, error) {
	ds := m.Node.Docs(coll)
	out := make([]KVDoc, 0, len(ds))
	for _, d := range ds {
		out = append(out, KVDoc{Key: d.Key, Version: d.Version, Value: d.Value})
	}
	return out, nil
}

// Txn implements KV.
func (m MetastoreKV) Txn(puts ...KVPut) error {
	if len(puts) == 0 {
		return nil
	}
	ops := make([]metastore.DocOp, 0, len(puts))
	for _, p := range puts {
		ops = append(ops, metastore.DocOp{Collection: p.Collection, Key: p.Key, Value: p.Value, Delete: p.Delete, IfVersion: p.IfVersion})
	}
	_, err := m.Node.ApplyDocs(ops...)
	if errors.Is(err, metastore.ErrVersionConflict) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

// MemoryKV is an in-memory KV with the same version semantics (a fake).
type MemoryKV struct {
	mu   sync.Mutex
	rev  uint64
	docs map[string]map[string]KVDoc
}

// Get implements KV.
func (m *MemoryKV) Get(coll, key string) (KVDoc, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.docs[coll][key]
	return d, ok, nil
}

// List implements KV.
func (m *MemoryKV) List(coll string) ([]KVDoc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]KVDoc, 0, len(m.docs[coll]))
	for _, d := range m.docs[coll] {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Txn implements KV.
func (m *MemoryKV) Txn(puts ...KVPut) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.docs == nil {
		m.docs = map[string]map[string]KVDoc{}
	}
	for _, p := range puts {
		if p.IfVersion == nil {
			continue
		}
		d, ok := m.docs[p.Collection][p.Key]
		if (*p.IfVersion == 0 && ok) || (*p.IfVersion != 0 && (!ok || d.Version != *p.IfVersion)) {
			return fmt.Errorf("%w: %s/%s", ErrConflict, p.Collection, p.Key)
		}
	}
	m.rev++
	for _, p := range puts {
		if p.Delete {
			delete(m.docs[p.Collection], p.Key)
			continue
		}
		if m.docs[p.Collection] == nil {
			m.docs[p.Collection] = map[string]KVDoc{}
		}
		m.docs[p.Collection][p.Key] = KVDoc{Key: p.Key, Version: m.rev, Value: append(json.RawMessage(nil), p.Value...)}
	}
	return nil
}

// Store is the typed persistence of this package.
type Store struct{ KV KV }

func put(coll, key string, v any, ifVersion *uint64) (KVPut, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return KVPut{}, err
	}
	return KVPut{Collection: coll, Key: key, Value: b, IfVersion: ifVersion}, nil
}

func list[T any](kv KV, coll string) ([]T, []uint64, error) {
	docs, err := kv.List(coll)
	if err != nil {
		return nil, nil, err
	}
	out := make([]T, 0, len(docs))
	vers := make([]uint64, 0, len(docs))
	for _, d := range docs {
		var v T
		if err := json.Unmarshal(d.Value, &v); err != nil {
			return nil, nil, fmt.Errorf("vuln: decode %s/%s: %w", coll, d.Key, err)
		}
		out = append(out, v)
		vers = append(vers, d.Version)
	}
	return out, vers, nil
}

func get[T any](kv KV, coll, key string) (T, uint64, bool, error) {
	var v T
	d, ok, err := kv.Get(coll, key)
	if err != nil || !ok {
		return v, 0, ok, err
	}
	if err := json.Unmarshal(d.Value, &v); err != nil {
		return v, 0, false, fmt.Errorf("vuln: decode %s/%s: %w", coll, key, err)
	}
	return v, d.Version, true, nil
}

// Advisories returns the stored advisories sorted by ID.
func (s *Store) Advisories() ([]Advisory, error) {
	out, _, err := list[Advisory](s.KV, CollAdvisories)
	return out, err
}

// UpsertAdvisories deduplicates incoming advisories against each other and
// the stored ones by canonical ID and aliases, merges records of the same
// vulnerability and writes the result in one transaction. A record stored
// under an older key (for example a GHSA ID before a CVE was assigned) is
// moved to the canonical key. It returns the number of distinct advisories
// written.
func (s *Store) UpsertAdvisories(in []Advisory) (int, error) {
	stored, err := s.Advisories()
	if err != nil {
		return 0, err
	}
	byKey := map[string]Advisory{}
	alias := map[string]string{} // any id -> stored key
	for _, a := range stored {
		byKey[a.ID] = a
		alias[a.ID] = a.ID
		for _, x := range a.Aliases {
			alias[x] = a.ID
		}
	}
	touched := map[string]bool{}
	deleted := map[string]bool{}
	for _, a := range in {
		a = a.Normalize()
		if a.ID == "" {
			continue
		}
		var prevKeys []string
		for _, id := range append([]string{a.ID}, a.Aliases...) {
			if k, ok := alias[id]; ok && k != "" {
				prevKeys = append(prevKeys, k)
			}
		}
		merged := a
		for _, k := range sortedUnique(prevKeys) {
			if prev, ok := byKey[k]; ok {
				merged = Merge(prev, merged)
			}
		}
		for _, k := range sortedUnique(prevKeys) {
			if k != merged.ID {
				delete(byKey, k)
				deleted[k] = true
				delete(touched, k)
			}
		}
		delete(deleted, merged.ID)
		byKey[merged.ID] = merged
		touched[merged.ID] = true
		alias[merged.ID] = merged.ID
		for _, x := range merged.Aliases {
			alias[x] = merged.ID
		}
	}
	var puts []KVPut
	keys := make([]string, 0, len(touched))
	for k := range touched {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p, err := put(CollAdvisories, k, byKey[k], nil)
		if err != nil {
			return 0, err
		}
		puts = append(puts, p)
	}
	for k := range deleted {
		puts = append(puts, KVPut{Collection: CollAdvisories, Key: k, Delete: true})
	}
	return len(keys), s.KV.Txn(puts...)
}

// Findings returns the stored findings sorted by ID.
func (s *Store) Findings() ([]Finding, error) {
	out, _, err := list[Finding](s.KV, CollFindings)
	return out, err
}

// Finding returns one finding and its version.
func (s *Store) Finding(id string) (Finding, uint64, bool, error) {
	return get[Finding](s.KV, CollFindings, id)
}

// PutFindings writes findings in one transaction.
func (s *Store) PutFindings(fs []Finding) error {
	puts := make([]KVPut, 0, len(fs))
	for _, f := range fs {
		p, err := put(CollFindings, f.ID, f, nil)
		if err != nil {
			return err
		}
		puts = append(puts, p)
	}
	return s.KV.Txn(puts...)
}

// PutFinding writes one finding conditionally.
func (s *Store) PutFinding(f Finding, ifVersion uint64) error {
	p, err := put(CollFindings, f.ID, f, &ifVersion)
	if err != nil {
		return err
	}
	return s.KV.Txn(p)
}

// FeedStates returns all feed states.
func (s *Store) FeedStates() ([]FeedState, error) {
	out, _, err := list[FeedState](s.KV, CollFeeds)
	return out, err
}

// FeedState returns the state of one feed (zero when unknown).
func (s *Store) FeedState(name string) (FeedState, error) {
	st, _, ok, err := get[FeedState](s.KV, CollFeeds, name)
	if !ok {
		st = FeedState{Name: name}
	}
	return st, err
}

// PutFeedState stores a feed state.
func (s *Store) PutFeedState(st FeedState) error {
	p, err := put(CollFeeds, st.Name, st, nil)
	if err != nil {
		return err
	}
	return s.KV.Txn(p)
}

const kevKey = "catalog"

// KEV returns the stored known-exploited CVE IDs.
func (s *Store) KEV() (map[string]bool, error) {
	ids, _, _, err := get[[]string](s.KV, CollKEV, kevKey)
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m, err
}

// PutKEV replaces the known-exploited set.
func (s *Store) PutKEV(ids []string) error {
	p, err := put(CollKEV, kevKey, sortedUnique(ids), nil)
	if err != nil {
		return err
	}
	return s.KV.Txn(p)
}

// Proposals returns all proposals sorted by ID.
func (s *Store) Proposals() ([]Proposal, error) {
	out, _, err := list[Proposal](s.KV, CollProposals)
	return out, err
}

// Proposal returns one proposal and its version.
func (s *Store) Proposal(id string) (Proposal, uint64, bool, error) {
	return get[Proposal](s.KV, CollProposals, id)
}

// PutProposal writes a proposal; ifVersion 0 requires that it is new.
func (s *Store) PutProposal(p Proposal, ifVersion uint64) error {
	kp, err := put(CollProposals, p.ID, p, &ifVersion)
	if err != nil {
		return err
	}
	return s.KV.Txn(kp)
}

const policyKey = "default"

// Policy returns the stored policy and its version, or the default policy
// (notify-only) with version 0.
func (s *Store) Policy() (Policy, uint64, error) {
	p, v, ok, err := get[Policy](s.KV, CollPolicy, policyKey)
	if err != nil {
		return Policy{}, 0, err
	}
	if !ok {
		return DefaultPolicy(), 0, nil
	}
	return p, v, nil
}

// PutPolicy writes the policy conditionally (ifVersion 0: none stored yet).
func (s *Store) PutPolicy(p Policy, ifVersion uint64) error {
	kp, err := put(CollPolicy, policyKey, p, &ifVersion)
	if err != nil {
		return err
	}
	return s.KV.Txn(kp)
}

// AuditRecords returns the audit trail in order.
func (s *Store) AuditRecords() ([]AuditRecord, error) {
	out, _, err := list[AuditRecord](s.KV, CollAudit)
	return out, err
}

// AppendAudit stores one record; the key orders records by time and is
// unique per record (it must not exist yet).
func (s *Store) AppendAudit(r AuditRecord) error {
	zero := uint64(0)
	p, err := put(CollAudit, r.Key(), r, &zero)
	if err != nil {
		return err
	}
	return s.KV.Txn(p)
}
