// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

func seqUUIDs(prefix int) func() (string, error) {
	n := 0
	return func() (string, error) {
		n++
		return fmt.Sprintf("00000000-0000-4000-8%03d-%012x", prefix, n), nil
	}
}

func restoreSource() RestoreSource {
	m := validManifest()
	m.Object = "prod-b1.xbs.dbcms"
	return RestoreSource{Manifest: m, ClusterID: testCluster, BackendSecret: "backend-cfg", KeySecret: "restore-key"}
}

func prodInventory() Inventory {
	return Inventory{ClusterIDs: []string{testCluster}, ClusterNames: []string{"prod"}, Networks: []string{"dbcm-prod-net"},
		Volumes: []string{"dbcm-prod-n1-00000000"}, NodeUUIDs: []string{uuid1, uuid2, uuid3},
		ServiceNames: []string{"dbcm-prod-n1", "dbcm-prod-n2", "dbcm-prod-n3"}}
}

func clusterRestoreRequest(base string) ClusterRestoreRequest {
	host := func(id string) galera.Host {
		return galera.Host{ID: id, Ready: true, Active: true, Capacity: galera.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}}
	}
	return ClusterRestoreRequest{
		OpID: "op-r1", ApprovalID: "appr-1", Source: restoreSource(),
		Target: galera.Topology{
			Cluster: galera.Cluster{InstallationID: testInst, ClusterID: "cluster-restored", Name: "restored",
				Image: "mariadb:11.4", Network: "dbcm-restored-net", RootPasswordSecret: "restored-root"},
			Size:      3,
			Resources: galera.Resources{Limits: galera.ResourceAmount{MilliCPU: 1000, MemoryBytes: 2 * gib}, Reservations: galera.ResourceAmount{MilliCPU: 500, MemoryBytes: gib}},
			NewUUID:   seqUUIDs(1),
		},
		Hosts: []galera.Host{host("h1"), host("h2"), host("h3")},
		Bindings: runner.GaleraBindings{InstallationID: testInst, VolumeBase: base,
			TokenIDs: map[string]string{"n1": "tok-1", "n2": "tok-2", "n3": "tok-3"}, StepTimeout: time.Minute},
		JobImage: "dbcm/job:1", Job: agent.JobResources{NanoCPUs: 1e9, MemoryBytes: gib},
	}
}

func TestClusterRestorePlanInsertsRestoreBeforeFirstStart(t *testing.T) {
	base := t.TempDir()
	res, err := ClusterRestorePlan(clusterRestoreRequest(base), prodInventory())
	if err != nil {
		t.Fatal(err)
	}
	op := res.Operation
	var order []string
	for _, s := range op.Steps {
		order = append(order, string(s.Command)+"@"+s.Target)
	}
	ri := slices.IndexFunc(op.Steps, func(s runner.Step) bool { return s.Command == agent.KindRunRestore })
	ii := slices.IndexFunc(op.Steps, func(s runner.Step) bool { return s.Command == agent.KindInitIdentity })
	di := slices.IndexFunc(op.Steps, func(s runner.Step) bool { return s.Command == agent.KindInitVolumeDirs })
	ci := slices.IndexFunc(op.Steps, func(s runner.Step) bool { return s.Command == agent.KindCreateService })
	// The job extracts into data/ as the database user, so the directories
	// exist before it runs.
	if !(ii < di && di+1 == ri && ri < ci) || op.Steps[ri+1].Command != "" || op.Steps[ri+2].Command != agent.KindRemoveJob {
		t.Fatalf("order %v", order)
	}
	// The first start depends on the job removal, not on init-volume-dirs.
	if !slices.ContainsFunc(op.Steps[ci].Preconditions, func(c runner.Condition) bool {
		return c.Kind == runner.CondStepSucceeded && c.StepID == op.Steps[ri+2].ID
	}) {
		t.Fatalf("create-service preconditions %+v", op.Steps[ci].Preconditions)
	}
	var a agent.RunRestoreArgs
	if err := json.Unmarshal(op.Steps[ri].Args, &a); err != nil {
		t.Fatal(err)
	}
	if a.Expected.ClusterID != "cluster-restored" || a.SourceClusterID != testCluster || a.Mode != agent.RestorePhysical ||
		a.ApprovalID != "appr-1" || a.TargetVolume != res.Plan.Nodes[0].Volume {
		t.Fatalf("args %+v", a)
	}
	requireApprovalOnDestructive(t, op, "appr-1")
	// The restored node bootstraps a new cluster under the new name.
	var spec agent.ServiceSpec
	_ = json.Unmarshal(op.Steps[ci].Args, &spec)
	if !slices.Contains(spec.Args, galera.BootstrapFlag) || !slices.Contains(spec.Args, "--wsrep_cluster_name=restored") {
		t.Fatalf("bootstrap spec args %v", spec.Args)
	}
	for _, arg := range spec.Args {
		if strings.Contains(arg, "dbcm-prod-") {
			t.Fatalf("restored cluster references production: %s", arg)
		}
	}
	requireAgentAccepts(t, op, base)
}

