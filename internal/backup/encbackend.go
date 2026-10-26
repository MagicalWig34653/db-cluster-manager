// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bufio"
	"context"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// EncryptedBackend wraps a Backend with client-side encryption: every
// object is sealed before it leaves the process, so the remote target
// (S3-compatible store, SFTP server, local directory) only sees ciphertext
// and object names. It uses a data key of the manager keyring (KeySource);
// the key ID is stored in the object header, so a rotated keyring keeps
// older objects readable.
//
// Object format: "DBCE1" | key ID length (1) | key ID | salt (16), then
// chunks of up to 64 KiB plaintext, each AES-256-GCM sealed with the key
// HKDF(dataKey, salt, "dbcm/object"), the chunk counter as nonce and
// AAD = header | name | counter | final flag. The name binds an object to
// its place (a swap of two objects fails), the final flag detects
// truncation and appended chunks.
//
// List sizes are those of the stored (encrypted) objects; names and
// modification times are unchanged. Delete is passed through, so retention
// (dbbackup.ApplyRetention) works on the wrapper unchanged.
type EncryptedBackend struct {
	Inner Backend
	Keys  KeySource
}

const (
	encMagic    = "DBCE1"
	encChunk    = 64 << 10
	encSaltSize = 16
	encTagSize  = 16
)

// ErrEncryptedObject: the object is not a valid encrypted object (wrong
// magic, truncated, reordered or tampered).
var ErrEncryptedObject = errors.New("backup: invalid encrypted object")

func encAEAD(dataKey, salt []byte) (cipher.AEAD, error) {
	k, err := hkdf.Key(sha256.New, dataKey, salt, "dbcm/object", 32)
	if err != nil {
		return nil, err
	}
	return newGCM(k)
}

func encNonce(i uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], i)
	return n
}

func encAAD(hdr []byte, name string, i uint64, final bool) []byte {
	a := append(append([]byte(nil), hdr...), 0)
	a = append(append(a, name...), 0)
	a = binary.BigEndian.AppendUint64(a, i)
	if final {
		return append(a, 1)
	}
	return append(a, 0)
}

// Put implements Backend: it encrypts r while streaming it to the inner
// backend.
func (e *EncryptedBackend) Put(ctx context.Context, name string, r io.Reader) error {
	if err := validName(name); err != nil {
		return err
	}
	id, key, err := e.Keys.Current()
	if err != nil {
		return err
	}
	if len(id) == 0 || len(id) > 255 {
		return fmt.Errorf("%w: key ID length", ErrUnknownKey)
	}
	salt := make([]byte, encSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	aead, err := encAEAD(key, salt)
	if err != nil {
		return err
	}
	hdr := append(append([]byte(encMagic), byte(len(id))), id...)
	hdr = append(hdr, salt...)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(encryptTo(pw, hdr, name, aead, &ctxReader{ctx, r})) }()
	err = e.Inner.Put(ctx, name, pr)
	pr.CloseWithError(err)
	return err
}

func encryptTo(w io.Writer, hdr []byte, name string, aead cipher.AEAD, r io.Reader) error {
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	br := bufio.NewReaderSize(r, encChunk+1)
	buf := make([]byte, encChunk)
	for i := uint64(0); ; i++ {
		n, err := io.ReadFull(br, buf)
		final := false
		switch {
		case err == io.EOF || err == io.ErrUnexpectedEOF:
			final = true
		case err != nil:
			return err
		default:
			if _, perr := br.Peek(1); perr == io.EOF {
				final = true
			} else if perr != nil {
				return perr
			}
		}
		if _, err := w.Write(aead.Seal(nil, encNonce(i), buf[:n], encAAD(hdr, name, i, final))); err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}

type encReader struct {
	rc    io.Closer
	br    *bufio.Reader
	aead  cipher.AEAD
	hdr   []byte
	name  string
	i     uint64
	buf   []byte
	final bool
	err   error
}

// Get implements Backend: the returned reader decrypts and authenticates
// chunk by chunk; truncation or tampering surfaces as an error before the
// damaged chunk's bytes are released.
func (e *EncryptedBackend) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	rc, err := e.Inner.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.ReadCloser, error) { _ = rc.Close(); return nil, err }
	br := bufio.NewReaderSize(rc, encChunk+encTagSize+1)
	head := make([]byte, len(encMagic)+1)
	if _, err := io.ReadFull(br, head); err != nil || string(head[:len(encMagic)]) != encMagic || head[len(encMagic)] == 0 {
		return fail(fmt.Errorf("%w: header", ErrEncryptedObject))
	}
	idLen := int(head[len(encMagic)])
	rest := make([]byte, idLen+encSaltSize)
	if _, err := io.ReadFull(br, rest); err != nil {
		return fail(fmt.Errorf("%w: header", ErrEncryptedObject))
	}
	key, err := e.Keys.Key(string(rest[:idLen]))
	if err != nil {
		return fail(err)
	}
	aead, err := encAEAD(key, rest[idLen:])
	if err != nil {
		return fail(err)
	}
	return &encReader{rc: rc, br: br, aead: aead, hdr: append(head, rest...), name: name}, nil
}

func (r *encReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.final {
			return 0, io.EOF
		}
		r.next()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *encReader) next() {
	ct := make([]byte, encChunk+encTagSize)
	n, err := io.ReadFull(r.br, ct)
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		r.final = true
	case err != nil:
		r.err = err
		return
	default:
		if _, perr := r.br.Peek(1); perr == io.EOF {
			r.final = true
		} else if perr != nil {
			r.err = perr
			return
		}
	}
	if n < encTagSize {
		r.err = fmt.Errorf("%w: truncated", ErrEncryptedObject)
		return
	}
	pt, err := r.aead.Open(nil, encNonce(r.i), ct[:n], encAAD(r.hdr, r.name, r.i, r.final))
	if err != nil {
		r.err = fmt.Errorf("%w: chunk %d: %v", ErrEncryptedObject, r.i, ErrDecrypt)
		return
	}
	r.i++
	r.buf = pt
}

func (r *encReader) Close() error { return r.rc.Close() }

// List implements Backend.
func (e *EncryptedBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	return e.Inner.List(ctx, prefix)
}

// Delete implements Backend.
func (e *EncryptedBackend) Delete(ctx context.Context, name string) error {
	return e.Inner.Delete(ctx, name)
}
