// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

type fakeZeroUUID struct {
	*fakeControl
	starts, sets int
	err          error
	enabled      bool
}

func (f *fakeZeroUUID) StartZeroUUIDRecovery(context.Context, authz.Subject, string) (ZeroUUIDRecoveryStarted, error) {
	f.starts++
	return ZeroUUIDRecoveryStarted{ID: "op-prep", Kind: OperationKindZeroUUIDPrepare, Snapshot: "pre-recovery-op-prep", Approval: OperationApproval{Digest: digestA}}, f.err
}

func (f *fakeZeroUUID) ZeroUUIDRecoveryStatus(context.Context, authz.Subject, string) (ZeroUUIDRecoveryStatus, error) {
	return ZeroUUIDRecoveryStatus{AutoRecover: f.enabled, Prepare: &RecoveryPhase{ID: "op-prep", Kind: OperationKindZeroUUIDPrepare, State: "pending", AwaitingApproval: true}}, nil
}

func (f *fakeZeroUUID) SetAutoRecover(_ context.Context, _ authz.Subject, _ string, enabled bool) (AutoRecoverPolicy, error) {
	f.sets++
	f.enabled = enabled
	return AutoRecoverPolicy{Enabled: enabled}, nil
}

func TestZeroUUIDRecoveryAuthorizationAndContract(t *testing.T) {
	cf := newControlFixture(t)
	ctl := &fakeZeroUUID{fakeControl: cf.ctl}
	cf.srv.cfg.Control = ctl
	viewer := cf.loggedIn("viewer", authz.RoleViewer, authz.SwarmScope("s1"))
	operator := cf.loggedIn("operator", authz.RoleOperator, authz.SwarmScope("s1"))
	admin := cf.loggedIn("admin", authz.RoleAdministrator, authz.SwarmScope("s1"))

	// Viewer reads the status, cannot propose or switch the policy.
	var st ZeroUUIDRecoveryStatus
	r := viewer.do("GET", "/api/v1/clusters/c1/zero-uuid-recovery", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &st)
	if st.Prepare == nil || !st.Prepare.AwaitingApproval {
		t.Fatalf("status %+v", st)
	}
	wantError(t, viewer.do("POST", "/api/v1/clusters/c1/zero-uuid-recovery", nil), http.StatusForbidden, errForbidden)
	wantError(t, viewer.do("PUT", "/api/v1/clusters/c1/auto-recover", map[string]any{"enabled": true}), http.StatusForbidden, errForbidden)
	if ctl.starts != 0 || ctl.sets != 0 {
		t.Fatal("denied request reached control")
	}

	// Operator proposes; the policy and the approval stay with the administrator.
	var started ZeroUUIDRecoveryStarted
	r = operator.do("POST", "/api/v1/clusters/c1/zero-uuid-recovery", nil)
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &started)
	if started.ID != "op-prep" || started.Approval.Digest != digestA || started.Snapshot == "" {
		t.Fatalf("started %+v", started)
	}
	wantError(t, operator.do("PUT", "/api/v1/clusters/c1/auto-recover", map[string]any{"enabled": true}), http.StatusForbidden, errForbidden)
	wantError(t, operator.do("POST", "/api/v1/clusters/c2/zero-uuid-recovery", nil), http.StatusNotFound, errNotFound)
	for _, kind := range []string{OperationKindZeroUUIDPrepare, OperationKindZeroUUIDRecovery} {
		if act, res := ApprovalPermission(kind, "s1", "c1"); act != authz.ActionRecover || res.Type != authz.ResourceCluster {
			t.Fatalf("approval permission of %s: %s %+v", kind, act, res)
		}
	}

	var pol AutoRecoverPolicy
	r = admin.do("PUT", "/api/v1/clusters/c1/auto-recover", map[string]any{"enabled": true})
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &pol)
	if !pol.Enabled || ctl.sets != 1 {
		t.Fatalf("policy %+v", pol)
	}
	wantError(t, admin.do("PUT", "/api/v1/clusters/c1/auto-recover", map[string]any{"enabled": true, "x": 1}), http.StatusBadRequest, errBadRequest)

	// A real Primary component is a stable conflict reason.
	ctl.err = ErrPrimaryComponent
	var body ErrorBody
	r = operator.do("POST", "/api/v1/clusters/c1/zero-uuid-recovery", nil)
	wantStatus(t, r, http.StatusConflict)
	r.decode(t, &body)
	if body.Reason != "primary_component_exists" || body.Offer != "" {
		t.Fatalf("body %+v", body)
	}
}

func TestZeroUUIDPrimaryConflictOffersTheRecovery(t *testing.T) {
	cf := newControlFixture(t)
	cf.ctl.err = ErrZeroUUIDPrimary
	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	r := op.do("POST", "/api/v1/clusters/c1/outage-diagnosis", nil)
	wantStatus(t, r, http.StatusConflict)
	var body ErrorBody
	r.decode(t, &body)
	if body.Reason != "zero_cluster_uuid_primary" || body.Offer != "zero-uuid-recovery" {
		t.Fatalf("body %+v", body)
	}
}