func requireApprovalOnDestructive(t *testing.T, op runner.Operation, id string) {
	t.Helper()
	n := 0
	for _, s := range op.Steps {
		if !destructive(s) {
			continue
		}
		n++
		if !slices.ContainsFunc(s.Preconditions, func(c runner.Condition) bool { return c.Kind == CondApproved && c.Target == id }) {
			t.Fatalf("destructive step %s (%s) lacks approval", s.ID, s.Command)
		}
	}
	if n == 0 {
		t.Fatal("no destructive steps found")
	}
}

func TestClusterRestoreSafetyRefusals(t *testing.T) {
	base := t.TempDir()
	cases := map[string]struct {
		mut  func(*ClusterRestoreRequest)
		want error
	}{
		"same cluster id": {func(r *ClusterRestoreRequest) { r.Target.Cluster.ClusterID = testCluster }, ErrProductionTarget},
		"live cluster id": {func(r *ClusterRestoreRequest) {
			r.Target.Cluster.ClusterID = "cluster-x"
			r.Source.ClusterID = "cluster-x"
			r.Source.Manifest.ClusterID = "cluster-x"
		}, ErrProductionTarget},
		"production name":    {func(r *ClusterRestoreRequest) { r.Target.Cluster.Name = "prod" }, ErrProductionTarget},
		"production network": {func(r *ClusterRestoreRequest) { r.Target.Cluster.Network = "dbcm-prod-net" }, ErrProductionTarget},
		"production node uuid": {func(r *ClusterRestoreRequest) {
			gen := seqUUIDs(1)
			first := true
			r.Target.NewUUID = func() (string, error) {
				if first {
					first = false
					return uuid1, nil
				}
				return gen()
			}
		}, ErrProductionTarget},
		"mismatching identity": {func(r *ClusterRestoreRequest) { r.Source.ClusterID = "cluster-other" }, ErrIdentityMismatch},
		"no approval":          {func(r *ClusterRestoreRequest) { r.ApprovalID = "" }, ErrApprovalRequired},
		"logical backup": {func(r *ClusterRestoreRequest) {
			r.Source.Manifest.Method, r.Source.Manifest.Database = MethodLogical, "shop"
		}, ErrInvalidRequest},
		"no key secret": {func(r *ClusterRestoreRequest) { r.Source.KeySecret = "" }, ErrInvalidRequest},
	}
	for name, c := range cases {
		r := clusterRestoreRequest(base)
		c.mut(&r)
		inv := prodInventory()
		if name == "live cluster id" {
			inv.ClusterIDs = append(inv.ClusterIDs, "cluster-restored")
			r.Target.Cluster.ClusterID = "cluster-restored"
		}
		if _, err := ClusterRestorePlan(r, inv); !errors.Is(err, c.want) {
			t.Fatalf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

func inspectionRequest(base string) InspectionRequest {
	return InspectionRequest{
		OpID: "op-i1", SessionID: "s1", ApprovalID: "appr-2", Source: restoreSource(),
		Host: galera.Host{ID: "h4"}, InstallationID: testInst, VolumeBase: base,
		Image: "mariadb:11.4", JobImage: "dbcm/job:1", Network: "dbcm-inspect-s1-net", InitSecret: "inspect-s1-init",
		Resources: galera.Resources{Limits: galera.ResourceAmount{MilliCPU: 1000, MemoryBytes: gib}},
		Job:       agent.JobResources{NanoCPUs: 1e9, MemoryBytes: gib},
		TTL:       4 * time.Hour, Now: t0, TokenID: "tok-i1", NewUUID: seqUUIDs(2),
	}
}

func TestInspectionRestoreIsIsolated(t *testing.T) {
	base := t.TempDir()
	res, err := InspectionRestorePlan(inspectionRequest(base), prodInventory())
	if err != nil {
		t.Fatal(err)
	}
	sp := res.Spec
	for _, want := range []string{"--wsrep-on=OFF", "--wsrep-provider=none", "--wsrep-cluster-address=", "--wsrep-cluster-name=inspect-s1",
		"--port=3307", "--skip-slave-start", "--event-scheduler=OFF", "--skip-log-bin", "--init-file=/run/secrets/inspect-s1-init",
		"--socket=" + galera.SocketPath} {
		if !slices.Contains(sp.Args, want) {
			t.Fatalf("spec args %v lack %s", sp.Args, want)
		}
	}
	for _, a := range sp.Args {
		if strings.Contains(a, galera.BootstrapFlag) || strings.Contains(a, "gcomm://") || strings.Contains(a, "dbcm-prod") {
			t.Fatalf("inspection arg %q could reach production", a)
		}
	}
	if !slices.Equal(sp.Networks, []string{"dbcm-inspect-s1-net"}) || !slices.Equal(sp.Secrets, []string{"inspect-s1-init"}) {
		t.Fatalf("networks %v secrets %v (no production credentials)", sp.Networks, sp.Secrets)
	}
	if sp.Resources == nil || sp.Resources.Limits.MemoryBytes != gib || sp.Constraints[0] != "node.id==h4" {
		t.Fatalf("limits/placement %+v %v", sp.Resources, sp.Constraints)
	}
	s := res.Session
	if s.ClusterID != "inspect-s1" || !s.ExpiresAt.Equal(t0.Add(4*time.Hour)) || !strings.HasPrefix(s.Volume, "dbcm-inspect-s1-") {
		t.Fatalf("session %+v", s)
	}
	requireApprovalOnDestructive(t, res.Operation, "appr-2")
	var cmds []string
	for _, st := range res.Operation.Steps[:3] {
		cmds = append(cmds, string(st.Command))
	}
	if !slices.Equal(cmds, []string{"create-volume", "init-identity", "init-volume-dirs"}) || res.Operation.Steps[3].Command != agent.KindRunRestore {
		t.Fatalf("inspection prepares the volume directories before the restore job: %v", cmds)
	}
	var dirs agent.InitVolumeDirsArgs
	if err := json.Unmarshal(res.Operation.Steps[2].Args, &dirs); err != nil || dirs.UID != galera.DefaultDBUID || dirs.Expected.ClusterID != "inspect-s1" {
		t.Fatalf("init-volume-dirs args %+v (%v)", dirs, err)
	}
	last := res.Operation.Steps[len(res.Operation.Steps)-1]
	if !slices.Contains(condKinds(last), CondRestoreVerified) {
		t.Fatalf("last step %+v", last)
	}
	requireAgentAccepts(t, res.Operation, base)
}

func TestInspectionRefusals(t *testing.T) {
	base := t.TempDir()
	cases := map[string]struct {
		mut  func(*InspectionRequest)
		want error
	}{
		"production network": {func(r *InspectionRequest) { r.Network = "dbcm-prod-net" }, ErrProductionTarget},
		"no ttl":             {func(r *InspectionRequest) { r.TTL = 0 }, ErrInvalidRequest},
		"ttl too long":       {func(r *InspectionRequest) { r.TTL = 30 * 24 * time.Hour }, ErrInvalidRequest},
		"production port":    {func(r *InspectionRequest) { r.Port = 3306 }, ErrInvalidRequest},
		"no limits":          {func(r *InspectionRequest) { r.Resources = galera.Resources{} }, ErrInvalidRequest},
		"no approval":        {func(r *InspectionRequest) { r.ApprovalID = "" }, ErrApprovalRequired},
		"wrong cluster":      {func(r *InspectionRequest) { r.Source.ClusterID = "cluster-other" }, ErrIdentityMismatch},
		"bad session":        {func(r *InspectionRequest) { r.SessionID = "S/1" }, ErrInvalidRequest},
	}
	for name, c := range cases {
		r := inspectionRequest(base)
		c.mut(&r)
		if _, err := InspectionRestorePlan(r, prodInventory()); !errors.Is(err, c.want) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestTeardownPlan(t *testing.T) {
	base := t.TempDir()
	res, err := InspectionRestorePlan(inspectionRequest(base), prodInventory())
	if err != nil {
		t.Fatal(err)
	}
	s := res.Session
	op, err := TeardownPlan("op-t1", testInst, s, prodInventory())
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Steps) != 2 || op.Steps[1].Command != agent.KindRemoveVolume {
		t.Fatalf("steps %+v", op.Steps)
	}
	s.ServiceID = "svc-9"
	op, _ = TeardownPlan("op-t1", testInst, s, prodInventory())
	var kinds []agent.Kind
	for _, st := range op.Steps {
		kinds = append(kinds, st.Command)
	}
	if !slices.Equal(kinds, []agent.Kind{agent.KindRemoveJob, agent.KindRemoveService, agent.KindRemoveVolume}) {
		t.Fatalf("kinds %v", kinds)
	}
	var rv agent.RemoveVolumeArgs
	_ = json.Unmarshal(op.Steps[2].Args, &rv)
	if rv.Expected.ClusterID != "inspect-s1" || rv.Expected.NodeUUID != s.NodeUUID {
		t.Fatalf("remove-volume does not verify the inspection identity: %+v", rv)
	}
	requireAgentAccepts(t, op, base)

	// A session pointing at production is refused.
	prod := s
	prod.ClusterID, prod.Volume = testCluster, "dbcm-prod-n1-00000000"
	if _, err := TeardownPlan("op-t2", testInst, prod, prodInventory()); !errors.Is(err, ErrProductionTarget) {
		t.Fatalf("got %v", err)
	}
	sneaky := s
	sneaky.Volume = "dbcm-prod-n1-00000000"
	if _, err := TeardownPlan("op-t3", testInst, sneaky, prodInventory()); !errors.Is(err, ErrProductionTarget) {
		t.Fatalf("got %v", err)
	}
	if got := ExpiredSessions([]InspectionSession{s}, s.ExpiresAt.Add(-time.Second)); len(got) != 0 {
		t.Fatal("expired early")
	}
	if got := ExpiredSessions([]InspectionSession{s}, s.ExpiresAt); len(got) != 1 {
		t.Fatal("not expired at expiry")
	}
}

func TestRenderInspectionInit(t *testing.T) {
	pw, err := NewInspectionPassword()
	if err != nil {
		t.Fatal(err)
	}
	sql, err := RenderInspectionInit(pw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"account_locked', true", "RESET SLAVE ALL", "CREATE USER IF NOT EXISTS 'dbcm_inspect'@'%'", "GRANT SELECT"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("init SQL lacks %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "ALL PRIVILEGES") || strings.Contains(sql, "INSERT") {
		t.Fatal("inspection account must be read-only")
	}
	if _, err := RenderInspectionInit("x'; DROP DATABASE shop; --"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("got %v", err)
	}
}

func TestSelectInspectionHost(t *testing.T) {
	cap := galera.ResourceAmount{MilliCPU: 4000, MemoryBytes: 8 * gib}
	h := func(id string, disk int64, approved, prod bool) RestoreHost {
		return RestoreHost{Host: galera.Host{ID: id, Ready: true, Active: true, Capacity: cap}, FreeDiskBytes: disk, Approved: approved, Production: prod}
	}
	hosts := []RestoreHost{h("h1", 500*gib, false, true), h("h2", 100*gib, false, false), h("h3", 20*gib, true, false)}
	demand := galera.ResourceAmount{MilliCPU: 1000, MemoryBytes: gib}
	got, err := SelectInspectionHost(hosts, false, demand, 50*gib)
	if err != nil || got.ID != "h2" {
		t.Fatalf("auto: %s %v (prefer non-production with room)", got.ID, err)
	}
	if _, err := SelectInspectionHost(hosts, true, demand, 50*gib); err == nil {
		t.Fatal("approved-only mode used an unapproved host")
	}
	got, err = SelectInspectionHost(hosts, true, demand, 10*gib)
	if err != nil || got.ID != "h3" {
		t.Fatalf("approved: %s %v", got.ID, err)
	}
}
