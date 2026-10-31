// SPDX-License-Identifier: AGPL-3.0-only

package api

// Fakes only: a fake host agent, the in-memory KV behind a real
// patch.Service and a fake SSH runner. No package manager or host is touched.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

type fakePatchAgent struct {
	mu      sync.Mutex
	pending map[string]bool
	applied int
}

func (f *fakePatchAgent) Do(_ context.Context, _ string, c patch.AgentCommand) (patch.AgentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch c.Kind {
	case patch.CmdReport:
		r := patch.HostReport{Manager: patch.APT, RebootRequired: patch.No, SecurityKnown: true, OS: patch.OSInfo{ID: "debian", VersionID: "12"}, CollectedAt: time.Now()}
		for n := range f.pending {
			r.Updates = append(r.Updates, patch.Update{Name: n, Available: "2", Installed: "1", Security: n == "openssl"})
		}
		return patch.AgentResult{Report: &r}, nil
	case patch.CmdSimulate:
		return patch.AgentResult{Simulation: &patch.Simulation{Source: "native", Changes: []patch.Change{}}}, nil
	case patch.CmdApply:
		f.applied++
		for _, p := range c.Packages {
			delete(f.pending, p)
		}
	}
	return patch.AgentResult{}, nil
}

type fakeSealer struct{}

func (s *fakeSealer) Put(c, k string, v any) error { return nil }
func (s *fakeSealer) Get(c, k string, v any) (bool, error) {
	return false, nil
}
func (s *fakeSealer) Delete(c, k string) error { return nil }

type fakeSSH struct{}

func (fakeSSH) Run(_ context.Context, _ provision.Host, _ provision.Credential, cmd, _ string) (string, error) {
	if strings.Contains(cmd, "/etc/os-release") {
		return "os=debian\narch=aarch64\nsystemd=yes\npriv=root\nmanager=apt-get\nagent=absent\ndisk=5000000000\n", nil
	}
	return "", nil
}

func newPatchFixture(t *testing.T) (*fixture, *patch.Service, *fakePatchAgent) {
	f := newFixture(t)
	ag := &fakePatchAgent{pending: map[string]bool{"openssl": true, "tzdata": true}}
	svc := &patch.Service{KV: &vuln.MemoryKV{}, Journal: opjournal.New(), Agents: ag, Guard: patch.AllowAllGuard{},
		Term: func() (uint64, error) { return 3, nil }}
	inst := &patch.Installer{Runner: fakeSSH{}, Store: &fakeSealer{}, Token: func(context.Context, string) (string, error) { return "tok", nil }}
	f.srv.cfg.Patch = &PatchBackend{Svc: svc, Installer: inst}
	return f, svc, ag
}

func TestPatchUnavailableWithoutBackend(t *testing.T) {
	f := newFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	wantError(t, admin.do("GET", "/api/v1/patch/hosts", nil), http.StatusServiceUnavailable, errUnavailable)
}

