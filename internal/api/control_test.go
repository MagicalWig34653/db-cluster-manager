// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// fakeControl records commands and returns configured results.
type fakeControl struct {
	mu       sync.Mutex
	hosts    []Host
	calls    []string
	err      error
	diag     Diagnosis
	lastReq  CreateClusterRequest
	approval OperationApproval
}

func (f *fakeControl) record(actor authz.Subject, format string, a ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, actor.ID+":"+fmt.Sprintf(format, a...))
	return f.err
}

func (f *fakeControl) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeControl) Hosts(context.Context) ([]Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Host(nil), f.hosts...), nil
}

func (f *fakeControl) ConnectSwarm(_ context.Context, a authz.Subject, in SwarmInput) (Swarm, error) {
	if err := f.record(a, "connect %s %s", in.Name, in.DockerHost); err != nil {
		return Swarm{}, err
	}
	return Swarm{ID: "swarm-new", Name: in.Name, State: "bogus"}, nil
}

func (f *fakeControl) SyncSwarm(_ context.Context, a authz.Subject, id string) error {
	return f.record(a, "sync %s", id)
}

func (f *fakeControl) DeployAgents(_ context.Context, a authz.Subject, id string) (AgentDeployment, error) {
	if err := f.record(a, "deploy %s", id); err != nil {
		return AgentDeployment{}, err
	}
	return AgentDeployment{ServiceID: "svc1", Labeled: []string{"n1"}}, nil
}

func (f *fakeControl) SetEnrollment(_ context.Context, a authz.Subject, sw, node string, enroll bool) (Host, error) {
	if err := f.record(a, "enroll %s/%s %v", sw, node, enroll); err != nil {
		return Host{}, err
	}
	e := EnrollmentRevoked
	if enroll {
		e = EnrollmentEnrolled
	}
	return Host{Swarm: sw, NodeID: node, Enrollment: e}, nil
}

func (f *fakeControl) RevokeAgent(_ context.Context, a authz.Subject, sw, node, runtime string) (Host, error) {
	if err := f.record(a, "revoke-agent %s/%s/%s", sw, node, runtime); err != nil {
		return Host{}, err
	}
	return Host{Swarm: sw, NodeID: node, Enrollment: EnrollmentEnrolled, Agents: []HostAgentRuntime{{Runtime: runtime, Capabilities: []string{}, Identity: false}}}, nil
}

func (f *fakeControl) CreateCluster(_ context.Context, a authz.Subject, req CreateClusterRequest) (OperationRef, error) {
	f.mu.Lock()
	f.lastReq = req
	f.mu.Unlock()
	if err := f.record(a, "create %s/%s", req.Swarm, req.Name); err != nil {
		return OperationRef{}, err
	}
	return OperationRef{ID: "op-1", Kind: "galera/create"}, nil
}

func (f *fakeControl) ReplaceNode(_ context.Context, a authz.Subject, c, slot string) (OperationRef, error) {
	if err := f.record(a, "replace %s/%s", c, slot); err != nil {
		return OperationRef{}, err
	}
	return OperationRef{ID: "op-2"}, nil
}

func (f *fakeControl) DeleteCluster(_ context.Context, a authz.Subject, c string) (ClusterDeletion, error) {
	if err := f.record(a, "delete %s", c); err != nil {
		return ClusterDeletion{}, err
	}
	return ClusterDeletion{ID: "op-d", Kind: OperationKindClusterDelete, Approval: OperationApproval{Digest: digestA}}, nil
}

func (f *fakeControl) OutageDiagnosis(_ context.Context, a authz.Subject, c string) (Diagnosis, error) {
	if err := f.record(a, "diagnose %s", c); err != nil {
		return Diagnosis{}, err
	}
	return f.diag, nil
}

func (f *fakeControl) OutageDiagnosisRecover(_ context.Context, a authz.Subject, c string) (Diagnosis, error) {
	if err := f.record(a, "diagnose-recover %s", c); err != nil {
		return Diagnosis{}, err
	}
	return f.diag, nil
}

func (f *fakeControl) OutageRecovery(_ context.Context, a authz.Subject, c, cand string) (RecoveryStarted, error) {
	if err := f.record(a, "recover %s %s", c, cand); err != nil {
		return RecoveryStarted{}, err
	}
	return RecoveryStarted{ID: "op-rec", Kind: "galera/full-outage-recovery", Candidate: cand,
		Approval: OperationApproval{Digest: digestA}, Diagnosis: f.diag}, nil
}

func (f *fakeControl) ResumeOperation(_ context.Context, a authz.Subject, id string) error {
	return f.record(a, "resume %s", id)
}

func (f *fakeControl) AbortOperation(_ context.Context, a authz.Subject, id string) error {
	return f.record(a, "abort %s", id)
}

func (f *fakeControl) ApproveOperation(_ context.Context, a authz.Subject, id, digest string) (OperationApproval, error) {
	if err := f.record(a, "approve %s %s", id, digest); err != nil {
		return OperationApproval{}, err
	}
	return OperationApproval{Digest: digest, ApprovedBy: a.ID}, nil
}

type fakeBackups struct {
	fakeControl
	summaries []BackupSummary
	lastPol   BackupPolicy
	lastInsp  InspectionInput
}

func (f *fakeBackups) Backups(context.Context) ([]BackupSummary, error) {
	return append([]BackupSummary(nil), f.summaries...), nil
}

func (f *fakeBackups) SetBackupPolicy(_ context.Context, a authz.Subject, c string, p BackupPolicy) (BackupSummary, error) {
	f.lastPol = p
	if err := f.record(a, "policy %s %s", c, p.Schedule); err != nil {
		return BackupSummary{}, err
	}
	return BackupSummary{Cluster: c, Policy: p.Schedule, BackupPolicy: &p}, nil
}

func (f *fakeBackups) StartBackup(_ context.Context, a authz.Subject, c string) (OperationRef, error) {
	if err := f.record(a, "backup %s", c); err != nil {
		return OperationRef{}, err
	}
	return OperationRef{ID: "op-b", Kind: "dbbackup/backup"}, nil
}

