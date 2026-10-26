// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

// BackupCopy is one copy of a cluster's backup to a named backup target
// (another cluster's or Swarm's storage, B05). A copy is data only: it
// never promotes anything. Status is "running", "succeeded" or "failed".
type BackupCopy struct {
	ID         string    `json:"id"`
	Cluster    string    `json:"cluster"`
	Swarm      string    `json:"swarm"`
	Backup     string    `json:"backup"`
	Target     string    `json:"target"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	Objects    []string  `json:"objects"`
	By         string    `json:"by"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// BackupCopyInput starts a copy of one backup to a configured target.
type BackupCopyInput struct {
	Backup string `json:"backup"`
	Target string `json:"target"`
}

// RestoreProposalInput names where a copied backup would be restored.
type RestoreProposalInput struct {
	TargetSwarm   string `json:"targetSwarm"`
	TargetCluster string `json:"targetCluster"`
}

// RestoreProposal is the read-only, digest-bound proposal to restore a
// verified copy into a NEW cluster. Computing it changes nothing; there is
// no automatic promotion (approvalRequired is always true, autoPromotion
// always false).
type RestoreProposal struct {
	Copy             string    `json:"copy"`
	BackupID         string    `json:"backupId"`
	SourceCluster    string    `json:"sourceCluster"`
	SourceSwarm      string    `json:"sourceSwarm,omitempty"`
	ManifestSHA256   string    `json:"manifestSha256"`
	ObjectSHA256     string    `json:"objectSha256"`
	CopiedBy         string    `json:"copiedBy"`
	CopiedAt         time.Time `json:"copiedAt"`
	TargetSwarm      string    `json:"targetSwarm"`
	TargetCluster    string    `json:"targetCluster"`
	ApprovalRequired bool      `json:"approvalRequired"`
	AutoPromotion    bool      `json:"autoPromotion"`
	Steps            []string  `json:"steps"`
	Digest           string    `json:"digest"`
}

// BackupTransfers is implemented by a Backups service that copies backups
// between targets.
type BackupTransfers interface {
	BackupCopies(ctx context.Context) ([]BackupCopy, error)
	BackupCopy(ctx context.Context, id string) (BackupCopy, error)
	StartBackupCopy(ctx context.Context, actor authz.Subject, clusterID string, in BackupCopyInput) (BackupCopy, error)
	ProposeCopyRestore(ctx context.Context, actor authz.Subject, copyID string, in RestoreProposalInput) (RestoreProposal, error)
}

// MetastoreBackup is one backup of the manager's own metadata store. The
// object is sealed for the backup recovery key; Verified reports that the
// signed record and the stored object's size match.
type MetastoreBackup struct {
	Name        string    `json:"name"`
	Target      string    `json:"target,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	Size        int64     `json:"size"`
	Documents   int       `json:"documents"`
	Collections int       `json:"collections"`
	By          string    `json:"by,omitempty"`
	Verified    bool      `json:"verified"`
}

// MetastoreBackups is implemented by a Backups service that can back up
// the manager's own metastore (global backup permission on settings).
type MetastoreBackups interface {
	MetastoreBackups(ctx context.Context) ([]MetastoreBackup, error)
	CreateMetastoreBackup(ctx context.Context, actor authz.Subject) (MetastoreBackup, error)
}

var copyIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (s *server) transfers() BackupTransfers {
	t, _ := s.backups().(BackupTransfers)
	return t
}

func (s *server) metastoreBackups() MetastoreBackups {
	t, _ := s.backups().(MetastoreBackups)
	return t
}

func (s *server) handleBackupCopies(w http.ResponseWriter, r *http.Request, c *caller) {
	t := s.transfers()
	if t == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	items, err := t.BackupCopies(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	items = authz.Filter(s.cfg.Authorizer, c.subject, authz.ActionRead, items, func(b BackupCopy) authz.Resource {
		return backupResource(b.Swarm, b.Cluster)
	})
	if items == nil {
		items = []BackupCopy{}
	}
	writeJSON(w, http.StatusOK, List[BackupCopy]{Items: items})
}

func (s *server) handleBackupCopyStart(w http.ResponseWriter, r *http.Request, c *caller) {
	t := s.transfers()
	if t == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	var in BackupCopyInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if !copyIDRe.MatchString(in.Backup) || !copyIDRe.MatchString(in.Target) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	cl := c.target.(Cluster)
	out, err := t.StartBackupCopy(r.Context(), c.subject, cl.ID, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

// resolveBackupCopy maps a copy to the backups of its source cluster.
func (s *server) resolveBackupCopy(r *http.Request) (authz.Resource, any, error) {
	t := s.transfers()
	if t == nil {
		return authz.Resource{}, nil, ErrUnavailable
	}
	cp, err := t.BackupCopy(r.Context(), r.PathValue("id"))
	if err != nil {
		return authz.Resource{}, nil, err
	}
	return backupResource(cp.Swarm, cp.Cluster), cp, nil
}

func (s *server) handleRestoreProposal(w http.ResponseWriter, r *http.Request, c *caller) {
	var in RestoreProposalInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	if !copyIDRe.MatchString(in.TargetSwarm) || !copyIDRe.MatchString(in.TargetCluster) {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	cp := c.target.(BackupCopy)
	out, err := s.transfers().ProposeCopyRestore(r.Context(), c.subject, cp.ID, in)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleMetastoreBackups(w http.ResponseWriter, r *http.Request, _ *caller) {
	m := s.metastoreBackups()
	if m == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	items, err := m.MetastoreBackups(r.Context())
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if items == nil {
		items = []MetastoreBackup{}
	}
	writeJSON(w, http.StatusOK, List[MetastoreBackup]{Items: items})
}

func (s *server) handleMetastoreBackupCreate(w http.ResponseWriter, r *http.Request, c *caller) {
	m := s.metastoreBackups()
	if m == nil {
		s.writeServiceError(w, r, ErrUnavailable)
		return
	}
	out, err := m.CreateMetastoreBackup(r.Context(), c.subject)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}
