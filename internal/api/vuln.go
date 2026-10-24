// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// Vulnerability response endpoints (X13, ADR 0015, proposal; fakes only).
// Every route needs a permission on the global resource "vuln", which no
// preset except the administrator holds. Reads need read; polling and
// requesting a proposal need operate; status decisions, approvals, the
// policy and provider settings need update (providers: create).

// VulnStatusInput is a VEX decision on a finding.
type VulnStatusInput struct {
	Status        vuln.Status `json:"status"`
	Justification string      `json:"justification,omitempty"`
}

// VulnApproveInput approves the plan with this digest.
type VulnApproveInput struct {
	Digest string `json:"digest"`
}

// VulnDeclineInput declines a proposal.
type VulnDeclineInput struct {
	Reason string `json:"reason,omitempty"`
}

// VulnPolicyView is the policy with its version for conditional updates.
type VulnPolicyView struct {
	Policy  vuln.Policy `json:"policy"`
	Version uint64      `json:"version"`
}

// VulnPolicyInput replaces the policy if Version still matches.
type VulnPolicyInput struct {
	Policy  vuln.Policy `json:"policy"`
	Version uint64      `json:"version"`
}

// VulnProviderInput registers an AI provider. APIKey is accepted once and
// stored as a secret; it is never returned.
type VulnProviderInput struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Kind            ai.Kind   `json:"kind"`
	BaseURL         string    `json:"baseUrl,omitempty"`
	Models          []string  `json:"models,omitempty"`
	DefaultModel    string    `json:"defaultModel,omitempty"`
	Binary          string    `json:"binary,omitempty"`
	Home            string    `json:"home,omitempty"`
	Experimental    bool      `json:"experimental,omitempty"`
	Limits          ai.Limits `json:"limits"`
	MaxOutputTokens int       `json:"maxOutputTokens,omitempty"`
	TimeoutSeconds  int       `json:"timeoutSeconds,omitempty"`
	APIKey          string    `json:"apiKey,omitempty"`
}

// VulnConsentInput grants (Statement must equal the provider's current
// egress statement) or revokes data-egress consent.
type VulnConsentInput struct {
	Grant     bool   `json:"grant"`
	Statement string `json:"statement,omitempty"`
}

// Vuln backs the /vuln endpoints.
type Vuln interface {
	Findings(ctx context.Context) ([]vuln.Finding, error)
	Finding(ctx context.Context, id string) (vuln.Finding, error)
	SetFindingStatus(ctx context.Context, actor authz.Subject, id string, in VulnStatusInput) (vuln.Finding, error)
	Propose(ctx context.Context, actor authz.Subject, findingID string, o vuln.ProposeOptions) (vuln.Proposal, error)
	Proposals(ctx context.Context) ([]vuln.Proposal, error)
	Approve(ctx context.Context, actor authz.Subject, id string, in VulnApproveInput) (vuln.Proposal, error)
	Decline(ctx context.Context, actor authz.Subject, id string, in VulnDeclineInput) (vuln.Proposal, error)
	Poll(ctx context.Context, actor authz.Subject) (vuln.PollReport, error)
	Feeds(ctx context.Context) ([]vuln.FeedState, error)
	Policy(ctx context.Context) (VulnPolicyView, error)
	SetPolicy(ctx context.Context, actor authz.Subject, in VulnPolicyInput) (vuln.Policy, error)
	Providers(ctx context.Context) ([]vuln.ProviderView, error)
	AddProvider(ctx context.Context, actor authz.Subject, in VulnProviderInput) (vuln.ProviderView, error)
	SetConsent(ctx context.Context, actor authz.Subject, id string, in VulnConsentInput) (vuln.ProviderView, error)
	Audit(ctx context.Context) ([]vuln.AuditRecord, error)
}

// unavailableVuln answers every call with ErrUnavailable (503).
type unavailableVuln struct{}