func (f *fakeBackups) ProposePointInTime(_ context.Context, a authz.Subject, c string, at time.Time) (PointInTimeProposal, error) {
	if err := f.record(a, "pitr %s %s", c, at.UTC().Format(time.RFC3339)); err != nil {
		return PointInTimeProposal{}, err
	}
	return PointInTimeProposal{Cluster: c, At: at, Covered: true, Backup: "b1", Segments: 2, ApprovalRequired: true}, nil
}

func (f *fakeBackups) StartBinlogArchive(_ context.Context, a authz.Subject, c string) (OperationRef, error) {
	if err := f.record(a, "binlog-archive %s", c); err != nil {
		return OperationRef{}, err
	}
	return OperationRef{ID: "op-a", Kind: "dbbackup/binlog-archive"}, nil
}

func (f *fakeBackups) StartInspection(_ context.Context, a authz.Subject, in InspectionInput) (InspectionStarted, error) {
	f.lastInsp = in
	if err := f.record(a, "inspect %s", in.Cluster); err != nil {
		return InspectionStarted{}, err
	}
	return InspectionStarted{ID: "op-i", Kind: "dbbackup/inspection", Session: "s1"}, nil
}

type fakeSettings struct {
	fakeControl
	cur  NotificationSettings
	last NotificationSettingsInput
}

func (f *fakeSettings) NotificationSettings(context.Context) (NotificationSettings, error) {
	return f.cur, f.err
}

func (f *fakeSettings) SetNotificationSettings(_ context.Context, a authz.Subject, in NotificationSettingsInput) (NotificationSettings, error) {
	f.last = in
	if err := f.record(a, "settings"); err != nil {
		return NotificationSettings{}, err
	}
	f.cur.Webhook = WebhookSettingsView{Enabled: in.Webhook.Enabled, URL: in.Webhook.URL, SecretSet: in.Webhook.Secret != nil}
	return f.cur, nil
}

func (f *fakeSettings) TestNotification(_ context.Context, a authz.Subject, ch string) (NotificationTest, error) {
	if err := f.record(a, "test %s", ch); err != nil {
		return NotificationTest{}, err
	}
	return NotificationTest{Channel: ch, Delivered: true}, nil
}

type controlFixture struct {
	*fixture
	ctl *fakeControl
	bak *fakeBackups
	set *fakeSettings
}

func newControlFixture(t *testing.T) *controlFixture {
	f := newFixture(t)
	cf := &controlFixture{fixture: f, ctl: &fakeControl{}, bak: &fakeBackups{}, set: &fakeSettings{}}
	f.srv.cfg.Control, f.srv.cfg.Backups, f.srv.cfg.Settings = cf.ctl, cf.bak, cf.set
	f.inventory.swarms = []Swarm{{ID: "s1", Name: "one", State: StateHealthy}, {ID: "s2", Name: "two", State: StateHealthy}}
	f.inventory.clusters = []Cluster{
		{ID: "c1", Swarm: "s1", Name: "alpha", State: StateHealthy, Nodes: []ClusterNode{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}}},
		{ID: "c2", Swarm: "s2", Name: "beta", State: StateHealthy, Nodes: []ClusterNode{{ID: "n1"}}},
	}
	f.inventory.operations = []Operation{
		{ID: "op-c1", Kind: "galera/create", Swarm: "s1", Cluster: "c1", State: OperationPending},
		{ID: "op-r1", Kind: "dbbackup/inspection", Swarm: "s1", Cluster: "c1", State: OperationPending,
			Approval: &OperationApproval{Digest: strings.Repeat("a", 64)}},
		{ID: "op-d1", Kind: OperationKindClusterDelete, Swarm: "s1", Cluster: "c1", State: OperationPending,
			Approval: &OperationApproval{Digest: strings.Repeat("a", 64)}},
	}
	cf.ctl.hosts = []Host{
		{Swarm: "s1", NodeID: "h1", Hostname: "one", Enrollment: EnrollmentDiscovered},
		{Swarm: "s1", NodeID: "h2", Enrollment: "weird"},
		{Swarm: "s2", NodeID: "h3", Enrollment: EnrollmentEnrolled},
	}
	cf.bak.summaries = []BackupSummary{{Cluster: "c1", Swarm: "s1"}, {Cluster: "c2", Swarm: "s2"}}
	return cf
}

var digestA = strings.Repeat("a", 64)

func auditsOf(f *fixture, act authz.Action, out authz.Outcome, typ authz.ResourceType) int {
	n := 0
	for _, r := range f.sink.Records() {
		if r.Action == act && r.Outcome == out && r.Resource.Type == typ {
			n++
		}
	}
	return n
}

type route struct {
	method, path string
	body         any
}

var commandRoutes = []route{
	{"GET", "/api/v1/hosts", nil},
	{"POST", "/api/v1/hosts/h1/enroll", nil},
	{"POST", "/api/v1/hosts/h1/revoke", nil},
	{"POST", "/api/v1/swarms", map[string]string{"name": "x", "dockerHost": "unix:///var/run/docker.sock"}},
	{"POST", "/api/v1/swarms/s1/sync", nil},
	{"POST", "/api/v1/swarms/s1/agents/deploy", nil},
	{"POST", "/api/v1/clusters", map[string]any{"swarm": "s1", "name": "gamma", "size": 3, "image": "mariadb:11", "network": "net", "rootPasswordSecret": "pw"}},
	{"POST", "/api/v1/clusters/c1/nodes/n1/replace", map[string]any{}},
	{"GET", "/api/v1/clusters/c1/outage-diagnosis", nil},
	{"POST", "/api/v1/clusters/c1/outage-recovery", map[string]string{"candidate": "n1"}},
	{"POST", "/api/v1/clusters/c1/outage-diagnosis", nil},
	{"POST", "/api/v1/operations/op-c1/resume", nil},
	{"POST", "/api/v1/operations/op-c1/abort", nil},
	{"POST", "/api/v1/operations/op-r1/approve", map[string]string{"digest": digestA}},
	{"POST", "/api/v1/operations/op-d1/approve", map[string]string{"digest": digestA}},
	{"DELETE", "/api/v1/clusters/c2", nil},
	{"GET", "/api/v1/backups", nil},
	{"PUT", "/api/v1/clusters/c1/backup-policy", map[string]any{"enabled": true, "schedule": "0 2 * * *"}},
	{"POST", "/api/v1/clusters/c1/backups", nil},
	{"POST", "/api/v1/restores/inspect", map[string]string{"cluster": "c1"}},
	{"POST", "/api/v1/clusters/c1/binlog-archives", nil},
	{"GET", "/api/v1/clusters/c1/pitr-proposal?at=2026-10-22T01:30:00Z", nil},
	{"GET", "/api/v1/settings/notifications", nil},
	{"PUT", "/api/v1/settings/notifications", map[string]any{"webhook": map[string]any{"enabled": false, "url": ""}, "email": map[string]any{"enabled": false, "recipients": []string{}}}},
	{"POST", "/api/v1/settings/notifications/test", map[string]string{"channel": "webhook"}},
}

