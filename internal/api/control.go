// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// Control, Backups and Settings are the command services behind the
// state-changing endpoints. The API authorizes (and, for state changes,
// audits) every call before it reaches a service, so implementations
// receive only permitted requests; they may check again (defence in depth).
// A nil service answers its endpoints with 503.
//
// Implementations report failures with the sentinel errors below; the API
// maps them to coarse codes and never forwards error text.

// Command errors. Each maps to one HTTP status and one coarse code.
var (
	// ErrInvalidInput: the request is well-formed JSON but semantically
	// invalid (400 bad_request).
	ErrInvalidInput = errors.New("api: invalid input")
	// ErrForbidden: the service refused the actor (403 forbidden).
	ErrForbidden = errors.New("api: forbidden")
	// ErrRequiresGuidedRecovery: an automatic replacement is not safe
	// (galera.ErrRequiresGuidedRecovery; 409 requires_guided_recovery).
	ErrRequiresGuidedRecovery = errors.New("api: guided recovery required")
	// ErrNoCapacity: no eligible host fits (409 no_capacity).
	ErrNoCapacity = errors.New("api: no capacity")
	// ErrPolicyConflict: contradictory placement policy (409 policy_conflict).
	ErrPolicyConflict = errors.New("api: policy conflict")
	// ErrSettingsIncomplete: required manager settings are missing (409
	// settings_incomplete).
	ErrSettingsIncomplete = errors.New("api: settings incomplete")
	// ErrClusterExists: the cluster name is used in the Swarm (409
	// cluster_exists).
	ErrClusterExists = errors.New("api: cluster exists")
	// ErrBusy: another operation holds a lock of the resource (409 busy).
	ErrBusy = errors.New("api: busy")
	// ErrApprovalMismatch: the approval digest does not match the
	// operation (409 approval_mismatch).
	ErrApprovalMismatch = errors.New("api: approval does not match the operation")
	// ErrNotLeader: this replica cannot run commands (503 not_leader).
	ErrNotLeader = errors.New("api: not leader")
)

// Host is one Swarm node as inventoried by the manager. Its ID in paths is
// the Swarm node ID (NodeID).
type Host struct {
	Swarm         string    `json:"swarm"`
	NodeID        string    `json:"nodeId"`
	Hostname      string    `json:"hostname,omitempty"`
	Role          string    `json:"role,omitempty"`
	Availability  string    `json:"availability,omitempty"`
	State         string    `json:"state,omitempty"`
	EngineVersion string    `json:"engineVersion,omitempty"`
	Enrollment    string    `json:"enrollment"` // discovered, enrolled, revoked
	Spare         bool      `json:"spare,omitempty"`
	NanoCPUs      int64     `json:"nanoCpus,omitempty"`
	MemoryBytes   int64     `json:"memoryBytes,omitempty"`
	ObservedAt    time.Time `json:"observedAt,omitzero"`
	// AgentIdentity reports whether an agent certificate is recorded for
	// the host (it says nothing about the agent being reachable).
	AgentIdentity bool `json:"agentIdentity,omitempty"`
}

// Enrollment states of a host.
const (
	EnrollmentDiscovered = "discovered"
	EnrollmentEnrolled   = "enrolled"
	EnrollmentRevoked    = "revoked"
)

// SwarmInput connects a Swarm.
type SwarmInput struct {
	Name       string `json:"name"`
	DockerHost string `json:"dockerHost"`
}

// AgentDeployment is the result of deploying agents to a Swarm.
type AgentDeployment struct {
	ServiceID string   `json:"serviceId,omitempty"`
	Created   bool     `json:"created"`
	Labeled   []string `json:"labeled"`
	Unlabeled []string `json:"unlabeled"`
	Failed    []string `json:"failed"`
}

// CreateClusterRequest mirrors manager.CreateClusterRequest.
type CreateClusterRequest struct {
	Swarm              string                 `json:"swarm"`
	Name               string                 `json:"name"`
	Size               int                    `json:"size"`
	Image              string                 `json:"image"`
	Network            string                 `json:"network"`
	RootPasswordSecret string                 `json:"rootPasswordSecret"`
	GcacheSize         string                 `json:"gcacheSize,omitempty"`
	Resources          galera.Resources       `json:"resources"`
	Placement          galera.PlacementPolicy `json:"placement"`
	AutoReplace        bool                   `json:"autoReplace,omitempty"`
}

// OperationRef names a started operation.
type OperationRef struct {
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"`
}

// Diagnosis is the read-only full-outage diagnosis (galera.Diagnosis
// without the plan). RequiresApproval is always true.
type Diagnosis struct {
	Candidate        string                `json:"candidate,omitempty"`
	Positions        []galera.NodePosition `json:"positions"`
	Reasons          []string              `json:"reasons"`
	Risks            []galera.Risk         `json:"risks,omitempty"`
	RequiresApproval bool                  `json:"requires_approval"`
}

