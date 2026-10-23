// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sealStream(t *testing.T, key RecoveryKey, payload []byte, chunk int) ([]byte, StreamSummary) {
	t.Helper()
	pub, err := key.StreamPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w, err := NewSealWriter(&out, StreamMeta{ChunkSize: chunk, Kind: "physical", BackupID: "b1", ClusterID: "c1",
		Created: time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)}, pub)
	if err != nil {
		t.Fatal(err)
	}
	// Write in odd-sized pieces to exercise buffering.
	for p := payload; len(p) > 0; {
		n := min(len(p), 7)
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), w.Summary()
}

func openAll(data []byte, key RecoveryKey) ([]byte, *StreamReader, error) {
	r, err := OpenStream(bytes.NewReader(data), key)
	if err != nil {
		return nil, nil, err
	}
	pt, err := io.ReadAll(r)
	return pt, r, err
}

func randBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStreamRoundTripSizes(t *testing.T) {
	key := mustKey(t)
	for _, n := range []int{0, 1, 63, 64, 65, 128, 1000} {
		payload := randBytes(t, n)
		data, sum := sealStream(t, key, payload, 64)
		pt, r, err := openAll(data, key)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(pt, payload) {
			t.Fatalf("n=%d: payload differs", n)
		}
		if err := CompareSummary(r.Summary(), sum); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		wantChunks := max(1, (n+63)/64)
		if len(sum.Chunks) != wantChunks {
			t.Fatalf("n=%d: %d chunks, want %d", n, len(sum.Chunks), wantChunks)
		}
		h := sha256.Sum256(payload)
		if sum.PlainSHA256 != "" || sum.PlainMAC == "" || sum.PlainMAC == hex.EncodeToString(h[:]) || sum.PlainSize != int64(n) {
			t.Fatalf("n=%d: plaintext summary wrong", n)
		}
		if got := r.Summary(); got.PlainSHA256 != hex.EncodeToString(h[:]) {
			t.Fatalf("n=%d: reader plain digest wrong", n)
		}
		for _, c := range sum.Chunks {
			if c.SHA256 != "" || len(c.MAC) != 64 {
				t.Fatalf("n=%d: chunk digest %+v", n, c)
			}
		}
		ch := sha256.Sum256(data)
		if sum.CipherSHA256 != hex.EncodeToString(ch[:]) || sum.CipherSize != int64(len(data)) {
			t.Fatalf("n=%d: cipher summary wrong", n)
		}
		if r.Meta().BackupID != "b1" || r.Meta().Wrap != WrapX25519 {
			t.Fatalf("meta %+v", r.Meta())
		}
	}
}

func TestStreamWrongKey(t *testing.T) {
	data, _ := sealStream(t, mustKey(t), []byte("secret"), 64)
	if _, err := OpenStream(bytes.NewReader(data), mustKey(t)); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v", err)
	}
}