func TestCommandRoutesNeedSession(t *testing.T) {
	cf := newControlFixture(t)
	c := cf.client()
	for _, rt := range commandRoutes {
		wantError(t, c.do(rt.method, rt.path, rt.body), http.StatusUnauthorized, errUnauthenticated)
	}
	if n := len(cf.ctl.Calls()) + len(cf.bak.Calls()) + len(cf.set.Calls()); n != 0 {
		t.Fatalf("services called without session: %d", n)
	}
}

func TestCommandRoutesUnavailableWithoutServices(t *testing.T) {
	f := newFixture(t)
	f.inventory.swarms = []Swarm{{ID: "s1"}}
	f.inventory.clusters = []Cluster{{ID: "c1", Swarm: "s1", Nodes: []ClusterNode{{ID: "n1"}}}, {ID: "c2", Swarm: "s1"}}
	f.inventory.operations = []Operation{{ID: "op-c1", Swarm: "s1", Cluster: "c1"}, {ID: "op-r1", Swarm: "s1", Cluster: "c1", Approval: &OperationApproval{Digest: digestA}},
		{ID: "op-d1", Kind: OperationKindClusterDelete, Swarm: "s1", Cluster: "c1", Approval: &OperationApproval{Digest: digestA}}}
	admin, _, _ := f.setupAdmin("admin")
	for _, rt := range commandRoutes {
		r := admin.do(rt.method, rt.path, rt.body)
		if r.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d %s, want 503", rt.method, rt.path, r.Code, r.Body.String())
		}
	}
}

func TestCommandRoutesRejectCrossOrigin(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	for _, rt := range commandRoutes {
		if rt.method == "GET" {
			continue
		}
		admin.origin = "https://evil.example"
		wantError(t, admin.do(rt.method, rt.path, rt.body), http.StatusForbidden, errOriginRejected)
		admin.origin = ""
		wantError(t, admin.do(rt.method, rt.path, rt.body), http.StatusForbidden, errOriginRejected)
		admin.origin = testOrigin
	}
	if n := len(cf.ctl.Calls()) + len(cf.bak.Calls()) + len(cf.set.Calls()); n != 0 {
		t.Fatalf("services called cross-origin: %d", n)
	}
}

func TestAdminRunsEveryCommandAudited(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	before := len(cf.sink.Records())
	writes := 0
	for _, rt := range commandRoutes {
		r := admin.do(rt.method, rt.path, rt.body)
		if r.Code >= 300 {
			t.Errorf("%s %s = %d %s", rt.method, rt.path, r.Code, r.Body.String())
		}
		if rt.method != "GET" && !strings.HasSuffix(rt.path, "/read") {
			writes++
		}
	}
	if got := len(cf.sink.Records()) - before; got != writes {
		t.Fatalf("audit records = %d, want one per state change (%d)", got, writes)
	}
}

func TestHostsFilteredAndNormalized(t *testing.T) {
	cf := newControlFixture(t)
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	var list List[Host]
	r := viewer.do("GET", "/api/v1/hosts", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &list)
	if len(list.Items) != 2 || list.Items[0].NodeID != "h1" || list.Items[1].Enrollment != EnrollmentDiscovered {
		t.Fatalf("%+v", list.Items)
	}
}

func TestHostEnrollmentAuthorization(t *testing.T) {
	cf := newControlFixture(t)
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	// Readable but not permitted: 403, audited as denied.
	wantError(t, viewer.do("POST", "/api/v1/hosts/h1/enroll", nil), http.StatusForbidden, errForbidden)
	if auditsOf(cf.fixture, authz.ActionUpdate, authz.OutcomeDenied, authz.ResourceHost) != 1 {
		t.Fatal("denied enrollment must be audited")
	}
	// Not even readable: 404.
	wantError(t, viewer.do("POST", "/api/v1/hosts/h3/enroll", nil), http.StatusNotFound, errNotFound)
	wantError(t, viewer.do("POST", "/api/v1/hosts/nope/enroll", nil), http.StatusNotFound, errNotFound)

	admin, _, _ := cf.setupAdmin("admin")
	var h Host
	r := admin.do("POST", "/api/v1/hosts/h1/enroll", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &h)
	if h.Enrollment != EnrollmentEnrolled {
		t.Fatalf("%+v", h)
	}
	wantStatus(t, admin.do("POST", "/api/v1/hosts/h1/revoke", map[string]any{}), http.StatusOK)
	wantError(t, admin.do("POST", "/api/v1/hosts/h1/revoke", map[string]any{"x": 1}), http.StatusBadRequest, errBadRequest)
	calls := cf.ctl.Calls()
	if len(calls) != 2 || calls[0] != "admin:enroll s1/h1 true" || calls[1] != "admin:enroll s1/h1 false" {
		t.Fatalf("calls %v", calls)
	}
	// An ambiguous node ID is refused, not guessed.
	cf.ctl.hosts = append(cf.ctl.hosts, Host{Swarm: "s2", NodeID: "h1"})
	wantError(t, admin.do("POST", "/api/v1/hosts/h1/enroll", nil), http.StatusConflict, errConflict)
}

