// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"fmt"
	"net/url"
	"sort"
)

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }

// DistributionMode says where images live.
type DistributionMode string

const (
	DistributionManaged  DistributionMode = "managed"
	DistributionExternal DistributionMode = "external"
)

// DistributionPolicy says which registry holds the images and which Swarms
// pull from it with which pull secret. It is data, not enforcement: the
// service framework must apply it when it deploys services.
type DistributionPolicy struct {
	Mode DistributionMode `json:"mode"`
	// Registry is the ID of the registry configuration.
	Registry string `json:"registry"`
	// PullSecrets maps a Swarm ID to the manager secret (reference only)
	// that Swarm's hosts use to pull. Every Swarm that may pull needs an
	// entry unless AnonymousPull is set.
	PullSecrets   map[string]string `json:"pullSecrets,omitempty"`
	AnonymousPull bool              `json:"anonymousPull,omitempty"`
	// AirGapped requires that no host reaches an external registry; only
	// the managed mode satisfies it.
	AirGapped bool `json:"airGapped,omitempty"`
}

// Validate checks the policy against the Swarms it should cover and the kind
// of the chosen registry.
func (p DistributionPolicy) Validate(kind Kind, swarms []string) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(f, a...)) }
	switch p.Mode {
	case DistributionManaged:
		if kind != KindManaged {
			return bad("mode managed needs a managed registry, got %q", kind)
		}
	case DistributionExternal:
		if kind == KindManaged {
			return bad("mode external needs an external registry")
		}
		if p.AirGapped {
			return bad("air-gapped operation needs the managed registry")
		}
	default:
		return bad("mode must be managed or external")
	}
	if p.Registry == "" {
		return bad("registry is required")
	}
	if !p.AnonymousPull {
		var missing []string
		for _, s := range swarms {
			if p.PullSecrets[s] == "" {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return bad("no pull secret for Swarm(s) %v", missing)
		}
	}
	return nil
}

// PullSecretFor returns the pull secret reference of a Swarm, or "" with
// ok=true when pulls are anonymous.
func (p DistributionPolicy) PullSecretFor(swarm string) (ref string, ok bool) {
	if p.AnonymousPull {
		return "", true
	}
	ref = p.PullSecrets[swarm]
	return ref, ref != ""
}
