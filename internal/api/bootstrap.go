// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// Bootstrap backs the fresh-server bootstrap endpoints (X18, ADR 0027). All
// routes need the administrator. *provision.BootstrapService implements it.
type Bootstrap interface {
	Draft(context.Context, provision.DraftInput) (provision.BootstrapView, error)
	ConfirmFingerprint(ctx context.Context, id, fingerprint string) (provision.BootstrapView, error)
	Preview(id string) (provision.BootstrapPlan, error)
	Approve(id, digest string) (provision.BootstrapView, error)
	ApproveLockdown(id, digest string) (provision.BootstrapView, error)
	SupplyCredentials(id string, c provision.Credential) (provision.BootstrapView, error)
	Run(ctx context.Context, id string) (provision.BootstrapView, error)
	Status(id string) provision.BootstrapView
	List() []provision.BootstrapView
	Cancel(id string) (provision.BootstrapView, error)
}

// BootstrapCredential is the write-only login of a fresh server. It is never
// returned, logged or stored beyond the run.
type BootstrapCredential struct {
	PrivateKey string `json:"privateKey,omitempty"`
	Password   string `json:"password,omitempty"`
}

// BootstrapDraftInput creates a bootstrap draft.
type BootstrapDraftInput struct {
	ID         string                     `json:"id"`
	Host       provision.Host             `json:"host"`
	Profile    provision.BootstrapProfile `json:"profile"`
	Credential BootstrapCredential        `json:"credential"`
	// JoinToken is the Swarm join token for a join profile (write-only).
	JoinToken string `json:"joinToken,omitempty"`
}

var digestRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s *server) resolveBootstrap(*http.Request) (authz.Resource, any, error) {
	return authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}, nil, nil
}

func (s *server) bootstrapEndpoints() []endpoint {
	ep := func(method, path string, h func(http.ResponseWriter, *http.Request, *caller)) endpoint {
		return endpoint{method: method, path: path, access: accessPermission, adminOnly: true, action: authz.ActionUpdate, resolve: s.resolveBootstrap, handle: h}
	}
	const base = "/bootstrap/drafts"
	return []endpoint{
		ep("GET", "/bootstrap/support", s.handleBootstrapSupport),
		ep("GET", base, s.handleBootstrapList),
		ep("POST", base, s.handleBootstrapDraft),
		ep("GET", base+"/{id}", s.handleBootstrapGet),
		ep("GET", base+"/{id}/plan", s.handleBootstrapPlan),
		ep("POST", base+"/{id}/fingerprint", s.handleBootstrapFingerprint),
		ep("POST", base+"/{id}/approve", s.handleBootstrapApprove(false)),
		ep("POST", base+"/{id}/approve-lockdown", s.handleBootstrapApprove(true)),
		ep("POST", base+"/{id}/credentials", s.handleBootstrapCredentials),
		ep("POST", base+"/{id}/run", s.handleBootstrapRun),
		ep("POST", base+"/{id}/cancel", s.handleBootstrapCancel),
	}
}

func (s *server) bootstrapErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, provision.ErrInvalid), errors.Is(err, provision.ErrUnsupportedOS):
		writeError(w, http.StatusBadRequest, errBadRequest)
	case errors.Is(err, provision.ErrBootstrapNotFound):
		writeError(w, http.StatusNotFound, errNotFound)
	case errors.Is(err, provision.ErrDigest):
		writeError(w, http.StatusConflict, errApprovalMismatch)
	case errors.Is(err, provision.ErrHostKeyMismatch), errors.Is(err, provision.ErrBootstrapBlocked), errors.Is(err, provision.ErrNotApproved),
		errors.Is(err, provision.ErrBootstrapState), errors.Is(err, provision.ErrConflict), errors.Is(err, provision.ErrExistingCluster),
		errors.Is(err, provision.ErrPreflight), errors.Is(err, provision.ErrCredentialsExpired):
		writeError(w, http.StatusConflict, errConflict)
	default:
		s.log.Error("api: bootstrap failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
	}
}

// backend returns the configured backend or answers 503.
func (s *server) bootstrap(w http.ResponseWriter) Bootstrap {
	if s.cfg.Bootstrap == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return nil
	}
	return s.cfg.Bootstrap
}

func (s *server) handleBootstrapSupport(w http.ResponseWriter, _ *http.Request, _ *caller) {
	if s.bootstrap(w) == nil {
		return
	}
	writeJSON(w, http.StatusOK, provision.SupportMatrix())
}