func TestRuntimeAgentRevocationAuthorization(t *testing.T) {
	cf := newControlFixture(t)
	viewer := cf.loggedIn("viewer", authz.RoleViewer, authz.SwarmScope("s1"))
	operator := cf.loggedIn("operator", authz.RoleOperator, authz.SwarmScope("s1"))
	path := "/api/v1/hosts/h1/agents/native/revoke"
	wantError(t, viewer.do("POST", path, nil), http.StatusForbidden, errForbidden)
	wantError(t, operator.do("POST", path, nil), http.StatusForbidden, errForbidden)
	wantError(t, viewer.do("POST", "/api/v1/hosts/h3/agents/native/revoke", nil), http.StatusNotFound, errNotFound)
	admin, _, _ := cf.setupAdmin("admin")
	r := admin.do("POST", path, map[string]any{})
	wantStatus(t, r, http.StatusOK)
	var h Host
	r.decode(t, &h)
	if h.Enrollment != EnrollmentEnrolled || len(h.Agents) != 1 || h.Agents[0].Identity {
		t.Fatalf("updated host: %+v", h)
	}
	wantError(t, admin.do("POST", "/api/v1/hosts/h1/agents/other/revoke", nil), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", path, map[string]any{"unexpected": true}), http.StatusBadRequest, errBadRequest)
	if calls := cf.ctl.Calls(); len(calls) != 1 || calls[0] != "admin:revoke-agent s1/h1/native" {
		t.Fatalf("calls %v", calls)
	}
}

func TestSwarmCommands(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	for _, bad := range []map[string]string{
		{"name": "", "dockerHost": "unix:///x"},
		{"name": "x", "dockerHost": "http://x"},
		{"name": "x\x01y", "dockerHost": "unix:///x"},
		{"name": "x", "dockerHost": "tcp://"},
	} {
		wantError(t, admin.do("POST", "/api/v1/swarms", bad), http.StatusBadRequest, errBadRequest)
	}
	var sw Swarm
	r := admin.do("POST", "/api/v1/swarms", map[string]string{"name": "lab", "dockerHost": "tcp://10.0.0.1:2375"})
	wantStatus(t, r, http.StatusCreated)
	r.decode(t, &sw)
	if sw.ID != "swarm-new" || sw.State != StateUnknown {
		t.Fatalf("%+v", sw)
	}
	wantStatus(t, admin.do("POST", "/api/v1/swarms/s1/sync", nil), http.StatusNoContent)
	wantError(t, admin.do("POST", "/api/v1/swarms/zz/sync", nil), http.StatusNotFound, errNotFound)
	var dep AgentDeployment
	r = admin.do("POST", "/api/v1/swarms/s2/agents/deploy", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &dep)
	if dep.ServiceID != "svc1" || dep.Unlabeled == nil || dep.Failed == nil {
		t.Fatalf("%+v", dep)
	}
	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	wantError(t, op.do("POST", "/api/v1/swarms", map[string]string{"name": "x", "dockerHost": "unix:///x"}), http.StatusForbidden, errForbidden)
	// Operators lack swarm operate: readable, so 403.
	wantError(t, op.do("POST", "/api/v1/swarms/s1/sync", nil), http.StatusForbidden, errForbidden)
	wantError(t, op.do("POST", "/api/v1/swarms/s2/sync", nil), http.StatusNotFound, errNotFound)
}

func TestClusterCreate(t *testing.T) {
	cf := newControlFixture(t)
	body := map[string]any{
		"swarm": "s1", "name": "gamma", "size": 3, "image": "mariadb:11.4", "network": "dbnet", "rootPasswordSecret": "root-pw",
		"resources":   map[string]any{"limits": map[string]any{"milli_cpu": 1000, "memory_bytes": 1 << 30}, "reservations": map[string]any{}},
		"placement":   map[string]any{"allowlist": []string{"h1"}, "reserve": map[string]any{"milli_cpu": 100}},
		"autoReplace": true,
	}
	op := cf.loggedIn("op", authz.RoleOperator, authz.Global)
	wantError(t, op.do("POST", "/api/v1/clusters", body), http.StatusForbidden, errForbidden)
	if len(cf.ctl.Calls()) != 0 {
		t.Fatal("denied create reached the service")
	}
	admin, _, _ := cf.setupAdmin("admin")
	var ref OperationRef
	r := admin.do("POST", "/api/v1/clusters", body)
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &ref)
	if ref.ID != "op-1" {
		t.Fatalf("%+v", ref)
	}
	got := cf.ctl.lastReq
	if got.Resources.Limits.MemoryBytes != 1<<30 || got.Placement.Allowlist[0] != "h1" || !got.AutoReplace || got.Placement.Reserve.MilliCPU != 100 {
		t.Fatalf("request not passed through: %+v", got)
	}
	for name, mut := range map[string]func(map[string]any){
		"no swarm":   func(m map[string]any) { delete(m, "swarm") },
		"bad size":   func(m map[string]any) { m["size"] = 4 },
		"bad name":   func(m map[string]any) { m["name"] = "Gamma!" },
		"no secret":  func(m map[string]any) { m["rootPasswordSecret"] = "" },
		"unknown fl": func(m map[string]any) { m["password"] = "x" },
	} {
		b := map[string]any{}
		for k, v := range body {
			b[k] = v
		}
		mut(b)
		if r := admin.do("POST", "/api/v1/clusters", b); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
		}
	}
}

func TestCommandErrorCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: secret detail", ErrNoCapacity), http.StatusConflict, "no_capacity"},
		{fmt.Errorf("%w: secret detail", ErrPolicyConflict), http.StatusConflict, "policy_conflict"},
		{fmt.Errorf("%w: secret detail", ErrSettingsIncomplete), http.StatusConflict, "settings_incomplete"},
		{fmt.Errorf("%w: secret detail", ErrClusterExists), http.StatusConflict, "cluster_exists"},
		{fmt.Errorf("%w: secret detail", ErrNotLeader), http.StatusServiceUnavailable, "not_leader"},
		{fmt.Errorf("%w: secret detail", ErrBusy), http.StatusConflict, "busy"},
		{fmt.Errorf("%w: secret detail", ErrInvalidInput), http.StatusBadRequest, errBadRequest},
		{fmt.Errorf("%w: secret detail", ErrForbidden), http.StatusForbidden, errForbidden},
		{fmt.Errorf("%w: secret detail", ErrNotFound), http.StatusNotFound, errNotFound},
		{errors.New("secret detail"), http.StatusServiceUnavailable, errUnavailable},
	}
	for _, tc := range cases {
		cf := newControlFixture(t)
		cf.ctl.err = tc.err
		admin, _, _ := cf.setupAdmin("admin")
		body := map[string]any{"swarm": "s1", "name": "gamma", "size": 3, "image": "i", "network": "n", "rootPasswordSecret": "s"}
		wantError(t, admin.do("POST", "/api/v1/clusters", body), tc.status, tc.code)
	}
}

func TestReplaceNode(t *testing.T) {
	cf := newControlFixture(t)
	op := cf.loggedIn("op", authz.RoleOperator, authz.ClusterScope("s1", "c1"))
	wantError(t, op.do("POST", "/api/v1/clusters/c1/nodes/n9/replace", map[string]any{}), http.StatusNotFound, errNotFound)
	wantError(t, op.do("POST", "/api/v1/clusters/c2/nodes/n1/replace", map[string]any{}), http.StatusNotFound, errNotFound)
	var ref OperationRef
	r := op.do("POST", "/api/v1/clusters/c1/nodes/n2/replace", map[string]any{})
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &ref)
	if ref.ID != "op-2" {
		t.Fatalf("%+v", ref)
	}
	// The quorum refusal is passed on as an explanation code, never overridden.
	cf.ctl.err = fmt.Errorf("%w: only 1 of 3 nodes in Primary", ErrRequiresGuidedRecovery)
	wantError(t, op.do("POST", "/api/v1/clusters/c1/nodes/n2/replace", nil), http.StatusConflict, "requires_guided_recovery")
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.Global)
	wantError(t, viewer.do("POST", "/api/v1/clusters/c1/nodes/n2/replace", nil), http.StatusForbidden, errForbidden)
}

func TestOutageDiagnosisReadOnly(t *testing.T) {
	cf := newControlFixture(t)
	cf.ctl.diag = Diagnosis{Candidate: "n2", Positions: []galera.NodePosition{{Slot: "n2", Seqno: 42, Source: "grastate"}}, Reasons: []string{"highest seqno"}}
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	before := len(cf.sink.Records())
	var d Diagnosis
	r := viewer.do("GET", "/api/v1/clusters/c1/outage-diagnosis", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &d)
	if d.Candidate != "n2" || !d.RequiresApproval || len(d.Positions) != 1 {
		t.Fatalf("%+v", d)
	}
	if len(cf.sink.Records()) != before {
		t.Fatal("a read-only diagnosis must not write audit records")
	}
	wantError(t, viewer.do("GET", "/api/v1/clusters/c2/outage-diagnosis", nil), http.StatusNotFound, errNotFound)
	cf.ctl.diag = Diagnosis{}
	r = viewer.do("GET", "/api/v1/clusters/c1/outage-diagnosis", nil)
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.Body.String(), `"positions":[]`) || !strings.Contains(r.Body.String(), `"reasons":[]`) {
		t.Fatalf("%s", r.Body.String())
	}
}

func TestOutageDiagnosisRecoverNeedsOperate(t *testing.T) {
	cf := newControlFixture(t)
	cf.ctl.diag = Diagnosis{Candidate: "n2", Positions: []galera.NodePosition{{Slot: "n2", Seqno: 42, Source: "wsrep-recover"}}}
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	wantError(t, viewer.do("POST", "/api/v1/clusters/c1/outage-diagnosis", nil), http.StatusForbidden, errForbidden)
	op := cf.loggedIn("op", authz.RoleOperator, authz.ClusterScope("s1", "c1"))
	var d Diagnosis
	r := op.do("POST", "/api/v1/clusters/c1/outage-diagnosis", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &d)
	if d.Candidate != "n2" || !d.RequiresApproval || d.Reasons == nil {
		t.Fatalf("%+v", d)
	}
}

func TestOutageConflictReasons(t *testing.T) {
	cf := newControlFixture(t)
	op := cf.loggedIn("op", authz.RoleOperator, authz.ClusterScope("s1", "c1"))
	for _, tc := range []struct {
		method, path string
		body         any
		err          error
		reason       string
	}{
		{"GET", "/api/v1/clusters/c1/outage-diagnosis", nil, ErrZeroUUIDPrimary, "zero_cluster_uuid_primary"},
		{"POST", "/api/v1/clusters/c1/outage-diagnosis", nil, ErrPrimaryComponent, "primary_component_exists"},
		{"POST", "/api/v1/clusters/c1/outage-recovery", map[string]string{"candidate": "n2"}, ErrNoRecoveryCandidate, "no_candidate"},
	} {
		cf.ctl.err = tc.err
		r := op.do(tc.method, tc.path, tc.body)
		wantStatus(t, r, http.StatusConflict)
		var body ErrorBody
		r.decode(t, &body)
		if body.Error != "conflict" || body.Reason != tc.reason {
			t.Fatalf("%s %s: %+v", tc.method, tc.path, body)
		}
	}
}

