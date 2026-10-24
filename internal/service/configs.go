// SPDX-License-Identifier: AGPL-3.0-only

package service

import "encoding/json"

// ManagedConfigName is scoped to the manager-issued service identity.
func ManagedConfigName(in Context, artifact string) string {
	return SwarmServiceName(in.Name) + "-" + VolumeIdentity(in.ServiceID, artifact) + "-config"
}

// deliverConfig inserts the config before service creation and refreshes the
// review artifact so preview and execution describe exactly the same spec.
func deliverConfig(p *Plan, in Context, artifact, target string) {
	name := ManagedConfigName(in, artifact)
	id := "create-config-" + artifact
	for i, s := range p.Steps {
		if s.Kind != StepCreateService {
			continue
		}
		s.Spec.Configs = append(s.Spec.Configs, ConfigRef{Config: name, Target: target})
		s.After = append(s.After, id)
		p.Steps[i] = s
		config := Step{ID: id, Kind: StepCreateConfig, Config: name, Artifact: artifact, Note: "create owned config " + name}
		steps := append([]Step{}, p.Steps[:i]...)
		steps = append(steps, config)
		p.Steps = append(steps, p.Steps[i:]...)
		raw, _ := json.MarshalIndent(s.Spec, "", "  ")
		for j := range p.Artifacts {
			if p.Artifacts[j].Name == "swarm-service.json" {
				p.Artifacts[j].Content = string(raw) + "\n"
			}
		}
		return
	}
}

func removeManagedConfig(p *Plan, in Context, artifact string) {
	name := ManagedConfigName(in, artifact)
	p.Steps = append(p.Steps, Step{ID: "remove-config-" + artifact, Kind: StepRemoveConfig, Config: name, After: []string{p.Steps[0].ID}, Destructive: true, Note: "remove owned config " + name})
}
