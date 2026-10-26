// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// TypeTraefik is the registry name of the Traefik type (X07).
const TypeTraefik = "traefik"

// Traefik container paths and defaults.
const (
	// TraefikACMEDir is where the ACME storage volume is mounted.
	TraefikACMEDir = "/acme"
	// TraefikDynamicFile is the path of the file provider's configuration.
	TraefikDynamicFile = "/etc/traefik/dynamic.yml"
	// LetsEncryptStaging is the staging directory URL used by Staging.
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

	traefikACMEVolume = "acme"
)

var (
	hostRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	pathRe     = regexp.MustCompile(`^/[A-Za-z0-9._~/%-]*$`)
	headerRe   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	providerRe = regexp.MustCompile(`^[a-z0-9]{1,40}$`)
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	// sensitiveHeaderRe matches header names whose value is a credential;
	// such a value would end up in the rendered configuration.
	sensitiveHeaderRe = regexp.MustCompile(`(?i)^(authorization|proxy-authorization|cookie|set-cookie)$|(?i)(token|secret|password|api-?key)`)
)

// safeRoutePath rejects ambiguous paths before they reach Traefik's matcher
// or health checker. Encoded separators and dot segments can be decoded by
// different layers in different orders.
func safeRoutePath(s string) bool {
	if !pathRe.MatchString(s) || strings.Contains(s, "%") {
		return false
	}
	// A single trailing slash is meaningful for a PathPrefix matcher.
	cleanInput := strings.TrimSuffix(s, "/")
	if cleanInput == "" {
		return s == "/"
	}
	if path.Clean(cleanInput) != cleanInput {
		return false
	}
	for _, segment := range strings.Split(s, "/") {
		if segment == ".." || segment == "." {
			return false
		}
	}
	return true
}

// EntryPoint is a Traefik listener. The Swarm publishes Published (default:
// Port) to it.
type EntryPoint struct {
	Name string `json:"name"`
	// Port is the port Traefik listens on inside the container.
	Port      int `json:"port"`
	Published int `json:"published,omitempty"`
	// RedirectTo names another entry point; requests are redirected to it
	// with scheme https (permanent).
	RedirectTo string `json:"redirectTo,omitempty"`
}

// Challenge is an ACME challenge. Credentials of DNS providers are Swarm
// secrets: EnvFiles maps a provider environment variable to the secret that
// holds the value; the service gets "<VAR>_FILE=/run/secrets/<secret>".
type Challenge struct {
	// Type is "http", "tls" or "dns".
	Type string `json:"type"`
	// EntryPoint (http only) is the entry point that serves port 80.
	EntryPoint  string            `json:"entryPoint,omitempty"`
	DNSProvider string            `json:"dnsProvider,omitempty"`
	EnvFiles    map[string]string `json:"envFiles,omitempty"`
}

// CertResolver is an ACME certificate resolver.
type CertResolver struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	// Staging selects the Let's Encrypt staging CA; CAServer selects another
	// CA directory URL. Both empty: the Traefik default (production
	// Let's Encrypt).
	Staging   bool      `json:"staging,omitempty"`
	CAServer  string    `json:"caServer,omitempty"`
	Challenge Challenge `json:"challenge"`
}

// Match is the typed request matcher of a router; it is rendered into
// Traefik's rule syntax, so no free-form rule text exists.
type Match struct {
	Hosts      []string `json:"hosts,omitempty"`
	Path       string   `json:"path,omitempty"`
	PathPrefix string   `json:"pathPrefix,omitempty"`
}

// RouterTLS enables TLS on a router.
type RouterTLS struct {
	// CertResolver names a resolver of the spec; empty serves the default
	// certificate (or certificates Traefik already has).
	CertResolver string `json:"certResolver,omitempty"`
}

// Router routes matching requests of entry points to a backend.
type Router struct {
	Name        string     `json:"name"`
	Match       Match      `json:"match"`
	EntryPoints []string   `json:"entryPoints"`
	Service     string     `json:"service"`
	Middlewares []string   `json:"middlewares,omitempty"`
	TLS         *RouterTLS `json:"tls,omitempty"`
	Priority    int        `json:"priority,omitempty"`
}

