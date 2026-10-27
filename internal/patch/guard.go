// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"fmt"
)

// Action is what the guard is asked about.
type Action string

// Guarded actions.
const (
	ActionApply  Action = "apply"
	ActionReboot Action = "reboot"
)

// ErrGaleraBlocked means the host is (or may be) a Galera member and no
// rolling procedure coordinates the action.
var ErrGaleraBlocked = errors.New("patch: blocked: host is a Galera member and the cluster planner does not coordinate this action")

// Guard decides whether an action on a host is currently allowed.
type Guard interface {
	Check(ctx context.Context, hostID string, a Action, coordination string) error
}

// GaleraDirectory tells which Galera cluster a host belongs to.
type GaleraDirectory interface {
	// Member returns the cluster ID if the host runs a member of a Galera
	// cluster. An error means the membership could not be determined.
	Member(ctx context.Context, hostID string) (clusterID string, member bool, err error)
}

// Coordinator is implemented by the cluster planner's rolling procedure. It
// confirms that coordination ID currently permits the action on the host:
// the planner has taken the member out of rotation, quorum is preserved and
// no other member is down.
type Coordinator interface {
	Permits(ctx context.Context, coordination, clusterID, hostID string, a Action) (bool, error)
}

// GaleraGuard blocks apply and reboot on a Galera member unless the planner
// coordinates it. It fails closed: an unknown membership blocks.
type GaleraGuard struct {
	Directory   GaleraDirectory
	Coordinator Coordinator
}

// Check implements Guard.
func (g GaleraGuard) Check(ctx context.Context, hostID string, a Action, coordination string) error {
	if g.Directory == nil {
		return fmt.Errorf("%w: no membership directory", ErrGaleraBlocked)
	}
	cluster, member, err := g.Directory.Member(ctx, hostID)
	if err != nil {
		return fmt.Errorf("%w: membership unknown: %v", ErrGaleraBlocked, err)
	}
	if !member {
		return nil
	}
	if coordination == "" || g.Coordinator == nil {
		return fmt.Errorf("%w (cluster %s)", ErrGaleraBlocked, cluster)
	}
	ok, err := g.Coordinator.Permits(ctx, coordination, cluster, hostID, a)
	if err != nil || !ok {
		return fmt.Errorf("%w (cluster %s, coordination %s)", ErrGaleraBlocked, cluster, coordination)
	}
	return nil
}

// AllowAllGuard permits everything. Only for tests of unrelated behavior.
type AllowAllGuard struct{}

// Check implements Guard.
func (AllowAllGuard) Check(context.Context, string, Action, string) error { return nil }
