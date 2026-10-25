// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
)

// RemediationKind is the kind of change a proposal makes.
type RemediationKind string

const (
	// KindPatchedRebuild rebuilds the image so the component reaches a
	// fixed version.
	KindPatchedRebuild RemediationKind = "patched_rebuild"
	// KindMitigation changes an allow-listed setting to reduce the attack
	// surface while no patch is available; it is time-limited.
	KindMitigation RemediationKind = "mitigation"
	// KindNoOp records that no change is made (no safe fix, or the finding
	// is assessed as not exploitable here).
	KindNoOp RemediationKind = "no_op"
)

// Origin says who produced a proposal.
type Origin string

const (
	OriginDeterministic Origin = "deterministic"
	OriginAI            Origin = "ai"
)

// ProposalStatus is the lifecycle state of a proposal.
type ProposalStatus string

const (
	// StatusProposed: shown and notified; in notify-only mode it cannot be
	// approved.
	StatusProposed ProposalStatus = "proposed"
	// StatusAwaitingApproval: waiting for an admin's digest-bound approval.
	StatusAwaitingApproval ProposalStatus = "awaiting_approval"
	// StatusInvalid: the validator rejected the proposal (for example an
	// AI answer outside the allow-list); it can never be approved.
	StatusInvalid ProposalStatus = "invalid"
	// StatusApproved: approved; Execution says what was started.
	StatusApproved ProposalStatus = "approved"
	// StatusDeclined: an admin declined it.
	StatusDeclined ProposalStatus = "declined"
	// StatusFailed: execution failed after approval.
	StatusFailed ProposalStatus = "failed"
)

// Level is a benefit or risk rating.
type Level string

const (
	LevelNone   Level = "none"
	LevelLow    Level = "low"
	LevelMedium Level = "medium"
	LevelHigh   Level = "high"
)

// Rating is the deterministic benefit/risk rating of a proposal with its
// basis. An AI's own assessment is kept separately in AIAssessment and
// does not change Rating.
type Rating struct {
	Benefit Level  `json:"benefit"`
	Risk    Level  `json:"risk"`
	Basis   string `json:"basis"`
}

// AIAssessment is what the model said, display-only.
type AIAssessment struct {
	Affected string `json:"affected"`
	Benefit  string `json:"benefit"`
	Risk     string `json:"risk"`
}

