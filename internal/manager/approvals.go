// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Explicit approvals of destructive operation steps.
//
// An operation that needs an approval (restores) is planned with an
// approval ID; every destructive step carries the condition
// CondApproved(approval ID). The operation record stores the ID and the
// digest of the definition. An approval is recorded only for that digest,
// and the approval store re-derives the digest from the stored definition
// on every check, so an approval can never cover a plan other than the one
// that was shown. Approving needs the restore permission on the cluster's
// backups and is audited by the Authorizer.

// CollApprovals holds approval records; key: approval ID.
const CollApprovals = "approvals"

// Errors of approvals.
var (
	// ErrNoApproval: the operation does not ask for an approval.
	ErrNoApproval = errors.New("manager: operation does not ask for an approval")
	// ErrApprovalMismatch: the digest is not the operation's.
	ErrApprovalMismatch = errors.New("manager: approval digest does not match the operation definition")
	// ErrOperationEnded: the operation has ended.
	ErrOperationEnded = errors.New("manager: operation has ended")
)

// ApprovalRecord is one recorded approval.
type ApprovalRecord struct {
	ID       string    `json:"id"`
	OpID     string    `json:"opId"`
	Digest   string    `json:"digest"`
	By       string    `json:"by"`
	Approved time.Time `json:"approved"`
}

// DefinitionDigest is the SHA-256 (hex) of an operation definition's JSON
// after a round trip through its stored form.
func DefinitionDigest(op runner.Operation) (string, error) {
	b, err := json.Marshal(op)
	if err != nil {
		return "", err
	}
	var rt runner.Operation
	if err := json.Unmarshal(b, &rt); err != nil {
		return "", err
	}
	if b, err = json.Marshal(rt); err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// StoreApprovals implements dbbackup.ApprovalStore over the replicated
// store. An approval counts only while its digest equals the digest of the
// operation's stored definition and the operation names it.
type StoreApprovals struct {
	Store *Store
}

var _ dbbackup.ApprovalStore = StoreApprovals{}

// Approval implements dbbackup.ApprovalStore.
func (a StoreApprovals) Approval(id string) (dbbackup.Approval, bool) {
	rec, ok, err := Get[ApprovalRecord](a.Store, CollApprovals, id)
	if err != nil || !ok || rec.Value.By == "" {
		return dbbackup.Approval{}, false
	}
	op, ok, err := Get[OperationRecord](a.Store, CollOperations, rec.Value.OpID)
	if err != nil || !ok || op.Value.ApprovalID != id {
		return dbbackup.Approval{}, false
	}
	d, err := DefinitionDigest(op.Value.Definition)
	if err != nil || d != rec.Value.Digest || d != op.Value.ApprovalDigest {
		return dbbackup.Approval{}, false
	}
	return dbbackup.Approval{ID: id, Digest: d, By: rec.Value.By, Approved: rec.Value.Approved}, true
}

func backupRes(swarm, cluster string) authz.Resource {
	return authz.Resource{Type: authz.ResourceBackup, Scope: authz.ClusterScope(swarm, cluster)}
}

// locatedRes is the resource an operation is located at (cluster, else
// Swarm, else manager settings).
func locatedRes(swarm, cluster string) authz.Resource {
	switch {
	case swarm != "" && cluster != "":
		return clusterRes(swarm, cluster)
	case swarm != "":
		return authz.Resource{Type: authz.ResourceSwarm, Scope: authz.SwarmScope(swarm)}
	}
	return authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}
}

// approvalPermission is the permission an approval needs: delete on the
// cluster for a cluster deletion, restore on the cluster's backups
// otherwise (manager settings for unlocated operations). The API checks the
// same mapping (api.ApprovalPermission).
func approvalPermission(rec OperationRecord) (authz.Action, authz.Resource) {
	return api.ApprovalPermission(rec.Kind, rec.Swarm, rec.Cluster)
}

// Approve records actor's approval of the operation's destructive steps for
// digest and resumes the operation. It needs the permission of
// approvalPermission (audited).
func (o *Operations) Approve(actor authz.Subject, opID, digest string) (ApprovalRecord, error) {
	l, err := o.leadership()
	if err != nil {
		return ApprovalRecord{}, err
	}
	cur, ok, err := Get[OperationRecord](o.Svc.store, CollOperations, opID, Consistent())
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !ok {
		return ApprovalRecord{}, fmt.Errorf("%w: operation %q", api.ErrNotFound, opID)
	}
	rec := cur.Value
	act, res := approvalPermission(rec)
	if err := o.authorize(actor, act, res); err != nil {
		return ApprovalRecord{}, err
	}
	if rec.ApprovalID == "" {
		return ApprovalRecord{}, ErrNoApproval
	}
	if !rec.EndedAt.IsZero() {
		return ApprovalRecord{}, ErrOperationEnded
	}
	want, err := DefinitionDigest(rec.Definition)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if digest != want || digest != rec.ApprovalDigest {
		return ApprovalRecord{}, ErrApprovalMismatch
	}
	a := ApprovalRecord{ID: rec.ApprovalID, OpID: opID, Digest: digest, By: actor.ID, Approved: o.Svc.cfg.Now()}
	if prev, ok, err := Get[ApprovalRecord](o.Svc.store, CollApprovals, a.ID, Consistent()); err != nil {
		return ApprovalRecord{}, err
	} else if ok {
		return prev.Value, nil // already approved; the first approval stands
	}
	if _, err := Put(o.Svc.store, CollApprovals, a.ID, a, IfVersion(0)); err != nil {
		return ApprovalRecord{}, err
	}
	o.Svc.recordEvent(rec.Swarm, rec.Cluster, api.SeverityWarning,
		fmt.Sprintf("%s approved the destructive steps of operation %s (%s)", actor.ID, opID, rec.Kind))
	// A paused operation continues; a running one sees the approval at its
	// next precondition poll.
	l.Go(func(ctx context.Context) {
		v, err := l.Runner.Resume(ctx, opID)
		if err != nil && v.ID == "" {
			return
		}
		_ = o.Svc.saveView(v)
	})
	return a, nil
}

