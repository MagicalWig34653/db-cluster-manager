// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func renderedFixture() RenderedRequest {
	r := Rendered{
		SpecHash:   "sha256:" + strings.Repeat("b", 64),
		Dockerfile: "FROM scratch\n",
		Files:      []File{{Path: "a.conf", Mode: 0o644, Content: "x"}},
		BaseImage:  "docker.io/library/alpine:3.20@" + baseDigest,
	}
	r.ContextHash = ComputeContextHash(r)
	return RenderedRequest{
		Name: "composed", Source: "composer", Recipe: json.RawMessage(`{"name":"composed"}`),
		Rendered: r, Platforms: []string{"linux/amd64"},
		Target: Target{Registry: "ghcr-main", Repository: "team/shop", Tag: "1"},
	}
}

func TestRenderedPlanApprovalAndJob(t *testing.T) {
	ctx := context.Background()
	fb := &FakeBuilder{}
	s := newService(fb)
	req := renderedFixture()
	plan, err := s.PlanRendered(ctx, req)
	if err != nil || plan.Digest == "" || plan.Reference == "" || len(fb.Calls) != 0 {
		t.Fatalf("%+v %v", plan, err)
	}
	if _, err := s.StartRendered(ctx, "admin", req); !errors.Is(err, ErrApprovalMismatch) || len(fb.Calls) != 0 {
		t.Fatalf("started without approval: %v", err)
	}
	req.ApprovalDigest = plan.Digest
	job, err := s.StartRendered(ctx, "admin", req)
	if err != nil || job.Status != JobSucceeded || job.Source != "composer" || job.SpecHash != req.Rendered.SpecHash || len(fb.Calls) != 1 {
		t.Fatalf("%+v %v", job, err)
	}
	if string(job.Recipe) != `{"name":"composed"}` || job.ImageDigest == "" {
		t.Fatalf("%+v", job)
	}
	// A plan for another target needs its own approval.
	other := req
	other.Target.Tag = "2"
	if _, err := s.StartRendered(ctx, "admin", other); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("approval reused for another target: %v", err)
	}
}

func TestRenderedPlanRejectsTamperedContext(t *testing.T) {
	req := renderedFixture()
	req.Rendered.Dockerfile += "RUN id\n"
	if _, err := NewPlanFromRendered(req); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("tampered context accepted: %v", err)
	}
	req = renderedFixture()
	req.Platforms = nil
	if _, err := NewPlanFromRendered(req); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("incomplete request accepted: %v", err)
	}
}
