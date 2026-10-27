// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

const (
	CollHostSources                  = "host-sources"
	CollHostSourceSecrets            = "host-source-secrets"
	KindHostBackupFailed             = "backup.host_failed"
	KindHostRestore                  = "restic/restore"
	KindResticSSHBackup   agent.Kind = "restic-ssh-backup"
	KindResticSSHForget   agent.Kind = "restic-ssh-forget"
	KindResticSSHCheck    agent.Kind = "restic-ssh-check"
	KindResticSSHRestore  agent.Kind = "restic-ssh-restore"
)

type HostSourceSecrets struct {
	Password      string            `json:"password"`
	Credentials   map[string]string `json:"credentials,omitempty"`
	SSHPrivateKey string            `json:"sshPrivateKey,omitempty"`
	SSHKnownHosts string            `json:"sshKnownHosts,omitempty"`
}

// HostSources owns standalone filesystem backup definitions. The source
// record is safe to return; credentials are encrypted in a separate document.
type HostSources struct {
	Ops    *Operations
	Keys   KeyProvider
	Notify func(context.Context, notify.Event) bool
}

var _ api.HostSources = (*HostSources)(nil)

func hostSourceRes(h api.HostSource) authz.Resource {
	scope := authz.Global
	if h.Swarm != "" {
		scope = authz.SwarmScope(h.Swarm)
	}
	return authz.Resource{Type: authz.ResourceBackup, Scope: scope, ID: h.ID}
}

func (s *HostSources) List(_ context.Context) ([]api.HostSource, error) {
	recs, err := List[api.HostSource](s.Ops.Svc.store, CollHostSources)
	if err != nil {
		return nil, MapCommandError(err)
	}
	out := make([]api.HostSource, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Value)
	}
	return out, nil
}

func (s *HostSources) Get(_ context.Context, id string) (api.HostSource, error) {
	r, ok, err := Get[api.HostSource](s.Ops.Svc.store, CollHostSources, id)
	if err != nil {
		return api.HostSource{}, MapCommandError(err)
	}
	if !ok {
		return api.HostSource{}, api.ErrNotFound
	}
	return r.Value, nil
}

var hostSourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var sshIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)

func validHostPath(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\r\n") {
		return false
	}
	for _, blocked := range []string{"/run/secrets", "/var/run/docker.sock", "/run/docker.sock"} {
		if p == blocked || strings.HasPrefix(p, blocked+"/") || strings.HasPrefix(blocked, p+"/") {
			return false
		}
	}
	return true
}

func (s *HostSources) validate(h api.HostSource) error {
	if !hostSourceID.MatchString(h.ID) || h.Name == "" || !hostSourceID.MatchString(h.HostID) || !hostSourceID.MatchString(h.PasswordSecret) ||
		(h.CredentialSecret != "" && !hostSourceID.MatchString(h.CredentialSecret)) || len(h.Paths) == 0 {
		return api.ErrInvalidInput
	}
	for _, p := range append(append([]string{}, h.Paths...), h.Excludes...) {
		if !validHostPath(p) {
			return api.ErrInvalidInput
		}
	}
	if _, err := restic.New(restic.Config{Backend: h.Backend, Repository: h.Repository, PasswordFile: "/run/secrets/placeholder"}, restic.ExecRunner{}); err != nil {
		return api.ErrInvalidInput
	}
	if h.Schedule != "" {
		if _, err := dbbackup.ParseSchedule(h.Schedule, nil); err != nil {
			return api.ErrInvalidInput
		}
	}
	if h.CheckSubset != "" && !regexp.MustCompile(`^(?:[1-9][0-9]?|100)%$`).MatchString(h.CheckSubset) {
		return api.ErrInvalidInput
	}
	keep := h.Retention.KeepLast + h.Retention.KeepDaily + h.Retention.KeepWeekly + h.Retention.KeepMonthly + h.Retention.KeepYearly
	if h.Retention.KeepLast < 0 || h.Retention.KeepDaily < 0 || h.Retention.KeepWeekly < 0 || h.Retention.KeepMonthly < 0 || h.Retention.KeepYearly < 0 ||
		(keep > 0 && !h.Retention.Prune) || (h.Retention.Prune && keep == 0) {
		return api.ErrInvalidInput
	}
	switch h.Kind {
	case "managed-host":
		if h.Transport != "agent" || h.Swarm == "" {
			return api.ErrInvalidInput
		}
		host, ok, err := Get[HostRecord](s.Ops.Svc.store, CollHosts, HostKey(h.Swarm, h.HostID), Consistent())
		if err != nil {
			return err
		}
		if !ok || host.Value.Enrollment != EnrollEnrolled {
			return api.ErrConflict
		}
	case "external-host":
		if h.Swarm != "" || (h.Transport != "ssh-plan" && h.Transport != "agent") {
			return api.ErrInvalidInput
		}
		if h.Transport == "ssh-plan" && (!sshIdentity.MatchString(h.SSHAddress) || !sshIdentity.MatchString(h.SSHUser) || h.SSHUser == "root") {
			return api.ErrInvalidInput
		}
		known, err := List[HostRecord](s.Ops.Svc.store, CollHosts, Consistent())
		if err != nil {
			return err
		}
		for _, managed := range known {
			if managed.Value.NodeID == h.HostID || (h.SSHAddress != "" && managed.Value.Addr == h.SSHAddress) {
				return api.ErrConflict
			}
		}
	default:
		return api.ErrInvalidInput
	}
	return nil
}