// Backend is a load-balanced set of servers (a Traefik "service").
type Backend struct {
	Name string `json:"name"`
	// Servers are http(s) URLs without credentials, for example
	// "http://web:8080" (a service name on a shared network).
	Servers        []string `json:"servers"`
	PassHostHeader *bool    `json:"passHostHeader,omitempty"`
	// HealthPath enables an active health check of every server.
	HealthPath            string `json:"healthPath,omitempty"`
	HealthIntervalSeconds int    `json:"healthIntervalSeconds,omitempty"`
}

// Middleware types.
const (
	MWRedirectScheme = "redirectScheme"
	MWStripPrefix    = "stripPrefix"
	MWHeaders        = "headers"
	MWRateLimit      = "rateLimit"
	MWBasicAuth      = "basicAuth"
	MWIPAllowList    = "ipAllowList"
	MWCompress       = "compress"
)

// RedirectScheme redirects to a scheme.
type RedirectScheme struct {
	Scheme    string `json:"scheme"`
	Permanent bool   `json:"permanent,omitempty"`
}

// HeadersMW sets request and response headers. Header values are literal
// and appear in the rendered configuration, so credentials are refused.
type HeadersMW struct {
	Request    map[string]string `json:"request,omitempty"`
	Response   map[string]string `json:"response,omitempty"`
	STSSeconds int               `json:"stsSeconds,omitempty"`
}

// RateLimit limits requests per second.
type RateLimit struct {
	Average int `json:"average"`
	Burst   int `json:"burst,omitempty"`
}

// Middleware is a typed Traefik middleware: Type selects which one field is
// set.
type Middleware struct {
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	RedirectScheme *RedirectScheme `json:"redirectScheme,omitempty"`
	StripPrefix    []string        `json:"stripPrefix,omitempty"`
	Headers        *HeadersMW      `json:"headers,omitempty"`
	RateLimit      *RateLimit      `json:"rateLimit,omitempty"`
	// BasicAuthUsersSecret names a Swarm secret holding the users file
	// (htpasswd format). The file is read from /run/secrets; no user or
	// hash is part of the spec.
	BasicAuthUsersSecret string `json:"basicAuthUsersSecret,omitempty"`
	// IPAllowList are CIDR ranges.
	IPAllowList []string `json:"ipAllowList,omitempty"`
}

// TraefikSpec is the desired state of a Traefik service: its deployment and
// its static and dynamic configuration as a typed model.
type TraefikSpec struct {
	// Image must be pinned, for example "traefik:v3.1.2".
	Image string `json:"image"`
	// Placement pins the service. With certificate resolvers it must name a
	// host, because the ACME storage is a local volume; Traefik OSS cannot
	// share ACME storage, so the service always runs one replica.
	Placement Placement `json:"placement"`
	// Networks are the networks shared with the backends.
	Networks  []string  `json:"networks,omitempty"`
	Resources Resources `json:"resources"`
	// LogLevel is DEBUG, INFO (default), WARN or ERROR.
	LogLevel    string         `json:"logLevel,omitempty"`
	EntryPoints []EntryPoint   `json:"entryPoints"`
	Resolvers   []CertResolver `json:"certResolvers,omitempty"`
	Routers     []Router       `json:"routers,omitempty"`
	Services    []Backend      `json:"services,omitempty"`
	Middlewares []Middleware   `json:"middlewares,omitempty"`
}

// Traefik is the Traefik service type.
type Traefik struct{}

var _ Type = Traefik{}

// Name implements Type.
func (Traefik) Name() string { return TypeTraefik }

func decodeTraefik(raw json.RawMessage) (TraefikSpec, error) {
	var t TraefikSpec
	if err := decodeStrict(raw, &t); err != nil {
		return TraefikSpec{}, err
	}
	return t, t.validate()
}

// Validate implements Type.
func (Traefik) Validate(spec json.RawMessage) error {
	_, err := decodeTraefik(spec)
	return err
}

func (t TraefikSpec) published(e EntryPoint) int {
	if e.Published != 0 {
		return e.Published
	}
	return e.Port
}

