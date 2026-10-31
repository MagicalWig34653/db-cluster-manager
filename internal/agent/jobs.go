// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Backup and restore jobs (ADR 0009).
//
// A job is an owned one-shot Swarm service (replicas 1, restart condition
// "none") pinned to this agent's node. The agent never runs an exec inside
// the database container and never accepts an argv from the manager: the
// manager sends structured arguments, and the agent builds the job's
// command line from a fixed allow-list (JobCommand plus JobArgs). The job
// image runs the dbcm job helper, which in turn only invokes the allow-listed
// tools (internal/dbbackup).
//
// Credentials never travel in arguments: the database option file, the
// backend configuration and (for restores) the recovery key are Swarm
// secrets the manager created; the agent only lets a job reference secrets
// that pass the ownership guard.

// Job command kinds.
const (
	// KindRunBackup launches a backup job (args: RunBackupArgs). A physical
	// backup mounts the verified source volume read-only.
	KindRunBackup Kind = "run-backup"
	// KindRunRestore launches a restore job (args: RunRestoreArgs). It needs
	// an approval ID and refuses targets in use by a non-job service.
	KindRunRestore Kind = "run-restore"
	// KindRemoveJob removes a finished job service by name (args:
	// RemoveJobArgs). Only services labelled as jobs are removed.
	KindRemoveJob Kind = "remove-job"
)

// Job labels.
const (
	LabelJob   = "dbcm.job"    // "backup" or "restore"
	LabelJobID = "dbcm.job-id" // backup ID
)

// Job kinds (values of LabelJob).
const (
	JobBackup  = "backup"
	JobRestore = "restore"
)

// Fixed job contract shared with internal/dbbackup.
const (
	// JobCommand is the job helper inside the job image.
	JobCommand = "/usr/local/bin/dbcm-job"
	// JobVolumeTarget is where a job mounts a database volume (the same
	// layout as the database service: data below /dbcm/data).
	JobVolumeTarget = "/dbcm"
	// JobDataDir is the database datadir inside the job.
	JobDataDir = JobVolumeTarget + "/data"
	// SecretsDir is where Swarm mounts secrets.
	SecretsDir = "/run/secrets"
)

// BackupTool is an allow-listed backup tool.
type BackupTool string

// Allow-listed tools.
const (
	// ToolMariabackup takes a physical, streamed (xbstream) backup.
	ToolMariabackup BackupTool = "mariabackup"
	// ToolMariadbDump takes a logical export of exactly one database.
	ToolMariadbDump BackupTool = "mariadb-dump"
)

// RestoreMode is an allow-listed restore mode.
type RestoreMode string

// Restore modes.
const (
	// RestorePhysical extracts and prepares a physical backup into an empty,
	// newly initialized volume.
	RestorePhysical RestoreMode = "physical"
	// RestoreLogical loads one database export into a running target.
	RestoreLogical RestoreMode = "logical"
)

// JobResources are the mandatory limits of a job.
type JobResources struct {
	NanoCPUs    int64 `json:"nano_cpus"`
	MemoryBytes int64 `json:"memory_bytes"`
}

// RunBackupArgs are the arguments of KindRunBackup.
type RunBackupArgs struct {
	JobName  string     `json:"job_name"`
	Image    string     `json:"image"`
	Tool     BackupTool `json:"tool"`
	BackupID string     `json:"backup_id"`
	// Object is the backend object for the stream; the manifest is stored
	// as Object + ".manifest.json".
	Object string `json:"object"`
	// Expected is the identity of the source node. For a physical backup
	// the marker of SourceVolumeRoot must match it.
	Expected         Identity `json:"expected"`
	SourceVolume     string   `json:"source_volume,omitempty"`
	SourceVolumeRoot string   `json:"source_volume_root,omitempty"`
	// Database is the single database of a logical export.
	Database string `json:"database,omitempty"`
	// DBHost is the source node's hostname on Network.
	DBHost  string `json:"db_host"`
	Network string `json:"network"`
	// DBCredentialSecret holds a client option file of the backup account;
	// BackendSecret the backend configuration.
	DBCredentialSecret string `json:"db_credential_secret"`
	BackendSecret      string `json:"backend_secret"`
	// RecipientKey is the stream public key ("dbcmpk1:..."); not secret.
	RecipientKey string       `json:"recipient_key"`
	Resources    JobResources `json:"resources"`
	// NodeID, if set and not this agent's node, makes this a delegated job:
	// the agent runs on a Swarm manager node (only managers accept service
	// changes; lab E4: a worker's agent got "This node is not a swarm
	// manager"), pins the job to NodeID and skips the source volume checks,
	// which only the node's own agent can make: the plan runs
	// verify-identity there first, in the same fenced operation.
	NodeID string `json:"node_id,omitempty"`
}

