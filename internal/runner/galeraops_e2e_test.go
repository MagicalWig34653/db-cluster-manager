// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// Execute the translated commands through the actual ownership and identity
// guards, using only an in-memory Docker and temporary directories.
func TestGaleraOperationsAgainstExecutor(t *testing.T) {
	for _, kind := range []galera.PlanKind{galera.PlanRollingRestart, galera.PlanMinorUpgrade, galera.PlanScaleOut, galera.PlanScaleIn} {
		t.Run(string(kind), func(t *testing.T) {
			p := createPlan(t)
			p.Cluster.Image = "mariadb:11.4.13@sha256:" + strings.Repeat("a", 64)
			if kind == galera.PlanScaleIn {
				var err error
				p, err = galera.PlanCreate(galera.Topology{Cluster: p.Cluster, Size: 4, Resources: p.Nodes[0].Resources, NewUUID: seqUUIDs("00000000")}, testHosts("h1", "h2", "h3", "h4", "h5"), galera.PlacementPolicy{})
				if err != nil {
					t.Fatal(err)
				}
			}
			root := t.TempDir()
			base := filepath.Join(root, "volumes")
			if err := os.MkdirAll(base, 0755); err != nil {
				t.Fatal(err)
			}
			own := map[string]string{agent.LabelInstallation: "inst-1"}
			d := &memDocker{specs: map[string]agent.ServiceSpec{}, objects: map[string]agent.Object{
				"net-1":    {Kind: agent.KindNetwork, ID: "net-1", Name: "dbcm-shop-galera", Labels: own},
				"secret-1": {Kind: agent.KindSecret, ID: "secret-1", Name: "dbcm-shop-root-pw", Labels: own},
			}}
			guard, err := fencing.NewGuard(fencing.NewFilePersister(filepath.Join(root, "term")))
			if err != nil {
				t.Fatal(err)
			}
			e, err := agent.NewExecutor(agent.Config{InstallationID: "inst-1", StatePath: filepath.Join(root, "state.json"), VolumeBase: base, AllowedIDs: []string{"net-1", "secret-1"}, Guard: guard, Docker: d})
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]string{}
			execute := func(p galera.Plan, opID string) {
				t.Helper()
				for i := range p.Steps {
					if p.Steps[i].Kind == galera.StepInitVolumeDirs {
						p.Steps[i].Owner = &galera.DirOwner{UID: os.Getuid(), GID: os.Getgid()}
					}
				}
				b := createBindings(opID)
				b.VolumeBase = base
				b.ServiceIDs = ids
				b.ApprovalID = "approval"
				b.TokenIDs["n4"] = "tok4"
				op, err := FromGaleraPlan(p, b)
				if err != nil {
					t.Fatal(err)
				}
				for i, s := range op.Steps {
					if s.Command == "" {
						continue
					}
					res, err := e.Handle(context.Background(), agent.Command{Term: 1, OpID: op.ID, StepID: s.ID, Kind: s.Command, Args: s.Args})
					if err != nil || res.Status != agent.StatusSucceeded {
						t.Fatalf("%s: %+v %v", s.ID, res, err)
					}
					if s.Command == agent.KindCreateService {
						var out struct {
							ID string `json:"id"`
						}
						if err := json.Unmarshal(res.Output, &out); err != nil {
							t.Fatal(err)
						}
						ids[p.Steps[i].Slot] = out.ID
					}
				}
			}
			execute(p, "create")
			st := galera.ClusterState{Cluster: p.Cluster, LastKnownSize: len(p.Nodes), Hosts: testHosts("h1", "h2", "h3", "h4", "h5")}
			for _, n := range p.Nodes {
				st.Nodes = append(st.Nodes, galera.NodeState{Node: n, ObservedAt: time.Now(), Health: galera.NodeHealth{State: galera.HealthHealthy, ClusterStatus: "Primary", LocalState: "Synced", Ready: true, Connected: true, ClusterSize: len(p.Nodes), StateUUID: "history"}})
			}
			req := galera.OperationRequest{Kind: kind}
			if kind == galera.PlanMinorUpgrade {
				req.Image = "mariadb:11.4.14@sha256:" + strings.Repeat("b", 64)
			}
			if kind == galera.PlanScaleIn || kind == galera.PlanScaleOut {
				req.Slot = "n4"
			}
			plan, err := galera.PlanOperation(st, req, galera.ReplacementPolicy{Now: time.Now(), NewUUID: seqUUIDs("11111111")})
			if err != nil {
				t.Fatal(err)
			}
			// The adapter must refuse this operation without an explicit approval ID.
			b := createBindings("no-approval")
			if _, err := FromGaleraPlan(plan, b); err == nil {
				t.Fatal("approval binding missing")
			}
			execute(plan, "change")
			if len(d.specs) != len(plan.Nodes) {
				t.Fatalf("%d services, expected %d", len(d.specs), len(plan.Nodes))
			}
			for _, s := range d.specs {
				if s.Replicas != 1 {
					t.Fatal("node left stopped")
				}
				if kind == galera.PlanMinorUpgrade && s.Image != req.Image {
					t.Fatal("image not upgraded")
				}
			}
		})
	}
}