func TestPatchRoleMatrix(t *testing.T) {
	f, svc, _ := newPatchFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	viewer := f.loggedIn("view", authz.RoleViewer, authz.Global)
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)
	dba := f.loggedIn("dba", authz.RoleDatabaseAdmin, authz.Global)
	svc.UpsertHost(patch.Host{ID: "h1"})

	type route struct{ m, p string }
	reads := []route{{"GET", "/patch/hosts"}, {"GET", "/patch/hosts/h1"}, {"GET", "/patch/hosts/h1/history"}, {"GET", "/patch/packages"}, {"GET", "/patch/groups"}, {"GET", "/patch/policy"}, {"GET", "/patch/plans"}}
	operates := []route{{"POST", "/patch/hosts/h1/poll"}, {"POST", "/patch/hosts/h1/plans"}}
	admins := []route{{"PUT", "/patch/hosts/h1"}, {"PUT", "/patch/groups/g"}, {"DELETE", "/patch/groups/g"}, {"PUT", "/patch/policy"},
		{"POST", "/patch/plans/x/approve"}, {"POST", "/patch/plans/x/decline"}, {"POST", "/patch/plans/x/apply"},
		{"POST", "/patch/install/plans"}, {"POST", "/patch/install/plans/x/execute"}}

	forbidden := func(c *client, rs []route) {
		t.Helper()
		for _, r := range rs {
			wantError(t, c.do(r.m, "/api/v1"+r.p, map[string]any{}), http.StatusForbidden, errForbidden)
		}
	}
	allowed := func(c *client, rs []route) {
		t.Helper()
		for _, r := range rs {
			resp := c.do(r.m, "/api/v1"+r.p, map[string]any{})
			if resp.Code == http.StatusForbidden || resp.Code == http.StatusUnauthorized {
				t.Fatalf("%s %s denied for a role that should pass: %d", r.m, r.p, resp.Code)
			}
		}
	}
	forbidden(dba, append(append(append([]route{}, reads...), operates...), admins...))
	allowed(viewer, reads)
	forbidden(viewer, append(append([]route{}, operates...), admins...))
	allowed(op, append(append([]route{}, reads...), operates...))
	forbidden(op, admins)
	allowed(admin, reads)
}