func (t TraefikSpec) validate() error {
	var p problems
	// The deployment part is validated as the container it becomes.
	if err := t.container().validate(); err != nil {
		p.addf("%s", strings.TrimPrefix(err.Error(), ErrInvalidSpec.Error()+": "))
	}
	switch t.LogLevel {
	case "", "DEBUG", "INFO", "WARN", "ERROR":
	default:
		p.addf("logLevel %q (want DEBUG, INFO, WARN or ERROR)", t.LogLevel)
	}
	eps := map[string]EntryPoint{}
	if len(t.EntryPoints) == 0 {
		p.addf("at least one entry point is required")
	}
	for _, e := range t.EntryPoints {
		switch {
		case !nameRe.MatchString(e.Name):
			p.addf("entry point name %q must match %s", e.Name, nameRe)
		case eps[e.Name].Name != "":
			p.addf("entry point %q listed twice", e.Name)
		}
		if e.Port < 1 || e.Port > 65535 || e.Published < 0 || e.Published > 65535 {
			p.addf("entry point %q: port out of range", e.Name)
		}
		eps[e.Name] = e
	}
	for _, e := range t.EntryPoints {
		if e.RedirectTo != "" {
			if _, ok := eps[e.RedirectTo]; !ok || e.RedirectTo == e.Name {
				p.addf("entry point %q redirects to %q, which is not another entry point", e.Name, e.RedirectTo)
			}
		}
	}
	resolvers := t.validateResolvers(&p, eps)
	backends := map[string]bool{}
	for _, b := range t.Services {
		t.validateBackend(&p, b, backends)
	}
	mws := map[string]bool{}
	for _, m := range t.Middlewares {
		t.validateMiddleware(&p, m, mws)
	}
	routers := map[string]bool{}
	for _, r := range t.Routers {
		t.validateRouter(&p, r, routers, eps, backends, mws, resolvers)
	}
	return p.err()
}

func (t TraefikSpec) validateResolvers(p *problems, eps map[string]EntryPoint) map[string]bool {
	out := map[string]bool{}
	for _, r := range t.Resolvers {
		switch {
		case !nameRe.MatchString(r.Name):
			p.addf("resolver name %q must match %s", r.Name, nameRe)
		case out[r.Name]:
			p.addf("resolver %q listed twice", r.Name)
		}
		out[r.Name] = true
		if !emailRe.MatchString(r.Email) {
			p.addf("resolver %q: email %q is invalid", r.Name, r.Email)
		}
		if r.Staging && r.CAServer != "" {
			p.addf("resolver %q: staging and caServer exclude each other", r.Name)
		}
		if r.CAServer != "" {
			if u, err := url.Parse(r.CAServer); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				p.addf("resolver %q: caServer must be an https URL without credentials", r.Name)
			}
		}
		c := r.Challenge
		switch c.Type {
		case "http":
			if _, ok := eps[c.EntryPoint]; !ok {
				p.addf("resolver %q: http challenge needs an existing entryPoint", r.Name)
			}
			if len(c.EnvFiles) > 0 || c.DNSProvider != "" {
				p.addf("resolver %q: dnsProvider and envFiles belong to the dns challenge", r.Name)
			}
		case "tls":
			if c.EntryPoint != "" || len(c.EnvFiles) > 0 || c.DNSProvider != "" {
				p.addf("resolver %q: the tls challenge takes no further settings", r.Name)
			}
		case "dns":
			if !providerRe.MatchString(c.DNSProvider) {
				p.addf("resolver %q: dnsProvider %q is invalid", r.Name, c.DNSProvider)
			}
			for k, s := range c.EnvFiles {
				if !envNameRe.MatchString(k) || strings.HasSuffix(k, "_FILE") || !identRe.MatchString(s) {
					p.addf("resolver %q: envFiles %q -> %q is invalid (variable without _FILE, secret name)", r.Name, k, s)
				}
			}
		default:
			p.addf("resolver %q: challenge type %q (want http, tls or dns)", r.Name, c.Type)
		}
	}
	return out
}

