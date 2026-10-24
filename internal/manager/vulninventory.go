// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// RegistryLister lists the configured registries (registry.Service).
type RegistryLister interface {
	List(ctx context.Context) ([]registry.Config, error)
}

// VulnInventory builds the vulnerability inventory (X13, ADR 0015) from the
// manager's own records: the images of managed services (container and
// Traefik types) and of Galera clusters, enriched with the build record of
// an image the manager built (spec and registry target, so a remediation can
// be planned as a rebuild of the same composition).
//
// No SBOM is generated at build time yet, so every entry has reference
// coverage (image name and tag matched as ecosystem "docker") or unknown
// coverage (digest-only reference); unknown entries are reported as not
// covered, never as safe.
type VulnInventory struct {
	Store      *Store
	Jobs       imagebuild.JobStore
	Registries RegistryLister
}

var _ vuln.InventorySource = VulnInventory{}

// Inventory implements vuln.InventorySource. Entries are grouped by image
// reference and sorted by ID.
func (v VulnInventory) Inventory(ctx context.Context) ([]vuln.InventoryEntry, error) {
	type use struct {
		image    string
		services []string
		kind     vuln.ServiceKind
	}
	byImage := map[string]*use{}
	add := func(image, name string, kind vuln.ServiceKind) {
		if image == "" {
			return
		}
		u := byImage[image]
		if u == nil {
			u = &use{image: image, kind: kind}
			byImage[image] = u
		}
		u.services = append(u.services, name)
		// The most restrictive kind wins when one image backs several
		// services.
		if rank(kind) > rank(u.kind) {
			u.kind = kind
		}
	}

	svcs, err := List[ServiceRecord](v.Store, CollServices, Consistent())
	if err != nil {
		return nil, err
	}
	for _, d := range svcs {
		r := d.Value
		if r.DeleteOp != "" {
			continue
		}
		image, kind := serviceImage(r)
		add(image, r.Swarm+"/"+r.Name, kind)
	}
	clusters, err := List[ClusterRecord](v.Store, CollClusters, Consistent())
	if err != nil {
		return nil, err
	}
	for _, d := range clusters {
		add(d.Value.Spec.Image, d.Value.Swarm+"/"+d.Value.Name, vuln.ServiceKindGalera)
	}

	var jobs []imagebuild.Job
	if v.Jobs != nil {
		if jobs, err = v.Jobs.List(ctx); err != nil {
			return nil, err
		}
	}
	var regs []registry.Config
	if v.Registries != nil {
		if regs, err = v.Registries.List(ctx); err != nil {
			return nil, err
		}
	}

	out := make([]vuln.InventoryEntry, 0, len(byImage))
	for _, u := range byImage {
		sort.Strings(u.services)
		sum := sha256.Sum256([]byte(u.image))
		e := vuln.InventoryEntry{
			ID: "img-" + hex.EncodeToString(sum[:8]), Image: u.image, Services: u.services,
			ServiceKind: u.kind, Coverage: vuln.CoverageUnknown,
		}
		if _, tag := splitRef(u.image); tag != "" {
			e.Coverage = vuln.CoverageReference
		}
		if j, ok := jobFor(jobs, u.image); ok {
			e.Digest = j.ImageDigest
			if e.ServiceKind == vuln.ServiceKindGeneric {
				e.ServiceKind = vuln.ServiceKindWeb
			}
			if t, ok := targetOf(regs, j.Reference); ok {
				e.Build = &vuln.BuildSource{Spec: j.Spec, Target: t}
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func rank(k vuln.ServiceKind) int {
	switch k {
	case vuln.ServiceKindGalera:
		return 5
	case vuln.ServiceKindDatabase:
		return 4
	case vuln.ServiceKindRegistry:
		return 3
	case vuln.ServiceKindProxy:
		return 2
	case vuln.ServiceKindWeb:
		return 1
	}
	return 0
}

// databaseImages are repository names (last path component) treated as
// databases when found in a generic container service.
var databaseImages = []string{"mariadb", "mysql", "postgres", "postgresql", "mongo", "redis", "valkey", "percona", "galera", "cockroach", "clickhouse"}

// serviceImage extracts the image and the service kind of a service record.
// Unparsable specs yield no image.
func serviceImage(r ServiceRecord) (string, vuln.ServiceKind) {
	var s struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(r.Spec, &s); err != nil || s.Image == "" {
		return "", vuln.ServiceKindGeneric
	}
	if r.Type == service.TypeTraefik {
		return s.Image, vuln.ServiceKindProxy
	}
	name, _ := splitRef(s.Image)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	for _, d := range databaseImages {
		if strings.Contains(strings.ToLower(name), d) {
			return s.Image, vuln.ServiceKindDatabase
		}
	}
	if strings.Contains(strings.ToLower(name), "registry") {
		return s.Image, vuln.ServiceKindRegistry
	}
	return s.Image, vuln.ServiceKindGeneric
}

// splitRef returns the repository part (with host) and the tag of an image
// reference; a digest suffix is dropped.
func splitRef(ref string) (name, tag string) {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndexByte(ref, '/')
	if i := strings.LastIndexByte(ref, ':'); i > slash {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

// jobFor returns the newest succeeded build whose reference is the image
// (JobStore.List is newest first).
func jobFor(jobs []imagebuild.Job, image string) (imagebuild.Job, bool) {
	want, tag := splitRef(image)
	for _, j := range jobs {
		if j.Status != imagebuild.JobSucceeded {
			continue
		}
		n, t := splitRef(j.Reference)
		if n == want && t == tag {
			return j, true
		}
	}
	return imagebuild.Job{}, false
}

// targetOf maps a build reference back to its registry and repository.
func targetOf(regs []registry.Config, ref string) (imagebuild.Target, bool) {
	name, tag := splitRef(ref)
	host, repo, ok := strings.Cut(name, "/")
	if !ok || tag == "" {
		return imagebuild.Target{}, false
	}
	for _, c := range regs {
		var h string
		switch c.Kind {
		case registry.KindManaged:
			continue
		case registry.KindDockerHub:
			h = "docker.io"
		default:
			u, err := url.Parse(c.Endpoint)
			if err != nil {
				continue
			}
			h = u.Host
		}
		if h == host {
			return imagebuild.Target{Registry: c.ID, Repository: repo, Tag: tag}, true
		}
	}
	return imagebuild.Target{}, false
}
