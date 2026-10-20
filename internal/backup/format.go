// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Container layout (all integers big endian):
//
//	magic[8] | version u16 | metaLen u32 | meta JSON | wrapLen u32 | wrap JSON | payloadNonce[12] | ciphertext
//
// The magic, version and meta section are authenticated as AAD of both the
// key wrap and the payload. The wrap section holds the key id and the wrapped
// data key, so rotation only rewrites it and never touches the payload.
const (
	magic = "DBCMBKP\n"
	// FormatVersion is the container version written by Seal.
	FormatVersion = 1
	// KDFRaw means the recovery key is used directly as the 256-bit
	// key-encryption key (it is random, so no password stretching is needed).
	KDFRaw = 1
	// CipherAES256GCM identifies AES-256-GCM for payload and key wrap.
	CipherAES256GCM = 1

	maxSection = 1 << 20
	nonceSize  = 12
)

var (
	// ErrNotBackup means the data does not start with the container magic.
	ErrNotBackup = errors.New("backup: not a backup container")
	// ErrVersion means the container version is unsupported.
	ErrVersion = errors.New("backup: unsupported container version")
	// ErrCorrupt means the container is truncated or malformed.
	ErrCorrupt = errors.New("backup: corrupt or truncated container")
	// ErrDecrypt means authentication failed: wrong key or tampered data.
	ErrDecrypt = errors.New("backup: decryption failed (wrong key or tampered data)")
	// ErrAlgorithm means unknown KDF or cipher ids.
	ErrAlgorithm = errors.New("backup: unsupported algorithm id")
)

// Meta is the authenticated, non-secret header information.
type Meta struct {
	KDF                int       `json:"kdf"`
	Cipher             int       `json:"cipher"`
	Created            time.Time `json:"created"`
	SourceInstallation string    `json:"source_installation"`
	MetaTerm           uint64    `json:"metastore_term"`
	MetaIndex          uint64    `json:"metastore_index"`
}

type wrapSection struct {
	KeyID      string `json:"key_id"`
	Nonce      []byte `json:"nonce"`
	WrappedKey []byte `json:"wrapped_key"`
}

// Header is the parsed, not yet authenticated header of a container.
type Header struct {
	Version int
	Meta    Meta
	KeyID   string
}

// Seal encrypts payload into a container. KDF and cipher ids in meta are set
// by Seal.
func Seal(payload []byte, meta Meta, key RecoveryKey) ([]byte, error) {
	meta.KDF, meta.Cipher = KDFRaw, CipherAES256GCM
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	aad := buildAAD(FormatVersion, metaJSON)
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, err
	}
	wrap, err := wrapKey(dataKey, key, aad)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return assemble(aad, wrap, nonce, gcm.Seal(nil, nonce, payload, aad))
}

// Open decrypts a container and returns the payload and header.
func Open(data []byte, key RecoveryKey) ([]byte, Header, error) {
	p, err := parse(data)
	if err != nil {
		return nil, Header{}, err
	}
	if p.wrap.KeyID != key.ID() {
		return nil, p.header, ErrDecrypt
	}
	dataKey, err := unwrapKey(p.wrap, key, p.aad)
	if err != nil {
		return nil, p.header, err
	}
	gcm, err := newGCM(dataKey)
	if err != nil {
		return nil, p.header, err
	}
	pt, err := gcm.Open(nil, p.nonce, p.ct, p.aad)
	if err != nil {
		return nil, p.header, ErrDecrypt
	}
	return pt, p.header, nil
}

// ReadHeader parses the header without any key. The content is not
// authenticated; it is informational (for example to pick the right key).
func ReadHeader(data []byte) (Header, error) {
	p, err := parse(data)
	if err != nil {
		return Header{}, err
	}
	return p.header, nil
}

// Rewrap re-encrypts the data key of a container from oldKey to newKey (key
// rotation). The payload ciphertext is unchanged.
func Rewrap(data []byte, oldKey, newKey RecoveryKey) ([]byte, error) {
	p, err := parse(data)
	if err != nil {
		return nil, err
	}
	dataKey, err := unwrapKey(p.wrap, oldKey, p.aad)
	if err != nil {
		return nil, err
	}
	wrap, err := wrapKey(dataKey, newKey, p.aad)
	if err != nil {
		return nil, err
	}
	return assemble(p.aad, wrap, p.nonce, p.ct)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func wrapKey(dataKey []byte, key RecoveryKey, aad []byte) (wrapSection, error) {
	gcm, err := newGCM(key[:])
	if err != nil {
		return wrapSection{}, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return wrapSection{}, err
	}
	return wrapSection{KeyID: key.ID(), Nonce: nonce, WrappedKey: gcm.Seal(nil, nonce, dataKey, aad)}, nil
}

func unwrapKey(w wrapSection, key RecoveryKey, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key[:])
	if err != nil {
		return nil, err
	}
	if len(w.Nonce) != nonceSize {
		return nil, ErrCorrupt
	}
	dk, err := gcm.Open(nil, w.Nonce, w.WrappedKey, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return dk, nil
}

func buildAAD(version uint16, metaJSON []byte) []byte {
	var b bytes.Buffer
	b.WriteString(magic)
	_ = binary.Write(&b, binary.BigEndian, version)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(metaJSON)))
	b.Write(metaJSON)
	return b.Bytes()
}

func assemble(aad []byte, w wrapSection, nonce, ct []byte) ([]byte, error) {
	wj, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(aad)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(wj)))
	b.Write(wj)
	b.Write(nonce)
	b.Write(ct)
	return b.Bytes(), nil
}

type parsed struct {
	header Header
	aad    []byte
	wrap   wrapSection
	nonce  []byte
	ct     []byte
}

func parse(data []byte) (parsed, error) {
	var p parsed
	if len(data) < len(magic) || string(data[:len(magic)]) != magic {
		return p, ErrNotBackup
	}
	r := data[len(magic):]
	if len(r) < 2 {
		return p, ErrCorrupt
	}
	version := binary.BigEndian.Uint16(r)
	if version != FormatVersion {
		return p, fmt.Errorf("%w: %d", ErrVersion, version)
	}
	r = r[2:]
	meta, r, ok := section(r)
	if !ok {
		return p, ErrCorrupt
	}
	wrapJSON, r, ok := section(r)
	if !ok {
		return p, ErrCorrupt
	}
	var m Meta
	if err := json.Unmarshal(meta, &m); err != nil {
		return p, ErrCorrupt
	}
	if m.KDF != KDFRaw || m.Cipher != CipherAES256GCM {
		return p, ErrAlgorithm
	}
	if err := json.Unmarshal(wrapJSON, &p.wrap); err != nil {
		return p, ErrCorrupt
	}
	if len(r) < nonceSize+16 {
		return p, ErrCorrupt
	}
	p.nonce, p.ct = r[:nonceSize], r[nonceSize:]
	p.aad = buildAAD(version, meta)
	p.header = Header{Version: int(version), Meta: m, KeyID: p.wrap.KeyID}
	return p, nil
}

func section(r []byte) (sec, rest []byte, ok bool) {
	if len(r) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(r)
	if n > maxSection || uint64(len(r)-4) < uint64(n) {
		return nil, nil, false
	}
	return r[4 : 4+n], r[4+n:], true
}
