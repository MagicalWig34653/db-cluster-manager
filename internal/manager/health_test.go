// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// healthAgents answers collect-health and read-grastate per host.
type healthAgents struct {
	mu       sync.Mutex
	status   map[string]map[string]string // host -> wsrep vars
	dbDown   map[string]bool
	down     map[string]bool // transport failure
	grastate map[string]string
	// recovered answers wsrep-recover per host with a position; a host
	// without an entry fails like a mariadbd without a recovered position.
	recovered map[string]galera.Position
	sent      []sent
}

func (h *healthAgents) Send(_ context.Context, target string, cmd agent.Command) (agent.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, sent{Target: target, Cmd: cmd})
	if h.down[target] {
		return agent.Result{}, errors.New("dial tcp: connection refused")
	}
	r := agent.Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: agent.StatusSucceeded}
	switch cmd.Kind {
	case agent.KindCollectHealth:
		if h.dbDown[target] {
			r.Status, r.Code, r.Message = agent.StatusFailed, agent.CodeDBUnavailable, "connect: no such file"
			return r, nil
		}
		r.Output, _ = json.Marshal(agent.HealthOutput{Status: h.status[target]})
	case agent.KindReadGrastate:
		content, ok := h.grastate[target]
		out := agent.GrastateOutput{Present: ok, Content: content}
		if ok {
			g, err := galera.ParseGrastate(content)
			if err == nil {
				out.Grastate = &g
			}
		}
		r.Output, _ = json.Marshal(out)
	case agent.KindWsrepRecover:
		p, ok := h.recovered[target]
		if !ok {
			r.Status, r.Code, r.Message = agent.StatusFailed, agent.CodeIO, "no recovered position"
			return r, nil
		}
		r.Output, _ = json.Marshal(agent.WsrepRecoverOutput{Position: &p})
	}
	return r, nil
}

func (h *healthAgents) commands() []sent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sent(nil), h.sent...)
}

const stateUUID = "6b1f8a4c-1f3e-11ef-9c6e-7a1b2c3d4e5f"

func wsrep(local, status string, size string) map[string]string {
	return map[string]string{"wsrep_cluster_size": size, "wsrep_cluster_status": status, "wsrep_local_state_comment": local,
		"wsrep_ready": "ON", "wsrep_connected": "ON", "wsrep_cluster_state_uuid": stateUUID}
}

const (
	nodeUUID1 = "559e29b2-0000-4000-8000-000000000001"
	nodeUUID2 = "559e29b2-0000-4000-8000-000000000002"
	nodeUUID3 = "559e29b2-0000-4000-8000-000000000003"
)

func healthSetup(t *testing.T, ha *healthAgents, now func() time.Time) (*Service, ClusterRecord) {
	t.Helper()
	c := newCluster(t, 1, func(_ string, cfg *Config) {
		cfg.Agents = ha
		if now != nil {
			cfg.Now = now
		}
	})
	s := c.leader()
	st := s.Store()
	if _, err := Put(st, CollSettings, SettingsKey, Settings{InstallationID: "inst-1", VolumeBase: "/srv/dbcm"}, nil); err != nil {
		t.Fatal(err)
	}
	cr := ClusterRecord{ID: "clu-1", Swarm: "sw1", Name: "shop", Size: 3, Nodes: []galera.Node{
		{Slot: "n1", NodeUUID: nodeUUID1, HostID: "h1", Volume: "shop-n1"},
		{Slot: "n2", NodeUUID: nodeUUID2, HostID: "h2", Volume: "shop-n2"},
		{Slot: "n3", NodeUUID: nodeUUID3, HostID: "h3", Volume: "shop-n3"},
	}, MonitorPorts: map[string]int{"n2": 13306}}
	if _, err := Put(st, CollClusters, cr.ID, cr, nil); err != nil {
		t.Fatal(err)
	}
	return s, cr
}