func (unavailableVuln) Findings(context.Context) ([]vuln.Finding, error) { return nil, ErrUnavailable }
func (unavailableVuln) Finding(context.Context, string) (vuln.Finding, error) {
	return vuln.Finding{}, ErrUnavailable
}
func (unavailableVuln) SetFindingStatus(context.Context, authz.Subject, string, VulnStatusInput) (vuln.Finding, error) {
	return vuln.Finding{}, ErrUnavailable
}
func (unavailableVuln) Propose(context.Context, authz.Subject, string, vuln.ProposeOptions) (vuln.Proposal, error) {
	return vuln.Proposal{}, ErrUnavailable
}
func (unavailableVuln) Proposals(context.Context) ([]vuln.Proposal, error) {
	return nil, ErrUnavailable
}
func (unavailableVuln) Approve(context.Context, authz.Subject, string, VulnApproveInput) (vuln.Proposal, error) {
	return vuln.Proposal{}, ErrUnavailable
}
func (unavailableVuln) Decline(context.Context, authz.Subject, string, VulnDeclineInput) (vuln.Proposal, error) {
	return vuln.Proposal{}, ErrUnavailable
}
func (unavailableVuln) Poll(context.Context, authz.Subject) (vuln.PollReport, error) {
	return vuln.PollReport{}, ErrUnavailable
}
func (unavailableVuln) Feeds(context.Context) ([]vuln.FeedState, error) { return nil, ErrUnavailable }
func (unavailableVuln) Policy(context.Context) (VulnPolicyView, error) {
	return VulnPolicyView{}, ErrUnavailable
}
func (unavailableVuln) SetPolicy(context.Context, authz.Subject, VulnPolicyInput) (vuln.Policy, error) {
	return vuln.Policy{}, ErrUnavailable
}
func (unavailableVuln) Providers(context.Context) ([]vuln.ProviderView, error) {
	return nil, ErrUnavailable
}
func (unavailableVuln) AddProvider(context.Context, authz.Subject, VulnProviderInput) (vuln.ProviderView, error) {
	return vuln.ProviderView{}, ErrUnavailable
}
func (unavailableVuln) SetConsent(context.Context, authz.Subject, string, VulnConsentInput) (vuln.ProviderView, error) {
	return vuln.ProviderView{}, ErrUnavailable
}
func (unavailableVuln) Audit(context.Context) ([]vuln.AuditRecord, error) { return nil, ErrUnavailable }

var vulnResource = authz.Resource{Type: authz.ResourceVuln, Scope: authz.Global}

func (s *server) resolveVuln(*http.Request) (authz.Resource, any, error) {
	return vulnResource, nil, nil
}

func (s *server) vulnSvc() Vuln {
	if s.cfg.Vuln != nil {
		return s.cfg.Vuln
	}
	return unavailableVuln{}
}

// vulnEndpoints are appended to the endpoint table.
func (s *server) vulnEndpoints() []endpoint {
	ep := func(method, path string, a authz.Action, h func(http.ResponseWriter, *http.Request, *caller)) endpoint {
		return endpoint{method: method, path: path, access: accessPermission, action: a, resolve: s.resolveVuln, handle: h}
	}
	return []endpoint{
		ep("GET", "/vuln/findings", authz.ActionRead, s.handleVulnFindings),
		ep("GET", "/vuln/findings/{id}", authz.ActionRead, s.handleVulnFinding),
		ep("PUT", "/vuln/findings/{id}/status", authz.ActionUpdate, s.handleVulnFindingStatus),
		ep("POST", "/vuln/findings/{id}/proposals", authz.ActionOperate, s.handleVulnPropose),
		ep("GET", "/vuln/proposals", authz.ActionRead, s.handleVulnProposals),
		ep("POST", "/vuln/proposals/{id}/approve", authz.ActionUpdate, s.handleVulnApprove),
		ep("POST", "/vuln/proposals/{id}/decline", authz.ActionUpdate, s.handleVulnDecline),
		ep("GET", "/vuln/feeds", authz.ActionRead, s.handleVulnFeeds),
		ep("POST", "/vuln/poll", authz.ActionOperate, s.handleVulnPoll),
		ep("GET", "/vuln/policy", authz.ActionRead, s.handleVulnPolicy),
		ep("PUT", "/vuln/policy", authz.ActionUpdate, s.handleVulnPolicyUpdate),
		ep("GET", "/vuln/providers", authz.ActionRead, s.handleVulnProviders),
		ep("POST", "/vuln/providers", authz.ActionCreate, s.handleVulnProviderCreate),
		ep("PUT", "/vuln/providers/{id}/consent", authz.ActionUpdate, s.handleVulnConsent),
		ep("GET", "/vuln/audit", authz.ActionRead, s.handleVulnAudit),
	}
}

