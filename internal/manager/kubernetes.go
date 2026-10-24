package manager

import (
	"context"
	"errors"

	"github.com/MagicalWig34653/db-cluster-manager/internal/kube"
)

const CollKubernetes = "kubernetes-registrations"

// KubernetesRegistrations is opt-in groundwork. Credentials and metadata
// are sealed atomically in the existing replicated document path. No API
// client, kubeconfig parser, provisioning or cluster probe is constructed.
type KubernetesRegistrations struct {
	Enabled bool
	Store   *Store
	Keys    KeyProvider
}
type sealedKubernetesRegistration struct {
	Registration kube.Registration `json:"registration"`
	Token        string            `json:"token,omitempty"`
	Kubeconfig   string            `json:"kubeconfig,omitempty"`
}

func (s KubernetesRegistrations) Register(_ context.Context, in kube.RegistrationInput) (kube.Registration, error) {
	if !s.Enabled {
		return kube.Registration{}, kube.ErrDisabled
	}
	if err := in.Validate(); err != nil {
		return kube.Registration{}, err
	}
	if s.Store == nil || s.Keys == nil {
		return kube.Registration{}, errors.New("kube: sealed store unavailable")
	}
	view := in.View()
	v, err := seal(s.Keys, CollKubernetes, in.ID, sealedKubernetesRegistration{Registration: view, Token: in.Token.Reveal(), Kubeconfig: in.Kubeconfig.Reveal()})
	if err != nil {
		return kube.Registration{}, err
	}
	zero := uint64(0)
	_, err = Put(s.Store, CollKubernetes, in.ID, v, &zero)
	if errors.Is(err, ErrConflict) {
		return kube.Registration{}, kube.ErrConflict
	}
	if err != nil {
		return kube.Registration{}, err
	}
	return view, nil
}
func (s KubernetesRegistrations) Registrations(context.Context) ([]kube.Registration, error) {
	if !s.Enabled {
		return nil, kube.ErrDisabled
	}
	if s.Store == nil || s.Keys == nil {
		return nil, errors.New("kube: sealed store unavailable")
	}
	docs, err := ListSealed[sealedKubernetesRegistration](s.Store, s.Keys, CollKubernetes)
	if err != nil {
		return nil, err
	}
	out := make([]kube.Registration, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Value.Registration)
	}
	return out, nil
}
