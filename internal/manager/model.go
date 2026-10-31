// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Collections of the replicated store. Keys are given per collection.
const (
	CollSwarms     = "swarms"     // key: swarm ID
	CollHosts      = "hosts"      // key: HostKey(swarm, node ID)
	CollClusters   = "clusters"   // key: cluster ID
	CollSettings   = "settings"   // key: SettingsKey
	CollOperations = "operations" // key: operation ID
	CollEvents     = "events"     // key: time-ordered event ID
	CollDashboards = "dashboards" // key: dashboard ID

	CollAccounts         = "accounts"          // key: account ID
	CollSessions         = "sessions"          // key: token digest (sealed)
	CollRecoverySessions = "recovery-sessions" // key: token digest (sealed)
	CollRecoveryCodes    = "recovery-codes"    // key: account ID (sealed)
	CollPasskeys         = "passkeys"          // key: credential ID (sealed)
	CollPolicy           = "policy"            // key: PolicyKey
	CollAudit            = "audit"             // key: time-ordered record ID

	CollPKI          = "pki"           // key: PKIKeyAgentCA (sealed)
	CollEnrollTokens = "enroll-tokens" // key: token ID (digest only)
	CollRevocations  = "revocations"   // key: certificate serial (hex)
)

// Singleton keys.
const (
	SettingsKey = "global"
	PolicyKey   = "current"
)

// Observation is an observed state with its time. A missing or stale
// observation is reported as unknown.
type Observation struct {
	State  api.State `json:"state"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

// SwarmRecord is one connected Docker Swarm (P06, multi-Swarm).
type SwarmRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// DockerHost is the Docker API endpoint ("unix:///var/run/docker.sock"
	// or "tcp://host:port"). Secure remote connection settings (P04) are
	// not designed yet; tcp without TLS is lab-only.
	DockerHost string `json:"dockerHost"`
	// SwarmClusterID is the Docker Swarm cluster ID once observed.
	SwarmClusterID string       `json:"swarmClusterId,omitempty"`
	CreatedAt      time.Time    `json:"createdAt"`
	Observed       *Observation `json:"observed,omitempty"`
	// Compatibility holds the latest Docker compatibility findings (A09).
	Compatibility []CompatibilityNote `json:"compatibility,omitempty"`
}

// CompatibilityNote is one stored compatibility finding.
type CompatibilityNote struct {
	NodeID  string `json:"nodeId"`
	Version string `json:"version"`
	Level   string `json:"level"` // "ok", "warning", "error"
	Message string `json:"message"`
}

// Enrollment is the agent enrollment state of a host.
type Enrollment string

// Enrollment states.
const (
	// EnrollDiscovered: seen in the Swarm, not managed.
	EnrollDiscovered Enrollment = "discovered"
	// EnrollEnrolled: the user enrolled the host; the agent service may run
	// there (its node carries the agent label).
	EnrollEnrolled Enrollment = "enrolled"
	// EnrollRevoked: no longer managed; the agent label is removed.
	EnrollRevoked Enrollment = "revoked"
)

// HostRecord is one Swarm node as inventoried by the manager.
type HostRecord struct {
	Swarm         string            `json:"swarm"`
	NodeID        string            `json:"nodeId"`
	Hostname      string            `json:"hostname,omitempty"`
	Role          string            `json:"role,omitempty"`
	Availability  string            `json:"availability,omitempty"`
	State         string            `json:"state,omitempty"`
	EngineVersion string            `json:"engineVersion,omitempty"`
	Arch          string            `json:"arch,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	NanoCPUs      int64             `json:"nanoCpus,omitempty"`
	MemoryBytes   int64             `json:"memoryBytes,omitempty"`
	ObservedAt    time.Time         `json:"observedAt,omitzero"`

	Enrollment Enrollment `json:"enrollment"`
	EnrolledAt time.Time  `json:"enrolledAt,omitzero"`
	Spare      bool       `json:"spare,omitempty"`
	// Addr is the node's Swarm address (an IP); the mTLS agent listens on
	// it in the host network namespace.
	Addr string `json:"addr,omitempty"`
	// AgentURL is the lab HTTP endpoint of the host's agent (no TLS, no
	// authentication); used only by LabAgentDirectory.
	AgentURL string `json:"agentUrl,omitempty"`
	// AgentSerial is the serial (hex) of the agent certificate issued at
	// enrollment or the last renewal: the host's agent identity. Empty
	// means the agent has not enrolled (or its identity was revoked).
	AgentSerial     string    `json:"agentSerial,omitempty"`
	AgentNotAfter   time.Time `json:"agentNotAfter,omitzero"`
	AgentIdentityAt time.Time `json:"agentIdentityAt,omitzero"`
	// AgentIdentities holds independent certificate slots. The legacy fields
	// above remain the container slot for records written by older managers.
	AgentIdentities map[agentrt.Runtime]AgentIdentityRecord `json:"agentIdentities,omitempty"`
	// AgentVersion and AgentSeenAt record the last HTTP answer received
	// from the host's agent (build version from its response header).
	// Written best-effort by AgentDirectory, at most about once a minute.
	AgentVersion string    `json:"agentVersion,omitempty"`
	AgentSeenAt  time.Time `json:"agentSeenAt,omitzero"`
	// AgentReports are the validated runtime reports of the host's agents
	// (ADR 0024), keyed by runtime ("container", "native"). Written from
	// enrollment and from the headers of every agent answer.
	AgentReports map[agentrt.Runtime]agentrt.Info `json:"agentReports,omitempty"`
	// AgentReportError is set when an agent sent an inconsistent report;
	// the host is then refused for routing until it reports correctly.
	AgentReportError string `json:"agentReportError,omitempty"`
	// AgentServiceID is the agent global service that serves this host.
	AgentServiceID string `json:"agentServiceId,omitempty"`
	// DenyListDigest is the deny list the host's agent last confirmed
	// (denylist.go).
	DenyListDigest  string                     `json:"denyListDigest,omitempty"`
	DenyListDigests map[agentrt.Runtime]string `json:"denyListDigests,omitempty"`
	// Maintenance is set while the host is in guided maintenance
	// (maintenance.go); replacement on it is paused.
	Maintenance *HostMaintenanceRecord `json:"maintenance,omitempty"`
}

