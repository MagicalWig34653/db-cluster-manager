// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Secrets at rest.
//
// Documents that hold secret or authentication material (session digests,
// recovery-code hashes, passkey records) are sealed with AES-256-GCM before
// they enter the replicated log, so the Raft log, snapshots and backups of
// the data directory do not contain them in clear. The additional data binds
// a sealed value to its collection and key, so a value cannot be moved to
// another document.
//
// Key provisioning: FileKeyProvider creates a random key file on first use,
// which is only adequate for a single-instance installation or replicas
// given the same file out of band. Keyring (keyring.go) distributes the key
// across replicas in wrapped form and keeps a recovery envelope for the
// backup recovery key (B07). Rotation re-seals documents in bounded, resumable transactions (keyrotation.go).

// ErrNoKey is returned when the KeyProvider does not know a key ID.
var ErrNoKey = errors.New("manager: data key not available")

// KeyProvider supplies data keys for sealing documents.
type KeyProvider interface {
	// Current returns the key ID and 32-byte key used for new seals.
	Current() (id string, key []byte, err error)
	// Key returns the key with the given ID, or ErrNoKey.
	Key(id string) ([]byte, error)
}

// StaticKey is a KeyProvider with one key (tests and explicit
// configuration).
type StaticKey struct {
	ID    string
	Bytes []byte
}

// Current implements KeyProvider.
func (k StaticKey) Current() (string, []byte, error) {
	if len(k.Bytes) != 32 || k.ID == "" {
		return "", nil, fmt.Errorf("%w: static key needs an ID and 32 bytes", ErrNoKey)
	}
	return k.ID, k.Bytes, nil
}

// Key implements KeyProvider.
func (k StaticKey) Key(id string) ([]byte, error) {
	if id != k.ID || len(k.Bytes) != 32 {
		return nil, fmt.Errorf("%w: %q", ErrNoKey, id)
	}
	return k.Bytes, nil
}

// FileKeyProvider loads a 32-byte key from path, creating it with random
// bytes (mode 0600) when it does not exist. Lab-grade; see above.
func FileKeyProvider(path string) (StaticKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return StaticKey{}, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return StaticKey{}, fmt.Errorf("manager: create key dir: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return StaticKey{}, fmt.Errorf("manager: create data key: %w", err)
		}
		if _, err := f.Write(b); err != nil {
			_ = f.Close()
			return StaticKey{}, fmt.Errorf("manager: write data key: %w", err)
		}
		if err := f.Close(); err != nil {
			return StaticKey{}, err
		}
	} else if err != nil {
		return StaticKey{}, fmt.Errorf("manager: read data key: %w", err)
	}
	if len(b) != 32 {
		return StaticKey{}, fmt.Errorf("%w: %s holds %d bytes, want 32", ErrNoKey, path, len(b))
	}
	return StaticKey{ID: "file", Bytes: b}, nil
}

// sealed is the stored form of a sealed document value.
type sealed struct {
	KeyID string `json:"kid"`
	Nonce []byte `json:"nonce"`
	Data  []byte `json:"data"`
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func aad(coll, key string) []byte { return []byte("dcm-sealed/1\x00" + coll + "\x00" + key) }

// seal encrypts the JSON encoding of v for (coll, key).
func seal(kp KeyProvider, coll, key string, v any) (sealed, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return sealed{}, err
	}
	id, k, err := kp.Current()
	if err != nil {
		return sealed{}, err
	}
	a, err := gcm(k)
	if err != nil {
		return sealed{}, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return sealed{}, err
	}
	return sealed{KeyID: id, Nonce: nonce, Data: a.Seal(nil, nonce, plain, aad(coll, key))}, nil
}

// unseal decrypts a sealed value of (coll, key) into out.
func unseal(kp KeyProvider, coll, key string, s sealed, out any) error {
	k, err := kp.Key(s.KeyID)
	if err != nil {
		return err
	}
	a, err := gcm(k)
	if err != nil {
		return err
	}
	if len(s.Nonce) != a.NonceSize() {
		return fmt.Errorf("manager: sealed %s/%s: bad nonce", coll, key)
	}
	plain, err := a.Open(nil, s.Nonce, s.Data, aad(coll, key))
	if err != nil {
		return fmt.Errorf("manager: unseal %s/%s: %w", coll, key, err)
	}
	return json.Unmarshal(plain, out)
}

// PutSealed seals v and writes it unconditionally.
func PutSealed(s *Store, kp KeyProvider, coll, key string, v any) error {
	sv, err := seal(kp, coll, key, v)
	if err != nil {
		return err
	}
	_, err = Put(s, coll, key, sv, nil)
	return err
}

// GetSealed decodes one sealed document.
func GetSealed[T any](s *Store, kp KeyProvider, coll, key string, opts ...ReadOption) (T, bool, error) {
	var v T
	d, ok, err := Get[sealed](s, coll, key, opts...)
	if err != nil || !ok {
		return v, ok, err
	}
	if err := unseal(kp, coll, key, d.Value, &v); err != nil {
		return v, false, err
	}
	return v, true, nil
}

// ListSealed decodes every sealed document of a collection.
func ListSealed[T any](s *Store, kp KeyProvider, coll string, opts ...ReadOption) ([]Versioned[T], error) {
	docs, err := List[sealed](s, coll, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]Versioned[T], 0, len(docs))
	for _, d := range docs {
		var v T
		if err := unseal(kp, coll, d.Key, d.Value, &v); err != nil {
			return nil, err
		}
		out = append(out, Versioned[T]{Key: d.Key, Version: d.Version, Value: v})
	}
	return out, nil
}
