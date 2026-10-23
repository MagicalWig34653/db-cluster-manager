// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// ConfiguredSettings are the installation settings an operator provides in
// the manager configuration (cmd/manager "manager.settings"). Before lab E4
// nothing could write Settings.InstallationID, VolumeBase, AgentImage,
// Accounts or Backup: agent deployment, enrollment and cluster creation all
// answered settings_incomplete. There is still no API for them.
type ConfiguredSettings struct {
	InstallationID string
	VolumeBase     string
	AgentImage     string
	MonitorSocket  string
	// Accounts, if set, are the database accounts every new cluster gets
	// (copied into galera.Cluster.Accounts by CreateCluster).
	Accounts *galera.Accounts
	// Backup, if set, replaces Settings.Backup.
	Backup *BackupSettings
}

// ApplyConfiguredSettings merges c into the replicated settings (leader
// only, compare-and-set). The installation ID is immutable once set; the
// volume base can change only while no cluster exists. Empty fields of c
// leave the stored value alone. It reports whether anything changed.
func ApplyConfiguredSettings(s *Store, c ConfiguredSettings) (bool, error) {
	if c.VolumeBase != "" && (!filepath.IsAbs(c.VolumeBase) || filepath.Clean(c.VolumeBase) != c.VolumeBase) {
		return false, fmt.Errorf("%w: volume base %q must be a clean absolute path", ErrInvalidSettings, c.VolumeBase)
	}
	if c.MonitorSocket != "" && !filepath.IsLocal(c.MonitorSocket) {
		return false, fmt.Errorf("%w: monitor socket %q must be relative to the volume root", ErrInvalidSettings, c.MonitorSocket)
	}
	var clusters int
	if c.VolumeBase != "" {
		cl, err := List[ClusterRecord](s, CollClusters, Consistent())
		if err != nil {
			return false, err
		}
		clusters = len(cl)
	}
	changed := false
	_, _, err := Update(s, CollSettings, SettingsKey, func(cur Settings, _ bool) (Settings, error) {
		next := cur
		if c.InstallationID != "" {
			switch cur.InstallationID {
			case "", c.InstallationID:
				next.InstallationID = c.InstallationID
			default:
				return cur, fmt.Errorf("%w: installation ID is %q and cannot change to %q", ErrInvalidSettings, cur.InstallationID, c.InstallationID)
			}
		}
		if c.VolumeBase != "" && c.VolumeBase != cur.VolumeBase {
			if cur.VolumeBase != "" && clusters > 0 {
				return cur, fmt.Errorf("%w: volume base cannot change while %d cluster(s) exist", ErrInvalidSettings, clusters)
			}
			next.VolumeBase = c.VolumeBase
		}
		if c.AgentImage != "" {
			next.AgentImage = c.AgentImage
		}
		if c.MonitorSocket != "" {
			next.MonitorSocket = c.MonitorSocket
		}
		if c.Accounts != nil {
			next.Accounts = *c.Accounts
		}
		if c.Backup != nil {
			next.Backup = *c.Backup
		}
		if reflect.DeepEqual(next, cur) {
			return cur, errUnchanged
		}
		changed = true
		return next, nil
	})
	if errors.Is(err, errUnchanged) {
		return false, nil
	}
	return changed, err
}

var errUnchanged = errors.New("manager: settings unchanged")
