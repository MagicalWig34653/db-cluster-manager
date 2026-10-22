// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidTopology reports an unusable cluster topology.
var ErrInvalidTopology = errors.New("galera: invalid topology")

// MinClusterSize is the smallest cluster PlanCreate accepts. One- and
// two-node clusters cannot keep quorum after a single node loss.
const MinClusterSize = 3

// PlanKind names a plan.
type PlanKind string

// Plan kinds.
const (
	PlanCreateCluster      PlanKind = "create-cluster"
	PlanReplaceNode        PlanKind = "replace-node"
	PlanFullOutageRecovery PlanKind = "full-outage-recovery"
)

// StepKind names one operation step. The steps map onto agent commands
// (create-service, init-identity, start-db, ...) but this package does not
// execute them.
type StepKind string

// Step kinds.
const (
	StepCreateVolume   StepKind = "create-volume"   // local named volume on the host
	StepInitIdentity   StepKind = "init-identity"   // write the new identity marker (agent KindInitIdentity)
	StepCreateService  StepKind = "create-service"  // pinned service, replicas 1
	StepUpdateService  StepKind = "update-service"  // replace the spec (e.g. drop the bootstrap flag)
	StepWaitSynced     StepKind = "wait-synced"     // node Synced and Primary at the expected cluster size
	StepVerifyDonor    StepKind = "verify-donor"    // re-check the donor is Synced right before the join
	StepRemoveService  StepKind = "remove-service"  // remove the failed node's service
	StepRetainVolume   StepKind = "retain-volume"   // keep the old volume for inspection; never deleted automatically
	StepStopAll        StepKind = "stop-all"        // ensure no database task of the cluster runs
	StepVerifyIdentity StepKind = "verify-identity" // agent verifies the marker before a start
	StepMarkSafe       StepKind = "mark-safe-to-bootstrap"
	// StepInitVolumeDirs creates data/ and run/ on the new volume, owned by
	// the database user, after the agent verified the new marker (agent
	// KindInitVolumeDirs).
	StepInitVolumeDirs StepKind = "init-volume-dirs"
	// StepExecSQL runs one allow-listed SQL template on the node through
	// its unix socket (agent KindExecSQL).
	StepExecSQL StepKind = "exec-sql"
)

// DirOwner is the numeric owner of the database directories.
type DirOwner struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// SQLTemplate names an allow-listed statement template (rendered by
// internal/dbadmin, executed by the agent).
type SQLTemplate string

// SQL templates.
const (
	SQLCreateMonitorUser SQLTemplate = "create-monitor-user"
	SQLCreateBackupUser  SQLTemplate = "create-backup-user"
	SQLSetRootPassword   SQLTemplate = "set-root-password"
)

// SQLStep is the structured input of an exec-sql step. Passwords are named
// by Swarm secret, never included.
type SQLStep struct {
	Template       SQLTemplate `json:"template"`
	User           string      `json:"user,omitempty"`
	Host           string      `json:"host,omitempty"`
	PasswordSecret string      `json:"password_secret"`
	RequireSSL     bool        `json:"require_ssl,omitempty"`
	Logical        bool        `json:"logical,omitempty"`
}

// PreconditionKind names a condition that must hold before a step starts.
type PreconditionKind string

// Precondition kinds.
const (
	PreStepSucceeded PreconditionKind = "step-succeeded" // StepID completed successfully
	// PreNodeSynced: Slot reports Synced, Primary, ready and connected with
	// wsrep_cluster_size == ClusterSize.
	PreNodeSynced PreconditionKind = "node-synced"
	// PreQuorum: at least ClusterSize nodes are members of the Primary
	// component (a strict majority of the last known cluster size).
	PreQuorum PreconditionKind = "quorum"
	// PreApproved: a user approved the plan explicitly.
	PreApproved PreconditionKind = "approved"
	// PreNoNodeRunning: no database task of the cluster runs.
	PreNoNodeRunning PreconditionKind = "no-node-running"
)

// Precondition is one condition of a step.
type Precondition struct {
	Kind        PreconditionKind `json:"kind"`
	StepID      string           `json:"step_id,omitempty"`
	Slot        string           `json:"slot,omitempty"`
	ClusterSize int              `json:"cluster_size,omitempty"`
}

// Step is one planned operation.
type Step struct {
	ID            string         `json:"id"`
	Kind          StepKind       `json:"kind"`
	Slot          string         `json:"slot,omitempty"`
	HostID        string         `json:"host_id,omitempty"`
	NodeUUID      string         `json:"node_uuid,omitempty"`
	Volume        string         `json:"volume,omitempty"`
	ClusterSize   int            `json:"cluster_size,omitempty"` // wait-synced: expected size
	Spec          *ServiceSpec   `json:"spec,omitempty"`
	Owner         *DirOwner      `json:"owner,omitempty"` // init-volume-dirs
	SQL           *SQLStep       `json:"sql,omitempty"`   // exec-sql
	Preconditions []Precondition `json:"preconditions,omitempty"`
	Note          string         `json:"note,omitempty"`
}

