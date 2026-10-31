// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Gate conditions evaluated by Checker.
const (
	// CondSourceEligible: Target (a slot) still passes the source rules.
	CondSourceEligible = "backup-source-eligible"
	// CondBackupComplete: the manifest of Target (an object) exists, is
	// valid, and matches the stored object.
	CondBackupComplete = "backup-complete"
	// CondRestoreComplete: the job Target reported success.
	CondRestoreComplete = "restore-complete"
	// CondRestoreVerified: the verification of Target (a session ID) passed.
	CondRestoreVerified = "restore-verified"
	// CondApproved: the approval with ID Target exists and matches the
	// operation. Shared with galera plans.
	CondApproved = runner.CondApproved
)

// Operation kinds.
const (
	OpBackup         = "dbbackup/backup"
	OpRestoreCluster = "dbbackup/restore-cluster"
	OpInspection     = "dbbackup/inspection"
	OpTeardown       = "dbbackup/inspection-teardown"
	OpSelective      = "dbbackup/selective-restore"
	OpBinlogArchive  = "dbbackup/binlog-archive"
)

// BackupRequest describes one backup run.
type BackupRequest struct {
	OpID           string
	BackupID       string
	InstallationID string
	ClusterID      string
	ClusterName    string
	Method         Method
	// Databases are exported one job each (logical method only).
	Databases []string
	// JobImage contains the dbcm job helper and the tools.
	JobImage string
	// Network is the cluster network; the job reaches the node by its
	// service hostname there.
	Network            string
	DBCredentialSecret string
	BackendSecret      string
	// RecipientKey is the stream public key; the recovery key itself is
	// never needed for a backup.
	RecipientKey string
	VolumeBase   string
	Job          agent.JobResources
	StepTimeout  time.Duration
	MaxRetries   int
	// ServiceHost is the host ID of the agent that creates and removes the
	// job services (a Swarm manager node, lab E4); empty keeps them on the
	// source host. The source host's agent still verifies the volume.
	ServiceHost string
}

// BackupPlanResult is a planned backup.
type BackupPlanResult struct {
	Operation  runner.Operation `json:"operation"`
	Source     NodeState        `json:"source"`
	Objects    []string         `json:"objects"`
	Rejections []Rejection      `json:"rejections,omitempty"`
}

func (r BackupRequest) validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(f, a...)) }
	switch {
	case r.OpID == "" || r.InstallationID == "" || r.ClusterID == "" || r.ClusterName == "":
		return bad("op, installation, cluster ID and name required")
	case !backupIDRe.MatchString(r.BackupID):
		return bad("backup ID %q (lowercase [a-z0-9-])", r.BackupID)
	case r.JobImage == "" || r.Network == "" || r.DBCredentialSecret == "" || r.BackendSecret == "" || r.RecipientKey == "":
		return bad("job image, network, credential secret, backend secret and recipient key required")
	case r.VolumeBase == "" || !filepath.IsAbs(r.VolumeBase):
		return bad("absolute volume base required")
	case r.Job.NanoCPUs <= 0 || r.Job.MemoryBytes <= 0:
		return bad("job CPU and memory limits required")
	}
	switch r.Method {
	case MethodPhysical:
		if len(r.Databases) > 0 {
			return bad("physical backups cover the whole node; databases not allowed")
		}
	case MethodLogical:
		if len(r.Databases) == 0 {
			return bad("logical backup needs at least one database")
		}
	default:
		return bad("method %q", r.Method)
	}
	return nil
}

type jobDemand agent.JobResources

// amount converts job limits into the placement unit.
func (j jobDemand) amount() galera.ResourceAmount {
	return galera.ResourceAmount{MilliCPU: j.NanoCPUs / 1e6, MemoryBytes: j.MemoryBytes}
}

type steps struct {
	list []runner.Step
	b    BackupRequest
}

func (s *steps) add(st runner.Step, pre ...runner.Condition) string {
	if n := len(s.list); n > 0 {
		st.Preconditions = append([]runner.Condition{{Kind: runner.CondStepSucceeded, StepID: s.list[n-1].ID}}, st.Preconditions...)
	}
	st.Preconditions = append(st.Preconditions, pre...)
	st.ID = fmt.Sprintf("%02d-%s", len(s.list)+1, st.ID)
	if st.Timeout == 0 {
		st.Timeout = s.b.StepTimeout
	}
	if st.MaxRetries == 0 {
		st.MaxRetries = s.b.MaxRetries
	}
	s.list = append(s.list, st)
	return st.ID
}

func command(id, target string, kind agent.Kind, args any) runner.Step {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(fmt.Sprintf("dbbackup: encode %s args: %v", kind, err)) // only plain structs are encoded
	}
	return runner.Step{ID: id, Target: target, Command: kind, Args: raw}
}

func gate(id string, conds ...runner.Condition) runner.Step {
	return runner.Step{ID: id, Preconditions: conds}
}