func (t TraefikSpec) validateBackend(p *problems, b Backend, seen map[string]bool) {
	switch {
	case !nameRe.MatchString(b.Name):
		p.addf("service name %q must match %s", b.Name, nameRe)
	case seen[b.Name]:
		p.addf("service %q listed twice", b.Name)
	}
	seen[b.Name] = true
	if len(b.Servers) == 0 {
		p.addf("service %q has no servers", b.Name)
	}
	for _, s := range b.Servers {
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			p.addf("service %q: server %q must be an http(s) URL with host and optional port only", b.Name, s)
			continue
		}
		if port := u.Port(); port != "" {
			if _, err := net.LookupPort("tcp", port); err != nil {
				p.addf("service %q: server %q has an invalid port", b.Name, s)
			}
		}
	}
	if b.HealthPath != "" && !safeRoutePath(b.HealthPath) {
		p.addf("service %q: healthPath %q is invalid", b.Name, b.HealthPath)
	}
	if b.HealthIntervalSeconds < 0 || b.HealthIntervalSeconds > 3600 || (b.HealthIntervalSeconds > 0 && b.HealthPath == "") {
		p.addf("service %q: healthIntervalSeconds needs a healthPath and must be 0..3600", b.Name)
	}
}

func (t TraefikSpec) validateMiddleware(p *problems, m Middleware, seen map[string]bool) {
	switch {
	case !nameRe.MatchString(m.Name):
		p.addf("middleware name %q must match %s", m.Name, nameRe)
	case seen[m.Name]:
		p.addf("middleware %q listed twice", m.Name)
	}
	seen[m.Name] = true
	set := 0
	for _, b := range []bool{m.RedirectScheme != nil, len(m.StripPrefix) > 0, m.Headers != nil, m.RateLimit != nil,
		m.BasicAuthUsersSecret != "", len(m.IPAllowList) > 0} {
		if b {
			set++
		}
	}
	want := func(ok bool, what string) {
		if !ok || set != 1 {
			p.addf("middleware %q of type %s needs exactly its own settings (%s)", m.Name, m.Type, what)
		}
	}
	switch m.Type {
	case MWRedirectScheme:
		want(m.RedirectScheme != nil, "redirectScheme")
		if m.RedirectScheme != nil && m.RedirectScheme.Scheme != "https" && m.RedirectScheme.Scheme != "http" {
			p.addf("middleware %q: scheme must be http or https", m.Name)
		}
	case MWStripPrefix:
		want(len(m.StripPrefix) > 0, "stripPrefix")
		for _, x := range m.StripPrefix {
			if !safeRoutePath(x) || x == "/" {
				p.addf("middleware %q: prefix %q is invalid", m.Name, x)
			}
		}
	case MWHeaders:
		want(m.Headers != nil, "headers")
		if m.Headers != nil {
			for _, hm := range []map[string]string{m.Headers.Request, m.Headers.Response} {
				for k, v := range hm {
					if !headerRe.MatchString(k) || sensitiveHeaderRe.MatchString(k) || strings.ContainsAny(v, "\x00\r\n") {
						p.addf("middleware %q: header %q is invalid or would carry a credential", m.Name, k)
					}
				}
			}
			if m.Headers.STSSeconds < 0 {
				p.addf("middleware %q: stsSeconds is negative", m.Name)
			}
		}
	case MWRateLimit:
		want(m.RateLimit != nil, "rateLimit")
		if m.RateLimit != nil && (m.RateLimit.Average < 1 || m.RateLimit.Burst < 0) {
			p.addf("middleware %q: average must be at least 1 and burst not negative", m.Name)
		}
	case MWBasicAuth:
		want(m.BasicAuthUsersSecret != "", "basicAuthUsersSecret")
		if m.BasicAuthUsersSecret != "" && !identRe.MatchString(m.BasicAuthUsersSecret) {
			p.addf("middleware %q: secret name %q is invalid", m.Name, m.BasicAuthUsersSecret)
		}
	case MWIPAllowList:
		want(len(m.IPAllowList) > 0, "ipAllowList")
		for _, c := range m.IPAllowList {
			if _, _, err := net.ParseCIDR(c); err != nil {
				p.addf("middleware %q: %q is not a CIDR range", m.Name, c)
			}
		}
	case MWCompress:
		if set != 0 {
			p.addf("middleware %q of type compress takes no settings", m.Name)
		}
	default:
		p.addf("middleware %q: type %q is not supported", m.Name, m.Type)
	}
}

