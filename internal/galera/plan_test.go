// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const gib = int64(1) << 30

// seqUUIDs returns a deterministic UUID source.
func seqUUIDs() func() (string, error) {
	n := 0
	return func() (string, error) {
		n++
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", n), nil
	}
}

func host(id string, cpu, memGiB int64) Host {
	return Host{ID: id, Ready: true, Active: true, Capacity: ResourceAmount{MilliCPU: cpu, MemoryBytes: memGiB * gib}}
}

func testTopology(size int) Topology {
	c := testCluster()
	c.Members = nil
	return Topology{
		Cluster: c,
		Size:    size,
		Resources: Resources{
			Limits:       ResourceAmount{MilliCPU: 1000, MemoryBytes: 2 * gib},
			Reservations: ResourceAmount{MilliCPU: 500, MemoryBytes: 1 * gib},
		},
		NewUUID: seqUUIDs(),
	}
}

func threeHosts() []Host {
	return []Host{host("h1", 4000, 8), host("h2", 4000, 8), host("h3", 4000, 8)}
}

func assignment(p Plan) map[string]string {
	m := map[string]string{}
	for _, n := range p.Nodes {
		m[n.Slot] = n.HostID
	}
	return m
}

func TestPlanCreateOrdering(t *testing.T) {
	p, err := PlanCreate(testTopology(3), threeHosts(), PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != PlanCreateCluster || p.RequiresApproval {
		t.Errorf("kind=%s approval=%v", p.Kind, p.RequiresApproval)
	}
	var got []string
	for _, s := range p.Steps {
		got = append(got, string(s.Kind)+":"+s.Slot)
	}
	want := []string{
		"create-volume:n1", "init-identity:n1", "init-volume-dirs:n1", "create-service:n1", "wait-synced:n1",
		"create-volume:n2", "init-identity:n2", "init-volume-dirs:n2", "create-service:n2", "wait-synced:n2",
		"create-volume:n3", "init-identity:n3", "init-volume-dirs:n3", "create-service:n3", "wait-synced:n3",
		"update-service:n1", "await-update:n1", "wait-synced:n1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("steps =\n%v\nwant\n%v", got, want)
	}
	// Every step after the first depends on its predecessor (strictly
	// sequential, one joiner at a time).
	for i, s := range p.Steps {
		if i == 0 {
			if len(s.Preconditions) != 0 {
				t.Errorf("first step has preconditions %v", s.Preconditions)
			}
			continue
		}
		if !hasPre(s, Precondition{Kind: PreStepSucceeded, StepID: p.Steps[i-1].ID}) {
			t.Errorf("step %s does not depend on %s", s.ID, p.Steps[i-1].ID)
		}
	}
	// Only the bootstrap node's create-service carries the bootstrap flag.
	for _, s := range p.Steps {
		if s.Spec == nil {
			continue
		}
		boot := s.Kind == StepCreateService && s.Slot == "n1"
		if s.Spec.Bootstrap() != boot {
			t.Errorf("step %s bootstrap=%v want %v", s.ID, s.Spec.Bootstrap(), boot)
		}
	}
	// Joiner k starts only after joiner k-1 reported Synced at size k-1.
	cs := stepsOf(p, StepCreateService)
	if !hasPre(cs[1], Precondition{Kind: PreNodeSynced, Slot: "n1", ClusterSize: 1}) ||
		!hasPre(cs[2], Precondition{Kind: PreNodeSynced, Slot: "n2", ClusterSize: 2}) {
		t.Errorf("joiner preconditions: %v / %v", cs[1].Preconditions, cs[2].Preconditions)
	}
	ws := stepsOf(p, StepWaitSynced)
	for i, size := range []int{1, 2, 3, 3} {
		if ws[i].ClusterSize != size {
			t.Errorf("wait %s size = %d, want %d", ws[i].ID, ws[i].ClusterSize, size)
		}
	}
	upd := stepsOf(p, StepUpdateService)[0]
	for _, slot := range []string{"n1", "n2", "n3"} {
		if !hasPre(upd, Precondition{Kind: PreNodeSynced, Slot: slot, ClusterSize: 3}) {
			t.Errorf("bootstrap removal does not require %s Synced at size 3", slot)
		}
	}
	// Desired state never persists the bootstrap flag.
	if len(p.Desired) != 3 {
		t.Fatalf("desired = %d specs", len(p.Desired))
	}
	for _, d := range p.Desired {
		if err := d.ValidateSteadyState(); err != nil {
			t.Error(err)
		}
	}
	if !slices.Equal(upd.Spec.Args, p.Desired[0].Args) {
		t.Errorf("update-service spec differs from desired n1 spec")
	}
	// Identities: new UUID and a volume name per node, init carries them.
	seen := map[string]bool{}
	for _, n := range p.Nodes {
		if seen[n.NodeUUID] || seen[n.Volume] || !strings.Contains(n.Volume, n.NodeUUID[:8]) {
			t.Errorf("node identity %+v not unique", n)
		}
		seen[n.NodeUUID], seen[n.Volume] = true, true
	}
	for _, s := range stepsOf(p, StepInitIdentity) {
		if s.NodeUUID == "" || s.Volume == "" || s.HostID == "" {
			t.Errorf("init step lacks identity: %+v", s)
		}
	}
	// init-volume-dirs follows init-identity of the same node and carries
	// the identity (the agent verifies the marker first) and the owner.
	for _, s := range stepsOf(p, StepInitVolumeDirs) {
		if s.NodeUUID == "" || s.Volume == "" || s.HostID == "" || s.Owner == nil || *s.Owner != (DirOwner{UID: DefaultDBUID, GID: DefaultDBGID}) {
			t.Errorf("init-volume-dirs step %+v (owner %+v)", s, s.Owner)
		}
	}
	if len(stepsOf(p, StepExecSQL)) != 0 {
		t.Error("exec-sql steps without configured accounts")
	}
	if !slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "monitoring account") }) {
		t.Errorf("no warning about the missing monitoring account: %v", p.Warnings)
	}
	if !slices.Equal(p.Cluster.Members, []string{"n1", "n2", "n3"}) {
		t.Errorf("members = %v", p.Cluster.Members)
	}
	// Step IDs are unique.
	ids := map[string]bool{}
	for _, s := range p.Steps {
		if ids[s.ID] {
			t.Errorf("duplicate step ID %s", s.ID)
		}
		ids[s.ID] = true
	}
}