// RunRestoreArgs are the arguments of KindRunRestore.
type RunRestoreArgs struct {
	JobName  string      `json:"job_name"`
	Image    string      `json:"image"`
	Mode     RestoreMode `json:"mode"`
	BackupID string      `json:"backup_id"`
	Object   string      `json:"object"`
	// ApprovalID references the user's explicit approval of the restore.
	ApprovalID string `json:"approval_id"`
	// SourceClusterID is the cluster the backup must come from (the job
	// compares it with the authenticated stream header).
	SourceClusterID string `json:"source_cluster_id"`
	// Physical: the new, empty target volume and its fresh identity. The
	// identity's cluster must differ from SourceClusterID.
	TargetVolume     string   `json:"target_volume,omitempty"`
	TargetVolumeRoot string   `json:"target_volume_root,omitempty"`
	Expected         Identity `json:"expected"`
	// Logical: the export's database and the target database name.
	Database           string `json:"database,omitempty"`
	TargetDatabase     string `json:"target_database,omitempty"`
	DBHost             string `json:"db_host,omitempty"`
	Network            string `json:"network,omitempty"`
	DBCredentialSecret string `json:"db_credential_secret,omitempty"`
	// BackendSecret holds the backend configuration, KeySecret the recovery
	// key (a short-lived secret the manager removes after the operation).
	BackendSecret string       `json:"backend_secret"`
	KeySecret     string       `json:"key_secret"`
	Resources     JobResources `json:"resources"`
	// NodeID, if set and not this agent's node, makes this a delegated job
	// like RunBackupArgs.NodeID: the agent runs on a Swarm manager node,
	// pins the job to NodeID and skips the target volume checks only the
	// node's own agent can make (the plan runs check-restore-target there
	// first, in the same fenced operation). The check that no database
	// service mounts the target needs the Swarm service list and stays here.
	NodeID string `json:"node_id,omitempty"`
}

// RemoveJobArgs are the arguments of KindRemoveJob.
type RemoveJobArgs struct {
	JobName string `json:"job_name"`
}

// Job outcome codes.
const (
	CodeNodeNotConfigured Code = "node-not-configured"
	CodeVolumeInUse       Code = "volume-in-use"
	CodeNotJob            Code = "not-a-job"
	CodeSameCluster       Code = "same-cluster"
)

var (
	// ErrVolumeInUse: a restore target is mounted by a non-job service.
	ErrVolumeInUse = errors.New("agent: volume is used by a database service")
	// ErrNotJob: remove-job named a service that is not a job.
	ErrNotJob = errors.New("agent: service is not a job")
	// ErrSameCluster: the restore target belongs to the backup's own cluster.
	ErrSameCluster = errors.New("agent: restore target identity belongs to the source cluster")
	// ErrNodeNotConfigured: Config.NodeID is empty, so a job cannot be pinned.
	ErrNodeNotConfigured = errors.New("agent: swarm node ID not configured")
)

