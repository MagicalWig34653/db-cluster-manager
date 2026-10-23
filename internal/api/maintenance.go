// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// HostMaintenance is the maintenance state of a host (A08).
type HostMaintenance struct {
	Since      time.Time `json:"since"`
	By         string    `json:"by"`
	Reason     string    `json:"reason,omitempty"`
	Operations []string  `json:"operations"`
	// Ready reports that every maintenance operation succeeded: the
	// cluster nodes on the host are stopped and the operator may reboot it.
	Ready bool `json:"ready"`
}

// PlanStep is one step of a visible plan.
type PlanStep struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Slot string `json:"slot,omitempty"`
	Note string `json:"note,omitempty"`
	// Conditions are the preconditions in words.
	Conditions []string `json:"conditions,omitempty"`
}

// ClusterMaintenancePlan is the maintenance plan of one cluster with a
// node on the host. Refused is set (and Steps empty) when the cluster
// cannot safely lose its nodes on the host.
type ClusterMaintenancePlan struct {
	Cluster  string     `json:"cluster"`
	Name     string     `json:"name"`
	Slots    []string   `json:"slots"`
	Steps    []PlanStep `json:"steps"`
	Warnings []string   `json:"warnings,omitempty"`
	Refused  string     `json:"refused,omitempty"`
}

// MaintenancePreview is the read-only plan for entering (or leaving)
// maintenance. Safe is false when any cluster refuses.
type MaintenancePreview struct {
	Host     string                   `json:"host"`
	End      bool                     `json:"end"`
	Safe     bool                     `json:"safe"`
	Clusters []ClusterMaintenancePlan `json:"clusters"`
}

// MaintenanceStarted is the result of entering or leaving maintenance.
type MaintenanceStarted struct {
	Host       string             `json:"host"`
	Operations []OperationRef     `json:"operations"`
	Preview    MaintenancePreview `json:"plan"`
}

// MaintenanceInput enters maintenance.
type MaintenanceInput struct {
	Reason string `json:"reason"`
}

// DriftFinding is one difference between the desired state of a cluster
// and what the Swarm reports. Action is the proposed manual next step;
// nothing is changed automatically.
type DriftFinding struct {
	Kind    string `json:"kind"`
	Slot    string `json:"slot,omitempty"`
	Service string `json:"service,omitempty"`
	Detail  string `json:"detail"`
	Action  string `json:"action"`
}

// DriftReport compares a cluster's desired state with the Swarm.
type DriftReport struct {
	Cluster   string         `json:"cluster"`
	CheckedAt time.Time      `json:"checkedAt"`
	InSync    bool           `json:"inSync"`
	Findings  []DriftFinding `json:"findings"`
}

// Maintenance runs host maintenance and drift checks.
type Maintenance interface {
	MaintenancePlan(ctx context.Context, actor authz.Subject, swarm, nodeID string, end bool) (MaintenancePreview, error)
	EnterMaintenance(ctx context.Context, actor authz.Subject, swarm, nodeID, reason string) (MaintenanceStarted, error)
	LeaveMaintenance(ctx context.Context, actor authz.Subject, swarm, nodeID string) (MaintenanceStarted, error)
	ClusterDrift(ctx context.Context, actor authz.Subject, clusterID string) (DriftReport, error)
}

func (s *server) maintenance() Maintenance {
	if s.cfg.Maintenance != nil {
		return s.cfg.Maintenance
	}
	return Unavailable{}
}

// MaintenancePlan implements Maintenance.
func (Unavailable) MaintenancePlan(context.Context, authz.Subject, string, string, bool) (MaintenancePreview, error) {
	return MaintenancePreview{}, ErrUnavailable
}

// EnterMaintenance implements Maintenance.
func (Unavailable) EnterMaintenance(context.Context, authz.Subject, string, string, string) (MaintenanceStarted, error) {
	return MaintenanceStarted{}, ErrUnavailable
}

// LeaveMaintenance implements Maintenance.
func (Unavailable) LeaveMaintenance(context.Context, authz.Subject, string, string) (MaintenanceStarted, error) {
	return MaintenanceStarted{}, ErrUnavailable
}

// ClusterDrift implements Maintenance.
func (Unavailable) ClusterDrift(context.Context, authz.Subject, string) (DriftReport, error) {
	return DriftReport{}, ErrUnavailable
}

func (s *server) handleMaintenancePlan(w http.ResponseWriter, r *http.Request, c *caller) {
	h := c.target.(Host)
	end := r.URL.Query().Get("end") == "true"
	out, err := s.maintenance().MaintenancePlan(r.Context(), c.subject, h.Swarm, h.NodeID, end)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizePreview(out))
}

func (s *server) handleMaintenanceEnter(w http.ResponseWriter, r *http.Request, c *caller) {
	h := c.target.(Host)
	var in MaintenanceInput
	if !decodeJSON(w, r, &in, true) {
		return
	}
	if len(in.Reason) > 500 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.maintenance().EnterMaintenance(r.Context(), c.subject, h.Swarm, h.NodeID, in.Reason)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: host maintenance started", "host", h.NodeID, "by", c.subject.ID, "operations", len(out.Operations))
	out.Preview = normalizePreview(out.Preview)
	writeJSON(w, http.StatusAccepted, out)
}

func (s *server) handleMaintenanceLeave(w http.ResponseWriter, r *http.Request, c *caller) {
	h := c.target.(Host)
	out, err := s.maintenance().LeaveMaintenance(r.Context(), c.subject, h.Swarm, h.NodeID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: host maintenance ended", "host", h.NodeID, "by", c.subject.ID, "operations", len(out.Operations))
	out.Preview = normalizePreview(out.Preview)
	writeJSON(w, http.StatusAccepted, out)
}

func (s *server) handleClusterDrift(w http.ResponseWriter, r *http.Request, c *caller) {
	cl := c.target.(Cluster)
	out, err := s.maintenance().ClusterDrift(r.Context(), c.subject, cl.ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if out.Findings == nil {
		out.Findings = []DriftFinding{}
	}
	writeJSON(w, http.StatusOK, out)
}

func normalizePreview(p MaintenancePreview) MaintenancePreview {
	if p.Clusters == nil {
		p.Clusters = []ClusterMaintenancePlan{}
	}
	for i := range p.Clusters {
		if p.Clusters[i].Steps == nil {
			p.Clusters[i].Steps = []PlanStep{}
		}
	}
	return p
}
