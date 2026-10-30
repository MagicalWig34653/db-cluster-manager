// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

const gib = int64(1) << 30

func seqUUIDs(prefix string) func() (string, error) {
	n := 0
	return func() (string, error) {
		n++
		return fmt.Sprintf("%s-0000-4000-8000-%012x", prefix, n), nil
	}
}

func testGaleraCluster() galera.Cluster {
	return galera.Cluster{
		InstallationID:     "inst-1",
		ClusterID:          "clu-1",
		Name:               "shop",
		Image:              "mariadb:11.4@sha256:250e1609e93c0e6aecf64bfbab2d84d5e89d2481540fe77447ff6c855c3b1873",
		Network:            "dbcm-shop-galera",
		RootPasswordSecret: "dbcm-shop-root-pw",
	}
}

func testHosts(ids ...string) []galera.Host {
	var out []galera.Host
	for _, id := range ids {
		out = append(out, galera.Host{ID: id, Ready: true, Active: true, Capacity: galera.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}})
	}
	return out
}

func createPlan(t *testing.T) galera.Plan {
	t.Helper()
	p, err := galera.PlanCreate(galera.Topology{
		Cluster: testGaleraCluster(),
		Size:    3,
		Resources: galera.Resources{
			Limits:       galera.ResourceAmount{MilliCPU: 1000, MemoryBytes: 2 * gib},
			Reservations: galera.ResourceAmount{MilliCPU: 500, MemoryBytes: gib},
		},
		NewUUID: seqUUIDs("00000000"),
	}, testHosts("h1", "h2", "h3"), galera.PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func createBindings(opID string) GaleraBindings {
	return GaleraBindings{OpID: opID, InstallationID: "inst-1", VolumeBase: "/var/lib/dbcm/volumes",
		TokenIDs: map[string]string{"n1": "tok-1", "n2": "tok-2", "n3": "tok-3"}, MaxRetries: 2, StepTimeout: 30 * time.Second}
}

func TestFromGaleraPlanCreate(t *testing.T) {
	p := createPlan(t)
	op, err := FromGaleraPlan(p, createBindings("op-create"))
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != "galera/create-cluster" || !slices.Equal(op.Locks, []string{"cluster/clu-1", "node/clu-1/n1", "node/clu-1/n2", "node/clu-1/n3"}) {
		t.Errorf("kind %s locks %v", op.Kind, op.Locks)
	}
	if len(op.Steps) != len(p.Steps) {
		t.Fatalf("%d steps, plan has %d", len(op.Steps), len(p.Steps))
	}
	var kinds []string
	for i, s := range op.Steps {
		if s.ID != p.Steps[i].ID {
			t.Errorf("step %d ID %s, plan %s", i, s.ID, p.Steps[i].ID)
		}
		kinds = append(kinds, string(s.Command))
		if s.Command != "" && s.Target != p.Steps[i].HostID {
			t.Errorf("step %s target %s", s.ID, s.Target)
		}
	}
	want := []string{}
	for range 3 {
		want = append(want, "create-volume", "init-identity", "init-volume-dirs", "create-service", "")
	}
	want = append(want, "update-service", "await-service", "")
	if !slices.Equal(kinds, want) {
		t.Errorf("commands %q\nwant %q", kinds, want)
	}
	// init-identity carries the bound token and the plan's identity.
	var ia agent.InitIdentityArgs
	if err := json.Unmarshal(op.Steps[1].Args, &ia); err != nil {
		t.Fatal(err)
	}
	if ia.Token.TokenID != "tok-1" || ia.Token.Identity.ClusterID != "clu-1" || ia.Token.Identity.NodeUUID != p.Nodes[0].NodeUUID ||
		ia.VolumeRoot != "/var/lib/dbcm/volumes/"+p.Nodes[0].Volume {
		t.Errorf("init args %+v", ia)
	}
	// The bootstrap node is created with the bootstrap flag, the update drops it.
	var spec agent.ServiceSpec
	if err := json.Unmarshal(op.Steps[3].Args, &spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Args, galera.BootstrapFlag) {
		t.Errorf("bootstrap create without flag: %v", spec.Args)
	}
	var upd UpdateServiceArgs
	if err := json.Unmarshal(op.Steps[15].Args, &upd); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(upd.Spec.Args, galera.BootstrapFlag) || upd.ServiceName != "dbcm-shop-n1" {
		t.Errorf("update args %+v", upd)
	}
	// Joiner n2 waits for n1 synced at size 1; wait-synced is a gate.
	if c := op.Steps[8].Preconditions; !slices.Contains(c, Condition{Kind: CondNodeSynced, Target: "n1", ClusterSize: 1}) {
		t.Errorf("n2 create preconditions %+v", c)
	}
	if g := op.Steps[4]; g.Command != "" || !slices.Contains(g.Preconditions, Condition{Kind: CondNodeSynced, Target: "n1", ClusterSize: 1}) {
		t.Errorf("wait gate %+v", g)
	}
}

func TestFromGaleraPlanRequiresBindings(t *testing.T) {
	p := createPlan(t)
	b := createBindings("op")
	delete(b.TokenIDs, "n2")
	if _, err := FromGaleraPlan(p, b); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("missing token: %v", err)
	}
	if _, err := FromGaleraPlan(galera.Plan{Kind: galera.PlanFullOutageRecovery, ClusterID: "c"}, createBindings("op")); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("full outage plan accepted: %v", err)
	}
}

