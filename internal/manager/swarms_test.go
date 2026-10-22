// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dockerclient"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
)

type memConn struct {
	nodes    []dockerclient.Node
	labels   map[string]map[string]string
	services []agent.Object
	fail     error
}

func (m *memConn) Version(context.Context) (dockerclient.VersionInfo, error) {
	if m.fail != nil {
		return dockerclient.VersionInfo{}, m.fail
	}
	return dockerclient.VersionInfo{Version: "29.9.1"}, nil
}
func (m *memConn) Nodes(context.Context) ([]dockerclient.Node, error) { return m.nodes, nil }
func (m *memConn) SetNodeLabel(_ context.Context, id, k, v string) error {
	if m.labels[id] == nil {
		m.labels[id] = map[string]string{}
	}
	m.labels[id][k] = v
	return nil
}
func (m *memConn) CreateGlobalService(_ context.Context, s agent.ServiceSpec) (string, error) {
	m.services = append(m.services, agent.Object{ID: "agent-svc", Labels: s.Labels})
	return "agent-svc", nil
}
func (m *memConn) ListByLabel(context.Context, agent.ObjectKind, string, string) ([]agent.Object, error) {
	return m.services, nil
}

func TestSwarmConnectSyncEnrollDeploy(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	ops := opsFor(t, s)
	if _, err := Put(s.Store(), CollSettings, SettingsKey, Settings{InstallationID: "inst-1", VolumeBase: "/srv/dbcm", AgentImage: "agent:1"}, nil); err != nil {
		t.Fatal(err)
	}
	conn := &memConn{labels: map[string]map[string]string{}, nodes: []dockerclient.Node{
		{ID: "n1", Hostname: "m1", Role: "manager", Availability: "active", State: "ready", EngineVersion: "29.9.1", NanoCPUs: 2e9, MemoryBytes: 2 * gib},
		{ID: "n2", Hostname: "w1", Role: "worker", Availability: "active", State: "ready", EngineVersion: "28.1.0"},
	}}
	sw := &Swarms{Svc: s, Authorizer: ops.Authorizer, Matrix: swarm.DefaultMatrix(),
		Dial: func(string) (swarm.Connector, error) { return conn, nil }}
	if _, err := sw.Connect(nobody, "lab", "unix:///var/run/docker.sock"); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("unauthorized connect: %v", err)
	}
	rec, err := sw.Connect(adminSubject, "lab", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Sync(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Get[SwarmRecord](s.Store(), CollSwarms, rec.ID)
	if got.Value.Observed == nil || got.Value.Observed.State != api.StateHealthy || len(got.Value.Compatibility) != 2 ||
		got.Value.Compatibility[0].Level != "ok" || got.Value.Compatibility[1].Level != "warning" {
		t.Fatalf("swarm record %+v", got.Value)
	}
	hosts, _ := List[HostRecord](s.Store(), CollHosts)
	if len(hosts) != 2 || hosts[0].Value.Enrollment != EnrollDiscovered {
		t.Fatalf("hosts %+v", hosts)
	}
	if _, err := sw.SetEnrollment(adminSubject, rec.ID, "n1", true, "http://127.0.0.1:7070"); err != nil {
		t.Fatal(err)
	}
	// A re-sync keeps the enrollment.
	if err := sw.Sync(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey(rec.ID, "n1"))
	if h.Value.Enrollment != EnrollEnrolled || h.Value.AgentURL == "" {
		t.Fatalf("enrollment lost: %+v", h.Value)
	}
	res, err := sw.DeployAgents(context.Background(), adminSubject, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || conn.labels["n1"][swarm.AgentLabel("inst-1")] != "true" || conn.labels["n2"] != nil {
		t.Fatalf("deploy %+v labels %+v", res, conn.labels)
	}
	h, _, _ = Get[HostRecord](s.Store(), CollHosts, HostKey(rec.ID, "n1"))
	if h.Value.AgentServiceID != "agent-svc" {
		t.Fatalf("service ID not recorded: %+v", h.Value)
	}
	// An unreachable Swarm is reported unavailable, not healthy.
	conn.fail = errors.New("connection refused")
	if err := sw.Sync(context.Background(), rec.ID); err == nil {
		t.Fatal("sync of unreachable swarm succeeded")
	}
	got, _, _ = Get[SwarmRecord](s.Store(), CollSwarms, rec.ID)
	if got.Value.Observed.State != api.StateUnavailable {
		t.Fatalf("observed %+v", got.Value.Observed)
	}
}

func TestSyncReconcilerRunsOnlyWhileLeading(t *testing.T) {
	conn := &memConn{labels: map[string]map[string]string{}, nodes: []dockerclient.Node{{ID: "n1", State: "ready", EngineVersion: "29.9.1"}}}
	var sw *Swarms
	c := newCluster(t, 1, func(_ string, cfg *Config) {
		sw = &Swarms{Matrix: swarm.DefaultMatrix(), Dial: func(string) (swarm.Connector, error) { return conn, nil }}
		cfg.Reconcilers = []Reconciler{SyncReconciler{Swarms: sw, Interval: 10 * time.Millisecond}}
	})
	s := c.leader()
	if _, err := Put(s.Store(), CollSwarms, "sw1", SwarmRecord{ID: "sw1", DockerHost: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reconciler sync", func() bool {
		_, ok, _ := s.Store().Raw(CollHosts, HostKey("sw1", "n1"))
		return ok
	})
}

func TestLabAgentDirectoryRoutesToEnrolledHosts(t *testing.T) {
	var got agent.Command
	srv := httptest.NewServer(agent.NewLabHTTPHandler(handlerFunc(func(_ context.Context, cmd agent.Command) (agent.Result, error) {
		got = cmd
		return agent.Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: agent.StatusSucceeded}, nil
	})))
	defer srv.Close()
	c := newCluster(t, 1, nil)
	s := c.leader()
	if _, err := Put(s.Store(), CollHosts, HostKey("sw", "h1"), HostRecord{Swarm: "sw", NodeID: "h1", Enrollment: EnrollEnrolled, AgentURL: srv.URL}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(s.Store(), CollHosts, HostKey("sw", "h2"), HostRecord{Swarm: "sw", NodeID: "h2", Enrollment: EnrollRevoked}, nil); err != nil {
		t.Fatal(err)
	}
	d := LabAgentDirectory{Store: s.Store()}
	res, err := d.Send(context.Background(), "h1", agent.Command{Term: 1, OpID: "o", StepID: "s", Kind: agent.KindInspect})
	if err != nil || res.Status != agent.StatusSucceeded || got.OpID != "o" {
		t.Fatalf("res %+v err %v", res, err)
	}
	for _, target := range []string{"h2", "h9"} {
		if _, err := d.Send(context.Background(), target, agent.Command{}); !errors.Is(err, agent.ErrInvalidCommand) {
			t.Fatalf("%s: %v", target, err)
		}
	}
}

type handlerFunc func(context.Context, agent.Command) (agent.Result, error)

func (f handlerFunc) Handle(ctx context.Context, cmd agent.Command) (agent.Result, error) {
	return f(ctx, cmd)
}
