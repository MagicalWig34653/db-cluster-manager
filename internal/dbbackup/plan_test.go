// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

func backupRequest(t *testing.T, base string) BackupRequest {
	return BackupRequest{
		OpID: "op-b1", BackupID: "b1", InstallationID: testInst, ClusterID: testCluster, ClusterName: "prod",
		Method: MethodPhysical, JobImage: "dbcm/job:1", Network: "dbcm-prod-net", DBCredentialSecret: "db-cred",
		BackendSecret: "backend-cfg", RecipientKey: pubString(t, testKey(t)), VolumeBase: base,
		Job: agent.JobResources{NanoCPUs: 5e8, MemoryBytes: 256 << 20}, StepTimeout: time.Minute,
	}
}

func TestBackupPlanPhysical(t *testing.T) {
	base := t.TempDir()
	res, err := BackupPlan(backupRequest(t, base), threeNodes(), SourcePolicy{ClusterSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source.Slot != "n2" {
		t.Fatalf("source %s", res.Source.Slot)
	}
	op := res.Operation
	var kinds []string
	for _, s := range op.Steps {
		kinds = append(kinds, s.ID+":"+string(s.Command))
		if s.Command != "" && s.Target != "h2" {
			t.Fatalf("step %s targets %s, not the source host", s.ID, s.Target)
		}
	}
	want := []string{"01-check-source:", "02-verify-identity:verify-identity", "03-run-backup:run-backup", "04-wait-backup:", "05-remove-job:remove-job"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("steps %v", kinds)
	}
	if !slices.Contains(condKinds(op.Steps[0]), runner.CondNodeSynced) || !slices.Contains(condKinds(op.Steps[0]), CondSourceEligible) {
		t.Fatalf("check-source conditions %v", condKinds(op.Steps[0]))
	}
	var a agent.RunBackupArgs
	if err := json.Unmarshal(op.Steps[2].Args, &a); err != nil {
		t.Fatal(err)
	}
	if a.Tool != agent.ToolMariabackup || a.SourceVolume != "dbcm-prod-n2-00000000" || a.Expected.NodeUUID != uuid2 ||
		a.DBHost != "dbcm-prod-n2" || a.Object != res.Objects[0] {
		t.Fatalf("args %+v", a)
	}
	if op.Steps[3].Preconditions[1].Kind != CondBackupComplete || op.Steps[3].Preconditions[1].Target != a.Object {
		t.Fatalf("wait gate %+v", op.Steps[3].Preconditions)
	}
	if !slices.Contains(op.Locks, "cluster/"+testCluster) {
		t.Fatalf("locks %v", op.Locks)
	}
	if strings.Contains(string(op.Steps[2].Args), "password") {
		t.Fatal("credentials in args")
	}
	requireAgentAccepts(t, op, base)
}

func TestBackupPlanLogicalOneJobPerDatabase(t *testing.T) {
	base := t.TempDir()
	req := backupRequest(t, base)
	req.Method, req.Databases = MethodLogical, []string{"shop", "crm"}
	res, err := BackupPlan(req, threeNodes(), SourcePolicy{ClusterSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 2 || len(res.Operation.Steps) != 1+2*3 {
		t.Fatalf("objects %v steps %d", res.Objects, len(res.Operation.Steps))
	}
	for _, s := range res.Operation.Steps {
		if s.Command == agent.KindVerifyIdentity {
			t.Fatal("logical backup does not mount the volume")
		}
	}
	requireAgentAccepts(t, res.Operation, base)

	req.Databases = []string{"Shop", "shop"}
	if _, err := BackupPlan(req, threeNodes(), SourcePolicy{ClusterSize: 3}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("job name collision: %v", err)
	}
}

func TestBackupPlanRefusals(t *testing.T) {
	base := t.TempDir()
	cases := map[string]func(*BackupRequest){
		"backup id":     func(r *BackupRequest) { r.BackupID = "B 1" },
		"no limits":     func(r *BackupRequest) { r.Job = agent.JobResources{} },
		"no recipient":  func(r *BackupRequest) { r.RecipientKey = "" },
		"physical dbs":  func(r *BackupRequest) { r.Databases = []string{"x"} },
		"logical no db": func(r *BackupRequest) { r.Method = MethodLogical },
		"method":        func(r *BackupRequest) { r.Method = "snapshot" },
		"relative base": func(r *BackupRequest) { r.VolumeBase = "vol" },
	}
	for name, mut := range cases {
		r := backupRequest(t, base)
		mut(&r)
		if _, err := BackupPlan(r, threeNodes(), SourcePolicy{ClusterSize: 3}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Unsynced donor candidates: no plan at all.
	nodes := threeNodes()
	nodes[1].Health.LocalState, nodes[2].Health.LocalState = "Donor/Desynced", "Donor/Desynced"
	if _, err := BackupPlan(backupRequest(t, base), nodes, SourcePolicy{ClusterSize: 3}); !errors.Is(err, ErrNoSource) {
		t.Fatalf("got %v", err)
	}
}

func TestBackupStepsNeedNoApprovalButRestoresDo(t *testing.T) {
	res, err := BackupPlan(backupRequest(t, t.TempDir()), threeNodes(), SourcePolicy{ClusterSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Operation.Steps {
		if slices.Contains(condKinds(s), CondApproved) {
			t.Fatalf("scheduled backups must run unattended; step %s needs approval", s.ID)
		}
	}
}

// TestBackupPlanServiceHost: job services are created and removed by the
// agent on the service host (a Swarm manager node, lab E4) and pinned to
// the source node; verify-identity stays on the source host.
func TestBackupPlanServiceHost(t *testing.T) {
	req := backupRequest(t, t.TempDir())
	req.ServiceHost = "mgr"
	res, err := BackupPlan(req, threeNodes(), SourcePolicy{ClusterSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	src := res.Source.HostID
	for _, s := range res.Operation.Steps {
		switch s.Command {
		case agent.KindVerifyIdentity:
			if s.Target != src {
				t.Errorf("verify-identity on %s, want the source host %s", s.Target, src)
			}
		case agent.KindRunBackup:
			var a agent.RunBackupArgs
			if s.Target != "mgr" || json.Unmarshal(s.Args, &a) != nil || a.NodeID != src {
				t.Errorf("run-backup on %s pinned to %q", s.Target, a.NodeID)
			}
		case agent.KindRemoveJob:
			if s.Target != "mgr" {
				t.Errorf("remove-job on %s", s.Target)
			}
		}
	}
}