func TestOutageRecoveryNeedsOperateAndAKnownCandidate(t *testing.T) {
	cf := newControlFixture(t)
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	wantError(t, viewer.do("POST", "/api/v1/clusters/c1/outage-recovery", map[string]string{"candidate": "n1"}), http.StatusForbidden, errForbidden)
	op := cf.loggedIn("op", authz.RoleOperator, authz.ClusterScope("s1", "c1"))
	wantError(t, op.do("POST", "/api/v1/clusters/c1/outage-recovery", map[string]string{"candidate": "n9"}), http.StatusBadRequest, errBadRequest)
	wantError(t, op.do("POST", "/api/v1/clusters/c1/outage-recovery", map[string]string{}), http.StatusBadRequest, errBadRequest)
	wantError(t, op.do("POST", "/api/v1/clusters/c2/outage-recovery", map[string]string{"candidate": "n1"}), http.StatusNotFound, errNotFound)
	var out RecoveryStarted
	r := op.do("POST", "/api/v1/clusters/c1/outage-recovery", map[string]string{"candidate": "n2"})
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &out)
	if out.ID != "op-rec" || out.Approval.Digest != digestA || !out.Diagnosis.RequiresApproval || out.Diagnosis.Positions == nil {
		t.Fatalf("%+v", out)
	}
	// A refusal (no candidate, candidate changed) is a conflict, never an override.
	cf.ctl.err = fmt.Errorf("%w: no candidate", ErrConflict)
	r = op.do("POST", "/api/v1/clusters/c1/outage-recovery", map[string]string{"candidate": "n2"})
	wantStatus(t, r, http.StatusConflict)
	var body ErrorBody
	r.decode(t, &body)
	if body.Error != "conflict" || body.Reason != "conflict" {
		t.Fatalf("unclassified conflict: %+v", body)
	}
}

