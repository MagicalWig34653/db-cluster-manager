// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// Patch management endpoints (X15, ADR 0023; fakes only). All routes use the
// global resource "patch". Reads need read (viewer); polling and proposing
// need operate (operator); host/group/policy changes, approval, apply and the
// native agent install need update or create (administrator).

// PatchHostInput creates or updates the patch record of a host.
type PatchHostInput struct {
	Name   string        `json:"name,omitempty"`
	Group  string        `json:"group,omitempty"`
	Policy *patch.Policy `json:"policy,omitempty"`
}

// PatchGroupInput creates or updates a group.
type PatchGroupInput struct {
	Name   string        `json:"name"`
	Policy *patch.Policy `json:"policy,omitempty"`
}

// PatchApproveInput approves the plan with this digest.
type PatchApproveInput struct {
	Digest string `json:"digest"`
}

// PatchHostView is one host with its latest report (no installed list).
type PatchHostView struct {
	Summary patch.HostSummary `json:"summary"`
	Updates []patch.Update    `json:"updates"`
	Reasons []string          `json:"rebootReasons,omitempty"`
	Notes   []string          `json:"warnings,omitempty"`
}

// PatchInstallInput asks for a native agent install plan. The credential is
// write-only: it is sealed and never returned.
type PatchInstallInput struct {
	Request    patch.InstallRequest `json:"request"`
	Credential provision.Credential `json:"credential"`
}

// Patch backs the /patch endpoints.
type Patch interface {
	Overview(ctx context.Context) ([]patch.HostSummary, error)
	Host(ctx context.Context, id string) (PatchHostView, error)
	UpsertHost(ctx context.Context, id string, in PatchHostInput) (patch.Host, error)
	Poll(ctx context.Context, id string) (PatchHostView, error)
	History(ctx context.Context, id string) ([]patch.HistoryEntry, error)
	Packages(ctx context.Context) ([]patch.PackageView, error)
	Groups(ctx context.Context) ([]patch.Group, error)
	UpsertGroup(ctx context.Context, id string, in PatchGroupInput) (patch.Group, error)
	DeleteGroup(ctx context.Context, id string) error
	DefaultPolicy(ctx context.Context) (patch.Policy, error)
	SetDefaultPolicy(ctx context.Context, p patch.Policy) (patch.Policy, error)
	Plans(ctx context.Context) ([]patch.Plan, error)
	Plan(ctx context.Context, id string) (patch.Plan, error)
	Propose(ctx context.Context, actor authz.Subject, host string, in patch.ProposeInput) (patch.Plan, error)
	Approve(ctx context.Context, actor authz.Subject, id string, in PatchApproveInput) (patch.Plan, error)
	Decline(ctx context.Context, id string) (patch.Plan, error)
	Apply(ctx context.Context, id string) (patch.Plan, error)
	InstallPlan(ctx context.Context, in PatchInstallInput) (patch.InstallPlan, error)
	InstallExecute(ctx context.Context, id string, in PatchApproveInput) error
}

type unavailablePatch struct{ Patch }

func (unavailablePatch) Overview(context.Context) ([]patch.HostSummary, error) {
	return nil, ErrUnavailable
}
func (unavailablePatch) Host(context.Context, string) (PatchHostView, error) {
	return PatchHostView{}, ErrUnavailable
}
func (unavailablePatch) UpsertHost(context.Context, string, PatchHostInput) (patch.Host, error) {
	return patch.Host{}, ErrUnavailable
}
func (unavailablePatch) Poll(context.Context, string) (PatchHostView, error) {
	return PatchHostView{}, ErrUnavailable
}
func (unavailablePatch) History(context.Context, string) ([]patch.HistoryEntry, error) {
	return nil, ErrUnavailable
}
func (unavailablePatch) Packages(context.Context) ([]patch.PackageView, error) {
	return nil, ErrUnavailable
}
func (unavailablePatch) Groups(context.Context) ([]patch.Group, error) { return nil, ErrUnavailable }
func (unavailablePatch) UpsertGroup(context.Context, string, PatchGroupInput) (patch.Group, error) {
	return patch.Group{}, ErrUnavailable
}
func (unavailablePatch) DeleteGroup(context.Context, string) error { return ErrUnavailable }
func (unavailablePatch) DefaultPolicy(context.Context) (patch.Policy, error) {
	return patch.Policy{}, ErrUnavailable
}
func (unavailablePatch) SetDefaultPolicy(context.Context, patch.Policy) (patch.Policy, error) {
	return patch.Policy{}, ErrUnavailable
}
func (unavailablePatch) Plans(context.Context) ([]patch.Plan, error) { return nil, ErrUnavailable }
func (unavailablePatch) Plan(context.Context, string) (patch.Plan, error) {
	return patch.Plan{}, ErrUnavailable
}
func (unavailablePatch) Propose(context.Context, authz.Subject, string, patch.ProposeInput) (patch.Plan, error) {
	return patch.Plan{}, ErrUnavailable
}
func (unavailablePatch) Approve(context.Context, authz.Subject, string, PatchApproveInput) (patch.Plan, error) {
	return patch.Plan{}, ErrUnavailable
}
func (unavailablePatch) Decline(context.Context, string) (patch.Plan, error) {
	return patch.Plan{}, ErrUnavailable
}
func (unavailablePatch) Apply(context.Context, string) (patch.Plan, error) {
	return patch.Plan{}, ErrUnavailable
}
func (unavailablePatch) InstallPlan(context.Context, PatchInstallInput) (patch.InstallPlan, error) {
	return patch.InstallPlan{}, ErrUnavailable
}
func (unavailablePatch) InstallExecute(context.Context, string, PatchApproveInput) error {
	return ErrUnavailable
}

