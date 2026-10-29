// SPDX-License-Identifier: AGPL-3.0-only

// Package imageupdate is the image update service (X16, ADR 0025): it
// finds newer images for managed Swarm services, turns them into reviewed
// plans and, under an explicit per-service policy, applies them through the
// existing approval-gated service update operation (agent update-service
// with the Swarm UpdateConfig and RollbackConfig) with a health gate and
// rollback.
//
// Scope: only services that carry the installation's ownership labels. A
// foreign service is considered only when it explicitly opted in by label
// and inventory entry, and then only for notifications: the manager has no
// service record to update. The Docker engine itself is not handled here
// (ADR 0019: reviewed plans only).
//
// Status: implemented with fakes only; no registry, Swarm or VM was used.
package imageupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
)

// Labels that make a service eligible (agent.LabelInstallation has the same
// value; the manager adapter test keeps them equal).
const (
	LabelInstallation = "dbcm.installation"
	// LabelImageUpdate with the value "opt-in" is the explicit opt-in of a
	// service the manager does not own.
	LabelImageUpdate = "dbcm.image-update"
	OptInValue       = "opt-in"
)

// Collections in the replicated document store (vuln.KV).
const (
	CollPolicies = "imageupdate_policies"
	CollPlans    = "imageupdate_plans"
	CollState    = "imageupdate_state"
)

// Errors. The API maps them to stable codes.
var (
	ErrInvalid         = errors.New("imageupdate: invalid request")
	ErrNotFound        = errors.New("imageupdate: not found")
	ErrConflict        = errors.New("imageupdate: conflict")
	ErrDigest          = errors.New("imageupdate: plan digest mismatch")
	ErrOutsideWindow   = errors.New("imageupdate: outside the maintenance window")
	ErrRoutedToPlanner = errors.New("imageupdate: this service is a database cluster; use the rolling or minor-upgrade planner")
	ErrForeign         = errors.New("imageupdate: the service is not owned by this installation; it can only be reported")
	ErrStale           = errors.New("imageupdate: the running image changed since the plan was made")
	ErrBusy            = errors.New("imageupdate: another operation is running on the service")
	ErrNotOwned        = errors.New("imageupdate: the service does not carry this installation's ownership labels")
)

// Mode is what the service does about candidates.
type Mode string

// Modes. Nothing runs without a policy (manual): candidates are only found
// when an operator asks.
const (
	// ModeManual: no scheduled check; an operator asks for a check and
	// approves plans.
	ModeManual Mode = "manual"
	// ModeNotify: scheduled checks create reviewed plans and notify; a
	// human approves and applies.
	ModeNotify Mode = "notify"
	// ModeAuto: scheduled checks create plans that the policy approves
	// itself when the candidate is inside the automatic limits, and apply
	// them inside a maintenance window.
	ModeAuto Mode = "auto"
)

// Strategy is the rolling-update strategy; it becomes the Swarm UpdateConfig
// and RollbackConfig of the update. OnFailure is always "rollback".
type Strategy struct {
	Order           string  `json:"order,omitempty"` // stop-first (default) or start-first
	Parallelism     int     `json:"parallelism,omitempty"`
	DelaySeconds    int     `json:"delaySeconds,omitempty"`
	MonitorSeconds  int     `json:"monitorSeconds,omitempty"`
	MaxFailureRatio float64 `json:"maxFailureRatio,omitempty"`
}

// Policy governs one service. The zero value is manual / digest-only.
type Policy struct {
	ServiceID string         `json:"serviceId"`
	Mode      Mode           `json:"mode"`
	Track     Track          `json:"track"`
	Windows   []patch.Window `json:"windows,omitempty"`
	Strategy  Strategy       `json:"strategy"`
	// CheckEveryMinutes is the scheduled check interval (default 360).
	CheckEveryMinutes int `json:"checkEveryMinutes,omitempty"`
	// WaitMinutes is how long an applied update may take until the health
	// gate has an answer (default 15). After it the plan fails with an
	// unknown outcome; nothing is rolled back by the manager itself.
	WaitMinutes int `json:"waitMinutes,omitempty"`
}

// Defaults.
const (
	DefaultCheckEvery = 360
	DefaultWaitMinute = 15
)

func (p Policy) normalized() Policy {
	if p.Mode == "" {
		p.Mode = ModeManual
	}
	if p.Track == "" {
		p.Track = TrackDigest
	}
	if p.CheckEveryMinutes == 0 {
		p.CheckEveryMinutes = DefaultCheckEvery
	}
	if p.WaitMinutes == 0 {
		p.WaitMinutes = DefaultWaitMinute
	}
	return p
}

