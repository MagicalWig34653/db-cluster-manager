// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
)

func TestCreateVolumeCreatesLabelledOwnedVolumeWithoutMarker(t *testing.T) {
	f := newFixture(t)
	root := f.volume("shop-n1")
	args := CreateVolumeArgs{Volume: "shop-n1", VolumeRoot: root, Labels: map[string]string{LabelCluster: "c1", LabelNode: "u1"}}
	r := f.mustDo(1, "op", "vol", KindCreateVolume, args)
	wantStatus(t, r, StatusSucceeded, "")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, MarkerFileName)); err == nil {
		t.Fatal("create-volume wrote an identity marker")
	}
	obj := f.docker.objects["shop-n1"]
	want := map[string]string{LabelInstallation: testInstallation, LabelCluster: "c1", LabelNode: "u1", LabelOpID: "op", LabelStepID: "vol"}
	if !reflect.DeepEqual(obj.Labels, want) || obj.Mountpoint != root {
		t.Fatalf("volume %+v, want labels %v mountpoint %s", obj, want, root)
	}
	f.restart()
	if !f.exec.owner.Allowed("shop-n1") {
		t.Fatal("new volume not in the allow-list after restart")
	}
	// A new step with the same arguments is idempotent: no second Docker create.
	wantStatus(t, f.mustDo(1, "op", "vol-again", KindCreateVolume, args), StatusSucceeded, "")
	if n := countPrefix(f.docker.Calls(), "create-volume"); n != 1 {
		t.Fatalf("%d create-volume calls, want 1", n)
	}
}

