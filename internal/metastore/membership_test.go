// SPDX-License-Identifier: AGPL-3.0-only
package metastore

import (
	"context"
	"errors"
	"github.com/hashicorp/raft"
	"testing"
)

func TestMembershipQuorumAndRemoval(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	follower := c.follower(leader)
	v, err := leader.Membership()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	all := func(context.Context, Peer, uint64) bool { return true }
	var peer Peer
	for _, p := range v.Voters {
		if p.ID == follower.ID() {
			peer = p
		}
	}
	// The other follower is unavailable: removing this healthy peer would
	// leave only one reachable voter in a two-member configuration.
	onlyRemoved := func(_ context.Context, p Peer, _ uint64) bool { return p.ID == peer.ID }
	if err := leader.ChangeMembership(ctx, v, peer, true, onlyRemoved); !errors.Is(err, ErrUnsafeMembership) {
		t.Fatalf("quorum risk: %v", err)
	}
	if err := leader.ChangeMembership(ctx, v, peer, true, nil); !errors.Is(err, ErrUnsafeMembership) {
		t.Fatal(err)
	}
	if err := follower.ChangeMembership(ctx, v, peer, true, all); !errors.Is(err, ErrNotLeader) {
		t.Fatal(err)
	}
	if err := leader.ChangeMembership(ctx, v, Peer{ID: leader.ID(), Address: leader.ID()}, true, all); !errors.Is(err, ErrUnsafeMembership) {
		t.Fatal(err)
	}
	if err := leader.ChangeMembership(ctx, v, peer, true, all); err != nil {
		t.Fatal(err)
	}
	now, err := leader.Membership()
	if err != nil || len(now.Voters) != 2 {
		t.Fatalf("membership: %+v %v", now, err)
	}
	if err := leader.ChangeMembership(ctx, v, peer, false, all); !errors.Is(err, ErrMembershipChanged) {
		t.Fatalf("stale index: %v", err)
	}
	// Re-add the running former voter using Raft's staging/catchup path.
	if err := leader.ChangeMembership(ctx, now, peer, false, all); err != nil {
		t.Fatal(err)
	}
	now, err = leader.Membership()
	if err != nil || len(now.Voters) != 3 {
		t.Fatalf("re-add: %+v %v", now, err)
	}
}

func TestMembershipConfigurationIndexSurvivesCompaction(t *testing.T) {
	storage := newMemStorage()
	n := &Node{st: storage}
	_, tr := raft.NewInmemTransport("test")
	cfg := raft.Configuration{Servers: []raft.Server{{ID: "one", Address: "one", Suffrage: raft.Voter}}}
	entry := &raft.Log{Index: 7, Term: 1, Type: raft.LogConfiguration, Data: raft.EncodeConfiguration(cfg)}
	if err := storage.logStore().StoreLog(entry); err != nil {
		t.Fatal(err)
	}
	if index, err := n.configurationIndex(cfg); err != nil || index != 7 {
		t.Fatalf("log: %d %v", index, err)
	}
	sink, err := storage.snapshotStore().Create(raft.SnapshotVersionMax, 8, 1, cfg, 7, tr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write([]byte("snapshot")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.logStore().DeleteRange(1, 8); err != nil {
		t.Fatal(err)
	}
	if index, err := n.configurationIndex(cfg); err != nil || index != 7 {
		t.Fatalf("snapshot: %d %v", index, err)
	}
	cfg = cfg.Clone()
	cfg.Servers[0].Address = "other"
	if _, err := n.configurationIndex(cfg); !errors.Is(err, ErrMembershipChanged) {
		t.Fatalf("mismatch: %v", err)
	}
}
