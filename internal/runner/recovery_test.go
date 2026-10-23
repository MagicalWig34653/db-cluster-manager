// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

const recoveryUUID = "3b2d2499-ce8f-11f1-a8b6-67c327274574"

// recoveryPlan diagnoses a clean full outage (n2 ahead) of the create
// plan's cluster and attaches specs.
func recoveryPlan(t *testing.T) galera.Plan {
	t.Helper()
	cp := createPlan(t)
	g := func(seqno int64) *galera.Grastate {
		return &galera.Grastate{Version: "2.1", UUID: recoveryUUID, Seqno: seqno, HasSafeToBootstrap: true}
	}
	d, err := galera.DiagnoseFullOutage([]galera.NodeInfo{
		{Slot: "n1", Grastate: g(3517)}, {Slot: "n2", Grastate: g(3518)}, {Slot: "n3", Grastate: g(3516)},
	})
	if err != nil || d.Candidate != "n2" {
		t.Fatalf("diagnosis %+v, %v", d, err)
	}
	p, err := galera.RecoveryPlan(d, cp.Cluster, cp.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func recoveryBindings() GaleraBindings {
	return GaleraBindings{OpID: "op-rec", InstallationID: "inst-1", VolumeBase: "/var/lib/dbcm/volumes",
		ServiceIDs: map[string]string{"n1": "svc-1", "n2": "svc-2", "n3": "svc-3"}, MaxRetries: 30,
		ServiceHost: "mgr", ApprovalID: "apr-1"}
}

func TestRecoveryPlanNeedsApproval(t *testing.T) {
	p := recoveryPlan(t)
	b := recoveryBindings()
	b.ApprovalID = ""
	if _, err := FromGaleraPlan(p, b); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("without approval ID: %v", err)
	}
	unapproved := p
	unapproved.Steps = slices.Clone(p.Steps[1:])
	if _, err := FromGaleraPlan(unapproved, recoveryBindings()); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("plan without the approved first step: %v", err)
	}
	b = recoveryBindings()
	delete(b.ServiceIDs, "n3")
	if _, err := FromGaleraPlan(p, b); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("missing service ID: %v", err)
	}
}

func TestRecoveryPlanConversion(t *testing.T) {
	p := recoveryPlan(t)
	op, err := FromGaleraPlan(p, recoveryBindings())
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != "galera/full-outage-recovery" {
		t.Fatalf("kind %q", op.Kind)
	}
	hostOf := map[string]string{}
	for _, n := range p.Nodes {
		hostOf[n.Slot] = n.HostID
	}
	var kinds []string
	var awaitIDs []string
	for i, s := range op.Steps {
		gs := p.Steps[i]
		kinds = append(kinds, string(s.Command))
		// Approval only on the first step, bound to the approval ID.
		approved := slices.ContainsFunc(s.Preconditions, func(c Condition) bool { return c.Kind == CondApproved && c.Target == "apr-1" })
		if approved != (i == 0) {
			t.Errorf("step %s approval condition = %v", s.ID, approved)
		}
		switch gs.Kind {
		case galera.StepStopAll, galera.StepAwaitStopped, galera.StepStartNode, galera.StepUpdateService, galera.StepAwaitUpdate:
			if s.Target != "mgr" {
				t.Errorf("step %s target %q, want the service host", s.ID, s.Target)
			}
		case galera.StepVerifyIdentity, galera.StepMarkSafe:
			if s.Target != hostOf[gs.Slot] {
				t.Errorf("step %s target %q, want the volume's host %q", s.ID, s.Target, hostOf[gs.Slot])
			}
		}
		if gs.Kind == galera.StepAwaitStopped {
			awaitIDs = append(awaitIDs, s.ID)
		}
		if slices.ContainsFunc(s.Preconditions, func(c Condition) bool { return c.Kind == CondNoNodeRunning }) {
			t.Errorf("step %s keeps an unevaluable no-node-running condition", s.ID)
		}
		if gs.Kind == galera.StepMarkSafe {
			for _, id := range awaitIDs {
				if !slices.ContainsFunc(s.Preconditions, func(c Condition) bool { return c.Kind == CondStepSucceeded && c.StepID == id }) {
					t.Errorf("mark-safe does not depend on %s", id)
				}
			}
			var a agent.MarkSafeArgs
			if err := json.Unmarshal(s.Args, &a); err != nil || a.UUID != recoveryUUID || a.Seqno != 3518 || a.Expected.NodeUUID != gs.NodeUUID {
				t.Errorf("mark-safe args %+v (%v)", a, err)
			}
		}
		if s.Command == agent.KindUpdateService {
			var a agent.UpdateServiceArgs
			if err := json.Unmarshal(s.Args, &a); err != nil {
				t.Fatal(err)
			}
			boot := slices.Contains(a.Spec.Args, galera.BootstrapFlag)
			if boot != (a.Spec.Replicas == 0) {
				t.Errorf("step %s: bootstrap %v with %d replicas", s.ID, boot, a.Spec.Replicas)
			}
		}
	}
	want := []agent.Kind{agent.KindStopDB, agent.KindStopDB, agent.KindStopDB,
		agent.KindAwaitStopped, agent.KindAwaitStopped, agent.KindAwaitStopped,
		agent.KindVerifyIdentity, agent.KindVerifyIdentity, agent.KindVerifyIdentity,
		agent.KindMarkSafeToBootstrap, agent.KindUpdateService, agent.KindStartService, "",
		agent.KindStartService, "", agent.KindStartService, "",
		agent.KindUpdateService, agent.KindAwaitService, ""}
	var wantS []string
	for _, k := range want {
		wantS = append(wantS, string(k))
	}
	if !slices.Equal(kinds, wantS) {
		t.Fatalf("commands\n%v\nwant\n%v", kinds, wantS)
	}
}
