// SPDX-License-Identifier: AGPL-3.0-only

package passkey

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/go-webauthn/webauthn/webauthn"
)

// Persistence hook. The Manager keeps credentials in memory; a caller that
// needs durability installs a CredentialPersister and reloads with Load. The
// persister is called synchronously with the Manager lock held and must not
// call back into the Manager.
//
// Rules: a new credential is persisted before the account store attaches
// it (an orphaned record is harmless, the account store is authoritative);
// a successful login is reported only after its new sign counter was
// persisted (the counter is the clone-detection state); clone warnings and
// removals are persisted best effort, errors go to the error callback.

// StoredCredential is the persisted form of one passkey: its public key
// record, its non-secret metadata and the WebAuthn user handle of its
// account.
type StoredCredential struct {
	Info       CredentialInfo      `json:"info"`
	UserHandle []byte              `json:"userHandle"`
	Record     webauthn.Credential `json:"record"`
}

// CredentialPersister stores passkey credentials by credential ID.
type CredentialPersister interface {
	PutCredential(StoredCredential) error
	DeleteCredential(id string) error
}

// SetPersister installs p (nil removes it). onErr receives errors of
// best-effort writes; it may be nil.
func (m *Manager) SetPersister(p CredentialPersister, onErr func(error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persist, m.onErr = p, onErr
}

func (m *Manager) storedLocked(sc *storedCredential) StoredCredential {
	return StoredCredential{Info: sc.info, UserHandle: slices.Clone(m.handles[sc.info.AccountID]), Record: sc.record}
}

func (m *Manager) putLocked(sc *storedCredential) error {
	if m.persist == nil {
		return nil
	}
	if err := m.persist.PutCredential(m.storedLocked(sc)); err != nil {
		return fmt.Errorf("passkey: persist credential: %w", err)
	}
	return nil
}

func (m *Manager) bestEffort(err error) {
	if err != nil && m.onErr != nil {
		m.onErr(err)
	}
}

func (m *Manager) deleteLocked(id string) {
	delete(m.creds, id)
	if m.persist != nil {
		if err := m.persist.DeleteCredential(id); err != nil {
			m.bestEffort(fmt.Errorf("passkey: persist credential removal: %w", err))
		}
	}
}

// Export returns every held credential sorted by ID.
func (m *Manager) Export() []StoredCredential {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StoredCredential, 0, len(m.creds))
	for _, sc := range m.creds {
		out = append(out, m.storedLocked(sc))
	}
	slices.SortFunc(out, func(a, b StoredCredential) int {
		switch {
		case a.Info.ID < b.Info.ID:
			return -1
		case a.Info.ID > b.Info.ID:
			return 1
		}
		return 0
	})
	return out
}

// Load replaces the held credentials and user handles. Every record needs an
// ID, an account and a 32-byte user handle that no other account uses.
// Pending ceremonies are kept.
func (m *Manager) Load(creds []StoredCredential) error {
	handles := map[string][]byte{}
	byHandle := map[string]string{}
	next := map[string]*storedCredential{}
	for _, c := range creds {
		if c.Info.ID == "" || c.Info.AccountID == "" || len(c.UserHandle) != 32 {
			return fmt.Errorf("passkey: invalid stored credential %q", c.Info.ID)
		}
		if encodeID(c.Record.ID) != c.Info.ID {
			return fmt.Errorf("passkey: stored credential %q has a mismatching record ID", c.Info.ID)
		}
		if _, dup := next[c.Info.ID]; dup {
			return fmt.Errorf("passkey: duplicate stored credential %q", c.Info.ID)
		}
		if h, ok := handles[c.Info.AccountID]; ok && !bytes.Equal(h, c.UserHandle) {
			return fmt.Errorf("passkey: account %q has two user handles", c.Info.AccountID)
		}
		if a, ok := byHandle[string(c.UserHandle)]; ok && a != c.Info.AccountID {
			return fmt.Errorf("passkey: user handle shared by %q and %q", a, c.Info.AccountID)
		}
		handles[c.Info.AccountID] = slices.Clone(c.UserHandle)
		byHandle[string(c.UserHandle)] = c.Info.AccountID
		next[c.Info.ID] = &storedCredential{info: c.Info, record: c.Record}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handles, m.byHandle, m.creds = handles, byHandle, next
	return nil
}