// Validate checks the policy against the service it governs.
func (p Policy) Validate(ms ManagedService) error {
	p = p.normalized()
	fail := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }
	switch p.Mode {
	case ModeManual, ModeNotify, ModeAuto:
	default:
		return fail("mode %q", p.Mode)
	}
	if !p.Track.Valid() {
		return fail("track %q", p.Track)
	}
	if p.CheckEveryMinutes < 15 || p.CheckEveryMinutes > 10080 {
		return fail("checkEveryMinutes %d out of range 15..10080", p.CheckEveryMinutes)
	}
	if p.WaitMinutes < 1 || p.WaitMinutes > 240 {
		return fail("waitMinutes %d out of range 1..240", p.WaitMinutes)
	}
	if len(p.Windows) > 14 {
		return fail("too many windows")
	}
	for _, w := range p.Windows {
		if err := w.Validate(); err != nil {
			return fail("%v", err)
		}
	}
	s := p.Strategy
	if s.Order != "" && s.Order != "stop-first" && s.Order != "start-first" {
		return fail("strategy order %q", s.Order)
	}
	if s.Parallelism < 0 || s.Parallelism > 50 || s.DelaySeconds < 0 || s.DelaySeconds > 3600 || s.MonitorSeconds < 0 || s.MonitorSeconds > 3600 ||
		s.MaxFailureRatio < 0 || s.MaxFailureRatio > 1 || s.MaxFailureRatio != s.MaxFailureRatio {
		return fail("strategy out of range")
	}
	db := IsDatabase(ms.Type, repoOf(ms.Image))
	if db && p.Track == TrackMajor {
		return fail("a database never follows major versions")
	}
	if p.Mode == ModeAuto {
		if ms.Type == "galera" {
			return fail("a Galera cluster is never updated automatically; it goes through the rolling or minor-upgrade planner")
		}
		if p.Track == TrackMajor {
			return fail("automatic mode never crosses a major version")
		}
		if len(p.Windows) == 0 {
			return fail("automatic mode needs at least one maintenance window")
		}
		if ms.Foreign {
			return fail("a foreign service can only be reported")
		}
	}
	if ms.Foreign && p.Mode != ModeManual && p.Mode != ModeNotify {
		return fail("a foreign service can only be reported")
	}
	return nil
}

func (p Policy) inWindow(now time.Time) bool {
	if len(p.Windows) == 0 {
		return true
	}
	for _, w := range p.Windows {
		if w.Contains(now) {
			return true
		}
	}
	return false
}

func repoOf(image string) string {
	r, err := ParseRef(image)
	if err != nil {
		return image
	}
	return r.Repo
}

// ManagedService is what the service needs to know about a service.
type ManagedService struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Swarm string `json:"swarm,omitempty"`
	Type  string `json:"type"`
	// Image is the image reference of the desired state.
	Image  string            `json:"image"`
	Labels map[string]string `json:"labels,omitempty"`
	// Foreign marks a service from the inventory that has no managed
	// service record. It is eligible only with the opt-in label and then
	// only for notifications.
	Foreign bool `json:"foreign,omitempty"`
}

// Source lists the services.
type Source interface {
	Services(ctx context.Context) ([]ManagedService, error)
}

// OpState is the state of a service update operation.
type OpState string

// Operation states.
const (
	OpRunning    OpState = "running"
	OpSucceeded  OpState = "succeeded"
	OpFailed     OpState = "failed"
	OpRolledBack OpState = "rolled_back"
)

// Started names a started service operation and the digest its approval is
// bound to.
type Started struct {
	OperationID string
	Digest      string
}

// OpStatus is the observed state of an operation.
type OpStatus struct {
	State OpState
	// Runtime is the agent runtime that executed the update (ADR 0024).
	Runtime agentrt.Runtime
	Detail  string
}

// Driver executes service updates. The manager adapter uses the managed
// service update operation: agent update-service with the Swarm
// UpdateConfig/RollbackConfig, await-service and the health gate. The
// operation waits for an approval bound to its digest; Approve gives it.
type Driver interface {
	StartUpdate(ctx context.Context, actor, serviceID, image string, s Strategy) (Started, error)
	// StartRollback starts the update back to the spec before the last
	// successful update.
	StartRollback(ctx context.Context, actor, serviceID string, s Strategy) (Started, error)
	Approve(ctx context.Context, actor, operationID, digest string) error
	Status(ctx context.Context, operationID string) (OpStatus, error)
}

