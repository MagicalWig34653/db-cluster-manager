// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Inventory is the live production state a restore must never touch.
type Inventory struct {
	ClusterIDs   []string
	ClusterNames []string
	Networks     []string
	Volumes      []string
	NodeUUIDs    []string
	// ServiceNames are the names of all managed services (database nodes).
	ServiceNames []string
}

func (inv Inventory) overlap(clusterID, clusterName, network string, volumes, uuids, services []string) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrProductionTarget, fmt.Sprintf(f, a...)) }
	switch {
	case slices.Contains(inv.ClusterIDs, clusterID):
		return bad("cluster ID %s is a live cluster", clusterID)
	case clusterName != "" && slices.Contains(inv.ClusterNames, clusterName):
		return bad("cluster name %s is in use; Galera membership is keyed by name and address", clusterName)
	case slices.Contains(inv.Networks, network):
		return bad("network %s carries production traffic; restores use their own network", network)
	}
	for _, v := range volumes {
		if slices.Contains(inv.Volumes, v) {
			return bad("volume %s belongs to a production node", v)
		}
	}
	for _, u := range uuids {
		if slices.Contains(inv.NodeUUIDs, u) {
			return bad("node UUID %s is a production node", u)
		}
	}
	for _, s := range services {
		if slices.Contains(inv.ServiceNames, s) {
			return bad("service name %s is in use", s)
		}
	}
	return nil
}

// RestoreSource names the backup to restore and how to read it.
type RestoreSource struct {
	Manifest Manifest
	// ClusterID is the cluster the user chose the backup from; it must match
	// the manifest.
	ClusterID     string
	BackendSecret string
	// KeySecret is a short-lived Swarm secret holding the recovery key. The
	// manager creates it before the operation and removes it afterwards.
	KeySecret string
}

func (s RestoreSource) check() error {
	if err := s.Manifest.Validate(); err != nil {
		return err
	}
	if s.Manifest.Method != MethodPhysical {
		return fmt.Errorf("%w: a full or inspection restore needs a physical backup, got %s", ErrInvalidRequest, s.Manifest.Method)
	}
	if s.ClusterID == "" || s.Manifest.ClusterID != s.ClusterID {
		return fmt.Errorf("%w: backup %s is from cluster %q, requested %q", ErrIdentityMismatch, s.Manifest.BackupID, s.Manifest.ClusterID, s.ClusterID)
	}
	if s.BackendSecret == "" || s.KeySecret == "" {
		return fmt.Errorf("%w: backend and key secrets required", ErrInvalidRequest)
	}
	return nil
}

// ClusterRestoreRequest restores a physical backup into a NEW cluster.
type ClusterRestoreRequest struct {
	OpID       string
	ApprovalID string
	Source     RestoreSource
	// Target is the new cluster: new IDs, a name and network unused by
	// production. Members are derived (n1..nSize); n1 receives the backup
	// and bootstraps, the others join by SST.
	Target    galera.Topology
	Hosts     []galera.Host
	Placement galera.PlacementPolicy
	// Bindings for the galera steps (OpID is taken from this request).
	Bindings runner.GaleraBindings
	JobImage string
	Job      agent.JobResources
}

// ClusterRestoreResult is a planned full restore.
type ClusterRestoreResult struct {
	Operation runner.Operation `json:"operation"`
	Plan      galera.Plan      `json:"plan"`
}

