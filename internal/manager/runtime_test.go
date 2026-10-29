// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// setReports replaces the stored agent reports of a host.
func setReports(t *testing.T, s *Service, swarm, node string, infos ...agentrt.Info) {
	t.Helper()
	_, _, err := Update(s.Store(), CollHosts, HostKey(swarm, node), func(cur HostRecord, _ bool) (HostRecord, error) {
		cur.AgentReports = map[agentrt.Runtime]agentrt.Info{}
		for _, i := range infos {
			cur.AgentReports[i.Runtime] = i
		}
		return cur, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func resticSource(id string) api.HostSourceInput {
	return api.HostSourceInput{HostSource: api.HostSource{ID: id, Name: id, Kind: "managed-host", Transport: "agent",
		Swarm: "sw1", HostID: "h1", Paths: []string{"/srv/data"}, Backend: restic.BackendLocal,
		Repository: "/backup/repo", PasswordSecret: "restic-password"}, Password: "test-only"}
}

func TestRouteUsesTableAndNeverFallsBack(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	d := &AgentDirectory{Store: f.svc.Store(), PKI: f.pki}
	route := func(kind string) (agentrt.Runtime, error) { return d.Route("n1", kind) }

	// No report yet: the legacy container agent serves only its old kinds.
	if rt, err := route("create-service"); err != nil || rt != agentrt.Container {
		t.Fatalf("legacy docker step: %v %v", rt, err)
	}
	if _, err := route("restic-backup"); !errors.Is(err, agent.ErrInvalidCommand) {
		t.Fatalf("restic without any report must be refused: %v", err)
	}
	if _, err := route("patch/apply"); err == nil || !strings.Contains(err.Error(), "native") {
		t.Fatalf("patch without native agent: %v", err)
	}
	if _, err := route("format-disk"); err == nil || !strings.Contains(err.Error(), "no runtime mapping") {
		t.Fatalf("unmapped kind: %v", err)
	}

	setReports(t, f.svc, "sw1", "n1", agentrt.ContainerInfo(true))
	if rt, err := route("restic-backup"); err != nil || rt != agentrt.Container {
		t.Fatalf("restic container only: %v %v", rt, err)
	}
	setReports(t, f.svc, "sw1", "n1", agentrt.ContainerInfo(true), agentrt.NativeInfo(true))
	if rt, err := route("restic-backup"); err != nil || rt != agentrt.Native {
		t.Fatalf("restic prefers native: %v %v", rt, err)
	}
	if rt, err := route("patch/report"); err != nil || rt != agentrt.Native {
		t.Fatalf("patch native: %v %v", rt, err)
	}
	if rt, err := route("exec-sql"); err != nil || rt != agentrt.Container {
		t.Fatalf("galera step stays on the container agent: %v %v", rt, err)
	}
	// A native-only host cannot run Docker object steps.
	setReports(t, f.svc, "sw1", "n1", agentrt.NativeInfo(true))
	if _, err := route("create-service"); err == nil || !strings.Contains(err.Error(), "container") {
		t.Fatalf("docker step on a native-only host: %v", err)
	}
}

func TestNativeReportKeepsLegacyContainerRoute(t *testing.T) {
	h := HostRecord{AgentSerial: "ab", AgentIdentities: map[agentrt.Runtime]AgentIdentityRecord{agentrt.Native: {Serial: "cd"}}, AgentReports: map[agentrt.Runtime]agentrt.Info{agentrt.Native: agentrt.NativeInfo(false)}}
	for kind, want := range map[string]agentrt.Runtime{"create-service": agentrt.Container, "patch/report": agentrt.Native} {
		req, _ := agentrt.RequirementFor(kind)
		if got, err := routeHost(h, req); err != nil || got != want {
			t.Fatalf("%s: %s %v", kind, got, err)
		}
	}
	req, _ := agentrt.RequirementFor("restic-backup")
	if _, err := routeHost(h, req); err == nil {
		t.Fatal("legacy container gained restic")
	}
}

func TestNoteReportRejectsMismatchAndRefusesHost(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	d := &AgentDirectory{Store: f.svc.Store(), PKI: f.pki}
	get := func() HostRecord {
		h, _, _ := Get[HostRecord](f.svc.Store(), CollHosts, HostKey("sw1", "n1"))
		return h.Value
	}
	d.NoteReport("n1", agentrt.Container, agentrt.ContainerInfo(true), true, nil)
	if got := get().AgentReports[agentrt.Container]; !got.Has(agentrt.Restic) {
		t.Fatalf("report not stored: %+v", get().AgentReports)
	}
	// The container endpoint claims a native capability.
	bad := agentrt.Info{Runtime: agentrt.Container, Capabilities: []agentrt.Capability{agentrt.DockerObjects, agentrt.Patch}}
	d.NoteReport("n1", agentrt.Container, bad, true, bad.Validate())
	if get().AgentReportError == "" {
		t.Fatal("mismatch not recorded")
	}
	if got := get().AgentReports[agentrt.Container]; got.Has(agentrt.Patch) {
		t.Fatal("mismatched capabilities were stored")
	}
	if _, err := d.Route("n1", "create-service"); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("host with a mismatched report must be refused: %v", err)
	}
	if h := toAPIHost(get()); h.AgentReportError == "" {
		t.Fatal("error not exposed in the API host")
	}
	// A native report on the container endpoint is a mismatch, too.
	d.NoteReport("n1", agentrt.Container, agentrt.NativeInfo(false), true, nil)
	if !strings.Contains(get().AgentReportError, "answered as native") {
		t.Fatalf("endpoint/runtime mismatch: %q", get().AgentReportError)
	}
	// A correct report heals it.
	d.NoteReport("n1", agentrt.Container, agentrt.ContainerInfo(false), true, nil)
	if get().AgentReportError != "" {
		t.Fatal("valid report did not clear the error")
	}
}

func TestEnrollmentRecordsReportAndRefusesMismatch(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	plain, _, err := f.pki.IssueEnrollmentToken("sw1", 0)
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := f.pki.CAFingerprint()
	cfg := enroll.AgentConfig{ManagerURL: f.url, Installation: "inst-1", HostID: "n1", CAFingerprint: fp, Token: plain,
		Info: agentrt.Info{Runtime: agentrt.Container, Capabilities: []agentrt.Capability{agentrt.DockerObjects, agentrt.HostMaintenance}}}
	if _, err := enroll.Enroll(context.Background(), cfg); err == nil {
		t.Fatal("container agent claiming a native capability enrolled")
	}
	// The refused request did not burn the token.
	cfg.Info = agentrt.NativeInfo(true)
	if _, err := enroll.Enroll(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	h, _, _ := Get[HostRecord](f.svc.Store(), CollHosts, HostKey("sw1", "n1"))
	if got := h.Value.AgentReports[agentrt.Native]; !got.Has(agentrt.Patch) || !got.Has(agentrt.Restic) {
		t.Fatalf("enrollment report: %+v", h.Value.AgentReports)
	}
}

func TestHostSourceRuntimeIsVisibleAndRefusedWithoutAgent(t *testing.T) {
	l, svcs, agents := serviceFixture(t)
	agents.runtime = func(cmd agent.Command) agentrt.Runtime {
		rt, _ := d2rt(l, cmd)
		return rt
	}
	s := &HostSources{Ops: svcs.Ops, Keys: testKey}
	ctx := context.Background()
	if _, err := s.Put(ctx, adminSubject, resticSource("data")); err != nil {
		t.Fatal(err)
	}
	// Container agent without the restic capability: refused, with a reason.
	setReports(t, l, "sw1", "h1", agentrt.ContainerInfo(false))
	got, _ := s.Get(ctx, "data")
	if got.Runtime != "" || !strings.Contains(got.RuntimeNote, "restic") {
		t.Fatalf("view without capability: %+v", got)
	}
	if _, err := s.Run(ctx, adminSubject, "data"); !errors.Is(err, api.ErrRuntimeUnavailable) {
		t.Fatalf("run without capability: %v", err)
	}
	if n := len(agents.commands()); n != 0 {
		t.Fatalf("%d commands sent to a host without the capability", n)
	}
	// Container with restic, then native preferred.
	setReports(t, l, "sw1", "h1", agentrt.ContainerInfo(true))
	if got, _ = s.Get(ctx, "data"); got.Runtime != "container" {
		t.Fatalf("container view: %+v", got)
	}
	setReports(t, l, "sw1", "h1", agentrt.ContainerInfo(true), agentrt.NativeInfo(true))
	if got, _ = s.Get(ctx, "data"); got.Runtime != "native" {
		t.Fatalf("native preferred view: %+v", got)
	}
	// The computed fields are never stored.
	in := resticSource("data")
	in.Runtime, in.RuntimeNote = "native", "forged"
	if _, err := s.Put(ctx, adminSubject, in); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := Get[api.HostSource](l.Store(), CollHostSources, "data")
	if raw.Value.Runtime != "" || raw.Value.RuntimeNote != "" {
		t.Fatalf("stored computed fields: %+v", raw.Value)
	}
}

// d2rt resolves the runtime a step would use on h1 (test double of the
// directory's routing).
func d2rt(l *Service, cmd agent.Command) (agentrt.Runtime, error) {
	h, _, _ := Get[HostRecord](l.Store(), CollHosts, HostKey("sw1", "h1"))
	req, _ := agentrt.RequirementFor(string(cmd.Kind))
	return routeHost(h.Value, req)
}

func TestOperationRecordsExecutingRuntimeInViewAPIAndAudit(t *testing.T) {
	l, svcs, agents := serviceFixture(t)
	setReports(t, l, "sw1", "h1", agentrt.ContainerInfo(true), agentrt.NativeInfo(true))
	agents.runtime = func(cmd agent.Command) agentrt.Runtime { rt, _ := d2rt(l, cmd); return rt }
	s := &HostSources{Ops: svcs.Ops, Keys: testKey}
	ctx := context.Background()
	if _, err := s.Put(ctx, adminSubject, resticSource("data")); err != nil {
		t.Fatal(err)
	}
	ref, err := s.Run(ctx, adminSubject, "data")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "operation end", func() bool {
		op, ok, _ := Get[OperationRecord](l.Store(), CollOperations, ref.ID)
		return ok && op.Value.View != nil && op.Value.View.Status == runner.StatusSucceeded && !op.Value.EndedAt.IsZero()
	})
	op, _, _ := Get[OperationRecord](l.Store(), CollOperations, ref.ID)
	if sv := op.Value.View.Steps[0]; sv.Runtime != agentrt.Native || sv.LastResult.Runtime != agentrt.Native {
		t.Fatalf("step view: %+v", sv)
	}
	var timeline string
	for _, e := range op.Value.View.Timeline {
		timeline += e.Detail + "\n"
	}
	if !strings.Contains(timeline, "executed by runtime=native host=h1") {
		t.Fatalf("journal timeline lacks the runtime:\n%s", timeline)
	}
	c := Inventory{Store: l.Store()}
	ops, err := c.Operations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range ops {
		if o.ID == ref.ID {
			found = len(o.ExecutedBy) == 1 && o.ExecutedBy[0].Runtime == "native" && o.ExecutedBy[0].Host == "h1" && o.ExecutedBy[0].Agent == "agent/h1"
		}
	}
	if !found {
		t.Fatalf("API operation lacks executedBy: %+v", ops)
	}
	waitFor(t, "audit record naming the executing runtime", func() bool {
		audits, _ := List[authz.AuditRecord](l.Store(), CollAudit)
		for _, a := range audits {
			if strings.Contains(a.Value.Change, "executed by runtime=native agent=agent/h1") && strings.Contains(a.Value.Change, ref.ID) {
				return true
			}
		}
		return false
	})
}
