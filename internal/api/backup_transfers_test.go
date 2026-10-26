// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

type fakeTransfers struct {
	*fakeBackups
	copies   []BackupCopy
	started  BackupCopyInput
	proposed RestoreProposalInput
	metaMade int
}

func (f *fakeTransfers) BackupCopies(context.Context) ([]BackupCopy, error) {
	return append([]BackupCopy(nil), f.copies...), nil
}

func (f *fakeTransfers) BackupCopy(_ context.Context, id string) (BackupCopy, error) {
	for _, c := range f.copies {
		if c.ID == id {
			return c, nil
		}
	}
	return BackupCopy{}, ErrNotFound
}

func (f *fakeTransfers) StartBackupCopy(_ context.Context, a authz.Subject, c string, in BackupCopyInput) (BackupCopy, error) {
	f.started = in
	if err := f.record(a, "copy %s %s", c, in.Target); err != nil {
		return BackupCopy{}, err
	}
	return BackupCopy{ID: "cp-1", Cluster: c, Swarm: "s1", Backup: in.Backup, Target: in.Target, Status: "running", By: a.ID}, nil
}

func (f *fakeTransfers) ProposeCopyRestore(_ context.Context, a authz.Subject, id string, in RestoreProposalInput) (RestoreProposal, error) {
	f.proposed = in
	if err := f.record(a, "propose %s", id); err != nil {
		return RestoreProposal{}, err
	}
	return RestoreProposal{Copy: id, ApprovalRequired: true, TargetSwarm: in.TargetSwarm, TargetCluster: in.TargetCluster, Digest: "sha256:" + strings.Repeat("b", 64)}, nil
}

func (f *fakeTransfers) MetastoreBackups(context.Context) ([]MetastoreBackup, error) {
	return []MetastoreBackup{{Name: "meta-1", CreatedAt: time.Unix(1, 0), Size: 10, Verified: true}}, nil
}

func (f *fakeTransfers) CreateMetastoreBackup(_ context.Context, a authz.Subject) (MetastoreBackup, error) {
	f.metaMade++
	if err := f.record(a, "metabackup"); err != nil {
		return MetastoreBackup{}, err
	}
	return MetastoreBackup{Name: "meta-2", Verified: true}, nil
}

func TestBackupCopyAndRestoreProposalRoutes(t *testing.T) {
	cf := newControlFixture(t)
	ft := &fakeTransfers{fakeBackups: cf.bak, copies: []BackupCopy{
		{ID: "cp-1", Cluster: "c1", Swarm: "s1", Backup: "b1", Target: "dr", Status: "succeeded"},
		{ID: "cp-2", Cluster: "c2", Swarm: "s2", Backup: "b9", Target: "dr", Status: "succeeded"}}}
	cf.srv.cfg.Backups = ft
	admin, _, _ := cf.setupAdmin("admin")
	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))

	// Copies are listed only for clusters the caller may read.
	var list List[BackupCopy]
	r := viewer.do("GET", "/api/v1/backup-copies", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != "cp-1" {
		t.Fatalf("viewer sees %+v", list.Items)
	}

	// Starting a copy needs the backup permission (operator), not read.
	body := map[string]string{"backup": "b1", "target": "dr"}
	wantError(t, viewer.do("POST", "/api/v1/clusters/c1/backup-copies", body), http.StatusForbidden, errForbidden)
	wantStatus(t, op.do("POST", "/api/v1/clusters/c1/backup-copies", body), http.StatusAccepted)
	if ft.started.Target != "dr" {
		t.Fatalf("started %+v", ft.started)
	}
	wantError(t, op.do("POST", "/api/v1/clusters/c2/backup-copies", body), http.StatusNotFound, errNotFound)
	for _, bad := range []map[string]string{{"backup": "b1"}, {"target": "dr"}, {"backup": "B 1", "target": "dr"}, {"backup": "b1", "target": "../x"}} {
		wantError(t, admin.do("POST", "/api/v1/clusters/c1/backup-copies", bad), http.StatusBadRequest, errBadRequest)
	}

	// A restore proposal needs the restore permission: operators are refused,
	// the administrator gets a read-only proposal that demands approval.
	in := map[string]string{"targetSwarm": "s2", "targetCluster": "restored"}
	wantError(t, op.do("POST", "/api/v1/backup-copies/cp-1/restore-proposal", in), http.StatusForbidden, errForbidden)
	wantError(t, admin.do("POST", "/api/v1/backup-copies/nope/restore-proposal", in), http.StatusNotFound, errNotFound)
	wantError(t, admin.do("POST", "/api/v1/backup-copies/cp-1/restore-proposal", map[string]string{"targetSwarm": "s2"}), http.StatusBadRequest, errBadRequest)
	var p RestoreProposal
	r = admin.do("POST", "/api/v1/backup-copies/cp-1/restore-proposal", in)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &p)
	if !p.ApprovalRequired || p.AutoPromotion || p.TargetCluster != "restored" || !strings.HasPrefix(p.Digest, "sha256:") {
		t.Fatalf("proposal %+v", p)
	}
	// Both the denied and the allowed restore decision are audited.
	if n := auditsOf(cf.fixture, authz.ActionRestore, authz.OutcomeDenied, authz.ResourceBackup); n == 0 {
		t.Fatal("denied proposal not audited")
	}
	if n := auditsOf(cf.fixture, authz.ActionRestore, authz.OutcomeAllowed, authz.ResourceBackup); n == 0 {
		t.Fatal("allowed proposal not audited")
	}
}

