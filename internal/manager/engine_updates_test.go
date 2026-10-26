// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
)

type fakeEngineVersions []string

func (f fakeEngineVersions) Discover(_ context.Context, h HostRecord) (EngineVersionSet, error) {
	return EngineVersionSet{Current: h.EngineVersion, Available: f}, nil
}

type mismatchedEngineVersions struct{}

func (mismatchedEngineVersions) Discover(context.Context, HostRecord) (EngineVersionSet, error) {
	return EngineVersionSet{Current: "29.7.0", Available: []string{"29.9.1"}}, nil
}

func TestEngineUpdatePlanAndRunbook(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	ops, _ := replaceFixture(t, s, 2)
	ctl := &Control{Ops: ops, EngineVersions: fakeEngineVersions{"29.9.1", "bad", "29.9.1"}}
	h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "h4"))
	h.Value.EngineVersion = "29.8.0"
	h.Value.Role = "worker"
	h.Value.ObservedAt = s.cfg.Now()
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", "h4"), h.Value, IfVersion(h.Version)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := ctl.EngineUpdatePlan(ctx, nobody, "sw1", "h4", "29.9.1"); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("authz: %v", err)
	}
	p, err := ctl.EngineUpdatePlan(ctx, adminSubject, "sw1", "h4", "29.9.1")
	if err != nil || p.Exportable || len(p.AvailableVersions) != 1 || p.Digest == "" {
		t.Fatalf("plan: %+v %v", p, err)
	}
	ctl.EngineVersions = mismatchedEngineVersions{}
	mismatch, err := ctl.EngineUpdatePlan(ctx, adminSubject, "sw1", "h4", "29.9.1")
	if err != nil || !strings.Contains(strings.Join(mismatch.Blockers, " | "), "differs from Swarm inventory") {
		t.Fatalf("version mismatch: %+v %v", mismatch, err)
	}
	ctl.EngineVersions = fakeEngineVersions{"29.9.1", "bad", "29.9.1"}
	if _, err := ctl.ExportEngineRunbook(ctx, adminSubject, "sw1", "h4", api.EngineRunbookRequest{TargetVersion: "29.9.1", Digest: p.Digest}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("premature export: %v", err)
	}
	if _, err := ops.EnterMaintenance(adminSubject, "sw1", "h4", "engine"); err != nil {
		t.Fatal(err)
	}
	ready, err := ctl.EngineUpdatePlan(ctx, adminSubject, "sw1", "h4", "29.9.1")
	if err != nil || !ready.Exportable || ready.Digest == p.Digest {
		t.Fatalf("ready: %+v %v", ready, err)
	}
	p = ready
	if _, err := ctl.ExportEngineRunbook(ctx, adminSubject, "sw1", "h4", api.EngineRunbookRequest{TargetVersion: "29.9.1", Digest: "sha256:" + strings.Repeat("0", 64)}); !errors.Is(err, api.ErrApprovalMismatch) {
		t.Fatalf("digest: %v", err)
	}
	if _, err := ctl.ExportEngineRunbook(ctx, nobody, "sw1", "h4", api.EngineRunbookRequest{TargetVersion: "29.9.1", Digest: p.Digest}); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("export authz: %v", err)
	}
	book, err := ctl.ExportEngineRunbook(ctx, adminSubject, "sw1", "h4", api.EngineRunbookRequest{TargetVersion: "29.9.1", Digest: p.Digest})
	if err != nil || !strings.Contains(book.Script, "docker node update --availability drain h4") || strings.Contains(book.Script, "apt-get install") {
		t.Fatalf("runbook: %+v %v", book, err)
	}
	if _, err := ctl.ExportEngineRunbook(ctx, adminSubject, "sw1", "h4", api.EngineRunbookRequest{TargetVersion: "29.9.1", Digest: "sha256:" + strings.Repeat("0", 64)}); !errors.Is(err, api.ErrApprovalMismatch) {
		t.Fatalf("stale export: %v", err)
	}
}

func TestEngineUpdateQuorumAndUnknownCompatibility(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	ops := seed(t, s, 3)
	for _, id := range []string{"h1", "h2", "h3"} {
		h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", id))
		h.Value.Role, h.Value.EngineVersion = "manager", "29.8.0"
		h.Value.ObservedAt = s.cfg.Now()
		if _, err := Put(s.Store(), CollHosts, HostKey("sw1", id), h.Value, IfVersion(h.Version)); err != nil {
			t.Fatal(err)
		}
	}
	ctl := &Control{Ops: ops, EngineVersions: fakeEngineVersions{"29.9.1", "30.0.0"}}
	if _, err := ops.EnterMaintenance(adminSubject, "sw1", "h1", "engine"); err != nil {
		t.Fatal(err)
	}
	p, err := ctl.EngineUpdatePlan(context.Background(), adminSubject, "sw1", "h1", "29.9.1")
	if err != nil || !p.Exportable || !p.PinnedManager {
		t.Fatalf("manager plan: %+v %v", p, err)
	}
	if _, err := ctl.ExportEngineRunbook(context.Background(), adminSubject, "sw1", "h1", api.EngineRunbookRequest{TargetVersion: "29.9.1", Digest: p.Digest}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("pinned confirmation: %v", err)
	}
	other, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "h2"))
	other.Value.Maintenance = &HostMaintenanceRecord{}
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", "h2"), other.Value, IfVersion(other.Version)); err != nil {
		t.Fatal(err)
	}
	blocked, err := ctl.EngineUpdatePlan(context.Background(), adminSubject, "sw1", "h1", "29.9.1")
	if err != nil || !strings.Contains(strings.Join(blocked.Blockers, " | "), "Another host") {
		t.Fatalf("parallel maintenance: %+v %v", blocked, err)
	}
	other, _, _ = Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "h2"))
	other.Value.Maintenance = nil
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", "h2"), other.Value, IfVersion(other.Version)); err != nil {
		t.Fatal(err)
	}
	q, err := ctl.EngineUpdatePlan(context.Background(), adminSubject, "sw1", "h1", "30.0.0")
	if err != nil || q.Exportable {
		t.Fatalf("unknown compatibility: %+v %v", q, err)
	}
	h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey("sw1", "h2"))
	h.Value.State = "down"
	if _, err := Put(s.Store(), CollHosts, HostKey("sw1", "h2"), h.Value, IfVersion(h.Version)); err != nil {
		t.Fatal(err)
	}
	r, err := ctl.EngineUpdatePlan(context.Background(), adminSubject, "sw1", "h1", "29.9.1")
	if err != nil || r.Exportable {
		t.Fatalf("quorum: %+v %v", r, err)
	}
}
