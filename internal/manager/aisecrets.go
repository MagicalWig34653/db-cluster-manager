// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
)

// CollAISecrets holds AI provider credentials (X13, proposal), sealed with
// the data keyring. Key: the provider's credential reference.
const CollAISecrets = "ai-secrets"

// SealedSecretStore implements ai.SecretStore with sealed metastore
// documents, so provider API keys are encrypted like other secrets and are
// bound to their key (a value moved to another reference does not
// decrypt). The manager uses it for the vulnerability response (manager.vuln).
type SealedSecretStore struct {
	Store *Store
	Keys  KeyProvider
}

var _ ai.SecretStore = SealedSecretStore{}

type sealedAISecret struct {
	Value string `json:"value"`
}

// Put implements ai.SecretStore.
func (s SealedSecretStore) Put(_ context.Context, ref string, v ai.Secret) error {
	return PutSealed(s.Store, s.Keys, CollAISecrets, ref, sealedAISecret{Value: v.Reveal()})
}

// Get implements ai.SecretStore.
func (s SealedSecretStore) Get(_ context.Context, ref string) (ai.Secret, error) {
	v, ok, err := GetSealed[sealedAISecret](s.Store, s.Keys, CollAISecrets, ref)
	if err != nil {
		return ai.Secret{}, err
	}
	if !ok {
		return ai.Secret{}, ai.ErrSecretNotFound
	}
	return ai.NewSecret(v.Value), nil
}

// Delete implements ai.SecretStore.
func (s SealedSecretStore) Delete(_ context.Context, ref string) error {
	return Delete(s.Store, CollAISecrets, ref, nil)
}
