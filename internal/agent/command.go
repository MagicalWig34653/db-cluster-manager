// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// Kind names a command.
type Kind string

// Command kinds.
const (
	// KindCreateService creates a Swarm service (args: ServiceSpec). The
	// installation label is set by the agent; a spec carrying another
	// installation is refused. The new ID joins the allow-list.
	KindCreateService Kind = "create-service"
	// KindRemoveService removes an owned service (args: ServiceArgs).
	KindRemoveService Kind = "remove-service"
	// KindStartDB verifies the volume identity, then scales the owned
	// database service to one replica (args: StartDBArgs).
	KindStartDB Kind = "start-db"
	// KindStopDB scales an owned service to zero (args: ServiceArgs).
	KindStopDB Kind = "stop-db"
	// KindRemoveVolume verifies the volume identity, then removes the owned
	// volume (args: RemoveVolumeArgs).
	KindRemoveVolume Kind = "remove-volume"
	// KindInitIdentity consumes a single-use new-identity token and writes
	// the marker on an empty volume (args: InitIdentityArgs).
	KindInitIdentity Kind = "init-identity"
	// KindVerifyIdentity runs Verify without other effect (args: VerifyIdentityArgs).
	KindVerifyIdentity Kind = "verify-identity"
	// KindInspect inspects an owned object (args: InspectArgs).
	KindInspect Kind = "inspect"
	// KindListOwned lists objects of a kind that pass the ownership guard
	// (args: ListOwnedArgs).
	KindListOwned Kind = "list-owned"
	// KindCreateVolume creates the local volume of a node below VolumeBase
	// (args: CreateVolumeArgs). It never writes an identity marker.
	KindCreateVolume Kind = "create-volume"
	// KindUpdateService replaces the spec of an owned service, found by
	// name (args: UpdateServiceArgs). Ownership labels, placement, mounts
	// and replica count cannot change.
	KindUpdateService Kind = "update-service"
	// KindAllowObjects adds manager inventory IDs to the allow-list (args:
	// AllowObjectsArgs). Objects still need the installation label.
	KindAllowObjects Kind = "allow-objects"
	// KindCollectHealth reads the wsrep status of the local database node
	// through the configured StatusQuerier (args: CollectHealthArgs).
	// Read-only: admitted by fencing, never recorded in the step record.
	KindCollectHealth Kind = "collect-health"
	// KindReadGrastate verifies the volume identity and reads the node's
	// grastate.dat, confined to the volume root (args: ReadGrastateArgs).
	// Read-only like KindCollectHealth.
	KindReadGrastate Kind = "read-grastate"
	// KindInitVolumeDirs verifies the volume identity, then creates the
	// database directories data/ and run/ below the volume root, owned by
	// the database user (args: InitVolumeDirsArgs). The volume root and the
	// marker are not changed.
	KindInitVolumeDirs Kind = "init-volume-dirs"
	// KindExecSQL verifies the volume identity, then runs one allow-listed
	// SQL template (internal/dbadmin) through the node's unix socket on the
	// volume (args: ExecSQLArgs). Passwords are read from the agent's secret
	// mount; no SQL text and no password is accepted from the manager.
	KindExecSQL Kind = "exec-sql"
	// KindAwaitService reports whether the last spec update of an owned
	// service has completed (args: AwaitServiceArgs). It changes nothing (but
	// is recorded like other commands, as it reads the allow-list); it fails
	// with CodeUpdatePending while Swarm still replaces the task. Lab E4: a
	// wait-synced right after update-service otherwise observed the old
	// task, before Swarm had stopped it.
	KindAwaitService Kind = "await-service"
)

// AwaitServiceArgs are the arguments of KindAwaitService.
type AwaitServiceArgs struct {
	ServiceName string `json:"service_name"`
}

// Codes of KindAwaitService.
const (
	CodeUpdatePending Code = "update-pending"
	CodeUpdateFailed  Code = "update-failed"
)

// ReadOnly reports kinds without effect. They pass the fencing guard like
// every command but are not recorded: a re-sent read returns a fresh
// observation, and frequent polling does not grow the state file.
func ReadOnly(k Kind) bool { return k == KindCollectHealth || k == KindReadGrastate }

