// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe/fakemysql"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

const newUUID = "b8e7d239-1f3e-11ef-9c6e-7a1b2c3d4e5f"

func TestStateRecovered(t *testing.T) {
	good := fakemysql.WithVars(fakemysql.SyncedVars(), "wsrep_cluster_state_uuid", newUUID)
	tests := []struct {
		name  string
		nodes []map[string]string
		size  int
		ok    bool
		why   string
	}{
		{"three synced, one new UUID", []map[string]string{good, good, good}, 3, true, ""},
		{"zero UUID", []map[string]string{good, good, fakemysql.WithVars(good, "wsrep_cluster_state_uuid", galera.ZeroUUID)}, 3, false, "zero"},
		{"different UUIDs", []map[string]string{good, good, fakemysql.WithVars(good, "wsrep_cluster_state_uuid", "6b1f8a4c-1f3e-11ef-9c6e-7a1b2c3d4e5f")}, 3, false, "disagree"},
		{"one node still joining", []map[string]string{good, good, fakemysql.WithVars(good, "wsrep_local_state_comment", "Joining")}, 3, false, "not Synced"},
		{"cluster smaller than expected", []map[string]string{good, good}, 3, false, "2 of 3"},
		{"cluster size variable wrong", []map[string]string{fakemysql.WithVars(good, "wsrep_cluster_size", "2"), good, good}, 3, false, "wsrep_cluster_size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var srv []*fakemysql.Server
			for _, v := range tc.nodes {
				srv = append(srv, node(t, v))
			}
			k, _ := checkerFor(t, srv...)
			ok, why, err := k.Check(context.Background(), "op", runner.Step{}, runner.Condition{Kind: runner.CondStateRecovered, ClusterSize: tc.size})
			if err != nil || ok != tc.ok || !strings.Contains(why, tc.why) {
				t.Fatalf("ok=%v why=%q err=%v", ok, why, err)
			}
		})
	}
	k, _ := checkerFor(t, node(t, good))
	if ok, _, _ := k.Check(context.Background(), "op", runner.Step{}, runner.Condition{Kind: runner.CondStateRecovered}); ok {
		t.Fatal("missing cluster size accepted")
	}
}
