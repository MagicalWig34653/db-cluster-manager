package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/kube"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

func TestKubernetesRegistrationSealedAtomicAndCredentialFree(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	ctx := context.Background()
	store := KubernetesRegistrations{Enabled: true, Store: s.Store(), Keys: testKey}
	in := kube.RegistrationInput{ID: "lab", Name: "Lab", Endpoint: "https://kube.example.test", Namespace: "sandbox", Token: registry.NewSecret("synthetic-secret-token")}
	view, err := store.Register(ctx, in)
	if err != nil || !view.CredentialSet {
		t.Fatal(view, err)
	}
	raw, ok, err := s.Store().Raw(CollKubernetes, "lab")
	if err != nil || !ok || bytes.Contains(raw.Value, []byte("synthetic-secret-token")) {
		t.Fatal("credential stored in clear", err)
	}
	saved, ok, err := GetSealed[sealedKubernetesRegistration](s.Store(), testKey, CollKubernetes, "lab")
	if err != nil || !ok || saved.Token != "synthetic-secret-token" {
		t.Fatal("sealed roundtrip", err)
	}
	if _, err := store.Register(ctx, in); !errors.Is(err, kube.ErrConflict) {
		t.Fatal(err)
	}
	list, err := store.Registrations(ctx)
	if err != nil || len(list) != 1 || list[0] != view {
		t.Fatal(list, err)
	}
	moved := raw.Value
	// Copying ciphertext to another key must fail the existing AAD binding.
	var envelope sealed
	if err := json.Unmarshal(moved, &envelope); err != nil {
		t.Fatal(err)
	}
	var other sealedKubernetesRegistration
	if err := unseal(testKey, CollKubernetes, "other", envelope, &other); err == nil {
		t.Fatal("credential can move between registrations")
	}
	store.Enabled = false
	if _, err := store.Register(ctx, in); !errors.Is(err, kube.ErrDisabled) {
		t.Fatal(err)
	}
	if _, err := store.Registrations(ctx); !errors.Is(err, kube.ErrDisabled) {
		t.Fatal(err)
	}
}
func TestKubernetesConcurrentRegistrationDoesNotOverwriteCredential(t *testing.T) {
	c := newCluster(t, 1, nil)
	store := KubernetesRegistrations{Enabled: true, Store: c.leader().Store(), Keys: testKey}
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, value := range []string{"one", "two"} {
		wg.Go(func() {
			_, err := store.Register(ctx, kube.RegistrationInput{ID: "lab", Name: "Lab", Endpoint: "https://cluster.test", Namespace: "sandbox", Kubeconfig: registry.NewSecret(value)})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, kube.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal(success, conflict)
	}
}
