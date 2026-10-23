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
	CurrentKey string `json:"currentKey"`
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

// Replicas reports and registers manager replicas in the data keyring.
type Replicas interface {
	Keyring(ctx context.Context, actor authz.Subject) (KeyringStatus, error)
	RegisterReplicaKey(ctx context.Context, actor authz.Subject, replicaID, publicKey string) (ReplicaKeyStatus, error)
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
