// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// AuthMode selects how the managed registry authenticates clients.
type AuthMode string

const (
	AuthHtpasswd AuthMode = "htpasswd"
	AuthToken    AuthMode = "token"
)

// ManagedSpec is the desired state of a manager-operated registry. It is a
// specification only: the future service framework (W07) deploys it. A
// single replica with local storage is the only supported shape; a highly
// available registry (shared storage, several replicas) is out of scope.
type ManagedSpec struct {
	Name string `json:"name"`
	// Image must be the standard registry image pinned by digest, for
	// example docker.io/library/registry:2@sha256:...
	Image string   `json:"image"`
	Auth  AuthMode `json:"auth"`
	// HtpasswdSecret is the manager secret holding the htpasswd file
	// (AuthHtpasswd). Only the reference is stored.
	HtpasswdSecret string `json:"htpasswdSecret,omitempty"`
	// Token configures AuthToken.
	Token *TokenAuth `json:"token,omitempty"`
	// TLS is required; there is no plain-http option for a managed
	// registry.
	TLS TLSSpec `json:"tls"`
	// Storage pins the registry to one host with a local volume, following
	// the local-storage identity rule (A10).
	Storage StorageSpec `json:"storage"`
	// PublishedPort is the Swarm ingress or host port, default 5000.
	PublishedPort int `json:"publishedPort,omitempty"`
	// DeleteEnabled turns on manifest deletion (needed for Delete and for
	// garbage collection).
	DeleteEnabled bool `json:"deleteEnabled,omitempty"`
}

// TokenAuth configures registry token authentication.
type TokenAuth struct {
	Realm   string `json:"realm"`
	Service string `json:"service"`
	Issuer  string `json:"issuer"`
	// RootCertBundleSecret references the certificate bundle that verifies
	// token signatures.
	RootCertBundleSecret string `json:"rootCertBundleSecret"`
}

// TLSSpec references the certificate and key secrets.
type TLSSpec struct {
	CertSecret string `json:"certSecret"`
	KeySecret  string `json:"keySecret"`
}

// StorageSpec is the local storage of the registry.
type StorageSpec struct {
	Volume string `json:"volume"`
	// Host is the node (hostname) the registry is pinned to.
	Host string `json:"host"`
	// SizeGiB is informational capacity planning, not enforced by the
	// registry.
	SizeGiB int `json:"sizeGiB,omitempty"`
}

// ServiceDescriptor is the generic deploy description of a service,
// produced for the future service framework. It carries references to
// secrets, never values.
type ServiceDescriptor struct {
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Replicas    int               `json:"replicas"`
	Env         map[string]string `json:"env"`
	Secrets     []SecretMount     `json:"secrets"`
	Volumes     []VolumeMount     `json:"volumes"`
	Ports       []PortMapping     `json:"ports"`
	Constraints []string          `json:"constraints"`
	// Notes explain choices and limits for the plan preview.
	Notes []string `json:"notes"`
}

// SecretMount mounts a manager secret as /run/secrets/<Target>.
type SecretMount struct {
	Secret string `json:"secret"`
	Target string `json:"target"`
}

// VolumeMount is a local named volume.
type VolumeMount struct {
	Name   string `json:"name"`
	Target string `json:"target"`
	Driver string `json:"driver"`
}

// PortMapping publishes a container port.
type PortMapping struct {
	Published int    `json:"published"`
	Target    int    `json:"target"`
	Protocol  string `json:"protocol"`
}

