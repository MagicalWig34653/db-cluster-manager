// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"errors"
	"sync"
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

// MemoryPlanStore is an in-memory PlanStore for tests (a fake).
type MemoryPlanStore struct {
	mu sync.Mutex
	m  map[string]StoredPlan
}

// Put implements PlanStore.
func (s *MemoryPlanStore) Put(_ context.Context, p StoredPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]StoredPlan{}
	}
	s.m[p.Plan.Digest] = p
	return nil
}

// Get implements PlanStore.
func (s *MemoryPlanStore) Get(_ context.Context, digest string) (StoredPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[digest]
	if !ok {
		return StoredPlan{}, ErrPlanNotFound
	}
	return p, nil
}