func (t TraefikSpec) validateRouter(p *problems, r Router, seen map[string]bool, eps map[string]EntryPoint,
	backends, mws, resolvers map[string]bool) {
	switch {
	case !nameRe.MatchString(r.Name):
		p.addf("router name %q must match %s", r.Name, nameRe)
	case seen[r.Name]:
		p.addf("router %q listed twice", r.Name)
	}
	seen[r.Name] = true
	m := r.Match
	if len(m.Hosts) == 0 && m.Path == "" && m.PathPrefix == "" {
		p.addf("router %q matches everything: give a host or a path", r.Name)
	}
	for _, h := range m.Hosts {
		if !hostRe.MatchString(h) {
			p.addf("router %q: host %q is invalid (lower case, no wildcard)", r.Name, h)
		}
	}
	for _, x := range []string{m.Path, m.PathPrefix} {
		if x != "" && !safeRoutePath(x) {
			p.addf("router %q: path %q is invalid", r.Name, x)
		}
	}
	if m.Path != "" && m.PathPrefix != "" {
		p.addf("router %q: path and pathPrefix exclude each other", r.Name)
	}
	if len(r.EntryPoints) == 0 {
		p.addf("router %q names no entry point", r.Name)
	}
	for _, e := range r.EntryPoints {
		if _, ok := eps[e]; !ok {
			p.addf("router %q: unknown entry point %q", r.Name, e)
		}
	}
	if !backends[r.Service] {
		p.addf("router %q: unknown service %q", r.Name, r.Service)
	}
	for _, mw := range r.Middlewares {
		if !mws[mw] {
			p.addf("router %q: unknown middleware %q", r.Name, mw)
		}
	}
	if r.TLS != nil && r.TLS.CertResolver != "" && !resolvers[r.TLS.CertResolver] {
		p.addf("router %q: unknown certificate resolver %q", r.Name, r.TLS.CertResolver)
	}
	if r.Priority < 0 || r.Priority > 1_000_000 {
		p.addf("router %q: priority out of range", r.Name)
	}
}

// hasDynamic reports whether there is dynamic configuration to deliver.
func (t TraefikSpec) hasDynamic() bool {
	return len(t.Routers)+len(t.Services)+len(t.Middlewares) > 0
}

// container derives the deployment: the Swarm service Traefik runs as. The
// static configuration is passed as command-line flags, which Traefik treats
// like the static file; the dynamic configuration is a file (see Plan).
func (t TraefikSpec) container() ContainerSpec {
	c := ContainerSpec{
		Image: t.Image, Replicas: 1, Networks: t.Networks, Resources: t.Resources, Placement: t.Placement,
		Restart: RestartAny, Args: t.staticArgs(),
	}
	for _, e := range t.EntryPoints {
		c.Ports = append(c.Ports, Port{Target: e.Port, Published: t.published(e), Protocol: "tcp", Mode: "ingress"})
	}
	secrets := map[string]bool{}
	for _, r := range t.Resolvers {
		for _, k := range sortedKeys(r.Challenge.EnvFiles) {
			s := r.Challenge.EnvFiles[k]
			c.Env = append(c.Env, EnvVar{Name: k + "_FILE", Value: path.Join(SecretsDir, s)})
			secrets[s] = true
		}
	}
	for _, m := range t.Middlewares {
		if m.BasicAuthUsersSecret != "" {
			secrets[m.BasicAuthUsersSecret] = true
		}
	}
	c.Secrets = sortedUnique(mapKeys(secrets))
	if len(t.Resolvers) > 0 {
		c.Volumes = []Volume{{Name: traefikACMEVolume, Target: TraefikACMEDir}}
	}
	return c
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Plan implements Type.
func (Traefik) Plan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	t, err := decodeTraefik(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	p, err := t.container().plan(in, TypeTraefik)
	if err != nil {
		return Plan{}, err
	}
	p.Artifacts = append(p.Artifacts, Artifact{Name: "traefik-static.yml", MediaType: "application/yaml", Content: t.RenderStatic()})
	if t.hasDynamic() {
		p.Artifacts = append(p.Artifacts, Artifact{Name: "traefik-dynamic.yml", MediaType: "application/yaml", Content: t.RenderDynamic()})
		deliverConfig(&p, in.Context, "traefik-dynamic.yml", TraefikDynamicFile)
	}
	mgmt, err := t.managementArtifact(in.Name)
	if err != nil {
		return Plan{}, err
	}
	p.Artifacts = append(p.Artifacts, mgmt)
	if len(t.Resolvers) > 0 {
		p.Warnings = append(p.Warnings, "ACME certificates and their private keys are stored on the local volume "+VolumeName(in.Name, traefikACMEVolume)+
			" (host "+t.Placement.HostID+"); Traefik OSS cannot share them, so the service runs one replica and a lost host loses the certificates (they can be re-issued)")
	}
	for _, r := range t.Resolvers {
		if r.Challenge.Type == "http" {
			if e := t.entryPoint(r.Challenge.EntryPoint); t.published(e) != 80 {
				p.Warnings = append(p.Warnings, fmt.Sprintf("resolver %s: the HTTP challenge is only reachable when the entry point %s is published on port 80", r.Name, e.Name))
			}
		}
		if r.Challenge.Type == "tls" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("resolver %s: the TLS challenge needs port 443 to reach Traefik", r.Name))
		}
		if !r.Staging && r.CAServer == "" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("resolver %s uses the production CA; failed issuance attempts count against its rate limits, test with staging first", r.Name))
		}
	}
	return p, nil
}

