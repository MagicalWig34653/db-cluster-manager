// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// PITRCoverage is what the binary log archive of one full backup covers.
type PITRCoverage struct {
	Segments int       `json:"segments"`
	From     time.Time `json:"from"`
	Until    time.Time `json:"until,omitzero"`
	Gap      string    `json:"gap,omitempty"`
}

// PointInTimeProposal is the read-only answer to "can this cluster be
// restored to that time". Computing it changes nothing. The restore itself
// is POST /restores/inspect with pointInTime: a NEW isolated instance after
// an approval, never in place (InPlace is always false, ApprovalRequired
// always true).
type PointInTimeProposal struct {
	Cluster          string        `json:"cluster"`
	At               time.Time     `json:"at"`
	Covered          bool          `json:"covered"`
	Reason           string        `json:"reason,omitempty"`
	Backup           string        `json:"backup,omitempty"`
	BackupFinishedAt time.Time     `json:"backupFinishedAt,omitzero"`
	Segments         int           `json:"segments,omitempty"`
	Coverage         *PITRCoverage `json:"coverage,omitempty"`
	ApprovalRequired bool          `json:"approvalRequired"`
	InPlace          bool          `json:"inPlace"`
}

// PointInTimeRestores is implemented by a Backups service that archives the
// binary log and proposes point-in-time restores.
type PointInTimeRestores interface {
	ProposePointInTime(ctx context.Context, actor authz.Subject, clusterID string, at time.Time) (PointInTimeProposal, error)
	StartBinlogArchive(ctx context.Context, actor authz.Subject, clusterID string) (OperationRef, error)
}

func (s *server) pitr() PointInTimeRestores {
	p, _ := s.backups().(PointInTimeRestores)
	return p
}

func (s *server) handlePITRProposal(w http.ResponseWriter, r *http.Request, c *caller) {
	p := s.pitr()
	if p == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	at, err := time.Parse(time.RFC3339, r.URL.Query().Get("at"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := p.ProposePointInTime(r.Context(), c.subject, c.target.(Cluster).ID, at)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleBinlogArchiveStart(w http.ResponseWriter, r *http.Request, c *caller) {
	p := s.pitr()
	if p == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	ref, err := p.StartBinlogArchive(r.Context(), c.subject, c.target.(Cluster).ID)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ref)
}
