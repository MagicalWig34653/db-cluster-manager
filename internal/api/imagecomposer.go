// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagecompose"
)

// ComposerPreviewRequest asks for the generated Dockerfile and files of a
// recipe. Nothing is built.
type ComposerPreviewRequest struct {
	Recipe imagecompose.Recipe `json:"recipe"`
}

// ComposerPreview is the review output of a recipe. Invalid recipes answer
// with Valid false and the findings, not with an error, so a form can show
// them while the user edits.
type ComposerPreview struct {
	Valid  bool                 `json:"valid"`
	Issues []imagecompose.Issue `json:"issues"`
	// Variants tell, per distribution release, whether the same component
	// choice can be built there.
	Variants    []imagecompose.Variant    `json:"variants"`
	Recipe      *imagecompose.Recipe      `json:"recipe,omitempty"`
	Digest      string                    `json:"digest,omitempty"`
	Role        string                    `json:"role,omitempty"`
	BaseImage   string                    `json:"baseImage,omitempty"`
	ContextHash string                    `json:"contextHash,omitempty"`
	Dockerfile  string                    `json:"dockerfile,omitempty"`
	Files       []imagebuild.File         `json:"files,omitempty"`
	Packages    []imagecompose.PackageRef `json:"packages,omitempty"`
	Modules     []string                  `json:"modules,omitempty"`
	Warnings    []string                  `json:"warnings,omitempty"`
	Notes       []string                  `json:"notes,omitempty"`
}

// ComposerBuildRequest plans a build of a recipe; with ApprovalDigest equal
// to the digest of the reviewed plan it starts the build.
type ComposerBuildRequest struct {
	Recipe         imagecompose.Recipe `json:"recipe"`
	Target         imagebuild.Target   `json:"target"`
	ApprovalDigest string              `json:"approvalDigest,omitempty"`
}

// ImageComposer backs the image composer endpoints (X17, ADR 0026, fakes
// only). ComposerPlan must not change state; builds go through the existing
// image build approval flow.
type ImageComposer interface {
	ComposerCatalog(ctx context.Context) (*imagecompose.Catalog, error)
	ComposerPreview(ctx context.Context, actor authz.Subject, r imagecompose.Recipe) (ComposerPreview, error)
	ComposerPlan(ctx context.Context, actor authz.Subject, req ComposerBuildRequest) (ImageBuildPlan, error)
	ComposerStart(ctx context.Context, actor authz.Subject, req ComposerBuildRequest) (ImageBuildJob, error)
}

// ComposerCatalog implements ImageComposer.
func (Unavailable) ComposerCatalog(context.Context) (*imagecompose.Catalog, error) {
	return nil, ErrUnavailable
}

// ComposerPreview implements ImageComposer.
func (Unavailable) ComposerPreview(context.Context, authz.Subject, imagecompose.Recipe) (ComposerPreview, error) {
	return ComposerPreview{}, ErrUnavailable
}

// ComposerPlan implements ImageComposer.
func (Unavailable) ComposerPlan(context.Context, authz.Subject, ComposerBuildRequest) (ImageBuildPlan, error) {
	return ImageBuildPlan{}, ErrUnavailable
}

// ComposerStart implements ImageComposer.
func (Unavailable) ComposerStart(context.Context, authz.Subject, ComposerBuildRequest) (ImageBuildJob, error) {
	return ImageBuildJob{}, ErrUnavailable
}

func (s *server) imageComposer() ImageComposer {
	if s.cfg.ImageComposer != nil {
		return s.cfg.ImageComposer
	}
	return Unavailable{}
}

