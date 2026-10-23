// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// Managed services of a service type (W07: generic container, Traefik).
// Status: implemented with fakes only; no Swarm has run these routes.
//
// Creating and deleting a service is a two-step flow that reuses the
// operation approval machinery: the request stores an operation whose steps
// wait for an approval bound to the digest of the planned definition, the
// response carries the plan and that digest, and POST
// /operations/{id}/approve (permission: create or delete on the service
// resource in the Swarm) lets it run. A plan with blockers is previewed but
// the request that would start it is refused with 409 conflict.

// Operation kinds of service operations (internal/runner OpServiceCreate and
// OpServiceDelete have the same values).
const (
	OperationKindServiceCreate = "service/create"
	OperationKindServiceDelete = "service/delete"
)

// ServiceInput requests a service. Spec is the type-specific desired state
// (strict JSON, never secret values).
type ServiceInput struct {
	Swarm string          `json:"swarm"`
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Spec  json.RawMessage `json:"spec"`
}

// ServiceInfo is one managed service.
type ServiceInfo struct {
	ID        string          `json:"id"`
	Swarm     string          `json:"swarm"`
	Name      string          `json:"name"`
	Type      string          `json:"type"`
	Spec      json.RawMessage `json:"spec"`
	CreatedBy string          `json:"createdBy,omitempty"`
	CreatedAt time.Time       `json:"createdAt,omitzero"`
	// CreateOperation and DeleteOperation name the operations that created
	// the service or (while it is being removed) delete it.
	CreateOperation string `json:"createOperation,omitempty"`
	DeleteOperation string `json:"deleteOperation,omitempty"`
}

// ServiceArtifact is a generated document shown before approval.
type ServiceArtifact struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Content   string `json:"content"`
}

// ServicePlan is the human-readable plan of a create or delete request.
type ServicePlan struct {
	Kind      string            `json:"kind"`
	Type      string            `json:"type"`
	Name      string            `json:"name"`
	Steps     []PlanStep        `json:"steps"`
	Artifacts []ServiceArtifact `json:"artifacts"`
	Warnings  []string          `json:"warnings"`
	// Blockers are reasons the plan cannot be started with the current agent
	// contract. Executable is false when there are any.
	Blockers         []string `json:"blockers"`
	Executable       bool     `json:"executable"`
	RequiresApproval bool     `json:"requiresApproval"`
	Text             string   `json:"text"`
}

// ServiceStarted is a started create or delete operation waiting for its
// approval.
type ServiceStarted struct {
	Service   string            `json:"service"`
	Operation OperationRef      `json:"operation"`
	Approval  OperationApproval `json:"approval"`
	Plan      ServicePlan       `json:"plan"`
}

// ServiceDeleteInput selects whether the managed volumes are removed too.
// Default: the data is kept.
type ServiceDeleteInput struct {
	RemoveVolumes bool `json:"removeVolumes,omitempty"`
}

// Services runs the service commands. The API authorizes (and audits) first;
// implementations check again. A nil service answers 503.
type Services interface {
	Services(ctx context.Context) ([]ServiceInfo, error)
	// Service returns ErrNotFound for an unknown ID.
	Service(ctx context.Context, id string) (ServiceInfo, error)
	// PreviewService plans without changing anything.
	PreviewService(ctx context.Context, actor authz.Subject, in ServiceInput) (ServicePlan, error)
	CreateService(ctx context.Context, actor authz.Subject, in ServiceInput) (ServiceStarted, error)
	PreviewServiceDelete(ctx context.Context, actor authz.Subject, id string, removeVolumes bool) (ServicePlan, error)
	DeleteService(ctx context.Context, actor authz.Subject, id string, removeVolumes bool) (ServiceStarted, error)
}

// Services of Unavailable: every call fails with ErrUnavailable.
func (Unavailable) Services(context.Context) ([]ServiceInfo, error) { return nil, ErrUnavailable }
func (Unavailable) Service(context.Context, string) (ServiceInfo, error) {
	return ServiceInfo{}, ErrUnavailable
}
func (Unavailable) PreviewService(context.Context, authz.Subject, ServiceInput) (ServicePlan, error) {
	return ServicePlan{}, ErrUnavailable
}
func (Unavailable) CreateService(context.Context, authz.Subject, ServiceInput) (ServiceStarted, error) {
	return ServiceStarted{}, ErrUnavailable
}
func (Unavailable) PreviewServiceDelete(context.Context, authz.Subject, string, bool) (ServicePlan, error) {
	return ServicePlan{}, ErrUnavailable
}
func (Unavailable) DeleteService(context.Context, authz.Subject, string, bool) (ServiceStarted, error) {
	return ServiceStarted{}, ErrUnavailable
}

