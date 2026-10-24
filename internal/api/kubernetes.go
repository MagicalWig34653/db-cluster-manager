package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/kube"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// KubernetesRegistrations is a metadata-only contract, with no deployment
// or provisioning routes. The feature flag defaults to false.
type KubernetesRegistrations interface {
	Register(context.Context, kube.RegistrationInput) (kube.Registration, error)
	Registrations(context.Context) ([]kube.Registration, error)
}
type KubernetesRegistrationInput struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Endpoint   string `json:"endpoint"`
	Namespace  string `json:"namespace"`
	Token      string `json:"token,omitempty"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
}

func (s *server) resolveKubernetes(*http.Request) (authz.Resource, any, error) {
	return authz.Resource{Type: authz.ResourceKubernetes, Scope: authz.Global}, nil, nil
}
func (s *server) kubernetesEndpoints() []endpoint {
	return []endpoint{
		{method: "GET", path: "/kubernetes-clusters", access: accessPermission, action: authz.ActionRead, resolve: s.resolveKubernetes, handle: s.handleKubernetesList},
		{method: "POST", path: "/kubernetes-clusters", access: accessPermission, action: authz.ActionCreate, resolve: s.resolveKubernetes, handle: s.handleKubernetesRegister},
	}
}
func (s *server) handleKubernetesList(w http.ResponseWriter, r *http.Request, _ *caller) {
	if !s.cfg.ExperimentalKubernetes || s.cfg.Kubernetes == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	out, err := s.cfg.Kubernetes.Registrations(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *server) handleKubernetesRegister(w http.ResponseWriter, r *http.Request, _ *caller) {
	if !s.cfg.ExperimentalKubernetes || s.cfg.Kubernetes == nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	var in KubernetesRegistrationInput
	if !decodeJSON(w, r, &in, false) {
		return
	}
	req := kube.RegistrationInput{ID: in.ID, Name: in.Name, Endpoint: in.Endpoint, Namespace: in.Namespace, Token: registry.NewSecret(in.Token), Kubeconfig: registry.NewSecret(in.Kubeconfig)}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	out, err := s.cfg.Kubernetes.Register(r.Context(), req)
	if errors.Is(err, kube.ErrConflict) {
		writeError(w, http.StatusConflict, errConflict)
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
