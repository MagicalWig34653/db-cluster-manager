// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// KeySource provides the manager's data keys by ID. The manager keyring and
// manager.KeyProvider implementations satisfy it.
type KeySource interface {
	// Current returns the ID and bytes of the key used for new objects.
	Current() (string, []byte, error)
	// Key returns the key with the given ID.
	Key(id string) ([]byte, error)
}

// StaticKeys is a KeySource over fixed keys (tests, imported transfer keys).
type StaticKeys struct {
	CurrentID string
	Keys      map[string][]byte
}

// Current implements KeySource.
func (s StaticKeys) Current() (string, []byte, error) {
	b, err := s.Key(s.CurrentID)
	return s.CurrentID, b, err
}

// Key implements KeySource.
func (s StaticKeys) Key(id string) ([]byte, error) {
	if b, ok := s.Keys[id]; ok && len(b) >= 32 {
		return b, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownKey, id)
}

// Errors of signing and client-side encryption.
var (
	// ErrUnknownKey: the key ID is not known to the KeySource.
	ErrUnknownKey = errors.New("backup: unknown key")
	// ErrBadSignature: the HMAC does not verify (tampered, wrong key or
	// wrong purpose).
	ErrBadSignature = errors.New("backup: signature does not verify")
)

// SignedFormat is the version tag of Signed documents.
const SignedFormat = "dbcm-signed/1"

// Signed is an authenticated (not encrypted) document: the payload plus an
// HMAC-SHA256 over format, purpose, key ID and payload. The MAC key is
// derived from the data key with HKDF, separately for each purpose, so a
// signature made for one purpose never verifies for another.
type Signed struct {
	Format  string `json:"format"`
	Purpose string `json:"purpose"`
	KeyID   string `json:"key_id"`
	Payload []byte `json:"payload"`
	MAC     string `json:"mac"`
}

func signingKey(dataKey []byte, purpose string) ([]byte, error) {
	if len(dataKey) < 32 {
		return nil, fmt.Errorf("%w: key shorter than 32 bytes", ErrUnknownKey)
	}
	return hkdf.Key(sha256.New, dataKey, nil, "dbcm/sign/"+purpose, 32)
}

func signMAC(mk []byte, purpose, keyID string, payload []byte) []byte {
	h := hmac.New(sha256.New, mk)
	for _, f := range [][]byte{[]byte(SignedFormat), []byte(purpose), []byte(keyID), payload} {
		fmt.Fprintf(h, "%d:", len(f))
		h.Write(f)
	}
	return h.Sum(nil)
}

// Sign signs payload with the current key of keys.
func Sign(keys KeySource, purpose string, payload []byte) (Signed, error) {
	id, k, err := keys.Current()
	if err != nil {
		return Signed{}, err
	}
	mk, err := signingKey(k, purpose)
	if err != nil {
		return Signed{}, err
	}
	return Signed{Format: SignedFormat, Purpose: purpose, KeyID: id, Payload: append([]byte(nil), payload...),
		MAC: hex.EncodeToString(signMAC(mk, purpose, id, payload))}, nil
}

// Verify checks s for purpose and returns its payload.
func (s Signed) Verify(keys KeySource, purpose string) ([]byte, error) {
	if s.Format != SignedFormat || s.Purpose != purpose || s.KeyID == "" {
		return nil, fmt.Errorf("%w: format or purpose", ErrBadSignature)
	}
	k, err := keys.Key(s.KeyID)
	if err != nil {
		return nil, err
	}
	mk, err := signingKey(k, purpose)
	if err != nil {
		return nil, err
	}
	got, err := hex.DecodeString(s.MAC)
	if err != nil || !hmac.Equal(got, signMAC(mk, purpose, s.KeyID, s.Payload)) {
		return nil, ErrBadSignature
	}
	return s.Payload, nil
}

// EncodeSigned marshals a Signed document.
func EncodeSigned(s Signed) ([]byte, error) { return json.Marshal(s) }

// DecodeSigned parses a Signed document.
func DecodeSigned(data []byte) (Signed, error) {
	var s Signed
	if err := json.Unmarshal(data, &s); err != nil {
		return Signed{}, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	return s, nil
}