// BackupPlan selects the source node and plans the backup operation:
//
//	check-source (gate: node-synced + source rules re-checked at start)
//	verify-identity (physical: the agent checks the source volume marker)
//	run-backup        one owned one-shot job per object, pinned to the node
//	wait-backup       gate: manifest present and consistent
//	remove-job
//
// The operation locks the cluster, so no replacement or recovery runs while
// the backup reads a node. Nothing is executed here.
func BackupPlan(req BackupRequest, nodes []NodeState, p SourcePolicy) (BackupPlanResult, error) {
	if err := req.validate(); err != nil {
		return BackupPlanResult{}, err
	}
	p.Job = jobDemand(req.Job).amount()
	src, rej, err := SelectSource(nodes, p)
	if err != nil {
		return BackupPlanResult{Rejections: rej}, err
	}
	expected := agent.Identity{InstallationID: req.InstallationID, ClusterID: req.ClusterID, NodeUUID: src.NodeUUID}
	root := filepath.Join(req.VolumeBase, src.Volume)
	host := galera.ServiceName(req.ClusterName, src.Slot)
	s := &steps{b: req}
	s.add(gate("check-source"),
		runner.Condition{Kind: runner.CondNodeSynced, Target: src.Slot, ClusterSize: p.ClusterSize},
		runner.Condition{Kind: CondSourceEligible, Target: src.Slot, ClusterSize: p.ClusterSize})
	base := agent.RunBackupArgs{
		Image: req.JobImage, BackupID: req.BackupID, Expected: expected, DBHost: host, Network: req.Network,
		DBCredentialSecret: req.DBCredentialSecret, BackendSecret: req.BackendSecret, RecipientKey: req.RecipientKey,
		Resources: req.Job,
	}
	var objects []string
	type job struct {
		suffix string
		args   agent.RunBackupArgs
	}
	var jobs []job
	if req.Method == MethodPhysical {
		s.add(command("verify-identity", src.HostID, agent.KindVerifyIdentity, agent.VerifyIdentityArgs{VolumeRoot: root, Expected: expected}))
		a := base
		a.Tool, a.SourceVolume, a.SourceVolumeRoot = agent.ToolMariabackup, src.Volume, root
		a.Object = ObjectName(req.ClusterID, req.BackupID, MethodPhysical, "")
		a.JobName = JobName(req.BackupID, "")
		jobs = append(jobs, job{"", a})
	} else {
		dbs := slices.Clone(req.Databases)
		slices.Sort(dbs)
		for _, db := range dbs {
			a := base
			a.Tool, a.Database = agent.ToolMariadbDump, db
			a.Object = ObjectName(req.ClusterID, req.BackupID, MethodLogical, db)
			a.JobName = JobName(req.BackupID, db)
			jobs = append(jobs, job{"-" + db, a})
		}
	}
	names := map[string]bool{}
	for _, j := range jobs {
		if names[j.args.JobName] {
			return BackupPlanResult{}, fmt.Errorf("%w: job name %s is not unique (database names differ only in case or are too long)", ErrInvalidRequest, j.args.JobName)
		}
		names[j.args.JobName] = true
		jobHost := src.HostID
		if req.ServiceHost != "" && req.ServiceHost != src.HostID {
			jobHost, j.args.NodeID = req.ServiceHost, src.HostID
		}
		s.add(command("run-backup"+j.suffix, jobHost, agent.KindRunBackup, j.args))
		s.add(gate("wait-backup"+j.suffix, runner.Condition{Kind: CondBackupComplete, Target: j.args.Object}))
		s.add(command("remove-job"+j.suffix, jobHost, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: j.args.JobName}))
		objects = append(objects, j.args.Object)
	}
	op := runner.Operation{ID: req.OpID, Kind: OpBackup, Locks: []string{"cluster/" + req.ClusterID, "node/" + req.ClusterID + "/" + src.Slot},
		Steps: s.list}
	if err := op.Validate(); err != nil {
		return BackupPlanResult{}, err
	}
	return BackupPlanResult{Operation: op, Source: src, Objects: objects, Rejections: rej}, nil
}

// BinlogArchiveRequest describes one run of the archive job for a full backup.
type BinlogArchiveRequest struct {
	OpID string
	// Base is the full backup (with a binlog position) the closed binlog
	// files extend; they are read from the node it was taken from.
	Base           Manifest
	InstallationID string
	ClusterName    string
	JobImage       string
	// Network, DBCredentialSecret, BackendSecret, RecipientKey, VolumeBase,
	// Job, StepTimeout, MaxRetries and ServiceHost are as in BackupRequest.
	Network            string
	DBCredentialSecret string
	BackendSecret      string
	RecipientKey       string
	VolumeBase         string
	Job                agent.JobResources
	ClusterSize        int
	StepTimeout        time.Duration
	MaxRetries         int
	ServiceHost        string
}

