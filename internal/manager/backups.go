// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Backups glue: backup policies, manual and scheduled backups, and
// isolated inspection restores on top of internal/dbbackup and the
// operation runner (ADR 0009).
//
// Backups and restores are runner operations like every other change, so
// they are journaled, fenced, locked per cluster and taken over by a new
// leader. Restores carry an explicit approval (approvals.go) on every
// destructive step. Nothing here runs a backup tool or reads a database:
// the agent launches the owned one-shot job and the job helper does the
// work (not verified against real tools).

// Collections of the backup subsystem.
const (
	CollBackupPolicies = "backup-policies" // key: cluster ID
	CollBackups        = "backups"         // key: backup ID
	CollInspections    = "inspections"     // key: inspection session ID
	// CollVerifications holds dbbackup.VerifyReport values; key: session
	// ID. The restore-verified gate of an inspection writes it (verify.go);
	// passed reports set "last verified restore".
	CollVerifications = "restore-verifications"
)

// BackupSettings configure backup jobs and inspection restores. Secrets
// are named, never stored: each field ending in Secret names an existing
// Swarm secret.
type BackupSettings struct {
	// JobImage contains the dbcm job helper and the backup tools.
	JobImage string `json:"jobImage,omitempty"`
	// BackendSecret holds the backend configuration for the jobs.
	BackendSecret string `json:"backendSecret,omitempty"`
	// DBCredentialSecret holds the backup account credential.
	DBCredentialSecret string `json:"dbCredentialSecret,omitempty"`
	// RecipientKey is the public half of the recovery key (streams are
	// encrypted for it).
	RecipientKey string `json:"recipientKey,omitempty"`
	// KeySecret is the default Swarm secret with the recovery key for
	// restores (a request may name another one).
	KeySecret string `json:"keySecret,omitempty"`
	// InspectionNetwork is a non-production network for inspection
	// instances.
	InspectionNetwork string `json:"inspectionNetwork,omitempty"`
	// JobNanoCPUs and JobMemoryBytes limit each job (defaults 1 CPU, 1 GiB).
	JobNanoCPUs    int64 `json:"jobNanoCpus,omitempty"`
	JobMemoryBytes int64 `json:"jobMemoryBytes,omitempty"`
}

func (b BackupSettings) job() agent.JobResources {
	j := agent.JobResources{NanoCPUs: b.JobNanoCPUs, MemoryBytes: b.JobMemoryBytes}
	if j.NanoCPUs <= 0 {
		j.NanoCPUs = 1e9
	}
	if j.MemoryBytes <= 0 {
		j.MemoryBytes = 1 << 30
	}
	return j
}

func (b BackupSettings) backupReady() error {
	if b.JobImage == "" || b.BackendSecret == "" || b.DBCredentialSecret == "" || b.RecipientKey == "" {
		return fmt.Errorf("%w: backup jobImage, backendSecret, dbCredentialSecret and recipientKey required", ErrSettingsIncomplete)
	}
	return nil
}

