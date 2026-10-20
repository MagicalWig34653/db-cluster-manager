// SPDX-License-Identifier: AGPL-3.0-only

package recovery

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Session tokens are 256-bit random opaque strings. The store keeps only the
// SHA-256 digest, so a leaked store does not yield usable tokens. Lookup is a
// map access by digest; timing reveals nothing useful because an attacker
// would need a digest preimage.

const sessionTokenBytes = 32

// SessionPolicy configures session lifetimes.
type SessionPolicy struct {
	// IdleTimeout ends a session not used for this long.
	IdleTimeout time.Duration
	// AbsoluteTimeout ends a session this long after its creation, regardless
	// of activity or rotation.
	AbsoluteTimeout time.Duration
}

// DefaultSessionPolicy is a proposed default: 30 minutes idle, 12 hours
// absolute.
var DefaultSessionPolicy = SessionPolicy{IdleTimeout: 30 * time.Minute, AbsoluteTimeout: 12 * time.Hour}

var (
	// ErrSessionInvalid is returned for an unknown, rotated or revoked token.
	ErrSessionInvalid = errors.New("recovery: invalid session")
	// ErrSessionIdle is returned when the idle timeout has passed.
	ErrSessionIdle = errors.New("recovery: session idle timeout")
	// ErrSessionExpired is returned when the absolute timeout has passed.
	ErrSessionExpired = errors.New("recovery: session expired")
	// ErrSessionCredentialsChanged is returned when the account's credentials
	// changed after the session was issued.
	ErrSessionCredentialsChanged = errors.New("recovery: credentials changed since session start")
)

// EpochSource reports an account's current credential epoch.
type EpochSource interface {
	CredentialEpoch(userID string) (uint64, error)
}

// Session is the non-secret view of a session.
type Session struct {
	// ID is a stable, non-secret identifier (hex of a digest prefix) for
	// listing and audit; it cannot be used to authenticate.
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	Epoch      uint64    `json:"epoch"`
}

// Sessions is an in-memory session store.
type Sessions struct {
	mu     sync.Mutex
	policy SessionPolicy
	now    Clock
	epochs EpochSource
	byHash map[[32]byte]*Session
}

// NewSessions returns a store. epochs is required so credential changes end
// sessions. now may be nil for time.Now.
func NewSessions(p SessionPolicy, epochs EpochSource, now Clock) *Sessions {
	if now == nil {
		now = time.Now
	}
	return &Sessions{policy: p, now: now, epochs: epochs, byHash: make(map[[32]byte]*Session)}
}

func newSessionToken() (string, [32]byte, error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", [32]byte{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)
	return tok, digest(tok), nil
}

// Issue starts a session for userID at the account's current epoch.
func (s *Sessions) Issue(userID string) (string, Session, error) {
	epoch, err := s.epochs.CredentialEpoch(userID)
	if err != nil {
		return "", Session{}, err
	}
	tok, d, err := newSessionToken()
	if err != nil {
		return "", Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	sess := &Session{ID: hex.EncodeToString(d[:8]), UserID: userID, CreatedAt: now, LastSeenAt: now, Epoch: epoch}
	s.byHash[d] = sess
	return tok, *sess, nil
}

// lookup validates the token under s.mu and returns its digest and session.
// Invalid sessions are deleted.
func (s *Sessions) lookup(token string) ([32]byte, *Session, error) {
	d := digest(token)
	sess, ok := s.byHash[d]
	if !ok {
		return d, nil, ErrSessionInvalid
	}
	now := s.now()
	if !now.Before(sess.CreatedAt.Add(s.policy.AbsoluteTimeout)) {
		delete(s.byHash, d)
		return d, nil, ErrSessionExpired
	}
	if !now.Before(sess.LastSeenAt.Add(s.policy.IdleTimeout)) {
		delete(s.byHash, d)
		return d, nil, ErrSessionIdle
	}
	epoch, err := s.epochs.CredentialEpoch(sess.UserID)
	if err != nil || epoch != sess.Epoch {
		delete(s.byHash, d)
		return d, nil, ErrSessionCredentialsChanged
	}
	return d, sess, nil
}

// Validate checks a token and records activity.
func (s *Sessions) Validate(token string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, sess, err := s.lookup(token)
	if err != nil {
		return Session{}, err
	}
	sess.LastSeenAt = s.now()
	return *sess, nil
}

// Rotate replaces a valid token with a new one, for example after privilege
// changes or periodically. The old token stops working immediately; the
// absolute expiry is kept from the original session.
func (s *Sessions) Rotate(token string) (string, Session, error) {
	tok, nd, err := newSessionToken()
	if err != nil {
		return "", Session{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, sess, err := s.lookup(token)
	if err != nil {
		return "", Session{}, err
	}
	delete(s.byHash, d)
	next := *sess
	next.ID = hex.EncodeToString(nd[:8])
	next.LastSeenAt = s.now()
	s.byHash[nd] = &next
	return tok, next, nil
}

// Revoke ends one session. Revoking an unknown token is not an error.
func (s *Sessions) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byHash, digest(token))
}

// RevokeUser ends all sessions of a user and returns how many ended.
// Credential changes need not call it (the epoch check ends those sessions)
// but it frees memory and supports "sign out everywhere".
func (s *Sessions) RevokeUser(userID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for d, sess := range s.byHash {
		if sess.UserID == userID {
			delete(s.byHash, d)
			n++
		}
	}
	return n
}

// List returns the user's stored sessions. Sessions that expired since the
// last Validate or Prune may still be included.
func (s *Sessions) List(userID string) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Session
	for _, sess := range s.byHash {
		if sess.UserID == userID {
			out = append(out, *sess)
		}
	}
	return out
}

// Prune removes expired, idle and stale-epoch sessions.
func (s *Sessions) Prune() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for d, sess := range s.byHash {
		expired := !now.Before(sess.CreatedAt.Add(s.policy.AbsoluteTimeout)) ||
			!now.Before(sess.LastSeenAt.Add(s.policy.IdleTimeout))
		epoch, err := s.epochs.CredentialEpoch(sess.UserID)
		if expired || err != nil || epoch != sess.Epoch {
			delete(s.byHash, d)
			n++
		}
	}
	return n
}
