// SPDX-License-Identifier: AGPL-3.0-only

// Package recovery models local user accounts and their credentials (U01),
// single-use recovery codes, short-lived single-use recovery URL tokens,
// attempt limiting with lockout, and opaque session tokens.
//
// Passkeys are the primary credential; a password and recovery codes are
// separate, optional credential kinds, and passkey-only accounts are valid.
// The WebAuthn ceremony itself, persistence/replication and the HTTP layer
// are not implemented here. All stores are in-memory and safe for concurrent
// use; every time-dependent component takes an injected clock.
package recovery

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Clock returns the current time. Tests inject a fake.
type Clock func() time.Time

// CredentialKind distinguishes credential types. Each kind is stored and
// verified separately; none implies another.
type CredentialKind string

const (
	// CredentialPasskey is a WebAuthn credential. Its public-key material is
	// not modelled yet; it will be added with the WebAuthn library choice.
	CredentialPasskey CredentialKind = "passkey"
	// CredentialPassword is the optional password (a recovery option).
	CredentialPassword CredentialKind = "password"
	// CredentialRecoveryCodes marks an enrolled set of recovery codes; the
	// hashed codes themselves live in a CodeSet.
	CredentialRecoveryCodes CredentialKind = "recovery_codes"
)

// Credential is one credential registered on an account. It holds no secret
// material.
type Credential struct {
	ID        string         `json:"id"`
	Kind      CredentialKind `json:"kind"`
	Label     string         `json:"label,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// Account is a local user account.
type Account struct {
	ID          string       `json:"id"`
	Credentials []Credential `json:"credentials"`
	// CredentialEpoch increases with every credential change. Sessions issued
	// under an older epoch are invalid.
	CredentialEpoch uint64 `json:"credentialEpoch"`
	// PendingPasskeyEnrollment is set after a recovery that left the account
	// without a passkey; the user must register one before normal use.
	PendingPasskeyEnrollment bool `json:"pendingPasskeyEnrollment,omitempty"`
}

var (
	// ErrInvalidAccount is returned for an account that violates the model.
	ErrInvalidAccount = errors.New("recovery: invalid account")
	// ErrAccountExists is returned for a duplicate account ID.
	ErrAccountExists = errors.New("recovery: account exists")
	// ErrUnknownAccount is returned for a missing account.
	ErrUnknownAccount = errors.New("recovery: unknown account")
	// ErrUnknownCredential is returned for a missing credential.
	ErrUnknownCredential = errors.New("recovery: unknown credential")
	// ErrLastPasskey is returned when removing the only passkey of an account
	// that is not pending enrollment.
	ErrLastPasskey = errors.New("recovery: cannot remove the last passkey")
)

func (a Account) count(k CredentialKind) int {
	n := 0
	for _, c := range a.Credentials {
		if c.Kind == k {
			n++
		}
	}
	return n
}

// Validate checks the account model: passkeys are primary, so an account
// needs at least one passkey unless it is pending passkey enrollment after a
// recovery; at most one password and one recovery-code set; unique IDs.
func (a Account) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("%w: empty ID", ErrInvalidAccount)
	}
	seen := map[string]bool{}
	for _, c := range a.Credentials {
		if c.ID == "" || seen[c.ID] {
			return fmt.Errorf("%w: missing or duplicate credential ID %q", ErrInvalidAccount, c.ID)
		}
		seen[c.ID] = true
		switch c.Kind {
		case CredentialPasskey, CredentialPassword, CredentialRecoveryCodes:
		default:
			return fmt.Errorf("%w: unknown credential kind %q", ErrInvalidAccount, c.Kind)
		}
	}
	if a.count(CredentialPassword) > 1 {
		return fmt.Errorf("%w: more than one password", ErrInvalidAccount)
	}
	if a.count(CredentialRecoveryCodes) > 1 {
		return fmt.Errorf("%w: more than one recovery code set", ErrInvalidAccount)
	}
	if a.count(CredentialPasskey) == 0 && !a.PendingPasskeyEnrollment {
		return fmt.Errorf("%w: no passkey", ErrInvalidAccount)
	}
	return nil
}

// HasKind reports whether the account has a credential of kind k.
func (a Account) HasKind(k CredentialKind) bool { return a.count(k) > 0 }

func cloneAccount(a Account) Account {
	a.Credentials = slices.Clone(a.Credentials)
	return a
}

// Accounts is an in-memory account registry. It implements EpochSource so
// sessions are invalidated by any credential change.
type Accounts struct {
	mu       sync.RWMutex
	accounts map[string]Account
}

// NewAccounts returns an empty registry.
func NewAccounts() *Accounts { return &Accounts{accounts: make(map[string]Account)} }

// Create adds a valid account.
func (s *Accounts) Create(a Account) error {
	if err := a.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[a.ID]; ok {
		return ErrAccountExists
	}
	s.accounts[a.ID] = cloneAccount(a)
	return nil
}

// Get returns a copy of an account.
func (s *Accounts) Get(id string) (Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return Account{}, ErrUnknownAccount
	}
	return cloneAccount(a), nil
}

// CredentialEpoch implements EpochSource.
func (s *Accounts) CredentialEpoch(id string) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return 0, ErrUnknownAccount
	}
	return a.CredentialEpoch, nil
}

func (s *Accounts) mutate(id string, fn func(*Account) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return ErrUnknownAccount
	}
	next := cloneAccount(a)
	if err := fn(&next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	next.CredentialEpoch = a.CredentialEpoch + 1
	s.accounts[id] = next
	return nil
}

// AddCredential registers a credential. Registering a passkey clears
// PendingPasskeyEnrollment. The credential epoch advances.
func (s *Accounts) AddCredential(id string, c Credential) error {
	return s.mutate(id, func(a *Account) error {
		a.Credentials = append(a.Credentials, c)
		if c.Kind == CredentialPasskey {
			a.PendingPasskeyEnrollment = false
		}
		return nil
	})
}

// RemoveCredential removes a credential. The last passkey cannot be removed
// unless the account is pending enrollment. The credential epoch advances.
func (s *Accounts) RemoveCredential(id, credID string) error {
	return s.mutate(id, func(a *Account) error {
		i := slices.IndexFunc(a.Credentials, func(c Credential) bool { return c.ID == credID })
		if i < 0 {
			return ErrUnknownCredential
		}
		if a.Credentials[i].Kind == CredentialPasskey && a.count(CredentialPasskey) == 1 && !a.PendingPasskeyEnrollment {
			return ErrLastPasskey
		}
		a.Credentials = slices.Delete(a.Credentials, i, i+1)
		return nil
	})
}

// BeginPasskeyReenrollment is called after a successful recovery when the
// user lost their passkeys: it removes all passkeys, marks the account
// pending enrollment and advances the epoch, so every existing session ends.
func (s *Accounts) BeginPasskeyReenrollment(id string) error {
	return s.mutate(id, func(a *Account) error {
		a.Credentials = slices.DeleteFunc(a.Credentials, func(c Credential) bool { return c.Kind == CredentialPasskey })
		a.PendingPasskeyEnrollment = true
		return nil
	})
}

// RecordCredentialChange advances the epoch without changing the credential
// list, for changes such as a password reset or regenerated recovery codes.
func (s *Accounts) RecordCredentialChange(id string) error {
	return s.mutate(id, func(*Account) error { return nil })
}
