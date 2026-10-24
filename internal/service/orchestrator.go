package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Platform selects a renderer; execution remains a separate capability.
type Platform string

const (
	PlatformSwarm      Platform = "swarm"
	PlatformKubernetes Platform = "kubernetes"
)

// RenderedPlan is a reviewable export, including all semantic blockers.
type RenderedPlan struct {
	Platform         Platform   `json:"platform"`
	Source           Plan       `json:"source"`
	Artifacts        []Artifact `json:"artifacts"`
	Blockers         []string   `json:"blockers,omitempty"`
	RequiresApproval bool       `json:"requiresApproval"`
}

func (p RenderedPlan) Digest() string {
	b, _ := json.Marshal(p)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}
func (p RenderedPlan) Text() string { b, _ := json.MarshalIndent(p, "", "  "); return string(b) }

// Orchestrator renders the same service plan for a target platform.
// It deliberately offers no mutation methods during W10 groundwork.
type Orchestrator interface {
	Render(Plan) (RenderedPlan, error)
}

// Swarm preserves the current planner's artifacts and safety conditions.
type Swarm struct{}

func (Swarm) Render(p Plan) (RenderedPlan, error) {
	return RenderedPlan{Platform: PlatformSwarm, Source: p, Artifacts: p.Artifacts, Blockers: p.Blockers, RequiresApproval: true}, nil
}
