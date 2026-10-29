// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// RegistryOpener is the registry configuration store with its sealed
// credentials (registry.Service implements it).
type RegistryOpener interface {
	List(ctx context.Context) ([]registry.Config, error)
	Open(ctx context.Context, id string) (registry.Registry, registry.Config, error)
}

// RegistryResolver reads tags and digests over the OCI distribution API v2
// (registry.External). The registry of a reference is matched to a
// configured registry by host; its credential comes from the sealed
// registry secret store and is used for that registry only (the client sends
// it to the registry host and its token realm, never elsewhere). A registry
// that is not configured is read anonymously. Nothing is written.
type RegistryResolver struct {
	Registries RegistryOpener
	// Anonymous builds a client for a registry without configuration; nil
	// selects registry.NewExternal with the well-known endpoints.
	Anonymous func(host string) (registry.Registry, error)
}

var _ Resolver = (*RegistryResolver)(nil)

func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	switch u.Host {
	case "registry-1.docker.io", "index.docker.io", "docker.io":
		return DockerHub
	}
	return u.Host
}

func (r *RegistryResolver) client(ctx context.Context, ref Ref) (registry.Registry, error) {
	if r.Registries != nil {
		cfgs, err := r.Registries.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range cfgs {
			if c.Kind == registry.KindManaged {
				continue
			}
			host := endpointHost(c.Endpoint)
			if c.Endpoint == "" && c.Kind == registry.KindDockerHub {
				host = DockerHub
			}
			if c.Endpoint == "" && c.Kind == registry.KindGHCR {
				host = "ghcr.io"
			}
			if host == ref.Registry {
				cl, _, err := r.Registries.Open(ctx, c.ID)
				return cl, err
			}
		}
	}
	if r.Anonymous != nil {
		return r.Anonymous(ref.Registry)
	}
	return anonymousClient(ref.Registry)
}

func anonymousClient(host string) (registry.Registry, error) {
	switch host {
	case DockerHub:
		return registry.NewExternal(registry.ExternalConfig{Kind: registry.KindDockerHub})
	case "ghcr.io":
		return registry.NewExternal(registry.ExternalConfig{Kind: registry.KindGHCR})
	}
	scheme := "https://"
	if h, _, err := net.SplitHostPort(host); err == nil && (h == "localhost" || net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback()) {
		scheme = "http://"
	}
	return registry.NewExternal(registry.ExternalConfig{Kind: registry.KindGeneric, Endpoint: scheme + host})
}

// Tags implements Resolver.
func (r *RegistryResolver) Tags(ctx context.Context, ref Ref) ([]string, error) {
	cl, err := r.client(ctx, ref)
	if err != nil {
		return nil, err
	}
	tags, err := cl.List(ctx, ref.Repo)
	if err != nil {
		return nil, fmt.Errorf("list tags of %s/%s: %w", ref.Registry, ref.Repo, err)
	}
	return tags, nil
}

// Digest implements Resolver.
func (r *RegistryResolver) Digest(ctx context.Context, ref Ref) (string, error) {
	if ref.Tag == "" {
		return "", errors.New("imageupdate: a tag is needed to resolve a digest")
	}
	cl, err := r.client(ctx, ref)
	if err != nil {
		return "", err
	}
	d, err := cl.Resolve(ctx, ref.Repo, ref.Tag)
	if err != nil {
		return "", fmt.Errorf("resolve %s/%s:%s: %w", ref.Registry, ref.Repo, ref.Tag, err)
	}
	if !strings.HasPrefix(d, "sha256:") {
		return "", errors.New("imageupdate: unsupported digest algorithm")
	}
	return d, nil
}