var patchResource = authz.Resource{Type: authz.ResourcePatch, Scope: authz.Global}

func (s *server) resolvePatch(*http.Request) (authz.Resource, any, error) {
	return patchResource, nil, nil
}

func (s *server) patchSvc() Patch {
	if s.cfg.Patch != nil {
		return s.cfg.Patch
	}
	return unavailablePatch{}
}

func (s *server) patchEndpoints() []endpoint {
	ep := func(method, path string, a authz.Action, h func(http.ResponseWriter, *http.Request, *caller)) endpoint {
		return endpoint{method: method, path: path, access: accessPermission, action: a, resolve: s.resolvePatch, handle: h}
	}
	return []endpoint{
		ep("GET", "/patch/hosts", authz.ActionRead, s.handlePatchHosts),
		ep("GET", "/patch/hosts/{id}", authz.ActionRead, s.handlePatchHost),
		ep("PUT", "/patch/hosts/{id}", authz.ActionUpdate, s.handlePatchHostPut),
		ep("POST", "/patch/hosts/{id}/poll", authz.ActionOperate, s.handlePatchPoll),
		ep("GET", "/patch/hosts/{id}/history", authz.ActionRead, s.handlePatchHistory),
		ep("POST", "/patch/hosts/{id}/plans", authz.ActionOperate, s.handlePatchPropose),
		ep("GET", "/patch/packages", authz.ActionRead, s.handlePatchPackages),
		ep("GET", "/patch/groups", authz.ActionRead, s.handlePatchGroups),
		ep("PUT", "/patch/groups/{id}", authz.ActionUpdate, s.handlePatchGroupPut),
		ep("DELETE", "/patch/groups/{id}", authz.ActionUpdate, s.handlePatchGroupDelete),
		ep("GET", "/patch/policy", authz.ActionRead, s.handlePatchPolicy),
		ep("PUT", "/patch/policy", authz.ActionUpdate, s.handlePatchPolicyPut),
		ep("GET", "/patch/plans", authz.ActionRead, s.handlePatchPlans),
		ep("GET", "/patch/plans/{id}", authz.ActionRead, s.handlePatchPlan),
		ep("POST", "/patch/plans/{id}/approve", authz.ActionUpdate, s.handlePatchApprove),
		ep("POST", "/patch/plans/{id}/decline", authz.ActionUpdate, s.handlePatchDecline),
		ep("POST", "/patch/plans/{id}/apply", authz.ActionUpdate, s.handlePatchApply),
		ep("POST", "/patch/install/plans", authz.ActionCreate, s.handlePatchInstallPlan),
		ep("POST", "/patch/install/plans/{id}/execute", authz.ActionCreate, s.handlePatchInstallExecute),
	}
}