// RecoveryInput requests the guided full-outage recovery. Candidate must
// be the slot the diagnosis proposes (the caller confirms what it saw).
type RecoveryInput struct {
	Candidate string `json:"candidate"`
}

// RecoveryStarted is a guided recovery operation waiting for approval.
type RecoveryStarted struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Candidate string            `json:"candidate"`
	Approval  OperationApproval `json:"approval"`
	Diagnosis Diagnosis         `json:"diagnosis"`
}

// OperationApproval describes the approval an operation waits for. Digest
// binds an approval to the exact stored operation definition.
type OperationApproval struct {
	Digest     string    `json:"digest"`
	ApprovedBy string    `json:"approvedBy,omitempty"`
	ApprovedAt time.Time `json:"approvedAt,omitzero"`
}

// OperationKindClusterDelete is the kind of a cluster deletion operation.
const OperationKindClusterDelete = "cluster/delete"

// ClusterDeletion is a started cluster deletion. Its removals wait for
// POST /operations/{id}/approve with Approval.Digest.
type ClusterDeletion struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Approval OperationApproval `json:"approval"`
}

// OperationKindOutageRecovery is the kind of a guided full-outage recovery.
const OperationKindOutageRecovery = "galera/full-outage-recovery"

// ApprovalPermission is the permission an approval of an operation needs:
// delete on the cluster for a cluster deletion, recover on the cluster for
// a guided full-outage recovery, restore on the cluster's backups for other
// operations, manager settings for unlocated ones.
func ApprovalPermission(kind, swarm, cluster string) (authz.Action, authz.Resource) {
	if swarm == "" || cluster == "" {
		return authz.ActionRestore, settingsResource
	}
	switch kind {
	case OperationKindClusterDelete:
		return authz.ActionDelete, authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope(swarm, cluster)}
	case OperationKindOutageRecovery:
		return authz.ActionRecover, authz.Resource{Type: authz.ResourceCluster, Scope: authz.ClusterScope(swarm, cluster)}
	}
	return authz.ActionRestore, backupResource(swarm, cluster)
}

// ApprovalInput approves an operation; Digest must equal the operation's
// OperationApproval.Digest.
type ApprovalInput struct {
	Digest string `json:"digest"`
}

// Control runs Swarm, host, cluster and operation commands.
type Control interface {
	Hosts(ctx context.Context) ([]Host, error)
	ConnectSwarm(ctx context.Context, actor authz.Subject, in SwarmInput) (Swarm, error)
	SyncSwarm(ctx context.Context, actor authz.Subject, swarmID string) error
	DeployAgents(ctx context.Context, actor authz.Subject, swarmID string) (AgentDeployment, error)
	SetEnrollment(ctx context.Context, actor authz.Subject, swarmID, nodeID string, enroll bool) (Host, error)
	CreateCluster(ctx context.Context, actor authz.Subject, req CreateClusterRequest) (OperationRef, error)
	ReplaceNode(ctx context.Context, actor authz.Subject, clusterID, slot string) (OperationRef, error)
	// DeleteCluster starts the deletion of a cluster (also a failed
	// create); its removals wait for an approval of the returned digest.
	DeleteCluster(ctx context.Context, actor authz.Subject, clusterID string) (ClusterDeletion, error)
	OutageDiagnosis(ctx context.Context, actor authz.Subject, clusterID string) (Diagnosis, error)
	// OutageDiagnosisRecover is OutageDiagnosis after recovering the
	// positions of crashed nodes (mariadbd --wsrep-recover).
	OutageDiagnosisRecover(ctx context.Context, actor authz.Subject, clusterID string) (Diagnosis, error)
	// OutageRecovery plans the guided full-outage recovery for the
	// confirmed candidate and starts it waiting for an approval.
	OutageRecovery(ctx context.Context, actor authz.Subject, clusterID, candidate string) (RecoveryStarted, error)
	ResumeOperation(ctx context.Context, actor authz.Subject, opID string) error
	AbortOperation(ctx context.Context, actor authz.Subject, opID string) error
	ApproveOperation(ctx context.Context, actor authz.Subject, opID, digest string) (OperationApproval, error)
}

// BackupPolicy is the schedule of a cluster's backups.
type BackupPolicy struct {
	Enabled bool `json:"enabled"`
	// Schedule is a five-field cron-like spec (dbbackup.ParseSchedule).
	Schedule string `json:"schedule"`
	// Timezone is an IANA zone name; empty means UTC.
	Timezone string `json:"timezone,omitempty"`
	// Missed is "skip" (default) or "run-once".
	Missed string `json:"missed,omitempty"`
	// Method is "physical" (the only scheduled method).
	Method string `json:"method,omitempty"`
}