func TestStreamPublicKeyParse(t *testing.T) {
	key := mustKey(t)
	pub, _ := key.StreamPublicKey()
	got, err := ParseStreamPublicKey(pub.String())
	if err != nil || got.ID() != pub.ID() {
		t.Fatalf("parse: %v", err)
	}
	if strings.Contains(pub.String(), key.String()) {
		t.Fatal("public key leaks the recovery key")
	}
	for _, bad := range []string{"", "dbcmpk1:", "dbcmpk1:!!", "x:" + pub.String()[8:]} {
		if _, err := ParseStreamPublicKey(bad); !errors.Is(err, ErrKeyFormat) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	// Encrypting with a parsed key decrypts with the recovery key.
	var out bytes.Buffer
	w, _ := NewSealWriter(&out, StreamMeta{BackupID: "x"}, got)
	_, _ = w.Write([]byte("hello"))
	_ = w.Close()
	pt, _, err := openAll(out.Bytes(), key)
	if err != nil || string(pt) != "hello" {
		t.Fatalf("got %q %v", pt, err)
	}
}

// chunkOffsets returns the byte offsets of each chunk header.
func chunkOffsets(t *testing.T, data []byte) []int {
	t.Helper()
	off := len(streamMagic) + 2
	for range 2 {
		off += 4 + int(binary.BigEndian.Uint32(data[off:]))
	}
	var out []int
	for off < len(data) {
		out = append(out, off)
		off += 5 + int(binary.BigEndian.Uint32(data[off+1:]))
	}
	return out
}

func TestStreamTamperingDetected(t *testing.T) {
	key := mustKey(t)
	payload := randBytes(t, 64*3+10)
	data, _ := sealStream(t, key, payload, 64)
	offs := chunkOffsets(t, data)
	if len(offs) != 4 {
		t.Fatalf("%d chunks", len(offs))
	}
	cases := map[string]func() []byte{
		"truncated before final": func() []byte { return append([]byte(nil), data[:offs[3]]...) },
		"truncated inside chunk": func() []byte { return append([]byte(nil), data[:offs[2]+20]...) },
		"trailing data":          func() []byte { return append(append([]byte(nil), data...), 0) },
		"bit flip": func() []byte {
			d := append([]byte(nil), data...)
			d[offs[1]+10] ^= 1
			return d
		},
		"chunks swapped": func() []byte {
			d := append([]byte(nil), data[:offs[0]]...)
			d = append(d, data[offs[1]:offs[2]]...)
			d = append(d, data[offs[0]:offs[1]]...)
			return append(d, data[offs[2]:]...)
		},
		"final flag cleared": func() []byte {
			d := append([]byte(nil), data...)
			d[offs[3]] = 0
			return d
		},
		"meta changed": func() []byte {
			return bytes.Replace(data, []byte(`"backup_id":"b1"`), []byte(`"backup_id":"b2"`), 1)
		},
	}
	for name, mk := range cases {
		_, _, err := openAll(mk(), key)
		if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrDecrypt) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
	// A chunk from another stream with the same key cannot be spliced in.
	other, _ := sealStream(t, key, payload, 64)
	oo := chunkOffsets(t, other)
	d := append([]byte(nil), data[:offs[1]]...)
	d = append(d, other[oo[1]:oo[2]]...)
	d = append(d, data[offs[2]:]...)
	if _, _, err := openAll(d, key); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("splice: %v", err)
	}
}

func TestStreamHeaderWithoutKey(t *testing.T) {
	key := mustKey(t)
	data, sum := sealStream(t, key, []byte("x"), 64)
	m, kid, err := ReadStreamMeta(bytes.NewReader(data))
	if err != nil || m.ClusterID != "c1" || kid != sum.KeyID {
		t.Fatalf("%+v %s %v", m, kid, err)
	}
	if _, _, err := ReadStreamMeta(strings.NewReader("nope")); !errors.Is(err, ErrNotBackup) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareSummaryMismatch(t *testing.T) {
	key := mustKey(t)
	data, sum := sealStream(t, key, randBytes(t, 200), 64)
	_, r, _ := openAll(data, key)
	got := r.Summary()
	bad := sum
	bad.Chunks = append([]ChunkDigest(nil), sum.Chunks...)
	bad.Chunks[1].MAC = strings.Repeat("0", 64)
	if err := CompareSummary(got, bad); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("got %v", err)
	}
	bad = sum
	bad.KeyID = "other"
	if err := CompareSummary(got, bad); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("got %v", err)
	}
	bad = sum
	bad.PlainMAC = strings.Repeat("0", 64)
	if err := CompareSummary(got, bad); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("got %v", err)
	}
	// A keyed summary without MACs on a chunk (stripped) fails.
	bad = sum
	bad.Chunks = append([]ChunkDigest(nil), sum.Chunks...)
	bad.Chunks[0].MAC = ""
	if err := CompareSummary(got, bad); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("stripped chunk mac: %v", err)
	}
}

// TestCompareSummaryVersion1: manifests written before the keyed digests
// (plain SHA-256 per chunk) stay verifiable with a reader summary, and
// tampering is still detected.
func TestCompareSummaryVersion1(t *testing.T) {
	key := mustKey(t)
	payload := randBytes(t, 200)
	data, _ := sealStream(t, key, payload, 64)
	_, r, _ := openAll(data, key)
	got := r.Summary()
	v1 := got
	v1.PlainMAC = ""
	v1.Chunks = append([]ChunkDigest(nil), got.Chunks...)
	for i := range v1.Chunks {
		v1.Chunks[i].MAC = ""
	}
	if err := CompareSummary(got, v1); err != nil {
		t.Fatal(err)
	}
	v1.Chunks[2].SHA256 = strings.Repeat("0", 64)
	if err := CompareSummary(got, v1); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("got %v", err)
	}
}

