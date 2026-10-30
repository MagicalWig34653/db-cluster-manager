// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

const (
	CollProvisionPlans                = "provision-plans"
	CollProvisionOwnership            = "provision-ownership"
	provisionCommand       agent.Kind = "provision-host"
	provisionKind                     = "provision/swarm"
)

// ProvisionDraft is sealed as one document, including every SSH credential.
type ProvisionDraft struct {
	Plan        provision.Plan                  `json:"plan"`
	Credentials map[string]provision.Credential `json:"credentials"`
}

// ProvisionOwnership is the durable cleanup inventory. Only entries whose
// cluster identity was observed after a successful step are added.
type ProvisionOwnership struct {
	PlanID    string          `json:"planId"`
	Digest    string          `json:"digest"`
	ClusterID string          `json:"clusterId"`
	Hosts     map[string]bool `json:"hosts"`
}

type Provisioning struct {
	Ops             *Operations
	Keys            KeyProvider
	Commands        provision.CommandRunner
	AllowKubernetes func() bool
}

func (p *Provisioning) Plan(ctx context.Context, req provision.Request, creds map[string]provision.Credential) (provision.Plan, error) {
	if _, err := p.Ops.leadership(); err != nil {
		return provision.Plan{}, err
	}
	if err := req.Validate(p.AllowKubernetes()); err != nil {
		return provision.Plan{}, err
	}
	if p.Keys == nil || p.Commands == nil || len(creds) != len(req.Hosts) {
		return provision.Plan{}, provision.ErrInvalid
	}
	checks := make([]provision.Check, 0, len(req.Hosts))
	for _, h := range req.Hosts {
		cred, ok := creds[h.ID]
		if !ok || cred.PrivateKey == "" || cred.KnownHosts == "" {
			return provision.Plan{}, provision.ErrInvalid
		}
		c, err := provision.Inspect(ctx, p.Commands, h, cred, req.Platform)
		if err != nil {
			return provision.Plan{}, err
		}
		checks = append(checks, c)
	}
	plan, err := provision.Build(req, checks, p.AllowKubernetes())
	if err != nil {
		return provision.Plan{}, err
	}
	sealed, err := seal(p.Keys, CollProvisionPlans, plan.ID, ProvisionDraft{Plan: plan, Credentials: creds})
	if err != nil {
		return provision.Plan{}, err
	}
	zero := uint64(0)
	if _, err = Put(p.Ops.Svc.store, CollProvisionPlans, plan.ID, sealed, &zero); err != nil {
		if errors.Is(err, ErrConflict) {
			return provision.Plan{}, provision.ErrConflict
		}
		return provision.Plan{}, err
	}
	return plan, nil
}

// Start stores the exact approved operation definition before the runner
// starts. All mutating steps wait for a separate digest-bound approval.
func (p *Provisioning) Start(actor authz.Subject, id, digest string) (api.OperationRef, error) {
	l, err := p.Ops.leadership()
	if err != nil {
		return api.OperationRef{}, err
	}
	if err := p.Ops.authorize(actor, authz.ActionOperate, authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}); err != nil {
		return api.OperationRef{}, err
	}
	draft, ok, err := GetSealed[ProvisionDraft](p.Ops.Svc.store, p.Keys, CollProvisionPlans, id, Consistent())
	if err != nil {
		return api.OperationRef{}, err
	}
	if !ok || digest == "" || digest != draft.Plan.Digest {
		return api.OperationRef{}, api.ErrApprovalMismatch
	}
	if _, owned, err := Get[ProvisionOwnership](p.Ops.Svc.store, CollProvisionOwnership, id, Consistent()); err != nil {
		return api.OperationRef{}, err
	} else if owned {
		return api.OperationRef{}, provision.ErrExistingCluster
	}
	if draft.Plan.Platform != provision.Swarm {
		return api.OperationRef{}, errors.New("provision: Kubernetes execution disabled")
	}
	opID := newID("op")
	approvalID := newID("apr")
	op := runner.Operation{ID: opID, Kind: provisionKind, Locks: []string{"provision/" + id}}
	for _, step := range draft.Plan.Steps {
		args, _ := json.Marshal(struct {
			PlanID string `json:"planId"`
			StepID string `json:"stepId"`
		}{id, step.ID})
		op.Steps = append(op.Steps, runner.Step{ID: step.ID, Target: "provision/" + id + "/" + step.HostID, Command: provisionCommand, Args: args, Preconditions: []runner.Condition{{Kind: runner.CondApproved, Target: approvalID}}, MaxRetries: 0})
	}
	if err := op.Validate(); err != nil {
		return api.OperationRef{}, err
	}
	defDigest, err := DefinitionDigest(op)
	if err != nil {
		return api.OperationRef{}, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, RequestedBy: actor.ID, RequestedAt: p.Ops.Svc.cfg.Now(), Definition: op, ApprovalID: approvalID, ApprovalDigest: defDigest}
	if err := p.Ops.commit(rec, nil); err != nil {
		return api.OperationRef{}, err
	}
	p.Ops.start(l, op, nil)
	return api.OperationRef{ID: rec.ID, Kind: rec.Kind}, nil
}

