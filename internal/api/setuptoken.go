// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"sync"
)

// SetupToken protects the first-run setup endpoints. The manager prints the
// plaintext once at start; whoever can read the manager's console or log is
// trusted to create the first administrator. Only a SHA-256 digest is kept.
//
// Verify hashes the presented value and compares the fixed-length digests
// with crypto/subtle, so the comparison time depends neither on the position
// of the first wrong byte nor on the presented length. After Invalidate (the
// first administrator exists) every value is rejected.
type SetupToken struct {
	mu     sync.RWMutex
	digest [sha256.Size]byte
	active bool
}

// NewSetupToken creates a token with 256 bits from crypto/rand. It returns
// the token and its URL-safe plaintext, to be shown exactly once.
func NewSetupToken() (*SetupToken, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	plain := base64.RawURLEncoding.EncodeToString(b)
	return &SetupToken{digest: sha256.Sum256([]byte(plain)), active: true}, plain, nil
}

// Verify reports whether s is the active token.
func (t *SetupToken) Verify(s string) bool {
	if t == nil {
		return false
	}
	d := sha256.Sum256([]byte(s))
	t.mu.RLock()
	defer t.mu.RUnlock()
	ok := subtle.ConstantTimeCompare(d[:], t.digest[:]) == 1
	return ok && t.active
}

// Active reports whether the token can still be used.
func (t *SetupToken) Active() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.active
}

// Invalidate permanently disables the token.
func (t *SetupToken) Invalidate() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active = false
	t.digest = [sha256.Size]byte{}
}