type AgentIdentityRecord struct {
	Serial   string    `json:"serial"`
	NotAfter time.Time `json:"notAfter"`
	IssuedAt time.Time `json:"issuedAt"`
}

func (h HostRecord) identity(rt agentrt.Runtime) AgentIdentityRecord {
	if id := h.AgentIdentities[rt]; id.Serial != "" {
		return id
	}
	if rt == agentrt.Container {
		return AgentIdentityRecord{Serial: h.AgentSerial, NotAfter: h.AgentNotAfter, IssuedAt: h.AgentIdentityAt}
	}
	return AgentIdentityRecord{}
}

// HostKey is the key of a host record.
func HostKey(swarm, nodeID string) string { return swarm + "/" + nodeID }

// NodeObservation is the last health observation of a database node.
type NodeObservation struct {
	Health     galera.NodeHealth `json:"health"`
	ObservedAt time.Time         `json:"observedAt"`
}

// ClusterRecord is the desired state of one database cluster plus its last
// observations.
type ClusterRecord struct {
	ID        string                 `json:"id"`
	Swarm     string                 `json:"swarm"`
	Name      string                 `json:"name"`
	Engine    string                 `json:"engine"` // "mariadb-galera"
	Spec      galera.Cluster         `json:"spec"`
	Size      int                    `json:"size"`
	Resources galera.Resources       `json:"resources"`
	Placement galera.PlacementPolicy `json:"placement"`
	Nodes     []galera.Node          `json:"nodes"`
	ServiceID map[string]string      `json:"serviceIds,omitempty"` // slot -> Swarm service ID once known
	// MonitorPorts maps a slot to a database port published on its host;
	// health is then collected over 127.0.0.1:<port> instead of the socket
	// in the node's volume (Settings.MonitorSocket).
	MonitorPorts map[string]int             `json:"monitorPorts,omitempty"`
	Health       map[string]NodeObservation `json:"health,omitempty"`
	// LastKnownSize is the size of the last Primary component recorded.
	LastKnownSize int `json:"lastKnownSize"`
	// AutoReplace enables automatic replacement (A02); the quorum rules of
	// galera.PlanReplacement apply either way.
	AutoReplace bool `json:"autoReplace,omitempty"`
	// AutoRecover lets the guided Galera recovery run without an approval
	// when every node position is readable and the highest seqno is unique;
	// it always notifies and is never used for upgrades (ADR 0028).
	AutoRecover bool         `json:"autoRecover,omitempty"`
	Observed    *Observation `json:"observed,omitempty"`
	CreatedBy   string       `json:"createdBy"`
	CreatedAt   time.Time    `json:"createdAt"`
	CreateOp    string       `json:"createOp,omitempty"`
	// DeleteOp is the latest deletion operation (clusterdelete.go); while
	// it runs, node health changes raise no notifications.
	DeleteOp string `json:"deleteOp,omitempty"`
	// Drift is the last periodic drift check (drift.go).
	Drift *DriftObservation `json:"drift,omitempty"`
}