func (t TraefikSpec) entryPoint(name string) EntryPoint {
	for _, e := range t.EntryPoints {
		if e.Name == name {
			return e
		}
	}
	return EntryPoint{}
}

// DeletePlan implements Type.
func (Traefik) DeletePlan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	t, err := decodeTraefik(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	p := t.container().deletePlan(in, TypeTraefik)
	if t.hasDynamic() {
		removeManagedConfig(&p, in.Context, "traefik-dynamic.yml")
	}
	return p, nil
}

// Health implements Type.
func (Traefik) Health(in HealthInput) (Health, error) {
	if _, err := decodeTraefik(in.Spec); err != nil {
		return Health{}, err
	}
	return observationHealth(in.Observed), nil
}

// managementArtifact is the metadata a UI needs to manage this Traefik
// instance without parsing the rendered files: what the instance exposes,
// which objects the manager owns, where the dynamic file lives and that the
// dashboard and the insecure API are off.
func (t TraefikSpec) managementArtifact(name string) (Artifact, error) {
	type ep struct {
		Name       string `json:"name"`
		Port       int    `json:"port"`
		Published  int    `json:"published"`
		RedirectTo string `json:"redirectTo,omitempty"`
	}
	type obj struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind,omitempty"`
		Hosts   []string `json:"hosts,omitempty"`
		Service string   `json:"service,omitempty"`
	}
	m := struct {
		Service       string   `json:"service"`
		Dashboard     bool     `json:"dashboard"`
		InsecureAPI   bool     `json:"insecureApi"`
		DynamicFile   string   `json:"dynamicFile,omitempty"`
		EntryPoints   []ep     `json:"entryPoints"`
		Routers       []obj    `json:"routers"`
		Backends      []obj    `json:"services"`
		Middlewares   []obj    `json:"middlewares"`
		CertResolvers []string `json:"certResolvers"`
	}{
		Service:     SwarmServiceName(name),
		EntryPoints: []ep{}, Routers: []obj{}, Backends: []obj{}, Middlewares: []obj{}, CertResolvers: []string{},
	}
	if t.hasDynamic() {
		m.DynamicFile = TraefikDynamicFile
	}
	for _, e := range sortedBy(t.EntryPoints, func(e EntryPoint) string { return e.Name }) {
		m.EntryPoints = append(m.EntryPoints, ep{Name: e.Name, Port: e.Port, Published: t.published(e), RedirectTo: e.RedirectTo})
	}
	for _, r := range sortedBy(t.Routers, func(r Router) string { return r.Name }) {
		m.Routers = append(m.Routers, obj{Name: r.Name, Hosts: sortedUnique(r.Match.Hosts), Service: r.Service})
	}
	for _, b := range sortedBy(t.Services, func(b Backend) string { return b.Name }) {
		m.Backends = append(m.Backends, obj{Name: b.Name})
	}
	for _, mw := range sortedBy(t.Middlewares, func(x Middleware) string { return x.Name }) {
		m.Middlewares = append(m.Middlewares, obj{Name: mw.Name, Kind: mw.Type})
	}
	for _, r := range sortedBy(t.Resolvers, func(r CertResolver) string { return r.Name }) {
		m.CertResolvers = append(m.CertResolvers, r.Name)
	}
	return jsonArtifact("traefik-management.json", m)
}
