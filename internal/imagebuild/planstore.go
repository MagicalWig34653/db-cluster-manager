// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"errors"
	"time"
)

// ErrPlanNotFound is returned for an unknown plan digest.
var ErrPlanNotFound = errors.New("imagebuild: plan not found")

// StoredPlan is a plan as kept for audit, with the time it was last
// previewed. A plan contains no secret (registry credentials are never part
// of a plan).
type StoredPlan struct {
	Plan Plan      `json:"plan"`
	At   time.Time `json:"at"`
}

// PlanStore persists previewed plans by digest.
type PlanStore interface {
	Put(ctx context.Context, p StoredPlan) error // insert or replace
	Get(ctx context.Context, digest string) (StoredPlan, error)
}
