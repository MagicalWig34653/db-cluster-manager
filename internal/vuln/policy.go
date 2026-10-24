// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Mode is the policy mode.
type Mode string

const (
	// ModeNotifyOnly (default): findings and proposals are shown and
	// notified; nothing can be approved or executed.
	ModeNotifyOnly Mode = "notify_only"
	// ModeApprovalRequired: proposals wait for an admin's digest-bound
	// approval.
	ModeApprovalRequired Mode = "approval_required"
	// ModeAutoForPolicy: proposals that pass every rule of the allow-list
	// are approved automatically; all others wait for approval. Explicit
	// opt-in only.
	ModeAutoForPolicy Mode = "auto_for_policy"
)

// AutoAllow bounds auto-approval. Every list must be non-empty in
// auto_for_policy mode; a proposal qualifies only if its severity, every
// affected service and its kind are listed.
type AutoAllow struct {
	Severities []Severity        `json:"severities"`
	Services   []string          `json:"services"`
	Kinds      []RemediationKind `json:"kinds"`
	// MaxPerDay limits automatic approvals per UTC day (1..20).
	MaxPerDay int `json:"maxPerDay"`
}

// Policy is the vulnerability response policy.
type Policy struct {
	Mode Mode `json:"mode"`
	// Provider and Model select the AI provider for analyses; empty
	// Provider disables AI (deterministic proposals only).
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	Auto     AutoAllow `json:"auto"`
	// AutoOptInBy records who enabled auto_for_policy (required then).
	AutoOptInBy string `json:"autoOptInBy,omitempty"`
	// KillSwitch stops every automatic approval immediately.
	KillSwitch bool `json:"killSwitch"`
	// MitigationDays is the lifetime of a mitigation (default 30, 1..90).
	MitigationDays int `json:"mitigationDays,omitempty"`
	// RetainPrompts keeps the redacted prompt with the proposal; off by
	// default, only hashes are kept.
	RetainPrompts bool      `json:"retainPrompts,omitempty"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt,omitzero"`
}

// DefaultPolicy is notify-only without AI.
func DefaultPolicy() Policy { return Policy{Mode: ModeNotifyOnly, MitigationDays: 30} }

// ErrInvalidPolicy is returned for a malformed policy.
var ErrInvalidPolicy = errors.New("vuln: invalid policy")

// ErrPolicyForbids is returned when the policy mode does not allow the
// requested action (for example approval in notify-only mode).
var ErrPolicyForbids = errors.New("vuln: the policy does not allow this action")

// Validate checks the policy.
func (p Policy) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidPolicy, msg) }
	switch p.Mode {
	case ModeNotifyOnly, ModeApprovalRequired:
	case ModeAutoForPolicy:
		a := p.Auto
		switch {
		case p.AutoOptInBy == "":
			return bad("auto_for_policy needs an explicit opt-in")
		case len(a.Severities) == 0 || len(a.Services) == 0 || len(a.Kinds) == 0:
			return bad("auto_for_policy needs severities, services and kinds in the allow-list")
		case a.MaxPerDay < 1 || a.MaxPerDay > 20:
			return bad("maxPerDay must be 1..20")
		}
		for _, s := range a.Severities {
			if s.Rank() == 0 {
				return bad("unknown severity in the allow-list")
			}
		}
		for _, k := range a.Kinds {
			if k != KindPatchedRebuild && k != KindMitigation {
				return bad("only patched_rebuild and mitigation can be automatic")
			}
		}
		for _, s := range a.Services {
			if s == "" || s == "*" || len(s) > 128 {
				return bad("services must be named explicitly (no wildcard)")
			}
		}
	default:
		return bad("unknown mode")
	}
	if (p.Provider == "") != (p.Model == "") {
		return bad("provider and model go together")
	}
	if p.MitigationDays != 0 && (p.MitigationDays < 1 || p.MitigationDays > 90) {
		return bad("mitigationDays must be 1..90")
	}
	return nil
}

func (p Policy) mitigationDays() int {
	if p.MitigationDays == 0 {
		return 30
	}
	return p.MitigationDays
}

// Action is what the policy decides for a proposal.
type Action string

const (
	ActionNotify        Action = "notify"
	ActionAwaitApproval Action = "await_approval"
	ActionAutoApprove   Action = "auto_approve"
)

// Decision is the policy's decision with its reasons.
type Decision struct {
	Mode    Mode     `json:"mode"`
	Action  Action   `json:"action"`
	Reasons []string `json:"reasons,omitempty"`
}

// Decide applies the policy. autoToday is the number of automatic
// approvals already made today. The never-automatic rules of ADR 0015
// section 2 hold in every mode: database and Galera services, major
// upgrades, proxy and registry services, manual plans, no-op proposals and
// anything the validator rejected.
func (p Policy) Decide(pr Proposal, f Finding, autoToday int) Decision {
	d := Decision{Mode: p.Mode}
	switch p.Mode {
	case ModeApprovalRequired:
		d.Action = ActionAwaitApproval
		return d
	case ModeAutoForPolicy:
	default:
		d.Action = ActionNotify
		return d
	}
	var no []string
	if !pr.Validation.OK {
		no = append(no, "the validator rejected the proposal")
	}
	if f.Kind.Stateful() {
		no = append(no, "database and Galera services are never changed automatically")
	}
	if f.Kind == ServiceKindProxy || f.Kind == ServiceKindRegistry {
		no = append(no, "proxy and registry services are never changed automatically")
	}
	if pr.Plan.MajorUpgrade {
		no = append(no, "major version upgrades are never automatic")
	}
	if pr.Plan.Manual || pr.Kind == KindNoOp {
		no = append(no, "the plan has no automated step")
	}
	if p.KillSwitch {
		no = append(no, "the kill switch is on")
	}
	if !slices.Contains(p.Auto.Severities, f.Severity) {
		no = append(no, "severity is not in the auto allow-list")
	}
	if !slices.Contains(p.Auto.Kinds, pr.Kind) {
		no = append(no, "kind is not in the auto allow-list")
	}
	if len(f.Services) == 0 {
		no = append(no, "no named service")
	}
	for _, s := range f.Services {
		if !slices.Contains(p.Auto.Services, s) {
			no = append(no, fmt.Sprintf("service %q is not in the auto allow-list", s))
		}
	}
	if autoToday >= p.Auto.MaxPerDay {
		no = append(no, "the daily limit of automatic approvals is reached")
	}
	if len(no) > 0 {
		d.Action, d.Reasons = ActionAwaitApproval, no
		return d
	}
	d.Action = ActionAutoApprove
	d.Reasons = []string{"every auto allow-list rule passed"}
	return d
}
