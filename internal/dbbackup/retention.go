// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// BackupSet is one backup run with all its objects (stream and manifest per
// object; a logical run has one object per database).
type BackupSet struct {
	// BaseBackupID couples a binlog set's lifetime to a full physical set.
	BaseBackupID string    `json:"base_backup_id,omitempty"`
	BackupID     string    `json:"backup_id"`
	ClusterID    string    `json:"cluster_id"`
	Created      time.Time `json:"created"`
	Objects      []string  `json:"objects"`
	// Verified: a restore verification of this set passed.
	Verified bool `json:"verified,omitempty"`
	// Pinned: an inspection session or a running restore uses it.
	Pinned bool `json:"pinned,omitempty"`
}

// RetentionSelect applies backup.Select to backup sets and then protects:
// pinned sets, the newest verified set (so a verified restore point always
// remains), and the newest set overall. It is pure.
func RetentionSelect(sets []BackupSet, p backup.Policy) (keep, remove []BackupSet) {
	var roots, logs []BackupSet
	pins := map[string]bool{}
	for _, s := range sets {
		if s.BaseBackupID != "" {
			logs = append(logs, s)
			if s.Pinned {
				pins[s.BaseBackupID] = true
			}
		} else {
			roots = append(roots, s)
		}
	}
	for i := range roots {
		if pins[roots[i].BackupID] {
			roots[i].Pinned = true
		}
	}
	keep, remove = retentionSelectRoots(roots, p)
	kept, known := map[string]bool{}, map[string]bool{}
	for _, s := range keep {
		kept[s.BackupID] = true
		known[s.BackupID] = true
	}
	for _, s := range remove {
		known[s.BackupID] = true
	}
	for _, s := range logs {
		if kept[s.BaseBackupID] || !known[s.BaseBackupID] {
			keep = append(keep, s)
		} else {
			remove = append(remove, s)
		}
	}
	// Delete dependents before their base; keep orphan archives for review.
	sort.SliceStable(remove, func(i, j int) bool { return remove[i].BaseBackupID != "" && remove[j].BaseBackupID == "" })
	return keep, remove
}

func retentionSelectRoots(sets []BackupSet, p backup.Policy) (keep, remove []BackupSet) {
	byID := map[string]BackupSet{}
	entries := make([]backup.Entry, 0, len(sets))
	for _, s := range sets {
		byID[s.BackupID] = s
		entries = append(entries, backup.Entry{Name: s.BackupID, Created: s.Created})
	}
	k, r := backup.Select(entries, p)
	kept := map[string]bool{}
	for _, e := range k {
		kept[e.Name] = true
	}
	sorted := append([]BackupSet(nil), sets...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Created.After(sorted[j].Created) })
	if len(sorted) > 0 {
		kept[sorted[0].BackupID] = true
	}
	for _, s := range sorted {
		if s.Verified {
			kept[s.BackupID] = true
			break
		}
	}
	for _, s := range sets {
		if s.Pinned {
			kept[s.BackupID] = true
		}
	}
	for _, e := range k {
		keep = append(keep, byID[e.Name])
	}
	for _, e := range r {
		if kept[e.Name] {
			keep = append(keep, byID[e.Name])
		} else {
			remove = append(remove, byID[e.Name])
		}
	}
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].Created.After(keep[j].Created) })
	return keep, remove
}

// ApplyRetention deletes the objects of the given sets: the manifest first,
// so an interrupted deletion never leaves a manifest pointing at a missing
// stream (a stream without manifest is an incomplete backup and is ignored).
// Missing objects are not errors.
func ApplyRetention(ctx context.Context, b backup.Backend, remove []BackupSet) error {
	// Recheck pins before any deletion and order children before roots even if
	// a caller did not obtain its list from RetentionSelect.
	for _, s := range remove {
		if s.Pinned {
			return fmt.Errorf("dbbackup: set %s is pinned", s.BackupID)
		}
	}
	remove = append([]BackupSet(nil), remove...)
	sort.SliceStable(remove, func(i, j int) bool { return remove[i].BaseBackupID != "" && remove[j].BaseBackupID == "" })
	for _, s := range remove {
		for _, o := range s.Objects {
			for _, name := range []string{ManifestName(o), o} {
				if err := b.Delete(ctx, name); err != nil && !errors.Is(err, backup.ErrNotFound) {
					return fmt.Errorf("delete %s: %w", name, err)
				}
			}
		}
	}
	return nil
}
