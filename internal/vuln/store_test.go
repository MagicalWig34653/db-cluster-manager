// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

// newMetaNode starts a single-node in-memory Raft metastore.
func newMetaNode(t *testing.T) *metastore.Node {
	t.Helper()
	_, trans := raft.NewInmemTransport("n1")
	n, err := metastore.NewNode(metastore.Config{
		ID: "n1", Transport: trans, Peers: []metastore.Peer{{ID: "n1", Address: "n1"}}, Bootstrap: true,
		HeartbeatTimeout: 50 * time.Millisecond, ElectionTimeout: 50 * time.Millisecond,
		LeaderLeaseTimeout: 50 * time.Millisecond, CommitTimeout: 5 * time.Millisecond, ApplyTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for !n.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("no leader")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return n
}

func kvBackends(t *testing.T) map[string]KV {
	return map[string]KV{"memory": &MemoryKV{}, "metastore": MetastoreKV{Node: newMetaNode(t)}}
}

func TestStoreDedupAcrossFeeds(t *testing.T) {
	for name, kv := range kvBackends(t) {
		t.Run(name, func(t *testing.T) {
			s := &Store{KV: kv}
			// A GHSA record without CVE first ...
			n, err := s.UpsertAdvisories([]Advisory{{ID: "GHSA-aaaa-bbbb-cccc", Sources: []string{"github"}, Severity: SeverityHigh,
				Affect: []Affected{{Ecosystem: "npm", Package: "lib", Ranges: []Range{{Fixed: "2.0.0"}}}}}})
			if err != nil || n != 1 {
				t.Fatal(n, err)
			}
			// ... later the same advisory with a CVE alias from OSV, and the
			// CVE from NVD with a KEV listing, in one batch.
			_, err = s.UpsertAdvisories([]Advisory{
				{ID: "GHSA-aaaa-bbbb-cccc", Aliases: []string{"CVE-2026-5"}, Sources: []string{"osv"}},
				{ID: "CVE-2026-5", Sources: []string{"nvd"}, KEV: true, CVSS: 9.9},
			})
			if err != nil {
				t.Fatal(err)
			}
			advs, err := s.Advisories()
			if err != nil || len(advs) != 1 {
				t.Fatalf("%+v %v", advs, err)
			}
			a := advs[0]
			if a.ID != "CVE-2026-5" || !a.KEV || a.Severity != SeverityCritical || len(a.Sources) != 3 || a.Aliases[0] != "GHSA-aaaa-bbbb-cccc" || len(a.Affect) != 1 {
				t.Fatalf("%+v", a)
			}
			// Conditional writes.
			p := Proposal{ID: "vp-1", Status: StatusProposed}
			if err := s.PutProposal(p, 0); err != nil {
				t.Fatal(err)
			}
			if err := s.PutProposal(p, 0); !errors.Is(err, ErrConflict) {
				t.Fatal("second create must conflict", err)
			}
			_, v, ok, _ := s.Proposal("vp-1")
			if !ok || s.PutProposal(p, v) != nil || !errors.Is(s.PutProposal(p, v), ErrConflict) {
				t.Fatal("version check")
			}
			pol, ver, err := s.Policy()
			if err != nil || pol.Mode != ModeNotifyOnly || ver != 0 {
				t.Fatal("default policy is notify-only", pol)
			}
			if err := s.PutKEV([]string{"CVE-1", "CVE-1"}); err != nil {
				t.Fatal(err)
			}
			if k, _ := s.KEV(); len(k) != 1 || !k["CVE-1"] {
				t.Fatal(k)
			}
			r := AuditRecord{ID: "a", At: fixedNow, Event: EventPoll}
			if s.AppendAudit(r) != nil || !errors.Is(s.AppendAudit(r), ErrConflict) {
				t.Fatal("audit records are append-only")
			}
		})
	}
}

func inventoryFixture() []InventoryEntry {
	return []InventoryEntry{
		{ID: "img-shop", Image: "registry.corp.example/team/shop:1", Services: []string{"shop-web"}, ServiceKind: ServiceKindWeb, Coverage: CoverageSBOM,
			Components: []Component{{Ecosystem: "Alpine:v3.20", Name: "php83", Version: "8.3.10-r0"}, {Ecosystem: "alpine", Name: "musl", Version: "1.2.5-r0"}}},
		{ID: "img-galera", Image: "mariadb:11.4.2", Services: []string{"galera-shop"}, ServiceKind: ServiceKindGalera, Coverage: CoverageReference},
		{ID: "img-unknown", Image: "vendor/blob", Services: []string{"blob"}, ServiceKind: ServiceKindGeneric, Coverage: CoverageUnknown},
	}
}

func TestMatchAndReconcile(t *testing.T) {
	advs := []Advisory{
		Advisory{ID: "CVE-2026-109618", Severity: SeverityCritical, KEV: true, Sources: []string{"osv"},
			Affect: []Affected{{Ecosystem: "alpine", Package: "php83", Ranges: []Range{{Introduced: "0", Fixed: "8.3.12-r0"}}}}}.Normalize(),
		Advisory{ID: "CVE-2026-7000", Severity: SeverityHigh, Sources: []string{"github"},
			Affect: []Affected{{Ecosystem: "docker", Package: "mariadb", Ranges: []Range{{Introduced: "11.4.0", Fixed: "11.4.3"}}}}}.Normalize(),
		Advisory{ID: "CVE-2026-0001", Withdrawn: true,
			Affect: []Affected{{Ecosystem: "alpine", Package: "musl"}}}.Normalize(),
	}
	inv := inventoryFixture()
	fs := Match(advs, inv, fixedNow)
	if len(fs) != 2 {
		t.Fatalf("%+v", fs)
	}
	if fs[0].AdvisoryID != "CVE-2026-109618" || fs[0].Priority != P1 || fs[0].FixedVersions[0] != "8.3.12-r0" || fs[0].Status != StatusAffected {
		t.Fatalf("%+v", fs[0])
	}
	if fs[1].EntryID != "img-galera" || fs[1].Component.Name != "mariadb" || fs[1].Component.Version != "11.4.2" || fs[1].Kind != ServiceKindGalera {
		t.Fatalf("%+v", fs[1])
	}
	if FindingID("a", "b", Component{Name: "x", Version: "1"}) == FindingID("a", "b", Component{Name: "x", Version: "2"}) {
		t.Fatal("finding ID must include the version")
	}

	// An admin marks the Galera finding not affected; it stays so.
	stored := fs
	if err := stored[1].SetStatus(StatusNotAffected, "", "admin"); !errors.Is(err, ErrInvalidStatus) {
		t.Fatal("justification required")
	}
	_ = stored[1].SetStatus(StatusNotAffected, "vulnerable code path not compiled in", "admin")
	changed, created := Reconcile(stored, Match(advs, inv, fixedNow.Add(time.Hour)), fixedNow.Add(time.Hour))
	if len(created) != 0 || len(changed) != 2 || changed[1].Status != StatusNotAffected || !changed[0].FirstSeen.Equal(fixedNow) {
		t.Fatalf("%+v %v", changed, created)
	}
	// The shop image is upgraded: its finding resolves automatically.
	inv[0].Components[0].Version = "8.3.12-r0"
	later := fixedNow.Add(2 * time.Hour)
	changed, created = Reconcile(changed, Match(advs, inv, later), later)
	var resolved *Finding
	for i := range changed {
		if changed[i].EntryID == "img-shop" {
			resolved = &changed[i]
		}
	}
	if resolved == nil || resolved.Status != StatusFixed || !resolved.ResolvedAt.Equal(later) || len(created) != 0 {
		t.Fatalf("%+v", changed)
	}
	// A rollback to the vulnerable image re-opens it.
	inv[0].Components[0].Version = "8.3.10-r0"
	_, created = Reconcile(changed, Match(advs, inv, later), later)
	if len(created) != 1 {
		t.Fatal("rollback must re-open")
	}
}

func TestInventoryValidationAndImageRefs(t *testing.T) {
	for ref, want := range map[string][2]string{
		"registry.corp.example/team/shop:1":       {"team/shop", "1"},
		"mariadb:11.4.2":                          {"mariadb", "11.4.2"},
		"docker.io/library/nginx:1.27@sha256:abc": {"nginx", "1.27"},
		"localhost:5000/app":                      {"app", ""},
		"registry:5000/a/b:2":                     {"a/b", "2"},
	} {
		n, tag := splitImageRef(ref)
		if n != want[0] || tag != want[1] {
			t.Errorf("%s -> %s %s", ref, n, tag)
		}
	}
	bad := InventoryEntry{ID: "x", Image: "a", Coverage: CoverageSBOM, Components: []Component{{Name: "x"}}}
	if !errors.Is(bad.Validate(), ErrInvalidInventory) {
		t.Fatal()
	}
	if !errors.Is((InventoryEntry{ID: "x", Image: "a", Coverage: "full"}).Validate(), ErrInvalidInventory) {
		t.Fatal()
	}
}
