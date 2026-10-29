// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// BootstrapAgentScripts adapts the reviewed native agent install script to
// the fresh-server bootstrap (X18, ADR 0027). The install script reads the
// enrollment token from stdin, verifies the binary checksum and refuses to
// overwrite an existing agent.
type BootstrapAgentScripts struct{}

var _ provision.AgentScripts = BootstrapAgentScripts{}

// Native implements provision.AgentScripts.
func (BootstrapAgentScripts) Native(s provision.AgentInstallSpec) (provision.AgentScript, error) {
	req := InstallRequest{
		ID: "bootstrap", Host: s.Host, Mode: InstallMode(s.Mode), BinaryURL: s.Release.BinaryURL, BinarySHA256: s.Release.SHA256,
		Version: s.Release.Version, ManagerURL: s.ManagerURL, CAFingerprint: s.CAFingerprint, Installation: s.Installation,
		ResticPaths: s.ResticPaths,
	}
	if err := req.Validate(); err != nil {
		return provision.AgentScript{}, fmt.Errorf("agent install request: %w", err)
	}
	return provision.AgentScript{Install: InstallScript(req, UnitFile(req)), Cleanup: UninstallScript()}, nil
}