var (
	managedImageRe = regexp.MustCompile(`^(docker\.io/library/|)registry:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)
	managedNameRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}[a-z0-9]$`)
	refRe          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	hostNameRe     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

// Validate checks the spec.
func (m ManagedSpec) Validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(f, a...)) }
	switch {
	case !managedNameRe.MatchString(m.Name):
		return bad("name must be lower-case letters, digits and '-'")
	case !managedImageRe.MatchString(m.Image):
		return bad("image must be the standard registry image pinned by digest (registry:<tag>@sha256:...)")
	case m.TLS.CertSecret == "" || m.TLS.KeySecret == "" || !refRe.MatchString(m.TLS.CertSecret) || !refRe.MatchString(m.TLS.KeySecret):
		return bad("TLS certificate and key secrets are required")
	case !refRe.MatchString(m.Storage.Volume) || !hostNameRe.MatchString(m.Storage.Host):
		return bad("storage needs a volume name and the host it is pinned to")
	case m.PublishedPort < 0 || m.PublishedPort > 65535:
		return bad("published port out of range")
	}
	switch m.Auth {
	case AuthHtpasswd:
		if !refRe.MatchString(m.HtpasswdSecret) {
			return bad("htpasswd auth needs an htpasswd secret reference")
		}
		if m.Token != nil {
			return bad("token settings given with htpasswd auth")
		}
	case AuthToken:
		t := m.Token
		if t == nil || t.Realm == "" || t.Service == "" || t.Issuer == "" || !refRe.MatchString(t.RootCertBundleSecret) {
			return bad("token auth needs realm, service, issuer and a root certificate bundle secret")
		}
		if _, err := ValidateEndpointURL(t.Realm); err != nil {
			return bad("token realm must be an https URL")
		}
		if m.HtpasswdSecret != "" {
			return bad("htpasswd secret given with token auth")
		}
	default:
		return bad("auth must be htpasswd or token; anonymous access is not offered")
	}
	return nil
}

// ValidateEndpointURL accepts an https URL with a host (used for token
// realms; loopback http is not accepted here).
func ValidateEndpointURL(raw string) (string, error) {
	u, err := parseURL(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", ErrInvalidConfig
	}
	return u.String(), nil
}

// Descriptor produces the deploy description of the registry. It is pure and
// deterministic: the same spec gives the same descriptor.
func (m ManagedSpec) Descriptor() (ServiceDescriptor, error) {
	if err := m.Validate(); err != nil {
		return ServiceDescriptor{}, err
	}
	port := m.PublishedPort
	if port == 0 {
		port = 5000
	}
	env := map[string]string{
		"REGISTRY_HTTP_ADDR":                        "0.0.0.0:5000",
		"REGISTRY_HTTP_TLS_CERTIFICATE":             "/run/secrets/registry_tls_cert",
		"REGISTRY_HTTP_TLS_KEY":                     "/run/secrets/registry_tls_key",
		"REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY": "/var/lib/registry",
		"REGISTRY_STORAGE_DELETE_ENABLED":           strconv.FormatBool(m.DeleteEnabled),
	}
	secrets := []SecretMount{
		{Secret: m.TLS.CertSecret, Target: "registry_tls_cert"},
		{Secret: m.TLS.KeySecret, Target: "registry_tls_key"},
	}
	switch m.Auth {
	case AuthHtpasswd:
		env["REGISTRY_AUTH"] = "htpasswd"
		env["REGISTRY_AUTH_HTPASSWD_REALM"] = "DB Cluster Manager registry"
		env["REGISTRY_AUTH_HTPASSWD_PATH"] = "/run/secrets/registry_htpasswd"
		secrets = append(secrets, SecretMount{Secret: m.HtpasswdSecret, Target: "registry_htpasswd"})
	case AuthToken:
		env["REGISTRY_AUTH"] = "token"
		env["REGISTRY_AUTH_TOKEN_REALM"] = m.Token.Realm
		env["REGISTRY_AUTH_TOKEN_SERVICE"] = m.Token.Service
		env["REGISTRY_AUTH_TOKEN_ISSUER"] = m.Token.Issuer
		env["REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE"] = "/run/secrets/registry_token_roots"
		secrets = append(secrets, SecretMount{Secret: m.Token.RootCertBundleSecret, Target: "registry_token_roots"})
	}
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Target < secrets[j].Target })
	return ServiceDescriptor{
		Type: "registry", Name: m.Name, Image: m.Image, Replicas: 1, Env: env, Secrets: secrets,
		Volumes:     []VolumeMount{{Name: m.Storage.Volume, Target: "/var/lib/registry", Driver: "local"}},
		Ports:       []PortMapping{{Published: port, Target: 5000, Protocol: "tcp"}},
		Constraints: []string{"node.hostname == " + m.Storage.Host},
		Notes: []string{
			"One replica with a local volume pinned to " + m.Storage.Host + "; if that host is lost, images in this registry are lost unless they can be rebuilt or exist elsewhere.",
			"TLS is mandatory; clients on every Swarm must trust the certificate (offline use needs the CA distributed to the hosts).",
			"Deployment needs the service framework (not implemented); this descriptor is a specification only.",
		},
	}, nil
}