// Settings are manager-wide settings.
type Settings struct {
	// InstallationID labels every managed object (ADR 0004).
	InstallationID string `json:"installationId"`
	// VolumeBase is the agent's volume base directory.
	VolumeBase string `json:"volumeBase"`
	// AgentImage is the agent service image (pin by digest).
	AgentImage string `json:"agentImage,omitempty"`
	// Monitoring configures metric retention, the disk budget, alert rules
	// and notification channels. Zero values select the documented defaults.
	Monitoring MonitoringSettings `json:"monitoring,omitzero"`
	// MonitorSocket is the database's unix socket relative to a node's
	// volume root, used by health collection (default DefaultMonitorSocket).
	MonitorSocket string `json:"monitorSocket,omitempty"`
	// Backup configures backup jobs and inspection restores (backups.go).
	Backup BackupSettings `json:"backup,omitzero"`
	// Accounts are the database accounts (monitoring, backup) that the
	// create plan sets up on every new cluster. The secrets must exist and
	// be mounted by the agent service (ADR 0011).
	Accounts galera.Accounts `json:"accounts,omitzero"`
}

// DefaultMonitorSocket is the default Settings.MonitorSocket: the socket the
// Galera spec places on the node volume (galera.SocketRelPath, ADR 0011).
const DefaultMonitorSocket = galera.SocketRelPath

// OperationRecord stores an operation's definition (needed by a new leader
// to take it over) and its latest runner view (for the UI).
type OperationRecord struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Swarm   string `json:"swarm,omitempty"`
	Cluster string `json:"cluster,omitempty"`
	// Service is the managed service (services.go) of a service operation.
	Service     string           `json:"service,omitempty"`
	RequestedBy string           `json:"requestedBy"`
	RequestedAt time.Time        `json:"requestedAt"`
	Definition  runner.Operation `json:"definition"`
	// Nodes are the plan's node identities (for a replacement the new
	// node of the replaced slot), used to resolve health checks while the
	// operation runs.
	Nodes []galera.Node `json:"nodes,omitempty"`
	// GaleraPlan is the approved resulting state, applied atomically with success.
	GaleraPlan *galera.Plan `json:"galeraPlan,omitempty"`
	// ApprovalID and ApprovalDigest are set for operations whose
	// destructive steps wait for an explicit approval (approvals.go).
	ApprovalID     string `json:"approvalId,omitempty"`
	ApprovalDigest string `json:"approvalDigest,omitempty"`
	// Backend names the backup target (Backups.Targets) the operation's
	// restore job reads; empty is the primary backend. The result gates read
	// the job status from it.
	Backend string `json:"backend,omitempty"`
	// Snapshot names the pre-recovery data snapshot of a guided recovery;
	// both phases of a zero-UUID recovery share it (ADR 0028).
	Snapshot string `json:"snapshot,omitempty"`
	// AutoApproved marks a recovery operation that the cluster's autoRecover
	// policy approves instead of a human.
	AutoApproved bool `json:"autoApproved,omitempty"`
	// FollowUp is the recovery operation created after this prepare phase
	// (or "none" when the positions allowed no candidate).
	FollowUp string `json:"followUp,omitempty"`
	// EndNotified is set once the end-of-run notification was sent.
	EndNotified bool `json:"endNotified,omitempty"`
	// Diagnosis is the diagnosis a recovery operation was planned from.
	Diagnosis *galera.Diagnosis `json:"diagnosis,omitempty"`
	View      *runner.View      `json:"view,omitempty"`
	UpdatedAt time.Time         `json:"updatedAt,omitzero"`
	EndedAt   time.Time         `json:"endedAt,omitzero"`
}

// EventRecord is one stored event.
type EventRecord struct {
	ID       string       `json:"id"`
	Time     time.Time    `json:"time"`
	Swarm    string       `json:"swarm,omitempty"`
	Cluster  string       `json:"cluster,omitempty"`
	Severity api.Severity `json:"severity"`
	Message  string       `json:"message"`
}

// timeKey returns a key that sorts by time, with a random suffix against
// collisions.
func timeKey(t time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%020d-%s", t.UTC().UnixNano(), hex.EncodeToString(b[:]))
}

// newID returns a random identifier with a prefix.
func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}
