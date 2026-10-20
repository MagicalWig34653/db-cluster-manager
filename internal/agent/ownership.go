// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"fmt"
	"sort"
)

// ErrNotOwned is returned when an object is outside the agent's ownership:
// its ID is not in the allow-list, its installation label differs, or its
// kind is not the one the command names.
var ErrNotOwned = errors.New("agent: object not owned by this installation")

// OwnershipGuard decides which Docker objects the executor may touch. Both
// conditions must hold: the object ID is in the allow-list, and the
// object's dbcm.installation label equals the configured installation. The
// allow-list is the agent's view of the manager's inventory (ADR 0004 item 1);
// labels alone are not trusted. OwnershipGuard is not safe for concurrent
// use; the Executor serializes access.
type OwnershipGuard struct {
	installation string
	allowed      map[string]struct{}
}

// NewOwnershipGuard returns a guard for installation with the given IDs.
func NewOwnershipGuard(installation string, ids []string) (*OwnershipGuard, error) {
	if installation == "" {
		return nil, errors.New("agent: installation ID required")
	}
	g := &OwnershipGuard{installation: installation, allowed: map[string]struct{}{}}
	for _, id := range ids {
		g.Allow(id)
	}
	return g, nil
}

// Allow adds id to the allow-list. Empty IDs are ignored.
func (g *OwnershipGuard) Allow(id string) {
	if id != "" {
		g.allowed[id] = struct{}{}
	}
}

// Forget removes id from the allow-list.
func (g *OwnershipGuard) Forget(id string) { delete(g.allowed, id) }

// Allowed reports whether id is in the allow-list. It is checked before any
// Docker call so unrelated IDs are refused without even an inspect.
func (g *OwnershipGuard) Allowed(id string) bool {
	_, ok := g.allowed[id]
	return ok
}

// IDs returns the allow-list, sorted.
func (g *OwnershipGuard) IDs() []string {
	out := make([]string, 0, len(g.allowed))
	for id := range g.allowed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Check returns nil if obj may be touched as an object of kind want.
func (g *OwnershipGuard) Check(obj Object, want ObjectKind) error {
	if !g.Allowed(obj.ID) {
		return fmt.Errorf("%w: %s %q not in allow-list", ErrNotOwned, obj.Kind, obj.ID)
	}
	if obj.Kind != want {
		return fmt.Errorf("%w: %q is a %s, not a %s", ErrNotOwned, obj.ID, obj.Kind, want)
	}
	if got := obj.Labels[LabelInstallation]; got != g.installation {
		return fmt.Errorf("%w: %s %q has %s=%q, want %q", ErrNotOwned, obj.Kind, obj.ID, LabelInstallation, got, g.installation)
	}
	return nil
}
