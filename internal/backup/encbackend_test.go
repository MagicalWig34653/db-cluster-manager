// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func testKeys(t *testing.T) StaticKeys {
	t.Helper()
	k1, k2 := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(k1)
	_, _ = rand.Read(k2)
	return StaticKeys{CurrentID: "k2", Keys: map[string][]byte{"k1": k1, "k2": k2}}
}

func readAll(t *testing.T, b Backend, name string) ([]byte, error) {
	t.Helper()
	rc, err := b.Get(context.Background(), name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func TestEncryptedBackendRoundTripSizes(t *testing.T) {
	ctx := context.Background()
	inner := NewMemBackend()
	eb := &EncryptedBackend{Inner: inner, Keys: testKeys(t)}
	for _, n := range []int{0, 1, encChunk - 1, encChunk, encChunk + 1, 3*encChunk + 17} {
		data := make([]byte, n)
		_, _ = rand.Read(data)
		if err := eb.Put(ctx, "obj", bytes.NewReader(data)); err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		got, err := readAll(t, eb, "obj")
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: round trip err=%v equal=%v", n, err, bytes.Equal(got, data))
		}
		raw, _ := readAll(t, inner, "obj")
		if n >= 16 && bytes.Contains(raw, data[:16]) {
			t.Fatalf("size %d: plaintext visible in the stored object", n)
		}
	}
}

func TestEncryptedBackendTamperTruncateSwap(t *testing.T) {
	ctx := context.Background()
	inner := NewMemBackend()
	keys := testKeys(t)
	eb := &EncryptedBackend{Inner: inner, Keys: keys}
	data := bytes.Repeat([]byte("abcdefgh"), 3*encChunk/8)
	if err := eb.Put(ctx, "a", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	raw, _ := readAll(t, inner, "a")
	put := func(name string, b []byte) {
		if err := inner.Put(ctx, name, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)/2] ^= 1
	put("flip", flipped)
	hdr := len(encMagic) + 1 + 2 + encSaltSize
	chunkLen := encChunk + encTagSize
	put("trunc", raw[:hdr+chunkLen]) // chunk 0 is not flagged final
	put("trunc2", raw[:len(raw)-5])
	put("ext", append(append([]byte(nil), raw...), raw[hdr:hdr+chunkLen]...))
	put("plain", []byte("not encrypted at all"))
	put("moved", raw) // the name is bound into every chunk
	for _, n := range []string{"flip", "trunc", "trunc2", "ext", "plain", "moved"} {
		if _, err := readAll(t, eb, n); err == nil {
			t.Errorf("%s: damaged object read without error", n)
		}
	}
	other := &EncryptedBackend{Inner: inner, Keys: testKeys(t)}
	if _, err := readAll(t, other, "a"); err == nil {
		t.Error("another keyring decrypted the object")
	}
	missing := &EncryptedBackend{Inner: inner, Keys: StaticKeys{CurrentID: "x", Keys: map[string][]byte{"x": make([]byte, 32)}}}
	if _, err := missing.Get(ctx, "a"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown key ID: err = %v", err)
	}
}

func TestEncryptedBackendKeyRotationKeepsOldObjects(t *testing.T) {
	ctx := context.Background()
	inner := NewMemBackend()
	keys := testKeys(t)
	old := keys
	old.CurrentID = "k1"
	if err := (&EncryptedBackend{Inner: inner, Keys: old}).Put(ctx, "o1", bytes.NewReader([]byte("old data"))); err != nil {
		t.Fatal(err)
	}
	eb := &EncryptedBackend{Inner: inner, Keys: keys}
	if err := eb.Put(ctx, "o2", bytes.NewReader([]byte("new data"))); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]string{"o1": "old data", "o2": "new data"} {
		if got, err := readAll(t, eb, n); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", n, got, err)
		}
	}
	h1, _ := readAll(t, inner, "o1")
	h2, _ := readAll(t, inner, "o2")
	if !bytes.Contains(h1[:12], []byte("k1")) || !bytes.Contains(h2[:12], []byte("k2")) {
		t.Error("key IDs missing from the headers")
	}
}

func TestEncryptedBackendSharedSuites(t *testing.T) {
	keys := testKeys(t)
	t.Run("dir", func(t *testing.T) {
		d, err := NewDirBackend(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		testBackendOverhead(t, &EncryptedBackend{Inner: d, Keys: keys}, 40)
	})
	t.Run("s3", func(t *testing.T) {
		e := startFakeS3(t, false, nil)
		eb := &EncryptedBackend{Inner: e.be, Keys: keys}
		testBackendOverhead(t, eb, 40)
		if err := eb.Put(context.Background(), "secret.bin", bytes.NewReader([]byte("PLAINTEXT-MARKER-0123456789"))); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(e.stored("pre/secret.bin"), []byte("PLAINTEXT-MARKER")) {
			t.Fatal("plaintext reached the S3 server")
		}
	})
	t.Run("sftp", func(t *testing.T) {
		s := startSFTP(t)
		b, err := NewSFTPBackend(s.cfg())
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		testBackendOverhead(t, &EncryptedBackend{Inner: b, Keys: keys}, 40)
	})
}

func TestSignedPurposeAndTamper(t *testing.T) {
	keys := testKeys(t)
	s, err := Sign(keys, "transfer", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.Verify(keys, "transfer"); err != nil || string(p) != `{"a":1}` {
		t.Fatalf("verify: %q %v", p, err)
	}
	if _, err := s.Verify(keys, "other"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other purpose: %v", err)
	}
	bad := s
	bad.Payload = []byte(`{"a":2}`)
	if _, err := bad.Verify(keys, "transfer"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered payload: %v", err)
	}
	bad = s
	bad.KeyID = "k1"
	if _, err := bad.Verify(keys, "transfer"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other key id: %v", err)
	}
	if _, err := s.Verify(testKeys(t), "transfer"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other keys: %v", err)
	}
	enc, _ := EncodeSigned(s)
	dec, err := DecodeSigned(enc)
	if err != nil || dec.MAC != s.MAC {
		t.Fatalf("decode: %v", err)
	}
}
