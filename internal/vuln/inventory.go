// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
)

// Coverage says how completely the components of an inventory entry are
// known. Claims of vulnerability coverage must state it: an entry with
// reference-only coverage is matched by image name only, and unknown
// entries are reported as unknown, never as safe.
type Coverage string

const (
	// CoverageSBOM: the component list comes from an SBOM recorded at build
	// time (the build service, X08) or imported for the image.
	CoverageSBOM Coverage = "sbom"
	// CoverageReference: only the image reference and tag are known
	// (images the manager did not build, X06); matching uses ecosystem
	// "docker" with the image name and tag.
	CoverageReference Coverage = "reference"
	// CoverageUnknown: nothing usable is known.
	CoverageUnknown Coverage = "unknown"
)

// ServiceKind classifies what runs from an image. Database and Galera
// images are never eligible for auto-apply (ADR 0015 section 2).
type ServiceKind string

const (
	ServiceKindWeb      ServiceKind = "web"
	ServiceKindGeneric  ServiceKind = "generic"
	ServiceKindDatabase ServiceKind = "database"
	ServiceKindGalera   ServiceKind = "galera"
	ServiceKindProxy    ServiceKind = "proxy"
	ServiceKindRegistry ServiceKind = "registry"
)

// Stateful reports whether the kind is a database or touches database
// quorum rules.
func (k ServiceKind) Stateful() bool { return k == ServiceKindDatabase || k == ServiceKindGalera }

// Component is one installed package of an image (an SBOM entry).
type Component struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	// PURL is the package URL where the SBOM provides one; informational.
	PURL string `json:"purl,omitempty"`
}

// BuildSource records how the manager built an image, so a remediation can
// be planned as a rebuild of the same composition.
type BuildSource struct {
	Spec   imagebuild.WebServerSpec `json:"spec"`
	Target imagebuild.Target        `json:"target"`
}

// InventoryEntry is one image the manager knows about and the services
// that run it.
type InventoryEntry struct {
	ID          string      `json:"id"`
	Image       string      `json:"image"`
	Digest      string      `json:"digest,omitempty"`
	Services    []string    `json:"services"`
	ServiceKind ServiceKind `json:"serviceKind"`
	Coverage    Coverage    `json:"coverage"`
	Components  []Component `json:"components,omitempty"`
	// Build is set for images built by the manager's build service.
	Build *BuildSource `json:"build,omitempty"`
}

// ErrInvalidInventory is returned for a malformed inventory entry.
var ErrInvalidInventory = errors.New("vuln: invalid inventory entry")

var entryIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Validate checks the entry's shape.
func (e InventoryEntry) Validate() error {
	switch {
	case !entryIDRe.MatchString(e.ID):
		return fmt.Errorf("%w: id", ErrInvalidInventory)
	case e.Image == "" || len(e.Image) > 512:
		return fmt.Errorf("%w: image", ErrInvalidInventory)
	case e.Coverage != CoverageSBOM && e.Coverage != CoverageReference && e.Coverage != CoverageUnknown:
		return fmt.Errorf("%w: coverage", ErrInvalidInventory)
	}
	for _, c := range e.Components {
		if c.Name == "" || c.Version == "" || c.Ecosystem == "" {
			return fmt.Errorf("%w: component needs ecosystem, name and version", ErrInvalidInventory)
		}
	}
	return nil
}

// InventorySource provides the current inventory. The manager's source is
// manager.VulnInventory (service, cluster and build records);
// StaticInventory is the fake.
type InventorySource interface {
	Inventory(ctx context.Context) ([]InventoryEntry, error)
}

// StaticInventory is an in-memory InventorySource.
type StaticInventory struct {
	mu      sync.Mutex
	Entries []InventoryEntry
}

// Inventory implements InventorySource.
func (s *StaticInventory) Inventory(context.Context) ([]InventoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]InventoryEntry, len(s.Entries))
	copy(out, s.Entries)
	return out, nil
}

// Set replaces the entries.
func (s *StaticInventory) Set(e []InventoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Entries = e
}

// components returns the matchable components of an entry. Reference-only
// entries contribute their image name and tag as a "docker" component.
func (e InventoryEntry) components() []Component {
	switch e.Coverage {
	case CoverageSBOM:
		return e.Components
	case CoverageReference:
		name, tag := splitImageRef(e.Image)
		if tag == "" {
			return nil
		}
		return []Component{{Ecosystem: "docker", Name: name, Version: tag}}
	}
	return nil
}

// splitImageRef splits "registry/repo/name:tag@digest" into a name without
// the registry host and a tag.
func splitImageRef(ref string) (name, tag string) {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndexByte(ref, '/')
	if i := strings.LastIndexByte(ref, ':'); i > slash {
		ref, tag = ref[:i], ref[i+1:]
	}
	// Drop a registry host (first component with a dot or colon, or
	// "localhost").
	if i := strings.IndexByte(ref, '/'); i > 0 {
		first := ref[:i]
		if first == "localhost" || strings.ContainsAny(first, ".:") {
			ref = ref[i+1:]
		}
	}
	if strings.HasPrefix(ref, "library/") {
		ref = strings.TrimPrefix(ref, "library/")
	}
	return NormalizePackage(ref), tag
}
