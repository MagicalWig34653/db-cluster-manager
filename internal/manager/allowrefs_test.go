// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
)

// refConn lists networks and secrets per kind.
type refConn struct {
	memConn
	objs map[agent.ObjectKind][]agent.Object
}

func (r *refConn) ListByLabel(_ context.Context, k agent.ObjectKind, key, value string) ([]agent.Object, error) {
	var out []agent.Object
	for _, o := range r.objs[k] {
		if o.Labels[key] == value {
			out = append(out, o)
		}
	}
	return out, nil
}

// TestCreateClusterAllowsReferencedObjects: lab E4 found that the agents
// refused the first create-service because the operator-provided network
// was not in their allow-list. The operation now starts with one
// allow-objects step per host carrying the resolved IDs; an unknown (or
// unlabelled) reference is refused before anything starts.
func TestCreateClusterAllowsReferencedObjects(t *testing.T) {
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = newFakeAgents(); cfg.Checker = okChecker{} })
	l := c.leader()
	ops := seed(t, l, 3)
	inst := map[string]string{agent.LabelInstallation: "inst-1"}
	conn := &refConn{objs: map[agent.ObjectKind][]agent.Object{
		agent.KindNetwork: {{ID: "net-id-1", Name: "dbcm-shop-galera", Labels: inst}},
		agent.KindSecret:  {{ID: "sec-id-9", Name: "dbcm-shop-root-pw", Labels: map[string]string{agent.LabelInstallation: "other"}}},
	}}
	ops.Swarms = &Swarms{Dial: func(string) (swarm.Connector, error) { return conn, nil }}
	// Service commands need an enrolled Swarm manager node with an agent
	// identity (lab E4: workers cannot create services).
	if _, err := ops.CreateCluster(adminSubject, createReq()); !errors.Is(err, ErrNoServiceHost) {
		t.Fatalf("without a manager node: %v", err)
	}
	if _, _, err := Update(l.Store(), CollHosts, HostKey("sw1", "h2"), func(h HostRecord, _ bool) (HostRecord, error) {
		h.Role, h.AgentSerial = "manager", "0a"
		return h, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.CreateCluster(adminSubject, createReq()); !errors.Is(err, ErrUnknownReference) {
		t.Fatalf("secret of another installation accepted: %v", err)
	}
	if recs, _ := List[OperationRecord](l.Store(), CollOperations); len(recs) != 0 {
		t.Fatalf("operation recorded after refusal: %d", len(recs))
	}
	conn.objs[agent.KindSecret] = append(conn.objs[agent.KindSecret], agent.Object{ID: "sec-id-1", Name: "dbcm-shop-root-pw", Labels: inst})
	rec, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	steps := rec.Definition.Steps
	var hosts []string
	for _, s := range steps[:3] {
		var a agent.AllowObjectsArgs
		if s.Command != agent.KindAllowObjects || json.Unmarshal(s.Args, &a) != nil || !slices.Equal(a.IDs, []string{"net-id-1", "sec-id-1"}) {
			t.Fatalf("step %+v", s)
		}
		hosts = append(hosts, s.Target)
	}
	slices.Sort(hosts)
	if !slices.Equal(hosts, []string{"h1", "h2", "h3"}) || steps[3].Command == agent.KindAllowObjects {
		t.Fatalf("allow steps for %v, next %s", hosts, steps[3].Command)
	}
	for _, s := range steps {
		switch s.Command {
		case agent.KindCreateService, "update-service":
			if s.Target != "h2" {
				t.Errorf("service step %s on %s, want the manager node h2", s.ID, s.Target)
			}
		case agent.KindCreateVolume, agent.KindInitIdentity, agent.KindInitVolumeDirs:
			if s.Target == "" {
				t.Errorf("node step %s without host", s.ID)
			}
		}
	}
	if !errors.Is(MapCommandError(ErrUnknownReference), api.ErrInvalidInput) {
		t.Fatal("unknown reference not mapped to invalid input")
	}
}