func TestOperationControlAndApproval(t *testing.T) {
	cf := newControlFixture(t)
	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	wantStatus(t, op.do("POST", "/api/v1/operations/op-c1/resume", nil), http.StatusAccepted)
	wantStatus(t, op.do("POST", "/api/v1/operations/op-c1/abort", nil), http.StatusAccepted)
	wantError(t, op.do("POST", "/api/v1/operations/nope/abort", nil), http.StatusNotFound, errNotFound)
	// Approval needs the explicit restore permission, which operators lack.
	wantError(t, op.do("POST", "/api/v1/operations/op-r1/approve", map[string]string{"digest": digestA}), http.StatusForbidden, errForbidden)
	if auditsOf(cf.fixture, authz.ActionRestore, authz.OutcomeDenied, authz.ResourceBackup) != 1 {
		t.Fatal("denied approval must be audited")
	}
	admin, _, _ := cf.setupAdmin("admin")
	wantError(t, admin.do("POST", "/api/v1/operations/op-r1/approve", map[string]string{"digest": "short"}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/operations/op-r1/approve", map[string]string{}), http.StatusBadRequest, errBadRequest)
	// Operations that do not ask for an approval cannot be approved.
	wantError(t, admin.do("POST", "/api/v1/operations/op-c1/approve", map[string]string{"digest": digestA}), http.StatusConflict, errConflict)
	var a OperationApproval
	allowed := auditsOf(cf.fixture, authz.ActionRestore, authz.OutcomeAllowed, authz.ResourceBackup)
	r := admin.do("POST", "/api/v1/operations/op-r1/approve", map[string]string{"digest": digestA})
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &a)
	if a.ApprovedBy != "admin" || a.Digest != digestA {
		t.Fatalf("%+v", a)
	}
	if auditsOf(cf.fixture, authz.ActionRestore, authz.OutcomeAllowed, authz.ResourceBackup) != allowed+1 {
		t.Fatal("approval must be audited")
	}
	cf.ctl.err = fmt.Errorf("%w: digest of another definition", ErrApprovalMismatch)
	wantError(t, admin.do("POST", "/api/v1/operations/op-r1/approve", map[string]string{"digest": digestA}), http.StatusConflict, "approval_mismatch")
}

// TestClusterDeleteNeedsDeletePermission: deleting a cluster (also a
// failed create, lab E4) is denied by default: operators and database
// admins lack the delete permission; a restore permission on the backups
// does not cover approving a deletion either. Every decision is audited.
func TestClusterDeleteNeedsDeletePermission(t *testing.T) {
	cf := newControlFixture(t)
	if err := cf.policy.AddRole(authz.Role{Name: "restorer", Permissions: []authz.Permission{
		{Type: authz.ResourceCluster, Action: authz.ActionRead}, {Type: authz.ResourceBackup, Action: authz.ActionRestore}}}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{authz.RoleOperator, authz.RoleDatabaseAdmin, "restorer"} {
		u := cf.loggedIn("u-"+role, role, authz.SwarmScope("s1"))
		wantError(t, u.do("DELETE", "/api/v1/clusters/c1", nil), http.StatusForbidden, errForbidden)
		wantError(t, u.do("POST", "/api/v1/operations/op-d1/approve", map[string]string{"digest": digestA}), http.StatusForbidden, errForbidden)
	}
	// Not readable at all: 404 without revealing the cluster.
	other := cf.loggedIn("v2", authz.RoleViewer, authz.SwarmScope("s2"))
	wantError(t, other.do("DELETE", "/api/v1/clusters/c1", nil), http.StatusNotFound, errNotFound)
	if n := auditsOf(cf.fixture, authz.ActionDelete, authz.OutcomeDenied, authz.ResourceCluster); n != 7 {
		t.Fatalf("denied deletions/approvals audited %d times, want 7", n)
	}
	// The restorer may still approve a restore.
	r := cf.loggedIn("r2", "restorer", authz.SwarmScope("s1"))
	wantStatus(t, r.do("POST", "/api/v1/operations/op-r1/approve", map[string]string{"digest": digestA}), http.StatusOK)
	if n := len(cf.ctl.Calls()); n != 1 {
		t.Fatalf("calls %v", cf.ctl.Calls())
	}

	admin, _, _ := cf.setupAdmin("admin")
	var d ClusterDeletion
	resp := admin.do("DELETE", "/api/v1/clusters/c1", nil)
	wantStatus(t, resp, http.StatusAccepted)
	resp.decode(t, &d)
	if d.ID != "op-d" || d.Kind != OperationKindClusterDelete || d.Approval.Digest != digestA {
		t.Fatalf("%+v", d)
	}
	wantStatus(t, admin.do("POST", "/api/v1/operations/op-d1/approve", map[string]string{"digest": digestA}), http.StatusOK)
	if n := auditsOf(cf.fixture, authz.ActionDelete, authz.OutcomeAllowed, authz.ResourceCluster); n != 2 {
		t.Fatalf("allowed deletion and approval audited %d times", n)
	}
	wantError(t, admin.do("DELETE", "/api/v1/clusters/nope", nil), http.StatusNotFound, errNotFound)
	cf.ctl.err = fmt.Errorf("%w: operation running", ErrBusy)
	wantError(t, admin.do("DELETE", "/api/v1/clusters/c1", nil), http.StatusConflict, "busy")
}

// TestOutageRecoveryApprovalNeedsRecoverPermission: lab E6 approved the
// guided recovery with the restore permission. Approving it now needs the
// separate recover permission on the cluster; operators may request the
// plan (operate) but not approve it, and a restore role does not cover it.
func TestOutageRecoveryApprovalNeedsRecoverPermission(t *testing.T) {
	cf := newControlFixture(t)
	cf.inventory.operations = append(cf.inventory.operations, Operation{ID: "op-o1", Kind: OperationKindOutageRecovery,
		Swarm: "s1", Cluster: "c1", State: OperationPending, Approval: &OperationApproval{Digest: digestA}})
	if err := cf.policy.AddRole(authz.Role{Name: "restorer", Permissions: []authz.Permission{
		{Type: authz.ResourceCluster, Action: authz.ActionRead}, {Type: authz.ResourceBackup, Action: authz.ActionRestore}}}); err != nil {
		t.Fatal(err)
	}
	if err := cf.policy.AddRole(authz.Role{Name: "recoverer", Permissions: []authz.Permission{
		{Type: authz.ResourceCluster, Action: authz.ActionRead}, {Type: authz.ResourceBackup, Action: authz.ActionRead},
		{Type: authz.ResourceCluster, Action: authz.ActionRecover}}}); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{authz.RoleOperator, authz.RoleDatabaseAdmin, "restorer"} {
		u := cf.loggedIn("u-"+role, role, authz.SwarmScope("s1"))
		wantError(t, u.do("POST", "/api/v1/operations/op-o1/approve", map[string]string{"digest": digestA}), http.StatusForbidden, errForbidden)
	}
	if n := auditsOf(cf.fixture, authz.ActionRecover, authz.OutcomeDenied, authz.ResourceCluster); n != 3 {
		t.Fatalf("denied approvals audited %d times, want 3", n)
	}
	rc := cf.loggedIn("rc", "recoverer", authz.SwarmScope("s1"))
	wantStatus(t, rc.do("POST", "/api/v1/operations/op-o1/approve", map[string]string{"digest": digestA}), http.StatusOK)
	// The recover permission does not approve a restore.
	wantError(t, rc.do("POST", "/api/v1/operations/op-r1/approve", map[string]string{"digest": digestA}), http.StatusForbidden, errForbidden)
	if n := auditsOf(cf.fixture, authz.ActionRecover, authz.OutcomeAllowed, authz.ResourceCluster); n != 1 {
		t.Fatalf("allowed approval audited %d times", n)
	}
}

func TestAuditOutageFailsCommandsClosed(t *testing.T) {
	cf := newControlFixture(t)
	admin, _, _ := cf.setupAdmin("admin")
	cf.sink.SetErr(errors.New("audit down"))
	for _, rt := range commandRoutes {
		if rt.method == "GET" {
			continue
		}
		wantError(t, admin.do(rt.method, rt.path, rt.body), http.StatusServiceUnavailable, errUnavailable)
	}
	if n := len(cf.ctl.Calls()) + len(cf.bak.Calls()) + len(cf.set.Calls()); n != 0 {
		t.Fatalf("services called without audit: %d", n)
	}
}

func TestBackupEndpoints(t *testing.T) {
	cf := newControlFixture(t)
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	var list List[BackupSummary]
	r := viewer.do("GET", "/api/v1/backups", nil)
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].Cluster != "c1" || strings.Contains(r.Body.String(), "lastBackupAt") {
		t.Fatalf("%s", r.Body.String())
	}
	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	wantStatus(t, op.do("POST", "/api/v1/clusters/c1/backups", nil), http.StatusAccepted)
	wantError(t, op.do("PUT", "/api/v1/clusters/c1/backup-policy", map[string]any{"enabled": true, "schedule": "0 2 * * *"}), http.StatusForbidden, errForbidden)
	wantError(t, op.do("POST", "/api/v1/restores/inspect", map[string]string{"cluster": "c1"}), http.StatusForbidden, errForbidden)
	wantError(t, op.do("POST", "/api/v1/restores/inspect", map[string]string{"cluster": "c2"}), http.StatusNotFound, errNotFound)
	wantError(t, op.do("POST", "/api/v1/restores/inspect", map[string]string{}), http.StatusBadRequest, errBadRequest)

	admin, _, _ := cf.setupAdmin("admin")
	r = admin.do("PUT", "/api/v1/clusters/c1/backup-policy", map[string]any{"enabled": true, "schedule": "0 2 * * *", "missed": "run-once"})
	wantStatus(t, r, http.StatusOK)
	if cf.bak.lastPol.Missed != "run-once" {
		t.Fatalf("%+v", cf.bak.lastPol)
	}
	r = admin.do("POST", "/api/v1/restores/inspect", map[string]any{"cluster": "c1", "backup": "b1", "ttlMinutes": 60})
	wantStatus(t, r, http.StatusAccepted)
	if cf.bak.lastInsp.Backup != "b1" || cf.bak.lastInsp.TTLMinutes != 60 {
		t.Fatalf("%+v", cf.bak.lastInsp)
	}
	wantError(t, admin.do("POST", "/api/v1/restores/inspect", map[string]any{"cluster": "c1", "ttlMinutes": 100000}), http.StatusBadRequest, errBadRequest)
}

func TestPointInTimeEndpoints(t *testing.T) {
	cf := newControlFixture(t)
	op := cf.loggedIn("op", authz.RoleOperator, authz.SwarmScope("s1"))
	// The operator may archive (backup permission) and look (read), but not restore.
	wantStatus(t, op.do("POST", "/api/v1/clusters/c1/binlog-archives", nil), http.StatusAccepted)
	wantStatus(t, op.do("GET", "/api/v1/clusters/c1/pitr-proposal?at=2026-10-22T01:30:00Z", nil), http.StatusOK)
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.SwarmScope("s1"))
	wantError(t, viewer.do("POST", "/api/v1/clusters/c1/binlog-archives", nil), http.StatusForbidden, errForbidden)
	wantStatus(t, viewer.do("GET", "/api/v1/clusters/c1/pitr-proposal?at=2026-10-22T01:30:00Z", nil), http.StatusOK)
	wantError(t, op.do("POST", "/api/v1/restores/inspect", map[string]any{"cluster": "c1", "pointInTime": "2026-10-22T01:30:00Z"}), http.StatusForbidden, errForbidden)

	admin, _, _ := cf.setupAdmin("admin")
	r := admin.do("GET", "/api/v1/clusters/c1/pitr-proposal?at=2026-10-22T03:30:00%2B02:00", nil)
	wantStatus(t, r, http.StatusOK)
	var p PointInTimeProposal
	r.decode(t, &p)
	if !p.Covered || p.Backup != "b1" || !p.ApprovalRequired || p.InPlace || !strings.Contains(strings.Join(cf.bak.Calls(), ","), "admin:pitr c1 2026-10-22T01:30:00Z") {
		t.Fatalf("%+v %v", p, cf.bak.Calls())
	}
	for _, q := range []string{"", "?at=yesterday", "?at=2026-10-22"} {
		wantError(t, admin.do("GET", "/api/v1/clusters/c1/pitr-proposal"+q, nil), http.StatusBadRequest, errBadRequest)
	}

	r = admin.do("POST", "/api/v1/restores/inspect", map[string]any{"cluster": "c1", "pointInTime": "2026-10-22T01:30:00Z"})
	wantStatus(t, r, http.StatusAccepted)
	if cf.bak.lastInsp.PointInTime == nil || !cf.bak.lastInsp.PointInTime.Equal(time.Date(2026, 10, 22, 1, 30, 0, 0, time.UTC)) {
		t.Fatalf("%+v", cf.bak.lastInsp)
	}
	// A time and a named backup contradict each other.
	wantError(t, admin.do("POST", "/api/v1/restores/inspect", map[string]any{"cluster": "c1", "backup": "b1", "pointInTime": "2026-10-22T01:30:00Z"}), http.StatusBadRequest, errBadRequest)
	// A reason from the planner reaches the caller as a conflict.
	cf.bak.err = fmt.Errorf("%w: the archive ends at 02:00", ErrConflict)
	wantError(t, admin.do("POST", "/api/v1/restores/inspect", map[string]any{"cluster": "c1", "pointInTime": "2026-10-22T05:30:00Z"}), http.StatusConflict, errConflict)
}