func writeList[T any](s *server, w http.ResponseWriter, r *http.Request, out []T, err error) {
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if out == nil {
		out = []T{}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeOne[T any](s *server, w http.ResponseWriter, r *http.Request, status int, out T, err error) {
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, status, out)
}

func (s *server) handleVulnFindings(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.vulnSvc().Findings(r.Context())
	writeList(s, w, r, out, err)
}

func (s *server) handleVulnFinding(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.vulnSvc().Finding(r.Context(), r.PathValue("id"))
	writeOne(s, w, r, http.StatusOK, out, err)
}

func (s *server) handleVulnFindingStatus(w http.ResponseWriter, r *http.Request, c *caller) {
	var in VulnStatusInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.vulnSvc().SetFindingStatus(r.Context(), c.subject, r.PathValue("id"), in)
	writeOne(s, w, r, http.StatusOK, out, err)
}

func (s *server) handleVulnPropose(w http.ResponseWriter, r *http.Request, c *caller) {
	var in vuln.ProposeOptions
	if !decodeJSON(w, r, &in, true) {
		return
	}
	out, err := s.vulnSvc().Propose(r.Context(), c.subject, r.PathValue("id"), in)
	if err == nil {
		s.log.Info("api: vulnerability proposal", "proposal", out.ID, "finding", out.FindingID, "by", c.subject.ID,
			"origin", out.Origin, "kind", out.Kind, "status", out.Status)
	}
	writeOne(s, w, r, http.StatusCreated, out, err)
}

func (s *server) handleVulnProposals(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.vulnSvc().Proposals(r.Context())
	writeList(s, w, r, out, err)
}

func (s *server) handleVulnApprove(w http.ResponseWriter, r *http.Request, c *caller) {
	var in VulnApproveInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.Digest == "" || len(in.Digest) > 128 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.vulnSvc().Approve(r.Context(), c.subject, r.PathValue("id"), in)
	if err == nil {
		s.log.Info("api: vulnerability proposal approved", "proposal", out.ID, "by", c.subject.ID, "status", out.Status)
	}
	writeOne(s, w, r, http.StatusOK, out, err)
}

func (s *server) handleVulnDecline(w http.ResponseWriter, r *http.Request, c *caller) {
	var in VulnDeclineInput
	if !decodeJSON(w, r, &in, true) {
		return
	}
	out, err := s.vulnSvc().Decline(r.Context(), c.subject, r.PathValue("id"), in)
	writeOne(s, w, r, http.StatusOK, out, err)
}

func (s *server) handleVulnFeeds(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.vulnSvc().Feeds(r.Context())
	writeList(s, w, r, out, err)
}

func (s *server) handleVulnPoll(w http.ResponseWriter, r *http.Request, c *caller) {
	out, err := s.vulnSvc().Poll(r.Context(), c.subject)
	writeOne(s, w, r, http.StatusOK, out, err)
}

func (s *server) handleVulnPolicy(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.vulnSvc().Policy(r.Context())
	writeOne(s, w, r, http.StatusOK, out, err)
}

func (s *server) handleVulnPolicyUpdate(w http.ResponseWriter, r *http.Request, c *caller) {
	var in VulnPolicyInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.vulnSvc().SetPolicy(r.Context(), c.subject, in)
	if err == nil {
		s.log.Info("api: vulnerability policy changed", "by", c.subject.ID, "mode", out.Mode, "killSwitch", out.KillSwitch)
	}
	writeOne(s, w, r, http.StatusOK, out, err)
}

func (s *server) handleVulnProviders(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.vulnSvc().Providers(r.Context())
	writeList(s, w, r, out, err)
}

func (s *server) handleVulnProviderCreate(w http.ResponseWriter, r *http.Request, c *caller) {
	var in VulnProviderInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if len(in.APIKey) > 4096 || len(in.Models) > 64 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.vulnSvc().AddProvider(r.Context(), c.subject, in)
	if err == nil {
		s.log.Info("api: AI provider registered", "provider", out.ID, "kind", out.Kind, "by", c.subject.ID)
	}
	writeOne(s, w, r, http.StatusCreated, out, err)
}

func (s *server) handleVulnConsent(w http.ResponseWriter, r *http.Request, c *caller) {
	var in VulnConsentInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	out, err := s.vulnSvc().SetConsent(r.Context(), c.subject, r.PathValue("id"), in)
	writeOne(s, w, r, http.StatusOK, out, err)
}

func (s *server) handleVulnAudit(w http.ResponseWriter, r *http.Request, _ *caller) {
	out, err := s.vulnSvc().Audit(r.Context())
	writeList(s, w, r, out, err)
}

// VulnBackend adapts *vuln.Service to the API and maps its errors to the
// API's sentinel errors. The manager mounts it when manager.vuln is set.
type VulnBackend struct{ Svc *vuln.Service }

var _ Vuln = (*VulnBackend)(nil)

func mapVulnError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vuln.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, vuln.ErrNotLeader):
		return ErrNotLeader
	case errors.Is(err, vuln.ErrApprovalMismatch):
		return ErrApprovalMismatch
	case errors.Is(err, vuln.ErrConflict), errors.Is(err, vuln.ErrClosed):
		return ErrConflict
	case errors.Is(err, vuln.ErrPolicyForbids):
		return ErrPolicyConflict
	case errors.Is(err, ai.ErrNoConsent):
		return ErrSettingsIncomplete
	case errors.Is(err, ai.ErrBudgetExceeded):
		return ErrBusy
	case errors.Is(err, vuln.ErrInvalidInput), errors.Is(err, vuln.ErrInvalidStatus), errors.Is(err, vuln.ErrInvalidPolicy),
		errors.Is(err, vuln.ErrInvalidInventory), errors.Is(err, ai.ErrInvalidConfig), errors.Is(err, ai.ErrUnknownModel):
		return ErrInvalidInput
	}
	// Provider and feed failures: 503 with a coarse code. The error text
	// is coarse by construction (no keys, no response bodies).
	return err
}

