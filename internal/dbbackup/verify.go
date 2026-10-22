// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"sync"
	"time"
)

// Observation is what the verifier measured on a restored instance. The
// measuring side (SQL against the isolated instance) is a port; this
// package only evaluates.
type Observation struct {
	// StreamVerified: the restore job matched every chunk digest and the
	// totals against the manifest (from the job status).
	StreamVerified bool `json:"stream_verified"`
	// ServerReady: the instance accepted a connection of the inspection
	// account and reported wsrep_on=OFF.
	ServerReady bool `json:"server_ready"`
	WsrepOff    bool `json:"wsrep_off"`
	// TableCounts maps schema name to its number of base tables.
	TableCounts map[string]int `json:"table_counts"`
	// Checksums maps "schema.table" to CHECKSUM TABLE results of the sample.
	Checksums map[string]uint64 `json:"checksums,omitempty"`
	// Marker is the value read from dbcm_meta.backup_marker ("" if absent).
	Marker string `json:"marker,omitempty"`
}

// CheckResult is one verification check.
type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// VerifyReport is the outcome of a restore verification. Only a passed
// report counts as "verified restore" (backup success and verified
// restorability are distinct states).
type VerifyReport struct {
	BackupID  string        `json:"backup_id"`
	ClusterID string        `json:"cluster_id"`
	SessionID string        `json:"session_id"`
	At        time.Time     `json:"at"`
	Checks    []CheckResult `json:"checks"`
	Passed    bool          `json:"passed"`
}

// SampleTables picks up to n "schema.table" names deterministically for a
// backup (seeded by its ID), so repeated verifications of one backup compare
// the same tables.
func SampleTables(c *Catalog, backupID string, n int) []string {
	if c == nil || n <= 0 {
		return nil
	}
	var all []string
	for _, s := range c.Schemas {
		for _, t := range s.Tables {
			all = append(all, s.Name+"."+t.Name)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		hi, hj := fnv.New64a(), fnv.New64a()
		hi.Write([]byte(backupID + "\x00" + all[i]))
		hj.Write([]byte(backupID + "\x00" + all[j]))
		if a, b := hi.Sum64(), hj.Sum64(); a != b {
			return a < b
		}
		return all[i] < all[j]
	})
	return all[:min(n, len(all))]
}

// EvaluateVerification checks an observation against the manifest. Checks:
// stream integrity, server start with wsrep off, table count per schema
// equal to the captured catalog, sampled checksums equal to captured
// baselines (tables without a baseline only report their value), and the
// backup marker when the manifest carries one.
func EvaluateVerification(m Manifest, sessionID string, obs Observation, now time.Time) VerifyReport {
	r := VerifyReport{BackupID: m.BackupID, ClusterID: m.ClusterID, SessionID: sessionID, At: now.UTC()}
	add := func(name string, ok bool, f string, a ...any) {
		r.Checks = append(r.Checks, CheckResult{Name: name, Passed: ok, Detail: fmt.Sprintf(f, a...)})
	}
	add("stream-integrity", obs.StreamVerified, "chunk and total digests matched the manifest: %v", obs.StreamVerified)
	add("server-started", obs.ServerReady && obs.WsrepOff, "ready=%v wsrep_off=%v", obs.ServerReady, obs.WsrepOff)
	if m.Catalog == nil {
		add("table-counts", false, "the manifest has no catalog; table counts cannot be compared")
	} else {
		var diffs []string
		for _, s := range m.Catalog.Schemas {
			if got, want := obs.TableCounts[s.Name], len(s.Tables); got != want {
				diffs = append(diffs, fmt.Sprintf("%s: %d tables, expected %d", s.Name, got, want))
			}
		}
		slices.Sort(diffs)
		add("table-counts", len(diffs) == 0, "%d schema(s) compared; differences: %v", len(m.Catalog.Schemas), diffs)
		var bad, base []string
		for _, name := range SampleTables(m.Catalog, m.BackupID, len(obs.Checksums)) {
			got, ok := obs.Checksums[name]
			want := catalogChecksum(m.Catalog, name)
			switch {
			case !ok:
				bad = append(bad, name+" not measured")
			case want == nil:
				base = append(base, name)
			case *want != got:
				bad = append(bad, fmt.Sprintf("%s checksum %d, expected %d", name, got, *want))
			}
		}
		add("checksum-sample", len(bad) == 0, "%d sampled, %d without baseline; mismatches: %v", len(obs.Checksums), len(base), bad)
	}
	if m.Marker != "" {
		add("marker", obs.Marker == m.Marker, "marker %q, expected %q", obs.Marker, m.Marker)
	}
	r.Passed = true
	for _, c := range r.Checks {
		r.Passed = r.Passed && c.Passed
	}
	return r
}

func catalogChecksum(c *Catalog, name string) *uint64 {
	for _, s := range c.Schemas {
		for _, t := range s.Tables {
			if s.Name+"."+t.Name == name {
				return t.Checksum
			}
		}
	}
	return nil
}

// VerifiedStore keeps verification reports. The manager implements it on
// the metastore; MemVerifiedStore is for tests and single-process use.
type VerifiedStore interface {
	Record(r VerifyReport) error
	// LastVerified returns the newest PASSED report of a cluster: the "last
	// verified restore" the UI shows.
	LastVerified(clusterID string) (VerifyReport, bool)
}

// MemVerifiedStore is an in-memory VerifiedStore.
type MemVerifiedStore struct {
	mu      sync.Mutex
	reports []VerifyReport
}

// Record implements VerifiedStore.
func (s *MemVerifiedStore) Record(r VerifyReport) error {
	if r.BackupID == "" || r.ClusterID == "" || r.At.IsZero() {
		return fmt.Errorf("%w: report needs backup, cluster and time", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, r)
	return nil
}

// LastVerified implements VerifiedStore.
func (s *MemVerifiedStore) LastVerified(clusterID string) (VerifyReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best VerifyReport
	found := false
	for _, r := range s.reports {
		if r.ClusterID == clusterID && r.Passed && (!found || r.At.After(best.At)) {
			best, found = r, true
		}
	}
	return best, found
}

// Verified reports whether the backup has a passed verification.
func (s *MemVerifiedStore) Verified(backupID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.reports, func(r VerifyReport) bool { return r.BackupID == backupID && r.Passed })
}