func TestPatchEndToEnd(t *testing.T) {
	f, _, ag := newPatchFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)

	wantStatus(t, admin.do("PUT", "/api/v1/patch/groups/web", map[string]any{"name": "Web", "policy": map[string]any{"mode": "approval_required"}}), http.StatusOK)
	wantStatus(t, admin.do("PUT", "/api/v1/patch/hosts/h1", map[string]any{"name": "web-1", "group": "web"}), http.StatusOK)
	wantError(t, admin.do("PUT", "/api/v1/patch/hosts/h2", map[string]any{"group": "missing"}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("PUT", "/api/v1/patch/hosts/h1", map[string]any{"policy": map[string]any{"mode": "auto"}}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("PUT", "/api/v1/patch/groups/web", map[string]any{"name": "Web", "extra": 1}), http.StatusBadRequest, errBadRequest)

	var view PatchHostView
	wantStatus(t, op.do("POST", "/api/v1/patch/hosts/h1/poll", nil), http.StatusOK)
	op.do("GET", "/api/v1/patch/hosts/h1", nil).decode(t, &view)
	if view.Summary.UpdatesPending != 2 || view.Summary.SecurityCount != 1 || view.Summary.Policy.Mode != patch.ModeApprovalRequired {
		t.Fatalf("%+v", view.Summary)
	}
	wantError(t, op.do("GET", "/api/v1/patch/hosts/nope", nil), http.StatusNotFound, errNotFound)
	wantError(t, op.do("POST", "/api/v1/patch/hosts/nope/poll", nil), http.StatusNotFound, errNotFound)

	var plan patch.Plan
	r := op.do("POST", "/api/v1/patch/hosts/h1/plans", map[string]any{"securityOnly": true})
	wantStatus(t, r, http.StatusCreated)
	r.decode(t, &plan)
	if len(plan.Packages) != 1 || plan.Packages[0] != "openssl" || plan.State != patch.PlanProposed {
		t.Fatalf("%+v", plan)
	}
	// An operator proposes; approving needs update (administrator).
	wantError(t, op.do("POST", "/api/v1/patch/plans/"+plan.ID+"/approve", map[string]any{"digest": plan.Digest}), http.StatusForbidden, errForbidden)
	wantError(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/approve", map[string]any{"digest": strings.Repeat("0", 64)}), http.StatusConflict, errApprovalMismatch)
	wantError(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/apply", nil), http.StatusConflict, errConflict)
	wantStatus(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/approve", map[string]any{"digest": plan.Digest}), http.StatusOK)
	if ag.applied != 0 {
		t.Fatal("approval applied")
	}
	r = admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/apply", nil)
	wantStatus(t, r, http.StatusAccepted)
	r.decode(t, &plan)
	if plan.State != patch.PlanSucceeded || ag.applied != 1 {
		t.Fatalf("%+v", plan)
	}
	var hist []patch.HistoryEntry
	op.do("GET", "/api/v1/patch/hosts/h1/history", nil).decode(t, &hist)
	if len(hist) < 2 || hist[0].Kind != "apply" {
		t.Fatalf("%+v", hist)
	}
	var pk []patch.PackageView
	op.do("GET", "/api/v1/patch/packages", nil).decode(t, &pk)
	if len(pk) != 1 || pk[0].Name != "tzdata" {
		t.Fatalf("%+v", pk)
	}
	wantError(t, admin.do("DELETE", "/api/v1/patch/groups/web", nil), http.StatusConflict, errConflict)
}

func TestPatchReportOnlyAndGuard(t *testing.T) {
	f, svc, ag := newPatchFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	svc.UpsertHost(patch.Host{ID: "h1"})
	svc.Poll(context.Background(), "h1", false)
	var plan patch.Plan
	admin.do("POST", "/api/v1/patch/hosts/h1/plans", nil).decode(t, &plan)
	wantError(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/approve", map[string]any{"digest": plan.Digest}), http.StatusConflict, errPolicyConflict)

	// Approval policy, but a Galera guard that refuses.
	wantStatus(t, admin.do("PUT", "/api/v1/patch/policy", map[string]any{"mode": "approval_required"}), http.StatusOK)
	svc.Guard = patch.GaleraGuard{Directory: galeraDir{}}
	admin.do("POST", "/api/v1/patch/hosts/h1/plans", nil).decode(t, &plan)
	wantStatus(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/approve", map[string]any{"digest": plan.Digest}), http.StatusOK)
	wantError(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/apply", nil), http.StatusConflict, errConflict)
	if ag.applied != 0 {
		t.Fatal("blocked Galera member was patched")
	}
	wantError(t, admin.do("PUT", "/api/v1/patch/policy", map[string]any{"mode": "approval_required", "reboot": "auto_in_window"}), http.StatusBadRequest, errBadRequest)
}

type galeraDir struct{}

func (galeraDir) Member(context.Context, string) (string, bool, error) { return "galera-1", true, nil }

func TestPatchInstallRoutesAndSecrets(t *testing.T) {
	f, _, _ := newPatchFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	req := map[string]any{
		"id": "inst1", "host": map[string]any{"id": "h1", "address": "h1.example.test", "user": "admin", "role": "worker"}, "mode": "report",
		"binaryUrl": "https://releases.example.test/agent", "binarySha256": strings.Repeat("ab", 32), "version": "0.9.0",
		"managerUrl": "https://manager.example.test:7444", "caFingerprint": strings.Repeat("AB", 32), "installation": "inst-1"}
	cred := map[string]any{"privateKey": "synthetic-private-key", "knownHosts": "h1.example.test ssh-ed25519 synthetic"}
	r := admin.do("POST", "/api/v1/patch/install/plans", map[string]any{"request": req, "credential": cred})
	wantStatus(t, r, http.StatusCreated)
	if strings.Contains(r.Body.String(), "synthetic-private-key") || strings.Contains(r.Body.String(), "knownHosts") {
		t.Fatal("credential leaked")
	}
	var plan patch.InstallPlan
	r.decode(t, &plan)
	if !strings.Contains(plan.Script, "binary checksum mismatch") {
		t.Fatal("script missing checksum check")
	}
	req["binaryUrl"] = "http://insecure.example.test/agent"
	wantError(t, admin.do("POST", "/api/v1/patch/install/plans", map[string]any{"request": req, "credential": cred}), http.StatusBadRequest, errBadRequest)
	wantError(t, admin.do("POST", "/api/v1/patch/install/plans/inst1/execute", map[string]any{"digest": "short"}), http.StatusBadRequest, errBadRequest)
}

func TestPatchErrorMapping(t *testing.T) {
	for err, want := range map[error]error{
		patch.ErrNotFound: ErrNotFound, patch.ErrInvalid: ErrInvalidInput, patch.ErrReportOnly: ErrPolicyConflict, patch.ErrOutsideWindow: ErrConflict,
		patch.ErrGaleraBlocked: ErrConflict, patch.ErrEngineGuard: ErrConflict, patch.ErrRuntimeReportOnly: ErrPolicyConflict, patch.ErrDigest: ErrApprovalMismatch, patch.ErrInstallDigest: ErrApprovalMismatch,
	} {
		if got := mapPatchError(err); !errors.Is(got, want) {
			t.Fatalf("%v -> %v, want %v", err, got, want)
		}
	}
}

type apiProbe struct{ st patch.EngineState }

func (p *apiProbe) Probe(context.Context, string) (patch.EngineState, error) { return p.st, nil }

func TestPatchEnginePlanAuthzGuardAndPolicy(t *testing.T) {
	f, svc, ag := newPatchFixture(t)
	admin, _, _ := f.setupAdmin("admin")
	op := f.loggedIn("op", authz.RoleOperator, authz.Global)
	viewer := f.loggedIn("view", authz.RoleViewer, authz.Global)
	ag.pending["docker-ce"] = true
	probe := &apiProbe{st: patch.EngineState{Version: "29.1.0", DockerInfoOK: true, SwarmRole: "worker", NodeState: "ready", Availability: "active", TasksKnown: true,
		RunningTasks: []string{"web.1.abc"}, TasksConverged: patch.Yes}}
	svc.Engine = probe
	svc.Sleep = func(time.Duration) {}

	wantStatus(t, admin.do("PUT", "/api/v1/patch/hosts/h1", map[string]any{"policy": map[string]any{"mode": "approval_required"}}), http.StatusOK)
	wantStatus(t, op.do("POST", "/api/v1/patch/hosts/h1/poll", nil), http.StatusOK)
	var view PatchHostView
	viewer.do("GET", "/api/v1/patch/hosts/h1", nil).decode(t, &view)
	classes := map[string]patch.Class{}
	for _, u := range view.Updates {
		classes[u.Name] = u.Class
	}
	if classes["docker-ce"] != patch.ClassContainerRuntime || classes["openssl"] != patch.ClassOS {
		t.Fatalf("%v", classes)
	}

	var plan patch.Plan
	r := op.do("POST", "/api/v1/patch/hosts/h1/plans", map[string]any{"packages": []string{"docker-ce"}})
	wantStatus(t, r, http.StatusCreated)
	r.decode(t, &plan)
	if !plan.ContainerRuntime || plan.EngineGuard == nil || plan.EngineGuard.Allowed || plan.EngineGuard.RestartNote == "" {
		t.Fatalf("%+v", plan)
	}
	wantError(t, viewer.do("POST", "/api/v1/patch/hosts/h1/plans", map[string]any{"packages": []string{"docker-ce"}}), http.StatusForbidden, errForbidden)
	wantError(t, op.do("POST", "/api/v1/patch/plans/"+plan.ID+"/approve", map[string]any{"digest": plan.Digest}), http.StatusForbidden, errForbidden)
	// The default policy of the class is report-only.
	wantError(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/approve", map[string]any{"digest": plan.Digest}), http.StatusConflict, errPolicyConflict)

	wantStatus(t, admin.do("PUT", "/api/v1/patch/hosts/h1", map[string]any{"policy": map[string]any{"mode": "approval_required", "containerRuntime": "approval_required"}}), http.StatusOK)
	wantStatus(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/approve", map[string]any{"digest": plan.Digest}), http.StatusOK)
	// The running task blocks the apply.
	wantError(t, admin.do("POST", "/api/v1/patch/plans/"+plan.ID+"/apply", nil), http.StatusConflict, errConflict)
	if ag.applied != 0 {
		t.Fatal("guard did not stop the apply")
	}
}
