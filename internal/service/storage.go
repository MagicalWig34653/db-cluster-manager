// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Shared parts of the storage protocol server types (SFTP, FTP(S),
// S3-compatible object storage, NFS; X05). Each type derives a
// ContainerSpec and uses the container plan, so volumes, ownership labels,
// the spec artifact and the agent mapping are the same as for any service.
//
// The images named in the documentation are proposals. Nothing here depends
// on a specific image: the user gives the image, and the plan says what the
// image has to read (Swarm secrets below /run/secrets, DBCM_* variables).

var (
	// userNameRe is a portable POSIX user name.
	userNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// labelNameRe is an entry point or similar short name.
	entryPointNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	resolverNameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
)

// Storage service type names.
const (
	TypeSFTP = "sftp"
	TypeFTP  = "ftp"
	TypeS3   = "s3"
	TypeNFS  = "nfs"
)

// StorageBase is what every storage type shares: the image, where the data
// lives and the task limits. Data is always a managed local volume, so the
// service is pinned to one host and runs one replica.
type StorageBase struct {
	// Image is the server image. A tag or digest is required; with
	// RequireDigest only a digest is accepted, otherwise the plan warns
	// about a tag.
	Image         string `json:"image"`
	RequireDigest bool   `json:"requireDigest,omitempty"`
	// Placement must name the host that holds the data (placement.hostId).
	Placement Placement `json:"placement"`
	Networks  []string  `json:"networks,omitempty"`
	Resources Resources `json:"resources"`
	// Restart is "any" (default), "on-failure" or "none".
	Restart string `json:"restart,omitempty"`
}

func (b StorageBase) validate(p *problems) {
	if err := validImage(b.Image); err != nil {
		p.addf("%v", err)
	} else if b.RequireDigest && !strings.Contains(b.Image, "@sha256:") {
		p.addf("image %q must be pinned by digest (requireDigest is set)", b.Image)
	}
	if b.Placement.HostID == "" {
		p.addf("a storage service keeps its data on a local volume: placement.hostId is required")
	}
	// The base is validated like a container with a data volume.
	c := ContainerSpec{Image: b.Image, Networks: b.Networks, Resources: b.Resources, Placement: b.Placement, Restart: b.Restart,
		Volumes: []Volume{{Name: storageDataVolume, Target: "/data"}}}
	var sub problems
	c.validatePlacement(&sub)
	for _, n := range b.Networks {
		if !identRe.MatchString(n) {
			sub.addf("network name %q is invalid", n)
		}
	}
	switch b.Restart {
	case "", RestartAny, RestartOnFailure, RestartNone:
	default:
		sub.addf("restart %q (want any, on-failure or none)", b.Restart)
	}
	for _, v := range []int64{b.Resources.Limits.MilliCPU, b.Resources.Limits.MemoryBytes, b.Resources.Reservations.MilliCPU, b.Resources.Reservations.MemoryBytes} {
		if v < 0 {
			sub.addf("negative resource value")
			break
		}
	}
	*p = append(*p, sub...)
}

// storageDataVolume is the name of the data volume of every storage type.
const storageDataVolume = "data"

// container returns the deployment for a data mount target.
func (b StorageBase) container(dataTarget string) ContainerSpec {
	restart := b.Restart
	if restart == "" {
		restart = RestartAny
	}
	return ContainerSpec{
		Image: b.Image, Replicas: 1, Networks: b.Networks, Resources: b.Resources, Placement: b.Placement, Restart: restart,
		Volumes: []Volume{{Name: storageDataVolume, Target: dataTarget}},
	}
}

// StorageUser is a login of an SFTP or FTP(S) server. Credentials are Swarm
// secrets that the operator created; the spec holds their names only.
type StorageUser struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid,omitempty"`
	// Home is the directory below the data root, for example "/alice".
	Home string `json:"home,omitempty"`
	// PasswordSecret names a Swarm secret with the password.
	PasswordSecret string `json:"passwordSecret,omitempty"`
	// AuthorizedKeysSecret (SFTP only) names a Swarm secret with the public
	// keys in authorized_keys format.
	AuthorizedKeysSecret string `json:"authorizedKeysSecret,omitempty"`
	ReadOnly             bool   `json:"readOnly,omitempty"`
}