// ClusterRestorePlan plans a full restore into a new cluster. It reuses the
// tested cluster-creation plan and inserts, between n1's identity marker and
// its first start, a physical restore job, a gate waiting for its success,
// and the job's removal. The restore job removes Galera state files, so n1
// bootstraps a NEW Galera history (new state UUID) under the new cluster
// name; production nodes cannot join it and it cannot join production.
// Every command step requires the approval.
func ClusterRestorePlan(req ClusterRestoreRequest, inv Inventory) (ClusterRestoreResult, error) {
	if err := req.Source.check(); err != nil {
		return ClusterRestoreResult{}, err
	}
	if req.ApprovalID == "" {
		return ClusterRestoreResult{}, ErrApprovalRequired
	}
	if req.OpID == "" || req.JobImage == "" || req.Job.NanoCPUs <= 0 || req.Job.MemoryBytes <= 0 {
		return ClusterRestoreResult{}, fmt.Errorf("%w: op ID, job image and job limits required", ErrInvalidRequest)
	}
	c := req.Target.Cluster
	if c.ClusterID == req.Source.ClusterID {
		return ClusterRestoreResult{}, fmt.Errorf("%w: target cluster ID equals the backup's cluster; a restore always gets a new identity", ErrProductionTarget)
	}
	plan, err := galera.PlanCreate(req.Target, req.Hosts, req.Placement)
	if err != nil {
		return ClusterRestoreResult{}, err
	}
	var vols, uuids, svcs []string
	for _, n := range plan.Nodes {
		vols, uuids = append(vols, n.Volume), append(uuids, n.NodeUUID)
		svcs = append(svcs, galera.ServiceName(c.Name, n.Slot))
	}
	if err := inv.overlap(c.ClusterID, c.Name, c.Network, vols, uuids, svcs); err != nil {
		return ClusterRestoreResult{}, err
	}
	b := req.Bindings
	b.OpID = req.OpID
	op, err := runner.FromGaleraPlan(plan, b)
	if err != nil {
		return ClusterRestoreResult{}, err
	}
	op.Kind = OpRestoreCluster
	n1 := plan.Nodes[0]
	// The restore runs once the new volume has its identity and its
	// database directories (the job extracts into data/ as the database user).
	at := slices.IndexFunc(op.Steps, func(s runner.Step) bool { return s.Command == agent.KindInitVolumeDirs })
	if at < 0 || op.Steps[at].Target != n1.HostID {
		return ClusterRestoreResult{}, fmt.Errorf("%w: cluster plan has no init-volume-dirs step for %s", ErrInvalidRequest, n1.Slot)
	}
	m := req.Source.Manifest
	job := JobName(m.BackupID, "restore-"+c.Name)
	expected := agent.Identity{InstallationID: c.InstallationID, ClusterID: c.ClusterID, NodeUUID: n1.NodeUUID}
	prev := op.Steps[at].ID
	ra := agent.RunRestoreArgs{
		JobName: job, Image: req.JobImage, Mode: agent.RestorePhysical, BackupID: m.BackupID, Object: m.Object,
		ApprovalID: req.ApprovalID, SourceClusterID: req.Source.ClusterID,
		TargetVolume: n1.Volume, TargetVolumeRoot: filepath.Join(b.VolumeBase, n1.Volume), Expected: expected,
		BackendSecret: req.Source.BackendSecret, KeySecret: req.Source.KeySecret, Resources: req.Job,
	}
	jobHost, local := delegate(b.ServiceHost, n1.HostID, &ra)
	var ins []runner.Step
	if local != nil {
		ins = append(ins, command("check-restore-target-"+n1.Slot, n1.HostID, agent.KindCheckRestoreTarget, *local))
	}
	ins = append(ins,
		command("restore-"+n1.Slot, jobHost, agent.KindRunRestore, ra),
		gate("wait-restore-"+n1.Slot, runner.Condition{Kind: CondRestoreComplete, Target: job}),
		command("remove-restore-job-"+n1.Slot, jobHost, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: job}),
	)
	for i := range ins {
		ins[i].Timeout, ins[i].MaxRetries = b.StepTimeout, b.MaxRetries
		ins[i].Preconditions = append([]runner.Condition{{Kind: runner.CondStepSucceeded, StepID: prev}}, ins[i].Preconditions...)
		prev = ins[i].ID
	}
	// The step after init-volume-dirs now waits for the job's removal.
	next := &op.Steps[at+1]
	for i, pc := range next.Preconditions {
		if pc.Kind == runner.CondStepSucceeded && pc.StepID == op.Steps[at].ID {
			next.Preconditions[i].StepID = prev
		}
	}
	op.Steps = slices.Concat(op.Steps[:at+1], ins, op.Steps[at+1:])
	op.Locks = append(op.Locks, "backup/"+m.BackupID)
	requireApproval(&op, req.ApprovalID)
	if err := op.Validate(); err != nil {
		return ClusterRestoreResult{}, err
	}
	return ClusterRestoreResult{Operation: op, Plan: plan}, nil
}

// delegate routes a physical restore job to the service host (a Swarm
// manager node: only managers accept service changes, lab E4 F12) when the
// target host is another node. The job is then pinned to the target host
// (RunRestoreArgs.NodeID) and the target volume checks, which only the
// target host's agent can make, are returned as a check-restore-target to
// run there first. Without a service host, or when it is the target host,
// the job stays on the target host and nothing is returned.
func delegate(serviceHost, targetHost string, a *agent.RunRestoreArgs) (string, *agent.CheckRestoreTargetArgs) {
	if serviceHost == "" || serviceHost == targetHost {
		return targetHost, nil
	}
	a.NodeID = targetHost
	return serviceHost, &agent.CheckRestoreTargetArgs{TargetVolume: a.TargetVolume, TargetVolumeRoot: a.TargetVolumeRoot, Expected: a.Expected}
}