func (s *HostSources) Put(_ context.Context, actor authz.Subject, in api.HostSourceInput) (api.HostSource, error) {
	if _, err := s.Ops.leadership(); err != nil {
		return api.HostSource{}, MapCommandError(err)
	}
	h := in.HostSource
	old, exists, err := Get[api.HostSource](s.Ops.Svc.store, CollHostSources, h.ID, Consistent())
	if err != nil {
		return api.HostSource{}, MapCommandError(err)
	}
	act := authz.ActionCreate
	if exists {
		act = authz.ActionUpdate
		if old.Value.Kind != h.Kind || old.Value.Swarm != h.Swarm || old.Value.HostID != h.HostID ||
			old.Value.Transport != h.Transport || old.Value.Backend != h.Backend || old.Value.Repository != h.Repository ||
			old.Value.SSHAddress != h.SSHAddress || old.Value.SSHUser != h.SSHUser ||
			old.Value.PasswordSecret != h.PasswordSecret || old.Value.CredentialSecret != h.CredentialSecret {
			return api.HostSource{}, api.ErrInvalidInput
		}
		h.LastRunAt, h.LastRunStatus = old.Value.LastRunAt, old.Value.LastRunStatus
	}
	if err := s.Ops.authorize(actor, act, hostSourceRes(h)); err != nil {
		return api.HostSource{}, MapCommandError(err)
	}
	if err := s.validate(h); err != nil {
		return api.HostSource{}, MapCommandError(err)
	}
	secret := HostSourceSecrets{Password: in.Password, Credentials: in.Credentials, SSHPrivateKey: in.SSHPrivateKey, SSHKnownHosts: in.SSHKnownHosts}
	if exists && in.Password == "" && in.Credentials == nil && in.SSHPrivateKey == "" && in.SSHKnownHosts == "" {
		var ok bool
		secret, ok, err = GetSealed[HostSourceSecrets](s.Ops.Svc.store, s.Keys, CollHostSourceSecrets, h.ID, Consistent())
		if err != nil || !ok {
			return api.HostSource{}, api.ErrUnavailable
		}
	}
	if secret.Password == "" || len(secret.Password) > 4096 || s.Keys == nil {
		return api.HostSource{}, api.ErrInvalidInput
	}
	if strings.ContainsAny(secret.Password, "\r\n\x00") ||
		(h.Kind == "external-host" && h.Transport == "ssh-plan" && (secret.SSHPrivateKey == "" || secret.SSHKnownHosts == "")) {
		return api.HostSource{}, api.ErrInvalidInput
	}
	if _, err := restic.New(restic.Config{Backend: h.Backend, Repository: h.Repository, PasswordFile: "/run/secrets/placeholder", Credentials: secret.Credentials}, restic.ExecRunner{}); err != nil {
		return api.HostSource{}, api.ErrInvalidInput
	}
	sealed, err := seal(s.Keys, CollHostSourceSecrets, h.ID, secret)
	if err != nil {
		return api.HostSource{}, MapCommandError(err)
	}
	version := uint64(0)
	if exists {
		version = old.Version
	}
	metaOp, _ := PutOp(CollHostSources, h.ID, h, &version)
	secretOp, _ := PutOp(CollHostSourceSecrets, h.ID, sealed, nil)
	if _, err := s.Ops.Svc.store.Txn(metaOp, secretOp); err != nil {
		return api.HostSource{}, MapCommandError(err)
	}
	s.Ops.Svc.recordEvent(h.Swarm, "", api.SeverityInfo, fmt.Sprintf("%s configured host backup source %s", actor.ID, h.ID))
	return h, nil
}

