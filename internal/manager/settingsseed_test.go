// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// TestApplyConfiguredSettings: lab E4 found no way to set the installation
// settings; the configured ones are merged on the leader, the installation
// ID never changes and the volume base only while no cluster exists.
func TestApplyConfiguredSettings(t *testing.T) {
	c := newCluster(t, 1, nil)
	st := c.leader().Store()
	cfg := ConfiguredSettings{InstallationID: "inst-1", VolumeBase: "/srv/dbcm", AgentImage: "agent@sha256:1",
		Accounts: &galera.Accounts{MonitorSecret: "dbcm-mon"}, Backup: &BackupSettings{JobImage: "job@sha256:2"}}
	if changed, err := ApplyConfiguredSettings(st, cfg); err != nil || !changed {
		t.Fatalf("first apply: %v %v", changed, err)
	}
	if changed, err := ApplyConfiguredSettings(st, cfg); err != nil || changed {
		t.Fatalf("second apply: %v %v", changed, err)
	}
	got, _, err := Get[Settings](st, CollSettings, SettingsKey)
	if err != nil || got.Value.InstallationID != "inst-1" || got.Value.AgentImage != "agent@sha256:1" ||
		got.Value.Accounts.MonitorSecret != "dbcm-mon" || got.Value.Backup.JobImage != "job@sha256:2" {
		t.Fatalf("stored %+v %v", got.Value, err)
	}
	if _, err := ApplyConfiguredSettings(st, ConfiguredSettings{InstallationID: "inst-2"}); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("installation ID changed: %v", err)
	}
	if _, err := ApplyConfiguredSettings(st, ConfiguredSettings{VolumeBase: "relative"}); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("relative volume base: %v", err)
	}
	if _, err := Put(st, CollClusters, "clu-1", ClusterRecord{ID: "clu-1"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyConfiguredSettings(st, ConfiguredSettings{VolumeBase: "/other"}); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("volume base changed under a cluster: %v", err)
	}
	// Unchanged volume base plus a new image is fine with clusters.
	if changed, err := ApplyConfiguredSettings(st, ConfiguredSettings{VolumeBase: "/srv/dbcm", AgentImage: "agent@sha256:3"}); err != nil || !changed {
		t.Fatalf("image update: %v %v", changed, err)
	}
}