// serviceTarget is the host that runs service commands: the service host
// if set, else the node's own host.
func serviceTarget(serviceHost, host string) string {
	if serviceHost != "" {
		return serviceHost
	}
	return host
}

// Inspection restores.

// InspectionPrefix starts the identity cluster ID of every inspection
// instance, so teardown can never be pointed at a production volume.
const InspectionPrefix = "inspect-"

// DefaultInspectionPort is the server port of inspection instances; it
// differs from production (3306) so a misrouted client fails loudly.
const DefaultInspectionPort = 3307

// MaxInspectionTTL bounds how long an inspection instance may live.
const MaxInspectionTTL = 7 * 24 * time.Hour

// InspectionAccount is the only account that can log in to an inspection
// instance.
const InspectionAccount = "dbcm_inspect"

// InspectionRequest opens a backup in a temporary isolated instance (B03).
type InspectionRequest struct {
	OpID       string
	SessionID  string // lowercase [a-z0-9-]
	ApprovalID string
	Source     RestoreSource
	// Host is where the instance runs: an explicitly approved host or the
	// result of SelectInspectionHost (B05).
	Host           galera.Host
	InstallationID string
	VolumeBase     string
	// Image is the MariaDB server image (the same major version as the
	// source); JobImage the job helper image.
	Image    string
	JobImage string
	// Network is a dedicated network for this session: not a production
	// network. phpMyAdmin or the manager attach to it for browsing.
	Network string
	Port    int
	// InitSecret holds the SQL from RenderInspectionInit (locks production
	// accounts, creates the inspection account).
	InitSecret  string
	Resources   galera.Resources // instance limits (required)
	Job         agent.JobResources
	TTL         time.Duration
	Now         time.Time
	TokenID     string
	NewUUID     func() (string, error)
	StepTimeout time.Duration
	// MaxRetries bounds precondition polls and idempotent retries per step
	// (lab E4: without it the inspection paused at its first gate poll,
	// before the restore job could report).
	MaxRetries int
	// ServiceHost is the host ID of the agent that creates and removes the
	// restore job and the instance service (a Swarm manager node, lab E4
	// F12); empty keeps them on Host. Volume steps always run on Host.
	ServiceHost string
	// PITR, if set, makes this a point-in-time restore: after the instance
	// is up, a replay job applies the archived binlog files to PITR.At.
	PITR *PITRRequest
}

// PITRRequest adds the binlog replay to an inspection restore.
type PITRRequest struct {
	// At is the target time (events up to and including its second).
	At time.Time
	// Archives are the closed-binlog archives of the backup
	// (ListBinlogArchives); the plan refuses a target they do not cover.
	Archives []Manifest
	// ReplaySecret holds the client option file of the replay account
	// (RenderReplayOptions). The plan removes it after the replay; teardown
	// removes it if it is left.
	ReplaySecret string
}

// ReplayAccount is the account the replay job connects with. Only a PITR
// session's init file creates it; the job drops it when the replay ends.
const ReplayAccount = "dbcm_replay"

// ReplaySecretName is the name of the replay option-file secret of a session.
func ReplaySecretName(session string) string { return "dbcm-inspect-" + session + "-replay" }

// ReplayJobName is the name of a session's replay job.
func ReplayJobName(backupID, session string) string { return JobName(backupID, "pitr-"+session) }

// RenderReplayOptions renders the replay job's client option file. The port
// is the inspection instance's (it differs from 3306).
func RenderReplayOptions(replayPassword string, port int) (string, error) {
	if !hexPasswordRe.MatchString(replayPassword) || port < 1024 || port > 65535 {
		return "", fmt.Errorf("%w: replay password must come from NewInspectionPassword", ErrInvalidRequest)
	}
	return "[client]\nuser=" + ReplayAccount + "\npassword=" + replayPassword + "\nport=" + strconv.Itoa(port) + "\n", nil
}

