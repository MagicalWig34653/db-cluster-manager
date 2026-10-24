package kube

import (
	"errors"
	"net/url"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// Registration is the public metadata of an existing cluster. It never
// contains a credential or kubeconfig, including in error responses.
type Registration struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Endpoint       string `json:"endpoint"`
	Namespace      string `json:"namespace"`
	CredentialKind string `json:"credentialKind"`
	CredentialSet  bool   `json:"credentialSet"`
}

// RegistrationInput consumes exactly one opaque credential. Kubeconfigs
// are not executed, parsed or connected to during groundwork.
type RegistrationInput struct {
	ID         string
	Name       string
	Endpoint   string
	Namespace  string
	Token      registry.Secret
	Kubeconfig registry.Secret
}

func (in RegistrationInput) Validate() error {
	u, e := url.Parse(in.Endpoint)
	if !validName(in.ID) || in.Name == "" || len(in.Name) > 100 || !validName(in.Namespace) || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || in.Token.IsZero() == in.Kubeconfig.IsZero() {
		return errors.New("kube: invalid registration; provide an HTTPS origin and exactly one credential")
	}
	if len(in.Token.Reveal()) > 32<<10 || len(in.Kubeconfig.Reveal()) > 32<<10 {
		return errors.New("kube: credential too large")
	}
	return nil
}
func (in RegistrationInput) View() Registration {
	kind := "token"
	if !in.Kubeconfig.IsZero() {
		kind = "kubeconfig"
	}
	return Registration{ID: in.ID, Name: in.Name, Endpoint: in.Endpoint, Namespace: in.Namespace, CredentialKind: kind, CredentialSet: true}
}
