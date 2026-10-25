// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// EngineUpdatePlan is a read-only, single-host proposal. Exportable means the
// runbook can be exported, not that the manager will execute an update.
type EngineUpdatePlan struct {
	Swarm             string             `json:"swarm"`
	Host              string             `json:"host"`
	CurrentVersion    string             `json:"currentVersion"`
	TargetVersion     string             `json:"targetVersion,omitempty"`
	AvailableVersions []string           `json:"availableVersions"`
	Compatibility     string             `json:"compatibility,omitempty"`
	Maintenance       MaintenancePreview `json:"maintenance"`
	Steps             []string           `json:"steps"`
	Warnings          []string           `json:"warnings"`
	Blockers          []string           `json:"blockers"`
	RollbackNotes     []string           `json:"rollbackNotes"`
	PinnedManager     bool               `json:"pinnedManager"`
	ApprovalRequired  bool               `json:"approvalRequired"`
	Exportable        bool               `json:"exportable"`
	Digest            string             `json:"digest"`
}

type EngineRunbookRequest struct {
	TargetVersion        string `json:"targetVersion"`
	Digest               string `json:"digest"`
	ConfirmPinnedManager bool   `json:"confirmPinnedManager,omitempty"`
}

type EngineRunbook struct {
	Digest string `json:"digest"`
	Script string `json:"script"`
}

type EngineUpdates interface {
	EngineUpdatePlan(ctx context.Context, actor authz.Subject, swarm, nodeID, target string) (EngineUpdatePlan, error)
	ExportEngineRunbook(ctx context.Context, actor authz.Subject, swarm, nodeID string, input EngineRunbookRequest) (EngineRunbook, error)
}

func (s *server) resolveEngineUpdateHost(r *http.Request) (authz.Resource, any, error) {
	_, target, err := s.resolveHost(r)
	if err != nil {
		return authz.Resource{}, nil, err
	}
	h := target.(Host)
	return authz.Resource{Type: authz.ResourceEngineUpdate, Scope: authz.SwarmScope(h.Swarm), ID: h.NodeID}, h, nil
}

func (s *server) engineUpdates() EngineUpdates {
	if s.cfg.EngineUpdates != nil {
		return s.cfg.EngineUpdates
	}
	return nil
}

func (s *server) handleEngineUpdatePlan(w http.ResponseWriter, r *http.Request, c *caller) {
	if s.engineUpdates() == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	h := c.target.(Host)
	plan, err := s.engineUpdates().EngineUpdatePlan(r.Context(), c.subject, h.Swarm, h.NodeID, r.URL.Query().Get("targetVersion"))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *server) handleEngineUpdateRunbook(w http.ResponseWriter, r *http.Request, c *caller) {
	if s.engineUpdates() == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	var input EngineRunbookRequest
	if !decodeJSON(w, r, &input, true) {
		return
	}
	if strings.TrimSpace(input.TargetVersion) == "" || len(input.TargetVersion) > 80 || len(input.Digest) != 71 || !strings.HasPrefix(input.Digest, "sha256:") {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	h := c.target.(Host)
	book, err := s.engineUpdates().ExportEngineRunbook(r.Context(), c.subject, h.Swarm, h.NodeID, input)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, book)
}
