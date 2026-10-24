// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"sort"

	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// Collections of the image build and registry state (replicated through the
// metastore like all other manager state).
const (
	collRegistries      = "registries"
	collRegistrySecrets = "registry-secrets"
	collImageJobs       = "image-jobs"
	collImagePlans      = "image-plans"
)

// maxStoredPlans bounds the number of kept plans; the oldest are removed
// when a new one is stored.
const maxStoredPlans = 200

// RegistryConfigStore keeps registry configurations (no secrets: only the
// reference of the credential) in the replicated store.
type RegistryConfigStore struct{ Store *Store }

var _ registry.ConfigStore = RegistryConfigStore{}

// Put implements registry.ConfigStore; an existing ID is registry.ErrExists.
func (s RegistryConfigStore) Put(_ context.Context, c registry.Config) error {
	_, err := Put(s.Store, collRegistries, c.ID, c, IfVersion(0))
	if errors.Is(err, ErrConflict) {
		return registry.ErrExists
	}
	return err
}

// Get implements registry.ConfigStore.
func (s RegistryConfigStore) Get(_ context.Context, id string) (registry.Config, error) {
	d, ok, err := Get[registry.Config](s.Store, collRegistries, id, Consistent())
	if err != nil {
		return registry.Config{}, err
	}
	if !ok {
		return registry.Config{}, registry.ErrNotFound
	}
	return d.Value, nil
}

// List implements registry.ConfigStore.
func (s RegistryConfigStore) List(context.Context) ([]registry.Config, error) {
	ds, err := List[registry.Config](s.Store, collRegistries, Consistent())
	if err != nil {
		return nil, err
	}
	out := make([]registry.Config, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Value)
	}
	return out, nil
}

// RegistrySecretStore keeps registry credentials sealed with the data key
// (the same path as session digests and passkey records), so neither the
// Raft log nor backups of the data directory hold them in clear.
type RegistrySecretStore struct {
	Store *Store
	Keys  KeyProvider
}

var _ registry.SecretStore = RegistrySecretStore{}

type storedSecret struct {
	Value string `json:"value"`
}

// Put implements registry.SecretStore.
func (s RegistrySecretStore) Put(_ context.Context, ref string, v registry.Secret) error {
	return PutSealed(s.Store, s.Keys, collRegistrySecrets, ref, storedSecret{Value: v.Reveal()})
}

// Get implements registry.SecretStore.
func (s RegistrySecretStore) Get(_ context.Context, ref string) (registry.Secret, error) {
	v, ok, err := GetSealed[storedSecret](s.Store, s.Keys, collRegistrySecrets, ref, Consistent())
	if err != nil {
		return registry.Secret{}, err
	}
	if !ok {
		return registry.Secret{}, registry.ErrSecretNotFound
	}
	return registry.NewSecret(v.Value), nil
}

// Delete implements registry.SecretStore.
func (s RegistrySecretStore) Delete(_ context.Context, ref string) error {
	return Delete(s.Store, collRegistrySecrets, ref, nil)
}

// ImageJobStore keeps image build jobs. The job's log is already redacted
// by the build service before it is stored.
type ImageJobStore struct{ Store *Store }

var _ imagebuild.JobStore = ImageJobStore{}

// Put implements imagebuild.JobStore.
func (s ImageJobStore) Put(_ context.Context, j imagebuild.Job) error {
	_, err := Put(s.Store, collImageJobs, j.ID, j, nil)
	return err
}

// Get implements imagebuild.JobStore.
func (s ImageJobStore) Get(_ context.Context, id string) (imagebuild.Job, error) {
	d, ok, err := Get[imagebuild.Job](s.Store, collImageJobs, id, Consistent())
	if err != nil {
		return imagebuild.Job{}, err
	}
	if !ok {
		return imagebuild.Job{}, imagebuild.ErrJobNotFound
	}
	return d.Value, nil
}

// List implements imagebuild.JobStore, newest first.
func (s ImageJobStore) List(context.Context) ([]imagebuild.Job, error) {
	ds, err := List[imagebuild.Job](s.Store, collImageJobs, Consistent())
	if err != nil {
		return nil, err
	}
	out := make([]imagebuild.Job, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Value)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// ImagePlanStore keeps previewed build plans by digest.
type ImagePlanStore struct{ Store *Store }

var _ imagebuild.PlanStore = ImagePlanStore{}

// Put implements imagebuild.PlanStore and prunes the oldest plans beyond
// maxStoredPlans.
func (s ImagePlanStore) Put(_ context.Context, p imagebuild.StoredPlan) error {
	if _, err := Put(s.Store, collImagePlans, p.Plan.Digest, p, nil); err != nil {
		return err
	}
	ds, err := List[imagebuild.StoredPlan](s.Store, collImagePlans)
	if err != nil || len(ds) <= maxStoredPlans {
		return nil
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].Value.At.Before(ds[j].Value.At) })
	var ops []metastore.DocOp
	for _, d := range ds[:len(ds)-maxStoredPlans] {
		ops = append(ops, metastore.DocOp{Collection: collImagePlans, Key: d.Key, Delete: true})
	}
	_, err = s.Store.Txn(ops...)
	return err
}

// ImageServices wires the registry and image build services onto the
// replicated stores. Registry credentials are sealed with keys. The builder
// is supplied by the caller (DockerBuilder in production, a fake in tests);
// async makes approved builds run in the background.
func ImageServices(st *Store, keys KeyProvider, b imagebuild.Builder, async bool, maxLogBytes int) (*imagebuild.Service, *registry.Service) {
	regs := &registry.Service{Configs: RegistryConfigStore{Store: st}, Secrets: RegistrySecretStore{Store: st, Keys: keys}}
	builds := &imagebuild.Service{
		Builder: b, Jobs: ImageJobStore{Store: st}, Plans: ImagePlanStore{Store: st}, Registries: regs,
		Async: async, MaxLogBytes: maxLogBytes,
	}
	return builds, regs
}

// Get implements imagebuild.PlanStore.
func (s ImagePlanStore) Get(_ context.Context, digest string) (imagebuild.StoredPlan, error) {
	d, ok, err := Get[imagebuild.StoredPlan](s.Store, collImagePlans, digest, Consistent())
	if err != nil {
		return imagebuild.StoredPlan{}, err
	}
	if !ok {
		return imagebuild.StoredPlan{}, imagebuild.ErrPlanNotFound
	}
	return d.Value, nil
}
