// SPDX-License-Identifier: AGPL-3.0-only

package passkey

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

var (
	// ErrUnknownCeremony is returned for a ceremony ID that was never issued,
	// was already finished (challenges are single-use), or belongs to the
	// other ceremony kind.
	ErrUnknownCeremony = errors.New("passkey: unknown or already used ceremony")
	// ErrCeremonyExpired is returned when the ceremony's TTL has passed.
	ErrCeremonyExpired = errors.New("passkey: ceremony expired")
	// ErrBindingMismatch is returned when a ceremony is finished from a
	// different session than the one that began it.
	ErrBindingMismatch = errors.New("passkey: ceremony belongs to another session")
	// ErrTooManyCeremonies is returned when the pending-ceremony limit is hit.
	ErrTooManyCeremonies = errors.New("passkey: too many pending ceremonies")
)

type ceremonyKind int

const (
	kindRegistration ceremonyKind = iota + 1
	kindLogin
)

// pending is the server-side state of one unfinished ceremony. The
// library's SessionData (challenge, user handle, UV requirement) never
// leaves the server.
type pending struct {
	kind       ceremonyKind
	binding    string
	accountID  string // empty for a discoverable login
	newAccount bool
	label      string
	handle     []byte
	session    webauthn.SessionData
	expires    time.Time
}

// challengeStore holds pending ceremonies keyed by an opaque random ID.
// Entries are removed on the first take, whatever the outcome.
type challengeStore struct {
	mu      sync.Mutex
	now     Clock
	ttl     time.Duration
	max     int
	entries map[string]*pending
}

func newChallengeStore(now Clock, ttl time.Duration, max int) *challengeStore {
	return &challengeStore{now: now, ttl: ttl, max: max, entries: make(map[string]*pending)}
}

func randomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// put stores p and returns its ceremony ID and expiry.
func (s *challengeStore) put(p *pending) (string, time.Time, error) {
	id, err := randomID()
	if err != nil {
		return "", time.Time{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.entries) >= s.max {
		s.pruneLocked(now)
		if len(s.entries) >= s.max {
			return "", time.Time{}, ErrTooManyCeremonies
		}
	}
	p.expires = now.Add(s.ttl)
	s.entries[id] = p
	return id, p.expires, nil
}

// take removes and returns the ceremony. It is consumed even when the kind,
// binding or expiry check fails, so a challenge can never be tried twice.
func (s *challengeStore) take(id string, kind ceremonyKind, binding string) (*pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.entries[id]
	if !ok {
		return nil, ErrUnknownCeremony
	}
	delete(s.entries, id)
	if p.kind != kind {
		return nil, ErrUnknownCeremony
	}
	if !s.now().Before(p.expires) {
		return nil, ErrCeremonyExpired
	}
	if subtle.ConstantTimeCompare([]byte(p.binding), []byte(binding)) != 1 {
		return nil, ErrBindingMismatch
	}
	return p, nil
}

func (s *challengeStore) pruneLocked(now time.Time) int {
	n := 0
	for id, p := range s.entries {
		if !now.Before(p.expires) {
			delete(s.entries, id)
			n++
		}
	}
	return n
}

// prune removes expired ceremonies and returns how many were removed.
func (s *challengeStore) prune() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(s.now())
}

func (s *challengeStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
