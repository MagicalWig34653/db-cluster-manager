// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"

	"github.com/MagicalWig34653/db-cluster-manager/internal/managertls"
)

// CollACME contains only sealed account, certificate and challenge records.
const CollACME = "manager-acme"

// ACMEStorage reads on all replicas. A writing instance must be bound to one
// Leadership.Valid, never a general IsLeader check that could cross terms.
type ACMEStorage struct {
	Store    *Store
	Keys     KeyProvider
	CanWrite func() bool
}

func (s ACMEStorage) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, ok, err := GetSealed[[]byte](s.Store, s.Keys, CollACME, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, managertls.ErrMissing
	}
	return v, nil
}
func (s ACMEStorage) writable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.CanWrite == nil || !s.CanWrite() {
		return ErrNotLeader
	}
	return nil
}
func (s ACMEStorage) Put(ctx context.Context, key string, value []byte) error {
	if err := s.writable(ctx); err != nil {
		return err
	}
	return PutSealed(s.Store, s.Keys, CollACME, key, value)
}
func (s ACMEStorage) Delete(ctx context.Context, key string) error {
	if err := s.writable(ctx); err != nil {
		return err
	}
	err := Delete(s.Store, CollACME, key, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
