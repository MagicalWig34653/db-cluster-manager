// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// RenderedRequest asks for a plan or build of a context that another
// generator already rendered (the image composer, internal/imagecompose).
// The caller must render server side from a validated recipe; a client never
// supplies Rendered. Rendered.SpecHash is the recipe digest.
type RenderedRequest struct {
	Name string
	// Source names the generator ("composer").
	Source string
	// Recipe is the normalized recipe as JSON, kept on the job for audit.
	Recipe         json.RawMessage
	Rendered       Rendered
	Platforms      []string
	Target         Target
	ApprovalDigest string
	ExecutionID    string
}

// NewPlanFromRendered describes the build of an already rendered context.
// Like NewPlan it is pure: no network, no registry lookup, no build.
func NewPlanFromRendered(req RenderedRequest) (Plan, error) {
	if err := req.Target.Validate(); err != nil {
		return Plan{}, err
	}
	if req.Rendered.Dockerfile == "" || req.Rendered.SpecHash == "" || len(req.Platforms) == 0 {
		return Plan{}, invalid("rendered context is incomplete")
	}
	if ComputeContextHash(req.Rendered) != req.Rendered.ContextHash {
		return Plan{}, invalid("rendered context does not match its context hash")
	}
	r, t := req.Rendered, req.Target
	p := Plan{
		SpecHash: r.SpecHash, ContextHash: r.ContextHash, BaseImage: r.BaseImage,
		Platforms: req.Platforms, Target: t, Dockerfile: r.Dockerfile, Files: r.Files,
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"dbcm-imagebuild-plan-rendered-v1", req.Source, r.SpecHash, r.ContextHash, strings.Join(req.Platforms, ","),
		t.Registry, t.Repository, t.Tag,
	}, "\n")))
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	target := fmt.Sprintf("%s/%s:%s", t.Registry, t.Repository, t.Tag)
	p.Steps = []Step{
		{1, "Generate build context", fmt.Sprintf("%d files plus a Dockerfile (context hash %s) from the %s recipe %s.", len(r.Files), short(r.ContextHash), req.Source, short(r.SpecHash))},
		{2, "Build", fmt.Sprintf("Build for %s from base %s using BuildKit; the base image is pinned by digest.", strings.Join(req.Platforms, ", "), r.BaseImage)},
		{3, "Verify configuration", "The Dockerfile tests the generated configuration during the build; a configuration error fails the build before any push."},
		{4, "Push", "Push the image to " + target + " using the stored credential of that registry; the credential is never written to logs or the build context."},
		{5, "Record", "Record recipe digest, base digest, resulting image digest and the redacted build log. An SBOM is not generated yet (placeholder only)."},
	}
	p.Warnings = []string{
		"Distribution package patch versions float within their constraint; only the base image is pinned by digest.",
		"No vulnerability scan and no image signature are produced yet.",
	}
	if len(req.Platforms) > 1 {
		p.Warnings = append(p.Warnings, "Multi-platform builds are pushed directly to the registry and need emulation or native builders; nothing was verified on this manager.")
	}
	p.Explanation = fmt.Sprintf("Image %q from %s recipe %s. Base image: %s. Platforms: %s. Target: %s. Context hash %s: the same recipe always produces the same Dockerfile and files.",
		req.Name, req.Source, short(r.SpecHash), r.BaseImage, strings.Join(req.Platforms, ", "), target, short(r.ContextHash))
	return p, nil
}

func (s *Service) planRendered(ctx context.Context, req RenderedRequest) (Plan, registry.Config, registry.Secret, string, error) {
	p, err := NewPlanFromRendered(req)
	if err != nil {
		return Plan{}, registry.Config{}, registry.Secret{}, "", err
	}
	cfg, sec, err := s.Registries.Credential(ctx, req.Target.Registry)
	if err != nil {
		return Plan{}, registry.Config{}, registry.Secret{}, "", err
	}
	ref, server, err := reference(cfg, req.Target)
	if err != nil {
		return Plan{}, registry.Config{}, registry.Secret{}, "", err
	}
	p.Reference = ref
	s.savePlan(ctx, p)
	return p, cfg, sec, server, nil
}

// PlanRendered validates the request and returns the preview; it changes
// nothing and builds nothing.
func (s *Service) PlanRendered(ctx context.Context, req RenderedRequest) (Plan, error) {
	p, _, _, _, err := s.planRendered(ctx, req)
	return p, err
}

// StartRendered verifies the approval against a freshly computed plan and
// builds, exactly like Start.
func (s *Service) StartRendered(ctx context.Context, actor string, req RenderedRequest) (Job, error) {
	p, cfg, sec, server, err := s.planRendered(ctx, req)
	if err != nil {
		return Job{}, err
	}
	if err := p.CheckApproval(req.ApprovalDigest); err != nil {
		return Job{}, err
	}
	job := Job{
		Name: req.Name, Status: JobRunning, Source: req.Source, Recipe: req.Recipe,
		SpecHash: p.SpecHash, ContextHash: p.ContextHash, PlanDigest: p.Digest, Reference: p.Reference,
		Platforms: p.Platforms, SBOM: SBOMNotGenerated, RequestedBy: actor,
	}
	return s.begin(ctx, job, req.Rendered, req.ExecutionID, cfg, sec, server)
}
