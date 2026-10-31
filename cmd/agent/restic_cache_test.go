// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The apply-mode unit has no writable home; restic needs a cache directory
// below the state directory (lab E17).
func TestEnsureResticCacheDir(t *testing.T) {
	t.Setenv("RESTIC_CACHE_DIR", "")
	state := t.TempDir()
	if err := ensureResticCacheDir(state); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(state, "restic-cache")
	if got := os.Getenv("RESTIC_CACHE_DIR"); got != want {
		t.Fatalf("RESTIC_CACHE_DIR = %q, want %q", got, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir: %v %v", fi, err)
	}
}

func TestEnsureResticCacheDirKeepsTheOperatorsChoice(t *testing.T) {
	t.Setenv("RESTIC_CACHE_DIR", "/custom/cache")
	state := t.TempDir()
	if err := ensureResticCacheDir(state); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("RESTIC_CACHE_DIR"); got != "/custom/cache" {
		t.Fatalf("overridden: %q", got)
	}
	if _, err := os.Stat(filepath.Join(state, "restic-cache")); err == nil {
		t.Fatal("directory created although the operator chose one")
	}
}