// Plan is an ordered list of steps. Steps run strictly in order; each step's
// preconditions must hold when it starts.
type Plan struct {
	Kind      PlanKind `json:"kind"`
	ClusterID string   `json:"cluster_id"`
	Cluster   Cluster  `json:"cluster"`
	// Nodes is the resulting node set; Desired the matching steady-state
	// specs (never with the bootstrap flag).
	Nodes            []Node        `json:"nodes"`
	Desired          []ServiceSpec `json:"desired"`
	Steps            []Step        `json:"steps"`
	Warnings         []string      `json:"warnings,omitempty"`
	RequiresApproval bool          `json:"requires_approval"`
}

// Topology describes a new cluster.
type Topology struct {
	// Cluster supplies IDs, name, image, network and secrets. Members is
	// ignored and derived as n1..nSize.
	Cluster   Cluster
	Size      int
	Resources Resources
	// NewUUID generates node UUIDs; nil uses random version-4 UUIDs.
	NewUUID func() (string, error)
}

// NewRandomUUID returns a random version-4 UUID.
func NewRandomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// VolumeName returns the volume name of a node identity. It includes the
// UUID prefix so that a replacement never reuses an old volume name.
func VolumeName(clusterName, slot, nodeUUID string) string {
	return ServiceName(clusterName, slot) + "-" + nodeUUID[:8]
}

// uuidSource wraps a generator and refuses invalid or repeated UUIDs.
type uuidSource struct {
	gen  func() (string, error)
	seen map[string]bool
}

func newUUIDSource(gen func() (string, error), existing ...string) *uuidSource {
	if gen == nil {
		gen = NewRandomUUID
	}
	s := &uuidSource{gen: gen, seen: map[string]bool{}}
	for _, u := range existing {
		s.seen[u] = true
	}
	return s
}

func (s *uuidSource) next() (string, error) {
	u, err := s.gen()
	if err != nil {
		return "", fmt.Errorf("galera: generate node UUID: %w", err)
	}
	if !uuidRe.MatchString(u) {
		return "", fmt.Errorf("galera: generated node UUID %q is invalid", u)
	}
	if s.seen[u] {
		return "", fmt.Errorf("galera: generated node UUID %s is not new", u)
	}
	s.seen[u] = true
	return u, nil
}

// stepList appends steps, linking each to its predecessor.
type stepList struct{ steps []Step }

func (l *stepList) add(s Step, pre ...Precondition) string {
	if n := len(l.steps); n > 0 {
		s.Preconditions = append([]Precondition{{Kind: PreStepSucceeded, StepID: l.steps[n-1].ID}}, s.Preconditions...)
	}
	s.Preconditions = append(s.Preconditions, pre...)
	if s.ID == "" {
		s.ID = fmt.Sprintf("%02d-%s", len(l.steps)+1, s.Kind)
		if s.Slot != "" {
			s.ID += "-" + s.Slot
		}
	}
	l.steps = append(l.steps, s)
	return s.ID
}

func synced(slot string, size int) Precondition {
	return Precondition{Kind: PreNodeSynced, Slot: slot, ClusterSize: size}
}

func specPtr(s ServiceSpec) *ServiceSpec { return &s }

func ownerOf(c Cluster) *DirOwner {
	uid, gid := c.DBOwner()
	return &DirOwner{UID: uid, GID: gid}
}

// addNewVolume appends the steps that prepare a new node volume: create it,
// write the new identity marker, then create the database directories.
func (l *stepList) addNewVolume(c Cluster, n Node) {
	l.add(Step{Kind: StepCreateVolume, Slot: n.Slot, HostID: n.HostID, Volume: n.Volume})
	l.add(Step{Kind: StepInitIdentity, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID, Volume: n.Volume})
	l.add(Step{Kind: StepInitVolumeDirs, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID, Volume: n.Volume, Owner: ownerOf(c),
		Note: "data/ and run/ owned by the database user; the volume root and the marker stay root-owned"})
}

// accountSteps returns the exec-sql steps for the configured accounts.
func accountSteps(c Cluster, n Node) []Step {
	a := c.Accounts.withDefaults()
	var out []Step
	if c.Accounts.MonitorSecret != "" {
		out = append(out, Step{Kind: StepExecSQL, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID, Volume: n.Volume,
			SQL:  &SQLStep{Template: SQLCreateMonitorUser, User: a.MonitorUser, Host: a.MonitorHost, PasswordSecret: c.Accounts.MonitorSecret, RequireSSL: a.MonitorRequireSSL},
			Note: "least-privilege monitoring account (USAGE only); replicated to joiners"})
	}
	if c.Accounts.BackupSecret != "" {
		out = append(out, Step{Kind: StepExecSQL, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID, Volume: n.Volume,
			SQL: &SQLStep{Template: SQLCreateBackupUser, User: a.BackupUser, Host: a.BackupHost, PasswordSecret: c.Accounts.BackupSecret,
				RequireSSL: a.BackupRequireSSL, Logical: a.BackupLogical},
			Note: "backup account with the mariabackup privileges"})
	}
	return out
}

