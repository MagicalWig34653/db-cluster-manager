// SPDX-License-Identifier: AGPL-3.0-only
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

type PeerCertificateInput struct {
	Address   string `json:"address"`
	CSR       string `json:"csr"`
	PublicKey string `json:"publicKey"`
}
type PeerCertificateResult struct {
	Certificate string    `json:"certificate"`
	Serial      string    `json:"serial"`
	NotAfter    time.Time `json:"notAfter"`
}
type MembershipInput struct {
	Remove bool   `json:"remove"`
	Digest string `json:"digest"`
}
type MembershipResult struct {
	Digest   string `json:"digest"`
	Complete bool   `json:"complete"`
}

// Membership is opt-in until a trusted peer readiness adapter is available.
type Membership interface {
	IssuePeerCertificate(context.Context, authz.Subject, string, PeerCertificateInput) (PeerCertificateResult, error)
	ChangeMembership(context.Context, authz.Subject, string, MembershipInput) (MembershipResult, error)
}

func (s *server) handlePeerCertificate(w http.ResponseWriter, r *http.Request, c *caller) {
	var in PeerCertificateInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if !replicaIDPattern.MatchString(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if s.cfg.Membership == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	out, err := s.cfg.Membership.IssuePeerCertificate(r.Context(), c.subject, r.PathValue("id"), in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *server) handleMembership(w http.ResponseWriter, r *http.Request, c *caller) {
	var in MembershipInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if !replicaIDPattern.MatchString(r.PathValue("id")) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if s.cfg.Membership == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	out, err := s.cfg.Membership.ChangeMembership(r.Context(), c.subject, r.PathValue("id"), in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