func TestPlanCreateAntiAffinityAndCapacity(t *testing.T) {
	// Only two hosts: a third node would share a host.
	_, err := PlanCreate(testTopology(3), threeHosts()[:2], PlacementPolicy{})
	var pe *PlacementError
	if !errors.Is(err, ErrNoCapacity) || !errors.As(err, &pe) || pe.Slot != "n3" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(pe.Rejected["h1"], "already hosts") {
		t.Errorf("rejections = %v", pe.Rejected)
	}
	// Capacity after reserve: h3 has 3 GiB, reserve 1.5 GiB leaves 1.5 GiB
	// < 2 GiB limit (the demand counts the limit, not the reservation).
	hosts := threeHosts()
	hosts[2].Capacity.MemoryBytes = 3 * gib
	policy := PlacementPolicy{Reserve: ResourceAmount{MilliCPU: 500, MemoryBytes: gib + gib/2}}
	_, err = PlanCreate(testTopology(3), hosts, policy)
	if !errors.As(err, &pe) || !strings.Contains(pe.Rejected["h3"], "memory") {
		t.Fatalf("err = %v", err)
	}
	// Already allocated workloads count against the host.
	hosts = threeHosts()
	hosts[1].Allocated = ResourceAmount{MilliCPU: 3500}
	_, err = PlanCreate(testTopology(3), hosts, PlacementPolicy{})
	if !errors.As(err, &pe) || !strings.Contains(pe.Rejected["h2"], "CPU") {
		t.Fatalf("err = %v", err)
	}
	// Unknown capacity, not ready, drained hosts are rejected.
	hosts = append(threeHosts()[:0:0], host("h1", 4000, 8), host("h2", 0, 0), host("h3", 4000, 8), host("h4", 4000, 8), host("h5", 4000, 8))
	hosts[3].Ready = false
	hosts[4].Active = false
	_, err = PlanCreate(testTopology(3), hosts, PlacementPolicy{})
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	for h, want := range map[string]string{"h2": "capacity unknown", "h4": "not ready", "h5": "not active"} {
		if !strings.Contains(pe.Rejected[h], want) {
			t.Errorf("rejection %s = %q, want %q", h, pe.Rejected[h], want)
		}
	}
}