func TestNotificationSettingsSecretWriteOnly(t *testing.T) {
	cf := newControlFixture(t)
	viewer := cf.loggedIn("v", authz.RoleViewer, authz.Global)
	wantError(t, viewer.do("GET", "/api/v1/settings/notifications", nil), http.StatusForbidden, errForbidden)
	admin, _, _ := cf.setupAdmin("admin")
	r := admin.do("GET", "/api/v1/settings/notifications", nil)
	wantStatus(t, r, http.StatusOK)
	var ns NotificationSettings
	r.decode(t, &ns)
	if ns.Email.Recipients == nil {
		t.Fatal("recipients must be an array")
	}
	r = admin.do("PUT", "/api/v1/settings/notifications", map[string]any{
		"webhook": map[string]any{"enabled": true, "url": "https://hooks.example/x", "secret": "s3cr3t-value"},
		"email":   map[string]any{"enabled": false, "recipients": []string{}},
	})
	wantStatus(t, r, http.StatusOK)
	if strings.Contains(r.Body.String(), "s3cr3t-value") || !strings.Contains(r.Body.String(), `"secretSet":true`) {
		t.Fatalf("%s", r.Body.String())
	}
	if cf.set.last.Webhook.Secret == nil || *cf.set.last.Webhook.Secret != "s3cr3t-value" {
		t.Fatal("secret not passed to the service")
	}
	for _, rec := range cf.sink.Records() {
		if strings.Contains(fmt.Sprintf("%+v", rec), "s3cr3t-value") {
			t.Fatal("secret in audit record")
		}
	}
	wantError(t, admin.do("PUT", "/api/v1/settings/notifications", map[string]any{"minSeverity": "loud",
		"webhook": map[string]any{"enabled": false, "url": ""}, "email": map[string]any{"enabled": false, "recipients": []string{}}}),
		http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/settings/notifications/test", map[string]string{"channel": "sms"}), http.StatusBadRequest, errBadRequest)
	var res NotificationTest
	r = admin.do("POST", "/api/v1/settings/notifications/test", map[string]string{"channel": "email"})
	wantStatus(t, r, http.StatusOK)
	r.decode(t, &res)
	if !res.Delivered || res.Channel != "email" {
		t.Fatalf("%+v", res)
	}
}

func TestNotificationReadAndAcknowledgedAlias(t *testing.T) {
	f := newFixture(t)
	fm := newFakeMonitoring()
	fm.notes = []Notification{{ID: "n1", Severity: "warning", Kind: "alert.firing", Message: "m", Acked: true}}
	f.srv.cfg.Monitoring = fm
	admin, _, _ := f.setupAdmin("admin")
	r := admin.do("GET", "/api/v1/notifications", nil)
	wantStatus(t, r, http.StatusOK)
	if !strings.Contains(r.Body.String(), `"acknowledged":true`) {
		t.Fatalf("%s", r.Body.String())
	}
	wantStatus(t, admin.do("POST", "/api/v1/notifications/n1/read", nil), http.StatusNoContent)
	if !fm.notes[0].Read {
		t.Fatal("not marked read")
	}
	wantError(t, admin.do("POST", "/api/v1/notifications/nope/read", nil), http.StatusNotFound, errNotFound)
	admin.origin = "https://evil.example"
	wantError(t, admin.do("POST", "/api/v1/notifications/n1/read", nil), http.StatusForbidden, errOriginRejected)
}