func TestMetastoreBackupRoutes(t *testing.T) {
	cf := newControlFixture(t)
	ft := &fakeTransfers{fakeBackups: cf.bak}
	cf.srv.cfg.Backups = ft
	admin, _, _ := cf.setupAdmin("admin")
	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	wantError(t, cf.client().do("POST", "/api/v1/settings/metastore-backups", nil), http.StatusUnauthorized, errUnauthenticated)
	wantError(t, op.do("POST", "/api/v1/settings/metastore-backups", nil), http.StatusForbidden, errForbidden)
	wantError(t, op.do("GET", "/api/v1/settings/metastore-backups", nil), http.StatusForbidden, errForbidden)
	if ft.metaMade != 0 {
		t.Fatal("service called although denied")
	}
	var list List[MetastoreBackup]
	r := admin.do("GET", "/api/v1/settings/metastore-backups", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &list)
	if len(list.Items) != 1 || !list.Items[0].Verified {
		t.Fatalf("%+v", list.Items)
	}
	wantStatus(t, admin.do("POST", "/api/v1/settings/metastore-backups", nil), http.StatusAccepted)
	if ft.metaMade != 1 {
		t.Fatalf("created %d", ft.metaMade)
	}
	// A service without the optional interfaces answers 503.
	cf.srv.cfg.Backups = cf.bak
	wantError(t, admin.do("POST", "/api/v1/settings/metastore-backups", nil), http.StatusServiceUnavailable, errUnavailable)
	wantError(t, admin.do("GET", "/api/v1/backup-copies", nil), http.StatusServiceUnavailable, errUnavailable)
}

func TestBackupPolicyVerifyBounds(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	long := strings.Repeat("x", 129)
	for _, v := range []map[string]any{{"enabled": true, "schedule": long}, {"enabled": true, "schedule": "0 4 * * 0", "maxMinutes": 241}} {
		wantError(t, admin.do("PUT", "/api/v1/clusters/c1/backup-policy", map[string]any{"enabled": true, "schedule": "0 2 * * *", "verify": v}), http.StatusBadRequest, errBadRequest)
	}
	ok := map[string]any{"enabled": true, "schedule": "0 2 * * *", "verify": map[string]any{"enabled": true, "schedule": "0 4 * * 0", "maxMinutes": 90}}
	wantStatus(t, admin.do("PUT", "/api/v1/clusters/c1/backup-policy", ok), http.StatusOK)
	if cf.bak.lastPol.Verify == nil || cf.bak.lastPol.Verify.MaxMinutes != 90 {
		t.Fatalf("policy %+v", cf.bak.lastPol)
	}
}
