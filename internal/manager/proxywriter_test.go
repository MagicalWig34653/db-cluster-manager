// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"errors"
	"testing"
)

func TestProxyWriterFreezeRequiresEveryMember(t *testing.T) {
	group := newCluster(t, 3, nil)
	first := group.leader()
	coord := ProxyWriterStore{Store: first.Store(), Cluster: "c1", InitialWriter: "n1", Candidates: []string{"n1", "n2", "n3"}, Members: []string{"p1", "p2"}}
	initial, err := coord.Current()
	if err != nil || initial.Writer != "n1" || initial.Epoch != 1 {
		t.Fatalf("initial: %+v %v", initial, err)
	}
	freeze, err := coord.Switch("n2")
	if err != nil || freeze.Writer != "" || freeze.Pending != "n2" || freeze.Epoch != 2 {
		t.Fatalf("freeze: %+v %v", freeze, err)
	}
	if _, err := coord.Switch("n3"); !errors.Is(err, ErrWriterSwitchPending) {
		t.Fatalf("overlap: %v", err)
	}
	if _, err := coord.Ack("other", freeze.Epoch); err == nil {
		t.Fatal("unregistered proxy acknowledged")
	}
	one, err := coord.Ack("p1", freeze.Epoch)
	if err != nil || one.Writer != "" || !one.Acks["p1"] {
		t.Fatalf("first acknowledgement: %+v %v", one, err)
	}
	if _, err := coord.Ack("p1", freeze.Epoch); err != nil {
		t.Fatalf("idempotent acknowledgement: %v", err)
	}
	if _, err := coord.Ack("p2", freeze.Epoch-1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale epoch: %v", err)
	}
	still, err := coord.Current()
	if err != nil || still.Writer != "" {
		t.Fatalf("partition must remain frozen: %+v %v", still, err)
	}
	// A leader change keeps the freeze and the first acknowledgement.
	group.close(first)
	next := group.leader(first.cfg.NodeID)
	coord.Store = next.Store()
	still, err = coord.Current()
	if err != nil || still.Pending != "n2" || !still.Acks["p1"] {
		t.Fatalf("new leader: %+v %v", still, err)
	}
	active, err := coord.Ack("p2", freeze.Epoch)
	if err != nil || active.Writer != "n2" || active.Pending != "" {
		t.Fatalf("commit: %+v %v", active, err)
	}
	if _, err := coord.Ack("p2", freeze.Epoch); !errors.Is(err, ErrConflict) {
		t.Fatalf("late ack: %v", err)
	}
	if _, err := coord.Switch("n2"); err != nil {
		t.Fatalf("no-op switch: %v", err)
	}
}

func TestProxyWriterRejectsMembershipDrift(t *testing.T) {
	group := newCluster(t, 1, nil)
	coord := ProxyWriterStore{Store: group.leader().Store(), Cluster: "c", InitialWriter: "n1", Candidates: []string{"n1", "n2"}, Members: []string{"p1", "p2"}}
	if _, err := coord.Current(); err != nil {
		t.Fatal(err)
	}
	coord.Members = []string{"p1"}
	if _, err := coord.Current(); err == nil {
		t.Fatal("membership drift accepted")
	}
	coord.Members = []string{"p1", "p2"}
	coord.Candidates = []string{"n1", "n3"}
	if _, err := coord.Current(); err == nil {
		t.Fatal("candidate drift accepted")
	}
}