// BinlogArchivePlan plans the archive job for the node the full backup was
// taken from (the binlog files exist only there): gate on the node being
// Synced, verify its volume identity, run the job (it rotates the log and
// archives the closed files), wait for its report, remove it. A replaced
// source node ends the chain: the plan refuses and a new full backup is
// needed. The operation holds the same locks as a backup.
func BinlogArchivePlan(req BinlogArchiveRequest, nodes []NodeState) (runner.Operation, NodeState, error) {
	b := req.Base
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(f, a...)) }
	switch {
	case req.OpID == "" || req.InstallationID == "" || req.ClusterName == "":
		return runner.Operation{}, NodeState{}, bad("op, installation and cluster name required")
	case b.Method != MethodPhysical || b.BinlogBase == nil:
		return runner.Operation{}, NodeState{}, bad("backup %s has no binlog position (the binary log was off when it ran)", b.BackupID)
	case req.JobImage == "" || req.Network == "" || req.DBCredentialSecret == "" || req.BackendSecret == "" || req.RecipientKey == "" || !filepath.IsAbs(req.VolumeBase):
		return runner.Operation{}, NodeState{}, bad("job image, network, secrets, recipient key and absolute volume base required")
	case req.Job.NanoCPUs <= 0 || req.Job.MemoryBytes <= 0:
		return runner.Operation{}, NodeState{}, bad("job CPU and memory limits required")
	}
	var src *NodeState
	for i := range nodes {
		if nodes[i].NodeUUID == b.SourceNodeUUID {
			src = &nodes[i]
		}
	}
	if src == nil || src.HostID == "" || src.Volume == "" {
		return runner.Operation{}, NodeState{}, fmt.Errorf("%w: the node backup %s was taken from no longer exists (replaced); its binlog files are gone, take a new full backup", ErrNoSource, b.BackupID)
	}
	expected := agent.Identity{InstallationID: req.InstallationID, ClusterID: b.ClusterID, NodeUUID: src.NodeUUID}
	root := filepath.Join(req.VolumeBase, src.Volume)
	a := agent.RunBackupArgs{
		JobName: JobName(b.BackupID, "arch-"+req.OpID), Image: req.JobImage, Tool: agent.ToolBinlogArchive, BackupID: b.BackupID,
		Object: b.Object, Expected: expected, SourceVolume: src.Volume, SourceVolumeRoot: root,
		DBHost: galera.ServiceName(req.ClusterName, src.Slot), Network: req.Network, DBCredentialSecret: req.DBCredentialSecret,
		BackendSecret: req.BackendSecret, RecipientKey: req.RecipientKey, Resources: req.Job,
	}
	jobHost := src.HostID
	if req.ServiceHost != "" && req.ServiceHost != src.HostID {
		jobHost, a.NodeID = req.ServiceHost, src.HostID
	}
	s := &steps{b: BackupRequest{StepTimeout: req.StepTimeout, MaxRetries: req.MaxRetries}}
	s.add(gate("check-source"), runner.Condition{Kind: runner.CondNodeSynced, Target: src.Slot, ClusterSize: req.ClusterSize})
	s.add(command("verify-identity", src.HostID, agent.KindVerifyIdentity, agent.VerifyIdentityArgs{VolumeRoot: root, Expected: expected}))
	s.add(command("run-archive", jobHost, agent.KindRunBackup, a))
	s.add(gate("wait-archive", runner.Condition{Kind: CondRestoreComplete, Target: a.JobName}))
	s.add(command("remove-job", jobHost, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: a.JobName}))
	op := runner.Operation{ID: req.OpID, Kind: OpBinlogArchive, Locks: []string{"cluster/" + b.ClusterID, "node/" + b.ClusterID + "/" + src.Slot}, Steps: s.list}
	return op, *src, op.Validate()
}

// OpJobCleanup removes the one-shot job services an aborted backup left.
const OpJobCleanup = "dbbackup/job-cleanup"

// JobCleanupPlan returns the operation that removes every job service the
// run-backup steps of an aborted operation may have started (lab E14: an
// aborted backup left its dbcm-job-* service at 0/1). Removing a job that
// does not exist is not an error. ok is false when def starts no job.
func JobCleanupPlan(opID string, def runner.Operation) (runner.Operation, bool) {
	s := &steps{}
	for _, st := range def.Steps {
		var a agent.RunBackupArgs
		if st.Command != agent.KindRunBackup || json.Unmarshal(st.Args, &a) != nil || a.JobName == "" {
			continue
		}
		rm := command(fmt.Sprintf("remove-job-%d", len(s.list)+1), st.Target, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: a.JobName})
		rm.Timeout, rm.MaxRetries = st.Timeout, st.MaxRetries
		s.add(rm)
	}
	if len(s.list) == 0 {
		return runner.Operation{}, false
	}
	op := runner.Operation{ID: opID, Kind: OpJobCleanup, Locks: []string{"job-cleanup/" + def.ID}, Steps: s.list}
	return op, op.Validate() == nil
}

// destructive reports whether a step of a restore operation needs approval:
// every command that is not read-only.
func destructive(s runner.Step) bool {
	return s.Command != "" && !runner.Idempotent(s.Command)
}

// requireApproval adds the approval condition to every destructive step.
func requireApproval(op *runner.Operation, approvalID string) {
	for i := range op.Steps {
		if destructive(op.Steps[i]) {
			op.Steps[i].Preconditions = append(op.Steps[i].Preconditions, runner.Condition{Kind: CondApproved, Target: approvalID})
		}
	}
}