func (s *server) handleComposerCatalog(w http.ResponseWriter, r *http.Request, _ *caller) {
	c, err := s.imageComposer().ComposerCatalog(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) handleComposerPreview(w http.ResponseWriter, r *http.Request, c *caller) {
	var in ComposerPreviewRequest
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.imageComposer().ComposerPreview(r.Context(), c.subject, in.Recipe)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleComposerBuild answers a request without approvalDigest with the
// build plan (200, nothing built) and one with it with the started job
// (202), like POST /image-builds.
func (s *server) handleComposerBuild(w http.ResponseWriter, r *http.Request, c *caller) {
	var in ComposerBuildRequest
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.ApprovalDigest == "" {
		plan, err := s.imageComposer().ComposerPlan(r.Context(), c.subject, in)
		if err != nil {
			s.writeServiceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, ImageBuildResult{Plan: &plan})
		return
	}
	job, err := s.imageComposer().ComposerStart(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: composer image build started", "job", job.ID, "by", c.subject.ID, "recipe", job.SpecHash, "status", job.Status)
	writeJSON(w, http.StatusAccepted, ImageBuildResult{Job: &job})
}

// ImageComposerBackend adapts the catalog and the image build service. The
// recipe is always generated server side from the validated recipe; the
// client never supplies a Dockerfile. Fakes-only integration: the manager
// does not construct it yet.
type ImageComposerBackend struct {
	// Cat is the catalog; nil selects the embedded default.
	Cat    *imagecompose.Catalog
	Builds *imagebuild.Service
}

var _ ImageComposer = (*ImageComposerBackend)(nil)

func (b *ImageComposerBackend) catalog() *imagecompose.Catalog {
	if b.Cat != nil {
		return b.Cat
	}
	return imagecompose.Default()
}

// ComposerCatalog implements ImageComposer.
func (b *ImageComposerBackend) ComposerCatalog(context.Context) (*imagecompose.Catalog, error) {
	return b.catalog(), nil
}

// ComposerPreview implements ImageComposer.
func (b *ImageComposerBackend) ComposerPreview(_ context.Context, _ authz.Subject, r imagecompose.Recipe) (ComposerPreview, error) {
	c := b.catalog()
	out := ComposerPreview{Issues: []imagecompose.Issue{}}
	res, err := c.Generate(r)
	if err != nil {
		var ve *imagecompose.ValidationError
		if !errors.As(err, &ve) {
			return ComposerPreview{}, err
		}
		out.Issues = ve.Issues
		out.Variants = c.Variants(r)
		return out, nil
	}
	out.Valid = true
	out.Variants = c.Variants(res.Recipe)
	out.Recipe, out.Digest, out.Role = &res.Recipe, res.Digest, res.Role
	out.BaseImage, out.ContextHash = res.Rendered.BaseImage, res.Rendered.ContextHash
	out.Dockerfile, out.Files = res.Rendered.Dockerfile, res.Rendered.Files
	out.Packages, out.Modules, out.Warnings, out.Notes = res.Packages, res.Modules, res.Warnings, res.Notes
	return out, nil
}

func (b *ImageComposerBackend) renderedRequest(req ComposerBuildRequest) (imagebuild.RenderedRequest, error) {
	res, err := b.catalog().Generate(req.Recipe)
	if err != nil {
		return imagebuild.RenderedRequest{}, err
	}
	raw, err := json.Marshal(res.Recipe)
	if err != nil {
		return imagebuild.RenderedRequest{}, err
	}
	return imagebuild.RenderedRequest{
		Name: res.Recipe.Name, Source: "composer", Recipe: raw, Rendered: res.Rendered,
		Platforms: res.Recipe.Platforms, Target: req.Target, ApprovalDigest: req.ApprovalDigest,
	}, nil
}

func mapComposerError(err error) error {
	if errors.Is(err, imagecompose.ErrInvalidRecipe) {
		return ErrInvalidInput
	}
	return mapImageError(err)
}

// ComposerPlan implements ImageComposer.
func (b *ImageComposerBackend) ComposerPlan(ctx context.Context, _ authz.Subject, req ComposerBuildRequest) (ImageBuildPlan, error) {
	rr, err := b.renderedRequest(req)
	if err != nil {
		return ImageBuildPlan{}, mapComposerError(err)
	}
	p, err := b.Builds.PlanRendered(ctx, rr)
	return p, mapComposerError(err)
}

// ComposerStart implements ImageComposer.
func (b *ImageComposerBackend) ComposerStart(ctx context.Context, a authz.Subject, req ComposerBuildRequest) (ImageBuildJob, error) {
	rr, err := b.renderedRequest(req)
	if err != nil {
		return ImageBuildJob{}, mapComposerError(err)
	}
	j, err := b.Builds.StartRendered(ctx, a.ID, rr)
	return j, mapComposerError(err)
}