func TestCreateVolumeRefusals(t *testing.T) {
	f := newFixture(t)
	for name, a := range map[string]CreateVolumeArgs{
		"escape":        {Volume: "x", VolumeRoot: filepath.Join(f.base, "..", "x")},
		"base itself":   {Volume: "x", VolumeRoot: f.base},
		"nested":        {Volume: "x", VolumeRoot: filepath.Join(f.base, "a", "x")},
		"name mismatch": {Volume: "x", VolumeRoot: f.volume("y")},
		"traversal":     {Volume: "../x", VolumeRoot: f.volume("../x")},
		"empty name":    {Volume: "", VolumeRoot: f.base},
	} {
		if _, err := f.do(1, "op", "s-"+name, KindCreateVolume, a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := countPrefix(f.docker.Calls(), "create-volume"); n != 0 {
		t.Fatal("Docker volume created for refused args")
	}
	wantStatus(t, f.mustDo(1, "op", "foreign-label", KindCreateVolume,
		CreateVolumeArgs{Volume: "x", VolumeRoot: f.volume("x"), Labels: map[string]string{LabelInstallation: "other"}}), StatusRefused, CodeNotOwned)
	// Existing non-empty directory without a volume is unknown data.
	if err := os.MkdirAll(f.volume("full"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.volume("full"), "data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "op", "full", KindCreateVolume, CreateVolumeArgs{Volume: "full", VolumeRoot: f.volume("full")}), StatusRefused, CodeUnknownData)
	// An existing volume of the name that is not owned is refused untouched.
	f.docker.objects["foreign-vol"] = Object{Kind: KindVolume, ID: "foreign-vol", Labels: map[string]string{LabelInstallation: "other"}, Mountpoint: f.volume("foreign-vol")}
	wantStatus(t, f.mustDo(1, "op", "fv", KindCreateVolume, CreateVolumeArgs{Volume: "foreign-vol", VolumeRoot: f.volume("foreign-vol")}), StatusRefused, CodeNotOwned)
	// An owned volume with another mountpoint is a mismatch.
	wantStatus(t, f.mustDo(1, "op", "mm", KindCreateVolume, CreateVolumeArgs{Volume: "vol-1", VolumeRoot: f.volume("vol-1")}), StatusRefused, CodeVolumeMismatch)
	if n := countPrefix(f.docker.Calls(), "create-volume"); n != 0 {
		t.Fatal("Docker volume created despite refusals")
	}
}

func TestCreateVolumeSymlinkLeafRefused(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(f.volume("galera-1"), f.volume("link")); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "op", "link", KindCreateVolume, CreateVolumeArgs{Volume: "link", VolumeRoot: f.volume("link")}), StatusRefused, CodeInvalidPath)
	// A symlinked parent escaping the base is refused too.
	outside := t.TempDir()
	if err := os.Remove(f.volume("link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.volume("esc")); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, f.mustDo(1, "op", "esc", KindCreateVolume, CreateVolumeArgs{Volume: "esc", VolumeRoot: f.volume("esc")}), StatusRefused, CodeInvalidPath)
}

func TestCreateVolumeStaleTermNoEffect(t *testing.T) {
	f := newFixture(t)
	f.mustDo(5, "op", "a", KindCreateService, spec("other"))
	_, err := f.do(4, "op", "vol", KindCreateVolume, CreateVolumeArgs{Volume: "v", VolumeRoot: f.volume("v")})
	if err == nil || !isStale(err) {
		t.Fatalf("err = %v, want stale term", err)
	}
	if _, serr := os.Stat(f.volume("v")); serr == nil {
		t.Fatal("directory created for a stale term")
	}
}

func isStale(err error) bool { return err != nil && errors.Is(err, fencing.ErrStaleTerm) }

// updateFixture creates a galera-like service via create-service so the
// fixture has an owned service with constraints, mounts and a bootstrap arg.
func updateFixture(t *testing.T) (*fixture, ServiceSpec, string) {
	t.Helper()
	f := newFixture(t)
	s := ServiceSpec{
		Name: "shop-n1", Image: "mariadb:11.4", Replicas: 1,
		Labels:      map[string]string{LabelCluster: "c1", LabelNode: "u1"},
		Args:        []string{"--datadir=/dbcm/data", "--wsrep-new-cluster"},
		Mounts:      []Mount{{Type: "volume", Source: "shop-n1", Target: "/dbcm"}},
		Constraints: []string{"node.id==h1"},
	}
	r := f.mustDo(1, "op", "svc", KindCreateService, s)
	wantStatus(t, r, StatusSucceeded, "")
	return f, s, "svc-new-1"
}

func TestUpdateServiceRemovesBootstrapFlag(t *testing.T) {
	f, s, id := updateFixture(t)
	s.Args = []string{"--datadir=/dbcm/data"}
	r := f.mustDo(1, "op2", "upd", KindUpdateService, UpdateServiceArgs{ServiceName: s.Name, Spec: s})
	wantStatus(t, r, StatusSucceeded, "")
	got := f.docker.specs[id]
	if !reflect.DeepEqual(got.Args, s.Args) {
		t.Fatalf("args %v", got.Args)
	}
	want := map[string]string{LabelInstallation: testInstallation, LabelCluster: "c1", LabelNode: "u1", LabelOpID: "op", LabelStepID: "svc"}
	if !reflect.DeepEqual(got.Labels, want) {
		t.Fatalf("labels %v, want %v (creating op/step preserved, installation kept)", got.Labels, want)
	}
	// Replay returns the record without a second update.
	r2 := f.mustDo(1, "op2", "upd", KindUpdateService, UpdateServiceArgs{ServiceName: s.Name, Spec: s})
	if !r2.Replayed || countPrefix(f.docker.Calls(), "update-service") != 1 {
		t.Fatalf("replay %+v, calls %v", r2, f.docker.Calls())
	}
}

func TestUpdateServiceRefusals(t *testing.T) {
	f, s, _ := updateFixture(t)
	for name, mod := range map[string]struct {
		edit func(*ServiceSpec)
		code Code
	}{
		"installation label": {func(x *ServiceSpec) { x.Labels = map[string]string{LabelInstallation: "other"} }, CodeImmutable},
		"cluster label":      {func(x *ServiceSpec) { x.Labels = map[string]string{LabelCluster: "c2"} }, CodeImmutable},
		"node label":         {func(x *ServiceSpec) { x.Labels = map[string]string{LabelNode: "u2"} }, CodeImmutable},
		"constraint":         {func(x *ServiceSpec) { x.Constraints = []string{"node.id==h2"} }, CodeImmutable},
		"constraint removed": {func(x *ServiceSpec) { x.Constraints = nil }, CodeImmutable},
		"mount":              {func(x *ServiceSpec) { x.Mounts = []Mount{{Type: "volume", Source: "other", Target: "/dbcm"}} }, CodeImmutable},
		"replicas":           {func(x *ServiceSpec) { x.Replicas = 0 }, CodeImmutable},
		"foreign network":    {func(x *ServiceSpec) { x.Networks = []string{"unknown-net"} }, CodeNotFound},
	} {
		c := s
		c.Labels = map[string]string{LabelCluster: "c1", LabelNode: "u1"}
		mod.edit(&c)
		r := f.mustDo(1, "op2", "u-"+name, KindUpdateService, UpdateServiceArgs{ServiceName: s.Name, Spec: c})
		if r.Code != mod.code || r.Status == StatusSucceeded {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if n := countPrefix(f.docker.Calls(), "update-service"); n != 0 {
		t.Fatalf("%d update-service calls despite refusals", n)
	}
}

func TestUpdateServiceOnlyOwnedServices(t *testing.T) {
	f := newFixture(t)
	f.docker.objects["svc-unlisted"] = Object{Kind: KindService, ID: "svc-unlisted", Name: "unlisted", Labels: map[string]string{LabelInstallation: testInstallation}}
	f.docker.objects["svc-foreign"] = Object{Kind: KindService, ID: "svc-foreign", Name: "foreignsvc", Labels: map[string]string{LabelInstallation: "other"}}
	for _, name := range []string{"unlisted", "foreignsvc", "missing"} {
		r := f.mustDo(1, "op", "u-"+name, KindUpdateService, UpdateServiceArgs{ServiceName: name, Spec: ServiceSpec{Name: name, Image: "x"}})
		if r.Status == StatusSucceeded {
			t.Errorf("%s: updated", name)
		}
	}
	if n := countPrefix(f.docker.Calls(), "update-service"); n != 0 {
		t.Fatal("update reached Docker for unowned services")
	}
	if _, err := f.do(1, "op", "bad", KindUpdateService, UpdateServiceArgs{ServiceName: "a", Spec: ServiceSpec{Name: "b", Image: "x"}}); err == nil {
		t.Fatal("name mismatch accepted")
	}
}
