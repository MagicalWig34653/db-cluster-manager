// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imageupdate"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

// Image update endpoints (X16, ADR 0025; fakes only). All routes use the
// global resource "image_update". Reads need read (viewer); checking for
// candidates needs operate (operator); policies, approval, apply, rollback
// and offers need update (administrator). Applying also needs update on the
// service resource, which the service update operation checks itself.

// ImageUpdatePolicyInput is the policy of a service (the service ID is the
// path).
type ImageUpdatePolicyInput struct {
	Mode              imageupdate.Mode     `json:"mode"`
	Track             imageupdate.Track    `json:"track"`
	Windows           []patch.Window       `json:"windows,omitempty"`
	Strategy          imageupdate.Strategy `json:"strategy"`
	CheckEveryMinutes int                  `json:"checkEveryMinutes,omitempty"`
	WaitMinutes       int                  `json:"waitMinutes,omitempty"`
}

// ImageUpdateOfferInput offers a finished image build as a candidate.
type ImageUpdateOfferInput struct {
	JobID     string `json:"jobId"`
	FindingID string `json:"findingId,omitempty"`
}

// ImageUpdateBuilds resolves a finished build job (the image build service
// backs it).
type ImageUpdateBuilds interface {
	Built(ctx context.Context, jobID, findingID string) (imageupdate.Built, error)
}

// ImageUpdates backs the /image-updates endpoints.
type ImageUpdates interface {
	Services(ctx context.Context) ([]imageupdate.ServiceView, error)
	SetPolicy(ctx context.Context, actor authz.Subject, serviceID string, in ImageUpdatePolicyInput) (imageupdate.Policy, error)
	DeletePolicy(ctx context.Context, serviceID string) error
	Check(ctx context.Context, actor authz.Subject, serviceID string) (imageupdate.CheckResult, error)
	Plans(ctx context.Context) ([]imageupdate.Plan, error)
	Plan(ctx context.Context, id string) (imageupdate.Plan, error)
	Approve(ctx context.Context, actor authz.Subject, id string, in PatchApproveInput) (imageupdate.Plan, error)
	Decline(ctx context.Context, actor authz.Subject, id string) (imageupdate.Plan, error)
	Apply(ctx context.Context, actor authz.Subject, id string) (imageupdate.Plan, error)
	Rollback(ctx context.Context, actor authz.Subject, id string) (imageupdate.Plan, error)
	Offer(ctx context.Context, actor authz.Subject, in ImageUpdateOfferInput) ([]imageupdate.Plan, error)
}

type unavailableImageUpdates struct{ ImageUpdates }

func (unavailableImageUpdates) Services(context.Context) ([]imageupdate.ServiceView, error) {
	return nil, ErrUnavailable
}
func (unavailableImageUpdates) SetPolicy(context.Context, authz.Subject, string, ImageUpdatePolicyInput) (imageupdate.Policy, error) {
	return imageupdate.Policy{}, ErrUnavailable
}
func (unavailableImageUpdates) DeletePolicy(context.Context, string) error { return ErrUnavailable }
func (unavailableImageUpdates) Check(context.Context, authz.Subject, string) (imageupdate.CheckResult, error) {
	return imageupdate.CheckResult{}, ErrUnavailable
}
func (unavailableImageUpdates) Plans(context.Context) ([]imageupdate.Plan, error) {
	return nil, ErrUnavailable
}
func (unavailableImageUpdates) Plan(context.Context, string) (imageupdate.Plan, error) {
	return imageupdate.Plan{}, ErrUnavailable
}
func (unavailableImageUpdates) Approve(context.Context, authz.Subject, string, PatchApproveInput) (imageupdate.Plan, error) {
	return imageupdate.Plan{}, ErrUnavailable
}
func (unavailableImageUpdates) Decline(context.Context, authz.Subject, string) (imageupdate.Plan, error) {
	return imageupdate.Plan{}, ErrUnavailable
}
func (unavailableImageUpdates) Apply(context.Context, authz.Subject, string) (imageupdate.Plan, error) {
	return imageupdate.Plan{}, ErrUnavailable
}
func (unavailableImageUpdates) Rollback(context.Context, authz.Subject, string) (imageupdate.Plan, error) {
	return imageupdate.Plan{}, ErrUnavailable
}
func (unavailableImageUpdates) Offer(context.Context, authz.Subject, ImageUpdateOfferInput) ([]imageupdate.Plan, error) {
	return nil, ErrUnavailable
}

var imageUpdateResource = authz.Resource{Type: authz.ResourceImageUpdate, Scope: authz.Global}

func (s *server) resolveImageUpdate(*http.Request) (authz.Resource, any, error) {
	return imageUpdateResource, nil, nil
}

func (s *server) imageUpdates() ImageUpdates {
	if s.cfg.ImageUpdates != nil {
		return s.cfg.ImageUpdates
	}
	return unavailableImageUpdates{}
}

