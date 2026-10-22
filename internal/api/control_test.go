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

func (f *fakeControl) OutageDiagnosis(_ context.Context, a authz.Subject, c string) (Diagnosis, error) {
	if err := f.record(a, "diagnose %s", c); err != nil {
		return Diagnosis{}, err
	}
	return f.diag, nil
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
	{"POST", "/api/v1/operations/op-c1/resume", nil},
	{"POST", "/api/v1/operations/op-c1/abort", nil},
	{"POST", "/api/v1/operations/op-r1/approve", map[string]string{"digest": digestA}},
	{"GET", "/api/v1/backups", nil},
	{"PUT", "/api/v1/clusters/c1/backup-policy", map[string]any{"enabled": true, "schedule": "0 2 * * *"}},
	{"POST", "/api/v1/clusters/c1/backups", nil},
	{"POST", "/api/v1/restores/inspect", map[string]string{"cluster": "c1"}},
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
	f.inventory.clusters = []Cluster{{ID: "c1", Swarm: "s1", Nodes: []ClusterNode{{ID: "n1"}}}}
	f.inventory.operations = []Operation{{ID: "op-c1", Swarm: "s1", Cluster: "c1"}, {ID: "op-r1", Swarm: "s1", Cluster: "c1", Approval: &OperationApproval{Digest: digestA}}}
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