func TestPlanCreateAutomaticScoring(t *testing.T) {
	hosts := []Host{host("h1", 4000, 8), host("h2", 4000, 16), host("h3", 8000, 8), host("h4", 4000, 4)}
	p, err := PlanCreate(testTopology(3), hosts, PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	// Most free memory first, then most free CPU, then host ID.
	want := map[string]string{"n1": "h2", "n2": "h3", "n3": "h1"}
	if got := assignment(p); !mapsEqual(got, want) {
		t.Errorf("assignment = %v, want %v", got, want)
	}
}

// Precedence: allowlist > exclusion > preference > automatic scoring.
func TestPlacementPrecedence(t *testing.T) {
	five := []Host{host("h1", 4000, 8), host("h2", 4000, 8), host("h3", 4000, 8), host("h4", 4000, 32), host("h5", 4000, 32)}
	cases := []struct {
		name    string
		policy  PlacementPolicy
		want    map[string]string
		wantErr error
		warn    string
	}{
		{
			name: "automatic scoring prefers big hosts",
			want: map[string]string{"n1": "h4", "n2": "h5", "n3": "h1"},
		},
		{
			name:   "preference beats automatic scoring",
			policy: PlacementPolicy{Preferred: []string{"h3", "h2"}},
			want:   map[string]string{"n1": "h3", "n2": "h2", "n3": "h4"},
		},
		{
			name:   "exclusion beats preference",
			policy: PlacementPolicy{Preferred: []string{"h3", "h2"}, Excluded: []string{"h3", "h4"}},
			want:   map[string]string{"n1": "h2", "n2": "h5", "n3": "h1"},
			warn:   "preferred host h3 is excluded",
		},
		{
			name:   "allowlist beats preference",
			policy: PlacementPolicy{Allowlist: []string{"h1", "h2", "h3"}, Preferred: []string{"h5", "h3"}},
			want:   map[string]string{"n1": "h3", "n2": "h1", "n3": "h2"},
			warn:   "preferred host h5 is not in the allowlist",
		},
		{
			name:    "allowlist is strict: never falls back to other hosts",
			policy:  PlacementPolicy{Allowlist: []string{"h1", "h2"}},
			wantErr: ErrNoCapacity,
		},
		{
			name:   "exclusion within the allowlist",
			policy: PlacementPolicy{Allowlist: []string{"h1", "h2", "h3", "h4"}, Excluded: []string{"h5"}},
			want:   map[string]string{"n1": "h4", "n2": "h1", "n3": "h2"},
		},
		{
			name:    "allowlisted and excluded is a contradiction, not silently resolved",
			policy:  PlacementPolicy{Allowlist: []string{"h1", "h2", "h3"}, Excluded: []string{"h3"}},
			wantErr: ErrPolicyConflict,
		},
		{
			name:   "unknown host in policy is warned about",
			policy: PlacementPolicy{Preferred: []string{"hX"}},
			want:   map[string]string{"n1": "h4", "n2": "h5", "n3": "h1"},
			warn:   "unknown host hX",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := PlanCreate(testTopology(3), five, tc.policy)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := assignment(p); !mapsEqual(got, tc.want) {
				t.Errorf("assignment = %v, want %v", got, tc.want)
			}
			if tc.warn != "" && !slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, tc.warn) }) {
				t.Errorf("warnings %v lack %q", p.Warnings, tc.warn)
			}
		})
	}
}

