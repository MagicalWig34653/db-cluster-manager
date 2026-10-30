// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// Operation kinds of the guided Galera recovery (ADR 0028).
const (
	// OperationKindZeroUUIDPrepare stops every node of a cluster in a
	// zero-UUID incident and snapshots the data directories.
	OperationKindZeroUUIDPrepare = "galera/zero-uuid-prepare"
	// OperationKindZeroUUIDRecovery bootstraps the highest-seqno node with
	// a new state UUID and rebuilds the others by SST.
	OperationKindZeroUUIDRecovery = "galera/zero-uuid-recovery"
)

// ZeroUUIDRecoveryStarted is the first phase of a zero-UUID recovery,
// waiting for an approval of Approval.Digest (POST /operations/{id}/approve)
// unless the cluster's autoRecover policy started it.
type ZeroUUIDRecoveryStarted struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Snapshot string            `json:"snapshot"`
	Approval OperationApproval `json:"approval"`
}

// RecoveryPhase describes one operation of a recovery run.
type RecoveryPhase struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	// AwaitingApproval is true while the operation waits for an approval.
	AwaitingApproval bool              `json:"awaitingApproval"`
	Approval         OperationApproval `json:"approval"`
	// Auto is true when the autoRecover policy approved the operation.
	Auto      bool       `json:"auto,omitempty"`
	Candidate string     `json:"candidate,omitempty"`
	Diagnosis *Diagnosis `json:"diagnosis,omitempty"`
}

// ZeroUUIDRecoveryStatus is the newest recovery run of a cluster: the
// prepare phase and, once the positions are read, the recovery phase.
type ZeroUUIDRecoveryStatus struct {
	AutoRecover bool           `json:"autoRecover"`
	Snapshot    string         `json:"snapshot,omitempty"`
	Prepare     *RecoveryPhase `json:"prepare,omitempty"`
	Recovery    *RecoveryPhase `json:"recovery,omitempty"`
}

// AutoRecoverInput switches the per-cluster autoRecover policy.
type AutoRecoverInput struct {
	Enabled bool `json:"enabled"`
}

// AutoRecoverPolicy is the autoRecover policy of a cluster.
type AutoRecoverPolicy struct {
	Enabled bool `json:"enabled"`
}

// ZeroUUIDRecoveryControl extends Control without changing existing
// implementations (like ClusterOperations).
type ZeroUUIDRecoveryControl interface {
	StartZeroUUIDRecovery(ctx context.Context, actor authz.Subject, clusterID string) (ZeroUUIDRecoveryStarted, error)
	ZeroUUIDRecoveryStatus(ctx context.Context, actor authz.Subject, clusterID string) (ZeroUUIDRecoveryStatus, error)
	SetAutoRecover(ctx context.Context, actor authz.Subject, clusterID string, enabled bool) (AutoRecoverPolicy, error)
}

func (s *server) zeroUUIDControl() (ZeroUUIDRecoveryControl, bool) {
	c, ok := s.control().(ZeroUUIDRecoveryControl)
	return c, ok
}

func (s *server) handleZeroUUIDRecoveryStart(w http.ResponseWriter, r *http.Request, c *caller) {
	var empty struct{}
	if !decodeJSON(w, r, &empty, true) {
		return
	}
	svc, ok := s.zeroUUIDControl()
	if !ok {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	out, err := svc.StartZeroUUIDRecovery(r.Context(), c.subject, c.target.(Cluster).ID)
	if err != nil {
		s.writeOutageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (s *server) handleZeroUUIDRecoveryStatus(w http.ResponseWriter, r *http.Request, c *caller) {
	svc, ok := s.zeroUUIDControl()
	if !ok {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	out, err := svc.ZeroUUIDRecoveryStatus(r.Context(), c.subject, c.target.(Cluster).ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleAutoRecover(w http.ResponseWriter, r *http.Request, c *caller) {
	var in AutoRecoverInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	svc, ok := s.zeroUUIDControl()
	if !ok {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	out, err := svc.SetAutoRecover(r.Context(), c.subject, c.target.(Cluster).ID, in.Enabled)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
