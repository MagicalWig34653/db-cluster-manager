// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

type fakeRegs []registry.Config

func (f fakeRegs) List(context.Context) ([]registry.Config, error) { return f, nil }

func TestVulnInventoryFromRecords(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	st := s.Store()
	ctx := context.Background()
	put := func(coll, key string, v any) {
		t.Helper()
		if _, err := Put(st, coll, key, v, nil); err != nil {
			t.Fatal(err)
		}
	}
	spec := func(image string) json.RawMessage { return json.RawMessage(`{"image":"` + image + `"}`) }
	put(CollServices, "s1", ServiceRecord{ID: "s1", Swarm: "sw", Name: "site", Type: "container", Spec: spec("reg.example.test:5000/team/web:1.2.0")})
	put(CollServices, "s2", ServiceRecord{ID: "s2", Swarm: "sw", Name: "site2", Type: "container", Spec: spec("reg.example.test:5000/team/web:1.2.0")})
	put(CollServices, "s3", ServiceRecord{ID: "s3", Swarm: "sw", Name: "edge", Type: "traefik", Spec: spec("traefik:v3.1.2")})
	put(CollServices, "s4", ServiceRecord{ID: "s4", Swarm: "sw", Name: "cache", Type: "container", Spec: spec("redis:7.2.4")})
	put(CollServices, "s5", ServiceRecord{ID: "s5", Swarm: "sw", Name: "gone", Type: "container", Spec: spec("busybox:1.36"), DeleteOp: "op-1"})
	put(CollServices, "s6", ServiceRecord{ID: "s6", Swarm: "sw", Name: "pinned", Type: "container", Spec: spec("alpine@sha256:" + strings.Repeat("a", 64))})
	put(CollClusters, "c1", ClusterRecord{ID: "c1", Swarm: "sw", Name: "db1", Spec: galera.Cluster{Image: "mariadb:11.4.2"}})

	jobs := ImageJobStore{Store: st}
	web := imagebuild.WebServerSpec{Name: "web"}
	_ = jobs.Put(ctx, imagebuild.Job{ID: "j1", Status: imagebuild.JobFailed, Reference: "reg.example.test:5000/team/web:1.2.0", CreatedAt: time.Now()})
	_ = jobs.Put(ctx, imagebuild.Job{ID: "j2", Status: imagebuild.JobSucceeded, Spec: web, Reference: "reg.example.test:5000/team/web:1.2.0",
		ImageDigest: "sha256:abc", CreatedAt: time.Now()})
	inv := VulnInventory{Store: st, Jobs: jobs, Registries: fakeRegs{{ID: "r1", Kind: registry.KindGeneric, Endpoint: "https://reg.example.test:5000"}}}
	got, err := inv.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]vuln.InventoryEntry{}
	for _, e := range got {
		if err := e.Validate(); err != nil {
			t.Fatalf("%s: %v", e.Image, err)
		}
		by[e.Image] = e
	}
	if len(got) != 5 {
		t.Fatalf("want 5 entries (deleted service skipped, shared image grouped), got %d: %+v", len(got), got)
	}
	w := by["reg.example.test:5000/team/web:1.2.0"]
	if len(w.Services) != 2 || w.ServiceKind != vuln.ServiceKindWeb || w.Coverage != vuln.CoverageReference || w.Digest != "sha256:abc" ||
		w.Build == nil || w.Build.Target != (imagebuild.Target{Registry: "r1", Repository: "team/web", Tag: "1.2.0"}) {
		t.Fatalf("built web image: %+v", w)
	}
	if by["traefik:v3.1.2"].ServiceKind != vuln.ServiceKindProxy || by["redis:7.2.4"].ServiceKind != vuln.ServiceKindDatabase ||
		by["mariadb:11.4.2"].ServiceKind != vuln.ServiceKindGalera || by["mariadb:11.4.2"].Services[0] != "sw/db1" {
		t.Fatalf("kinds: %+v", by)
	}
	for _, e := range got {
		if strings.HasPrefix(e.Image, "alpine") && e.Coverage != vuln.CoverageUnknown {
			t.Fatalf("digest-only image must be unknown coverage: %+v", e)
		}
	}
	again, _ := inv.Inventory(ctx)
	if again[0].ID != got[0].ID {
		t.Fatal("entry IDs must be stable")
	}
}
