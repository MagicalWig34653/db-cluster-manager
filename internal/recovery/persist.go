// SPDX-License-Identifier: AGPL-3.0-only

package recovery

import (
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Persistence hooks. The stores in this package stay in-memory; a caller
// that needs durability (internal/manager) installs a persister, which is
// called synchronously while the store's lock is held, and loads the
// persisted state back with Load. Persisters must not call back into the
// store.
//
// Write-through rules:
//   - Account changes, new and rotated sessions and consumed recovery codes
//     are persisted before they take effect; a persister error rejects the
//     change (fail closed).
//   - Deletions (revoked, expired or pruned sessions; revoked code sets) and
//     code-set enrolment cannot report errors through the existing API. They
//     are persisted best effort and errors go to the persister's error
//     callback. A session whose deletion was lost reappears after a reload
//     but still expires by its own timeouts and credential epoch.
//   - Session activity (LastSeenAt) is persisted at most once per quarter of
//     the idle timeout, so a reload can end a session somewhat earlier than
//     its idle timeout would have.

// AccountPersister stores accounts.
type AccountPersister interface {
	PutAccount(Account) error
}

// SetPersister installs p (nil removes it).
func (s *Accounts) SetPersister(p AccountPersister) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persist = p
}

// List returns copies of all accounts sorted by ID.
func (s *Accounts) List() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, cloneAccount(a))
	}
	slices.SortFunc(out, func(a, b Account) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

// Load replaces the registry contents with accounts (for example read back
// from durable storage). Every account must be valid; the persister is not
// called.
func (s *Accounts) Load(accounts []Account) error {
	next := make(map[string]Account, len(accounts))
	for _, a := range accounts {
		if err := a.Validate(); err != nil {
			return err
		}
		if _, dup := next[a.ID]; dup {
			return fmt.Errorf("%w: duplicate account %q", ErrInvalidAccount, a.ID)
		}
		next[a.ID] = cloneAccount(a)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts = next
	return nil
}

// SessionRecord is a persisted session: the session view plus the hex
// SHA-256 digest of its token. The token itself is never stored.
type SessionRecord struct {
	Digest  string  `json:"digest"`
	Session Session `json:"session"`
}

// SessionPersister stores sessions by token digest.
type SessionPersister interface {
	PutSession(SessionRecord) error
	DeleteSession(digest string) error
}

// SetPersister installs p (nil removes it). onErr receives errors of
// best-effort writes; it may be nil.
func (s *Sessions) SetPersister(p SessionPersister, onErr func(error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persist, s.onErr = p, onErr
}

// Records returns every stored session.
func (s *Sessions) Records() []SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionRecord, 0, len(s.byHash))
	for d, sess := range s.byHash {
		out = append(out, SessionRecord{Digest: hex.EncodeToString(d[:]), Session: *sess})
	}
	slices.SortFunc(out, func(a, b SessionRecord) int {
		switch {
		case a.Digest < b.Digest:
			return -1
		case a.Digest > b.Digest:
			return 1
		}
		return 0
	})
	return out
}

// Load replaces the stored sessions with records. Expired sessions are
// still loaded and end on their next use or Prune.
func (s *Sessions) Load(records []SessionRecord) error {
	next := make(map[[32]byte]*Session, len(records))
	for _, r := range records {
		b, err := hex.DecodeString(r.Digest)
		if err != nil || len(b) != 32 {
			return fmt.Errorf("recovery: invalid session digest %q", r.Digest)
		}
		var d [32]byte
		copy(d[:], b)
		sess := r.Session
		next[d] = &sess
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHash = next
	s.seenPersisted = map[[32]byte]time.Time{}
	for d, sess := range next {
		s.seenPersisted[d] = sess.LastSeenAt
	}
	return nil
}

func (s *Sessions) putLocked(d [32]byte, sess Session) error {
	if s.persist == nil {
		return nil
	}
	if err := s.persist.PutSession(SessionRecord{Digest: hex.EncodeToString(d[:]), Session: sess}); err != nil {
		return fmt.Errorf("recovery: persist session: %w", err)
	}
	s.seenPersisted[d] = sess.LastSeenAt
	return nil
}

func (s *Sessions) deleteLocked(d [32]byte) {
	delete(s.byHash, d)
	delete(s.seenPersisted, d)
	if s.persist == nil {
		return
	}
	if err := s.persist.DeleteSession(hex.EncodeToString(d[:])); err != nil && s.onErr != nil {
		s.onErr(fmt.Errorf("recovery: persist session deletion: %w", err))
	}
}

// touchLocked persists activity when enough time passed since the last
// persisted activity.
func (s *Sessions) touchLocked(d [32]byte, sess Session) {
	if s.persist == nil {
		return
	}
	if sess.LastSeenAt.Sub(s.seenPersisted[d]) < s.policy.IdleTimeout/4 {
		return
	}
	if err := s.putLocked(d, sess); err != nil && s.onErr != nil {
		s.onErr(err)
	}
}

// CodePersister stores recovery-code sets (hashes only) per user.
type CodePersister interface {
	PutCodes(userID string, set CodeSet) error
	DeleteCodes(userID string) error
}

// SetPersister installs p (nil removes it). onErr receives errors of
// best-effort writes (Enroll, Revoke); it may be nil.
func (v *CodeVerifier) SetPersister(p CodePersister, onErr func(error)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.persist, v.onErr = p, onErr
}

func (v *CodeVerifier) persister() (CodePersister, func(error)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.persist, v.onErr
}

// Export returns a copy of every stored code set by user.
func (v *CodeVerifier) Export() map[string]CodeSet {
	v.mu.Lock()
	ids := slices.Collect(maps.Keys(v.entries))
	v.mu.Unlock()
	out := make(map[string]CodeSet, len(ids))
	for _, id := range ids {
		if set, ok := v.Snapshot(id); ok {
			out[id] = set
		}
	}
	return out
}

// Load replaces every stored code set; the persister is not called.
func (v *CodeVerifier) Load(sets map[string]CodeSet) {
	next := make(map[string]*codeEntry, len(sets))
	for id, set := range sets {
		next[id] = &codeEntry{set: CodeSet{CreatedAt: set.CreatedAt, Codes: slices.Clone(set.Codes)}}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.entries = next
}
