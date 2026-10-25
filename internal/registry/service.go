// SPDX-License-Identifier: AGPL-3.0-only

package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Config is the stored, non-secret description of a registry. The credential
// is referenced, never embedded.
type Config struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     Kind   `json:"kind"`
	Endpoint string `json:"endpoint"`
	Username string `json:"username,omitempty"`
	// CredentialRef names the SecretStore entry of the password or token.
	CredentialRef string    `json:"credentialRef,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	CreatedBy     string    `json:"createdBy,omitempty"`
}

// ErrExists is returned when a registry ID is already used.
var ErrExists = errors.New("registry: id already exists")

// ConfigStore persists registry configurations.
type ConfigStore interface {
	Put(ctx context.Context, c Config) error // ErrExists if the ID is taken
	Get(ctx context.Context, id string) (Config, error)
	List(ctx context.Context) ([]Config, error)
}

// MemoryConfigStore is an in-memory ConfigStore for tests (a fake).
type MemoryConfigStore struct {
	mu sync.Mutex
	m  map[string]Config
}

// Put implements ConfigStore.
func (s *MemoryConfigStore) Put(_ context.Context, c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Config{}
	}
	if _, ok := s.m[c.ID]; ok {
		return ErrExists
	}
	s.m[c.ID] = c
	return nil
}

// Get implements ConfigStore.
func (s *MemoryConfigStore) Get(_ context.Context, id string) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[id]
	if !ok {
		return Config{}, ErrNotFound
	}
	return c, nil
}

// List implements ConfigStore, sorted by ID.
func (s *MemoryConfigStore) List(context.Context) ([]Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Config, 0, len(s.m))
	for _, c := range s.m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ExternalInput registers an external registry. Password is consumed once and
// stored through the SecretStore.
type ExternalInput struct {
	ID       string
	Name     string
	Kind     Kind
	Endpoint string
	Username string
	Password Secret
}

// Pinger is implemented by clients that can check reachability and
// credentials.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Service manages registry configurations and hands out clients.
type Service struct {
	Configs ConfigStore
	Secrets SecretStore
	Now     func() time.Time
	// NewClient builds a client; nil selects NewExternal. Tests inject
	// clients bound to a fake server.
	NewClient func(ExternalConfig) (Registry, error)
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) client(cfg ExternalConfig) (Registry, error) {
	if s.NewClient != nil {
		return s.NewClient(cfg)
	}
	return NewExternal(cfg)
}

// AddExternal validates the registration, checks the credentials by
// contacting the registry when the client supports it, and only then stores
// the secret and the configuration.
func (s *Service) AddExternal(ctx context.Context, actor string, in ExternalInput) (Config, error) {
	if !idRe.MatchString(in.ID) || in.Name == "" || len(in.Name) > 100 {
		return Config{}, fmt.Errorf("%w: id or name malformed", ErrInvalidConfig)
	}
	ep := in.Endpoint
	if ep == "" {
		ep = DefaultEndpoint(in.Kind)
	}
	ecfg := ExternalConfig{Kind: in.Kind, Endpoint: ep, Username: in.Username, Password: in.Password}
	cl, err := s.client(ecfg)
	if err != nil {
		return Config{}, err
	}
	if in.Password.IsZero() != (in.Username == "") {
		return Config{}, fmt.Errorf("%w: username and password go together", ErrInvalidConfig)
	}
	if _, err := s.Configs.Get(ctx, in.ID); err == nil {
		return Config{}, ErrExists
	}
	if p, ok := cl.(Pinger); ok {
		if err := p.Ping(ctx); err != nil {
			return Config{}, err
		}
	}
	cfg := Config{ID: in.ID, Name: in.Name, Kind: in.Kind, Endpoint: ep, Username: in.Username,
		CreatedAt: s.now().UTC(), CreatedBy: actor}
	if !in.Password.IsZero() {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return Config{}, err
		}
		cfg.CredentialRef = "registry/" + in.ID + "/credential/" + hex.EncodeToString(nonce[:])
		if err := s.Secrets.Put(ctx, cfg.CredentialRef, in.Password); err != nil {
			return Config{}, err
		}
	}
	if err := s.Configs.Put(ctx, cfg); err != nil {
		if cfg.CredentialRef != "" {
			_ = s.Secrets.Delete(ctx, cfg.CredentialRef)
		}
		return Config{}, err
	}
	return cfg, nil
}

// List returns all registry configurations.
func (s *Service) List(ctx context.Context) ([]Config, error) { return s.Configs.List(ctx) }

// Credential returns the stored credential of a registry for a consumer
// such as the builder. The caller must not log it.
func (s *Service) Credential(ctx context.Context, id string) (Config, Secret, error) {
	cfg, err := s.Configs.Get(ctx, id)
	if err != nil {
		return Config{}, Secret{}, err
	}
	if cfg.CredentialRef == "" {
		return cfg, Secret{}, nil
	}
	sec, err := s.Secrets.Get(ctx, cfg.CredentialRef)
	return cfg, sec, err
}

// Open returns a client for a configured external registry.
func (s *Service) Open(ctx context.Context, id string) (Registry, Config, error) {
	cfg, sec, err := s.Credential(ctx, id)
	if err != nil {
		return nil, Config{}, err
	}
	if cfg.Kind == KindManaged {
		return nil, cfg, fmt.Errorf("%w: managed registries are not reachable until the service framework deploys them", ErrInvalidConfig)
	}
	cl, err := s.client(ExternalConfig{Kind: cfg.Kind, Endpoint: cfg.Endpoint, Username: cfg.Username, Password: sec})
	return cl, cfg, err
}
