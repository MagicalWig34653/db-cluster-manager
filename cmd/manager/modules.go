// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/modules"
)

// collModules holds the module switches (key: module ID), replicated like
// every other manager state.
const collModules = "modules"

type moduleStore struct{ s *manager.Store }

func (m moduleStore) Load() (map[string]modules.State, error) {
	docs, err := manager.List[modules.State](m.s, collModules, manager.Consistent())
	if err != nil {
		return nil, err
	}
	out := make(map[string]modules.State, len(docs))
	for _, d := range docs {
		out[d.Key] = d.Value
	}
	return out, nil
}

func (m moduleStore) Save(id string, s modules.State) error {
	_, err := manager.Put(m.s, collModules, id, s, nil)
	return err
}

// orDefault lets an absent configuration section behave like an empty one.
func orDefault[T any](c *T) *T {
	if c == nil {
		return new(T)
	}
	return c
}

// moduleDefaults maps the configuration file to the initial module states.
// A module that an administrator switched keeps that state; the file is only
// consulted for modules without a stored state.
func (m *managerConfig) moduleDefaults() map[string]bool {
	return map[string]bool{
		"patch":          m.Patch.on(),
		"image-update":   m.ImageUpdate.on(),
		"image-build":    m.ImageBuild != nil,
		"image-composer": m.ImageComposer.on(),
		"bootstrap":      m.ServerBootstrap.on(),
		"vuln":           m.Vuln != nil,
		"provisioning":   m.ExperimentalProvisioning,
		"kubernetes":     m.ExperimentalKubernetesProvisioning,
		// These had no switch before; they stay on where they were usable.
		"host-backups":  m.Backup != nil,
		"backup-copies": m.Backup != nil,
	}
}

// missingConfig names the configuration a module cannot run without.
func (rt *managedRuntime) missingConfig(id string) []string {
	switch {
	case id == "patch" && rt.pki == nil, id == "bootstrap" && rt.pki == nil:
		return []string{"manager.agentTransport"}
	case id == "bootstrap" && rt.bootstrapCfg.releaseErr() != nil:
		return []string{"manager.serverBootstrap.agentRelease"}
	case id == "image-build" && rt.imageCfg == nil:
		return []string{"manager.imageBuild"}
	case id == "vuln" && rt.vulnCfg == nil:
		return []string{"manager.vuln"}
	}
	return nil
}

// on reports whether the leadership's registry runs the module.
func (rt *managedRuntime) on(id string) bool {
	r := rt.modules.Load()
	return r != nil && r.Enabled(id)
}