func validateUsers(p *problems, users []StorageUser, keysAllowed bool) {
	if len(users) == 0 {
		p.addf("at least one user is required")
	}
	names, uids := map[string]bool{}, map[int]bool{}
	for _, u := range users {
		switch {
		case !userNameRe.MatchString(u.Name):
			p.addf("user name %q must match %s", u.Name, userNameRe)
		case names[u.Name]:
			p.addf("user %q listed twice", u.Name)
		}
		names[u.Name] = true
		switch {
		case u.UID < 1000 || u.UID > 60000:
			p.addf("user %q: uid %d out of range 1000..60000", u.Name, u.UID)
		case uids[u.UID]:
			p.addf("user %q: uid %d is used by another user", u.Name, u.UID)
		}
		uids[u.UID] = true
		if u.GID < 0 || u.GID > 60000 {
			p.addf("user %q: gid %d out of range", u.Name, u.GID)
		}
		if u.Home != "" && (!path.IsAbs(u.Home) || path.Clean(u.Home) != u.Home || u.Home == "/" || strings.ContainsAny(u.Home, "\\%\x00\r\n") || strings.Contains(u.Home, "..")) {
			p.addf("user %q: home %q must be a clean absolute path below the data root", u.Name, u.Home)
		}
		if u.PasswordSecret == "" && u.AuthorizedKeysSecret == "" {
			p.addf("user %q has no credential: set passwordSecret or authorizedKeysSecret", u.Name)
		}
		if u.AuthorizedKeysSecret != "" && !keysAllowed {
			p.addf("user %q: authorizedKeysSecret is not supported by this type", u.Name)
		}
		for _, s := range []string{u.PasswordSecret, u.AuthorizedKeysSecret} {
			if s != "" && !identRe.MatchString(s) {
				p.addf("user %q: secret name %q is invalid", u.Name, s)
			}
		}
		if u.PasswordSecret != "" && u.PasswordSecret == u.AuthorizedKeysSecret {
			p.addf("user %q: password and key use the same secret", u.Name)
		}
	}
}