func (s *HostSources) Delete(_ context.Context, actor authz.Subject, id string) error {
	if _, err := s.Ops.leadership(); err != nil {
		return MapCommandError(err)
	}
	h, err := s.Get(context.Background(), id)
	if err != nil {
		return err
	}
	if err := s.Ops.authorize(actor, authz.ActionDelete, hostSourceRes(h)); err != nil {
		return MapCommandError(err)
	}
	if _, err := s.Ops.Svc.store.Txn(
		metastore.DocOp{Collection: CollHostSources, Key: id, Delete: true},
		metastore.DocOp{Collection: CollHostSourceSecrets, Key: id, Delete: true},
	); err != nil {
		return MapCommandError(err)
	}
	s.Ops.Svc.recordEvent(h.Swarm, "", api.SeverityInfo, fmt.Sprintf("%s deleted host backup source %s", actor.ID, id))
	return nil
}

func (s *HostSources) Run(ctx context.Context, actor authz.Subject, id string) (api.OperationRef, error) {
	return s.start(ctx, actor, id, nil)
}

func (s *HostSources) Restore(ctx context.Context, actor authz.Subject, id string, req api.HostRestoreInput) (api.OperationRef, error) {
	return s.start(ctx, actor, id, &req)
}

func (s *HostSources) start(ctx context.Context, actor authz.Subject, id string, restore *api.HostRestoreInput) (api.OperationRef, error) {
	l, err := s.Ops.leadership()
	if err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	h, err := s.Get(ctx, id)
	if err != nil {
		return api.OperationRef{}, err
	}
	action := authz.ActionBackup
	if restore != nil {
		action = authz.ActionRestore
	}
	if err := s.Ops.authorize(actor, action, hostSourceRes(h)); err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	// An external agent needs an identity independent of a Swarm node. SSH
	// plans use their own pinned-host-key command adapter below.
	if h.Kind == "external-host" && h.Transport == "agent" {
		return api.OperationRef{}, api.ErrUnavailable
	}
	ssh := h.Kind == "external-host" && h.Transport == "ssh-plan"
	if !ssh && (h.Kind != "managed-host" || h.Transport != "agent") {
		return api.OperationRef{}, api.ErrUnavailable
	}
	a := agent.ResticArgs{Backend: h.Backend, Repository: h.Repository, PasswordSecret: h.PasswordSecret,
		CredentialSecret: h.CredentialSecret, Paths: h.Paths, Excludes: h.Excludes,
		Retention: h.Retention, ReadDataSubset: h.CheckSubset}
	op := runner.Operation{ID: newID("op"), Kind: "restic/backup", Locks: []string{"host-source/" + id}}
	backupArgs, _ := json.Marshal(a)
	target := h.HostID
	backupKind, forgetKind, checkKind, restoreKind := agent.KindResticBackup, agent.KindResticForget, agent.KindResticCheck, agent.KindResticRestore
	if ssh {
		target = "restic-source/" + id
		backupKind, forgetKind, checkKind, restoreKind = KindResticSSHBackup, KindResticSSHForget, KindResticSSHCheck, KindResticSSHRestore
	}
	op.Steps = []runner.Step{{ID: "backup", Target: target, Command: backupKind, Args: backupArgs, MaxRetries: 0}}
	approvalID := ""
	if restore != nil {
		if !validHostPath(restore.Target) || !regexp.MustCompile(`^[a-fA-F0-9]{6,64}$`).MatchString(restore.Snapshot) {
			return api.OperationRef{}, api.ErrInvalidInput
		}
		op.Kind = KindHostRestore
		approvalID = newID("apr")
		a.Paths, a.Excludes, a.Snapshot, a.Target, a.Approved = nil, nil, restore.Snapshot, restore.Target, true
		args, _ := json.Marshal(a)
		op.Steps = []runner.Step{{ID: "restore", Target: target, Command: restoreKind, Args: args,
			Preconditions: []runner.Condition{{Kind: runner.CondApproved, Target: approvalID}}, MaxRetries: 0}}
	} else {
		if h.Retention.Prune {
			args, _ := json.Marshal(a)
			op.Steps = append(op.Steps, runner.Step{ID: "retention", Target: target, Command: forgetKind, Args: args, MaxRetries: 0})
		}
		if h.CheckSubset != "" {
			args, _ := json.Marshal(a)
			op.Steps = append(op.Steps, runner.Step{ID: "check", Target: target, Command: checkKind, Args: args, MaxRetries: 0})
		}
	}
	if err := op.Validate(); err != nil {
		return api.OperationRef{}, api.ErrInvalidInput
	}
	digest, err := DefinitionDigest(op)
	if err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	rec := OperationRecord{ID: op.ID, Kind: op.Kind, Swarm: h.Swarm, RequestedBy: actor.ID, RequestedAt: s.Ops.Svc.cfg.Now(), Definition: op,
		ApprovalID: approvalID, ApprovalDigest: digest}
	if err := s.Ops.commit(rec, nil); err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	s.Ops.start(l, op, nil)
	if restore == nil {
		_, _, _ = Update(s.Ops.Svc.store, CollHostSources, id, func(cur api.HostSource, exists bool) (api.HostSource, error) {
			if !exists {
				return cur, ErrNotFound
			}
			cur.LastRunAt = s.Ops.Svc.cfg.Now().UTC().Format(time.RFC3339)
			cur.LastRunStatus = "started"
			return cur, nil
		})
	}
	return api.OperationRef{ID: op.ID, Kind: op.Kind}, nil
}