// InspectionSession is the durable record of an inspection instance. The
// manager stores it and runs TeardownPlan on completion or expiry, also
// after a manager restart.
type InspectionSession struct {
	ID          string `json:"id"`
	BackupID    string `json:"backup_id"`
	ClusterID   string `json:"cluster_id"` // identity cluster: InspectionPrefix + ID
	NodeUUID    string `json:"node_uuid"`
	HostID      string `json:"host_id"`
	Volume      string `json:"volume"`
	VolumeRoot  string `json:"volume_root"`
	ServiceName string `json:"service_name"`
	JobName     string `json:"job_name"`
	// ServiceID is filled in by the manager from the create-service result.
	ServiceID string    `json:"service_id,omitempty"`
	Network   string    `json:"network"`
	Port      int       `json:"port"`
	Created   time.Time `json:"created"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InspectionResult is a planned inspection.
type InspectionResult struct {
	Operation runner.Operation  `json:"operation"`
	Session   InspectionSession `json:"session"`
	Spec      agent.ServiceSpec `json:"spec"`
}

var sessionRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// InspectionServiceSpec is the single-node instance: wsrep off, no
// provider, no cluster address, a distinct cluster name and port, replica
// threads and events not started, binary log off, production accounts
// locked through the init file. It joins only the session network.
func InspectionServiceSpec(s InspectionSession, image, initSecret string, res galera.Resources) agent.ServiceSpec {
	return agent.ServiceSpec{
		Name: s.ServiceName, Image: image, Hostname: s.ServiceName,
		Labels: map[string]string{agent.LabelCluster: s.ClusterID, agent.LabelNode: s.NodeUUID, "dbcm.inspection": s.ID},
		Args: []string{
			"--datadir=" + galera.DataDir,
			"--socket=" + galera.SocketPath,
			"--wsrep-on=OFF",
			"--wsrep-provider=none",
			"--wsrep-cluster-address=",
			"--wsrep-cluster-name=" + s.ClusterID,
			"--port=" + strconv.Itoa(s.Port),
			"--skip-slave-start",
			"--event-scheduler=OFF",
			"--skip-log-bin",
			"--init-file=" + filepath.Join(agent.SecretsDir, initSecret),
		},
		Mounts:        []agent.Mount{{Type: "volume", Source: s.Volume, Target: galera.VolumeTarget}},
		Constraints:   []string{"node.id==" + s.HostID},
		Replicas:      1,
		RestartPolicy: &agent.RestartPolicy{Condition: "on-failure", MaxAttempts: 3},
		Networks:      []string{s.Network},
		Secrets:       []string{initSecret},
		Resources: &agent.Resources{
			Limits:       &agent.ResourceLimits{NanoCPUs: res.Limits.MilliCPU * 1e6, MemoryBytes: res.Limits.MemoryBytes},
			Reservations: &agent.ResourceReservations{NanoCPUs: res.Reservations.MilliCPU * 1e6, MemoryBytes: res.Reservations.MemoryBytes},
		},
	}
}

// InspectionRestorePlan plans: create-volume, init-identity (inspection
// identity), init-volume-dirs, run-restore, wait, remove-job, create-service (isolated
// instance), wait for verification. All command steps need the approval.
func InspectionRestorePlan(req InspectionRequest, inv Inventory) (InspectionResult, error) {
	if err := req.Source.check(); err != nil {
		return InspectionResult{}, err
	}
	if req.ApprovalID == "" {
		return InspectionResult{}, ErrApprovalRequired
	}
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(f, a...)) }
	switch {
	case req.OpID == "" || !sessionRe.MatchString(req.SessionID):
		return InspectionResult{}, bad("op ID and session ID ([a-z0-9-], at most 31) required")
	case req.Host.ID == "" || req.InstallationID == "" || !filepath.IsAbs(req.VolumeBase):
		return InspectionResult{}, bad("host, installation and absolute volume base required")
	case req.Image == "" || req.JobImage == "" || req.Network == "" || req.InitSecret == "" || req.TokenID == "":
		return InspectionResult{}, bad("image, job image, network, init secret and token required")
	case req.Resources.Limits.MilliCPU <= 0 || req.Resources.Limits.MemoryBytes <= 0:
		return InspectionResult{}, bad("instance CPU and memory limits required (B05)")
	case req.Job.NanoCPUs <= 0 || req.Job.MemoryBytes <= 0:
		return InspectionResult{}, bad("job limits required")
	case req.TTL <= 0 || req.TTL > MaxInspectionTTL:
		return InspectionResult{}, bad("TTL must be in (0, %s]", MaxInspectionTTL)
	case req.Now.IsZero():
		return InspectionResult{}, bad("clock required")
	}
	if err := req.Resources.Validate(); err != nil {
		return InspectionResult{}, err
	}
	port := req.Port
	if port == 0 {
		port = DefaultInspectionPort
	}
	if port == 3306 || port < 1024 || port > 65535 {
		return InspectionResult{}, bad("port %d (must differ from 3306 and be unprivileged)", port)
	}
	gen := req.NewUUID
	if gen == nil {
		gen = galera.NewRandomUUID
	}
	nodeUUID, err := gen()
	if err != nil {
		return InspectionResult{}, err
	}
	m := req.Source.Manifest
	var segments []string
	if req.PITR != nil {
		if req.PITR.ReplaySecret == "" {
			return InspectionResult{}, bad("replay secret required")
		}
		chain, err := ResolvePITR(m, req.PITR.Archives, req.PITR.At)
		if err != nil {
			return InspectionResult{}, err
		}
		for _, c := range chain {
			segments = append(segments, c.Object)
		}
	}
	sess := InspectionSession{
		ID: req.SessionID, BackupID: m.BackupID, ClusterID: InspectionPrefix + req.SessionID, NodeUUID: nodeUUID,
		HostID: req.Host.ID, Volume: "dbcm-inspect-" + req.SessionID + "-" + nodeUUID[:8],
		ServiceName: "dbcm-inspect-" + req.SessionID, JobName: JobName(m.BackupID, "inspect-"+req.SessionID),
		Network: req.Network, Port: port, Created: req.Now.UTC(), ExpiresAt: req.Now.UTC().Add(req.TTL),
	}
	sess.VolumeRoot = filepath.Join(req.VolumeBase, sess.Volume)
	if err := inv.overlap(sess.ClusterID, sess.ClusterID, sess.Network, []string{sess.Volume}, []string{nodeUUID}, []string{sess.ServiceName}); err != nil {
		return InspectionResult{}, err
	}
	expected := agent.Identity{InstallationID: req.InstallationID, ClusterID: sess.ClusterID, NodeUUID: nodeUUID}
	spec := InspectionServiceSpec(sess, req.Image, req.InitSecret, req.Resources)
	s := &steps{b: BackupRequest{StepTimeout: req.StepTimeout, MaxRetries: req.MaxRetries}}
	s.add(command("create-volume", sess.HostID, agent.KindCreateVolume, agent.CreateVolumeArgs{
		Volume: sess.Volume, VolumeRoot: sess.VolumeRoot,
		Labels: map[string]string{agent.LabelCluster: sess.ClusterID, agent.LabelNode: nodeUUID, "dbcm.inspection": sess.ID}}))
	s.add(command("init-identity", sess.HostID, agent.KindInitIdentity, agent.InitIdentityArgs{
		VolumeRoot: sess.VolumeRoot, Token: agent.NewIdentityToken{TokenID: req.TokenID, Identity: expected}}))
	s.add(command("init-volume-dirs", sess.HostID, agent.KindInitVolumeDirs, agent.InitVolumeDirsArgs{
		VolumeRoot: sess.VolumeRoot, Expected: expected, UID: galera.DefaultDBUID, GID: galera.DefaultDBGID}))
	ra := agent.RunRestoreArgs{
		JobName: sess.JobName, Image: req.JobImage, Mode: agent.RestorePhysical, BackupID: m.BackupID, Object: m.Object,
		ApprovalID: req.ApprovalID, SourceClusterID: req.Source.ClusterID,
		TargetVolume: sess.Volume, TargetVolumeRoot: sess.VolumeRoot, Expected: expected,
		BackendSecret: req.Source.BackendSecret, KeySecret: req.Source.KeySecret, Resources: req.Job,
	}
	jobHost, local := delegate(req.ServiceHost, sess.HostID, &ra)
	if local != nil {
		s.add(command("check-restore-target", sess.HostID, agent.KindCheckRestoreTarget, *local))
	}
	s.add(command("restore", jobHost, agent.KindRunRestore, ra))
	s.add(gate("wait-restore", runner.Condition{Kind: CondRestoreComplete, Target: sess.JobName}))
	s.add(command("remove-job", jobHost, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: sess.JobName}))
	s.add(command("create-instance", serviceTarget(req.ServiceHost, sess.HostID), agent.KindCreateService, spec))
	if segments != nil {
		svc := serviceTarget(req.ServiceHost, sess.HostID)
		replayJob := ReplayJobName(m.BackupID, sess.ID)
		rp := agent.RunRestoreArgs{
			JobName: replayJob, Image: req.JobImage, Mode: agent.RestoreBinlog, BackupID: m.BackupID, Object: m.Object,
			ApprovalID: req.ApprovalID, SourceClusterID: req.Source.ClusterID,
			Segments: segments, StartOffset: m.BinlogBase.Offset, StopDatetime: ReplayStopDatetime(req.PITR.At),
			DBHost: sess.ServiceName, Network: sess.Network, DBCredentialSecret: req.PITR.ReplaySecret,
			BackendSecret: req.Source.BackendSecret, KeySecret: req.Source.KeySecret, Resources: req.Job,
		}
		s.add(command("replay", svc, agent.KindRunRestore, rp))
		s.add(gate("wait-replay", runner.Condition{Kind: CondRestoreComplete, Target: replayJob}))
		s.add(command("remove-replay-job", svc, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: replayJob}))
		rs := command("remove-replay-secret", svc, agent.KindRemoveSecret, agent.RemoveSecretArgs{Secret: req.PITR.ReplaySecret})
		rs.RetryCodes = []agent.Code{agent.CodeSecretInUse}
		s.add(rs)
	}
	// The manager evaluates CondRestoreVerified by probing the instance
	// through the agent on its host and records a VerifyReport.
	s.add(gate("verify", runner.Condition{Kind: CondRestoreVerified, Target: sess.ID}))
	op := runner.Operation{ID: req.OpID, Kind: OpInspection, Locks: []string{"inspection/" + sess.ID, "backup/" + m.BackupID}, Steps: s.list}
	requireApproval(&op, req.ApprovalID)
	if err := op.Validate(); err != nil {
		return InspectionResult{}, err
	}
	return InspectionResult{Operation: op, Session: sess, Spec: spec}, nil
}

// TeardownPlan removes an inspection instance (TeardownPlanFor without a
// service host or init secret).
func TeardownPlan(opID, installationID string, s InspectionSession, inv Inventory) (runner.Operation, error) {
	return TeardownPlanFor(TeardownRequest{OpID: opID, InstallationID: installationID, Session: s}, inv)
}

// TeardownRequest describes the teardown of one inspection session.
type TeardownRequest struct {
	OpID           string
	InstallationID string
	Session        InspectionSession
	// ServiceHost runs the service and secret commands (a Swarm manager
	// node); empty keeps them on the session's host.
	ServiceHost string
	// InitSecret is the session's init secret (created by the manager for
	// this session only); empty skips its removal.
	InitSecret string
	// ReplaySecret is the session's replay option-file secret (point-in-time
	// sessions); empty for plain inspections.
	ReplaySecret string
	StepTimeout  time.Duration
	MaxRetries   int
}

// InitSecretName is the name of the init secret the manager creates for an
// inspection session; it is the only secret a teardown removes.
func InitSecretName(session string) string { return "dbcm-inspect-" + session + "-init" }

// TeardownPlanFor removes an inspection instance: the restore job (if left),
// the instance service (if created), the session's init secret and the
// volume after the agent has verified its inspection identity (a volume
// that was never created counts as removed). It refuses sessions whose
// identity is not an inspection identity, that overlap production, and a
// secret that is not the session's init secret. It needs no approval, so
// expiry cleanup can run unattended (B03).
func TeardownPlanFor(req TeardownRequest, inv Inventory) (runner.Operation, error) {
	s := req.Session
	if req.OpID == "" || req.InstallationID == "" || !sessionRe.MatchString(s.ID) || s.HostID == "" {
		return runner.Operation{}, fmt.Errorf("%w: op, installation, session and host required", ErrInvalidRequest)
	}
	if s.ClusterID != InspectionPrefix+s.ID || !strings.HasPrefix(s.Volume, "dbcm-inspect-"+s.ID+"-") {
		return runner.Operation{}, fmt.Errorf("%w: session %s does not describe an inspection instance", ErrProductionTarget, s.ID)
	}
	if req.InitSecret != "" && req.InitSecret != InitSecretName(s.ID) {
		return runner.Operation{}, fmt.Errorf("%w: secret %s is not the init secret of session %s", ErrProductionTarget, req.InitSecret, s.ID)
	}
	if err := inv.overlap(s.ClusterID, "", s.Network, []string{s.Volume}, []string{s.NodeUUID}, nil); err != nil {
		return runner.Operation{}, err
	}
	if req.ReplaySecret != "" && req.ReplaySecret != ReplaySecretName(s.ID) {
		return runner.Operation{}, fmt.Errorf("%w: secret %s is not the replay secret of session %s", ErrProductionTarget, req.ReplaySecret, s.ID)
	}
	svc := serviceTarget(req.ServiceHost, s.HostID)
	st := &steps{b: BackupRequest{StepTimeout: req.StepTimeout, MaxRetries: req.MaxRetries}}
	st.add(command("remove-job", svc, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: s.JobName}))
	if req.ReplaySecret != "" {
		st.add(command("remove-replay-job", svc, agent.KindRemoveJob, agent.RemoveJobArgs{JobName: ReplayJobName(s.BackupID, s.ID)}))
	}
	if s.ServiceID != "" {
		st.add(command("remove-instance", svc, agent.KindRemoveService, agent.ServiceArgs{ServiceID: s.ServiceID}))
	}
	if req.InitSecret != "" {
		// After the instance: Docker refuses to remove a secret in use.
		rs := command("remove-init-secret", svc, agent.KindRemoveSecret, agent.RemoveSecretArgs{Secret: req.InitSecret})
		rs.RetryCodes = []agent.Code{agent.CodeSecretInUse}
		st.add(rs)
	}
	if req.ReplaySecret != "" {
		rs := command("remove-replay-secret", svc, agent.KindRemoveSecret, agent.RemoveSecretArgs{Secret: req.ReplaySecret})
		rs.RetryCodes = []agent.Code{agent.CodeSecretInUse}
		st.add(rs)
	}
	rv := command("remove-volume", s.HostID, agent.KindRemoveVolume, agent.RemoveVolumeArgs{
		VolumeID: s.Volume, VolumeRoot: s.VolumeRoot, MissingOK: true,
		Expected: agent.Identity{InstallationID: req.InstallationID, ClusterID: s.ClusterID, NodeUUID: s.NodeUUID}})
	// The instance's task may hold the volume until it has stopped.
	rv.RetryCodes = []agent.Code{agent.CodeVolumeInUse}
	st.add(rv)
	op := runner.Operation{ID: req.OpID, Kind: OpTeardown, Locks: []string{"inspection/" + s.ID}, Steps: st.list}
	return op, op.Validate()
}

// ExpiredSessions returns the sessions whose expiry has passed.
func ExpiredSessions(sessions []InspectionSession, now time.Time) []InspectionSession {
	var out []InspectionSession
	for _, s := range sessions {
		if !now.Before(s.ExpiresAt) {
			out = append(out, s)
		}
	}
	return out
}

// RestoreHost is a candidate host for a restore (B05).
type RestoreHost struct {
	Host          galera.Host
	FreeDiskBytes int64
	// Approved marks hosts the user explicitly approved for restores.
	Approved bool
	// Production marks hosts running production database nodes.
	Production bool
}

// SelectInspectionHost picks a host: in approved mode only approved hosts,
// otherwise any ready, active, non-spare host. Hosts must fit the instance
// and hold the restored data with 25 % headroom; hosts without production
// nodes are preferred, then the most free disk.
func SelectInspectionHost(hosts []RestoreHost, approvedOnly bool, demand galera.ResourceAmount, dataBytes int64) (galera.Host, error) {
	need := dataBytes + dataBytes/4
	var best *RestoreHost
	for i := range hosts {
		h := &hosts[i]
		free := galera.ResourceAmount{MilliCPU: h.Host.Capacity.MilliCPU - h.Host.Allocated.MilliCPU,
			MemoryBytes: h.Host.Capacity.MemoryBytes - h.Host.Allocated.MemoryBytes}
		switch {
		case approvedOnly && !h.Approved, !h.Host.Ready, !h.Host.Active, h.Host.Spare:
			continue
		case free.MilliCPU < demand.MilliCPU || free.MemoryBytes < demand.MemoryBytes, h.FreeDiskBytes < need:
			continue
		}
		if best == nil || (best.Production && !h.Production) ||
			(best.Production == h.Production && h.FreeDiskBytes > best.FreeDiskBytes) {
			best = h
		}
	}
	if best == nil {
		return galera.Host{}, fmt.Errorf("%w: no host fits %d mCPU, %d B memory and %d B disk", ErrInvalidRequest, demand.MilliCPU, demand.MemoryBytes, need)
	}
	return best.Host, nil
}

// NewInspectionPassword returns a random password for the inspection
// account (hex, so it needs no quoting).
func NewInspectionPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

var hexPasswordRe = regexp.MustCompile(`^[0-9a-f]{32,128}$`)

// RenderInspectionInit renders the --init-file SQL of an inspection
// instance. It runs with full privileges at every start, before clients
// connect: it locks every restored account (production credentials stay in
// the data but cannot log in), creates the read-only inspection account and
// clears replication configuration. Requires MariaDB 10.4+ (global_priv).
func RenderInspectionInit(password string) (string, error) {
	return RenderInspectionInitKeeping(password)
}

// LocalAccount names a restored account that stays usable in an
// inspection instance.
type LocalAccount struct {
	User string
	Host string
}

var (
	accountUserRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
	// Only local accounts may stay unlocked: they can log in through the
	// instance's unix socket on its volume, not over the network.
	localHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
)

// RenderInspectionInitKeeping is RenderInspectionInit, but leaves the
// given restored local accounts unlocked. The manager keeps the monitoring
// account (USAGE only, localhost) so the agent on the instance's host can
// verify that the instance answers (restore verification) with the
// credentials it already has; no password is rendered for it.
func RenderInspectionInitKeeping(password string, keep ...LocalAccount) (string, error) {
	return renderInit(password, "", keep)
}

// RenderInspectionInitReplay is RenderInspectionInitKeeping for a
// point-in-time session: it also creates ReplayAccount with full privileges,
// which the replay job needs to apply the binlog files. The account is
// reachable only on the session's isolated network; its password is known to
// the replay job's option-file secret alone, and the job drops the account
// when the replay ends.
func RenderInspectionInitReplay(password, replayPassword string, keep ...LocalAccount) (string, error) {
	if !hexPasswordRe.MatchString(replayPassword) {
		return "", fmt.Errorf("%w: replay password must come from NewInspectionPassword", ErrInvalidRequest)
	}
	return renderInit(password, replayPassword, keep)
}

func renderInit(password, replayPassword string, keep []LocalAccount) (string, error) {
	if !hexPasswordRe.MatchString(password) {
		return "", fmt.Errorf("%w: inspection password must come from NewInspectionPassword", ErrInvalidRequest)
	}
	exempt := "(User = '" + InspectionAccount + "' AND Host = '%')"
	if replayPassword != "" {
		exempt += " OR (User = '" + ReplayAccount + "' AND Host = '%')"
	}
	for _, k := range keep {
		if !accountUserRe.MatchString(k.User) || !localHosts[k.Host] || k.User == InspectionAccount {
			return "", fmt.Errorf("%w: account %q@%q cannot stay unlocked (local accounts only)", ErrInvalidRequest, k.User, k.Host)
		}
		exempt += " OR (User = '" + k.User + "' AND Host = '" + k.Host + "')"
	}
	acct := "'" + InspectionAccount + "'@'%'"
	var replay []string
	if replayPassword != "" {
		racct := "'" + ReplayAccount + "'@'%'"
		replay = []string{
			"CREATE USER IF NOT EXISTS " + racct + " IDENTIFIED BY '" + replayPassword + "';",
			"ALTER USER " + racct + " IDENTIFIED BY '" + replayPassword + "' ACCOUNT UNLOCK;",
			"GRANT ALL PRIVILEGES ON *.* TO " + racct + ";",
		}
	}
	return strings.Join(append([]string{
		"SET SESSION sql_log_bin=0;",
		"STOP ALL SLAVES;",
		"RESET SLAVE ALL;",
		"UPDATE mysql.global_priv SET Priv = JSON_SET(Priv, '$.account_locked', true) WHERE NOT (" + exempt + ");",
		"FLUSH PRIVILEGES;",
		"CREATE USER IF NOT EXISTS " + acct + " IDENTIFIED BY '" + password + "';",
		"ALTER USER " + acct + " IDENTIFIED BY '" + password + "' ACCOUNT UNLOCK;",
		"GRANT SELECT, SHOW VIEW, SHOW DATABASES, LOCK TABLES, EVENT, TRIGGER ON *.* TO " + acct + ";",
	}, append(replay, "")...), "\n"), nil
}

// ReplayStopDatetime is the --stop-datetime of the replay for target time at:
// mariadb-binlog stops at the first event at or after it, so the second after
// at's second includes every event of at's second. The job container runs in
// UTC.
func ReplayStopDatetime(at time.Time) string {
	return at.UTC().Truncate(time.Second).Add(time.Second).Format("2006-01-02 15:04:05")
}