// TestChunkMACsDoNotRevealPlaintext: two streams of the same plaintext
// have unrelated chunk digests (per-stream key), and no digest equals the
// plain SHA-256 of the chunk.
func TestChunkMACsDoNotRevealPlaintext(t *testing.T) {
	key := mustKey(t)
	payload := randBytes(t, 128)
	_, a := sealStream(t, key, payload, 64)
	_, b := sealStream(t, key, payload, 64)
	for i := range a.Chunks {
		plain := sha256.Sum256(payload[i*64 : (i+1)*64])
		if a.Chunks[i].MAC == b.Chunks[i].MAC || a.Chunks[i].MAC == hex.EncodeToString(plain[:]) {
			t.Fatalf("chunk %d digest links streams or reveals the plaintext", i)
		}
	}
	if a.PlainMAC == b.PlainMAC {
		t.Fatal("plaintext MAC links two streams")
	}
}

type failAfter struct {
	data []byte
	err  error
}

func (f *failAfter) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestPutStreamDirAndFailure(t *testing.T) {
	dir := t.TempDir()
	b, err := NewDirBackend(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := randBytes(t, 5000)
	res, err := PutStream(ctx, b, "obj", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(payload)
	if res.Size != 5000 || res.SHA256 != hex.EncodeToString(h[:]) {
		t.Fatalf("%+v", res)
	}
	boom := errors.New("tool failed")
	if _, err := PutStream(ctx, b, "broken", &failAfter{data: payload, err: boom}); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, err := b.Get(ctx, "broken"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial object committed: %v", err)
	}
	des, _ := os.ReadDir(dir)
	for _, d := range des {
		if strings.HasPrefix(d.Name(), ".tmp-") {
			t.Fatalf("leftover %s", filepath.Join(dir, d.Name()))
		}
	}
}

func TestPutStreamFailureNotCommittedMem(t *testing.T) {
	b := NewMemBackend()
	boom := errors.New("pipe closed")
	if _, err := PutStream(context.Background(), b, "x", &failAfter{data: []byte("abc"), err: boom}); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if objs, _ := b.List(context.Background(), ""); len(objs) != 0 {
		t.Fatalf("%v", objs)
	}
}

// dropReader yields first, then calls drop (killing the connection), then
// yields rest.
type dropReader struct {
	first, rest []byte
	drop        func()
	dropped     bool
}

func (d *dropReader) Read(p []byte) (int, error) {
	if len(d.first) > 0 {
		n := copy(p, d.first)
		d.first = d.first[n:]
		return n, nil
	}
	if !d.dropped {
		d.dropped = true
		d.drop()
		time.Sleep(50 * time.Millisecond)
	}
	if len(d.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, d.rest)
	d.rest = d.rest[n:]
	return n, nil
}

func TestSFTPPutStreamNotRetriedAfterPartialConsume(t *testing.T) {
	s := startSFTP(t)
	b, err := NewSFTPBackend(s.cfg())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	if err := b.Put(ctx, "warm", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	r := &dropReader{first: randBytes(t, 256<<10), rest: randBytes(t, 256<<10), drop: s.dropConns}
	// Without the no-retry rule the reconnect-once logic would resend the
	// already consumed reader (masked here only by the O_EXCL temp name).
	if _, err := PutStream(ctx, b, "big", r); err == nil || !errors.Is(err, errNoRetry) {
		t.Fatalf("upload over a dropped connection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "bk", "big")); err == nil {
		t.Fatal("a truncated object was committed")
	}
	// The backend recovers for the next upload.
	if _, err := PutStream(ctx, b, "after", strings.NewReader("ok")); err != nil {
		t.Fatal(err)
	}
}

func TestS3PutStreamMultipart(t *testing.T) {
	e := startFakeS3(t, false, func(c *S3Config) { c.PartSize = 1024 })
	key := mustKey(t)
	payload := randBytes(t, 4000)
	data, sum := sealStream(t, key, payload, 512)
	res, err := PutStream(context.Background(), e.be, "stream", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA256 != sum.CipherSHA256 || res.Size != sum.CipherSize {
		t.Fatalf("put result %+v differs from seal summary", res)
	}
	if e.multiPuts.Load() != 1 {
		t.Fatalf("multipart uploads %d", e.multiPuts.Load())
	}
	rc, err := e.be.Get(context.Background(), "stream")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	r, err := OpenStream(rc, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("stream differs: %v", err)
	}
	if err := CompareSummary(r.Summary(), sum); err != nil {
		t.Fatal(err)
	}
	// A failing source aborts the multipart upload and stores nothing.
	boom := errors.New("tool failed")
	if _, err := PutStream(context.Background(), e.be, "broken", &failAfter{data: data, err: boom}); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if e.stored("pre/broken") != nil || e.aborts.Load() != 1 {
		t.Fatalf("broken upload: stored=%v aborts=%d", e.stored("pre/broken") != nil, e.aborts.Load())
	}
}
