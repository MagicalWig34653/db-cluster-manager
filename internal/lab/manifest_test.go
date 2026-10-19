// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadManifestMissingFile(t *testing.T) {
	m, err := LoadManifest(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || len(m.Resources) != 0 {
		t.Fatalf("expected empty manifest, got %+v", m)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "m.json")
	m := &Manifest{Version: 1}
	r1 := Resource{Kind: "lima-vm", Name: NamePrefix + "a", Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Spec: &VMSpec{Name: NamePrefix + "a", CPUs: 2, RAMBytes: 4 << 30, DiskBytes: 20 << 30}}
	r2 := Resource{Kind: "directory", Name: NamePrefix + "dir", Created: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)}
	for _, r := range []Resource{r1, r2} {
		if err := m.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Resources, m.Resources) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got.Resources, m.Resources)
	}
	if r, ok := got.Find("lima-vm", r1.Name); !ok || r.Spec == nil || r.Spec.CPUs != 2 {
		t.Fatalf("Find failed: %+v %v", r, ok)
	}
}

func TestManifestSaveMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	if err := (&Manifest{Version: 1}).Save(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestManifestAddRejectsForeignNames(t *testing.T) {
	m := &Manifest{Version: 1}
	for _, n := range []string{"default", "thermoctl-test-vm"} {
		if err := m.Add(Resource{Kind: "lima-vm", Name: n}); err == nil {
			t.Errorf("Add(%q) should fail", n)
		}
	}
	if len(m.Resources) != 0 {
		t.Fatal("resources must stay empty")
	}
}

func TestManifestAddRejectsDuplicates(t *testing.T) {
	m := &Manifest{Version: 1}
	r := Resource{Kind: "lima-vm", Name: NamePrefix + "x"}
	if err := m.Add(r); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(r); err == nil {
		t.Fatal("duplicate should fail")
	}
	if err := m.Add(Resource{Kind: "directory", Name: NamePrefix + "x"}); err != nil {
		t.Fatalf("same name, other kind should be allowed: %v", err)
	}
}

func TestManifestRemove(t *testing.T) {
	m := &Manifest{Version: 1}
	_ = m.Add(Resource{Kind: "lima-vm", Name: NamePrefix + "x"})
	_ = m.Add(Resource{Kind: "directory", Name: NamePrefix + "x"})
	m.Remove("lima-vm", NamePrefix+"x")
	if _, ok := m.Find("lima-vm", NamePrefix+"x"); ok {
		t.Fatal("not removed")
	}
	if _, ok := m.Find("directory", NamePrefix+"x"); !ok {
		t.Fatal("wrong resource removed")
	}
	m.Remove("lima-vm", "absent")
}

func TestLoadManifestInvalid(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"bad.json": "{not json", "v2.json": `{"version": 2, "resources": []}`} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadManifest(p); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}
