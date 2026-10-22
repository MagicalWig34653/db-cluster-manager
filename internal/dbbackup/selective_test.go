// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

func sourceCatalog() Catalog {
	return Catalog{
		Schemas: []Schema{
			{Name: "shop", Tables: []Table{
				{Name: "orders", Rows: 10, ForeignKeys: []ForeignKey{{Name: "fk_cust", RefSchema: "shop", RefTable: "customers"}}},
				{Name: "customers", Rows: 5},
				{Name: "invoices", ForeignKeys: []ForeignKey{{Name: "fk_acct", RefSchema: "billing", RefTable: "accounts"}}},
			}, Views: []string{"v_open"}},
			{Name: "billing", Tables: []Table{{Name: "accounts"}}},
		},
		Accounts: []Account{
			{User: "app", Host: "%", AuthDigest: "d1", Schemas: []string{"shop"}},
			{User: "report", Host: "%", AuthDigest: "d2", Schemas: []string{"shop"}},
		},
	}
}

func selReq(target Catalog, maps ...Mapping) SelectiveRequest {
	return SelectiveRequest{BackupID: "b1", SourceClusterID: testCluster, TargetClusterID: testCluster, Source: sourceCatalog(),
		Exports: map[string]string{"shop": "obj-shop", "billing": "obj-billing"}, Target: target, Mappings: maps, Now: t0}
}

func kinds(p Preview, sev Severity) []ConflictKind {
	var out []ConflictKind
	for _, c := range p.Conflicts {
		if c.Severity == sev {
			out = append(out, c.Kind)
		}
	}
	return out
}