// MitigationChange is a typed setting change.
type MitigationChange struct {
	Setting   string    `json:"setting"`
	Value     string    `json:"value"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Plan is the approvable plan of a proposal. Approval binds to Digest.
// Build is set when the plan rebuilds a manager-built image; Manual plans
// have no automated step and approval only records the decision.
type Plan struct {
	Digest          string              `json:"digest"`
	Manual          bool                `json:"manual"`
	Build           *imagebuild.Request `json:"build,omitempty"`
	BuildPlanDigest string              `json:"buildPlanDigest,omitempty"`
	Steps           []imagebuild.Step   `json:"steps"`
	Warnings        []string            `json:"warnings"`
	MajorUpgrade    bool                `json:"majorUpgrade,omitempty"`
}

// Validation is the validator's verdict.
type Validation struct {
	OK     bool     `json:"ok"`
	Errors []string `json:"errors,omitempty"`
}

// Proposal is a remediation proposal for one finding.
type Proposal struct {
	ID         string          `json:"id"`
	FindingID  string          `json:"findingId"`
	AdvisoryID string          `json:"advisoryId"`
	EntryID    string          `json:"entryId"`
	Kind       RemediationKind `json:"kind"`
	Origin     Origin          `json:"origin"`
	// Provider, Model and the hashes are set for AI proposals.
	Provider       string            `json:"provider,omitempty"`
	Model          string            `json:"model,omitempty"`
	PromptSHA256   string            `json:"promptSha256,omitempty"`
	ResponseSHA256 string            `json:"responseSha256,omitempty"`
	Component      Component         `json:"component"`
	TargetVersion  string            `json:"targetVersion,omitempty"`
	Mitigation     *MitigationChange `json:"mitigation,omitempty"`
	Rating         Rating            `json:"rating"`
	AIAssessment   *AIAssessment     `json:"aiAssessment,omitempty"`
	// Rationale is display-only text (from the model or the deterministic
	// proposer); it never reaches a recipe or a command.
	Rationale  string         `json:"rationale,omitempty"`
	Plan       Plan           `json:"plan"`
	Validation Validation     `json:"validation"`
	Decision   Decision       `json:"decision"`
	Status     ProposalStatus `json:"status"`
	CreatedBy  string         `json:"createdBy"`
	CreatedAt  time.Time      `json:"createdAt"`
	DecidedBy  string         `json:"decidedBy,omitempty"`
	DecidedAt  time.Time      `json:"decidedAt,omitzero"`
	// ExecutionID is the durable idempotency key committed before any build.
	ExecutionID string `json:"executionId,omitempty"`
	// Execution references what approval started (an image build job ID)
	// or explains why nothing was started.
	Execution string `json:"execution,omitempty"`
}

// change is the typed change a proposal makes, before rating and plan.
type change struct {
	Kind          RemediationKind
	Component     Component
	TargetVersion string
	Mitigation    *MitigationChange
}

// ErrValidation wraps validator rejections.
var ErrValidation = errors.New("vuln: proposal rejected by the validator")

// MitigationSettings are the settings a mitigation may change: the
// php.ini allow-list of the image build service, so a mitigation passes
// exactly the validators of a manual change.
func MitigationSettings() []string { return imagebuild.IniSettings() }

// validateChange checks a typed change against the finding it claims to
// fix: the component must be the finding's component; a target version
// must be newer than the installed one and outside every affected range of
// the advisory; a mitigation must use an allow-listed setting of a
// manager-built PHP image. It returns the reasons for rejection.
func validateChange(c change, f Finding, adv Advisory, e InventoryEntry) []string {
	var errs []string
	if NormalizePackage(c.Component.Name) != f.Component.Name {
		errs = append(errs, "the change targets a component that is not the finding's component")
	}
	switch c.Kind {
	case KindPatchedRebuild:
		eco := f.Component.Ecosystem
		if CompareIn(eco, c.TargetVersion, f.Component.Version) <= 0 {
			errs = append(errs, "the target version is not newer than the installed version")
		}
		for _, af := range adv.Affect {
			if af.Ecosystem == eco && af.Package == f.Component.Name && af.Affects(c.TargetVersion) {
				errs = append(errs, "the target version is still affected according to the advisory")
				break
			}
		}
		if len(f.FixedVersions) == 0 {
			errs = append(errs, "the advisory states no fixed version, so a target version cannot be verified")
		}
	case KindMitigation:
		if c.Mitigation == nil {
			errs = append(errs, "a mitigation needs a setting")
			break
		}
		if !slices.Contains(MitigationSettings(), c.Mitigation.Setting) {
			errs = append(errs, fmt.Sprintf("setting %q is not in the mitigation allow-list", c.Mitigation.Setting))
		}
		if e.Build == nil || e.Build.Spec.PHP.Version == "" {
			errs = append(errs, "a setting mitigation needs a manager-built PHP image (typed composition)")
		}
	case KindNoOp:
	default:
		errs = append(errs, "unknown remediation kind")
	}
	return errs
}

// rate computes the deterministic rating.
func rate(c change, f Finding, plan Plan) Rating {
	r := Rating{}
	switch c.Kind {
	case KindPatchedRebuild:
		r.Benefit = map[Priority]Level{P1: LevelHigh, P2: LevelHigh, P3: LevelMedium, P4: LevelLow}[f.Priority]
		r.Risk = LevelLow
		if plan.MajorUpgrade {
			r.Risk = LevelHigh
		}
		r.Basis = "a fixed version removes the finding; risk from the version step"
	case KindMitigation:
		r.Benefit = map[Priority]Level{P1: LevelMedium, P2: LevelMedium, P3: LevelLow, P4: LevelLow}[f.Priority]
		r.Risk = LevelMedium
		r.Basis = "a mitigation reduces exposure but leaves the vulnerable code in place; a setting change can affect applications"
	default:
		r.Benefit, r.Risk = LevelNone, LevelLow
		r.Basis = "no change is made; the finding stays open"
	}
	if f.Kind.Stateful() && r.Risk != LevelHigh {
		r.Risk = LevelHigh
		r.Basis += "; stateful service (database rules apply, never automatic)"
	}
	r.Basis = fmt.Sprintf("priority %s (%s): %s", f.Priority, f.Prioritization.Basis, r.Basis)
	return r
}

// buildPlan renders the plan of a valid change. It is a pure function of
// the change, the finding and the inventory entry, so approval can
// recompute it and detect a changed state.
func buildPlan(c change, f Finding, e InventoryEntry, cat *imagebuild.Catalog) (Plan, error) {
	p := Plan{Warnings: []string{}}
	switch c.Kind {
	case KindPatchedRebuild:
		p.MajorUpgrade = Major(c.TargetVersion) != Major(f.Component.Version)
		if p.MajorUpgrade {
			p.Warnings = append(p.Warnings, "Major version upgrade: never applied automatically.")
		}
		if e.Build == nil {
			p.Manual = true
			p.Steps = []imagebuild.Step{
				{Order: 1, Title: "Update the image", Detail: fmt.Sprintf("The manager did not build %s; update it to a release containing %s %s (manual).", e.Image, c.Component.Name, c.TargetVersion)},
				{Order: 2, Title: "Verify", Detail: "The finding closes when the inventory no longer matches the advisory."},
			}
			break
		}
		req := imagebuild.Request{Spec: e.Build.Spec, Target: rebuildTarget(e.Build.Target, f.ID)}
		bp, err := imagebuild.NewPlan(req.Spec, req.Target, cat)
		if err != nil {
			return Plan{}, err
		}
		p.Build, p.BuildPlanDigest = &req, bp.Digest
		p.Steps = append(p.Steps, bp.Steps...)
		p.Steps = append(p.Steps, imagebuild.Step{Order: len(bp.Steps) + 1, Title: "Verify the fix",
			Detail: fmt.Sprintf("Rescan the built image; the finding closes when %s is at %s or newer. Rollout is not automated yet.", c.Component.Name, c.TargetVersion)})
		p.Warnings = append(p.Warnings, bp.Warnings...)
		p.Warnings = append(p.Warnings,
			"The rebuild takes package versions from the distribution repository at build time; whether it reaches the fixed version is verified only by rescanning, which is not implemented yet.",
			"If the vulnerable component comes from the base image, a new base image digest is needed; plan that build manually.")
	case KindMitigation:
		spec := e.Build.Spec
		spec.PHP.INI = maps.Clone(spec.PHP.INI)
		if spec.PHP.INI == nil {
			spec.PHP.INI = map[string]string{}
		}
		spec.PHP.INI[c.Mitigation.Setting] = c.Mitigation.Value
		req := imagebuild.Request{Spec: spec, Target: rebuildTarget(e.Build.Target, f.ID)}
		bp, err := imagebuild.NewPlan(req.Spec, req.Target, cat)
		if err != nil {
			// The same validators as a manual change rejected the value.
			return Plan{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
		p.Build, p.BuildPlanDigest = &req, bp.Digest
		p.Steps = append(p.Steps, bp.Steps...)
		p.Warnings = append(p.Warnings, bp.Warnings...)
		p.Warnings = append(p.Warnings, fmt.Sprintf("Mitigation expires at %s and re-alerts before then; it is proposed for removal when a patch lands.", c.Mitigation.ExpiresAt.UTC().Format(time.RFC3339)))
	default:
		p.Manual = true
		p.Steps = []imagebuild.Step{{Order: 1, Title: "Record the decision", Detail: "No change is made. The finding stays open and is re-evaluated on every poll."}}
	}
	sum := sha256.New()
	enc, _ := json.Marshal(struct {
		V      string
		F      string
		Kind   RemediationKind
		C      Component
		T      string
		M      *MitigationChange
		BPD    string
		Manual bool
	}{"dbcm-vuln-plan-v1", f.ID, c.Kind, c.Component, c.TargetVersion, c.Mitigation, p.BuildPlanDigest, p.Manual})
	sum.Write(enc)
	p.Digest = "sha256:" + hex.EncodeToString(sum.Sum(nil))
	return p, nil
}

// rebuildTarget derives a new tag so the rebuilt image never overwrites
// the running one.
func rebuildTarget(t imagebuild.Target, findingID string) imagebuild.Target {
	suffix := "-" + strings.TrimPrefix(findingID, "vf-")[:8]
	tag := t.Tag
	if len(tag)+len(suffix) > 128 {
		tag = tag[:128-len(suffix)]
	}
	t.Tag = tag + suffix
	return t
}

// deterministicChange proposes without AI: the lowest fixed version newer
// than the installed one (preferring the same major version), or no-op.
func deterministicChange(f Finding) (change, string) {
	var sameMajor, any string
	for _, v := range f.FixedVersions {
		if CompareIn(f.Component.Ecosystem, v, f.Component.Version) <= 0 {
			continue
		}
		if any == "" {
			any = v
		}
		if sameMajor == "" && Major(v) == Major(f.Component.Version) {
			sameMajor = v
		}
	}
	target := sameMajor
	if target == "" {
		target = any
	}
	if target == "" {
		return change{Kind: KindNoOp, Component: f.Component},
			"The advisory states no fixed version newer than the installed one. Review mitigations manually or request an AI analysis."
	}
	return change{Kind: KindPatchedRebuild, Component: f.Component, TargetVersion: target},
		fmt.Sprintf("The advisory states %s as fixed version of %s.", target, f.Component.Name)
}

// aiChange converts a parsed AI answer into a typed change.
func aiChange(p ai.Proposal, f Finding, mitigationDays int, now time.Time) change {
	c := change{Kind: RemediationKind(p.Kind), Component: Component{Ecosystem: f.Component.Ecosystem, Name: NormalizePackage(p.Component), Version: f.Component.Version}}
	switch c.Kind {
	case KindPatchedRebuild:
		c.TargetVersion = p.TargetVersion
	case KindMitigation:
		c.Mitigation = &MitigationChange{Setting: p.Mitigation.Setting, Value: p.Mitigation.Value,
			ExpiresAt: now.Add(time.Duration(mitigationDays) * 24 * time.Hour).UTC()}
	}
	return c
}