// Resolver reads tags and digests from a registry.
type Resolver interface {
	Tags(ctx context.Context, ref Ref) ([]string, error)
	// Digest returns the manifest digest of ref.Tag.
	Digest(ctx context.Context, ref Ref) (string, error)
}

// PlanState is the state of a plan.
type PlanState string

// Plan states.
const (
	PlanProposed   PlanState = "proposed"
	PlanApproved   PlanState = "approved"
	PlanRunning    PlanState = "running"
	PlanSucceeded  PlanState = "succeeded"
	PlanFailed     PlanState = "failed"
	PlanRolledBack PlanState = "rolled_back"
	PlanDeclined   PlanState = "declined"
	PlanSuperseded PlanState = "superseded"
)

func (s PlanState) open() bool { return s == PlanProposed || s == PlanApproved }

// Image names an image by tag and recorded digest.
type Image struct {
	Ref    string `json:"ref"`
	Tag    string `json:"tag,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// Route says how a plan can run.
const (
	RouteServiceUpdate = "service-update"
	RoutePlanner       = "planner"
	RouteReportOnly    = "report-only"
)

// Sources of a candidate.
const (
	SourceRegistry   = "registry"
	SourceImageBuild = "imagebuild"
	SourceRollback   = "rollback"
)

// RollbackRecord says what happened to the previous image.
type RollbackRecord struct {
	// Automatic: Swarm rolled the service back itself (RollbackConfig).
	Automatic bool      `json:"automatic"`
	Restored  string    `json:"restored"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
	// PlanID is the rollback plan of a manual rollback.
	PlanID string `json:"planId,omitempty"`
}

// Plan is a reviewed image update of one service. The digest covers
// everything that will be applied.
type Plan struct {
	ID          string    `json:"id"`
	ServiceID   string    `json:"serviceId"`
	ServiceName string    `json:"serviceName"`
	Swarm       string    `json:"swarm,omitempty"`
	ServiceType string    `json:"serviceType"`
	Kind        Kind      `json:"kind"`
	Source      string    `json:"source"`
	Reference   string    `json:"reference,omitempty"` // build job ID or rolled-back plan ID
	From        Image     `json:"from"`
	To          Image     `json:"to"`
	Strategy    Strategy  `json:"strategy"`
	Database    bool      `json:"database,omitempty"`
	Route       string    `json:"route"`
	Digest      string    `json:"digest"`
	State       PlanState `json:"state"`
	// Auto: approved by the service's own policy, not by a person.
	Auto       bool      `json:"auto,omitempty"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	ApprovedBy string    `json:"approvedBy,omitempty"`
	ApprovedAt time.Time `json:"approvedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// OperationID is the service update operation; JournalID the apply
	// journal entry.
	OperationID string `json:"operationId,omitempty"`
	JournalID   string `json:"journalId,omitempty"`
	// ExecutedBy is the agent runtime that executed the update (ADR 0024).
	ExecutedBy agentrt.Runtime `json:"executedBy,omitempty"`
	Error      string          `json:"error,omitempty"`
	Rollback   *RollbackRecord `json:"rollback,omitempty"`
}

func (p Plan) computeDigest() string {
	b, _ := json.Marshal(struct {
		ID, ServiceID, Type, Source, Reference, Route string
		Kind                                          Kind
		From, To                                      Image
		Strategy                                      Strategy
	}{p.ID, p.ServiceID, p.ServiceType, p.Source, p.Reference, p.Route, p.Kind, p.From, p.To, p.Strategy})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Baseline is the recorded digest of the tag a service follows.
type Baseline struct {
	Tag    string    `json:"tag"`
	Digest string    `json:"digest"`
	At     time.Time `json:"at"`
	PlanID string    `json:"planId,omitempty"`
}

// ServiceState is the bookkeeping of one service.
type ServiceState struct {
	ServiceID   string    `json:"serviceId"`
	Baseline    *Baseline `json:"baseline,omitempty"`
	LastCheckAt time.Time `json:"lastCheckAt,omitzero"`
	LastError   string    `json:"lastError,omitempty"`
}

var secretRe = regexp.MustCompile(`(?i)((authorization["']?\s*[:=]\s*)?(bearer|basic)\s+\S+|(password|passwd|token|secret|authorization)["']?\s*[:=]\s*\S+)`)

// redact shortens text taken from a registry or an agent and removes
// credential-looking parts before it is stored, logged or notified.
func redact(s string) string {
	s = secretRe.ReplaceAllString(s, "[redacted]")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