// PlanCreate plans a new cluster: place every node, then bootstrap the first
// node, add joiners one at a time (each only after the previous node reports
// Synced at the expected size), and finally replace the bootstrap node's
// spec with its steady-state spec so a restart can never bootstrap a second
// cluster. It only plans; nothing is executed.
func PlanCreate(topo Topology, hosts []Host, policy PlacementPolicy) (Plan, error) {
	if topo.Size < MinClusterSize {
		return Plan{}, fmt.Errorf("%w: size %d below minimum %d", ErrInvalidTopology, topo.Size, MinClusterSize)
	}
	if err := topo.Resources.Validate(); err != nil {
		return Plan{}, err
	}
	demand := topo.Resources.Demand()
	if demand.MilliCPU <= 0 || demand.MemoryBytes <= 0 {
		return Plan{}, fmt.Errorf("%w: CPU and memory limits are required for capacity planning (A04)", ErrInvalidTopology)
	}
	c := topo.Cluster
	c.Members = make([]string, topo.Size)
	for i := range c.Members {
		c.Members[i] = fmt.Sprintf("n%d", i+1)
	}
	c.TLS.Args, c.TLS.Secrets = slices.Clone(c.TLS.Args), slices.Clone(c.TLS.Secrets)
	if err := c.validate(); err != nil {
		return Plan{}, err
	}
	warns, err := policy.validate(hosts)
	if err != nil {
		return Plan{}, err
	}
	if topo.Size%2 == 0 {
		warns = append(warns, fmt.Sprintf("even cluster size %d: a symmetric split leaves no majority", topo.Size))
	}

	ids := newUUIDSource(topo.NewUUID)
	pl := newPlacer(hosts, policy, forCreate)
	nodes := make([]Node, topo.Size)
	for i, slot := range c.Members {
		h, err := pl.choose(slot, demand)
		if err != nil {
			return Plan{}, err
		}
		u, err := ids.next()
		if err != nil {
			return Plan{}, err
		}
		nodes[i] = Node{Slot: slot, NodeUUID: u, HostID: h.ID, Volume: VolumeName(c.Name, slot, u), Resources: topo.Resources}
	}

	desired := make([]ServiceSpec, len(nodes))
	var l stepList
	for i, n := range nodes {
		spec, err := BuildServiceSpec(c, n, SpecOptions{Bootstrap: i == 0})
		if err != nil {
			return Plan{}, err
		}
		desired[i] = spec.WithoutBootstrap()
		var joinPre []Precondition
		if i > 0 {
			joinPre = []Precondition{synced(nodes[i-1].Slot, i)}
		}
		l.addNewVolume(c, n)
		note := "joiner; state transfer from the running cluster"
		if i == 0 {
			note = "bootstrap node: forms the new cluster"
		}
		l.add(Step{Kind: StepCreateService, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID, Spec: specPtr(spec), Note: note}, joinPre...)
		if i == 0 {
			// The accounts come before the bootstrap node's wait-synced: that
			// check reads wsrep status through the monitoring account, so it
			// could never pass before the account exists (found in lab E4
			// preparation). exec-sql is idempotent and retried until the
			// server accepts the root login over the socket.
			for _, s := range accountSteps(c, n) {
				l.add(s)
			}
		}
		l.add(Step{Kind: StepWaitSynced, Slot: n.Slot, HostID: n.HostID, ClusterSize: i + 1})
	}
	if c.Accounts.MonitorSecret == "" {
		warns = append(warns, "no monitoring account configured: health collection through the node socket will fail")
	}
	all := make([]Precondition, len(nodes))
	for i, n := range nodes {
		all[i] = synced(n.Slot, len(nodes))
	}
	boot := nodes[0]
	l.add(Step{Kind: StepUpdateService, Slot: boot.Slot, HostID: boot.HostID, NodeUUID: boot.NodeUUID, Spec: specPtr(desired[0]),
		Note: "remove " + BootstrapFlag + "; the node restarts and rejoins by IST"}, all...)
	l.add(Step{Kind: StepWaitSynced, Slot: boot.Slot, HostID: boot.HostID, ClusterSize: len(nodes)})

	return Plan{
		Kind: PlanCreateCluster, ClusterID: c.ClusterID, Cluster: c,
		Nodes: nodes, Desired: desired, Steps: l.steps, Warnings: warns,
	}, nil
}
