// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// ImageBuildRequest asks for a plan; with ApprovalDigest equal to the digest
// of a reviewed plan it starts the build.
type ImageBuildRequest = imagebuild.Request

// ImageBuildPlan is the preview of a build (nothing has happened).
type ImageBuildPlan = imagebuild.Plan

// ImageBuildJob is a recorded build.
type ImageBuildJob = imagebuild.Job

// ImageBuildResult is either a plan (preview) or a started job.
type ImageBuildResult struct {
	Plan *ImageBuildPlan `json:"plan,omitempty"`
	Job  *ImageBuildJob  `json:"job,omitempty"`
}

// RegistryView is the non-secret description of a registry.
type RegistryView struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Kind     registry.Kind `json:"kind"`
	Endpoint string        `json:"endpoint,omitempty"`
	Username string        `json:"username,omitempty"`
	// CredentialSet reports that a credential is stored; the value is never
	// returned.
	CredentialSet bool   `json:"credentialSet"`
	CreatedBy     string `json:"createdBy,omitempty"`
}

// ExternalRegistryInput registers an external registry. The password is
// accepted once and stored as a secret.
type ExternalRegistryInput struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Kind     registry.Kind `json:"kind"`
	Endpoint string        `json:"endpoint,omitempty"`
	Username string        `json:"username,omitempty"`
	Password string        `json:"password,omitempty"`
}

// ImageBuilds backs the image build endpoints (X08, proposal). Plan must not
// change state or build anything; Start verifies the approval digest itself.
type ImageBuilds interface {
	Plan(ctx context.Context, actor authz.Subject, req ImageBuildRequest) (ImageBuildPlan, error)
	Start(ctx context.Context, actor authz.Subject, req ImageBuildRequest) (ImageBuildJob, error)
	Jobs(ctx context.Context) ([]ImageBuildJob, error)
}

// Registries backs the registry endpoints.
type Registries interface {
	Registries(ctx context.Context) ([]RegistryView, error)
	AddExternalRegistry(ctx context.Context, actor authz.Subject, in ExternalRegistryInput) (RegistryView, error)
}

// Unavailable implementations for unwired services.

// Plan implements ImageBuilds.
func (Unavailable) Plan(context.Context, authz.Subject, ImageBuildRequest) (ImageBuildPlan, error) {
	return ImageBuildPlan{}, ErrUnavailable
}

// Start implements ImageBuilds.
func (Unavailable) Start(context.Context, authz.Subject, ImageBuildRequest) (ImageBuildJob, error) {
	return ImageBuildJob{}, ErrUnavailable
}

// Jobs implements ImageBuilds.
func (Unavailable) Jobs(context.Context) ([]ImageBuildJob, error) { return nil, ErrUnavailable }

// Registries implements Registries.
func (Unavailable) Registries(context.Context) ([]RegistryView, error) { return nil, ErrUnavailable }

// AddExternalRegistry implements Registries.
func (Unavailable) AddExternalRegistry(context.Context, authz.Subject, ExternalRegistryInput) (RegistryView, error) {
	return RegistryView{}, ErrUnavailable
}

var (
	imageBuildResource = authz.Resource{Type: authz.ResourceImageBuild, Scope: authz.Global}
	registryResource   = authz.Resource{Type: authz.ResourceRegistry, Scope: authz.Global}
)

func (s *server) resolveImageBuild(*http.Request) (authz.Resource, any, error) {
	return imageBuildResource, nil, nil
}

func (s *server) resolveRegistry(*http.Request) (authz.Resource, any, error) {
	return registryResource, nil, nil
}

func (s *server) imageBuilds() ImageBuilds {
	if s.cfg.ImageBuilds != nil {
		return s.cfg.ImageBuilds
	}
	return Unavailable{}
}

func (s *server) registries() Registries {
	if s.cfg.Registries != nil {
		return s.cfg.Registries
	}
	return Unavailable{}
}

