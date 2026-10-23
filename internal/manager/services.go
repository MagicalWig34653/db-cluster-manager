// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

// Managed services of a service type (W07; fakes only, never run against a
// Swarm). Creating and deleting a service reuses the operation machinery:
// the plan of the service type becomes a runner operation whose
// state-changing steps wait for an approval bound to the digest of the
// definition (approvals.go), the record and the operation are stored in one
// transaction, and every call is authorized and audited by the Authorizer.
// Planning is pure (internal/service); a plan with blockers is previewed
// but never started.

// CollServices holds service records; key: service ID.
const CollServices = "services"

// ErrServiceExists: a service of that name exists in the Swarm.
var ErrServiceExists = errors.New("manager: service name already used in this swarm")

// ServiceRecord is the desired state of one managed service.
type ServiceRecord struct {
	ID        string          `json:"id"`
	Swarm     string          `json:"swarm"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Spec      json.RawMessage `json:"spec"`
	CreatedBy string          `json:"createdBy"`
	CreatedAt time.Time       `json:"createdAt"`
	CreateOp  string          `json:"createOp,omitempty"`
	DeleteOp  string          `json:"deleteOp,omitempty"`
}

// Services implements api.Services on top of Operations.
type Services struct {
	Ops      *Operations
	Registry *service.Registry
}

var _ api.Services = (*Services)(nil)

func (s *Services) registry() *service.Registry {
	if s.Registry != nil {
		return s.Registry
	}
	return service.DefaultRegistry()
}

func serviceRes(swarm, id string) authz.Resource {
	return authz.Resource{Type: authz.ResourceService, Scope: authz.SwarmScope(swarm), ID: id}
}

func toAPIService(r ServiceRecord) api.ServiceInfo {
	return api.ServiceInfo{ID: r.ID, Swarm: r.Swarm, Name: r.Name, Type: r.Type, Spec: r.Spec, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt, CreateOperation: r.CreateOp, DeleteOperation: r.DeleteOp}
}

// Services implements api.Services (local read).
func (s *Services) Services(context.Context) ([]api.ServiceInfo, error) {
	recs, err := List[ServiceRecord](s.Ops.Svc.store, CollServices)
	if err != nil {
		return nil, MapCommandError(err)
	}
	out := make([]api.ServiceInfo, 0, len(recs))
	for _, r := range recs {
		out = append(out, toAPIService(r.Value))
	}
	slices.SortFunc(out, func(a, b api.ServiceInfo) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// Service implements api.Services.
func (s *Services) Service(_ context.Context, id string) (api.ServiceInfo, error) {
	r, ok, err := Get[ServiceRecord](s.Ops.Svc.store, CollServices, id)
	if err != nil {
		return api.ServiceInfo{}, MapCommandError(err)
	}
	if !ok {
		return api.ServiceInfo{}, fmt.Errorf("%w: service %q", api.ErrNotFound, id)
	}
	return toAPIService(r.Value), nil
}

// serviceHosts returns the planner view of the enrolled hosts of a Swarm.
func (o *Operations) serviceHosts(swarm string) ([]service.Host, error) {
	hs, err := o.enrolledHosts(swarm)
	if err != nil {
		return nil, err
	}
	out := make([]service.Host, 0, len(hs))
	for _, h := range hs {
		out = append(out, service.Host{ID: h.ID, Name: h.Name, Ready: h.Ready, Active: h.Active})
	}
	return out, nil
}

// commandHost is the host of the service commands: the Swarm manager node
// with an agent in production, else (no Swarm connector, unit tests) the
// first enrolled host, so that no step is without a target.
func (o *Operations) commandHost(swarm string) (string, error) {
	h, err := o.serviceHostFor(swarm)
	if err != nil || h != "" {
		return h, err
	}
	hosts, err := o.serviceHosts(swarm)
	if err != nil {
		return "", err
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("%w in swarm %s", ErrNoServiceHost, swarm)
	}
	ids := make([]string, 0, len(hosts))
	for _, x := range hosts {
		ids = append(ids, x.ID)
	}
	slices.Sort(ids)
	return ids[0], nil
}

// planCreate validates the request and plans it. It changes nothing.
func (s *Services) planCreate(actor authz.Subject, in api.ServiceInput) (service.Plan, string, Settings, error) {
	o := s.Ops
	if _, ok, err := Get[SwarmRecord](o.Svc.store, CollSwarms, in.Swarm, Consistent()); err != nil {
		return service.Plan{}, "", Settings{}, err
	} else if !ok {
		return service.Plan{}, "", Settings{}, fmt.Errorf("%w: swarm %q", api.ErrNotFound, in.Swarm)
	}
	if err := o.authorize(actor, authz.ActionCreate, serviceRes(in.Swarm, "")); err != nil {
		return service.Plan{}, "", Settings{}, err
	}
	st, err := o.settings()
	if err != nil {
		return service.Plan{}, "", Settings{}, err
	}
	ty, err := s.registry().Get(in.Type)
	if err != nil {
		return service.Plan{}, "", Settings{}, err
	}
	existing, err := List[ServiceRecord](o.Svc.store, CollServices, Consistent())
	if err != nil {
		return service.Plan{}, "", Settings{}, err
	}
	for _, r := range existing {
		if r.Value.Swarm == in.Swarm && r.Value.Name == in.Name {
			return service.Plan{}, "", Settings{}, fmt.Errorf("%w: %s", ErrServiceExists, in.Name)
		}
	}
	hosts, err := o.serviceHosts(in.Swarm)
	if err != nil {
		return service.Plan{}, "", Settings{}, err
	}
	id := newID("svc")
	plan, err := ty.Plan(service.PlanInput{
		Context: service.Context{InstallationID: st.InstallationID, ServiceID: id, Name: in.Name},
		Spec:    in.Spec, Hosts: hosts,
	})
	return plan, id, st, err
}

// PreviewService implements api.Services.
func (s *Services) PreviewService(_ context.Context, actor authz.Subject, in api.ServiceInput) (api.ServicePlan, error) {
	plan, _, _, err := s.planCreate(actor, in)
	if err != nil {
		return api.ServicePlan{}, MapCommandError(err)
	}
	return toAPIPlan(plan), nil
}

func toAPIPlan(p service.Plan) api.ServicePlan {
	out := api.ServicePlan{Kind: string(p.Kind), Type: p.Type, Name: p.Name, Warnings: p.Warnings, Blockers: p.Blockers,
		Executable: p.Executable(), RequiresApproval: p.RequiresApproval, Text: p.Text()}
	for _, st := range p.Steps {
		ps := api.PlanStep{ID: st.ID, Kind: string(st.Kind), Slot: st.HostID, Note: st.Note}
		for _, a := range st.After {
			ps.Conditions = append(ps.Conditions, "step "+a+" succeeded")
		}
		if st.Destructive {
			ps.Conditions = append(ps.Conditions, "approved")
		}
		out.Steps = append(out.Steps, ps)
	}
	for _, a := range p.Artifacts {
		out.Artifacts = append(out.Artifacts, api.ServiceArtifact{Name: a.Name, MediaType: a.MediaType, Content: a.Content})
	}
	return out
}

// serviceRefs are the operator-provided objects a service spec's plan uses.
func serviceRefs(p service.Plan) objectRefs {
	var r objectRefs
	for _, st := range p.Steps {
		if st.Spec != nil {
			r.Networks = append(r.Networks, st.Spec.Networks...)
			r.Secrets = append(r.Secrets, st.Spec.Secrets...)
		}
	}
	return r
}

// CreateService implements api.Services.
func (s *Services) CreateService(_ context.Context, actor authz.Subject, in api.ServiceInput) (api.ServiceStarted, error) {
	started, err := s.create(actor, in)
	return started, MapCommandError(err)
}

func (s *Services) create(actor authz.Subject, in api.ServiceInput) (api.ServiceStarted, error) {
	o := s.Ops
	l, err := o.leadership()
	if err != nil {
		return api.ServiceStarted{}, err
	}
	plan, id, st, err := s.planCreate(actor, in)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	svcHost, err := o.commandHost(in.Swarm)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	tokenIDs := map[string]string{}
	var hosts []string
	for _, step := range plan.Steps {
		if step.Kind == service.StepInitIdentity {
			tokenIDs[step.Volume] = newID("tok")
		}
		if step.HostID != "" {
			hosts = append(hosts, step.HostID)
		}
	}
	opID, approvalID := newID("op"), newID("apr")
	op, err := runner.FromServicePlan(plan, runner.ServiceBindings{OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase,
		TokenIDs: tokenIDs, StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries, ServiceHost: svcHost})
	if err != nil {
		return api.ServiceStarted{}, err
	}
	if err := o.allowReferenced(&op, in.Swarm, st.InstallationID, append(hosts, svcHost), serviceRefs(plan)); err != nil {
		return api.ServiceStarted{}, err
	}
	requireApproval(&op, approvalID)
	if err := op.Validate(); err != nil {
		return api.ServiceStarted{}, err
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	now := o.Svc.cfg.Now()
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: in.Swarm, Service: id, RequestedBy: actor.ID, RequestedAt: now,
		Definition: op, ApprovalID: approvalID, ApprovalDigest: digest}
	svc := ServiceRecord{ID: id, Swarm: in.Swarm, Name: in.Name, Type: in.Type, Spec: slices.Clone(in.Spec),
		CreatedBy: actor.ID, CreatedAt: now, CreateOp: opID}
	opDoc, err := PutOp(CollOperations, opID, rec, IfVersion(0))
	if err != nil {
		return api.ServiceStarted{}, err
	}
	svcDoc, err := PutOp(CollServices, id, svc, IfVersion(0))
	if err != nil {
		return api.ServiceStarted{}, err
	}
	if _, err := o.Svc.store.Txn(opDoc, svcDoc); err != nil {
		return api.ServiceStarted{}, err
	}
	o.Svc.recordEvent(in.Swarm, "", api.SeverityInfo, fmt.Sprintf("%s requested %s service %s, operation %s (waits for approval)", actor.ID, in.Type, in.Name, opID))
	o.start(l, op, nil)
	return api.ServiceStarted{Service: id, Operation: api.OperationRef{ID: opID, Kind: op.Kind},
		Approval: api.OperationApproval{Digest: digest}, Plan: toAPIPlan(plan)}, nil
}

// planDelete plans the removal of a stored service.
func (s *Services) planDelete(actor authz.Subject, id string, removeVolumes bool) (service.Plan, Versioned[ServiceRecord], Settings, error) {
	o := s.Ops
	cur, ok, err := Get[ServiceRecord](o.Svc.store, CollServices, id, Consistent())
	if err != nil {
		return service.Plan{}, cur, Settings{}, err
	}
	if !ok {
		return service.Plan{}, cur, Settings{}, fmt.Errorf("%w: service %q", api.ErrNotFound, id)
	}
	r := cur.Value
	if err := o.authorize(actor, authz.ActionDelete, serviceRes(r.Swarm, r.ID)); err != nil {
		return service.Plan{}, cur, Settings{}, err
	}
	st, err := o.settings()
	if err != nil {
		return service.Plan{}, cur, Settings{}, err
	}
	ty, err := s.registry().Get(r.Type)
	if err != nil {
		return service.Plan{}, cur, Settings{}, err
	}
	plan, err := ty.DeletePlan(service.PlanInput{
		Context: service.Context{InstallationID: st.InstallationID, ServiceID: r.ID, Name: r.Name},
		Spec:    r.Spec, RemoveVolumes: removeVolumes,
	})
	return plan, cur, st, err
}

// PreviewServiceDelete implements api.Services.
func (s *Services) PreviewServiceDelete(_ context.Context, actor authz.Subject, id string, removeVolumes bool) (api.ServicePlan, error) {
	plan, _, _, err := s.planDelete(actor, id, removeVolumes)
	if err != nil {
		return api.ServicePlan{}, MapCommandError(err)
	}
	return toAPIPlan(plan), nil
}

// DeleteService implements api.Services.
func (s *Services) DeleteService(_ context.Context, actor authz.Subject, id string, removeVolumes bool) (api.ServiceStarted, error) {
	started, err := s.delete(actor, id, removeVolumes)
	return started, MapCommandError(err)
}

func (s *Services) delete(actor authz.Subject, id string, removeVolumes bool) (api.ServiceStarted, error) {
	o := s.Ops
	l, err := o.leadership()
	if err != nil {
		return api.ServiceStarted{}, err
	}
	plan, cur, st, err := s.planDelete(actor, id, removeVolumes)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	r := cur.Value
	ops, err := List[OperationRecord](o.Svc.store, CollOperations, Consistent())
	if err != nil {
		return api.ServiceStarted{}, err
	}
	var failed []string
	for _, x := range ops {
		v := x.Value
		if v.Service != id {
			continue
		}
		switch {
		case v.EndedAt.IsZero() && v.View != nil && v.View.Status == runner.StatusStopped && len(v.View.Steps) == 0:
			// never started
		case v.EndedAt.IsZero():
			return api.ServiceStarted{}, fmt.Errorf("%w: operation %s is not finished; abort it first", runner.ErrBusy, v.ID)
		case v.View != nil && v.View.Status == runner.StatusFailed:
			failed = append(failed, v.ID)
		}
	}
	svcHost, err := o.commandHost(r.Swarm)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	opID, approvalID := newID("op"), newID("apr")
	op, err := runner.FromServicePlan(plan, runner.ServiceBindings{OpID: opID, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase,
		StepTimeout: o.StepTimeout, MaxRetries: o.MaxRetries, ServiceHost: svcHost})
	if err != nil {
		return api.ServiceStarted{}, err
	}
	requireApproval(&op, approvalID)
	if err := op.Validate(); err != nil {
		return api.ServiceStarted{}, err
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return api.ServiceStarted{}, err
	}
	rec := OperationRecord{ID: opID, Kind: op.Kind, Swarm: r.Swarm, Service: id, RequestedBy: actor.ID, RequestedAt: o.Svc.cfg.Now(),
		Definition: op, ApprovalID: approvalID, ApprovalDigest: digest}
	opDoc, err := PutOp(CollOperations, opID, rec, IfVersion(0))
	if err != nil {
		return api.ServiceStarted{}, err
	}
	marked := r
	marked.DeleteOp = opID
	svcDoc, err := PutOp(CollServices, id, marked, IfVersion(cur.Version))
	if err != nil {
		return api.ServiceStarted{}, err
	}
	if _, err := o.Svc.store.Txn(opDoc, svcDoc); err != nil {
		return api.ServiceStarted{}, err
	}
	// A failed operation keeps its runner locks until aborted.
	for _, fid := range failed {
		if err := l.Runner.Abort(fid); err != nil && !errors.Is(err, runner.ErrUnknownOperation) && !errors.Is(err, runner.ErrFinished) {
			o.Svc.log.Warn("manager: failed operation not aborted before the service deletion", "op", fid, "err", err)
		} else if v, err := l.Runner.Snapshot(fid); err == nil {
			_ = o.Svc.saveView(v)
		}
	}
	o.Svc.recordEvent(r.Swarm, "", api.SeverityWarning, fmt.Sprintf("%s requested deletion of service %s, operation %s (waits for approval)", actor.ID, r.Name, opID))
	o.start(l, op, nil)
	return api.ServiceStarted{Service: id, Operation: api.OperationRef{ID: opID, Kind: op.Kind},
		Approval: api.OperationApproval{Digest: digest}, Plan: toAPIPlan(plan)}, nil
}

// finishServiceDelete removes the service record after its deletion
// operation succeeded (called from saveView, also for resumed runs). A
// record that names another deletion is kept.
func (s *Service) finishServiceDelete(rec OperationRecord) {
	cur, ok, err := Get[ServiceRecord](s.store, CollServices, rec.Service, Consistent())
	if err != nil || !ok {
		return
	}
	if cur.Value.DeleteOp != rec.ID {
		s.log.Warn("manager: service record not deleted: it names another deletion", "service", rec.Service, "op", rec.ID)
		return
	}
	if err := Delete(s.store, CollServices, rec.Service, IfVersion(cur.Version)); err != nil && !errors.Is(err, metastore.ErrNotLeader) {
		s.log.Warn("manager: service record not deleted after the deletion operation", "service", rec.Service, "err", err)
		return
	}
	s.recordEvent(rec.Swarm, "", api.SeverityWarning, fmt.Sprintf("service %s deleted (operation %s)", cur.Value.Name, rec.ID))
}
