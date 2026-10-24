// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Status is the VEX-like status of a finding.
type Status string

const (
	// StatusAffected: the deterministic matcher found the component in an
	// affected range. New findings start here.
	StatusAffected Status = "affected"
	// StatusUnderInvestigation: an admin is checking the finding.
	StatusUnderInvestigation Status = "under_investigation"
	// StatusNotAffected: an admin decided the component is not affected
	// (Justification required). The finding does not re-alert for the same
	// component and version.
	StatusNotAffected Status = "not_affected"
	// StatusFixed: the component no longer matches (an upgraded image was
	// inventoried) or an admin marked it fixed.
	StatusFixed Status = "fixed"
)

// Valid reports whether s is a defined status.
func (s Status) Valid() bool {
	switch s {
	case StatusAffected, StatusUnderInvestigation, StatusNotAffected, StatusFixed:
		return true
	}
	return false
}

// Open reports whether the finding still needs attention.
func (s Status) Open() bool { return s == StatusAffected || s == StatusUnderInvestigation }

// Priority is the coarse priority class derived from Score.
type Priority string

const (
	P1 Priority = "P1"
	P2 Priority = "P2"
	P3 Priority = "P3"
	P4 Priority = "P4"
)

// Prioritization is the computed priority of a finding with its inputs.
type Prioritization struct {
	Score    int      `json:"score"`
	Priority Priority `json:"priority"`
	Basis    string   `json:"basis"`
}

// Prioritize computes a 0..100 score from severity, a known-exploited
// listing (KEV), the EPSS probability and whether the affected services are
// stateful. The weights are a proposal; they are deterministic and shown
// with every finding.
func Prioritize(sev Severity, cvss float64, kev bool, epss float64, stateful bool) Prioritization {
	base := map[Severity]int{SeverityCritical: 70, SeverityHigh: 50, SeverityMedium: 30, SeverityLow: 10}[sev]
	if sev == SeverityUnknown || sev == "" {
		base = 20
		if cvss > 0 {
			base = map[Severity]int{SeverityCritical: 70, SeverityHigh: 50, SeverityMedium: 30, SeverityLow: 10}[SeverityFromCVSS(cvss)]
		}
	}
	score := base
	parts := []string{fmt.Sprintf("severity %s=%d", sev, base)}
	if kev {
		score += 20
		parts = append(parts, "known exploited +20")
	}
	if epss > 0 {
		e := int(math.Round(epss * 10))
		score += e
		parts = append(parts, fmt.Sprintf("EPSS %.2f +%d", epss, e))
	}
	if stateful {
		score += 5
		parts = append(parts, "stateful service +5")
	}
	score = min(score, 100)
	p := P4
	switch {
	case score >= 80:
		p = P1
	case score >= 55:
		p = P2
	case score >= 30:
		p = P3
	}
	return Prioritization{Score: score, Priority: p, Basis: strings.Join(parts, ", ")}
}

// Finding is one advisory matched against one component of one inventory
// entry.
type Finding struct {
	ID         string      `json:"id"`
	AdvisoryID string      `json:"advisoryId"`
	Aliases    []string    `json:"aliases,omitempty"`
	EntryID    string      `json:"entryId"`
	Image      string      `json:"image"`
	Services   []string    `json:"services"`
	Kind       ServiceKind `json:"serviceKind"`
	Coverage   Coverage    `json:"coverage"`
	Component  Component   `json:"component"`
	// FixedVersions are the fixed versions the advisory states (empty
	// when no patch is known).
	FixedVersions []string `json:"fixedVersions,omitempty"`
	Severity      Severity `json:"severity"`
	CVSS          float64  `json:"cvss,omitempty"`
	KEV           bool     `json:"kev,omitempty"`
	EPSS          float64  `json:"epss,omitempty"`
	Prioritization
	Status        Status    `json:"status"`
	Justification string    `json:"justification,omitempty"`
	StatusBy      string    `json:"statusBy,omitempty"`
	FirstSeen     time.Time `json:"firstSeen"`
	LastSeen      time.Time `json:"lastSeen"`
	ResolvedAt    time.Time `json:"resolvedAt,omitzero"`
	Sources       []string  `json:"sources"`
}

// FindingID is stable for (advisory, entry, component, version): an
// upgraded component yields a new finding, so a not_affected decision only
// covers the version it was made for.
func FindingID(advisoryID, entryID string, c Component) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"dbcm-vuln-finding-v1", advisoryID, entryID, c.Ecosystem, c.Name, c.Version}, "\n")))
	return "vf-" + hex.EncodeToString(sum[:10])
}

// ErrInvalidStatus is returned for a status change that is not allowed.
var ErrInvalidStatus = errors.New("vuln: invalid status change")

// MaxJustificationBytes caps a VEX justification.
const MaxJustificationBytes = 1024

// SetStatus applies an admin's VEX decision.
func (f *Finding) SetStatus(s Status, justification, actor string) error {
	justification = strings.TrimSpace(justification)
	switch {
	case !s.Valid():
		return fmt.Errorf("%w: unknown status %q", ErrInvalidStatus, s)
	case s == StatusNotAffected && justification == "":
		return fmt.Errorf("%w: not_affected needs a justification", ErrInvalidStatus)
	case len(justification) > MaxJustificationBytes:
		return fmt.Errorf("%w: justification too long", ErrInvalidStatus)
	}
	f.Status, f.Justification, f.StatusBy = s, justification, actor
	return nil
}
