// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

import (
	"context"
	"errors"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
)

// Built is a finished image build that may be offered to the services that
// run the same repository (link with the vulnerability response, X13).
type Built struct {
	JobID     string `json:"jobId"`
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
	// FindingID is the finding the build answers, when known.
	FindingID string `json:"findingId,omitempty"`
}

// BuiltFromJob converts a succeeded image build job. A failed or running
// build, or one without a recorded digest, is refused.
func BuiltFromJob(j imagebuild.Job, findingID string) (Built, error) {
	if j.Status != imagebuild.JobSucceeded {
		return Built{}, fmt.Errorf("%w: build %s did not succeed", ErrInvalid, j.ID)
	}
	if !ValidDigest(j.ImageDigest) {
		return Built{}, fmt.Errorf("%w: build %s has no image digest", ErrInvalid, j.ID)
	}
	if _, err := ParseRef(j.Reference); err != nil {
		return Built{}, fmt.Errorf("%w: build %s: %v", ErrInvalid, j.ID, err)
	}
	return Built{JobID: j.ID, Reference: j.Reference, Digest: j.ImageDigest, FindingID: findingID}, nil
}

// OfferBuilt proposes the built image as an update candidate for every
// service in scope that runs the same repository. The plans have the kind
// "built" and are never approved by a policy: a patched image built after a
// vulnerability is a decision for a person. Services of other repositories
// are not touched.
func (s *Service) OfferBuilt(ctx context.Context, actor string, b Built) ([]Plan, error) {
	ref, err := ParseRef(b.Reference)
	if err != nil || ref.Tag == "" || !ValidDigest(b.Digest) {
		return nil, ErrInvalid
	}
	views, err := s.Services(ctx)
	if err != nil {
		return nil, err
	}
	var out []Plan
	var errs []error
	for _, v := range views {
		cur, err := ParseRef(v.Service.Image)
		if err != nil || !cur.SameRepo(ref) {
			continue
		}
		if cur.Tag == ref.Tag && cur.Digest == b.Digest {
			continue // already running
		}
		curDigest := cur.Digest
		if curDigest == "" && v.State.Baseline != nil && v.State.Baseline.Tag == cur.Tag {
			curDigest = v.State.Baseline.Digest
		}
		pol := v.Policy
		pol.Mode = ModeManual
		p, err := s.makePlan(ctx, actor, v.Service, v.Scope, pol, cur, curDigest,
			Image{Ref: ref.Pinned(ref.Tag, b.Digest).String(), Tag: ref.Tag, Digest: b.Digest}, KindBuilt, SourceImageBuild, b.JobID, false)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, errors.Join(errs...)
}
