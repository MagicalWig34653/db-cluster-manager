// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// DefaultPBKDF2Iterations is the PBKDF2-HMAC-SHA256 iteration count used for
// new sealed blobs. The count is stored in the blob, so it can be raised later.
const DefaultPBKDF2Iterations = 600_000

const sealVersion = 1

// ErrBadPassphrase is returned when a sealed blob cannot be opened. A wrong
// passphrase and a tampered blob are deliberately indistinguishable.
var ErrBadPassphrase = errors.New("certs: wrong passphrase or corrupted sealed data")

type sealed struct {
	Version    int    `json:"v"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iter"`
	Salt       []byte `json:"salt"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ct"`
	// Public carries non-secret data (for example the certificate PEM) that
	// is authenticated as additional data.
	Public []byte `json:"public,omitempty"`
}

func deriveKey(passphrase string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, passphrase, salt, iter, 32)
}

func seal(plaintext, public []byte, passphrase string, iter int) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("certs: empty passphrase")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := deriveKey(passphrase, salt, iter)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	s := sealed{
		Version: sealVersion, KDF: "pbkdf2-sha256", Iterations: iter,
		Salt: salt, Nonce: nonce, Public: public,
		Ciphertext: gcm.Seal(nil, nonce, plaintext, public),
	}
	return json.Marshal(s)
}

func open(blob []byte, passphrase string) (plaintext, public []byte, err error) {
	var s sealed
	if err := json.Unmarshal(blob, &s); err != nil {
		return nil, nil, fmt.Errorf("certs: malformed sealed data: %w", err)
	}
	if s.Version != sealVersion || s.KDF != "pbkdf2-sha256" || s.Iterations < 1 {
		return nil, nil, errors.New("certs: unsupported sealed data format")
	}
	key, err := deriveKey(passphrase, s.Salt, s.Iterations)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	if len(s.Nonce) != gcm.NonceSize() {
		return nil, nil, ErrBadPassphrase
	}
	pt, err := gcm.Open(nil, s.Nonce, s.Ciphertext, s.Public)
	if err != nil {
		return nil, nil, ErrBadPassphrase
	}
	return pt, s.Public, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