func TestHealthCollectorStoresObservations(t *testing.T) {
	var clock struct {
		sync.Mutex
		t time.Time
	}
	clock.t = time.Now()
	now := func() time.Time { clock.Lock(); defer clock.Unlock(); return clock.t }
	ha := &healthAgents{status: map[string]map[string]string{
		"h1": wsrep("Synced", "Primary", "2"),
		"h2": wsrep("Donor/Desynced", "Primary", "2"),
	}, down: map[string]bool{"h3": true}}
	s, _ := healthSetup(t, ha, now)
	l := s.Current()
	if err := (HealthCollector{Timeout: time.Second, Concurrency: 2}).CollectOnce(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Get[ClusterRecord](s.Store(), CollClusters, "clu-1")
	h := got.Value.Health
	if h["n1"].Health.State != galera.HealthHealthy || h["n2"].Health.State != galera.HealthDegraded || h["n3"].Health.State != galera.HealthUnknown {
		t.Fatalf("health %+v", h)
	}
	if !strings.Contains(strings.Join(h["n3"].Health.Reasons, " "), "no answer from agent") || h["n3"].ObservedAt.IsZero() {
		t.Fatalf("unreachable agent observation %+v", h["n3"])
	}
	if got.Value.Observed == nil || got.Value.Observed.State != api.StateDegraded {
		t.Fatalf("cluster observation %+v", got.Value.Observed)
	}
	// Commands: fenced with the leadership term, socket in the volume or
	// the published port.
	args := map[string]agent.CollectHealthArgs{}
	for _, c := range ha.commands() {
		if c.Cmd.Term != l.Term || c.Cmd.Kind != agent.KindCollectHealth {
			t.Fatalf("command %+v", c.Cmd)
		}
		var a agent.CollectHealthArgs
		_ = json.Unmarshal(c.Cmd.Args, &a)
		args[c.Target] = a
	}
	if args["h1"].Socket != "/srv/dbcm/shop-n1/run/mysqld.sock" || args["h2"].Address != "127.0.0.1:13306" {
		t.Fatalf("endpoints %+v", args)
	}

	inv := Inventory{Store: s.Store(), Now: now, StaleAfter: time.Minute}
	ac, err := inv.Cluster(context.Background(), "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	if ac.State != api.StateDegraded || ac.Nodes[0].State != api.StateHealthy || ac.Nodes[0].Role != "synced" ||
		ac.Nodes[1].Role != "donor" || ac.Nodes[2].State != api.StateUnknown || ac.Nodes[2].Role != "unknown" || ac.Nodes[0].ObservedAt.IsZero() {
		t.Fatalf("inventory %+v", ac)
	}
	// Stale observations are unknown.
	clock.Lock()
	clock.t = clock.t.Add(2 * time.Minute)
	clock.Unlock()
	ac, _ = inv.Cluster(context.Background(), "clu-1")
	for _, n := range ac.Nodes {
		if n.State != api.StateUnknown || n.Role != "unknown" {
			t.Fatalf("stale node %+v", n)
		}
	}
	if ac.State != api.StateUnknown {
		t.Fatalf("stale cluster %+v", ac)
	}
}

func TestHealthCollectorDatabaseDownAndQuorumLoss(t *testing.T) {
	ha := &healthAgents{status: map[string]map[string]string{"h1": wsrep("Initialized", "non-Primary", "1")},
		dbDown: map[string]bool{"h2": true, "h3": true}}
	s, _ := healthSetup(t, ha, nil)
	if err := (HealthCollector{}).CollectOnce(context.Background(), s.Current()); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Get[ClusterRecord](s.Store(), CollClusters, "clu-1")
	if got.Value.Health["n2"].Health.State != galera.HealthUnhealthy || got.Value.Observed.State != api.StateUnavailable {
		t.Fatalf("health %+v observed %+v", got.Value.Health, got.Value.Observed)
	}
}

func TestHealthCollectorRunsOnlyWhileLeading(t *testing.T) {
	ha := &healthAgents{status: map[string]map[string]string{}}
	c := newCluster(t, 1, func(_ string, cfg *Config) {
		cfg.Agents = ha
		cfg.Reconcilers = []Reconciler{HealthCollector{Interval: 10 * time.Millisecond}}
	})
	s := c.leader()
	if _, err := Put(s.Store(), CollSettings, SettingsKey, Settings{InstallationID: "i", VolumeBase: "/v"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(s.Store(), CollClusters, "c", ClusterRecord{ID: "c", Nodes: []galera.Node{{Slot: "n1", HostID: "h1", Volume: "v1"}}}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a collected observation", func() bool {
		r, _, _ := Get[ClusterRecord](s.Store(), CollClusters, "c")
		return !r.Value.Health["n1"].ObservedAt.IsZero()
	})
	c.close(s)
	n := len(ha.commands())
	time.Sleep(50 * time.Millisecond)
	if len(ha.commands()) != n {
		t.Fatal("collector kept probing after leadership ended")
	}
}

func TestAgentHealthProbeFeedsChecker(t *testing.T) {
	ha := &healthAgents{status: map[string]map[string]string{
		"h1": wsrep("Synced", "Primary", "3"), "h2": wsrep("Synced", "Primary", "3"), "h9": wsrep("Synced", "Primary", "3"),
	}, down: map[string]bool{"h3": true}}
	s, cr := healthSetup(t, ha, nil)
	// A replacement moves n3 to h9: the probe follows the plan's nodes.
	nodes := append([]galera.Node(nil), cr.Nodes...)
	nodes[2] = galera.Node{Slot: "n3", NodeUUID: "559e29b2-0000-4000-8000-000000000009", HostID: "h9", Volume: "shop-n3-b"}
	if _, err := Put(s.Store(), CollOperations, "op-1", OperationRecord{ID: "op-1", Cluster: "clu-1", Nodes: nodes}, nil); err != nil {
		t.Fatal(err)
	}
	p := AgentHealthProbe{Svc: s}
	k := &galeraprobe.Checker{Prober: p, Topology: p}
	ctx := context.Background()
	ok, reason, err := k.Check(ctx, "op-1", runner.Step{}, runner.Condition{Kind: runner.CondNodeSynced, Target: "n3", ClusterSize: 3})
	if err != nil || !ok {
		t.Fatalf("node-synced on the new host: %v %q %v", ok, reason, err)
	}
	last := ha.commands()[len(ha.commands())-1]
	var a agent.CollectHealthArgs
	_ = json.Unmarshal(last.Cmd.Args, &a)
	if last.Target != "h9" || a.Socket != "/srv/dbcm/shop-n3-b/run/mysqld.sock" {
		t.Fatalf("probe went to %s %+v", last.Target, a)
	}
	ok, _, err = k.Check(ctx, "op-1", runner.Step{}, runner.Condition{Kind: runner.CondQuorum, ClusterSize: 3})
	if err != nil || !ok {
		t.Fatalf("quorum: %v %v", ok, err)
	}
	// A cluster record without a live answer fails closed.
	ha.mu.Lock()
	ha.down["h9"] = true
	ha.mu.Unlock()
	ok, reason, err = k.Check(ctx, "op-1", runner.Step{}, runner.Condition{Kind: runner.CondNodeSynced, Target: "n3", ClusterSize: 3})
	if err != nil || ok || !strings.Contains(reason, "unreachable") {
		t.Fatalf("unreachable node: %v %q %v", ok, reason, err)
	}
	if _, err := p.SlotAddress(ctx, "op-unknown", "n1"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown operation: %v", err)
	}
}

func TestCollectOutageEvidence(t *testing.T) {
	ha := &healthAgents{grastate: map[string]string{
		"h1": "version: 2.1\nuuid: " + stateUUID + "\nseqno: 120\nsafe_to_bootstrap: 0\n",
		"h2": "version: 2.1\nuuid: " + stateUUID + "\nseqno: 125\nsafe_to_bootstrap: 1\n",
	}, down: map[string]bool{"h3": true}}
	s, _ := healthSetup(t, ha, nil)
	ops := opsFor(t, s)
	if _, err := ops.CollectOutageEvidence(context.Background(), nobody, "clu-1"); err == nil {
		t.Fatal("evidence collected without permission")
	}
	d, ev, err := ops.DiagnoseOutageCollected(context.Background(), adminSubject, "clu-1")
	if err != nil {
		t.Fatal(err)
	}
	if ev[0].Grastate == nil || ev[0].Grastate.Seqno != 120 || ev[1].Grastate == nil || ev[2].Grastate != nil {
		t.Fatalf("evidence %+v", ev)
	}
	if !d.RequiresApproval {
		t.Fatal("diagnosis without approval requirement")
	}
	for _, c := range ha.commands() {
		var a agent.ReadGrastateArgs
		if err := json.Unmarshal(c.Cmd.Args, &a); err != nil {
			t.Fatal(err)
		}
		if c.Cmd.Kind != agent.KindReadGrastate || a.Expected.InstallationID != "inst-1" || a.Expected.ClusterID != "clu-1" ||
			!strings.HasPrefix(a.VolumeRoot, "/srv/dbcm/shop-n") {
			t.Fatalf("read-grastate %+v", a)
		}
	}
}