// Command is one fenced instruction from the manager leader.
type Command struct {
	Term   uint64          `json:"term"`
	OpID   string          `json:"op_id"`
	StepID string          `json:"step_id"`
	Kind   Kind            `json:"kind"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// ServiceArgs names one service.
type ServiceArgs struct {
	ServiceID string `json:"service_id"`
}

// CreateVolumeArgs are the arguments of KindCreateVolume. VolumeRoot must be
// VolumeBase/Volume. Labels are optional extra labels (for example
// dbcm.cluster and dbcm.node); dbcm.installation, dbcm.op and dbcm.step are
// set by the agent.
type CreateVolumeArgs struct {
	Volume     string            `json:"volume"`
	VolumeRoot string            `json:"volume_root"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// UpdateServiceArgs are the arguments of KindUpdateService. The service is
// named because a service created earlier in the same operation has no ID at
// planning time; Spec.Name must equal ServiceName.
type UpdateServiceArgs struct {
	ServiceName string      `json:"service_name"`
	Spec        ServiceSpec `json:"spec"`
}

// StartDBArgs are the arguments of KindStartDB.
type StartDBArgs struct {
	ServiceID  string   `json:"service_id"`
	VolumeRoot string   `json:"volume_root"`
	Expected   Identity `json:"expected"`
}

// RemoveVolumeArgs are the arguments of KindRemoveVolume.
type RemoveVolumeArgs struct {
	VolumeID   string   `json:"volume_id"`
	VolumeRoot string   `json:"volume_root"`
	Expected   Identity `json:"expected"`
	// MissingOK reports success when the volume does not exist (a cluster
	// deletion after a create that failed before create-volume). A volume
	// that exists but is not owned is still refused.
	MissingOK bool `json:"missing_ok,omitempty"`
	// AllowUnmarked also removes an owned volume that has no identity
	// marker and holds no file at all (a create that failed between
	// create-volume and init-identity). A volume with data of unknown
	// identity or a different marker is still refused.
	AllowUnmarked bool `json:"allow_unmarked,omitempty"`
}

// InitIdentityArgs are the arguments of KindInitIdentity.
type InitIdentityArgs struct {
	VolumeRoot string           `json:"volume_root"`
	Token      NewIdentityToken `json:"token"`
}

// VerifyIdentityArgs are the arguments of KindVerifyIdentity.
type VerifyIdentityArgs struct {
	VolumeRoot string   `json:"volume_root"`
	Expected   Identity `json:"expected"`
}

// InspectArgs are the arguments of KindInspect.
type InspectArgs struct {
	ObjectKind ObjectKind `json:"object_kind"`
	ID         string     `json:"id"`
}

// ListOwnedArgs are the arguments of KindListOwned.
type ListOwnedArgs struct {
	ObjectKind ObjectKind `json:"object_kind"`
}

// AllowObjectsArgs are the arguments of KindAllowObjects.
type AllowObjectsArgs struct {
	IDs []string `json:"ids"`
}

// CollectHealthArgs are the arguments of KindCollectHealth. Exactly one
// endpoint is given: Address, a loopback IP literal with port (a database
// port published on this host), or Socket, the absolute path of the
// database's unix socket below the volume base.
type CollectHealthArgs struct {
	Address string `json:"address,omitempty"`
	Socket  string `json:"socket,omitempty"`
}

// HealthOutput is the output of KindCollectHealth: the wsrep_% variables of
// SHOW GLOBAL STATUS.
type HealthOutput struct {
	Status map[string]string `json:"status"`
}

// InitVolumeDirsArgs are the arguments of KindInitVolumeDirs. UID and GID
// are the numeric database owner (1..2^31-1).
type InitVolumeDirsArgs struct {
	VolumeRoot string   `json:"volume_root"`
	Expected   Identity `json:"expected"`
	UID        int      `json:"uid"`
	GID        int      `json:"gid"`
}

// ExecSQLArgs are the arguments of KindExecSQL. Template is one of
// dbadmin.BootstrapTemplates; User, Host, RequireSSL and Logical are its
// structured arguments. PasswordSecret names the secret file with the
// password the template sets; AdminSecret the one with the password of
// AdminUser (default "root"), the account the agent connects as. Both are
// file names in the agent's secrets directory (Config.SecretsDir).
type ExecSQLArgs struct {
	VolumeRoot     string   `json:"volume_root"`
	Expected       Identity `json:"expected"`
	Template       string   `json:"template"`
	User           string   `json:"user,omitempty"`
	Host           string   `json:"host,omitempty"`
	RequireSSL     bool     `json:"require_ssl,omitempty"`
	Logical        bool     `json:"logical,omitempty"`
	PasswordSecret string   `json:"password_secret"`
	AdminUser      string   `json:"admin_user,omitempty"`
	AdminSecret    string   `json:"admin_secret"`
}

// ExecSQLOutput is the output of KindExecSQL: the executed statements with
// every password replaced by ***.
type ExecSQLOutput struct {
	Template   string   `json:"template"`
	Statements []string `json:"statements"`
}

// ReadGrastateArgs are the arguments of KindReadGrastate. DataDir is the
// datadir relative to the volume root (default "data", galera.DataDir).
type ReadGrastateArgs struct {
	VolumeRoot string   `json:"volume_root"`
	Expected   Identity `json:"expected"`
	DataDir    string   `json:"data_dir,omitempty"`
}

// GrastateOutput is the output of KindReadGrastate. Present is false when
// the file does not exist. Content is the raw file; Grastate is set when it
// parses, ParseError otherwise.
type GrastateOutput struct {
	Present    bool             `json:"present"`
	Content    string           `json:"content,omitempty"`
	Grastate   *galera.Grastate `json:"grastate,omitempty"`
	ParseError string           `json:"parse_error,omitempty"`
}

// Status is the outcome class of an executed step.
type Status string

// Step outcomes.
const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"  // the effect was attempted and failed
	StatusRefused   Status = "refused" // a guard refused the step before its effect
	StatusUnknown   Status = "unknown" // completion could not be observed
)

