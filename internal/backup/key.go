// SPDX-License-Identifier: AGPL-3.0-only

// Package backup implements encrypted manager-configuration backups
// (requirements B07-B09): a versioned container format, a recovery key that
// is shown once and never stored, storage backends, retention selection and
// an inspection-first restore flow.
package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// RecoveryKeySize is the size of the externally retained recovery key.
const RecoveryKeySize = 32

const keyChecksumSize = 4

// ErrKeyFormat reports a malformed recovery key string.
var ErrKeyFormat = errors.New("backup: malformed recovery key")

// ErrKeyChecksum reports a recovery key string with a failed checksum,
// typically a typing error.
var ErrKeyChecksum = errors.New("backup: recovery key checksum mismatch")

// RecoveryKey is a 256-bit key that is shown once and never stored by the
// manager.
type RecoveryKey [RecoveryKeySize]byte

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateRecoveryKey creates a new random recovery key.
func GenerateRecoveryKey() (RecoveryKey, error) {
	var k RecoveryKey
	if _, err := rand.Read(k[:]); err != nil {
		return k, fmt.Errorf("backup: generate key: %w", err)
	}
	return k, nil
}

func keyChecksum(k *RecoveryKey) []byte {
	h := sha256.Sum256(append([]byte("dbcm-recovery-key-checksum\x00"), k[:]...))
	return h[:keyChecksumSize]
}

// String renders the key as grouped base32 (groups of five, dash separated)
// with a trailing checksum. Callers must show it once and not log it.
func (k RecoveryKey) String() string {
	raw := append(append([]byte{}, k[:]...), keyChecksum(&k)...)
	s := keyEncoding.EncodeToString(raw)
	var b strings.Builder
	for i := 0; i < len(s); i += 5 {
		if i > 0 {
			b.WriteByte('-')
		}
		end := min(i+5, len(s))
		b.WriteString(s[i:end])
	}
	return b.String()
}

// ParseRecoveryKey parses the output of RecoveryKey.String. Dashes, spaces
// and letter case are ignored.
func ParseRecoveryKey(s string) (RecoveryKey, error) {
	var k RecoveryKey
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "", "\n", "").Replace(s))
	raw, err := keyEncoding.DecodeString(s)
	if err != nil || len(raw) != RecoveryKeySize+keyChecksumSize {
		return k, ErrKeyFormat
	}
	copy(k[:], raw[:RecoveryKeySize])
	if subtle.ConstantTimeCompare(raw[RecoveryKeySize:], keyChecksum(&k)) != 1 {
		return RecoveryKey{}, ErrKeyChecksum
	}
	return k, nil
}

// ID returns a non-secret identifier derived from the key, used to tell which
// key a backup is wrapped with.
func (k RecoveryKey) ID() string {
	h := sha256.Sum256(append([]byte("dbcm-recovery-key-id\x00"), k[:]...))
	return hex.EncodeToString(h[:8])
}
