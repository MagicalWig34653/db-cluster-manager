// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"strings"
	"testing"

	"github.com/hashicorp/raft"
)

// TestBootstrapRefusesNodeOutsideItsPeers: lab E4 re-run B bootstrapped
// three replicas whose IDs were not in their peer lists; none ever led and
// nothing reported why. Bootstrap now refuses such a membership.
func TestBootstrapRefusesNodeOutsideItsPeers(t *testing.T) {
	peers := []Peer{{ID: "manager-1", Address: "a1"}, {ID: "manager-2", Address: "a2"}, {ID: "manager-3", Address: "a3"}}
	_, tr := raft.NewInmemTransport("a1")
	_, err := NewNode(Config{ID: "manager-192.168.104.11", Transport: tr, Bootstrap: true, Peers: peers})
	if err == nil || !strings.Contains(err.Error(), "not in the bootstrap peers") {
		t.Fatalf("got %v", err)
	}
	for _, bad := range [][]Peer{
		{{ID: "manager-1", Address: "a1"}, {ID: "manager-1", Address: "a2"}},
		{{ID: "manager-1", Address: "a1"}, {ID: "manager-2", Address: "a1"}},
		{{ID: "manager-1", Address: ""}},
	} {
		if err := checkBootstrapPeers("manager-1", bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := checkBootstrapPeers("manager-2", peers); err != nil {
		t.Fatal(err)
	}
}