// ErrAbortLeavesNodeStopped: the operation stopped a database service
// (scaled it to 0) and has not started it again. Aborting now would leave
// that node down with no operation left to bring it back.
var ErrAbortLeavesNodeStopped = errors.New("manager: the operation stopped a node that it has not started again; resume it, or start the node by hand, before aborting")

// nodeLeftStopped reports a node-restarting Galera operation (rolling
// restart, minor upgrade, scale-in) in which a stop-db step ran (or may have
// run) and no later start-service step succeeded.
func nodeLeftStopped(rec OperationRecord, r *runner.Runner) bool {
	switch rec.Kind {
	case "galera/rolling-restart", "galera/minor-upgrade", "galera/scale-in", "galera/scale-out":
	default:
		return false
	}
	view := rec.View
	if v, err := r.Snapshot(rec.ID); err == nil {
		view = &v
	}
	if view == nil {
		return false
	}
	state := map[string]opjournal.State{}
	for _, sv := range view.Steps {
		state[sv.ID] = sv.State
	}
	down := false
	for _, st := range rec.Definition.Steps {
		switch st.Command {
		case agent.KindStopDB:
			switch state[st.ID] {
			case opjournal.Succeeded, opjournal.Running, opjournal.Unknown:
				down = true
			}
		case agent.KindStartService:
			if state[st.ID] == opjournal.Succeeded {
				down = false
			}
		case agent.KindRemoveService:
			// Scale-in removes the stopped service on purpose.
			if state[st.ID] == opjournal.Succeeded {
				down = false
			}
		}
	}
	return down
}

// neverStarted reports a record whose start was refused: stopped without any
// step, or without a view at all.
func neverStarted(rec OperationRecord) bool {
	return rec.View == nil || (rec.View.Status == runner.StatusStopped && len(rec.View.Steps) == 0)
}

// Abort ends an operation at a safe step boundary (runner.Runner.Abort).
// It needs operate permission where the operation is located (audited).
func (o *Operations) Abort(actor authz.Subject, opID string) error {
	l, err := o.leadership()
	if err != nil {
		return err
	}
	cur, ok, err := Get[OperationRecord](o.Svc.store, CollOperations, opID, Consistent())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: operation %q", api.ErrNotFound, opID)
	}
	if err := o.authorize(actor, authz.ActionOperate, locatedRes(cur.Value.Swarm, cur.Value.Cluster)); err != nil {
		return err
	}
	if !cur.Value.EndedAt.IsZero() {
		// A failed operation counts as ended for the inventory, but the
		// runner keeps its locks until it is aborted (lab E4: a failed
		// inspection kept its backup locked and could not be aborted).
		v, err := l.Runner.Snapshot(opID)
		if err != nil || v.Status == runner.StatusSucceeded || v.Status == runner.StatusAborted {
			return ErrOperationEnded
		}
	}
	if nodeLeftStopped(cur.Value, l.Runner) {
		return ErrAbortLeavesNodeStopped
	}
	if err := l.Runner.Abort(opID); err != nil {
		if errors.Is(err, runner.ErrUnknownOperation) && neverStarted(cur.Value) {
			// The start was refused before the runner journaled anything
			// (shown as "unknown"): there is nothing to stop, only the
			// record to close.
			v := runner.View{ID: opID, Kind: cur.Value.Kind, Status: runner.StatusAborted, Term: l.Term,
				Reason: "discarded by an operator before it started", Locks: cur.Value.Definition.Locks}
			if cur.Value.View != nil {
				v.Term = cur.Value.View.Term
			}
			if err := o.Svc.saveView(v); err != nil {
				return err
			}
			o.Svc.recordEvent(cur.Value.Swarm, cur.Value.Cluster, api.SeverityWarning, fmt.Sprintf("%s aborted operation %s", actor.ID, opID))
			return nil
		}
		return err
	}
	if v, err := l.Runner.Snapshot(opID); err == nil {
		_ = o.Svc.saveView(v)
	}
	o.Svc.recordEvent(cur.Value.Swarm, cur.Value.Cluster, api.SeverityWarning, fmt.Sprintf("%s aborted operation %s", actor.ID, opID))
	return nil
}