func TestPlanCreateKeepsSparesFree(t *testing.T) {
	hosts := append(threeHosts(), host("spare", 16000, 64))
	hosts[3].Spare = true
	p, err := PlanCreate(testTopology(3), hosts, PlacementPolicy{Preferred: []string{"spare"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range p.Nodes {
		if n.HostID == "spare" {
			t.Fatal("spare host used for creation")
		}
	}
	_, err = PlanCreate(testTopology(3), hosts[1:], PlacementPolicy{})
	var pe *PlacementError
	if !errors.As(err, &pe) || !strings.Contains(pe.Rejected["spare"], "spare") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanCreateValidation(t *testing.T) {
	cases := map[string]func(*Topology){
		"size 2":           func(tp *Topology) { tp.Size = 2 },
		"size 0":           func(tp *Topology) { tp.Size = 0 },
		"no memory limit":  func(tp *Topology) { tp.Resources.Limits.MemoryBytes = 0; tp.Resources.Reservations.MemoryBytes = 0 },
		"no cpu":           func(tp *Topology) { tp.Resources = Resources{Limits: ResourceAmount{MemoryBytes: gib}} },
		"bad resources":    func(tp *Topology) { tp.Resources.Reservations.MilliCPU = 5000 },
		"bad cluster":      func(tp *Topology) { tp.Cluster.Name = "UPPER" },
		"uuid source fail": func(tp *Topology) { tp.NewUUID = func() (string, error) { return "", errors.New("boom") } },
		"uuid source bad":  func(tp *Topology) { tp.NewUUID = func() (string, error) { return "nope", nil } },
		"uuid repeated": func(tp *Topology) {
			tp.NewUUID = func() (string, error) { return "00000000-0000-4000-8000-000000000001", nil }
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tp := testTopology(3)
			mutate(&tp)
			if _, err := PlanCreate(tp, threeHosts(), PlacementPolicy{}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	tp := testTopology(4)
	p, err := PlanCreate(tp, append(threeHosts(), host("h4", 4000, 8)), PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "even") }) {
		t.Errorf("even size not warned: %v", p.Warnings)
	}
}

func TestPlanCreateDefaultUUIDSource(t *testing.T) {
	tp := testTopology(3)
	tp.NewUUID = nil
	p, err := PlanCreate(tp, threeHosts(), PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range p.Nodes {
		if !uuidRe.MatchString(n.NodeUUID) || n.NodeUUID[14] != '4' {
			t.Errorf("random UUID %q is not a v4 UUID", n.NodeUUID)
		}
	}
}

func hasPre(s Step, want Precondition) bool { return slices.Contains(s.Preconditions, want) }

// TestPlanCreateBootstrapAccounts: with accounts configured, the monitoring
// and backup accounts are created on the bootstrap node right after its
// service, before its wait-synced (which reads status through the
// monitoring account, lab E4) and before the first joiner (joiners receive
// them by SST).
func TestPlanCreateBootstrapAccounts(t *testing.T) {
	topo := testTopology(3)
	topo.Cluster.DBUID, topo.Cluster.DBGID = 1001, 1001
	topo.Cluster.Accounts = Accounts{MonitorSecret: "dbcm-inst-monitor-pw", BackupSecret: "dbcm-inst-backup-pw", BackupLogical: true}
	p, err := PlanCreate(topo, threeHosts(), PlacementPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range p.Steps[:8] {
		got = append(got, string(s.Kind)+":"+s.Slot)
	}
	want := []string{"create-volume:n1", "init-identity:n1", "init-volume-dirs:n1", "create-service:n1",
		"exec-sql:n1", "exec-sql:n1", "wait-synced:n1", "create-volume:n2"}
	if !slices.Equal(got, want) {
		t.Fatalf("steps %v\nwant %v", got, want)
	}
	mon, bak := p.Steps[4], p.Steps[5]
	if mon.SQL == nil || *mon.SQL != (SQLStep{Template: SQLCreateMonitorUser, User: DefaultMonitorUser, Host: "localhost", PasswordSecret: "dbcm-inst-monitor-pw"}) {
		t.Errorf("monitor step %+v", mon.SQL)
	}
	if bak.SQL == nil || *bak.SQL != (SQLStep{Template: SQLCreateBackupUser, User: DefaultBackupUser, Host: "%", PasswordSecret: "dbcm-inst-backup-pw", Logical: true}) {
		t.Errorf("backup step %+v", bak.SQL)
	}
	for _, s := range []Step{mon, bak} {
		if s.NodeUUID != p.Nodes[0].NodeUUID || s.Volume != p.Nodes[0].Volume || s.HostID != p.Nodes[0].HostID {
			t.Errorf("exec-sql step not bound to the bootstrap node: %+v", s)
		}
		if slices.ContainsFunc(s.Preconditions, func(c Precondition) bool { return c.Kind == PreNodeSynced }) {
			t.Errorf("exec-sql step %s waits for a node-synced check that needs the account it creates", s.ID)
		}
	}
	if o := stepsOf(p, StepInitVolumeDirs)[0].Owner; *o != (DirOwner{UID: 1001, GID: 1001}) {
		t.Errorf("owner %+v", o)
	}
	if slices.ContainsFunc(p.Warnings, func(w string) bool { return strings.Contains(w, "monitoring account") }) {
		t.Errorf("unexpected warning %v", p.Warnings)
	}
}

func stepsOf(p Plan, k StepKind) []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Kind == k {
			out = append(out, s)
		}
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