// RefreshOutcomes records completed backup outcomes and sends the existing
// notification dispatcher a failure event. It is safe to repeat after failover.
func (s *HostSources) RefreshOutcomes(ctx context.Context) {
	sources, err := List[api.HostSource](s.Ops.Svc.store, CollHostSources)
	if err != nil {
		return
	}
	operations, err := List[OperationRecord](s.Ops.Svc.store, CollOperations)
	if err != nil {
		return
	}
	for _, source := range sources {
		var latest *OperationRecord
		for i := range operations {
			r := &operations[i].Value
			if r.Kind != "restic/backup" || len(r.Definition.Locks) != 1 || r.Definition.Locks[0] != "host-source/"+source.Value.ID {
				continue
			}
			if latest == nil || r.RequestedAt.After(latest.RequestedAt) {
				latest = r
			}
		}
		if latest == nil || latest.View == nil || latest.View.Status != runner.StatusSucceeded && latest.View.Status != runner.StatusFailed ||
			source.Value.LastRunStatus == string(latest.View.Status) {
			continue
		}
		status := string(latest.View.Status)
		_, _, err := Update(s.Ops.Svc.store, CollHostSources, source.Key, func(cur api.HostSource, exists bool) (api.HostSource, error) {
			if !exists {
				return cur, ErrNotFound
			}
			cur.LastRunStatus = status
			return cur, nil
		})
		if err != nil || status != string(runner.StatusFailed) {
			continue
		}
		s.Ops.Svc.recordEvent(source.Value.Swarm, "", api.SeverityWarning, fmt.Sprintf("host backup source %s failed", source.Value.ID))
		if s.Notify != nil {
			s.Notify(ctx, notify.Event{Time: s.Ops.Svc.cfg.Now(), Severity: notify.SeverityWarning, Kind: KindHostBackupFailed,
				Resource: "host-source/" + source.Value.ID, Message: "Host backup failed", Details: map[string]string{"source": source.Value.ID, "operation": latest.ID}})
		}
	}
}

func (s *HostSources) RunSchedules(ctx context.Context, actor authz.Subject) {
	sources, err := List[api.HostSource](s.Ops.Svc.store, CollHostSources)
	if err != nil {
		return
	}
	for _, v := range sources {
		h := v.Value
		if h.Schedule == "" || (h.Kind != "managed-host" && h.Transport != "ssh-plan") {
			continue
		}
		spec, err := dbbackup.ParseSchedule(h.Schedule, nil)
		if err != nil {
			continue
		}
		last := time.Time{}
		if h.LastRunAt != "" {
			last, _ = time.Parse(time.RFC3339, h.LastRunAt)
		}
		if last.IsZero() {
			last = s.Ops.Svc.cfg.Now().Add(-time.Minute)
		}
		next, err := spec.Next(last)
		if err != nil || next.After(s.Ops.Svc.cfg.Now()) {
			continue
		}
		if _, err := s.Run(ctx, actor, h.ID); err != nil && !errors.Is(err, runner.ErrBusy) {
			s.Ops.Svc.recordEvent(h.Swarm, "", api.SeverityWarning, fmt.Sprintf("host backup source %s did not start", h.ID))
			if s.Notify != nil {
				s.Notify(ctx, notify.Event{Time: s.Ops.Svc.cfg.Now(), Severity: notify.SeverityWarning, Kind: KindHostBackupFailed,
					Resource: "host-source/" + h.ID, Message: "Scheduled host backup did not start", Details: map[string]string{"source": h.ID}})
			}
		}
	}
}

// HostSourceScheduler runs only while this replica leads. The operation
// journal and source timestamp prevent a failover from blindly repeating a
// completed backup step.
type HostSourceScheduler struct {
	Sources  func() *HostSources
	Interval time.Duration
}

func (s HostSourceScheduler) Name() string { return "host-source-scheduler" }

func (s HostSourceScheduler) Run(ctx context.Context, _ *Leadership) error {
	interval := s.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if s.Sources != nil {
				if sources := s.Sources(); sources != nil {
					sources.RefreshOutcomes(ctx)
					sources.RunSchedules(ctx, collect.SystemSubject)
				}
			}
		}
	}
}
