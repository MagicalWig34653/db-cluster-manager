// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// Secret wraps a registry credential so it cannot leak through formatting,
// JSON or structured logging by accident. It mirrors dbadmin.Secret.
type Secret struct{ v string }

// NewSecret wraps v.
func NewSecret(v string) Secret { return Secret{v: v} }

// Reveal returns the plaintext. Call it only where the value is consumed.
func (s Secret) Reveal() string { return s.v }

// IsZero reports whether no value is set.
func (s Secret) IsZero() bool { return s.v == "" }

const redacted = "[redacted]"

func (s Secret) String() string               { return redacted }
func (s Secret) GoString() string             { return redacted }
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (s Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }

// ErrSecretNotFound is returned by a SecretStore for an unknown reference.
var ErrSecretNotFound = errors.New("registry: secret not found")

// SecretStore is the manager's encrypted secret storage as seen by this
// package. Configurations hold only the reference, never the value. The
// production implementation (sealed by the data keyring) is wired by the
// manager; MemorySecretStore is a fake for tests.
type SecretStore interface {
	Put(ctx context.Context, ref string, v Secret) error
	Get(ctx context.Context, ref string) (Secret, error)
	Delete(ctx context.Context, ref string) error
}

// MemorySecretStore is an in-memory SecretStore for tests (a fake).
type MemorySecretStore struct {
	mu sync.Mutex
	m  map[string]Secret
}

// Put implements SecretStore.
func (s *MemorySecretStore) Put(_ context.Context, ref string, v Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Secret{}
	}
	s.m[ref] = v
	return nil
}

// Get implements SecretStore.
func (s *MemorySecretStore) Get(_ context.Context, ref string) (Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[ref]
	if !ok {
		return Secret{}, ErrSecretNotFound
	}
	return v, nil
}

// Delete implements SecretStore.
func (s *MemorySecretStore) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, ref)
	return nil
}
