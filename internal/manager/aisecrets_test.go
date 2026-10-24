// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
)

func TestSealedSecretStoreForAIProviders(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	st := SealedSecretStore{Store: s.Store(), Keys: testKey}
	ctx := context.Background()
	const key = "sk-ant-sealed-provider-key-0123456789"
	if err := st.Put(ctx, "vuln/ai/claude/key", ai.NewSecret(key)); err != nil {
		t.Fatal(err)
	}
	raw, ok, _ := s.Store().Raw(CollAISecrets, "vuln/ai/claude/key")
	if !ok || bytes.Contains(raw.Value, []byte(key)) {
		t.Fatal("key stored in clear")
	}
	got, err := st.Get(ctx, "vuln/ai/claude/key")
	if err != nil || got.Reveal() != key {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "missing"); !errors.Is(err, ai.ErrSecretNotFound) {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "vuln/ai/claude/key"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "vuln/ai/claude/key"); !errors.Is(err, ai.ErrSecretNotFound) {
		t.Fatal(err)
	}
}
