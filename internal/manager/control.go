// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Control implements api.Control on top of Operations and Swarms. The API
// has already authorized and audited each call; Operations and Swarms
// check again (and audit again) as defence in depth, so every command has
// two audit records: the API's and the manager's.
type Control struct {
	Ops    *Operations
	Swarms *Swarms
}

var _ api.Control = (*Control)(nil)

func (c *Control) swarms() *Swarms {
	sw := *c.Swarms
	sw.Svc, sw.Authorizer = c.Ops.Svc, c.Ops.Authorizer
	return &sw
}

func toAPIHost(h HostRecord) api.Host {
	return api.Host{Swarm: h.Swarm, NodeID: h.NodeID, Hostname: h.Hostname, Role: h.Role, Availability: h.Availability,
		State: h.State, EngineVersion: h.EngineVersion, Enrollment: string(h.Enrollment), Spare: h.Spare,
		NanoCPUs: h.NanoCPUs, MemoryBytes: h.MemoryBytes, ObservedAt: h.ObservedAt, AgentIdentity: h.AgentSerial != ""}
}

// Hosts implements api.Control (local read).
func (c *Control) Hosts(context.Context) ([]api.Host, error) {
	recs, err := List[HostRecord](c.Ops.Svc.store, CollHosts)
	if err != nil {
		return nil, MapCommandError(err)
	}
	out := make([]api.Host, 0, len(recs))
	for _, r := range recs {
		out = append(out, toAPIHost(r.Value))
	}
	return out, nil
}

// ConnectSwarm implements api.Control.
func (c *Control) ConnectSwarm(_ context.Context, actor authz.Subject, in api.SwarmInput) (api.Swarm, error) {
	if _, err := c.Ops.leadership(); err != nil {
		return api.Swarm{}, MapCommandError(err)
	}
	rec, err := c.swarms().Connect(actor, in.Name, in.DockerHost)
	if err != nil {
		return api.Swarm{}, MapCommandError(err)
	}
	return api.Swarm{ID: rec.ID, Name: rec.Name, State: api.StateUnknown}, nil
}

// SyncSwarm implements api.Control.
func (c *Control) SyncSwarm(ctx context.Context, _ authz.Subject, id string) error {
	if _, err := c.Ops.leadership(); err != nil {
		return MapCommandError(err)
	}
	return MapCommandError(c.swarms().Sync(ctx, id))
}

// DeployAgents implements api.Control.
func (c *Control) DeployAgents(ctx context.Context, actor authz.Subject, id string) (api.AgentDeployment, error) {
	if _, err := c.Ops.leadership(); err != nil {
		return api.AgentDeployment{}, MapCommandError(err)
	}
	res, err := c.swarms().DeployAgents(ctx, actor, id)
	if err != nil && res.ServiceID == "" {
		return api.AgentDeployment{}, MapCommandError(err)
	}
	// A partial deployment (some labels failed) is reported, not hidden.
	failed := make([]string, 0, len(res.Failed))
	for id := range res.Failed {
		failed = append(failed, id) // node IDs only; error text stays in the log
	}
	slices.Sort(failed)
	if err != nil {
		c.Ops.Svc.log.Warn("manager: agent deployment incomplete", "swarm", id, "err", err)
	}
	return api.AgentDeployment{ServiceID: res.ServiceID, Created: res.Created, Labeled: res.Labeled,
		Unlabeled: res.Unlabeled, Failed: failed}, nil
}

// SetEnrollment implements api.Control. A lab agent URL already recorded
// for the host is kept.
func (c *Control) SetEnrollment(_ context.Context, actor authz.Subject, swarmID, nodeID string, enroll bool) (api.Host, error) {
	if _, err := c.Ops.leadership(); err != nil {
		return api.Host{}, MapCommandError(err)
	}
	cur, _, err := Get[HostRecord](c.Ops.Svc.store, CollHosts, HostKey(swarmID, nodeID), Consistent())
	if err != nil {
		return api.Host{}, MapCommandError(err)
	}
	h, err := c.swarms().SetEnrollment(actor, swarmID, nodeID, enroll, cur.Value.AgentURL)
	if err != nil {
		return api.Host{}, MapCommandError(err)
	}
	return toAPIHost(h), nil
}

// CreateCluster implements api.Control.
func (c *Control) CreateCluster(_ context.Context, actor authz.Subject, req api.CreateClusterRequest) (api.OperationRef, error) {
	rec, err := c.Ops.CreateCluster(actor, CreateClusterRequest(req))
	if err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	return api.OperationRef{ID: rec.ID, Kind: rec.Kind}, nil
}

// ReplaceNode implements api.Control.
func (c *Control) ReplaceNode(_ context.Context, actor authz.Subject, clusterID, slot string) (api.OperationRef, error) {
	rec, err := c.Ops.ReplaceNode(actor, clusterID, slot)
	if err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	return api.OperationRef{ID: rec.ID, Kind: rec.Kind}, nil
}

// OutageDiagnosis implements api.Control: read-only evidence collection
// (grastate through the agents plus the last health observations) and
// diagnosis. Nothing is started.
func (c *Control) OutageDiagnosis(ctx context.Context, actor authz.Subject, clusterID string) (api.Diagnosis, error) {
	d, _, err := c.Ops.DiagnoseOutageCollected(ctx, actor, clusterID)
	if err != nil {
		return api.Diagnosis{}, MapCommandError(err)
	}
	return api.Diagnosis{Candidate: d.Candidate, Positions: d.Positions, Reasons: d.Reasons, Risks: d.Risks,
		RequiresApproval: true}, nil
}