// ProvisionAgentClient dispatches only provision-host commands. It is wrapped
// by the existing term-fenced runner client, so a former leader cannot send.
type ProvisionAgentClient struct {
	Next     runner.AgentClient
	Store    *Store
	Keys     KeyProvider
	Commands provision.CommandRunner
}

func (c ProvisionAgentClient) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	if cmd.Kind != provisionCommand {
		if c.Next == nil {
			return agent.Result{}, errNoAgents
		}
		return c.Next.Send(ctx, target, cmd)
	}
	parts := strings.Split(target, "/")
	if len(parts) != 3 || parts[0] != "provision" {
		return agent.Result{}, agent.ErrInvalidCommand
	}
	var args struct {
		PlanID string `json:"planId"`
		StepID string `json:"stepId"`
	}
	if json.Unmarshal(cmd.Args, &args) != nil || args.PlanID != parts[1] {
		return agent.Result{}, agent.ErrInvalidCommand
	}
	draft, ok, err := GetSealed[ProvisionDraft](c.Store, c.Keys, CollProvisionPlans, args.PlanID, Consistent())
	if err != nil || !ok {
		return agent.Result{}, fmt.Errorf("%w: plan unavailable", agent.ErrInvalidCommand)
	}
	if draft.Plan.Platform != provision.Swarm {
		return agent.Result{}, agent.ErrInvalidCommand
	}
	var step *provision.Step
	for i := range draft.Plan.Steps {
		if draft.Plan.Steps[i].ID == args.StepID && draft.Plan.Steps[i].HostID == parts[2] {
			step = &draft.Plan.Steps[i]
			break
		}
	}
	if step == nil {
		return agent.Result{}, agent.ErrInvalidCommand
	}
	result := agent.Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: agent.StatusSucceeded}
	owned, has, err := Get[ProvisionOwnership](c.Store, CollProvisionOwnership, args.PlanID, Consistent())
	if err != nil {
		return agent.Result{}, err
	}
	if has && owned.Value.Hosts[step.HostID] {
		if owned.Value.Digest != draft.Plan.Digest {
			return agent.Result{}, agent.ErrInvalidCommand
		}
		host := draft.Plan.Hosts[0]
		for _, h := range draft.Plan.Hosts {
			if h.ID == step.HostID {
				host = h
				break
			}
		}
		observed, err := c.Commands.Run(ctx, host, draft.Credentials[host.ID], "docker info --format '{{.Swarm.Cluster.ID}}'", "")
		if err != nil || strings.TrimSpace(observed) != owned.Value.ClusterID {
			result.Status = agent.StatusUnknown
		}
		return result, nil
	}
	if err := provision.ApplyStep(ctx, c.Commands, draft.Plan, *step, draft.Credentials); err != nil {
		result.Status = agent.StatusRefused
		if errors.Is(err, provision.ErrOutcomeUnknown) || errors.Is(err, provision.ErrExistingCluster) {
			result.Status = agent.StatusUnknown
		}
		result.Message = "host preflight or provisioning step failed"
		return result, nil
	}
	// Inventory is retained for later scoped cleanup. If recording fails,
	// completion is unknown: the runner must pause and reconcile.
	clusterID, err := c.Commands.Run(ctx, draft.Plan.Hosts[0], draft.Credentials[draft.Plan.Hosts[0].ID], "docker info --format '{{.Swarm.Cluster.ID}}'", "")
	if err != nil || strings.TrimSpace(clusterID) == "" {
		result.Status = agent.StatusUnknown
		return result, nil
	}
	_, _, err = Update(c.Store, CollProvisionOwnership, args.PlanID, func(v ProvisionOwnership, exists bool) (ProvisionOwnership, error) {
		if exists && (v.Digest != draft.Plan.Digest || v.ClusterID != strings.TrimSpace(clusterID)) {
			return v, provision.ErrExistingCluster
		}
		if !exists {
			v = ProvisionOwnership{PlanID: args.PlanID, Digest: draft.Plan.Digest, ClusterID: strings.TrimSpace(clusterID), Hosts: map[string]bool{}}
		}
		v.Hosts[step.HostID] = true
		return v, nil
	})
	if err != nil {
		result.Status = agent.StatusUnknown
	}
	return result, nil
}
