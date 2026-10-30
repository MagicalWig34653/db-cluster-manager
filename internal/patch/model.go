// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"errors"
	"sort"
	"time"
)

// Manager names a package manager.
type Manager string

// Supported package managers.
const (
	APT    Manager = "apt"
	DNF    Manager = "dnf"
	YUM    Manager = "yum"
	Zypper Manager = "zypper"
	APK    Manager = "apk"
	Pacman Manager = "pacman"
)

// Tristate is yes, no or unknown; unknown is never reported as no.
type Tristate string

// Tristate values.
const (
	Yes     Tristate = "yes"
	No      Tristate = "no"
	Unknown Tristate = "unknown"
)

// Errors.
var (
	ErrInvalid            = errors.New("patch: invalid request")
	ErrNotFound           = errors.New("patch: not found")
	ErrNoManager          = errors.New("patch: no supported package manager found")
	ErrPartialUnsupported = errors.New("patch: package manager does not support partial upgrades")
	ErrConflict           = errors.New("patch: conflict")
	ErrDigest             = errors.New("patch: plan digest mismatch")
)

// Package is one installed package.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch,omitempty"`
}

// Update is one available update. Security is true only when the package
// manager or its repository metadata says so; an unknown classification is
// false with the report-level SecurityKnown note, never "safe".
type Update struct {
	Name      string `json:"name"`
	Installed string `json:"installed,omitempty"`
	Available string `json:"available"`
	Arch      string `json:"arch,omitempty"`
	Source    string `json:"source,omitempty"` // repository or suite
	Security  bool   `json:"security,omitempty"`
	// Class is set by the manager at ingest (os or container-runtime).
	Class      Class    `json:"class,omitempty"`
	Advisories []string `json:"advisories,omitempty"` // CVE, RHSA, DSA ... identifiers
}

// OSInfo identifies the operating system.
type OSInfo struct {
	ID         string `json:"id"`
	VersionID  string `json:"versionId,omitempty"`
	PrettyName string `json:"prettyName,omitempty"`
	Kernel     string `json:"kernel,omitempty"`
	Arch       string `json:"arch,omitempty"`
}

// HostReport is what the agent reports. It contains no secrets.
type HostReport struct {
	HostID         string    `json:"hostId"`
	CollectedAt    time.Time `json:"collectedAt"`
	OS             OSInfo    `json:"os"`
	Manager        Manager   `json:"manager"`
	Installed      []Package `json:"installed"`
	Updates        []Update  `json:"updates"`
	RebootRequired Tristate  `json:"rebootRequired"`
	// RebootReasons names packages that asked for a reboot, when known.
	RebootReasons []string `json:"rebootReasons,omitempty"`
	// SecurityKnown says whether the manager classifies security updates.
	SecurityKnown bool `json:"securityKnown"`
	// Warnings are non-fatal collection problems (a missing helper tool).
	Warnings []string `json:"warnings,omitempty"`
}

// SecurityUpdates returns the updates classified as security.
func (r HostReport) SecurityUpdates() []Update {
	var out []Update
	for _, u := range r.Updates {
		if u.Security {
			out = append(out, u)
		}
	}
	return out
}

func sortUpdates(u []Update) {
	sort.Slice(u, func(i, j int) bool { return u[i].Name < u[j].Name })
}

func sortPackages(p []Package) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].Name != p[j].Name {
			return p[i].Name < p[j].Name
		}
		return p[i].Arch < p[j].Arch
	})
}