// Code is a stable machine-readable reason for a non-success outcome.
type Code string

// Outcome codes.
const (
	CodeNotOwned              Code = "not-owned"
	CodeNotFound              Code = "not-found"
	CodeInvalidPath           Code = "invalid-path"
	CodeVolumeMismatch        Code = "volume-mismatch"
	CodeIdentityNotConfigured Code = "identity-not-configured"
	CodeForeignMarker         Code = "foreign-marker"
	CodeIdentityMismatch      Code = "identity-mismatch"
	CodeMarkerCorrupt         Code = "marker-corrupt"
	CodeUnknownData           Code = "unknown-data"
	CodeEmptyVolume           Code = "empty-volume"
	CodeTokenConsumed         Code = "token-consumed"
	CodeDocker                Code = "docker-error"
	CodeIO                    Code = "io-error"
	CodeInDoubt               Code = "in-doubt"
	CodeImmutable             Code = "immutable-field"
	CodeDBUnavailable         Code = "db-unavailable"
	CodeNotConfigured         Code = "not-configured"
)

// Result is the recorded outcome of one step.
type Result struct {
	OpID     string          `json:"op_id"`
	StepID   string          `json:"step_id"`
	Kind     Kind            `json:"kind"`
	Status   Status          `json:"status"`
	Code     Code            `json:"code,omitempty"`
	Message  string          `json:"message,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
	Replayed bool            `json:"replayed,omitempty"` // true when returned from the record
}

// Errors returned by Handle when a command is not executed and not recorded.
var (
	ErrInvalidCommand = errors.New("agent: invalid command")
	// ErrStepConflict: the (OpID, StepID) was recorded with another kind or
	// other arguments.
	ErrStepConflict = errors.New("agent: step ID reused with different command")
	// ErrPersist: the state file could not be written. If it happens after
	// the effect, the returned Result is valid but not durable, and a re-sent
	// step reports StatusUnknown.
	ErrPersist = errors.New("agent: state not persisted")
)

const maxIDLen = 256

// decoded is a validated command with its typed arguments.
type decoded struct {
	cmd    Command
	args   any
	digest string
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCommand, fmt.Sprintf(format, a...))
}

// decodeCommand validates the command shape without side effects.
func decodeCommand(cmd Command, volumeBase string) (decoded, error) {
	if err := checkID("op_id", cmd.OpID); err != nil {
		return decoded{}, err
	}
	if err := checkID("step_id", cmd.StepID); err != nil {
		return decoded{}, err
	}
	var args any
	switch cmd.Kind {
	case KindCreateService:
		args = &ServiceSpec{}
	case KindRemoveService, KindStopDB:
		args = &ServiceArgs{}
	case KindStartDB:
		args = &StartDBArgs{}
	case KindRemoveVolume:
		args = &RemoveVolumeArgs{}
	case KindInitIdentity:
		args = &InitIdentityArgs{}
	case KindVerifyIdentity:
		args = &VerifyIdentityArgs{}
	case KindInspect:
		args = &InspectArgs{}
	case KindListOwned:
		args = &ListOwnedArgs{}
	case KindAllowObjects:
		args = &AllowObjectsArgs{}
	case KindCreateVolume:
		args = &CreateVolumeArgs{}
	case KindUpdateService:
		args = &UpdateServiceArgs{}
	case KindRunBackup:
		args = &RunBackupArgs{}
	case KindRunRestore:
		args = &RunRestoreArgs{}
	case KindRemoveJob:
		args = &RemoveJobArgs{}
	case KindCollectHealth:
		args = &CollectHealthArgs{}
	case KindReadGrastate:
		args = &ReadGrastateArgs{}
	case KindInitVolumeDirs:
		args = &InitVolumeDirsArgs{}
	case KindExecSQL:
		args = &ExecSQLArgs{}
	case KindAwaitService, KindAwaitStopped, KindStartService:
		args = &AwaitServiceArgs{}
	case KindRemoveClusterService:
		args = &RemoveClusterServiceArgs{}
	case KindRemoveSecret:
		args = &RemoveSecretArgs{}
	case KindCheckRestoreTarget:
		args = &CheckRestoreTargetArgs{}
	case KindMarkSafeToBootstrap:
		args = &MarkSafeArgs{}
	case KindWsrepRecover:
		args = &WsrepRecoverArgs{}
	default:
		return decoded{}, invalid("unknown kind %q", cmd.Kind)
	}
	raw := cmd.Args
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(args); err != nil {
		return decoded{}, invalid("args for %s: %v", cmd.Kind, err)
	}
	if dec.More() {
		return decoded{}, invalid("trailing data in args")
	}
	if err := checkArgs(args, volumeBase); err != nil {
		return decoded{}, err
	}
	digest, err := argsDigest(cmd.Kind, raw)
	if err != nil {
		return decoded{}, invalid("args: %v", err)
	}
	return decoded{cmd: cmd, args: args, digest: digest}, nil
}

func checkID(name, v string) error {
	if v == "" || len(v) > maxIDLen {
		return invalid("%s must be 1..%d bytes", name, maxIDLen)
	}
	return nil
}

func checkArgs(args any, volumeBase string) error {
	switch a := args.(type) {
	case *ServiceSpec:
		if _, ok := a.Labels[LabelJob]; ok {
			return invalid("label %s is reserved for run-backup and run-restore", LabelJob)
		}
		return checkSpec(*a)
	case *RunBackupArgs:
		return checkRunBackup(a, volumeBase)
	case *RunRestoreArgs:
		return checkRunRestore(a, volumeBase)
	case *RemoveJobArgs:
		if !jobNameRe.MatchString(a.JobName) {
			return invalid("job_name %q", a.JobName)
		}
	case *AwaitServiceArgs:
		if a.ServiceName == "" || len(a.ServiceName) > maxIDLen {
			return invalid("service_name must be 1..%d bytes", maxIDLen)
		}
	case *RemoveClusterServiceArgs:
		if a.ServiceName == "" || len(a.ServiceName) > maxIDLen || a.ClusterID == "" || len(a.ClusterID) > maxIDLen {
			return invalid("service_name and cluster_id must be 1..%d bytes", maxIDLen)
		}
	case *RemoveSecretArgs:
		if !secretRe.MatchString(a.Secret) {
			return invalid("secret %q", a.Secret)
		}
	case *CheckRestoreTargetArgs:
		if a.TargetVolume == "" || !a.Expected.complete() {
			return invalid("target_volume and its expected identity required")
		}
		return checkVolumeRoot(a.TargetVolumeRoot, volumeBase)
	case *UpdateServiceArgs:
		if a.ServiceName == "" || len(a.ServiceName) > maxIDLen {
			return invalid("service_name must be 1..%d bytes", maxIDLen)
		}
		if _, ok := a.Spec.Labels[LabelJob]; ok {
			return invalid("label %s is reserved for run-backup and run-restore", LabelJob)
		}
		if a.Spec.Name != a.ServiceName {
			return invalid("spec name %q differs from service_name %q", a.Spec.Name, a.ServiceName)
		}
		return checkSpec(a.Spec)
	case *CreateVolumeArgs:
		if !volumeName.MatchString(a.Volume) {
			return invalid("volume name %q (want [A-Za-z0-9][A-Za-z0-9_.-]*, at most 255 bytes)", a.Volume)
		}
		if err := checkVolumeRoot(a.VolumeRoot, volumeBase); err != nil {
			return err
		}
		if want := filepath.Join(volumeBase, a.Volume); a.VolumeRoot != want {
			return invalid("volume_root %q must be %q", a.VolumeRoot, want)
		}
	case *ServiceArgs:
		if a.ServiceID == "" {
			return invalid("service_id required")
		}
	case *StartDBArgs:
		if a.ServiceID == "" {
			return invalid("service_id required")
		}
		return checkVolumeRoot(a.VolumeRoot, volumeBase)
	case *RemoveVolumeArgs:
		if a.VolumeID == "" {
			return invalid("volume_id required")
		}
		return checkVolumeRoot(a.VolumeRoot, volumeBase)
	case *InitIdentityArgs:
		return checkVolumeRoot(a.VolumeRoot, volumeBase)
	case *VerifyIdentityArgs:
		return checkVolumeRoot(a.VolumeRoot, volumeBase)
	case *InspectArgs:
		if a.ObjectKind == "" || a.ID == "" {
			return invalid("object_kind and id required")
		}
	case *ListOwnedArgs:
		if a.ObjectKind == "" {
			return invalid("object_kind required")
		}
	case *CollectHealthArgs:
		switch {
		case (a.Address == "") == (a.Socket == ""):
			return invalid("exactly one of address and socket required")
		case a.Address != "":
			if !loopbackHostPort(a.Address) {
				return invalid("address %q must be a loopback IP literal with port", a.Address)
			}
		default:
			return checkVolumeRoot(a.Socket, volumeBase)
		}
	case *ReadGrastateArgs:
		if a.DataDir != "" && !filepath.IsLocal(a.DataDir) {
			return invalid("data_dir %q must be a local relative path", a.DataDir)
		}
		return checkVolumeRoot(a.VolumeRoot, volumeBase)
	case *InitVolumeDirsArgs:
		if a.UID <= 0 || a.GID <= 0 || a.UID > math.MaxInt32 || a.GID > math.MaxInt32 {
			return invalid("uid %d / gid %d must be 1..%d (never root)", a.UID, a.GID, math.MaxInt32)
		}
		return checkVolumeRoot(a.VolumeRoot, volumeBase)
	case *ExecSQLArgs:
		return checkExecSQL(a, volumeBase)
	case *MarkSafeArgs:
		return checkMarkSafe(a, volumeBase)
	case *WsrepRecoverArgs:
		return checkWsrepRecover(a, volumeBase)
	case *AllowObjectsArgs:
		if len(a.IDs) == 0 {
			return invalid("ids required")
		}
		for _, id := range a.IDs {
			if id == "" {
				return invalid("empty id")
			}
		}
	}
	return nil
}

func checkSpec(a ServiceSpec) error {
	if a.Name == "" || a.Image == "" {
		return invalid("service spec needs name and image")
	}
	switch a.EndpointMode {
	case "", EndpointModeVIP, EndpointModeDNSRR:
	default:
		return invalid("endpoint_mode %q (want vip or dnsrr)", a.EndpointMode)
	}
	if a.StopGracePeriodNS < 0 {
		return invalid("negative stop_grace_period_ns")
	}
	for _, refs := range [][]string{a.Networks, a.Secrets} {
		for _, r := range refs {
			if r == "" || len(r) > maxIDLen {
				return invalid("network or secret reference must be 1..%d bytes", maxIDLen)
			}
		}
	}
	return nil
}

// volumeName is the accepted shape of a volume name (Docker's rule).
var volumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)

// checkVolumeRoot requires a clean absolute path strictly below volumeBase.
// Symlinks are checked again at execution time.
func checkVolumeRoot(p, volumeBase string) error {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return invalid("volume_root %q must be a clean absolute path", p)
	}
	if !within(volumeBase, p) {
		return invalid("volume_root %q is not below the volume base %q", p, volumeBase)
	}
	return nil
}

// within reports whether p is strictly below base (both clean).
func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// argsDigest hashes kind and the canonical JSON form of the arguments
// (object keys sorted, insignificant whitespace removed).
func argsDigest(kind Kind, raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write(canon)
	return hex.EncodeToString(h.Sum(nil)), nil
}
