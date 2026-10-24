// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
)

// S3 container ports and paths. The defaults follow MinIO, the proposed
// first image (a proposal, not a dependency): any server that reads its
// root credentials from files and serves the S3 API on one port and an
// optional console on another can be used.
const (
	S3DataDir      = "/data"
	s3APIPort      = 9000
	s3ConsolePort  = 9001
	s3RegionPatStr = `^[a-z0-9][a-z0-9-]{1,30}$`
)

var s3RegionRe = regexp.MustCompile(s3RegionPatStr)

// S3Spec is the desired state of an S3-compatible object storage server in
// single-node, single-drive mode (one local volume, no erasure coding).
//
// Environment contract (matches MinIO): MINIO_ROOT_USER_FILE and
// MINIO_ROOT_PASSWORD_FILE point to the root secrets, MINIO_REGION and
// MINIO_DOMAIN carry the region and the virtual-host domain, the command is
// "server /data --console-address :9001".
type S3Spec struct {
	StorageBase
	// RootUserSecret and RootPasswordSecret name Swarm secrets. The root
	// credentials never appear in the spec, the plan or the logs.
	RootUserSecret     string `json:"rootUserSecret"`
	RootPasswordSecret string `json:"rootPasswordSecret"`
	// Region is the S3 region name (default "us-east-1").
	Region string `json:"region,omitempty"`
	// Domain enables virtual-host-style addressing (bucket.domain).
	Domain string `json:"domain,omitempty"`
	// APIPublishedPort publishes the S3 API on the Swarm; 0 keeps it
	// internal. Publishing is not needed when a Traefik route is set.
	APIPublishedPort int `json:"apiPublishedPort,omitempty"`
	// Console enables the web console on its own port.
	Console bool `json:"console,omitempty"`
	// ConsolePublishedPort publishes the console; the plan warns about it.
	ConsolePublishedPort int `json:"consolePublishedPort,omitempty"`
	// PortMode is "ingress" (default) or "host" for published ports.
	PortMode string `json:"portMode,omitempty"`
	// APIRoute and ConsoleRoute expose the endpoints through Traefik.
	APIRoute     *StorageRoute `json:"apiRoute,omitempty"`
	ConsoleRoute *StorageRoute `json:"consoleRoute,omitempty"`
	// RouteNetwork is the network shared with Traefik; it must be in
	// networks.
	RouteNetwork string `json:"routeNetwork,omitempty"`
}

// S3 is the S3-compatible object storage service type.
type S3 struct{}

var _ Type = S3{}

// Name implements Type.
func (S3) Name() string { return TypeS3 }

func decodeS3(raw json.RawMessage) (S3Spec, error) {
	var s S3Spec
	if err := decodeStrict(raw, &s); err != nil {
		return S3Spec{}, err
	}
	return s, s.validate()
}

// Validate implements Type.
func (S3) Validate(spec json.RawMessage) error {
	_, err := decodeS3(spec)
	return err
}

func (s S3Spec) region() string {
	if s.Region == "" {
		return "us-east-1"
	}
	return s.Region
}

func (s S3Spec) validate() error {
	var p problems
	s.StorageBase.validate(&p)
	for _, x := range []struct{ what, v string }{{"rootUserSecret", s.RootUserSecret}, {"rootPasswordSecret", s.RootPasswordSecret}} {
		if x.v == "" || !identRe.MatchString(x.v) {
			p.addf("%s %q must name a Swarm secret", x.what, x.v)
		}
	}
	if s.RootUserSecret != "" && s.RootUserSecret == s.RootPasswordSecret {
		p.addf("rootUserSecret and rootPasswordSecret must be different secrets")
	}
	if !s3RegionRe.MatchString(s.region()) {
		p.addf("region %q is invalid", s.Region)
	}
	if s.Domain != "" && !hostRe.MatchString(s.Domain) {
		p.addf("domain %q is invalid", s.Domain)
	}
	validPublishedPort(&p, "s3 api", s.APIPublishedPort, s.PortMode)
	validPublishedPort(&p, "s3 console", s.ConsolePublishedPort, s.PortMode)
	if s.ConsolePublishedPort > 0 && !s.Console {
		p.addf("consolePublishedPort needs console")
	}
	if s.ConsoleRoute != nil && !s.Console {
		p.addf("consoleRoute needs console")
	}
	if s.APIPublishedPort > 0 && s.APIPublishedPort == s.ConsolePublishedPort {
		p.addf("the API and the console cannot publish the same port")
	}
	if s.APIRoute != nil {
		s.APIRoute.validate(&p, "api")
	}
	if s.ConsoleRoute != nil {
		s.ConsoleRoute.validate(&p, "console")
	}
	if s.RouteNetwork != "" {
		if !identRe.MatchString(s.RouteNetwork) {
			p.addf("routeNetwork %q is invalid", s.RouteNetwork)
		}
		found := false
		for _, n := range s.Networks {
			found = found || n == s.RouteNetwork
		}
		if !found {
			p.addf("routeNetwork %q must be one of networks", s.RouteNetwork)
		}
	}
	if (s.APIRoute != nil || s.ConsoleRoute != nil) && s.RouteNetwork == "" {
		p.addf("a Traefik route needs routeNetwork, the network shared with Traefik")
	}
	return p.err()
}

