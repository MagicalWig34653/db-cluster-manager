// SPDX-License-Identifier: AGPL-3.0-only
package metastore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/hashicorp/raft"
)

var ErrUnsafeMembership = errors.New("metastore: membership would risk quorum")
var ErrMembershipChanged = errors.New("metastore: membership configuration changed")

// MembershipView binds a maintenance request to one Raft configuration index.
type MembershipView struct {
	Index  uint64 `json:"index"`
	Voters []Peer `json:"voters"`
}

func (n *Node) Membership() (MembershipView, error) {
	f := n.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return MembershipView{}, err
	}
	index, err := n.configurationIndex(f.Configuration())
	if err != nil {
		return MembershipView{}, err
	}
	v := MembershipView{Index: index}
	for _, s := range f.Configuration().Servers {
		if s.Suffrage == raft.Voter {
			v.Voters = append(v.Voters, Peer{ID: string(s.ID), Address: string(s.Address)})
		}
	}
	return v, nil
}

// PeerReady must make a fresh, authenticated readiness observation; nil refuses
// every change. A successful observation is not a lease. Raft's own quorum and
// configuration-index checks still decide whether a change commits.
// requiredIndex=0 checks availability; otherwise the candidate must report
// that it has applied at least this leader index before it can vote.
type PeerReady func(context.Context, Peer, uint64) bool

// CheckMembership tests both the old and proposed quorum, the joining peer's
// readiness, duplicate addresses and leader removal. Removing the leader requires
// a separate leadership transfer first. A one-member group cannot be removed.
func (n *Node) CheckMembership(ctx context.Context, v MembershipView, p Peer, remove bool, ready PeerReady) error {
	if !n.IsLeader() {
		return ErrNotLeader
	}
	if ready == nil || p.ID == "" || p.Address == "" {
		return ErrUnsafeMembership
	}
	current, err := n.Membership()
	if err != nil {
		return err
	}
	if current.Index != v.Index {
		return ErrMembershipChanged
	}
	idx := slices.IndexFunc(current.Voters, func(m Peer) bool { return m.ID == p.ID })
	if remove {
		if idx < 0 || p.ID == n.ID() || len(current.Voters) <= 1 || current.Voters[idx].Address != p.Address {
			return ErrUnsafeMembership
		}
	} else {
		if idx >= 0 {
			return ErrUnsafeMembership
		}
		if slices.ContainsFunc(current.Voters, func(m Peer) bool { return m.Address == p.Address }) {
			return ErrUnsafeMembership
		}
	}
	oldLive, newLive := 0, 0
	for _, m := range current.Voters {
		live := m.ID == n.ID() || ready(ctx, m, 0)
		if live {
			oldLive++
			if !remove || m.ID != p.ID {
				newLive++
			}
		}
	}
	newSize := len(current.Voters) - 1
	if !remove {
		newSize = len(current.Voters) + 1
		required := uint64(0)
		config := n.raft.GetConfiguration()
		if err := config.Error(); err != nil {
			return err
		}
		for _, server := range config.Configuration().Servers {
			if string(server.ID) == p.ID && server.Suffrage == raft.Nonvoter {
				required = n.raft.AppliedIndex()
			}
		}
		if !ready(ctx, p, required) {
			return ErrUnsafeMembership
		}
		newLive++
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if oldLive < len(current.Voters)/2+1 || newLive < newSize/2+1 {
		return ErrUnsafeMembership
	}
	return n.Barrier(n.applyTimeout)
}

// ChangeMembership serializes single-server transitions and uses prevIndex so a
// concurrent or stale change cannot apply. Hashicorp Raft implements overlapping
// single-server configurations, not an exposed joint-configuration API. Never
// bypass its configuration log by editing the bootstrap list or FSM documents.
func (n *Node) ChangeMembership(ctx context.Context, v MembershipView, p Peer, remove bool, ready PeerReady) error {
	n.membershipMu.Lock()
	defer n.membershipMu.Unlock()
	if err := n.CheckMembership(ctx, v, p, remove, ready); err != nil {
		return err
	}
	if !remove {
		var err error
		v, err = n.stageReplica(v, p)
		if err != nil {
			return err
		}
		if err := n.CheckMembership(ctx, v, p, false, ready); err != nil {
			return err
		}
	}
	var f raft.IndexFuture
	if remove {
		f = n.raft.RemoveServer(raft.ServerID(p.ID), v.Index, n.applyTimeout)
	} else {
		f = n.raft.AddVoter(raft.ServerID(p.ID), raft.ServerAddress(p.Address), v.Index, n.applyTimeout)
	}
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return ErrNotLeader
		}
		return fmt.Errorf("%w: membership: %v", ErrOutcomeUnknown, err)
	}
	return n.Barrier(n.applyTimeout)
}

// hashicorp/raft v1.8.0 GetConfiguration omits latestIndex (it always returns
// zero). Read the durable configuration log or snapshot instead; never pass zero
// as prevIndex, since that would silently disable Raft's stale-change guard.
func (n *Node) configurationIndex(config raft.Configuration) (uint64, error) {
	logs := n.st.logStore()
	first, err := logs.FirstIndex()
	if err != nil {
		return 0, err
	}
	last, err := logs.LastIndex()
	if err != nil {
		return 0, err
	}
	for i := last; i >= first && i > 0; i-- {
		var entry raft.Log
		if err := logs.GetLog(i, &entry); err != nil {
			return 0, err
		}
		if entry.Type == raft.LogConfiguration {
			if !reflect.DeepEqual(raft.DecodeConfiguration(entry.Data), config) {
				return 0, ErrMembershipChanged
			}
			return i, nil
		}
	}
	snapshots, err := n.st.snapshotStore().List()
	if err != nil {
		return 0, err
	}
	for _, snapshot := range snapshots {
		if snapshot.ConfigurationIndex > 0 && reflect.DeepEqual(snapshot.Configuration, config) {
			return snapshot.ConfigurationIndex, nil
		}
	}
	return 0, fmt.Errorf("%w: no durable configuration index", ErrMembershipChanged)
}

// stageReplica catches up a nonvoter before promotion. It is idempotent across
// leader changes and never changes the voting quorum on its own.
func (n *Node) stageReplica(v MembershipView, p Peer) (MembershipView, error) {
	config := n.raft.GetConfiguration()
	if err := config.Error(); err != nil {
		return v, err
	}
	for _, server := range config.Configuration().Servers {
		if string(server.ID) == p.ID {
			if server.Suffrage != raft.Nonvoter || string(server.Address) != p.Address {
				return v, ErrUnsafeMembership
			}
			return v, nil
		}
		if string(server.Address) == p.Address {
			return v, ErrUnsafeMembership
		}
	}
	if err := n.raft.AddNonvoter(raft.ServerID(p.ID), raft.ServerAddress(p.Address), v.Index, n.applyTimeout).Error(); err != nil {
		return v, fmt.Errorf("%w: staging: %v", ErrOutcomeUnknown, err)
	}
	if err := n.Barrier(n.applyTimeout); err != nil {
		return v, err
	}
	return n.Membership()
}

// AppliedIndex is the local applied Raft index. It is used by fake readiness
// adapters; a production peer probe must authenticate the remote observation.
func (n *Node) AppliedIndex() uint64 { return n.raft.AppliedIndex() }