func replacementPlan(t *testing.T) galera.Plan {
	t.Helper()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	created := createPlan(t)
	st := galera.ClusterState{Cluster: created.Cluster, LastKnownSize: 3, Hosts: testHosts("h1", "h2", "h3", "h4")}
	synced := galera.HealthFromStatus(map[string]string{
		"wsrep_cluster_size": "2", "wsrep_cluster_status": "Primary", "wsrep_local_state_comment": "Synced",
		"wsrep_ready": "ON", "wsrep_connected": "ON", "wsrep_cluster_state_uuid": "9f14fd2e-cbc5-11f1-b18b-f6cb8d63277e",
		"wsrep_last_committed": "11", "wsrep_flow_control_paused": "0.000000", "wsrep_local_recv_queue_avg": "0.000000",
	})
	for _, n := range created.Nodes {
		h := synced
		if n.Slot == "n3" {
			h = galera.NodeHealth{}
		}
		st.Nodes = append(st.Nodes, galera.NodeState{Node: n, Health: h, ObservedAt: now.Add(-5 * time.Second)})
	}
	p, err := galera.PlanReplacement(st, "n3", galera.ReplacementPolicy{Now: now, NewUUID: seqUUIDs("11111111")})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFromGaleraPlanReplacement(t *testing.T) {
	p := replacementPlan(t)
	b := createBindings("op-replace")
	b.ServiceIDs = map[string]string{"n3": "svc-old-n3"}
	op, err := FromGaleraPlan(p, b)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, s := range op.Steps {
		kinds = append(kinds, string(s.Command))
	}
	want := []string{"remove-service", "", "create-volume", "init-identity", "init-volume-dirs", "", "create-service", ""}
	if !slices.Equal(kinds, want) {
		t.Errorf("commands %q want %q", kinds, want)
	}
	var sa agent.ServiceArgs
	_ = json.Unmarshal(op.Steps[0].Args, &sa)
	if sa.ServiceID != "svc-old-n3" || op.Steps[0].Target != p.Steps[0].HostID {
		t.Errorf("remove args %+v target %s", sa, op.Steps[0].Target)
	}
	if !slices.ContainsFunc(op.Steps[0].Preconditions, func(c Condition) bool { return c.Kind == CondQuorum && c.ClusterSize == 2 }) {
		t.Errorf("remove-service lost its quorum precondition: %+v", op.Steps[0].Preconditions)
	}
	if !slices.Equal(op.Locks, []string{"cluster/clu-1", "node/clu-1/n3", "node/clu-1/n1"}) {
		t.Errorf("locks %v", op.Locks)
	}
	delete(b.ServiceIDs, "n3")
	if _, err := FromGaleraPlan(p, b); !errors.Is(err, ErrInvalidOperation) {
		t.Errorf("missing service ID: %v", err)
	}
}

// galeraWorld is a simulated observation source: a node counts as Synced at
// cluster size N once its service was created on some agent and N services
// of the cluster exist. It reads the fake agents' records only.
type galeraWorld struct {
	agents  fakeAgents
	cluster string
}

func (w galeraWorld) created() map[string]bool {
	out := map[string]bool{}
	for _, a := range w.agents {
		a.mu.Lock()
		for _, rec := range a.recs {
			if rec.cmd.Kind == agent.KindCreateService && rec.result != nil && rec.result.Status == agent.StatusSucceeded {
				var s agent.ServiceSpec
				if json.Unmarshal(rec.cmd.Args, &s) == nil {
					out[s.Name] = true
				}
			}
		}
		a.mu.Unlock()
	}
	return out
}

func (w galeraWorld) Check(_ context.Context, _ string, _ Step, c Condition) (bool, string, error) {
	switch c.Kind {
	case CondNodeSynced:
		created := w.created()
		if !created[galera.ServiceName(w.cluster, c.Target)] {
			return false, "service not created", nil
		}
		if len(created) < c.ClusterSize {
			return false, fmt.Sprintf("cluster size %d < %d", len(created), c.ClusterSize), nil
		}
		return true, "", nil
	default:
		return false, "unsupported condition", nil
	}
}

// commandEffects returns per agent the executed (OpID/StepID) effects.
func countEffects(agents fakeAgents) map[string]int {
	out := map[string]int{}
	for _, a := range agents {
		for _, e := range a.Effects() {
			out[e]++
		}
	}
	return out
}

func TestCreatePlanEndToEndWithFakes(t *testing.T) {
	h := newHarness(t, "h1", "h2", "h3")
	h.checker = galeraWorld{agents: h.agents, cluster: "shop"}
	p := createPlan(t)
	op, err := FromGaleraPlan(p, createBindings("op-create"))
	if err != nil {
		t.Fatal(err)
	}
	// Lose one answer mid-way and fail over to m2 during the joiner n3.
	h.agents[p.Nodes[1].HostID].addFault("09-create-service-n2", dropResponse)
	var once sync.Once
	h.agents[p.Nodes[2].HostID].before = func(cmd agent.Command) {
		if cmd.StepID == "11-create-volume-n3" {
			once.Do(func() { h.meta.elect("m2", false) })
		}
	}
	v1, err := h.runner("m1").Run(context.Background(), op)
	if !errors.Is(err, ErrLostLeadership) {
		t.Fatalf("m1: %v %s", err, v1.Status)
	}
	r2 := h.runner("m2")
	if _, err := r2.TakeOver(func(id string) (Operation, bool) { return op, id == op.ID }); err != nil {
		t.Fatal(err)
	}
	v2, err := r2.Resume(context.Background(), op.ID)
	if err != nil || v2.Status != StatusSucceeded {
		t.Fatalf("m2: %v %s (%s)", err, v2.Status, v2.Reason)
	}
	for e, n := range countEffects(h.agents) {
		if n != 1 {
			t.Errorf("effect %s executed %d times", e, n)
		}
	}
	if n := len(countEffects(h.agents)); n != 14 {
		t.Errorf("%d distinct effects; want 14 agent commands", n)
	}
	for _, c := range h.agents[p.Nodes[2].HostID].Received() {
		if c.StepID == "14-create-service-n3" && c.Term != 2 {
			t.Errorf("n3 created under term %d", c.Term)
		}
	}
	if !strings.HasPrefix(v2.Kind, "galera/") || len(v2.Steps) != 18 {
		t.Errorf("view %s %d", v2.Kind, len(v2.Steps))
	}
}

// TestFromGaleraPlanAccounts: the bootstrap account steps become exec-sql
// commands on the bootstrap host that name templates and secrets only; the
// agent connects with the cluster's root password secret.
func TestFromGaleraPlanAccounts(t *testing.T) {
	c := testGaleraCluster()
	c.Accounts = galera.Accounts{MonitorSecret: "dbcm-inst-monitor-pw", BackupSecret: "dbcm-inst-backup-pw"}
	p, err := galera.PlanCreate(galera.Topology{Cluster: c, Size: 3,
		Resources: galera.Resources{Limits: galera.ResourceAmount{MilliCPU: 1000, MemoryBytes: 2 * gib}},
		NewUUID:   seqUUIDs("00000000")}, testHosts("h1", "h2", "h3"), galera.PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	op, err := FromGaleraPlan(p, createBindings("op-acc"))
	if err != nil {
		t.Fatal(err)
	}
	var got []agent.ExecSQLArgs
	for _, s := range op.Steps {
		if s.Command != agent.KindExecSQL {
			continue
		}
		if s.Target != p.Nodes[0].HostID {
			t.Errorf("exec-sql on %s, want bootstrap host %s", s.Target, p.Nodes[0].HostID)
		}
		var a agent.ExecSQLArgs
		if err := json.Unmarshal(s.Args, &a); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(s.Args), "IDENTIFIED") || strings.Contains(string(s.Args), "GRANT") {
			t.Errorf("SQL text in exec-sql args: %s", s.Args)
		}
		got = append(got, a)
	}
	if len(got) != 2 {
		t.Fatalf("exec-sql steps %d", len(got))
	}
	m, b := got[0], got[1]
	if m.Template != "create-monitor-user" || m.User != "dbcm_monitor" || m.Host != "localhost" || m.PasswordSecret != "dbcm-inst-monitor-pw" ||
		m.AdminSecret != "dbcm-shop-root-pw" || m.Expected.NodeUUID != p.Nodes[0].NodeUUID || m.VolumeRoot != "/var/lib/dbcm/volumes/"+p.Nodes[0].Volume {
		t.Errorf("monitor args %+v", m)
	}
	if b.Template != "create-backup-user" || b.User != "dbcm_backup" || b.Host != "%" || b.PasswordSecret != "dbcm-inst-backup-pw" {
		t.Errorf("backup args %+v", b)
	}
	var dirs agent.InitVolumeDirsArgs
	if err := json.Unmarshal(op.Steps[2].Args, &dirs); err != nil || dirs.UID != 999 || dirs.GID != 999 || dirs.Expected.ClusterID != "clu-1" {
		t.Errorf("init-volume-dirs args %+v (%v)", dirs, err)
	}
}

// TestExecSQLIsIdempotent: the bootstrap account step is retried while the
// new server does not accept the root login yet (lab E4); its templates are
// idempotent statements.
// A stopped MariaDB task needs seconds to leave Swarm's running count (lab RC
// run, rolling restart: await-stopped failed on its first answer). The step
// changes nothing and must be polled like await-service.
func TestAwaitStoppedIsPolled(t *testing.T) {
	if !Idempotent(agent.KindAwaitStopped) {
		t.Fatal("await-stopped must be idempotent so the runner polls it up to MaxRetries")
	}
	if Idempotent(agent.KindStartService) {
		t.Fatal("start-service changes state and must not be retried blindly")
	}
}

func TestExecSQLIsIdempotent(t *testing.T) {
	if !Idempotent(agent.KindExecSQL) {
		t.Fatal("exec-sql is not retried")
	}
	if Idempotent(agent.KindCreateService) {
		t.Fatal("create-service must not be blindly retried")
	}
}

// TestFromGaleraPlanServiceHost: Swarm service commands go to the service
// host (a Swarm manager node, lab E4); node-local steps stay on the node.
func TestFromGaleraPlanServiceHost(t *testing.T) {
	p, err := galera.PlanCreate(galera.Topology{Cluster: testGaleraCluster(), Size: 3,
		Resources: galera.Resources{Limits: galera.ResourceAmount{MilliCPU: 1000, MemoryBytes: 2 * gib}},
		NewUUID:   seqUUIDs("00000000")}, testHosts("h1", "h2", "h3"), galera.PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	b := createBindings("op-svc")
	b.ServiceHost = "mgr"
	op, err := FromGaleraPlan(p, b)
	if err != nil {
		t.Fatal(err)
	}
	hostOf := map[string]string{}
	for _, gs := range p.Steps {
		hostOf[gs.ID] = gs.HostID
	}
	services := 0
	for _, s := range op.Steps {
		switch s.Command {
		case agent.KindCreateService, KindUpdateService, agent.KindAwaitService:
			services++
			if s.Target != "mgr" {
				t.Errorf("%s on %s, want the service host", s.ID, s.Target)
			}
		case "":
		default:
			if s.Target != hostOf[s.ID] {
				t.Errorf("%s on %s, want its node host %s", s.ID, s.Target, hostOf[s.ID])
			}
		}
	}
	if services != 5 {
		t.Fatalf("service steps %d, want 3 creates, 1 update and 1 await", services)
	}
}