func (s *server) services() Services {
	if s.cfg.Services != nil {
		return s.cfg.Services
	}
	return Unavailable{}
}

func serviceResource(si ServiceInfo) authz.Resource {
	return authz.Resource{Type: authz.ResourceService, Scope: authz.SwarmScope(si.Swarm), ID: si.ID}
}

func (s *server) handleServices(w http.ResponseWriter, r *http.Request, c *caller) {
	items, err := s.services().Services(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, serviceResource)
	if items == nil {
		items = []ServiceInfo{}
	}
	writeJSON(w, http.StatusOK, List[ServiceInfo]{Items: items})
}

func (s *server) resolveService(r *http.Request) (authz.Resource, any, error) {
	si, err := s.services().Service(r.Context(), r.PathValue("id"))
	if err != nil {
		return authz.Resource{}, nil, err
	}
	return serviceResource(si), si, nil
}

func (s *server) handleService(w http.ResponseWriter, _ *http.Request, c *caller) {
	writeJSON(w, http.StatusOK, c.target.(ServiceInfo))
}

// resolveNewService reads the body: the target Swarm decides the scope.
func (s *server) resolveNewService(r *http.Request) (authz.Resource, any, error) {
	var in ServiceInput
	if err := readJSON(r, &in, false); err != nil {
		return authz.Resource{}, nil, err
	}
	if in.Swarm == "" {
		return authz.Resource{}, nil, ErrInvalidInput
	}
	return authz.Resource{Type: authz.ResourceService, Scope: authz.SwarmScope(in.Swarm)}, in, nil
}

func validServiceInput(in ServiceInput) bool {
	return validName(in.Name, 31) && validName(in.Type, 31) && len(in.Spec) > 0 && len(in.Spec) <= 256<<10
}

func (s *server) handleServicePreview(w http.ResponseWriter, r *http.Request, c *caller) {
	in := c.target.(ServiceInput)
	if !validServiceInput(in) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	p, err := s.services().PreviewService(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeServicePlan(p))
}

func (s *server) handleServiceCreate(w http.ResponseWriter, r *http.Request, c *caller) {
	in := c.target.(ServiceInput)
	if !validServiceInput(in) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.services().CreateService(r.Context(), c.subject, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: service creation requested", "service", out.Service, "operation", out.Operation.ID, "by", c.subject.ID)
	out.Plan = normalizeServicePlan(out.Plan)
	writeJSON(w, http.StatusAccepted, out)
}

func (s *server) handleServiceDeletePreview(w http.ResponseWriter, r *http.Request, c *caller) {
	si := c.target.(ServiceInfo)
	p, err := s.services().PreviewServiceDelete(r.Context(), c.subject, si.ID, r.URL.Query().Get("removeVolumes") == "true")
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeServicePlan(p))
}

func (s *server) handleServiceDelete(w http.ResponseWriter, r *http.Request, c *caller) {
	si := c.target.(ServiceInfo)
	var in ServiceDeleteInput
	if !decodeJSON(w, r, &in, true) {
		return
	}
	out, err := s.services().DeleteService(r.Context(), c.subject, si.ID, in.RemoveVolumes)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: service deletion requested", "service", si.ID, "operation", out.Operation.ID, "by", c.subject.ID)
	out.Plan = normalizeServicePlan(out.Plan)
	writeJSON(w, http.StatusAccepted, out)
}

func normalizeServicePlan(p ServicePlan) ServicePlan {
	if p.Steps == nil {
		p.Steps = []PlanStep{}
	}
	if p.Artifacts == nil {
		p.Artifacts = []ServiceArtifact{}
	}
	if p.Warnings == nil {
		p.Warnings = []string{}
	}
	if p.Blockers == nil {
		p.Blockers = []string{}
	}
	// Every plan changes state; the flag cannot be turned off by a service.
	p.RequiresApproval = true
	p.Executable = len(p.Blockers) == 0
	return p
}
