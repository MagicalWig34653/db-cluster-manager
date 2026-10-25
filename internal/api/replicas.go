// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// ReplicaKeyStatus is one manager replica (metastore voter) and its state
// in the data keyring. It carries only public key fingerprints.
type ReplicaKeyStatus struct {
	ID           string    `json:"id"`
	Address      string    `json:"address,omitempty"`
	Leader       bool      `json:"leader"`
	PublicKeyID  string    `json:"publicKeyId,omitempty"`
	RegisteredBy string    `json:"registeredBy,omitempty"`
	RegisteredAt time.Time `json:"registeredAt,omitzero"`
	// HasCurrentKey reports whether the current data key is wrapped for it.
	HasCurrentKey bool `json:"hasCurrentKey"`
}

// KeyringStatus is the non-secret view of the data keyring.
type KeyringStatus struct {
	CurrentKey string             `json:"currentKey"`
	Rotation   *KeyRotationStatus `json:"rotation,omitempty"`
	// Recoverable reports whether the current data key has an envelope
	// for the externally retained backup recovery key.
	Recoverable bool               `json:"recoverable"`
	Replicas    []ReplicaKeyStatus `json:"replicas"`
}

// ReplicaKeyInput registers a replica's public identity (printed by
// `manager -print-replica-key` on that replica).
type ReplicaKeyInput struct {
	PublicKey string `json:"publicKey"`
}

// ReplicaRevocation is the result of revoking a replica's manager
// certificates.
type ReplicaRevocation struct {
	Replica string `json:"replica"`
	Revoked int    `json:"revoked"`
}

// Replicas reports and registers manager replicas in the data keyring and
// revokes a replica's manager certificates.
type Replicas interface {
	Keyring(ctx context.Context, actor authz.Subject) (KeyringStatus, error)
	RegisterReplicaKey(ctx context.Context, actor authz.Subject, replicaID, publicKey string) (ReplicaKeyStatus, error)
	RevokeReplicaCertificates(ctx context.Context, actor authz.Subject, replicaID string) (ReplicaRevocation, error)
}

// RevokeReplicaCertificates implements Replicas.
func (Unavailable) RevokeReplicaCertificates(context.Context, authz.Subject, string) (ReplicaRevocation, error) {
	return ReplicaRevocation{}, ErrUnavailable
}

func (s *server) handleReplicaRevoke(w http.ResponseWriter, r *http.Request, c *caller) {
	id := r.PathValue("id")
	if !replicaIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	var empty struct{}
	if !decodeJSON(w, r, &empty, true) {
		return
	}
	out, err := s.replicas().RevokeReplicaCertificates(r.Context(), c.subject, id)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: replica manager certificates revoked", "replica", id, "by", c.subject.ID, "count", out.Revoked)
	writeJSON(w, http.StatusOK, out)
}

func (s *server) replicas() Replicas {
	if s.cfg.Replicas != nil {
		return s.cfg.Replicas
	}
	return Unavailable{}
}

// Keyring implements Replicas.
func (Unavailable) Keyring(context.Context, authz.Subject) (KeyringStatus, error) {
	return KeyringStatus{}, ErrUnavailable
}

// RegisterReplicaKey implements Replicas.
func (Unavailable) RegisterReplicaKey(context.Context, authz.Subject, string, string) (ReplicaKeyStatus, error) {
	return ReplicaKeyStatus{}, ErrUnavailable
}

var replicaIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func (s *server) handleReplicas(w http.ResponseWriter, r *http.Request, c *caller) {
	out, err := s.replicas().Keyring(r.Context(), c.subject)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if out.Replicas == nil {
		out.Replicas = []ReplicaKeyStatus{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleReplicaKey(w http.ResponseWriter, r *http.Request, c *caller) {
	id := r.PathValue("id")
	if !replicaIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	var in ReplicaKeyInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if in.PublicKey == "" || len(in.PublicKey) > 256 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.replicas().RegisterReplicaKey(r.Context(), c.subject, id, in.PublicKey)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	s.log.Info("api: replica data-key recipient registered", "replica", id, "by", c.subject.ID, "key", out.PublicKeyID)
	writeJSON(w, http.StatusOK, out)
}

// KeyRotationPlan is a non-secret, revision-bound approval preview. Submit its
// digest to the same route to start rotation; an empty digest only previews.
type KeyRotationPlan struct {
	Digest     string `json:"digest"`
	CurrentKey string `json:"currentKey"`
	Started    bool   `json:"started"`
}

// DataKeyRotator is optional when the replicated keyring is enabled.
type DataKeyRotator interface {
	RotateDataKey(context.Context, authz.Subject, string) (KeyRotationPlan, error)
}

func (s *server) handleKeyRotation(w http.ResponseWriter, r *http.Request, c *caller) {
	var in struct {
		Digest string `json:"digest"`
	}
	if !decodeJSON(w, r, &in, false) {
		return
	}
	svc, ok := s.cfg.Replicas.(DataKeyRotator)
	if !ok {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	out, err := svc.RotateDataKey(r.Context(), c.subject, in.Digest)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// KeyRotationStatus reports durable progress without exposing envelopes.
type KeyRotationStatus struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Processed uint64 `json:"processed"`
	Complete  bool   `json:"complete"`
}