func (s *server) handlePatchHosts(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().Overview(r.Context())
	writeList(s, w, r, out, err)
}
func (s *server) handlePatchHost(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().Host(r.Context(), r.PathValue("id"))
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchHostPut(w http.ResponseWriter, r *http.Request, c *caller) {
	var in PatchHostInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.patchSvc().UpsertHost(r.Context(), r.PathValue("id"), in)
	if err == nil {
		s.log.Info("api: patch host changed", "host", out.ID, "by", c.subject.ID)
	}
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchPoll(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().Poll(r.Context(), r.PathValue("id"))
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchHistory(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().History(r.Context(), r.PathValue("id"))
	writeList(s, w, r, out, err)
}
func (s *server) handlePatchPropose(w http.ResponseWriter, r *http.Request, c *caller) {
	var in patch.ProposeInput
	if !decodeJSON(w, r, &in, true) {
		return
	}
	out, err := s.patchSvc().Propose(r.Context(), c.subject, r.PathValue("id"), in)
	writeOne(s, w, r, http.StatusCreated, out, err)
}
func (s *server) handlePatchPackages(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().Packages(r.Context())
	writeList(s, w, r, out, err)
}
func (s *server) handlePatchGroups(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().Groups(r.Context())
	writeList(s, w, r, out, err)
}
func (s *server) handlePatchGroupPut(w http.ResponseWriter, r *http.Request, _ *caller) {
	var in PatchGroupInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.patchSvc().UpsertGroup(r.Context(), r.PathValue("id"), in)
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchGroupDelete(w http.ResponseWriter, r *http.Request, _ *caller) {
	if err := s.patchSvc().DeleteGroup(r.Context(), r.PathValue("id")); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *server) handlePatchPolicy(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().DefaultPolicy(r.Context())
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchPolicyPut(w http.ResponseWriter, r *http.Request, c *caller) {
	var in patch.Policy
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.patchSvc().SetDefaultPolicy(r.Context(), in)
	if err == nil {
		s.log.Info("api: patch default policy changed", "by", c.subject.ID, "mode", out.Mode)
	}
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchPlans(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().Plans(r.Context())
	writeList(s, w, r, out, err)
}
func (s *server) handlePatchPlan(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().Plan(r.Context(), r.PathValue("id"))
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchApprove(w http.ResponseWriter, r *http.Request, c *caller) {
	var in PatchApproveInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if len(in.Digest) != 64 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.patchSvc().Approve(r.Context(), c.subject, r.PathValue("id"), in)
	if err == nil {
		s.log.Info("api: patch plan approved", "plan", out.ID, "host", out.HostID, "by", c.subject.ID)
	}
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchDecline(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.patchSvc().Decline(r.Context(), r.PathValue("id"))
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handlePatchApply(w http.ResponseWriter, r *http.Request, c *caller) {
	out, err := s.patchSvc().Apply(r.Context(), r.PathValue("id"))
	s.log.Info("api: patch plan applied", "plan", r.PathValue("id"), "by", c.subject.ID, "ok", err == nil)
	if err != nil && out.ID != "" && out.State == patch.PlanFailed {
		// The run failed after it started: report the plan (with its error)
		// instead of a bare status.
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeOne(s, w, r, http.StatusAccepted, out, err)
}
func (s *server) handlePatchInstallPlan(w http.ResponseWriter, r *http.Request, _ *caller) {
	var in PatchInstallInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.patchSvc().InstallPlan(r.Context(), in)
	writeOne(s, w, r, http.StatusCreated, out, err)
}
func (s *server) handlePatchInstallExecute(w http.ResponseWriter, r *http.Request, c *caller) {
	var in PatchApproveInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if len(in.Digest) != 64 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if err := s.patchSvc().InstallExecute(r.Context(), r.PathValue("id"), in); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: patch agent install executed", "plan", r.PathValue("id"), "by", c.subject.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "installed"})
}

// PatchBackend adapts the patch services to the Patch interface.
type PatchBackend struct {
	Svc       *patch.Service
	Installer *patch.Installer
}

var _ Patch = (*PatchBackend)(nil)

func mapPatchError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, patch.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, patch.ErrInvalid), errors.Is(err, patch.ErrInstallInvalid), errors.Is(err, patch.ErrPartialUnsupported):
		return ErrInvalidInput
	case errors.Is(err, patch.ErrReportOnly), errors.Is(err, patch.ErrRebootDenied):
		return ErrPolicyConflict
	case errors.Is(err, patch.ErrOutsideWindow), errors.Is(err, patch.ErrGaleraBlocked), errors.Is(err, patch.ErrConflict),
		errors.Is(err, patch.ErrInstallExists), errors.Is(err, patch.ErrInstallPreflight):
		return ErrConflict
	case errors.Is(err, patch.ErrInstallDigest), errors.Is(err, patch.ErrDigest):
		return ErrApprovalMismatch
	}
	return err
}

func (b *PatchBackend) view(r patch.HostReport, sum patch.HostSummary) PatchHostView {
	return PatchHostView{Summary: sum, Updates: r.Updates, Reasons: r.RebootReasons, Notes: r.Warnings}
}

func (b *PatchBackend) hostView(id string) (PatchHostView, error) {
	sums, err := b.Svc.Overview()
	if err != nil {
		return PatchHostView{}, err
	}
	for _, s := range sums {
		if s.Host.ID == id {
			v := PatchHostView{Summary: s, Updates: []patch.Update{}}
			if r, err := b.Svc.Report(id); err == nil {
				v = b.view(r, s)
				if v.Updates == nil {
					v.Updates = []patch.Update{}
				}
			}
			return v, nil
		}
	}
	return PatchHostView{}, patch.ErrNotFound
}

// Overview implements Patch.
func (b *PatchBackend) Overview(context.Context) ([]patch.HostSummary, error) {
	out, err := b.Svc.Overview()
	return out, mapPatchError(err)
}

// Host implements Patch.
func (b *PatchBackend) Host(_ context.Context, id string) (PatchHostView, error) {
	v, err := b.hostView(id)
	return v, mapPatchError(err)
}

// UpsertHost implements Patch.
func (b *PatchBackend) UpsertHost(_ context.Context, id string, in PatchHostInput) (patch.Host, error) {
	out, err := b.Svc.UpsertHost(patch.Host{ID: id, Name: in.Name, Group: in.Group, Policy: in.Policy})
	return out, mapPatchError(err)
}

// Poll implements Patch.
func (b *PatchBackend) Poll(ctx context.Context, id string) (PatchHostView, error) {
	if _, err := b.Svc.Poll(ctx, id, true); err != nil {
		return PatchHostView{}, mapPatchError(err)
	}
	v, err := b.hostView(id)
	return v, mapPatchError(err)
}

// History implements Patch.
func (b *PatchBackend) History(_ context.Context, id string) ([]patch.HistoryEntry, error) {
	if _, err := b.Svc.Host(id); err != nil {
		return nil, mapPatchError(err)
	}
	out, err := b.Svc.History(id)
	return out, mapPatchError(err)
}

// Packages implements Patch.
func (b *PatchBackend) Packages(context.Context) ([]patch.PackageView, error) {
	out, err := b.Svc.Packages()
	return out, mapPatchError(err)
}

// Groups implements Patch.
func (b *PatchBackend) Groups(context.Context) ([]patch.Group, error) {
	out, err := b.Svc.Groups()
	return out, mapPatchError(err)
}

// UpsertGroup implements Patch.
func (b *PatchBackend) UpsertGroup(_ context.Context, id string, in PatchGroupInput) (patch.Group, error) {
	out, err := b.Svc.UpsertGroup(patch.Group{ID: id, Name: in.Name, Policy: in.Policy})
	return out, mapPatchError(err)
}

// DeleteGroup implements Patch.
func (b *PatchBackend) DeleteGroup(_ context.Context, id string) error {
	return mapPatchError(b.Svc.DeleteGroup(id))
}

// DefaultPolicy implements Patch.
func (b *PatchBackend) DefaultPolicy(context.Context) (patch.Policy, error) {
	out, err := b.Svc.DefaultPolicy()
	return out, mapPatchError(err)
}

// SetDefaultPolicy implements Patch.
func (b *PatchBackend) SetDefaultPolicy(_ context.Context, p patch.Policy) (patch.Policy, error) {
	if err := b.Svc.SetDefaultPolicy(p); err != nil {
		return patch.Policy{}, mapPatchError(err)
	}
	return p.Normalized(), nil
}

// Plans implements Patch.
func (b *PatchBackend) Plans(context.Context) ([]patch.Plan, error) {
	out, err := b.Svc.Plans()
	return out, mapPatchError(err)
}

// Plan implements Patch.
func (b *PatchBackend) Plan(_ context.Context, id string) (patch.Plan, error) {
	out, err := b.Svc.Plan(id)
	return out, mapPatchError(err)
}

// Propose implements Patch.
func (b *PatchBackend) Propose(ctx context.Context, a authz.Subject, host string, in patch.ProposeInput) (patch.Plan, error) {
	out, err := b.Svc.Propose(ctx, a.ID, host, in)
	return out, mapPatchError(err)
}

// Approve implements Patch.
func (b *PatchBackend) Approve(ctx context.Context, a authz.Subject, id string, in PatchApproveInput) (patch.Plan, error) {
	out, err := b.Svc.Approve(ctx, a.ID, id, in.Digest)
	return out, mapPatchError(err)
}

// Decline implements Patch.
func (b *PatchBackend) Decline(_ context.Context, id string) (patch.Plan, error) {
	out, err := b.Svc.Decline(id)
	return out, mapPatchError(err)
}

// Apply implements Patch.
func (b *PatchBackend) Apply(ctx context.Context, id string) (patch.Plan, error) {
	out, err := b.Svc.Apply(ctx, id)
	if out.State == patch.PlanFailed {
		return out, err // the handler returns the failed plan
	}
	return out, mapPatchError(err)
}

// InstallPlan implements Patch.
func (b *PatchBackend) InstallPlan(ctx context.Context, in PatchInstallInput) (patch.InstallPlan, error) {
	if b.Installer == nil {
		return patch.InstallPlan{}, ErrUnavailable
	}
	out, err := b.Installer.Plan(ctx, in.Request, in.Credential)
	return out, mapPatchError(err)
}

// InstallExecute implements Patch.
func (b *PatchBackend) InstallExecute(ctx context.Context, id string, in PatchApproveInput) error {
	if b.Installer == nil {
		return ErrUnavailable
	}
	return mapPatchError(b.Installer.Execute(ctx, id, in.Digest))
}
