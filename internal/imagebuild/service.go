// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// RegistryResolver provides the configuration and stored credential of a
// registry (registry.Service implements it).
type RegistryResolver interface {
	Credential(ctx context.Context, id string) (registry.Config, registry.Secret, error)
}

// Request asks for a plan or, with ApprovalDigest, a build.
type Request struct {
	Spec   WebServerSpec `json:"spec"`
	Target Target        `json:"target"`
	// ApprovalDigest must equal Plan.Digest of the plan the user reviewed.
	ApprovalDigest string `json:"approvalDigest,omitempty"`
	// ExecutionID is a durable caller-supplied idempotency key. Repeated
	// starts with this ID return the existing job without running the builder.
	ExecutionID string `json:"-"`
}

// DefaultMaxLogBytes caps the retained build log.
const DefaultMaxLogBytes = 256 << 10

// Service plans, approves and records builds. It never builds anything
// itself: that is the Builder's job.
type Service struct {
	Builder Builder
	Jobs    JobStore
	// Plans, when set, keeps the plans that were previewed or approved so
	// that an approval can be audited later (keyed by plan digest). A
	// failure to store a plan never blocks a preview.
	Plans      PlanStore
	Registries RegistryResolver
	Catalog    *Catalog
	Now        func() time.Time
	// IDFunc generates job IDs; nil selects random hex.
	IDFunc func() string
	// Async runs approved builds in a goroutine; tests leave it false.
	Async       bool
	MaxLogBytes int
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) newID() string {
	if s.IDFunc != nil {
		return s.IDFunc()
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ib-" + hex.EncodeToString(b[:])
}

// reference builds host/repository:tag and the docker config server key.
func reference(cfg registry.Config, t Target) (ref, authServer string, err error) {
	switch cfg.Kind {
	case registry.KindManaged:
		return "", "", fmt.Errorf("%w: a managed registry cannot be a build target until the service framework deploys it", registry.ErrInvalidConfig)
	case registry.KindDockerHub:
		return "docker.io/" + t.Repository + ":" + t.Tag, "https://index.docker.io/v1/", nil
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return "", "", registry.ErrInvalidConfig
	}
	return u.Host + "/" + t.Repository + ":" + t.Tag, u.Host, nil
}

// Plan validates the request and returns the preview. It resolves the
// registry (so a missing one is reported before approval) but changes
// nothing and builds nothing.
func (s *Service) Plan(ctx context.Context, req Request) (Plan, error) {
	p, _, _, _, err := s.plan(ctx, req)
	return p, err
}

func (s *Service) plan(ctx context.Context, req Request) (Plan, registry.Config, registry.Secret, string, error) {
	p, err := NewPlan(req.Spec, req.Target, s.Catalog)
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

// Start verifies the approval against a freshly computed plan, records a
// running job and builds. With Async the build continues in the background
// and the returned job is the running record.
func (s *Service) Start(ctx context.Context, actor string, req Request) (Job, error) {
	p, cfg, sec, server, err := s.plan(ctx, req)
	if err != nil {
		return Job{}, err
	}
	if err := p.CheckApproval(req.ApprovalDigest); err != nil {
		return Job{}, err
	}
	n := req.Spec.Normalize()
	job := Job{
		Name: n.Name, Status: JobRunning, Spec: n, SpecHash: p.SpecHash, ContextHash: p.ContextHash,
		PlanDigest: p.Digest, Reference: p.Reference, Platforms: p.Platforms, SBOM: SBOMNotGenerated,
		RequestedBy: actor,
	}
	rendered, err := Render(req.Spec, s.Catalog)
	if err != nil {
		return Job{}, err
	}
	return s.begin(ctx, job, rendered, req.ExecutionID, cfg, sec, server)
}

// begin records the running job (idempotent on executionID) and builds.
func (s *Service) begin(ctx context.Context, job Job, rendered Rendered, executionID string, cfg registry.Config, sec registry.Secret, server string) (Job, error) {
	job.ID = s.newID()
	if executionID != "" {
		job.ID = executionID
	}
	job.CreatedAt = s.now()
	if err := s.Jobs.Create(ctx, job); err != nil {
		if errors.Is(err, ErrJobExists) && executionID != "" {
			old, getErr := s.Jobs.Get(ctx, executionID)
			if getErr != nil {
				return Job{}, getErr
			}
			if old.PlanDigest != job.PlanDigest {
				return Job{}, ErrJobExists
			}
			return old, nil
		}
		return Job{}, err
	}
	in := BuildInput{Rendered: rendered, Platforms: job.Platforms, Reference: job.Reference, Push: true}
	red := NewRedactor()
	if !sec.IsZero() {
		in.Auth = &RegistryAuth{Server: server, Username: cfg.Username, Password: sec}
		red.Add(sec.Reveal())
		red.AddBasicAuth(cfg.Username, sec.Reveal())
	}
	run := func(ctx context.Context) Job {
		return s.run(ctx, job, in, red)
	}
	if s.Async {
		go run(context.WithoutCancel(ctx))
		return job, nil
	}
	return run(ctx), nil
}

func (s *Service) run(ctx context.Context, job Job, in BuildInput, red *Redactor) Job {
	max := s.MaxLogBytes
	if max <= 0 {
		max = DefaultMaxLogBytes
	}
	lc := &logCapture{r: red, max: max}
	in.Log = lc
	out, err := s.Builder.Build(ctx, in)
	lc.finish()
	job.Log, job.LogTruncated, job.LogSize = lc.out.String(), lc.truncated, lc.out.Len()
	job.FinishedAt = s.now()
	if err != nil {
		job.Status, job.Error = JobFailed, red.Redact(err.Error())
	} else {
		job.Status, job.ImageDigest, job.BaseDigests, job.SBOM = JobSucceeded, out.ImageDigest, out.BaseDigests, out.SBOM
	}
	// The record must be stored even when the request context is gone.
	_ = s.Jobs.Put(context.WithoutCancel(ctx), job)
	return job
}

// List returns jobs newest first without their logs.
func (s *Service) List(ctx context.Context) ([]Job, error) {
	jobs, err := s.Jobs.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		jobs[i].LogSize = len(jobs[i].Log)
		jobs[i].Log = ""
	}
	return jobs, nil
}

// Get returns one job with its log.
func (s *Service) Get(ctx context.Context, id string) (Job, error) {
	return s.Jobs.Get(ctx, strings.TrimSpace(id))
}

func (s *Service) savePlan(ctx context.Context, p Plan) {
	if s.Plans != nil {
		_ = s.Plans.Put(ctx, StoredPlan{Plan: p, At: s.now()})
	}
}

// GetPlan returns a stored plan by digest.
func (s *Service) GetPlan(ctx context.Context, digest string) (StoredPlan, error) {
	if s.Plans == nil {
		return StoredPlan{}, ErrPlanNotFound
	}
	return s.Plans.Get(ctx, strings.TrimSpace(digest))
}

// RecoverInterrupted marks jobs that are still "running" as failed. A
// manager restart or leadership change ends the build goroutine, so such a
// record would otherwise stay running forever. It returns the number of
// jobs changed.
func (s *Service) RecoverInterrupted(ctx context.Context) (int, error) {
	jobs, err := s.Jobs.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs {
		if j.Status != JobRunning {
			continue
		}
		j.Status, j.Error, j.FinishedAt = JobFailed, "interrupted: the manager restarted or lost leadership during the build", s.now()
		if err := s.Jobs.Put(ctx, j); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
