// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A 4 GiB host must be reported as an integer byte count. awk's plain print
// turns 4023000*1024 into 4.12e+09, which the parser read as 0 (lab E17).
func TestPreflightMemoryIsAnInteger(t *testing.T) {
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(meminfo, []byte("MemTotal:        4023000 kB\nMemFree: 1 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := strings.Replace(preflightMemCmd, "/proc/meminfo", meminfo, 1)
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Skipf("awk not usable: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "4119552000" {
		t.Fatalf("mem = %q, want 4119552000", got)
	}
}

// The Docker step verifies the repository key with gpg, so the base-packages
// step must not be skipped on a host that has curl and sudo but no gpg (a fresh
// Debian 12 cloud image, lab E17).
func TestBasePackagesStepChecksForGpg(t *testing.T) {
	for _, osv := range [][2]string{{"debian", "12"}, {"almalinux", "9.4"}} {
		s := newSimHost(osv[0], osv[1])
		p := fullProfile()
		p.NativeAgent, p.ContainerAgent, p.Patch, p.Restic, p.ResticPaths = false, false, false, false, nil
		p.DedicatedKey, p.DisableRootLogin = false, false
		plan, _ := inspectAndBuild(t, s, p)
		if c := plan.Step("base-packages").Check; !strings.Contains(c, "command -v gpg") {
			t.Fatalf("%s: base-packages check does not look for gpg:\n%s", osv[0], c)
		}
	}
}