func (s *server) handleBootstrapList(w http.ResponseWriter, _ *http.Request, _ *caller) {
	if b := s.bootstrap(w); b != nil {
		writeJSON(w, http.StatusOK, b.List())
	}
}

func (s *server) handleBootstrapDraft(w http.ResponseWriter, r *http.Request, c *caller) {
	b := s.bootstrap(w)
	if b == nil {
		return
	}
	var in BootstrapDraftInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.ID == "" || (in.Credential.PrivateKey == "" && in.Credential.Password == "") {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	v, err := b.Draft(r.Context(), provision.DraftInput{
		Request:    provision.BootstrapRequest{ID: in.ID, Host: in.Host, Profile: in.Profile},
		Credential: provision.Credential{PrivateKey: in.Credential.PrivateKey, Password: in.Credential.Password},
		JoinToken:  in.JoinToken,
	})
	if err != nil {
		s.bootstrapErr(w, r, err)
		return
	}
	s.log.Info("api: bootstrap drafted", "id", in.ID, "by", c.subject.ID)
	writeJSON(w, http.StatusCreated, v)
}

func (s *server) handleBootstrapGet(w http.ResponseWriter, r *http.Request, _ *caller) {
	b := s.bootstrap(w)
	if b == nil {
		return
	}
	v := b.Status(r.PathValue("id"))
	if v.Status == "" {
		writeError(w, http.StatusNotFound, errNotFound)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handleBootstrapPlan(w http.ResponseWriter, r *http.Request, _ *caller) {
	b := s.bootstrap(w)
	if b == nil {
		return
	}
	p, err := b.Preview(r.PathValue("id"))
	if err != nil {
		s.bootstrapErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) handleBootstrapFingerprint(w http.ResponseWriter, r *http.Request, c *caller) {
	b := s.bootstrap(w)
	if b == nil {
		return
	}
	var in struct {
		Fingerprint string `json:"fingerprint"`
	}
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.Fingerprint == "" {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	v, err := b.ConfirmFingerprint(r.Context(), r.PathValue("id"), in.Fingerprint)
	if err != nil {
		s.log.Warn("api: bootstrap fingerprint rejected", "id", r.PathValue("id"), "by", c.subject.ID)
		s.bootstrapErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handleBootstrapApprove(lockdown bool) func(http.ResponseWriter, *http.Request, *caller) {
	return func(w http.ResponseWriter, r *http.Request, c *caller) {
		b := s.bootstrap(w)
		if b == nil {
			return
		}
		var in struct {
			Digest string `json:"digest"`
		}
		if !decodeJSON(w, r, &in, false) {
			return
		}
		if !digestRe.MatchString(in.Digest) {
			writeError(w, http.StatusBadRequest, errBadRequest)
			return
		}
		var v provision.BootstrapView
		var err error
		if lockdown {
			v, err = b.ApproveLockdown(r.PathValue("id"), in.Digest)
		} else {
			v, err = b.Approve(r.PathValue("id"), in.Digest)
		}
		if err != nil {
			s.bootstrapErr(w, r, err)
			return
		}
		s.log.Info("api: bootstrap approved", "id", r.PathValue("id"), "lockdown", lockdown, "by", c.subject.ID)
		writeJSON(w, http.StatusOK, v)
	}
}

func (s *server) handleBootstrapCredentials(w http.ResponseWriter, r *http.Request, _ *caller) {
	b := s.bootstrap(w)
	if b == nil {
		return
	}
	var in BootstrapCredential
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.PrivateKey == "" && in.Password == "" {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	v, err := b.SupplyCredentials(r.PathValue("id"), provision.Credential{PrivateKey: in.PrivateKey, Password: in.Password})
	if err != nil {
		s.bootstrapErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handleBootstrapRun(w http.ResponseWriter, r *http.Request, c *caller) {
	b := s.bootstrap(w)
	if b == nil {
		return
	}
	v, err := b.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		s.bootstrapErr(w, r, err)
		return
	}
	s.log.Info("api: bootstrap run started", "id", r.PathValue("id"), "by", c.subject.ID)
	writeJSON(w, http.StatusAccepted, v)
}

func (s *server) handleBootstrapCancel(w http.ResponseWriter, r *http.Request, _ *caller) {
	b := s.bootstrap(w)
	if b == nil {
		return
	}
	v, err := b.Cancel(r.PathValue("id"))
	if err != nil {
		s.bootstrapErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