// handleImageBuildCreate answers a request without approvalDigest with the
// plan (HTTP 200, nothing built) and a request with one with the started
// job (HTTP 202). Both are audited as create decisions: the check runs
// before the body is read, so the preview cannot be told apart in advance.
func (s *server) handleImageBuildCreate(w http.ResponseWriter, r *http.Request, c *caller) {
	var in ImageBuildRequest
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.ApprovalDigest == "" {
		plan, err := s.imageBuilds().Plan(r.Context(), c.subject, in)
		if err != nil {
			s.writeServiceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, ImageBuildResult{Plan: &plan})
		return
	}
	job, err := s.imageBuilds().Start(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: image build started", "job", job.ID, "by", c.subject.ID, "specHash", job.SpecHash, "status", job.Status)
	writeJSON(w, http.StatusAccepted, ImageBuildResult{Job: &job})
}

func (s *server) handleImageBuilds(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.imageBuilds().Jobs(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if out == nil {
		out = []ImageBuildJob{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleRegistries(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.registries().Registries(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if out == nil {
		out = []RegistryView{}
	}
	writeJSON(w, http.StatusOK, out)
}

var registryFieldLimit = regexp.MustCompile(`^.{0,512}$`)

func (s *server) handleRegistryCreate(w http.ResponseWriter, r *http.Request, c *caller) {
	var in ExternalRegistryInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.ID == "" || in.Name == "" || in.Kind == "" ||
		!registryFieldLimit.MatchString(in.Endpoint) || !registryFieldLimit.MatchString(in.Username) || len(in.Password) > 4096 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.registries().AddExternalRegistry(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: external registry registered", "registry", out.ID, "by", c.subject.ID, "kind", out.Kind)
	writeJSON(w, http.StatusCreated, out)
}

// ImageBuildBackend adapts the imagebuild and registry services to the API
// interfaces and maps their errors to the API's sentinel errors. It is a
// fakes-only integration: the manager does not construct it yet.
type ImageBuildBackend struct {
	Builds *imagebuild.Service
	Regs   *registry.Service
}

var (
	_ ImageBuilds = (*ImageBuildBackend)(nil)
	_ Registries  = (*ImageBuildBackend)(nil)
)

func mapImageError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, imagebuild.ErrApprovalMismatch):
		return ErrApprovalMismatch
	case errors.Is(err, imagebuild.ErrInvalidSpec), errors.Is(err, registry.ErrInvalidConfig), errors.Is(err, registry.ErrInvalidName):
		return ErrInvalidInput
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, imagebuild.ErrJobNotFound):
		return ErrNotFound
	case errors.Is(err, registry.ErrExists):
		return ErrConflict
	case errors.Is(err, registry.ErrUnauthorized):
		// The supplied registry credentials were rejected: a client error,
		// not a manager authentication failure.
		return ErrInvalidInput
	}
	return err
}

// Plan implements ImageBuilds.
func (b *ImageBuildBackend) Plan(ctx context.Context, _ authz.Subject, req ImageBuildRequest) (ImageBuildPlan, error) {
	p, err := b.Builds.Plan(ctx, req)
	return p, mapImageError(err)
}

// Start implements ImageBuilds.
func (b *ImageBuildBackend) Start(ctx context.Context, a authz.Subject, req ImageBuildRequest) (ImageBuildJob, error) {
	j, err := b.Builds.Start(ctx, a.ID, req)
	return j, mapImageError(err)
}

// Jobs implements ImageBuilds.
func (b *ImageBuildBackend) Jobs(ctx context.Context) ([]ImageBuildJob, error) {
	j, err := b.Builds.List(ctx)
	return j, mapImageError(err)
}

func viewOf(c registry.Config) RegistryView {
	return RegistryView{ID: c.ID, Name: c.Name, Kind: c.Kind, Endpoint: c.Endpoint, Username: c.Username,
		CredentialSet: c.CredentialRef != "", CreatedBy: c.CreatedBy}
}

// Registries implements Registries.
func (b *ImageBuildBackend) Registries(ctx context.Context) ([]RegistryView, error) {
	cs, err := b.Regs.List(ctx)
	if err != nil {
		return nil, mapImageError(err)
	}
	out := make([]RegistryView, 0, len(cs))
	for _, c := range cs {
		out = append(out, viewOf(c))
	}
	return out, nil
}

// AddExternalRegistry implements Registries.
func (b *ImageBuildBackend) AddExternalRegistry(ctx context.Context, a authz.Subject, in ExternalRegistryInput) (RegistryView, error) {
	c, err := b.Regs.AddExternal(ctx, a.ID, registry.ExternalInput{
		ID: in.ID, Name: in.Name, Kind: in.Kind, Endpoint: in.Endpoint, Username: in.Username, Password: registry.NewSecret(in.Password),
	})
	if err != nil {
		return RegistryView{}, mapImageError(err)
	}
	return viewOf(c), nil
}
