// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
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

// TestInspectionAllowsReferencedObjects: lab E4 found the approved
// inspection restore refused with not-owned (recovery key secret). The
// inspection now starts with an allow-objects step for the inspection
// network, the backend and key secrets and the session's init secret; the
// step needs no approval and is covered by the approved digest.
func TestInspectionAllowsReferencedObjects(t *testing.T) {
	env := newBackupEnv(t, true)
	env.completedBackup(t, "b1", env.clock.Now().Add(-time.Hour))
	inst := map[string]string{agent.LabelInstallation: "inst-1"}
	rc := &refConn{objs: map[agent.ObjectKind][]agent.Object{
		agent.KindNetwork: {{ID: "net-insp", Name: "dbcm-inspect-net", Labels: inst}},
		agent.KindSecret:  {{ID: "sec-be", Name: "dbcm-backend", Labels: inst}, {ID: "sec-key", Name: "dbcm-recovery-key", Labels: inst}},
	}}
	env.b.Swarms = &Swarms{Svc: env.svc, Dial: func(string) (swarm.Connector, error) { return &initSecretConn{refConn: rc}, nil }}
	env.ops.Swarms = env.b.Swarms
	if _, err := env.b.StartInspection(context.Background(), adminSubject, api.InspectionInput{Cluster: "clu-1", Host: "h4", TTLMinutes: 30}); !errors.Is(err, api.ErrSettingsIncomplete) {
		t.Fatalf("without a Swarm manager node for the service commands: %v", err)
	}
	if _, _, err := Update(env.svc.Store(), CollHosts, HostKey("sw1", "h1"), func(h HostRecord, _ bool) (HostRecord, error) {
		h.Role, h.AgentSerial = "manager", "0a"
		return h, nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := env.b.StartInspection(context.Background(), adminSubject, api.InspectionInput{Cluster: "clu-1", Host: "h4", TTLMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	rec := opRecord(t, env.svc, res.ID)
	// One allow step for the inspection host and one for the manager node
	// that creates the job and the instance (F12).
	for i, host := range []string{"h4", "h1"} {
		st := rec.Definition.Steps[i]
		var a agent.AllowObjectsArgs
		if st.Command != agent.KindAllowObjects || st.Target != host || json.Unmarshal(st.Args, &a) != nil ||
			!slices.Equal(a.IDs, []string{"net-insp", "sec-be", "sec-key", "sec-init"}) {
			t.Fatalf("step %d %+v %v", i, st, a.IDs)
		}
		for _, c := range st.Preconditions {
			if c.Kind == dbbackup.CondApproved {
				t.Fatal("allow-objects waits for the approval")
			}
		}
	}
	for _, st := range rec.Definition.Steps {
		want := map[agent.Kind]string{agent.KindRunRestore: "h1", agent.KindCreateService: "h1", agent.KindRemoveJob: "h1",
			agent.KindCheckRestoreTarget: "h4", agent.KindCreateVolume: "h4", agent.KindInitIdentity: "h4"}[st.Command]
		if want != "" && st.Target != want {
			t.Errorf("%s on %s, want %s", st.Command, st.Target, want)
		}
	}
	if d, _ := DefinitionDigest(rec.Definition); d != res.Approval.Digest {
		t.Fatal("approval digest does not cover the allow step")
	}
}

// initSecretConn records the created init secret as a labelled object.
type initSecretConn struct{ *refConn }

func (c *initSecretConn) CreateSecret(_ context.Context, name string, labels map[string]string, _ []byte) (string, error) {
	c.objs[agent.KindSecret] = append(c.objs[agent.KindSecret], agent.Object{ID: "sec-init", Name: name, Labels: labels})
	return "sec-init", nil
}

// TestAbortFailedOperationReleasesIt: a failed operation has an end time
// for the inventory but keeps its runner locks; lab E4 found it could then
// never be aborted (409) and blocked the next inspection of the same
// backup. Abort is accepted for it and ends it as aborted.
func TestAbortFailedOperationReleasesIt(t *testing.T) {
	agents := newFakeAgents()
	agents.refuse = func(cmd agent.Command) bool { return cmd.Kind == agent.KindCreateVolume }
	c := newCluster(t, 1, func(_ string, cfg *Config) { cfg.Agents = agents; cfg.Checker = okChecker{} })
	l := c.leader()
	ops := seed(t, l, 3)
	ops.MaxRetries = 0
	rec, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	failed := waitOpStatus(t, l, rec.ID, runner.StatusFailed)
	if failed.EndedAt.IsZero() {
		t.Fatal("failed operation without end time")
	}
	if err := ops.Abort(adminSubject, rec.ID); err != nil {
		t.Fatalf("abort of a failed operation: %v", err)
	}
	waitOpStatus(t, l, rec.ID, runner.StatusAborted)
	if err := ops.Abort(adminSubject, rec.ID); !errors.Is(err, ErrOperationEnded) {
		t.Fatalf("second abort: %v", err)
	}
}