var (
	jobNameRe   = regexp.MustCompile(`^dbcm-job-[a-z0-9][a-z0-9-]{0,79}$`)
	backupIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	objectRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)
	dbNameRe    = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	hostRe      = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252}[A-Za-z0-9])?$`)
	secretRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	pubKeyRe    = regexp.MustCompile(`^dbcmpk1:[A-Za-z0-9_-]{43}$`)
	approvalRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	reservedDBs = map[string]bool{"mysql": true, "sys": true, "information_schema": true, "performance_schema": true}
)

func checkResources(r JobResources) error {
	if r.NanoCPUs <= 0 || r.MemoryBytes <= 0 {
		return invalid("job resources: CPU and memory limits are required")
	}
	return nil
}

func checkCommon(jobName, image, backupID, object, backendSecret string) error {
	switch {
	case !jobNameRe.MatchString(jobName):
		return invalid("job_name %q (want dbcm-job-[a-z0-9-]+)", jobName)
	case image == "":
		return invalid("image required")
	case !backupIDRe.MatchString(backupID):
		return invalid("backup_id %q", backupID)
	case !objectRe.MatchString(object):
		return invalid("object %q", object)
	case !secretRe.MatchString(backendSecret):
		return invalid("backend_secret %q", backendSecret)
	}
	return nil
}

func checkRunBackup(a *RunBackupArgs, volumeBase string) error {
	if err := checkCommon(a.JobName, a.Image, a.BackupID, a.Object, a.BackendSecret); err != nil {
		return err
	}
	if !a.Expected.complete() {
		return invalid("expected source identity required")
	}
	if !hostRe.MatchString(a.DBHost) || a.Network == "" || !secretRe.MatchString(a.DBCredentialSecret) {
		return invalid("db_host, network and db_credential_secret required")
	}
	if !pubKeyRe.MatchString(a.RecipientKey) {
		return invalid("recipient_key must be a dbcmpk1 public key")
	}
	switch a.Tool {
	case ToolMariabackup:
		if a.SourceVolume == "" || a.Database != "" {
			return invalid("mariabackup needs source_volume and no database")
		}
		if err := checkVolumeRoot(a.SourceVolumeRoot, volumeBase); err != nil {
			return err
		}
	case ToolMariadbDump:
		if a.SourceVolume != "" || a.SourceVolumeRoot != "" {
			return invalid("mariadb-dump does not mount a volume")
		}
		if !dbNameRe.MatchString(a.Database) || reservedDBs[a.Database] {
			return invalid("database %q", a.Database)
		}
	default:
		return invalid("tool %q is not allow-listed", a.Tool)
	}
	if strings.ContainsAny(a.NodeID, "/ \t\r\n=") {
		return invalid("node_id %q", a.NodeID)
	}
	return checkResources(a.Resources)
}

func checkRunRestore(a *RunRestoreArgs, volumeBase string) error {
	if err := checkCommon(a.JobName, a.Image, a.BackupID, a.Object, a.BackendSecret); err != nil {
		return err
	}
	if !approvalRe.MatchString(a.ApprovalID) {
		return invalid("approval_id required: restores need an explicit approval")
	}
	if a.SourceClusterID == "" || !secretRe.MatchString(a.KeySecret) {
		return invalid("source_cluster_id and key_secret required")
	}
	switch a.Mode {
	case RestorePhysical:
		if a.TargetVolume == "" || !a.Expected.complete() {
			return invalid("physical restore needs target_volume and its expected identity")
		}
		if a.Database != "" || a.TargetDatabase != "" || a.DBHost != "" {
			return invalid("physical restore takes no database or db_host")
		}
		if err := checkVolumeRoot(a.TargetVolumeRoot, volumeBase); err != nil {
			return err
		}
	case RestoreLogical:
		if a.TargetVolume != "" || a.TargetVolumeRoot != "" {
			return invalid("logical restore does not mount a volume")
		}
		if !dbNameRe.MatchString(a.Database) || !dbNameRe.MatchString(a.TargetDatabase) || reservedDBs[a.TargetDatabase] {
			return invalid("database %q / target_database %q", a.Database, a.TargetDatabase)
		}
		if !hostRe.MatchString(a.DBHost) || a.Network == "" || !secretRe.MatchString(a.DBCredentialSecret) {
			return invalid("db_host, network and db_credential_secret required")
		}
	default:
		return invalid("restore mode %q is not allow-listed", a.Mode)
	}
	if strings.ContainsAny(a.NodeID, "/ \t\r\n=") {
		return invalid("node_id %q", a.NodeID)
	}
	return checkResources(a.Resources)
}

// BackupJobArgs builds the job helper's argv for a backup. The helper
// (internal/dbbackup) parses exactly these flags.
func BackupJobArgs(a RunBackupArgs, dbSecretName, backendSecretName string) []string {
	args := []string{
		"backup",
		"--job=" + a.JobName,
		"--tool=" + string(a.Tool),
		"--backup-id=" + a.BackupID,
		"--object=" + a.Object,
		"--cluster-id=" + a.Expected.ClusterID,
		"--node-uuid=" + a.Expected.NodeUUID,
		"--db-host=" + a.DBHost,
		"--db-option-file=" + filepath.Join(SecretsDir, dbSecretName),
		"--backend-config=" + filepath.Join(SecretsDir, backendSecretName),
		"--recipient=" + a.RecipientKey,
	}
	if a.Tool == ToolMariabackup {
		args = append(args, "--datadir="+JobDataDir)
	} else {
		args = append(args, "--database="+a.Database)
	}
	return args
}

// RestoreJobArgs builds the job helper's argv for a restore.
func RestoreJobArgs(a RunRestoreArgs, secretNames map[string]string) []string {
	args := []string{
		"restore",
		"--job=" + a.JobName,
		"--mode=" + string(a.Mode),
		"--backup-id=" + a.BackupID,
		"--object=" + a.Object,
		"--approval-id=" + a.ApprovalID,
		"--source-cluster-id=" + a.SourceClusterID,
		"--backend-config=" + filepath.Join(SecretsDir, secretNames[a.BackendSecret]),
		"--key-file=" + filepath.Join(SecretsDir, secretNames[a.KeySecret]),
	}
	if a.Mode == RestorePhysical {
		args = append(args, "--datadir="+JobDataDir, "--target-cluster-id="+a.Expected.ClusterID)
	} else {
		args = append(args, "--database="+a.Database, "--target-database="+a.TargetDatabase,
			"--db-host="+a.DBHost, "--db-option-file="+filepath.Join(SecretsDir, secretNames[a.DBCredentialSecret]))
	}
	return args
}

// jobBaseOn builds the common job service spec for a job pinned to node.
func (e *Executor) jobBaseOn(node, name, image, kind, backupID string, res JobResources, cluster string) (ServiceSpec, Result, bool) {
	if e.cfg.NodeID == "" || node == "" {
		return ServiceSpec{}, Result{Status: StatusRefused, Code: CodeNodeNotConfigured, Message: ErrNodeNotConfigured.Error()}, false
	}
	labels := map[string]string{LabelJob: kind, LabelJobID: backupID}
	if cluster != "" {
		labels[LabelCluster] = cluster
	}
	return ServiceSpec{
		Name: name, Image: image, Labels: labels,
		Command:       []string{JobCommand},
		Constraints:   []string{"node.id==" + node},
		Replicas:      1,
		RestartPolicy: &RestartPolicy{Condition: "none"},
		Resources: &Resources{
			Limits:       &ResourceLimits{NanoCPUs: res.NanoCPUs, MemoryBytes: res.MemoryBytes},
			Reservations: &ResourceReservations{NanoCPUs: res.NanoCPUs, MemoryBytes: res.MemoryBytes},
		},
	}, Result{}, true
}

// secretNames resolves secret references to the names under which Swarm
// mounts them, after the ownership check.
func (e *Executor) secretNames(ctx context.Context, refs ...string) (map[string]string, Result, bool) {
	out := map[string]string{}
	for _, ref := range refs {
		obj, err := e.docker.Inspect(ctx, KindSecret, ref)
		if err != nil {
			return nil, outcome(fmt.Errorf("secret %q: %w", ref, err)), false
		}
		if err := e.owner.Check(obj, KindSecret); err != nil {
			return nil, outcome(fmt.Errorf("referenced secret %q: %w", ref, err)), false
		}
		name := obj.Name
		if name == "" {
			name = ref
		}
		out[ref] = name
	}
	return out, Result{}, true
}

// ownedVolumeAt checks that volume is owned and bound to root, and returns
// the resolved root.
func (e *Executor) ownedVolumeAt(ctx context.Context, volume, root string) (string, Result, bool) {
	vol, res, ok := e.owned(ctx, KindVolume, volume)
	if !ok {
		return "", res, false
	}
	if vol.Mountpoint == "" || filepath.Clean(vol.Mountpoint) != root {
		return "", Result{Status: StatusRefused, Code: CodeVolumeMismatch,
			Message: fmt.Sprintf("volume %q mountpoint %q is not %s", volume, vol.Mountpoint, root)}, false
	}
	return e.resolveVolumeRoot(root)
}

func (e *Executor) runBackup(ctx context.Context, cmd Command, a RunBackupArgs) Result {
	node, delegated := e.cfg.NodeID, a.NodeID != "" && a.NodeID != e.cfg.NodeID
	if delegated {
		node = a.NodeID
	}
	spec, res, ok := e.jobBaseOn(node, a.JobName, a.Image, JobBackup, a.BackupID, a.Resources, a.Expected.ClusterID)
	if !ok {
		return res
	}
	if a.Tool == ToolMariabackup && delegated {
		spec.Mounts = []Mount{{Type: "volume", Source: a.SourceVolume, Target: JobVolumeTarget, ReadOnly: true}}
	} else if a.Tool == ToolMariabackup {
		root, res, ok := e.ownedVolumeAt(ctx, a.SourceVolume, a.SourceVolumeRoot)
		if !ok {
			return res
		}
		if err := Verify(root, a.Expected); err != nil {
			return outcome(err)
		}
		spec.Mounts = []Mount{{Type: "volume", Source: a.SourceVolume, Target: JobVolumeTarget, ReadOnly: true}}
	}
	names, res, ok := e.secretNames(ctx, a.DBCredentialSecret, a.BackendSecret)
	if !ok {
		return res
	}
	spec.Networks = []string{a.Network}
	spec.Secrets = []string{a.DBCredentialSecret, a.BackendSecret}
	spec.Args = BackupJobArgs(a, names[a.DBCredentialSecret], names[a.BackendSecret])
	return e.createService(ctx, cmd, spec)
}

func (e *Executor) runRestore(ctx context.Context, cmd Command, a RunRestoreArgs) Result {
	cluster := a.Expected.ClusterID
	node, delegated := e.cfg.NodeID, a.NodeID != "" && a.NodeID != e.cfg.NodeID
	if delegated {
		node = a.NodeID
	}
	spec, res, ok := e.jobBaseOn(node, a.JobName, a.Image, JobRestore, a.BackupID, a.Resources, cluster)
	if !ok {
		return res
	}
	refs := []string{a.BackendSecret, a.KeySecret}
	if a.Mode == RestorePhysical {
		if a.Expected.ClusterID == a.SourceClusterID {
			return Result{Status: StatusRefused, Code: CodeSameCluster,
				Message: fmt.Sprintf("%v: target cluster %s; a restore always gets a new identity", ErrSameCluster, a.SourceClusterID)}
		}
		if !delegated {
			root, res, ok := e.ownedVolumeAt(ctx, a.TargetVolume, a.TargetVolumeRoot)
			if !ok {
				return res
			}
			if err := Verify(root, a.Expected); err != nil {
				return outcome(err)
			}
			if err := emptyOrMissing(filepath.Join(root, "data")); err != nil {
				return outcome(err)
			}
		}
		if res, ok := e.notMountedByService(ctx, a.TargetVolume, a.TargetVolumeRoot); !ok {
			return res
		}
		spec.Mounts = []Mount{{Type: "volume", Source: a.TargetVolume, Target: JobVolumeTarget}}
	} else {
		refs = append(refs, a.DBCredentialSecret)
		spec.Networks = []string{a.Network}
	}
	names, res, ok := e.secretNames(ctx, refs...)
	if !ok {
		return res
	}
	spec.Secrets = refs
	spec.Args = RestoreJobArgs(a, names)
	return e.createService(ctx, cmd, spec)
}

// notMountedByService refuses a volume that any owned non-job service
// mounts, running or scaled to zero: such a volume belongs to a database
// node and a restore must never write into it.
func (e *Executor) notMountedByService(ctx context.Context, volume, root string) (Result, bool) {
	objs, err := e.docker.ListByLabel(ctx, KindService, LabelInstallation, e.cfg.InstallationID)
	if err != nil {
		return outcome(err), false
	}
	for _, o := range objs {
		if o.Labels[LabelJob] != "" {
			continue
		}
		for _, m := range o.Mounts {
			src := filepath.Clean(m.Source)
			if m.Source == volume || src == root || within(root, src) {
				return Result{Status: StatusRefused, Code: CodeVolumeInUse,
					Message: fmt.Sprintf("%v: service %q (replicas %s) mounts %s", ErrVolumeInUse, o.Name, strconv.FormatUint(o.Replicas, 10), volume)}, false
			}
		}
	}
	return Result{}, true
}

func emptyOrMissing(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: restore target %s already holds data", ErrUnknownData, dir)
	}
	return nil
}

func (e *Executor) removeJob(ctx context.Context, a RemoveJobArgs) Result {
	objs, err := e.docker.ListByLabel(ctx, KindService, LabelInstallation, e.cfg.InstallationID)
	if err != nil {
		return outcome(err)
	}
	for _, o := range objs {
		if o.Name != a.JobName {
			continue
		}
		if o.Labels[LabelJob] == "" {
			return Result{Status: StatusRefused, Code: CodeNotJob, Message: fmt.Sprintf("%v: %q", ErrNotJob, a.JobName)}
		}
		return e.removeService(ctx, o.ID)
	}
	// Already gone: removing a job is idempotent by name.
	return succeeded(map[string]string{"removed": "absent"})
}