// userSecrets returns the sorted secret names the users refer to.
func userSecrets(users []StorageUser) []string {
	var out []string
	for _, u := range users {
		for _, s := range []string{u.PasswordSecret, u.AuthorizedKeysSecret} {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// userView is a user as shown in a plan artifact: secret references only.
type userView struct {
	Name           string `json:"name"`
	UID            int    `json:"uid"`
	GID            int    `json:"gid,omitempty"`
	Home           string `json:"home,omitempty"`
	PasswordFile   string `json:"passwordFile,omitempty"`
	AuthorizedKeys string `json:"authorizedKeysFile,omitempty"`
	ReadOnly       bool   `json:"readOnly,omitempty"`
}

func userViews(users []StorageUser) []userView {
	us := slices.Clone(users)
	slices.SortFunc(us, func(a, b StorageUser) int { return strings.Compare(a.Name, b.Name) })
	var out []userView
	for _, u := range us {
		v := userView{Name: u.Name, UID: u.UID, GID: u.GID, Home: u.Home, ReadOnly: u.ReadOnly}
		if u.PasswordSecret != "" {
			v.PasswordFile = path.Join(SecretsDir, u.PasswordSecret)
		}
		if u.AuthorizedKeysSecret != "" {
			v.AuthorizedKeys = path.Join(SecretsDir, u.AuthorizedKeysSecret)
		}
		out = append(out, v)
	}
	return out
}

// jsonArtifact renders v as an indented JSON artifact.
func jsonArtifact(name string, v any) (Artifact, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Name: name, MediaType: "application/json", Content: string(raw) + "\n"}, nil
}

// validPublished checks a published port and its mode.
func validPublishedPort(p *problems, what string, port int, mode string) {
	if port < 0 || port > 65535 {
		p.addf("%s: published port %d out of range", what, port)
	}
	switch mode {
	case "", "ingress", "host":
	default:
		p.addf("%s: port mode %q (want ingress or host)", what, mode)
	}
}

// StorageRoute exposes an HTTP(S) endpoint (S3 API or console) through a
// Traefik instance. Everything is typed: the rule is built from validated
// hosts, so no label text comes from the user.
type StorageRoute struct {
	// Hosts are the host names the route answers to (lower case).
	Hosts []string `json:"hosts"`
	// EntryPoints are entry point names of the Traefik service.
	EntryPoints []string `json:"entryPoints"`
	// CertResolver names a certificate resolver of the Traefik service;
	// empty with TLS true serves Traefik's default certificate.
	CertResolver string `json:"certResolver,omitempty"`
	TLS          bool   `json:"tls,omitempty"`
}

func (r StorageRoute) validate(p *problems, what string) {
	if len(r.Hosts) == 0 {
		p.addf("%s route needs at least one host", what)
	}
	for _, h := range r.Hosts {
		if !hostRe.MatchString(h) {
			p.addf("%s route: host %q is invalid (lower case, no wildcard)", what, h)
		}
	}
	if len(r.EntryPoints) == 0 {
		p.addf("%s route needs at least one entry point", what)
	}
	for _, e := range r.EntryPoints {
		if !entryPointNameRe.MatchString(e) {
			p.addf("%s route: entry point name %q is invalid", what, e)
		}
	}
	if r.CertResolver != "" {
		if !resolverNameRe.MatchString(r.CertResolver) {
			p.addf("%s route: certificate resolver name %q is invalid", what, r.CertResolver)
		}
		if !r.TLS {
			p.addf("%s route: a certificate resolver needs tls", what)
		}
	}
}

// routerName is the name of the Traefik router and service of a route.
func routerName(svc, part string) string {
	if part == "" {
		return svc
	}
	return svc + "-" + part
}

// labels returns the Traefik Swarm-provider labels of the route. They are
// service labels: the Traefik Swarm provider reads them from the service.
func (r StorageRoute) labels(svc, part string, port int, network string) map[string]string {
	n := routerName(svc, part)
	l := map[string]string{
		"traefik.enable":                                           "true",
		"traefik.http.routers." + n + ".rule":                      Match{Hosts: r.Hosts}.rule(),
		"traefik.http.routers." + n + ".entrypoints":               strings.Join(sortedUnique(r.EntryPoints), ","),
		"traefik.http.routers." + n + ".service":                   n,
		"traefik.http.services." + n + ".loadbalancer.server.port": fmt.Sprint(port),
	}
	if r.TLS {
		l["traefik.http.routers."+n+".tls"] = "true"
	}
	if r.CertResolver != "" {
		l["traefik.http.routers."+n+".tls.certresolver"] = r.CertResolver
	}
	if network != "" {
		l["traefik.swarm.network"] = network
	}
	return l
}

// dynamicConfig renders the same route as a file-provider snippet, for a
// Traefik instance that does not use the Swarm provider. The backend URL is
// the service name on a shared network.
func (r StorageRoute) dynamicConfig(svc, part string, port int) string {
	n := routerName(svc, part)
	t := TraefikSpec{
		Routers: []Router{{Name: n, Match: Match{Hosts: r.Hosts}, EntryPoints: r.EntryPoints, Service: n,
			TLS: tlsOf(r)}},
		Services: []Backend{{Name: n, Servers: []string{fmt.Sprintf("http://%s:%d", SwarmServiceName(svc), port)}}},
	}
	return t.RenderDynamic()
}

func tlsOf(r StorageRoute) *RouterTLS {
	if !r.TLS {
		return nil
	}
	return &RouterTLS{CertResolver: r.CertResolver}
}

// mergeLabels adds the labels of src to dst (dst wins on a clash).
func mergeLabels(dst *ServiceSpec, src map[string]string) {
	if dst.Labels == nil {
		dst.Labels = map[string]string{}
	}
	for k, v := range src {
		if _, ok := dst.Labels[k]; !ok {
			dst.Labels[k] = v
		}
	}
}

// storagePlan runs the container plan for a derived container, lets the
// type adjust the Swarm service (ports, labels, env, command) and appends
// its artifacts, warnings and blockers. The adjustment happens before the
// plan so that the spec artifact and the create-service step agree.
func storagePlan(in PlanInput, typ string, c ContainerSpec, hook func(*ServiceSpec)) (Plan, error) {
	if err := checkHost(c, in.Hosts); err != nil {
		return Plan{}, err
	}
	p, err := c.plan(in, typ)
	if err != nil {
		return Plan{}, err
	}
	if hook == nil {
		return p, nil
	}
	for i := range p.Steps {
		if p.Steps[i].Kind == StepCreateService && p.Steps[i].Spec != nil {
			hook(p.Steps[i].Spec)
			p.Steps[i].Note = describeService(*p.Steps[i].Spec)
			raw, err := json.MarshalIndent(p.Steps[i].Spec, "", "  ")
			if err != nil {
				return Plan{}, err
			}
			for j := range p.Artifacts {
				if p.Artifacts[j].Name == "swarm-service.json" {
					p.Artifacts[j].Content = string(raw) + "\n"
				}
			}
		}
	}
	return p, nil
}

// portArtifact lists the published ports of a plan for review.
func portArtifact(s ServiceSpec) (Artifact, error) {
	type row struct {
		Published int    `json:"published"`
		Target    int    `json:"target"`
		Protocol  string `json:"protocol"`
		Mode      string `json:"mode"`
	}
	rows := []row{}
	for _, pt := range s.PublishedPorts() {
		rows = append(rows, row{pt.Published, pt.Target, pt.Protocol, pt.Mode})
	}
	return jsonArtifact("published-ports.json", rows)
}

// validHostOrIP accepts an IP address or a lower-case host name.
func validHostOrIP(s string) bool {
	return net.ParseIP(s) != nil || hostRe.MatchString(s)
}

// storageBackupWarning is added to every storage plan: backup capability is
// not declared yet.
const storageBackupWarning = "backup and restore of this service's data are not wired yet: the data lives on one local volume, a lost host loses it"