// BackupSummary is the backup state of one cluster. Absent times are
// unknown, never "never".
type BackupSummary struct {
	Cluster string `json:"cluster"`
	Swarm   string `json:"swarm"`
	// Policy is the schedule spec of an enabled policy.
	Policy                string        `json:"policy,omitempty"`
	BackupPolicy          *BackupPolicy `json:"backupPolicy,omitempty"`
	LastBackupAt          time.Time     `json:"lastBackupAt,omitzero"`
	LastBackupID          string        `json:"lastBackupId,omitempty"`
	LastVerifiedRestoreAt time.Time     `json:"lastVerifiedRestoreAt,omitzero"`
	NextRunAt             time.Time     `json:"nextRunAt,omitzero"`
}

// InspectionInput starts an isolated inspection restore.
type InspectionInput struct {
	Cluster string `json:"cluster"`
	// Backup is the backup ID; empty selects the newest completed backup.
	Backup string `json:"backup,omitempty"`
	// Host is an explicitly approved Swarm node for the instance; empty
	// lets the manager select one.
	Host string `json:"host,omitempty"`
	// KeySecret is the Swarm secret with the recovery key for this restore;
	// empty uses the configured default.
	KeySecret string `json:"keySecret,omitempty"`
	// TTLMinutes bounds the instance lifetime (default 240).
	TTLMinutes int `json:"ttlMinutes,omitempty"`
}

// InspectionStarted is the result of starting an inspection. The
// operation pauses before its first destructive step until approved with
// Approval.Digest. The inspection password is returned only here.
type InspectionStarted struct {
	ID                 string            `json:"id"`
	Kind               string            `json:"kind"`
	Session            string            `json:"session"`
	Backup             string            `json:"backup"`
	Host               string            `json:"host"`
	Service            string            `json:"service"`
	Port               int               `json:"port"`
	ExpiresAt          time.Time         `json:"expiresAt"`
	Approval           OperationApproval `json:"approval"`
	InspectionAccount  string            `json:"inspectionAccount"`
	InspectionPassword string            `json:"inspectionPassword"`
}

// Backups runs backup commands.
type Backups interface {
	Backups(ctx context.Context) ([]BackupSummary, error)
	SetBackupPolicy(ctx context.Context, actor authz.Subject, clusterID string, p BackupPolicy) (BackupSummary, error)
	StartBackup(ctx context.Context, actor authz.Subject, clusterID string) (OperationRef, error)
	StartInspection(ctx context.Context, actor authz.Subject, in InspectionInput) (InspectionStarted, error)
}

// NotificationSettings is the readable notification configuration.
// Secrets are never returned; SecretSet and PasswordSet report presence.
type NotificationSettings struct {
	MinSeverity string              `json:"minSeverity"`
	Webhook     WebhookSettingsView `json:"webhook"`
	Email       EmailSettingsView   `json:"email"`
}

// WebhookSettingsView is the webhook part of NotificationSettings.
type WebhookSettingsView struct {
	Enabled      bool   `json:"enabled"`
	URL          string `json:"url,omitempty"`
	SecretSet    bool   `json:"secretSet"`
	AllowPrivate bool   `json:"allowPrivate,omitempty"`
}

// EmailSettingsView is the email part of NotificationSettings.
type EmailSettingsView struct {
	Enabled     bool     `json:"enabled"`
	Recipients  []string `json:"recipients"`
	Addr        string   `json:"addr,omitempty"`
	From        string   `json:"from,omitempty"`
	Username    string   `json:"username,omitempty"`
	PasswordSet bool     `json:"passwordSet"`
}

// NotificationSettingsInput replaces the notification configuration.
// Absent optional fields keep their stored values; Secret and Password are
// write-only and only replaced when given.
type NotificationSettingsInput struct {
	MinSeverity string               `json:"minSeverity,omitempty"`
	Webhook     WebhookSettingsInput `json:"webhook"`
	Email       EmailSettingsInput   `json:"email"`
}

// WebhookSettingsInput is the webhook part of NotificationSettingsInput.
type WebhookSettingsInput struct {
	Enabled      bool    `json:"enabled"`
	URL          string  `json:"url"`
	Secret       *string `json:"secret,omitempty"`
	AllowPrivate *bool   `json:"allowPrivate,omitempty"`
}

// EmailSettingsInput is the email part of NotificationSettingsInput.
type EmailSettingsInput struct {
	Enabled    bool     `json:"enabled"`
	Recipients []string `json:"recipients"`
	Addr       *string  `json:"addr,omitempty"`
	From       *string  `json:"from,omitempty"`
	Username   *string  `json:"username,omitempty"`
	Password   *string  `json:"password,omitempty"`
}