func TestPreviewNewTargetWithDependencyPresent(t *testing.T) {
	target := Catalog{Schemas: []Schema{{Name: "billing", Tables: []Table{{Name: "accounts"}}}},
		Accounts: []Account{{User: "app", Host: "%", AuthDigest: "other"}}}
	p, err := PreviewSelective(selReq(target, Mapping{Source: "shop", Target: "shop_restored"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocked() {
		t.Fatalf("blocked: %+v", p.Conflicts)
	}
	if len(p.Actions) != 2 || p.Actions[0].Kind != "create-schema" || p.Actions[1].Object != "obj-shop" {
		t.Fatalf("actions %+v", p.Actions)
	}
	info := kinds(p, SeverityInfo)
	if !slices.Contains(info, ConflictAccountMissing) || !slices.Contains(info, ConflictAccountDiffers) {
		t.Fatalf("account findings %v", info)
	}
}

func TestPreviewConflicts(t *testing.T) {
	recent := t0.Add(-time.Minute)
	target := Catalog{Schemas: []Schema{{Name: "shop", Tables: []Table{
		{Name: "orders", Rows: 99, UpdateTime: &recent}, {Name: "legacy"}}, Views: []string{"v_open"}}}}

	// Existing schema without replace, missing billing dependency, active writes.
	p, err := PreviewSelective(selReq(target, Mapping{Source: "shop", Target: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	bl := kinds(p, SeverityBlocker)
	for _, k := range []ConflictKind{ConflictSchemaExists, ConflictMissingDependency, ConflictTargetActive} {
		if !slices.Contains(bl, k) {
			t.Fatalf("blockers %v lack %s", bl, k)
		}
	}
	// Replace + accept active + restore billing too: only warnings remain.
	p, err = PreviewSelective(selReq(target, Mapping{Source: "shop", Target: "shop", Replace: true, AcceptActive: true},
		Mapping{Source: "billing", Target: "billing"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocked() {
		t.Fatalf("still blocked: %+v", p.Conflicts)
	}
	w := kinds(p, SeverityWarning)
	for _, k := range []ConflictKind{ConflictSchemaExists, ConflictTableReplaced, ConflictTargetActive, ConflictObjectReplaced} {
		if !slices.Contains(w, k) {
			t.Fatalf("warnings %v lack %s", w, k)
		}
	}
	if !slices.Contains(kinds(p, SeverityInfo), ConflictTableRetained) {
		t.Fatal("retained table not reported")
	}
	// Renaming a schema that another restored schema references by name.
	p, _ = PreviewSelective(selReq(Catalog{}, Mapping{Source: "shop", Target: "shop"}, Mapping{Source: "billing", Target: "billing_old"}))
	if !slices.Contains(kinds(p, SeverityBlocker), ConflictMissingDependency) {
		t.Fatalf("renamed dependency not blocked: %+v", p.Conflicts)
	}
	// No export for a schema.
	p, _ = PreviewSelective(selReq(Catalog{}, Mapping{Source: "crm", Target: "crm"}))
	if !slices.Contains(kinds(p, SeverityBlocker), ConflictNoExport) {
		t.Fatal("missing export not blocked")
	}
}

func TestPreviewRejectsBadMappings(t *testing.T) {
	for _, m := range [][]Mapping{
		{{Source: "shop", Target: "mysql"}},
		{{Source: "shop", Target: "a"}, {Source: "billing", Target: "a"}},
		{{Source: "sh op", Target: "x"}},
		nil,
	} {
		if _, err := PreviewSelective(selReq(Catalog{}, m...)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%v: %v", m, err)
		}
	}
}

func selBindings() SelectiveBindings {
	return SelectiveBindings{OpID: "op-s1", Writer: NodeState{Slot: "n1", HostID: "h1"}, ClusterName: "prod", JobImage: "dbcm/job:1",
		Network: "dbcm-prod-net", DBCredentialSecret: "restore-cred", BackendSecret: "backend-cfg", KeySecret: "restore-key",
		Job: agent.JobResources{NanoCPUs: 1e9, MemoryBytes: gib}}
}

func TestSelectivePlanRequiresMatchingApproval(t *testing.T) {
	base := t.TempDir()
	target := Catalog{Schemas: []Schema{{Name: "billing", Tables: []Table{{Name: "accounts"}}}}}
	p, err := PreviewSelective(selReq(target, Mapping{Source: "shop", Target: "shop_restored"}))
	if err != nil {
		t.Fatal(err)
	}
	appr := Approval{ID: "appr-3", Digest: p.Digest, By: "alice", Approved: t0}
	op, err := SelectivePlan(p, appr, selBindings())
	if err != nil {
		t.Fatal(err)
	}
	requireApprovalOnDestructive(t, op, "appr-3")
	ri := slices.IndexFunc(op.Steps, func(s runner.Step) bool { return s.Command == agent.KindRunRestore })
	var a agent.RunRestoreArgs
	_ = json.Unmarshal(op.Steps[ri].Args, &a)
	if a.Mode != agent.RestoreLogical || a.TargetDatabase != "shop_restored" || a.Database != "shop" || a.DBHost != "dbcm-prod-n1" || a.ApprovalID != "appr-3" {
		t.Fatalf("args %+v", a)
	}
	requireAgentAccepts(t, op, base)

	// Approval for another preview.
	changed := p
	changed.Mappings = []Mapping{{Source: "shop", Target: "shop"}}
	if _, err := SelectivePlan(changed, appr, selBindings()); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("tampered preview: %v", err)
	}
	if _, err := SelectivePlan(p, Approval{ID: "appr-3", Digest: "x", By: "alice"}, selBindings()); !errors.Is(err, ErrApprovalMismatch) {
		t.Fatalf("other digest: %v", err)
	}
	if _, err := SelectivePlan(p, Approval{Digest: p.Digest}, selBindings()); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("no approval: %v", err)
	}
	blocked, _ := PreviewSelective(selReq(Catalog{}, Mapping{Source: "shop", Target: "shop"}))
	if _, err := SelectivePlan(blocked, Approval{ID: "a", Digest: blocked.Digest, By: "alice"}, selBindings()); !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked preview: %v", err)
	}
}
