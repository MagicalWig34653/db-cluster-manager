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
	BackupID  string    `json:"backup_id"`
	ClusterID string    `json:"cluster_id"`
	Created   time.Time `json:"created"`
	Objects   []string  `json:"objects"`
	// Verified: a restore verification of this set passed.
	Verified bool `json:"verified,omitempty"`
	// Pinned: an inspection session or a running restore uses it.
	Pinned bool `json:"pinned,omitempty"`
}

// RetentionSelect applies backup.Select to backup sets and then protects:
// pinned sets, the newest verified set (so a verified restore point always
// remains), and the newest set overall. It is pure.
func RetentionSelect(sets []BackupSet, p backup.Policy) (keep, remove []BackupSet) {
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
	var errs []error
	for _, s := range remove {
		if s.Pinned {
			errs = append(errs, fmt.Errorf("dbbackup: set %s is pinned", s.BackupID))
			continue
		}
		for _, o := range s.Objects {
			for _, name := range []string{ManifestName(o), o} {
				if err := b.Delete(ctx, name); err != nil && !errors.Is(err, backup.ErrNotFound) {
					errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}
