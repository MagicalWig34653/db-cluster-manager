// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// ErrMajorUpgradeApproval maps a refused major/series path to a clear API code.
var ErrMajorUpgradeApproval = errors.New("major upgrades require product-level approval")

// ClusterOperations extends Control without changing existing implementations.
type ClusterOperations interface {
	PlanClusterOperation(context.Context, authz.Subject, string, galera.OperationRequest) (ClusterOperationPlan, error)
}

// ClusterOperationPlan is stored before execution; approval uses the exact
// runner definition digest, available again on GET /operations/{id}.
type ClusterOperationPlan struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Approval   OperationApproval `json:"approval"`
	Plan       galera.Plan       `json:"plan"`
	AgentSteps []runner.Step     `json:"agentSteps"`
}

func (s *server) handleClusterOperationPlan(w http.ResponseWriter, r *http.Request, c *caller) {
	var req galera.OperationRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	svc, ok := s.control().(ClusterOperations)
	if !ok {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	out, err := svc.PlanClusterOperation(r.Context(), c.subject, r.PathValue("id"), req)
	if errors.Is(err, ErrMajorUpgradeApproval) {
		writeError(w, http.StatusConflict, "major_upgrade_requires_product_approval")
		return
	}
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}