func actorID(a authz.Subject) string { return a.ID }

// Findings implements Vuln.
func (b *VulnBackend) Findings(context.Context) ([]vuln.Finding, error) {
	out, err := b.Svc.Findings()
	return out, mapVulnError(err)
}

// Finding implements Vuln.
func (b *VulnBackend) Finding(_ context.Context, id string) (vuln.Finding, error) {
	out, err := b.Svc.Finding(id)
	return out, mapVulnError(err)
}

// SetFindingStatus implements Vuln.
func (b *VulnBackend) SetFindingStatus(ctx context.Context, a authz.Subject, id string, in VulnStatusInput) (vuln.Finding, error) {
	out, err := b.Svc.SetFindingStatus(ctx, actorID(a), id, in.Status, in.Justification)
	return out, mapVulnError(err)
}

// Propose implements Vuln.
func (b *VulnBackend) Propose(ctx context.Context, a authz.Subject, id string, o vuln.ProposeOptions) (vuln.Proposal, error) {
	out, err := b.Svc.Propose(ctx, actorID(a), id, o)
	return out, mapVulnError(err)
}

// Proposals implements Vuln.
func (b *VulnBackend) Proposals(context.Context) ([]vuln.Proposal, error) {
	out, err := b.Svc.Proposals()
	return out, mapVulnError(err)
}

// Approve implements Vuln.
func (b *VulnBackend) Approve(ctx context.Context, a authz.Subject, id string, in VulnApproveInput) (vuln.Proposal, error) {
	out, err := b.Svc.Approve(ctx, actorID(a), id, in.Digest)
	return out, mapVulnError(err)
}

// Decline implements Vuln.
func (b *VulnBackend) Decline(ctx context.Context, a authz.Subject, id string, in VulnDeclineInput) (vuln.Proposal, error) {
	out, err := b.Svc.Decline(ctx, actorID(a), id, in.Reason)
	return out, mapVulnError(err)
}

// Poll implements Vuln.
func (b *VulnBackend) Poll(ctx context.Context, a authz.Subject) (vuln.PollReport, error) {
	out, err := b.Svc.Poll(ctx, actorID(a))
	return out, mapVulnError(err)
}

// Feeds implements Vuln.
func (b *VulnBackend) Feeds(context.Context) ([]vuln.FeedState, error) {
	out, err := b.Svc.FeedStates()
	return out, mapVulnError(err)
}

// Policy implements Vuln.
func (b *VulnBackend) Policy(context.Context) (VulnPolicyView, error) {
	p, v, err := b.Svc.Policy()
	return VulnPolicyView{Policy: p, Version: v}, mapVulnError(err)
}

// SetPolicy implements Vuln.
func (b *VulnBackend) SetPolicy(ctx context.Context, a authz.Subject, in VulnPolicyInput) (vuln.Policy, error) {
	out, err := b.Svc.SetPolicy(ctx, actorID(a), in.Policy, in.Version)
	return out, mapVulnError(err)
}

// Providers implements Vuln.
func (b *VulnBackend) Providers(context.Context) ([]vuln.ProviderView, error) {
	out, err := b.Svc.Providers()
	return out, mapVulnError(err)
}

// AddProvider implements Vuln.
func (b *VulnBackend) AddProvider(ctx context.Context, a authz.Subject, in VulnProviderInput) (vuln.ProviderView, error) {
	cfg := ai.ProviderConfig{ID: in.ID, Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL, Models: in.Models, DefaultModel: in.DefaultModel,
		Binary: in.Binary, Home: in.Home, Experimental: in.Experimental, Limits: in.Limits, MaxOutputTokens: in.MaxOutputTokens, TimeoutSeconds: in.TimeoutSeconds}
	out, err := b.Svc.AddProvider(ctx, actorID(a), cfg, ai.NewSecret(in.APIKey))
	return out, mapVulnError(err)
}

// SetConsent implements Vuln.
func (b *VulnBackend) SetConsent(ctx context.Context, a authz.Subject, id string, in VulnConsentInput) (vuln.ProviderView, error) {
	out, err := b.Svc.SetConsent(ctx, actorID(a), id, in.Grant, in.Statement)
	return out, mapVulnError(err)
}

// Audit implements Vuln.
func (b *VulnBackend) Audit(context.Context) ([]vuln.AuditRecord, error) {
	out, err := b.Svc.Audit()
	return out, mapVulnError(err)
}