func (s *server) imageUpdateEndpoints() []endpoint {
	ep := func(method, path string, a authz.Action, h func(http.ResponseWriter, *http.Request, *caller)) endpoint {
		return endpoint{method: method, path: path, access: accessPermission, action: a, resolve: s.resolveImageUpdate, handle: h}
	}
	return []endpoint{
		ep("GET", "/image-updates/services", authz.ActionRead, s.handleImageUpdateServices),
		ep("PUT", "/image-updates/services/{id}/policy", authz.ActionUpdate, s.handleImageUpdatePolicyPut),
		ep("DELETE", "/image-updates/services/{id}/policy", authz.ActionUpdate, s.handleImageUpdatePolicyDelete),
		ep("POST", "/image-updates/services/{id}/check", authz.ActionOperate, s.handleImageUpdateCheck),
		ep("GET", "/image-updates/plans", authz.ActionRead, s.handleImageUpdatePlans),
		ep("GET", "/image-updates/plans/{id}", authz.ActionRead, s.handleImageUpdatePlan),
		ep("POST", "/image-updates/plans/{id}/approve", authz.ActionUpdate, s.handleImageUpdateApprove),
		ep("POST", "/image-updates/plans/{id}/decline", authz.ActionUpdate, s.handleImageUpdateDecline),
		ep("POST", "/image-updates/plans/{id}/apply", authz.ActionUpdate, s.handleImageUpdateApply),
		ep("POST", "/image-updates/plans/{id}/rollback", authz.ActionUpdate, s.handleImageUpdateRollback),
		ep("POST", "/image-updates/offers", authz.ActionUpdate, s.handleImageUpdateOffer),
	}
}