// OutageDiagnosisRecover implements api.Control: the diagnosis with
// mariadbd --wsrep-recover on nodes without a saved seqno (operate
// permission). Nothing is started.
func (c *Control) OutageDiagnosisRecover(ctx context.Context, actor authz.Subject, clusterID string) (api.Diagnosis, error) {
	d, _, err := c.Ops.DiagnoseOutageRecovering(ctx, actor, clusterID)
	if err != nil {
		return api.Diagnosis{}, MapCommandError(err)
	}
	return api.Diagnosis{Candidate: d.Candidate, Positions: d.Positions, Reasons: d.Reasons, Risks: d.Risks,
		RequiresApproval: true}, nil
}

// OutageRecovery implements api.Control: guided full-outage recovery that
// waits for an explicit approval (recovery.go).
func (c *Control) OutageRecovery(ctx context.Context, actor authz.Subject, clusterID, candidate string) (api.RecoveryStarted, error) {
	rec, d, err := c.Ops.RecoverOutage(ctx, actor, clusterID, candidate)
	if err != nil {
		return api.RecoveryStarted{}, MapCommandError(err)
	}
	return api.RecoveryStarted{ID: rec.ID, Kind: rec.Kind, Candidate: d.Candidate,
		Approval: api.OperationApproval{Digest: rec.ApprovalDigest},
		Diagnosis: api.Diagnosis{Candidate: d.Candidate, Positions: d.Positions, Reasons: d.Reasons, Risks: d.Risks,
			RequiresApproval: true}}, nil
}

// ResumeOperation implements api.Control.
func (c *Control) ResumeOperation(_ context.Context, actor authz.Subject, opID string) error {
	return MapCommandError(c.Ops.Resume(actor, opID))
}

// AbortOperation implements api.Control.
func (c *Control) AbortOperation(_ context.Context, actor authz.Subject, opID string) error {
	return MapCommandError(c.Ops.Abort(actor, opID))
}

// ApproveOperation implements api.Control.
func (c *Control) ApproveOperation(_ context.Context, actor authz.Subject, opID, digest string) (api.OperationApproval, error) {
	a, err := c.Ops.Approve(actor, opID, digest)
	if err != nil {
		return api.OperationApproval{}, MapCommandError(err)
	}
	return api.OperationApproval{Digest: a.Digest, ApprovedBy: a.By, ApprovedAt: a.Approved}, nil
}

// MapCommandError maps manager, planner and runner errors to the API's
// sentinel errors. The original error stays in the chain's message for the
// server log only; the API never returns error text.
func MapCommandError(err error) error {
	if err == nil {
		return nil
	}
	var denied *authz.DeniedError
	var sentinel error
	switch {
	case errors.As(err, &denied):
		sentinel = api.ErrForbidden
	case errors.Is(err, galera.ErrRequiresGuidedRecovery):
		sentinel = api.ErrRequiresGuidedRecovery
	case errors.Is(err, galera.ErrNoCapacity):
		sentinel = api.ErrNoCapacity
	case errors.Is(err, galera.ErrPolicyConflict):
		sentinel = api.ErrPolicyConflict
	case errors.Is(err, ErrSettingsIncomplete), errors.Is(err, ErrNoInstallation), errors.Is(err, errNoDial), errors.Is(err, ErrNoServiceHost):
		sentinel = api.ErrSettingsIncomplete
	case errors.Is(err, ErrClusterExists):
		sentinel = api.ErrClusterExists
	case errors.Is(err, runner.ErrBusy), errors.Is(err, runner.ErrActive):
		sentinel = api.ErrBusy
	case errors.Is(err, ErrApprovalMismatch), errors.Is(err, dbbackup.ErrApprovalMismatch):
		sentinel = api.ErrApprovalMismatch
	case errors.Is(err, ErrNotLeader), errors.Is(err, runner.ErrNotLeader), errors.Is(err, runner.ErrLostLeadership),
		errors.Is(err, metastore.ErrNotLeader):
		sentinel = api.ErrNotLeader
	case errors.Is(err, api.ErrNotFound), errors.Is(err, ErrNotFound), errors.Is(err, runner.ErrUnknownOperation),
		errors.Is(err, galera.ErrUnknownNode):
		sentinel = api.ErrNotFound
	case errors.Is(err, ErrNoApproval), errors.Is(err, ErrOperationEnded), errors.Is(err, runner.ErrFinished),
		errors.Is(err, runner.ErrNotPaused), errors.Is(err, galera.ErrNodeNotFailed), errors.Is(err, galera.ErrNotFullOutage),
		errors.Is(err, galera.ErrReplacementPaused), errors.Is(err, ErrMissingServiceID), errors.Is(err, ErrConflict),
		errors.Is(err, galera.ErrNoCandidate), errors.Is(err, ErrCandidateMismatch),
		errors.Is(err, dbbackup.ErrNoSource), errors.Is(err, dbbackup.ErrProductionTarget), errors.Is(err, dbbackup.ErrIdentityMismatch):
		sentinel = api.ErrConflict
	case errors.Is(err, galera.ErrInvalidTopology), errors.Is(err, galera.ErrInvalidSpec), errors.Is(err, ErrUnknownReference), errors.Is(err, dbbackup.ErrInvalidRequest),
		errors.Is(err, dbbackup.ErrSchedule), errors.Is(err, ErrInvalidSettings), errors.Is(err, dbbackup.ErrNoNextRun), errors.Is(err, dbbackup.ErrInvalidManifest):
		sentinel = api.ErrInvalidInput
	default:
		return err
	}
	return fmt.Errorf("%w (%v)", sentinel, err)
}