// NotificationTest is the outcome of a test delivery. Delivered=false
// carries no error text.
type NotificationTest struct {
	Channel   string `json:"channel"`
	Delivered bool   `json:"delivered"`
}

// Settings reads and writes manager settings.
type Settings interface {
	NotificationSettings(ctx context.Context) (NotificationSettings, error)
	SetNotificationSettings(ctx context.Context, actor authz.Subject, in NotificationSettingsInput) (NotificationSettings, error)
	// TestNotification sends a test event through one configured channel
	// ("webhook" or "email"); ErrSettingsIncomplete if it is not enabled.
	TestNotification(ctx context.Context, actor authz.Subject, channel string) (NotificationTest, error)
}

// Methods of Unavailable for the command services.

func (Unavailable) Hosts(context.Context) ([]Host, error) { return nil, ErrUnavailable }
func (Unavailable) ConnectSwarm(context.Context, authz.Subject, SwarmInput) (Swarm, error) {
	return Swarm{}, ErrUnavailable
}
func (Unavailable) SyncSwarm(context.Context, authz.Subject, string) error { return ErrUnavailable }
func (Unavailable) DeployAgents(context.Context, authz.Subject, string) (AgentDeployment, error) {
	return AgentDeployment{}, ErrUnavailable
}
func (Unavailable) SetEnrollment(context.Context, authz.Subject, string, string, bool) (Host, error) {
	return Host{}, ErrUnavailable
}
func (Unavailable) CreateCluster(context.Context, authz.Subject, CreateClusterRequest) (OperationRef, error) {
	return OperationRef{}, ErrUnavailable
}
func (Unavailable) ReplaceNode(context.Context, authz.Subject, string, string) (OperationRef, error) {
	return OperationRef{}, ErrUnavailable
}
func (Unavailable) DeleteCluster(context.Context, authz.Subject, string) (ClusterDeletion, error) {
	return ClusterDeletion{}, ErrUnavailable
}
func (Unavailable) OutageDiagnosis(context.Context, authz.Subject, string) (Diagnosis, error) {
	return Diagnosis{}, ErrUnavailable
}

// OutageDiagnosisRecover implements Control.
func (Unavailable) OutageDiagnosisRecover(context.Context, authz.Subject, string) (Diagnosis, error) {
	return Diagnosis{}, ErrUnavailable
}

// OutageRecovery implements Control.
func (Unavailable) OutageRecovery(context.Context, authz.Subject, string, string) (RecoveryStarted, error) {
	return RecoveryStarted{}, ErrUnavailable
}
func (Unavailable) ResumeOperation(context.Context, authz.Subject, string) error {
	return ErrUnavailable
}
func (Unavailable) AbortOperation(context.Context, authz.Subject, string) error {
	return ErrUnavailable
}
func (Unavailable) ApproveOperation(context.Context, authz.Subject, string, string) (OperationApproval, error) {
	return OperationApproval{}, ErrUnavailable
}
func (Unavailable) Backups(context.Context) ([]BackupSummary, error) { return nil, ErrUnavailable }
func (Unavailable) SetBackupPolicy(context.Context, authz.Subject, string, BackupPolicy) (BackupSummary, error) {
	return BackupSummary{}, ErrUnavailable
}
func (Unavailable) StartBackup(context.Context, authz.Subject, string) (OperationRef, error) {
	return OperationRef{}, ErrUnavailable
}
func (Unavailable) StartInspection(context.Context, authz.Subject, InspectionInput) (InspectionStarted, error) {
	return InspectionStarted{}, ErrUnavailable
}
func (Unavailable) NotificationSettings(context.Context) (NotificationSettings, error) {
	return NotificationSettings{}, ErrUnavailable
}
func (Unavailable) SetNotificationSettings(context.Context, authz.Subject, NotificationSettingsInput) (NotificationSettings, error) {
	return NotificationSettings{}, ErrUnavailable
}
func (Unavailable) TestNotification(context.Context, authz.Subject, string) (NotificationTest, error) {
	return NotificationTest{}, ErrUnavailable
}

var (
	_ Control  = Unavailable{}
	_ Backups  = Unavailable{}
	_ Settings = Unavailable{}
)

func (s *server) control() Control {
	if s.cfg.Control != nil {
		return s.cfg.Control
	}
	return Unavailable{}
}

func (s *server) backups() Backups {
	if s.cfg.Backups != nil {
		return s.cfg.Backups
	}
	return Unavailable{}
}

func (s *server) settings() Settings {
	if s.cfg.Settings != nil {
		return s.cfg.Settings
	}
	return Unavailable{}
}
