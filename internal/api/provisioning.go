// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

type Provisioning interface {
	Plan(context.Context, provision.Request, map[string]provision.Credential) (provision.Plan, error)
	Start(authz.Subject, string, string) (OperationRef, error)
}

type ProvisionPlanInput struct {
	ID          string                          `json:"id"`
	Platform    provision.Platform              `json:"platform"`
	Hosts       []provision.Host                `json:"hosts"`
	Credentials map[string]provision.Credential `json:"credentials"`
}

func (s *server) resolveProvisioning(*http.Request) (authz.Resource, any, error) {
	return authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}, nil, nil
}
func (s *server) provisioningEndpoints() []endpoint {
	return []endpoint{
		{method: "POST", path: "/provision/plans", access: accessPermission, action: authz.ActionOperate, resolve: s.resolveProvisioning, handle: s.handleProvisionPlan},
		{method: "POST", path: "/provision/plans/{id}/execute", access: accessPermission, action: authz.ActionOperate, resolve: s.resolveProvisioning, handle: s.handleProvisionExecute},
	}
}
func (s *server) handleProvisionPlan(w http.ResponseWriter, r *http.Request, _ *caller) {
	if s.cfg.Provisioning == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	var in ProvisionPlanInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	plan, err := s.cfg.Provisioning.Plan(r.Context(), provision.Request{ID: in.ID, Platform: in.Platform, Hosts: in.Hosts}, in.Credentials)
	if errors.Is(err, provision.ErrInvalid) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if errors.Is(err, provision.ErrExistingCluster) || errors.Is(err, provision.ErrPreflight) || errors.Is(err, provision.ErrConflict) {
		writeError(w, http.StatusConflict, errConflict)
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	writeJSON(w, http.StatusCreated, plan)
}
func (s *server) handleProvisionExecute(w http.ResponseWriter, r *http.Request, c *caller) {
	if s.cfg.Provisioning == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	var in struct {
		Digest string `json:"digest"`
	}
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if len(in.Digest) != 64 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	ref, err := s.cfg.Provisioning.Start(c.subject, r.PathValue("id"), in.Digest)
	if err != nil {
		var denied *authz.DeniedError
		switch {
		case errors.As(err, &denied):
			writeError(w, http.StatusForbidden, errForbidden)
		case errors.Is(err, ErrApprovalMismatch), errors.Is(err, provision.ErrExistingCluster):
			writeError(w, http.StatusConflict, errConflict)
		default:
			writeError(w, http.StatusServiceUnavailable, errUnavailable)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, ref)
}
