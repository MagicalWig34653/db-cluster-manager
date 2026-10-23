// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

type fakeReplicas struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeReplicas) Keyring(context.Context, authz.Subject) (KeyringStatus, error) {
	return KeyringStatus{CurrentKey: "dk-1", Replicas: []ReplicaKeyStatus{{ID: "m1", Leader: true, HasCurrentKey: true}, {ID: "m2"}}}, nil
}

func (f *fakeReplicas) RegisterReplicaKey(_ context.Context, actor authz.Subject, id, pub string) (ReplicaKeyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, actor.ID+":"+id+":"+pub)
	if f.err != nil {
		return ReplicaKeyStatus{}, f.err
	}
	return ReplicaKeyStatus{ID: id, PublicKeyID: "abcd", RegisteredBy: actor.ID, HasCurrentKey: true}, nil
}

func (f *fakeReplicas) RevokeReplicaCertificates(_ context.Context, actor authz.Subject, id string) (ReplicaRevocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "revoke:"+actor.ID+":"+id)
	return ReplicaRevocation{Replica: id, Revoked: 2}, f.err
}

func TestReplicaRevokeRoute(t *testing.T) {
	cf := newControlFixture(t)
	fr := &fakeReplicas{}
	cf.srv.cfg.Replicas = fr
	op := cf.loggedIn("op", authz.RoleOperator, authz.Global)
	wantError(t, op.do("POST", "/api/v1/settings/replicas/m2/revoke-certificates", nil), http.StatusForbidden, errForbidden)
	admin, _, _ := cf.setupAdmin("admin")
	wantError(t, admin.do("POST", "/api/v1/settings/replicas/m2/revoke-certificates", map[string]string{"x": "y"}), http.StatusBadRequest, errBadRequest)
	var out ReplicaRevocation
	r := admin.do("POST", "/api/v1/settings/replicas/m2/revoke-certificates", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &out)
	if out.Replica != "m2" || out.Revoked != 2 || len(fr.calls) != 1 {
		t.Fatalf("%+v %v", out, fr.calls)
	}
	if auditsOf(cf.fixture, authz.ActionUpdate, authz.OutcomeDenied, authz.ResourceSettings) != 1 {
		t.Fatal("denied revocation not audited")
	}
	fr.err = ErrSettingsIncomplete
	wantError(t, admin.do("POST", "/api/v1/settings/replicas/m2/revoke-certificates", nil), http.StatusConflict, errSettingsIncomplete)
}

// TestReplicaKeyRoutes: listing and registering replicas need settings
// read/update permission (operators have neither), registrations are
// audited, malformed IDs and keys are refused before the service, and the
// routes answer 503 without a keyring service.
func TestReplicaKeyRoutes(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	wantError(t, admin.do("GET", "/api/v1/settings/replicas", nil), http.StatusServiceUnavailable, errUnavailable)

	fr := &fakeReplicas{}
	cf.srv.cfg.Replicas = fr
	op := cf.loggedIn("op", authz.RoleOperator, authz.Global)
	wantError(t, op.do("GET", "/api/v1/settings/replicas", nil), http.StatusForbidden, errForbidden)
	wantError(t, op.do("PUT", "/api/v1/settings/replicas/m2/data-key-recipient", map[string]string{"publicKey": "dbcmpk1:x"}),
		http.StatusForbidden, errForbidden)

	var st KeyringStatus
	r := admin.do("GET", "/api/v1/settings/replicas", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &st)
	if st.CurrentKey != "dk-1" || len(st.Replicas) != 2 || !st.Replicas[0].HasCurrentKey {
		t.Fatalf("%+v", st)
	}
	wantError(t, admin.do("PUT", "/api/v1/settings/replicas/bad%2Fid/data-key-recipient", map[string]string{"publicKey": "dbcmpk1:x"}),
		http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("PUT", "/api/v1/settings/replicas/m2/data-key-recipient", map[string]string{}), http.StatusBadRequest, errBadRequest)
	allowed := auditsOf(cf.fixture, authz.ActionUpdate, authz.OutcomeAllowed, authz.ResourceSettings)
	var rk ReplicaKeyStatus
	r = admin.do("PUT", "/api/v1/settings/replicas/m2/data-key-recipient", map[string]string{"publicKey": "dbcmpk1:x"})
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &rk)
	if rk.ID != "m2" || !rk.HasCurrentKey {
		t.Fatalf("%+v", rk)
	}
	if auditsOf(cf.fixture, authz.ActionUpdate, authz.OutcomeAllowed, authz.ResourceSettings) <= allowed {
		t.Fatal("registration not audited")
	}
	if auditsOf(cf.fixture, authz.ActionUpdate, authz.OutcomeDenied, authz.ResourceSettings) == 0 {
		t.Fatal("denied registration not audited")
	}
	fr.err = ErrInvalidInput
	wantError(t, admin.do("PUT", "/api/v1/settings/replicas/m9/data-key-recipient", map[string]string{"publicKey": "dbcmpk1:x"}),
		http.StatusBadRequest, errBadRequest)
	fr.err = ErrNotLeader
	wantError(t, admin.do("PUT", "/api/v1/settings/replicas/m2/data-key-recipient", map[string]string{"publicKey": "dbcmpk1:x"}),
		http.StatusServiceUnavailable, errNotLeader)
	if len(fr.calls) != 3 {
		t.Fatalf("calls %v", fr.calls)
	}
}