func (s *server) handleImageUpdateServices(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.imageUpdates().Services(r.Context())
	writeList(s, w, r, out, err)
}
func (s *server) handleImageUpdatePolicyPut(w http.ResponseWriter, r *http.Request, c *caller) {
	var in ImageUpdatePolicyInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.imageUpdates().SetPolicy(r.Context(), c.subject, r.PathValue("id"), in)
	if err == nil {
		s.log.Info("api: image update policy changed", "service", out.ServiceID, "mode", out.Mode, "track", out.Track, "by", c.subject.ID)
	}
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handleImageUpdatePolicyDelete(w http.ResponseWriter, r *http.Request, c *caller) {
	if err := s.imageUpdates().DeletePolicy(r.Context(), r.PathValue("id")); err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: image update policy removed", "service", r.PathValue("id"), "by", c.subject.ID)
	w.WriteHeader(http.StatusNoContent)
}
func (s *server) handleImageUpdateCheck(w http.ResponseWriter, r *http.Request, c *caller) {
	out, err := s.imageUpdates().Check(r.Context(), c.subject, r.PathValue("id"))
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handleImageUpdatePlans(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.imageUpdates().Plans(r.Context())
	writeList(s, w, r, out, err)
}
func (s *server) handleImageUpdatePlan(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.imageUpdates().Plan(r.Context(), r.PathValue("id"))
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handleImageUpdateApprove(w http.ResponseWriter, r *http.Request, c *caller) {
	var in PatchApproveInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if len(in.Digest) != 64 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.imageUpdates().Approve(r.Context(), c.subject, r.PathValue("id"), in)
	if err == nil {
		s.log.Info("api: image update plan approved", "plan", out.ID, "service", out.ServiceID, "by", c.subject.ID)
	}
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handleImageUpdateDecline(w http.ResponseWriter, r *http.Request, c *caller) {
	out, err := s.imageUpdates().Decline(r.Context(), c.subject, r.PathValue("id"))
	writeOne(s, w, r, http.StatusOK, out, err)
}
func (s *server) handleImageUpdateApply(w http.ResponseWriter, r *http.Request, c *caller) {
	out, err := s.imageUpdates().Apply(r.Context(), c.subject, r.PathValue("id"))
	s.log.Info("api: image update plan applied", "plan", r.PathValue("id"), "by", c.subject.ID, "ok", err == nil)
	writeOne(s, w, r, http.StatusAccepted, out, err)
}
func (s *server) handleImageUpdateRollback(w http.ResponseWriter, r *http.Request, c *caller) {
	out, err := s.imageUpdates().Rollback(r.Context(), c.subject, r.PathValue("id"))
	writeOne(s, w, r, http.StatusCreated, out, err)
}
func (s *server) handleImageUpdateOffer(w http.ResponseWriter, r *http.Request, c *caller) {
	var in ImageUpdateOfferInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.JobID == "" || len(in.JobID) > 128 || len(in.FindingID) > 128 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.imageUpdates().Offer(r.Context(), c.subject, in)
	writeList(s, w, r, out, err)
}

// ImageUpdateBackend adapts the image update service to ImageUpdates.
type ImageUpdateBackend struct {
	Svc *imageupdate.Service
	// Builds resolves finished build jobs for offers; nil disables offers
	// (they answer 503).
	Builds ImageUpdateBuilds
}

var _ ImageUpdates = (*ImageUpdateBackend)(nil)

func mapImageUpdateError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, imageupdate.ErrNotFound), errors.Is(err, imageupdate.ErrNotOwned):
		return ErrNotFound
	case errors.Is(err, imageupdate.ErrInvalid):
		return ErrInvalidInput
	case errors.Is(err, imageupdate.ErrDigest):
		return ErrApprovalMismatch
	case errors.Is(err, imageupdate.ErrBusy):
		return ErrBusy
	case errors.Is(err, imageupdate.ErrRoutedToPlanner), errors.Is(err, imageupdate.ErrForeign):
		return ErrPolicyConflict
	case errors.Is(err, imageupdate.ErrConflict), errors.Is(err, imageupdate.ErrStale), errors.Is(err, imageupdate.ErrOutsideWindow):
		return ErrConflict
	}
	return err
}

// Services implements ImageUpdates.
func (b *ImageUpdateBackend) Services(ctx context.Context) ([]imageupdate.ServiceView, error) {
	out, err := b.Svc.Services(ctx)
	return out, mapImageUpdateError(err)
}

// SetPolicy implements ImageUpdates.
func (b *ImageUpdateBackend) SetPolicy(ctx context.Context, a authz.Subject, id string, in ImageUpdatePolicyInput) (imageupdate.Policy, error) {
	p := imageupdate.Policy{ServiceID: id, Mode: in.Mode, Track: in.Track, Windows: in.Windows, Strategy: in.Strategy,
		CheckEveryMinutes: in.CheckEveryMinutes, WaitMinutes: in.WaitMinutes}
	out, err := b.Svc.SetPolicy(ctx, a.ID, p)
	return out, mapImageUpdateError(err)
}

// DeletePolicy implements ImageUpdates.
func (b *ImageUpdateBackend) DeletePolicy(ctx context.Context, id string) error {
	return mapImageUpdateError(b.Svc.DeletePolicy(ctx, id))
}

// Check implements ImageUpdates.
func (b *ImageUpdateBackend) Check(ctx context.Context, a authz.Subject, id string) (imageupdate.CheckResult, error) {
	out, err := b.Svc.Check(ctx, id, imageupdate.CheckOptions{Actor: a.ID, Force: true})
	return out, mapImageUpdateError(err)
}

// Plans implements ImageUpdates.
func (b *ImageUpdateBackend) Plans(context.Context) ([]imageupdate.Plan, error) {
	out, err := b.Svc.Plans()
	return out, mapImageUpdateError(err)
}

// Plan implements ImageUpdates.
func (b *ImageUpdateBackend) Plan(_ context.Context, id string) (imageupdate.Plan, error) {
	out, err := b.Svc.Plan(id)
	return out, mapImageUpdateError(err)
}

// Approve implements ImageUpdates.
func (b *ImageUpdateBackend) Approve(ctx context.Context, a authz.Subject, id string, in PatchApproveInput) (imageupdate.Plan, error) {
	out, err := b.Svc.Approve(ctx, a.ID, id, in.Digest)
	return out, mapImageUpdateError(err)
}

// Decline implements ImageUpdates.
func (b *ImageUpdateBackend) Decline(ctx context.Context, a authz.Subject, id string) (imageupdate.Plan, error) {
	out, err := b.Svc.Decline(ctx, a.ID, id)
	return out, mapImageUpdateError(err)
}

// Apply implements ImageUpdates. A plan that failed after it started is
// returned as the answer (with its error text) instead of a bare status.
func (b *ImageUpdateBackend) Apply(ctx context.Context, a authz.Subject, id string) (imageupdate.Plan, error) {
	out, err := b.Svc.Apply(ctx, a.ID, id)
	if err != nil && out.State == imageupdate.PlanFailed {
		return out, nil
	}
	return out, mapImageUpdateError(err)
}

// Rollback implements ImageUpdates.
func (b *ImageUpdateBackend) Rollback(ctx context.Context, a authz.Subject, id string) (imageupdate.Plan, error) {
	out, err := b.Svc.ProposeRollback(ctx, a.ID, id)
	return out, mapImageUpdateError(err)
}

// Offer implements ImageUpdates.
func (b *ImageUpdateBackend) Offer(ctx context.Context, a authz.Subject, in ImageUpdateOfferInput) ([]imageupdate.Plan, error) {
	if b.Builds == nil {
		return nil, ErrUnavailable
	}
	built, err := b.Builds.Built(ctx, in.JobID, in.FindingID)
	if err != nil {
		return nil, mapImageUpdateError(err)
	}
	out, err := b.Svc.OfferBuilt(ctx, a.ID, built)
	if out == nil {
		out = []imageupdate.Plan{}
	}
	return out, mapImageUpdateError(err)
}
