// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

type fakeHostSources struct {
	items []HostSource
	runs  int
}

func (f *fakeHostSources) List(context.Context) ([]HostSource, error) { return f.items, nil }
func (f *fakeHostSources) Get(_ context.Context, id string) (HostSource, error) {
	for _, h := range f.items {
		if h.ID == id {
			return h, nil
		}
	}
	return HostSource{}, ErrNotFound
}
func (f *fakeHostSources) Put(_ context.Context, _ authz.Subject, in HostSourceInput) (HostSource, error) {
	f.items = append(f.items, in.HostSource)
	return in.HostSource, nil
}
func (f *fakeHostSources) Delete(context.Context, authz.Subject, string) error { return nil }
func (f *fakeHostSources) Run(context.Context, authz.Subject, string) (OperationRef, error) {
	f.runs++
	return OperationRef{ID: "op-restic", Kind: "restic/backup"}, nil
}
func (f *fakeHostSources) Restore(context.Context, authz.Subject, string, HostRestoreInput) (OperationRef, error) {
	return OperationRef{ID: "op-restore", Kind: "restic/restore"}, nil
}

func TestHostSourceRoutesAuthzAndAudit(t *testing.T) {
	f := newFixture(t)
	store := &fakeHostSources{items: []HostSource{{ID: "m1", Name: "Managed", Kind: "managed-host", Swarm: "s1", HostID: "n1"},
		{ID: "e1", Name: "External", Kind: "external-host", HostID: "outside"}}}
	f.srv.cfg.HostSources = store
	viewer := f.loggedIn("viewer", authz.RoleViewer, authz.SwarmScope("s1"))
	op := f.loggedIn("operator", authz.RoleOperator, authz.SwarmScope("s1"))
	admin, _, _ := f.setupAdmin("admin")
	var got List[HostSource]
	r := viewer.do("GET", "/api/v1/host-sources", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &got)
	if len(got.Items) != 1 || got.Items[0].ID != "m1" {
		t.Fatalf("filtered list %+v", got)
	}
	wantError(t, viewer.do("POST", "/api/v1/host-sources/m1/backups", map[string]any{}), http.StatusForbidden, errForbidden)
	wantError(t, viewer.do("GET", "/api/v1/host-sources/e1", nil), http.StatusNotFound, errNotFound)
	wantStatus(t, op.do("POST", "/api/v1/host-sources/m1/backups", map[string]any{}), http.StatusAccepted)
	wantError(t, op.do("POST", "/api/v1/host-sources/m1/restore", HostRestoreInput{Snapshot: "abc123", Target: "/srv/new"}), http.StatusForbidden, errForbidden)
	wantError(t, op.do("POST", "/api/v1/host-sources", HostSourceInput{HostSource: HostSource{ID: "bad"}}), http.StatusForbidden, errForbidden)
	create := HostSourceInput{HostSource: HostSource{ID: "new", Name: "New", Kind: "external-host", Transport: "ssh-plan", HostID: "outside",
		Paths: []string{"/srv/data"}, Backend: restic.BackendLocal, Repository: "/backup/repo", PasswordSecret: "pw"}, Password: "test-only"}
	wantStatus(t, admin.do("POST", "/api/v1/host-sources", create), http.StatusCreated)
	wantStatus(t, admin.do("POST", "/api/v1/host-sources/m1/restore", HostRestoreInput{Snapshot: "abc123", Target: "/srv/new"}), http.StatusAccepted)
	if store.runs != 1 || auditsOf(f, authz.ActionBackup, authz.OutcomeAllowed, authz.ResourceBackup) != 1 {
		t.Fatalf("runs=%d backup audits missing", store.runs)
	}
	f.sink.SetErr(ErrUnavailable)
	wantError(t, admin.do("POST", "/api/v1/host-sources/m1/backups", map[string]any{}), http.StatusServiceUnavailable, errUnavailable)
	if store.runs != 1 {
		t.Fatal("audit outage reached backend")
	}
}
