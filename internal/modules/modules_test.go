// SPDX-License-Identifier: AGPL-3.0-only

package modules

import (
	"errors"
	"slices"
	"testing"
)

type memStore struct {
	m   map[string]State
	err error
}

func (s *memStore) Load() (map[string]State, error) { return s.m, s.err }
func (s *memStore) Save(id string, st State) error {
	if s.err != nil {
		return s.err
	}
	s.m[id] = st
	return nil
}

func newReg(t *testing.T, defaults map[string]bool, missing func(string) []string) (*Registry, *memStore) {
	t.Helper()
	st := &memStore{m: map[string]State{}}
	r, err := New(st, defaults, missing)
	if err != nil {
		t.Fatal(err)
	}
	return r, st
}

func TestCatalogIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Catalog() {
		if seen[m.ID] || m.Title.DE == "" || m.Title.EN == "" || m.Description.DE == "" || m.Description.EN == "" {
			t.Errorf("module %q: duplicate or missing text", m.ID)
		}
		seen[m.ID] = true
		for _, d := range m.Requires {
			if !seen[d] {
				t.Errorf("module %q requires %q, which must be listed before it", m.ID, d)
			}
		}
	}
}

func TestDefaultsApplyUntilStored(t *testing.T) {
	r, _ := newReg(t, map[string]bool{"patch": true}, nil)
	if !r.Enabled("patch") || r.Enabled("vuln") || r.Enabled("nope") {
		t.Fatal("config defaults not honoured")
	}
	if err := r.Set("patch", false, "alice"); err != nil {
		t.Fatal(err)
	}
	if r.Enabled("patch") {
		t.Fatal("stored state must win over the config default")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	r, st := newReg(t, nil, nil)
	if err := r.Set("vuln", true, "alice"); err != nil {
		t.Fatal(err)
	}
	r2, err := New(st, nil, nil)
	if err != nil || !r2.Enabled("vuln") {
		t.Fatalf("not restored: %v", err)
	}
	for _, i := range r2.List() {
		if i.ID == "vuln" && (i.By != "alice" || i.At == nil) {
			t.Fatalf("who/when missing: %+v", i)
		}
	}
}

func TestEnableNeedsConfigAndDependencies(t *testing.T) {
	missing := func(id string) []string {
		if id == "image-build" {
			return []string{"manager.imageBuild"}
		}
		return nil
	}
	r, _ := newReg(t, nil, missing)
	var be *BlockedError
	if err := r.Set("image-build", true, "a"); !errors.As(err, &be) || !slices.Equal(be.Config, []string{"manager.imageBuild"}) {
		t.Fatalf("config prerequisite not reported: %v", err)
	}
	if err := r.Set("image-composer", true, "a"); !errors.As(err, &be) || !slices.Equal(be.Modules, []string{"image-build"}) {
		t.Fatalf("dependency not reported: %v", err)
	}
	if err := r.Set("nope", true, "a"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown module: %v", err)
	}
}

func TestDisableBlockedByDependent(t *testing.T) {
	r, st := newReg(t, nil, nil)
	for _, id := range []string{"image-build", "image-composer"} {
		if err := r.Set(id, true, "a"); err != nil {
			t.Fatal(id, err)
		}
	}
	var be *BlockedError
	if err := r.Set("image-build", false, "a"); !errors.As(err, &be) || !slices.Equal(be.Dependents, []string{"image-composer"}) {
		t.Fatalf("dependent not reported: %v", err)
	}
	if err := r.Set("image-composer", false, "a"); err != nil {
		t.Fatal(err)
	}
	if err := r.Set("image-build", false, "a"); err != nil {
		t.Fatal(err)
	}
	if len(st.m) != 2 || st.m["image-build"].Enabled {
		t.Fatalf("disabling must store the state: %+v", st.m)
	}
}

func TestEnabledIsFailClosed(t *testing.T) {
	// A stored "on" whose prerequisite vanished (config edit) or whose
	// dependency is off does not run.
	gone := false
	r, st := newReg(t, nil, func(id string) []string {
		if gone && id == "image-build" {
			return []string{"manager.imageBuild"}
		}
		return nil
	})
	_ = r.Set("image-build", true, "a")
	_ = r.Set("image-composer", true, "a")
	gone = true
	if r.Enabled("image-build") || r.Enabled("image-composer") {
		t.Fatal("must not run without its prerequisite or dependency")
	}
	st.err = errors.New("down")
	if err := r.Set("vuln", true, "a"); err == nil || r.Enabled("vuln") {
		t.Fatal("a failed save must not change the state")
	}
}

func TestForRoute(t *testing.T) {
	for path, want := range map[string]string{
		"/patch/hosts/{id}":                    "patch",
		"/clusters/{id}/backup-copies":         "backup-copies",
		"/backup-copies/{id}/restore-proposal": "backup-copies",
		"/host-sources/{id}":                   "host-backups",
		"/clusters/{id}":                       "",
		"/patchwork":                           "",
	} {
		if got := ForRoute(path); got != want {
			t.Errorf("ForRoute(%q) = %q, want %q", path, got, want)
		}
	}
}
