// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

func TestBootstrapAgentScripts(t *testing.T) {
	spec := provision.AgentInstallSpec{
		Host: provision.Host{ID: "h1", Address: "h1.example.test", User: "root"}, Mode: "report", ResticPaths: []string{"/srv/data"},
		Release:    provision.AgentRelease{Version: "v0.1.0", BinaryURL: "https://releases.example.test/agent", SHA256: strings.Repeat("a", 64)},
		ManagerURL: "https://manager.example.test:8443", CAFingerprint: strings.Repeat("AB:", 31) + "AB", Installation: "inst-1",
	}
	a, err := BootstrapAgentScripts{}.Native(spec)
	if err != nil || !strings.Contains(a.Install, "read -r TOKEN") || !strings.Contains(a.Install, "binary checksum mismatch") || a.Cleanup == "" {
		t.Fatalf("%v", err)
	}
	spec.Release.SHA256 = "nothex"
	if _, err := (BootstrapAgentScripts{}).Native(spec); err == nil {
		t.Fatal("invalid release accepted")
	}
}