// BackupPolicyRecord is the stored policy of one cluster. Last is the
// scheduled time of the newest due run (run or skipped).
type BackupPolicyRecord struct {
	Cluster   string           `json:"cluster"`
	Policy    api.BackupPolicy `json:"policy"`
	Last      time.Time        `json:"last"`
	UpdatedBy string           `json:"updatedBy"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// BackupRecord is one started backup. Its outcome is the outcome of its
// operation: a backup is complete only when the operation succeeded, which
// includes the gate that checks the manifest.
type BackupRecord struct {
	ID        string          `json:"id"`
	Cluster   string          `json:"cluster"`
	Swarm     string          `json:"swarm"`
	OpID      string          `json:"opId"`
	Method    dbbackup.Method `json:"method"`
	Objects   []string        `json:"objects"`
	Source    string          `json:"source"`  // node slot
	Trigger   string          `json:"trigger"` // manual or schedule
	By        string          `json:"by"`
	StartedAt time.Time       `json:"startedAt"`
	// DeletedAt is set when retention deleted the backup's objects.
	DeletedAt time.Time `json:"deletedAt,omitzero"`
}

// InspectionRecord is one inspection session and its operations.
type InspectionRecord struct {
	Session    dbbackup.InspectionSession `json:"session"`
	Cluster    string                     `json:"cluster"`
	Swarm      string                     `json:"swarm"`
	OpID       string                     `json:"opId"`
	InitSecret string                     `json:"initSecret"`
	By         string                     `json:"by"`
	TeardownOp string                     `json:"teardownOp,omitempty"`
}

// Backups implements api.Backups.
type Backups struct {
	Ops *Operations
	// Swarms dials a Swarm to create the inspection init secret.
	Swarms *Swarms
	// Backend reads manifests for restores; nil refuses restores with
	// ErrSettingsIncomplete.
	Backend backup.Backend
	// StaleAfter bounds the age of health observations used to pick a
	// backup source (default 2 minutes).
	StaleAfter time.Duration
	// Targets are named backup targets for copies (transfers.go), typically
	// the storage of another cluster or Swarm.
	Targets map[string]backup.Backend
	// Keys signs and verifies transfer records and signs the records of
	// metastore backups; the manager keyring. Nil refuses copies.
	Keys backup.KeySource
	// MetaTarget names the target of metastore backups (metabackup.go);
	// empty uses Backend.
	MetaTarget string
	// Notify receives failure notifications (typically Monitoring.Notify).
	Notify func(ctx context.Context, e notify.Event) bool

	xfer transferState
}

func (b *Backups) store() *Store            { return b.Ops.Svc.store }
func (b *Backups) now() time.Time           { return b.Ops.Svc.cfg.Now() }
func (b *Backups) log(msg string, a ...any) { b.Ops.Svc.log.Info(msg, a...) }

var _ api.Backups = (*Backups)(nil)

func loadLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: timezone %q", dbbackup.ErrSchedule, tz)
	}
	return loc, nil
}

func schedulePolicy(p api.BackupPolicy) (dbbackup.SchedulePolicy, error) {
	loc, err := loadLocation(p.Timezone)
	if err != nil {
		return dbbackup.SchedulePolicy{}, err
	}
	sch, err := dbbackup.ParseSchedule(p.Schedule, loc)
	if err != nil {
		return dbbackup.SchedulePolicy{}, err
	}
	missed := dbbackup.MissedPolicy(p.Missed)
	if missed == "" {
		missed = dbbackup.MissedSkip
	}
	return dbbackup.SchedulePolicy{Schedule: sch, Missed: missed}, nil
}

// Backups implements api.Backups (local reads).
func (b *Backups) Backups(context.Context) ([]api.BackupSummary, error) {
	clusters, err := List[ClusterRecord](b.store(), CollClusters)
	if err != nil {
		return nil, err
	}
	idx, err := loadBackupIndex(b.store())
	if err != nil {
		return nil, err
	}
	out := make([]api.BackupSummary, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, idx.summary(c.Value, b.now()))
	}
	return out, nil
}

// backupIndex is a snapshot of the backup state of every cluster.
type backupIndex struct {
	policies map[string]BackupPolicyRecord
	last     map[string]BackupRecord // newest completed per cluster
	lastAt   map[string]time.Time
	verified map[string]time.Time
}

func loadBackupIndex(s *Store) (backupIndex, error) {
	idx := backupIndex{policies: map[string]BackupPolicyRecord{}, last: map[string]BackupRecord{},
		lastAt: map[string]time.Time{}, verified: map[string]time.Time{}}
	pols, err := List[BackupPolicyRecord](s, CollBackupPolicies)
	if err != nil {
		return idx, err
	}
	for _, p := range pols {
		idx.policies[p.Key] = p.Value
	}
	recs, err := List[BackupRecord](s, CollBackups)
	if err != nil {
		return idx, err
	}
	if len(recs) > 0 {
		ops, err := List[OperationRecord](s, CollOperations)
		if err != nil {
			return idx, err
		}
		byID := map[string]OperationRecord{}
		for _, o := range ops {
			byID[o.Key] = o.Value
		}
		for _, r := range recs {
			op, ok := byID[r.Value.OpID]
			if !ok || !r.Value.DeletedAt.IsZero() || op.View == nil || op.View.Status != runner.StatusSucceeded || op.EndedAt.IsZero() {
				continue
			}
			if op.EndedAt.After(idx.lastAt[r.Value.Cluster]) {
				idx.lastAt[r.Value.Cluster], idx.last[r.Value.Cluster] = op.EndedAt, r.Value
			}
		}
	}
	reports, err := List[dbbackup.VerifyReport](s, CollVerifications)
	if err != nil {
		return idx, err
	}
	for _, r := range reports {
		if r.Value.Passed && r.Value.At.After(idx.verified[r.Value.ClusterID]) {
			idx.verified[r.Value.ClusterID] = r.Value.At
		}
	}
	return idx, nil
}

func (idx backupIndex) summary(c ClusterRecord, now time.Time) api.BackupSummary {
	s := api.BackupSummary{Cluster: c.ID, Swarm: c.Swarm, LastBackupAt: idx.lastAt[c.ID], LastBackupID: idx.last[c.ID].ID,
		LastVerifiedRestoreAt: idx.verified[c.ID]}
	if p, ok := idx.policies[c.ID]; ok {
		pol := p.Policy
		s.BackupPolicy = &pol
		if pol.Enabled {
			s.Policy = pol.Schedule
			if sp, err := schedulePolicy(pol); err == nil {
				if next, err := sp.Schedule.Next(now); err == nil {
					s.NextRunAt = next
				}
			}
		}
	}
	return s
}

func (b *Backups) clusterRecord(id string) (ClusterRecord, error) {
	cur, ok, err := Get[ClusterRecord](b.store(), CollClusters, id, Consistent())
	if err != nil {
		return ClusterRecord{}, err
	}
	if !ok {
		return ClusterRecord{}, fmt.Errorf("%w: cluster %q", api.ErrNotFound, id)
	}
	return cur.Value, nil
}

func (b *Backups) setBackupPolicy(_ context.Context, actor authz.Subject, clusterID string, p api.BackupPolicy) (api.BackupSummary, error) {
	if _, err := b.Ops.leadership(); err != nil {
		return api.BackupSummary{}, err
	}
	c, err := b.clusterRecord(clusterID)
	if err != nil {
		return api.BackupSummary{}, err
	}
	if err := b.Ops.authorize(actor, authz.ActionUpdate, backupRes(c.Swarm, c.ID)); err != nil {
		return api.BackupSummary{}, err
	}
	if p.Missed == "" {
		p.Missed = string(dbbackup.MissedSkip)
	}
	if p.Method == "" {
		p.Method = string(dbbackup.MethodPhysical)
	}
	switch {
	case p.Method == string(dbbackup.MethodPhysical) && len(p.Databases) > 0:
		return api.BackupSummary{}, fmt.Errorf("%w: a physical backup covers every database", dbbackup.ErrInvalidRequest)
	case p.Method == string(dbbackup.MethodLogical) && (len(p.Databases) == 0 || len(p.Databases) > 64):
		return api.BackupSummary{}, fmt.Errorf("%w: a logical backup needs 1..64 databases", dbbackup.ErrInvalidRequest)
	case p.Method != string(dbbackup.MethodPhysical) && p.Method != string(dbbackup.MethodLogical):
		return api.BackupSummary{}, fmt.Errorf("%w: method %q", dbbackup.ErrInvalidRequest, p.Method)
	}
	if r := p.Retention; r != nil && (r.KeepLast < 1 || r.KeepDaily < 0 || r.KeepWeekly < 0 || r.KeepLast > 1000 || r.KeepDaily > 1000 || r.KeepWeekly > 1000) {
		return api.BackupSummary{}, fmt.Errorf("%w: retention keepLast must be 1..1000, keepDaily and keepWeekly 0..1000", dbbackup.ErrInvalidRequest)
	}
	if p.Missed != string(dbbackup.MissedSkip) && p.Missed != string(dbbackup.MissedRunOnce) {
		return api.BackupSummary{}, fmt.Errorf("%w: missed-run policy %q", dbbackup.ErrSchedule, p.Missed)
	}
	if p.Enabled || p.Schedule != "" {
		if _, err := schedulePolicy(p); err != nil {
			return api.BackupSummary{}, err
		}
	}
	now := b.now()
	_, _, err = Update(b.store(), CollBackupPolicies, c.ID, func(cur BackupPolicyRecord, exists bool) (BackupPolicyRecord, error) {
		// A new or changed schedule starts counting now: no catch-up runs
		// for times before the change.
		if !exists || cur.Policy.Schedule != p.Schedule || cur.Policy.Timezone != p.Timezone || (!cur.Policy.Enabled && p.Enabled) {
			cur.Last = now
		}
		cur.Cluster, cur.Policy, cur.UpdatedBy, cur.UpdatedAt = c.ID, p, actor.ID, now
		return cur, nil
	})
	if err != nil {
		return api.BackupSummary{}, err
	}
	b.Ops.Svc.recordEvent(c.Swarm, c.ID, api.SeverityInfo, fmt.Sprintf("%s set the backup policy of %s (enabled=%v, schedule %q)", actor.ID, c.Name, p.Enabled, p.Schedule))
	idx, err := loadBackupIndex(b.store())
	if err != nil {
		return api.BackupSummary{}, err
	}
	return idx.summary(c, now), nil
}

// SetBackupPolicy implements api.Backups. It needs the update permission on
// the cluster's backups (audited).
func (b *Backups) SetBackupPolicy(ctx context.Context, actor authz.Subject, clusterID string, p api.BackupPolicy) (api.BackupSummary, error) {
	s, err := b.setBackupPolicy(ctx, actor, clusterID, p)
	return s, MapCommandError(err)
}

// StartInspection implements api.Backups (see startInspection).
func (b *Backups) StartInspection(ctx context.Context, actor authz.Subject, in api.InspectionInput) (api.InspectionStarted, error) {
	s, err := b.startInspection(ctx, actor, in)
	return s, MapCommandError(err)
}

// StartBackup implements api.Backups: a physical backup now. It needs the
// backup permission on the cluster's backups (audited).
func (b *Backups) StartBackup(_ context.Context, actor authz.Subject, clusterID string) (api.OperationRef, error) {
	rec, err := b.start(actor, clusterID, "manual", dbbackup.MethodPhysical, nil)
	if err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	return api.OperationRef{ID: rec.ID, Kind: rec.Kind}, nil
}

var _ api.LogicalBackups = (*Backups)(nil)

// StartLogicalBackup implements api.LogicalBackups: one mariadb-dump
// export per database now (backup permission, audited). The agent
// validates the database names again.
func (b *Backups) StartLogicalBackup(_ context.Context, actor authz.Subject, clusterID string, databases []string) (api.OperationRef, error) {
	rec, err := b.start(actor, clusterID, "manual", dbbackup.MethodLogical, databases)
	if err != nil {
		return api.OperationRef{}, MapCommandError(err)
	}
	return api.OperationRef{ID: rec.ID, Kind: rec.Kind}, nil
}

func (b *Backups) fresh(at time.Time) bool {
	limit := b.StaleAfter
	if limit <= 0 {
		limit = 2 * time.Minute
	}
	d := b.now().Sub(at)
	return !at.IsZero() && d >= 0 && d <= limit
}

// nodeStates is the source-selection view of a cluster: fresh health
// observations only (stale ones become unknown and are rejected), and the
// free capacity of each node's host. The proxy's writer is not known to
// the manager yet, so no node is marked as writer.
func (b *Backups) nodeStates(c ClusterRecord) ([]dbbackup.NodeState, error) {
	hosts, err := b.Ops.enrolledHosts(c.Swarm)
	if err != nil {
		return nil, err
	}
	free := map[string]galera.ResourceAmount{}
	for _, h := range hosts {
		free[h.ID] = galera.ResourceAmount{MilliCPU: h.Capacity.MilliCPU - h.Allocated.MilliCPU,
			MemoryBytes: h.Capacity.MemoryBytes - h.Allocated.MemoryBytes}
	}
	out := make([]dbbackup.NodeState, 0, len(c.Nodes))
	for _, n := range c.Nodes {
		ns := dbbackup.NodeState{Slot: n.Slot, NodeUUID: n.NodeUUID, HostID: n.HostID, Volume: n.Volume,
			Health: galera.NodeHealth{State: galera.HealthUnknown}, HostFree: free[n.HostID]}
		if h, ok := c.Health[n.Slot]; ok && b.fresh(h.ObservedAt) {
			ns.Health = h.Health
		}
		out = append(out, ns)
	}
	return out, nil
}

func (b *Backups) start(actor authz.Subject, clusterID, trigger string, method dbbackup.Method, databases []string) (OperationRecord, error) {
	l, err := b.Ops.leadership()
	if err != nil {
		return OperationRecord{}, err
	}
	c, err := b.clusterRecord(clusterID)
	if err != nil {
		return OperationRecord{}, err
	}
	if err := b.Ops.authorize(actor, authz.ActionBackup, backupRes(c.Swarm, c.ID)); err != nil {
		return OperationRecord{}, err
	}
	st, err := b.Ops.settings()
	if err != nil {
		return OperationRecord{}, err
	}
	if err := st.Backup.backupReady(); err != nil {
		return OperationRecord{}, err
	}
	if busy, err := b.Ops.unfinishedFor(c.ID); err != nil {
		return OperationRecord{}, err
	} else if busy != "" {
		return OperationRecord{}, fmt.Errorf("%w: operation %s is not finished", runner.ErrBusy, busy)
	}
	nodes, err := b.nodeStates(c)
	if err != nil {
		return OperationRecord{}, err
	}
	now := b.now()
	backupID, err := dbbackup.NewBackupID(now)
	if err != nil {
		return OperationRecord{}, err
	}
	svcHost, err := b.Ops.serviceHostFor(c.Swarm)
	if err != nil {
		return OperationRecord{}, err
	}
	opID := newID("op")
	plan, err := dbbackup.BackupPlan(dbbackup.BackupRequest{
		OpID: opID, BackupID: backupID, InstallationID: st.InstallationID, ClusterID: c.ID, ClusterName: c.Name,
		Method: method, Databases: databases, JobImage: st.Backup.JobImage, Network: c.Spec.Network,
		DBCredentialSecret: st.Backup.DBCredentialSecret, BackendSecret: st.Backup.BackendSecret,
		RecipientKey: st.Backup.RecipientKey, VolumeBase: st.VolumeBase, Job: st.Backup.job(),
		StepTimeout: b.Ops.StepTimeout, MaxRetries: b.Ops.MaxRetries, ServiceHost: svcHost,
	}, nodes, dbbackup.SourcePolicy{ClusterSize: c.Size})
	if err != nil {
		return OperationRecord{}, err
	}
	if err := b.Ops.allowReferenced(&plan.Operation, c.Swarm, st.InstallationID, []string{plan.Source.HostID, svcHost},
		objectRefs{Networks: []string{c.Spec.Network}, Secrets: []string{st.Backup.DBCredentialSecret, st.Backup.BackendSecret}}); err != nil {
		return OperationRecord{}, err
	}
	rec := OperationRecord{ID: opID, Kind: plan.Operation.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: actor.ID,
		RequestedAt: now, Definition: plan.Operation, Nodes: c.Nodes}
	br := BackupRecord{ID: backupID, Cluster: c.ID, Swarm: c.Swarm, OpID: opID, Method: method,
		Objects: plan.Objects, Source: plan.Source.Slot, Trigger: trigger, By: actor.ID, StartedAt: now}
	if err := b.commit(rec, PutOpOf(CollBackups, backupID, br)); err != nil {
		return OperationRecord{}, err
	}
	b.Ops.Svc.recordEvent(c.Swarm, c.ID, api.SeverityInfo, fmt.Sprintf("%s started %s %s backup %s of %s from %s, operation %s",
		actor.ID, trigger, method, backupID, c.Name, plan.Source.Slot, opID))
	b.Ops.start(l, plan.Operation, nil)
	return rec, nil
}

// docOp is a deferred PutOp (create-only).
type docOp struct {
	coll, key string
	v         any
}

// PutOpOf names a create-only document for commit.
func PutOpOf(coll, key string, v any) docOp { return docOp{coll, key, v} }

// commit stores the operation record and the extra documents in one
// transaction; none of them may exist.
func (b *Backups) commit(rec OperationRecord, extra ...docOp) error {
	op, err := PutOp(CollOperations, rec.ID, rec, IfVersion(0))
	if err != nil {
		return err
	}
	ops := []metastore.DocOp{op}
	for _, e := range extra {
		d, err := PutOp(e.coll, e.key, e.v, IfVersion(0))
		if err != nil {
			return err
		}
		ops = append(ops, d)
	}
	_, err = b.store().Txn(ops...)
	return err
}

// productionInventory lists every managed cluster's identity, so a restore
// cannot overlap it.
func productionInventory(s *Store) (dbbackup.Inventory, error) {
	clusters, err := List[ClusterRecord](s, CollClusters)
	if err != nil {
		return dbbackup.Inventory{}, err
	}
	var inv dbbackup.Inventory
	for _, c := range clusters {
		v := c.Value
		inv.ClusterIDs = append(inv.ClusterIDs, v.ID)
		inv.ClusterNames = append(inv.ClusterNames, v.Name)
		if v.Spec.Network != "" {
			inv.Networks = append(inv.Networks, v.Spec.Network)
		}
		for _, n := range v.Nodes {
			inv.Volumes = append(inv.Volumes, n.Volume)
			inv.NodeUUIDs = append(inv.NodeUUIDs, n.NodeUUID)
			inv.ServiceNames = append(inv.ServiceNames, galera.ServiceName(v.Name, n.Slot))
		}
	}
	return inv, nil
}

// DefaultInspectionTTL is the lifetime of an inspection instance when the
// request does not set one.
const DefaultInspectionTTL = 4 * time.Hour

// chooseBackup returns the requested or the newest completed physical
// backup of the cluster.
func (b *Backups) chooseBackup(clusterID, want string) (BackupRecord, error) {
	recs, err := List[BackupRecord](b.store(), CollBackups, Consistent())
	if err != nil {
		return BackupRecord{}, err
	}
	var best BackupRecord
	var bestAt time.Time
	for _, r := range recs {
		v := r.Value
		if v.Cluster != clusterID || v.Method != dbbackup.MethodPhysical || len(v.Objects) == 0 || (want != "" && v.ID != want) {
			continue
		}
		op, ok, err := Get[OperationRecord](b.store(), CollOperations, v.OpID)
		if err != nil {
			return BackupRecord{}, err
		}
		if !ok || op.Value.View == nil || op.Value.View.Status != runner.StatusSucceeded {
			continue
		}
		if op.Value.EndedAt.After(bestAt) || best.ID == "" {
			best, bestAt = v, op.Value.EndedAt
		}
	}
	if best.ID == "" {
		return BackupRecord{}, fmt.Errorf("%w: no completed physical backup", api.ErrNotFound)
	}
	return best, nil
}

func (b *Backups) manifest(ctx context.Context, object string) (dbbackup.Manifest, error) {
	rc, err := b.Backend.Get(ctx, dbbackup.ManifestName(object))
	if errors.Is(err, backup.ErrNotFound) {
		return dbbackup.Manifest{}, fmt.Errorf("%w: manifest of %s", api.ErrNotFound, object)
	}
	if err != nil {
		return dbbackup.Manifest{}, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	if err != nil {
		return dbbackup.Manifest{}, err
	}
	return dbbackup.DecodeManifest(data)
}

func (b *Backups) inspectionHost(c ClusterRecord, want string, res galera.Resources, dataBytes int64) (galera.Host, error) {
	hosts, err := b.Ops.enrolledHosts(c.Swarm)
	if err != nil {
		return galera.Host{}, err
	}
	if want != "" {
		for _, h := range hosts {
			if h.ID == want {
				if !h.Ready || !h.Active || h.Spare {
					return galera.Host{}, fmt.Errorf("%w: host %s is not ready, active and non-spare", dbbackup.ErrInvalidRequest, want)
				}
				return h, nil
			}
		}
		return galera.Host{}, fmt.Errorf("%w: host %s is not enrolled in the cluster's Swarm", dbbackup.ErrInvalidRequest, want)
	}
	// Free disk space is not observed yet (FreeDiskBytes stays 0), so the
	// automatic selection only succeeds for empty backups; an explicit host
	// is required otherwise.
	prod := map[string]bool{}
	for _, n := range c.Nodes {
		prod[n.HostID] = true
	}
	var cands []dbbackup.RestoreHost
	for _, h := range hosts {
		cands = append(cands, dbbackup.RestoreHost{Host: h, Production: prod[h.ID]})
	}
	h, err := dbbackup.SelectInspectionHost(cands, false, res.Limits, dataBytes)
	if err != nil {
		return galera.Host{}, fmt.Errorf("%w: %v", galera.ErrNoCapacity, err)
	}
	return h, nil
}

// startInspection plans the isolated
// inspection restore, creates the instance's init secret, stores the
// operation (with its approval digest) and the session, and starts the
// operation, which pauses before its first destructive step until the
// approval is recorded. It needs the restore permission on the cluster's
// backups (audited).
func (b *Backups) startInspection(ctx context.Context, actor authz.Subject, in api.InspectionInput) (api.InspectionStarted, error) {
	l, err := b.Ops.leadership()
	if err != nil {
		return api.InspectionStarted{}, err
	}
	c, err := b.clusterRecord(in.Cluster)
	if err != nil {
		return api.InspectionStarted{}, err
	}
	if err := b.Ops.authorize(actor, authz.ActionRestore, backupRes(c.Swarm, c.ID)); err != nil {
		return api.InspectionStarted{}, err
	}
	st, err := b.Ops.settings()
	if err != nil {
		return api.InspectionStarted{}, err
	}
	bs := st.Backup
	keySecret := in.KeySecret
	if keySecret == "" {
		keySecret = bs.KeySecret
	}
	if b.Backend == nil || bs.JobImage == "" || bs.BackendSecret == "" || bs.InspectionNetwork == "" || keySecret == "" {
		return api.InspectionStarted{}, fmt.Errorf("%w: backup backend, jobImage, backendSecret, inspectionNetwork and a key secret required", ErrSettingsIncomplete)
	}
	res := c.Resources
	if res.Limits.MilliCPU <= 0 || res.Limits.MemoryBytes <= 0 {
		return api.InspectionStarted{}, fmt.Errorf("%w: the cluster has no resource limits to size the instance", ErrSettingsIncomplete)
	}
	br, err := b.chooseBackup(c.ID, in.Backup)
	if err != nil {
		return api.InspectionStarted{}, err
	}
	m, err := b.manifest(ctx, br.Objects[0])
	if err != nil {
		return api.InspectionStarted{}, err
	}
	host, err := b.inspectionHost(c, in.Host, res, m.Stream.PlainSize)
	if err != nil {
		return api.InspectionStarted{}, err
	}
	ttl := DefaultInspectionTTL
	if in.TTLMinutes > 0 {
		ttl = min(time.Duration(in.TTLMinutes)*time.Minute, dbbackup.MaxInspectionTTL)
	}
	session := strings.TrimPrefix(newID("i"), "i-")[:12]
	session = "i" + session
	initSecret := dbbackup.InitSecretName(session)
	svcHost, err := b.Ops.serviceHostFor(c.Swarm)
	if err != nil {
		return api.InspectionStarted{}, err
	}
	opID, approvalID := newID("op"), newID("apr")
	now := b.now()
	plan, err := dbbackup.InspectionRestorePlan(dbbackup.InspectionRequest{
		OpID: opID, SessionID: session, ApprovalID: approvalID,
		Source: dbbackup.RestoreSource{Manifest: m, ClusterID: c.ID, BackendSecret: bs.BackendSecret, KeySecret: keySecret},
		Host:   host, InstallationID: st.InstallationID, VolumeBase: st.VolumeBase, Image: c.Spec.Image, JobImage: bs.JobImage,
		Network: bs.InspectionNetwork, InitSecret: initSecret, Resources: res, Job: bs.job(), TTL: ttl, Now: now,
		TokenID: newID("tok"), StepTimeout: b.Ops.StepTimeout, MaxRetries: b.Ops.MaxRetries, ServiceHost: svcHost,
	}, mustInventory(b.store()))
	if err != nil {
		return api.InspectionStarted{}, err
	}
	password, err := dbbackup.NewInspectionPassword()
	if err != nil {
		return api.InspectionStarted{}, err
	}
	// The monitoring account stays usable in the instance, so the agent can
	// verify that it answers (restore verification).
	sql, err := dbbackup.RenderInspectionInitKeeping(password, monitorAccount(st.Accounts))
	if err != nil {
		b.Ops.Svc.log.Warn("manager: monitoring account cannot stay unlocked in the inspection instance; verification will not pass", "err", err)
		if sql, err = dbbackup.RenderInspectionInit(password); err != nil {
			return api.InspectionStarted{}, err
		}
	}
	if err := b.createSecret(ctx, c.Swarm, initSecret, st.InstallationID, session, []byte(sql)); err != nil {
		return api.InspectionStarted{}, err
	}
	// The allow-objects step is not destructive (no approval) and is part
	// of the approved definition, so it is added before the digest.
	if err := b.Ops.allowReferenced(&plan.Operation, c.Swarm, st.InstallationID, []string{host.ID, svcHost},
		objectRefs{Networks: []string{bs.InspectionNetwork}, Secrets: []string{bs.BackendSecret, keySecret, initSecret}}); err != nil {
		return api.InspectionStarted{}, err
	}
	digest, err := DefinitionDigest(plan.Operation)
	if err != nil {
		return api.InspectionStarted{}, err
	}
	rec := OperationRecord{ID: opID, Kind: plan.Operation.Kind, Swarm: c.Swarm, Cluster: c.ID, RequestedBy: actor.ID,
		RequestedAt: now, Definition: plan.Operation, ApprovalID: approvalID, ApprovalDigest: digest}
	ir := InspectionRecord{Session: plan.Session, Cluster: c.ID, Swarm: c.Swarm, OpID: opID, InitSecret: initSecret, By: actor.ID}
	if err := b.commit(rec, PutOpOf(CollInspections, session, ir)); err != nil {
		return api.InspectionStarted{}, err
	}
	b.Ops.Svc.recordEvent(c.Swarm, c.ID, api.SeverityWarning, fmt.Sprintf("%s requested inspection %s of backup %s on host %s, operation %s (waits for approval)",
		actor.ID, session, br.ID, host.ID, opID))
	b.Ops.start(l, plan.Operation, nil)
	return api.InspectionStarted{
		ID: opID, Kind: plan.Operation.Kind, Session: session, Backup: br.ID, Host: host.ID, Service: plan.Session.ServiceName,
		Port: plan.Session.Port, ExpiresAt: plan.Session.ExpiresAt, Approval: api.OperationApproval{Digest: digest},
		InspectionAccount: dbbackup.InspectionAccount, InspectionPassword: password,
	}, nil
}

// monitorAccount is the configured monitoring account (galera defaults).
func monitorAccount(a galera.Accounts) dbbackup.LocalAccount {
	acct := dbbackup.LocalAccount{User: a.MonitorUser, Host: a.MonitorHost}
	if acct.User == "" {
		acct.User = galera.DefaultMonitorUser
	}
	if acct.Host == "" {
		acct.Host = galera.DefaultMonitorHost
	}
	return acct
}

func mustInventory(s *Store) dbbackup.Inventory {
	inv, err := productionInventory(s)
	if err != nil {
		// Fail closed: an inventory that cannot be read overlaps everything.
		return dbbackup.Inventory{Networks: []string{"*unreadable*"}, ClusterIDs: []string{"*unreadable*"}}
	}
	return inv
}

func (b *Backups) createSecret(ctx context.Context, swarmID, name, inst, session string, data []byte) error {
	if b.Swarms == nil || b.Swarms.Dial == nil {
		return errNoDial
	}
	sw, ok, err := Get[SwarmRecord](b.store(), CollSwarms, swarmID, Consistent())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: swarm %q", api.ErrNotFound, swarmID)
	}
	conn, err := b.Swarms.Dial(sw.Value.DockerHost)
	if err != nil {
		return err
	}
	_, err = conn.CreateSecret(ctx, name, map[string]string{agent.LabelInstallation: inst, "dbcm.inspection": session}, data)
	return err
}

// --- scheduling and expiry (leader only) ---

// RunSchedules evaluates every enabled policy at now and starts due
// backups as actor (the manager's system subject). Due times are recorded
// as handled whether or not a backup starts, so a refused run is not
// retried in a burst.
func (b *Backups) RunSchedules(actor authz.Subject) {
	pols, err := List[BackupPolicyRecord](b.store(), CollBackupPolicies, Consistent())
	if err != nil {
		b.log("manager: backup schedules not read", "err", err)
		return
	}
	now := b.now()
	for _, p := range pols {
		rec := p.Value
		if !rec.Policy.Enabled {
			continue
		}
		sp, err := schedulePolicy(rec.Policy)
		if err != nil {
			continue
		}
		d, err := sp.Evaluate(rec.Last, now)
		if err != nil || d.ScheduledFor.IsZero() {
			continue
		}
		// Advance first (compare-and-set on the version read): two leaders
		// in a row cannot both start the same scheduled run.
		if _, err := Put(b.store(), CollBackupPolicies, p.Key, BackupPolicyRecord{Cluster: rec.Cluster, Policy: rec.Policy,
			Last: d.ScheduledFor, UpdatedBy: rec.UpdatedBy, UpdatedAt: rec.UpdatedAt}, IfVersion(p.Version)); err != nil {
			continue
		}
		c, _, _ := Get[ClusterRecord](b.store(), CollClusters, rec.Cluster)
		if d.Missed > 0 {
			b.Ops.Svc.recordEvent(c.Value.Swarm, rec.Cluster, api.SeverityWarning, fmt.Sprintf("%d scheduled backup run(s) of %s missed", d.Missed, rec.Cluster))
		}
		if !d.Run {
			continue
		}
		method := dbbackup.Method(rec.Policy.Method)
		if method == "" {
			method = dbbackup.MethodPhysical
		}
		if _, err := b.start(actor, rec.Cluster, "schedule", method, rec.Policy.Databases); err != nil {
			b.Ops.Svc.recordEvent(c.Value.Swarm, rec.Cluster, api.SeverityWarning, fmt.Sprintf("scheduled backup of %s not started: %v", rec.Cluster, err))
		}
	}
}

// createdInstanceID is the service ID reported by the create-instance step
// of an inspection operation.
func createdInstanceID(v *runner.View) string {
	if v == nil {
		return ""
	}
	for _, sv := range v.Steps {
		if !strings.HasSuffix(sv.ID, "-create-instance") || sv.LastResult == nil {
			continue
		}
		var out struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(sv.LastResult.Output, &out) == nil {
			return out.ID
		}
	}
	return ""
}

// teardownPlan plans the removal of an inspection session's job, instance
// (its service ID from the inspection operation's view), init secret and
// volume. Service and secret commands go to the Swarm manager node's agent;
// the init secret is allowed there first. A secret that no longer exists
// is not planned (nothing to remove).
func (b *Backups) teardownPlan(opID string, ir InspectionRecord, view *runner.View, st Settings) (runner.Operation, error) {
	sess := ir.Session
	sess.ServiceID = createdInstanceID(view)
	svcHost, err := b.Ops.serviceHostFor(ir.Swarm)
	if err != nil {
		return runner.Operation{}, err
	}
	req := dbbackup.TeardownRequest{OpID: opID, InstallationID: st.InstallationID, Session: sess, ServiceHost: svcHost,
		InitSecret: ir.InitSecret, StepTimeout: b.Ops.StepTimeout, MaxRetries: b.Ops.MaxRetries}
	if req.InitSecret != dbbackup.InitSecretName(sess.ID) {
		req.InitSecret = "" // records from before the name was fixed: never remove another secret
	}
	td, err := dbbackup.TeardownPlanFor(req, mustInventory(b.store()))
	if err != nil || req.InitSecret == "" {
		return td, err
	}
	err = b.Ops.allowReferenced(&td, ir.Swarm, st.InstallationID, []string{svcHost}, objectRefs{Secrets: []string{req.InitSecret}})
	if errors.Is(err, ErrUnknownReference) {
		req.InitSecret = ""
		return dbbackup.TeardownPlanFor(req, mustInventory(b.store()))
	}
	return td, err
}

// TeardownExpired removes expired inspection instances (B03): an unfinished
// inspection operation is aborted first; on the next pass, when it has
// ended, the teardown operation is started. Teardowns need no approval.
func (b *Backups) TeardownExpired(actor authz.Subject) {
	l, err := b.Ops.leadership()
	if err != nil {
		return
	}
	recs, err := List[InspectionRecord](b.store(), CollInspections, Consistent())
	if err != nil {
		return
	}
	now := b.now()
	st, err := b.Ops.settings()
	if err != nil {
		return
	}
	for _, r := range recs {
		ir := r.Value
		// A teardown (or a cluster deletion that included it) that ended
		// without success is planned again.
		if (ir.TeardownOp != "" && !b.Ops.endedUnsuccessfully(ir.TeardownOp)) || now.Before(ir.Session.ExpiresAt) {
			continue
		}
		op, ok, err := Get[OperationRecord](b.store(), CollOperations, ir.OpID, Consistent())
		if err != nil {
			continue
		}
		if ok && op.Value.EndedAt.IsZero() {
			if err := l.Runner.Abort(ir.OpID); err == nil {
				if v, err := l.Runner.Snapshot(ir.OpID); err == nil {
					_ = b.Ops.Svc.saveView(v)
				}
			}
			continue
		}
		var view *runner.View
		if ok {
			view = op.Value.View
		}
		if ir.TeardownOp != "" {
			// A failed operation keeps its locks until aborted (lab E4 F10).
			_ = l.Runner.Abort(ir.TeardownOp)
		}
		opID := newID("op")
		td, err := b.teardownPlan(opID, ir, view, st)
		if err != nil {
			b.Ops.Svc.recordEvent(ir.Swarm, ir.Cluster, api.SeverityWarning, fmt.Sprintf("inspection %s not torn down: %v", ir.Session.ID, err))
			continue
		}
		rec := OperationRecord{ID: opID, Kind: td.Kind, Swarm: ir.Swarm, Cluster: ir.Cluster, RequestedBy: actor.ID, RequestedAt: now, Definition: td}
		opDoc, err := PutOp(CollOperations, opID, rec, IfVersion(0))
		if err != nil {
			continue
		}
		ir.TeardownOp = opID
		irDoc, err := PutOp(CollInspections, r.Key, ir, IfVersion(r.Version))
		if err != nil {
			continue
		}
		if _, err := b.store().Txn(opDoc, irDoc); err != nil {
			continue
		}
		b.Ops.Svc.recordEvent(ir.Swarm, ir.Cluster, api.SeverityInfo, fmt.Sprintf("inspection %s expired; teardown operation %s", ir.Session.ID, opID))
		b.Ops.start(l, td, nil)
	}
}

// BackupScheduler runs backup schedules and inspection expiry while this
// replica leads. Backups returns the service of the current leadership (it
// needs the leadership's authorizer); nil skips a pass.
type BackupScheduler struct {
	Backups  func() *Backups
	Interval time.Duration // default 1 minute
	// Actor starts scheduled backups; default collect.SystemSubject, which
	// must be bound to the manager system role.
	Actor authz.Subject
}

// Name implements Reconciler.
func (s BackupScheduler) Name() string { return "backup-scheduler" }

// Run implements Reconciler.
func (s BackupScheduler) Run(ctx context.Context, _ *Leadership) error {
	iv := s.Interval
	if iv <= 0 {
		iv = time.Minute
	}
	actor := s.Actor
	if actor.ID == "" {
		actor = collect.SystemSubject
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if s.Backups == nil {
			continue
		}
		if b := s.Backups(); b != nil {
			b.RunSchedules(actor)
			b.failInterruptedCopies(ctx)
			b.TeardownExpired(actor)
			if err := b.ApplyRetention(ctx); err != nil && ctx.Err() == nil {
				b.log("manager: backup retention incomplete", "err", err)
			}
		}
	}
}