func (s S3Spec) container() ContainerSpec {
	c := s.StorageBase.container(S3DataDir)
	c.Secrets = sortedUnique([]string{s.RootUserSecret, s.RootPasswordSecret})
	c.Env = []EnvVar{
		{Name: "MINIO_ROOT_USER_FILE", Value: path.Join(SecretsDir, s.RootUserSecret)},
		{Name: "MINIO_ROOT_PASSWORD_FILE", Value: path.Join(SecretsDir, s.RootPasswordSecret)},
		{Name: "MINIO_REGION", Value: s.region()},
	}
	if s.Domain != "" {
		c.Env = append(c.Env, EnvVar{Name: "MINIO_DOMAIN", Value: s.Domain})
	}
	c.Command = nil
	c.Args = []string{"server", S3DataDir, "--address", fmt.Sprintf(":%d", s3APIPort)}
	if s.Console {
		c.Args = append(c.Args, "--console-address", fmt.Sprintf(":%d", s3ConsolePort))
	}
	return c
}

func (s S3Spec) ports() []Port {
	mode := s.PortMode
	if mode == "" {
		mode = "ingress"
	}
	var out []Port
	out = append(out, Port{Target: s3APIPort, Published: s.APIPublishedPort, Protocol: "tcp", Mode: pubMode(s.APIPublishedPort, mode)})
	if s.Console {
		out = append(out, Port{Target: s3ConsolePort, Published: s.ConsolePublishedPort, Protocol: "tcp", Mode: pubMode(s.ConsolePublishedPort, mode)})
	}
	return out
}

func pubMode(published int, mode string) string {
	if published == 0 {
		return ""
	}
	return mode
}

// Plan implements Type.
func (S3) Plan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	s, err := decodeS3(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	p, err := storagePlan(in, TypeS3, s.container(), func(sp *ServiceSpec) {
		sp.Ports = s.ports()
		if s.APIRoute != nil {
			mergeLabels(sp, s.APIRoute.labels(in.Name, "", s3APIPort, s.RouteNetwork))
		}
		if s.ConsoleRoute != nil {
			mergeLabels(sp, s.ConsoleRoute.labels(in.Name, "console", s3ConsolePort, s.RouteNetwork))
		}
	})
	if err != nil {
		return Plan{}, err
	}
	ports, err := portArtifact(*p.Steps[len(p.Steps)-1].Spec)
	if err != nil {
		return Plan{}, err
	}
	p.Artifacts = append(p.Artifacts, ports)
	if s.APIRoute != nil {
		p.Artifacts = append(p.Artifacts, Artifact{Name: "traefik-route-s3-api.yml", MediaType: "application/yaml", Content: s.APIRoute.dynamicConfig(in.Name, "", s3APIPort)})
	}
	if s.ConsoleRoute != nil {
		p.Artifacts = append(p.Artifacts, Artifact{Name: "traefik-route-s3-console.yml", MediaType: "application/yaml", Content: s.ConsoleRoute.dynamicConfig(in.Name, "console", s3ConsolePort)})
	}
	p.Warnings = append(p.Warnings, storageBackupWarning,
		"single-node, single-drive mode: no erasure coding and no replication; bitrot is detected but not repaired and a lost host loses all objects")
	if s.ConsolePublishedPort > 0 {
		p.Warnings = append(p.Warnings, "the console is published directly on the Swarm: it is an administration interface; route it through Traefik with an IP allow list or authentication instead")
	}
	if s.APIPublishedPort > 0 && s.APIRoute == nil {
		p.Warnings = append(p.Warnings, "the S3 API is published without TLS: credentials are signed but object data is not encrypted in transit; terminate TLS in front of it")
	}
	if s.APIRoute != nil && !s.APIRoute.TLS {
		p.Warnings = append(p.Warnings, "the S3 API route has no TLS")
	}
	if s.APIRoute != nil || s.ConsoleRoute != nil {
		p.Warnings = append(p.Warnings, "the traefik.* labels are read by a Traefik with the Swarm provider only; the file-provider snippet in the plan is the alternative, and the manager does not place it yet (see the Traefik type)")
	}
	if s.Domain != "" && s.APIRoute != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("virtual-host-style addressing needs a wildcard route for *.%s; the route above matches the listed hosts only", s.Domain))
	}
	return p, nil
}

// DeletePlan implements Type.
func (S3) DeletePlan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	s, err := decodeS3(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	return s.container().deletePlan(in, TypeS3), nil
}

// Health implements Type.
func (S3) Health(in HealthInput) (Health, error) {
	if _, err := decodeS3(in.Spec); err != nil {
		return Health{}, err
	}
	return observationHealth(in.Observed), nil
}
