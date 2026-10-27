// SPDX-License-Identifier: AGPL-3.0-only

package swarm

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// TestAgentSpecResticOptIn: restic host backups are opt-in. Without Restic
// the spec has no allow-list flag, no extra mount and no cache variable;
// with it each root becomes a flag plus a bind mount and the named secrets
// are mounted once (X14, lab E11).
func TestAgentSpecResticOptIn(t *testing.T) {
	m := AgentMTLS{ManagerURL: "https://m:7444", CAFingerprint: fp, TokenSecret: "dbcm-t", Token: []byte("x")}
	plain := AgentSpec(AgentDeployment{InstallationID: "i", Image: "a", VolumeBase: "/v", MTLS: &m})
	if slices.Contains(plain.Args, "-restic-allow-path") || len(plain.Mounts) != 3 {
		t.Fatalf("restic without opt-in: %v %v", plain.Args, plain.Mounts)
	}
	m.Restic = &AgentRestic{AllowPaths: []string{"/srv/home", "/srv/data"}, Secrets: []string{"restic-pw", "restic-s3"}}
	spec := AgentSpec(AgentDeployment{InstallationID: "i", Image: "a", VolumeBase: "/v", MTLS: &m})
	var roots []string
	for i, a := range spec.Args {
		if a == "-restic-allow-path" {
			roots = append(roots, spec.Args[i+1])
		}
	}
	if !slices.Equal(roots, []string{"/srv/home", "/srv/data"}) {
		t.Fatalf("flags %v", spec.Args)
	}
	for _, p := range roots {
		if !slices.Contains(spec.Mounts, agent.Mount{Type: "bind", Source: p, Target: p}) {
			t.Fatalf("no mount for %s: %v", p, spec.Mounts)
		}
	}
	for _, s := range []string{"restic-pw", "restic-s3"} {
		if !slices.Contains(spec.Secrets, s) {
			t.Fatalf("secret %s missing: %v", s, spec.Secrets)
		}
	}
	if !slices.Contains(spec.Env, "RESTIC_CACHE_DIR="+AgentStateDir+"/restic-cache") {
		t.Fatalf("env %v", spec.Env)
	}
	for name, r := range map[string]AgentRestic{
		"relative":   {AllowPaths: []string{"srv"}},
		"root":       {AllowPaths: []string{"/"}},
		"unclean":    {AllowPaths: []string{"/srv/../etc"}},
		"socket":     {AllowPaths: []string{"/var/run/docker.sock"}},
		"secrets":    {AllowPaths: []string{"/run/secrets"}},
		"state":      {AllowPaths: []string{"/var/lib/dbcm-agent"}},
		"empty":      {},
		"bad secret": {AllowPaths: []string{"/srv/x"}, Secrets: []string{"a/b"}},
	} {
		bad := m
		rr := r
		bad.Restic = &rr
		if _, err := DeployAgents(context.Background(), newFakeConn(), AgentDeployment{InstallationID: "i", Image: "a", VolumeBase: "/v", MTLS: &bad}); !errors.Is(err, ErrInvalidDeployment) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}
