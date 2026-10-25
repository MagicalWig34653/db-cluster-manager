// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"time"
)

// Stream container layout (all integers big endian):
//
//	magic[8] | version u16 | metaLen u32 | meta JSON | wrapLen u32 | wrap JSON | chunk*
//	chunk = flags u8 | ctLen u32 | ciphertext (AES-256-GCM, tag included)
//
// Streams are for cluster data backups (ADR 0009). Unlike Seal, they never
// hold the whole payload in memory, and the writer needs only a public key:
// a scheduled backup job can encrypt without access to the recovery key.
//
// Each stream has a random 256-bit data key. It is wrapped for the
// recipient's X25519 public key (derived from the recovery key) with an
// ephemeral X25519 key, HKDF-SHA256 and AES-256-GCM. Chunk i uses the nonce
// 0x00000000 || i (u64) and the AAD sha256(header) || i (u64) || flags, so
// chunks cannot be reordered, dropped, moved between streams, or the stream
// cut short: the last chunk carries the final flag, and a reader that sees EOF
// without it reports ErrCorrupt.
const (
	streamMagic = "DBCMBKS\n"
	// StreamFormatVersion is the stream container version written by NewSealWriter.
	StreamFormatVersion = 1
	// WrapX25519 identifies the X25519 + HKDF-SHA256 + AES-256-GCM key wrap.
	WrapX25519 = "x25519-hkdf-sha256-aes256gcm"

	// DefaultChunkSize is the plaintext size of one chunk.
	DefaultChunkSize = 1 << 20
	// MaxChunkSize bounds the chunk size accepted by readers.
	MaxChunkSize = 16 << 20

	chunkFinal    = 0x01
	chunkOverhead = 16 // GCM tag

	streamKeyInfo  = "dbcm-backup-stream-x25519-v1"
	streamWrapInfo = "dbcm-backup-stream-wrap-v1"
	pubKeyPrefix   = "dbcmpk1:"
)

// StreamMeta is the authenticated, non-secret header of a stream.
type StreamMeta struct {
	Wrap      string    `json:"wrap"`
	ChunkSize int       `json:"chunk_size"`
	Created   time.Time `json:"created"`
	// Kind, BackupID, ClusterID and SourceNodeUUID describe the content; a
	// restore compares them with the manifest and the restore request.
	Kind           string `json:"kind"`
	BackupID       string `json:"backup_id"`
	ClusterID      string `json:"cluster_id,omitempty"`
	SourceNodeUUID string `json:"source_node_uuid,omitempty"`
	Database       string `json:"database,omitempty"`
	// Binding authenticates a digest of kind-specific manifest metadata.
	Binding string `json:"binding,omitempty"`
}

type streamWrap struct {
	KeyID        string `json:"key_id"`
	EphemeralPub []byte `json:"ephemeral_pub"`
	Nonce        []byte `json:"nonce"`
	WrappedKey   []byte `json:"wrapped_key"`
}

// StreamPublicKey is the public half derived from a RecoveryKey. It is not
// secret; the manager may store it and hand it to backup jobs.
type StreamPublicKey struct {
	key *ecdh.PublicKey
}

func streamPrivate(k RecoveryKey) (*ecdh.PrivateKey, error) {
	seed, err := hkdf.Key(sha256.New, k[:], nil, streamKeyInfo, 32)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(seed)
}

// StreamPublicKey derives the public key that encrypts streams for k.
func (k RecoveryKey) StreamPublicKey() (StreamPublicKey, error) {
	p, err := streamPrivate(k)
	if err != nil {
		return StreamPublicKey{}, fmt.Errorf("backup: derive stream key: %w", err)
	}
	return StreamPublicKey{key: p.PublicKey()}, nil
}

// ID is a non-secret fingerprint of the public key, recorded in each stream.
func (p StreamPublicKey) ID() string {
	if p.key == nil {
		return ""
	}
	h := sha256.Sum256(append([]byte("dbcm-stream-key-id\x00"), p.key.Bytes()...))
	return hex.EncodeToString(h[:8])
}

// String renders the key as "dbcmpk1:<base64url>".
func (p StreamPublicKey) String() string {
	if p.key == nil {
		return ""
	}
	return pubKeyPrefix + base64.RawURLEncoding.EncodeToString(p.key.Bytes())
}

// ParseStreamPublicKey parses the output of StreamPublicKey.String.
func ParseStreamPublicKey(s string) (StreamPublicKey, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), pubKeyPrefix)
	if !ok {
		return StreamPublicKey{}, ErrKeyFormat
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return StreamPublicKey{}, ErrKeyFormat
	}
	pk, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return StreamPublicKey{}, ErrKeyFormat
	}
	return StreamPublicKey{key: pk}, nil
}

// ChunkDigest is the integrity record of one plaintext chunk.
//
// Summary version 2 (written since ADR 0009 was amended) carries only MAC:
// HMAC-SHA256 over the chunk index and plaintext, keyed with a key derived
// from the stream's random data key (HKDF-SHA256, info chunkMACInfo). It
// reveals nothing about the plaintext to someone without the recovery key
// (no confirmation of known content, no equal-chunk detection across
// backups, since every stream has its own data key). Version 1 summaries
// (older manifests) carry the plain SHA-256 instead; readers compute both,
// so both stay verifiable on restore.
type ChunkDigest struct {
	Index  uint64 `json:"index"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	MAC    string `json:"mac,omitempty"`
}

// StreamSummary describes a sealed or opened stream. Plain* cover the
// plaintext (PlainMAC keyed like ChunkDigest.MAC, PlainSHA256 only in
// version 1 summaries and in reader summaries); Cipher* cover the bytes
// stored in the backend (header included), which can be checked without
// the key.
type StreamSummary struct {
	Chunks       []ChunkDigest `json:"chunks"`
	PlainSize    int64         `json:"plain_size"`
	PlainSHA256  string        `json:"plain_sha256,omitempty"`
	PlainMAC     string        `json:"plain_mac,omitempty"`
	CipherSize   int64         `json:"cipher_size"`
	CipherSHA256 string        `json:"cipher_sha256"`
	KeyID        string        `json:"key_id"`
}

// Keyed reports whether the summary uses keyed digests (version 2).
func (s StreamSummary) Keyed() bool { return s.PlainMAC != "" }

const chunkMACInfo = "dbcm-backup-stream-chunk-mac-v1"

func chunkMACKey(dataKey []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, dataKey, nil, chunkMACInfo, 32)
}

// chunkMAC is HMAC-SHA256(key, index u64 || plaintext).
func chunkMAC(key []byte, index uint64, pt []byte) string {
	m := hmac.New(sha256.New, key)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], index)
	m.Write(b[:])
	m.Write(pt)
	return hex.EncodeToString(m.Sum(nil))
}

// SealWriter encrypts a stream chunk by chunk. Close must be called to write
// the final chunk; without it the stream is unreadable (truncated).
type SealWriter struct {
	w   io.Writer
	cw  *countingHash
	gcm interface {
		Seal(dst, nonce, pt, aad []byte) []byte
	}
	hdrHash [32]byte
	buf     []byte
	size    int
	index   uint64
	plain   hash.Hash // HMAC of the whole plaintext
	macKey  []byte
	plainN  int64
	chunks  []ChunkDigest
	keyID   string
	closed  bool
	err     error
}

type countingHash struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (c *countingHash) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	return n, err
}

// NewSealWriter writes the stream header to w and returns a writer for the
// plaintext. meta.Wrap and a zero meta.ChunkSize are filled in.
func NewSealWriter(w io.Writer, meta StreamMeta, pub StreamPublicKey) (*SealWriter, error) {
	if pub.key == nil {
		return nil, errors.New("backup: stream public key required")
	}
	if meta.ChunkSize == 0 {
		meta.ChunkSize = DefaultChunkSize
	}
	if meta.ChunkSize < 1 || meta.ChunkSize > MaxChunkSize {
		return nil, fmt.Errorf("backup: chunk size %d out of range", meta.ChunkSize)
	}
	meta.Wrap = WrapX25519
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	aad := streamAAD(metaJSON)
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, err
	}
	wrap, err := wrapForPublic(dataKey, pub, aad)
	if err != nil {
		return nil, err
	}
	wj, err := json.Marshal(wrap)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	cw := &countingHash{w: w, h: sha256.New()}
	var hdr bytes.Buffer
	hdr.Write(aad)
	_ = binary.Write(&hdr, binary.BigEndian, uint32(len(wj)))
	hdr.Write(wj)
	if _, err := cw.Write(hdr.Bytes()); err != nil {
		return nil, err
	}
	macKey, plainMAC, err := streamMACs(dataKey)
	if err != nil {
		return nil, err
	}
	return &SealWriter{w: w, cw: cw, gcm: gcm, hdrHash: sha256.Sum256(aad), size: meta.ChunkSize,
		buf: make([]byte, 0, meta.ChunkSize), plain: plainMAC, macKey: macKey, keyID: pub.ID()}, nil
}

// streamMACs derives the chunk MAC key and the whole-plaintext MAC of a
// stream's data key.
func streamMACs(dataKey []byte) ([]byte, hash.Hash, error) {
	macKey, err := chunkMACKey(dataKey)
	if err != nil {
		return nil, nil, err
	}
	plainKey, err := hkdf.Key(sha256.New, dataKey, nil, chunkMACInfo+"-plain", 32)
	if err != nil {
		return nil, nil, err
	}
	return macKey, hmac.New(sha256.New, plainKey), nil
}

// Write implements io.Writer.
func (s *SealWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.closed {
		return 0, errors.New("backup: write to closed stream")
	}
	n := 0
	for len(p) > 0 {
		// A full buffer is only flushed as non-final once more data
		// arrives, so the final chunk is never an extra empty one unless
		// the stream is empty.
		if len(s.buf) == s.size {
			if err := s.flush(false); err != nil {
				return n, err
			}
		}
		k := min(len(p), s.size-len(s.buf))
		s.buf = append(s.buf, p[:k]...)
		p, n = p[k:], n+k
	}
	return n, nil
}

func (s *SealWriter) flush(final bool) error {
	var flags byte
	if final {
		flags = chunkFinal
	}
	nonce := chunkNonce(s.index)
	ct := s.gcm.Seal(nil, nonce, s.buf, chunkAAD(s.hdrHash, s.index, flags))
	var h [5]byte
	h[0] = flags
	binary.BigEndian.PutUint32(h[1:], uint32(len(ct)))
	if _, err := s.cw.Write(h[:]); err != nil {
		s.err = err
		return err
	}
	if _, err := s.cw.Write(ct); err != nil {
		s.err = err
		return err
	}
	s.chunks = append(s.chunks, ChunkDigest{Index: s.index, Size: len(s.buf), MAC: chunkMAC(s.macKey, s.index, s.buf)})
	s.plain.Write(s.buf)
	s.plainN += int64(len(s.buf))
	s.index++
	s.buf = s.buf[:0]
	return nil
}

// Close writes the final chunk. It does not close the underlying writer.
func (s *SealWriter) Close() error {
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

// Summary returns the integrity summary (version 2: keyed digests only);
// it is complete after Close.
func (s *SealWriter) Summary() StreamSummary {
	return StreamSummary{
		Chunks:    append([]ChunkDigest(nil), s.chunks...),
		PlainSize: s.plainN, PlainMAC: hex.EncodeToString(s.plain.Sum(nil)),
		CipherSize: s.cw.n, CipherSHA256: hex.EncodeToString(s.cw.h.Sum(nil)),
		KeyID: s.keyID,
	}
}

func chunkNonce(i uint64) []byte {
	n := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(n[4:], i)
	return n
}

func chunkAAD(hdr [32]byte, i uint64, flags byte) []byte {
	a := make([]byte, 0, 32+8+1)
	a = append(a, hdr[:]...)
	a = binary.BigEndian.AppendUint64(a, i)
	return append(a, flags)
}

func streamAAD(metaJSON []byte) []byte {
	var b bytes.Buffer
	b.WriteString(streamMagic)
	_ = binary.Write(&b, binary.BigEndian, uint16(StreamFormatVersion))
	_ = binary.Write(&b, binary.BigEndian, uint32(len(metaJSON)))
	b.Write(metaJSON)
	return b.Bytes()
}

func wrapKEK(shared, ephPub, recipPub []byte) ([]byte, error) {
	salt := append(append([]byte{}, ephPub...), recipPub...)
	return hkdf.Key(sha256.New, shared, salt, streamWrapInfo, 32)
}

func wrapForPublic(dataKey []byte, pub StreamPublicKey, aad []byte) (streamWrap, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return streamWrap{}, err
	}
	shared, err := eph.ECDH(pub.key)
	if err != nil {
		return streamWrap{}, err
	}
	kek, err := wrapKEK(shared, eph.PublicKey().Bytes(), pub.key.Bytes())
	if err != nil {
		return streamWrap{}, err
	}
	gcm, err := newGCM(kek)
	if err != nil {
		return streamWrap{}, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return streamWrap{}, err
	}
	return streamWrap{KeyID: pub.ID(), EphemeralPub: eph.PublicKey().Bytes(), Nonce: nonce,
		WrappedKey: gcm.Seal(nil, nonce, dataKey, aad)}, nil
}

// StreamReader decrypts a stream sequentially and records chunk digests.
type StreamReader struct {
	r   io.Reader
	tee *countingHash
	gcm interface {
		Open(dst, nonce, ct, aad []byte) ([]byte, error)
	}
	hdrHash  [32]byte
	meta     StreamMeta
	maxCT    int
	index    uint64
	pending  []byte
	final    bool
	err      error
	plain    hash.Hash
	plainMAC hash.Hash
	macKey   []byte
	plainN   int64
	chunks   []ChunkDigest
	keyID    string
}

type hashOnly struct{}

func (hashOnly) Write(p []byte) (int, error) { return len(p), nil }

// ReadStreamMeta parses the header of a stream without a key. The content is
// not authenticated.
func ReadStreamMeta(r io.Reader) (StreamMeta, string, error) {
	meta, _, wrap, err := readStreamHeader(r)
	return meta, wrap.KeyID, err
}

func readStreamHeader(r io.Reader) (StreamMeta, []byte, streamWrap, error) {
	var m StreamMeta
	var w streamWrap
	head := make([]byte, len(streamMagic)+2)
	if _, err := io.ReadFull(r, head); err != nil {
		return m, nil, w, ErrNotBackup
	}
	if string(head[:len(streamMagic)]) != streamMagic {
		return m, nil, w, ErrNotBackup
	}
	if v := binary.BigEndian.Uint16(head[len(streamMagic):]); v != StreamFormatVersion {
		return m, nil, w, fmt.Errorf("%w: %d", ErrVersion, v)
	}
	metaJSON, err := readSection(r)
	if err != nil {
		return m, nil, w, err
	}
	wrapJSON, err := readSection(r)
	if err != nil {
		return m, nil, w, err
	}
	if err := json.Unmarshal(metaJSON, &m); err != nil {
		return m, nil, w, ErrCorrupt
	}
	if m.Wrap != WrapX25519 {
		return m, nil, w, ErrAlgorithm
	}
	if m.ChunkSize < 1 || m.ChunkSize > MaxChunkSize {
		return m, nil, w, ErrCorrupt
	}
	if err := json.Unmarshal(wrapJSON, &w); err != nil {
		return m, nil, w, ErrCorrupt
	}
	return m, streamAAD(metaJSON), w, nil
}

func readSection(r io.Reader) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > maxSection {
		return nil, ErrCorrupt
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, ErrCorrupt
	}
	return b, nil
}

// OpenStream reads the header and unwraps the data key with the recovery key.
func OpenStream(r io.Reader, key RecoveryKey) (*StreamReader, error) {
	tee := &countingHash{w: hashOnly{}, h: sha256.New()}
	tr := io.TeeReader(r, tee)
	meta, aad, wrap, err := readStreamHeader(tr)
	if err != nil {
		return nil, err
	}
	priv, err := streamPrivate(key)
	if err != nil {
		return nil, err
	}
	pub := StreamPublicKey{key: priv.PublicKey()}
	if wrap.KeyID != pub.ID() {
		return nil, ErrDecrypt
	}
	if len(wrap.Nonce) != nonceSize {
		return nil, ErrCorrupt
	}
	eph, err := ecdh.X25519().NewPublicKey(wrap.EphemeralPub)
	if err != nil {
		return nil, ErrCorrupt
	}
	shared, err := priv.ECDH(eph)
	if err != nil {
		return nil, ErrDecrypt
	}
	kek, err := wrapKEK(shared, wrap.EphemeralPub, pub.key.Bytes())
	if err != nil {
		return nil, err
	}
	kg, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	dataKey, err := kg.Open(nil, wrap.Nonce, wrap.WrappedKey, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	gcm, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	macKey, plainMAC, err := streamMACs(dataKey)
	if err != nil {
		return nil, err
	}
	return &StreamReader{r: tr, tee: tee, gcm: gcm, hdrHash: sha256.Sum256(aad), meta: meta,
		maxCT: meta.ChunkSize + chunkOverhead, plain: sha256.New(), plainMAC: plainMAC, macKey: macKey, keyID: wrap.KeyID}, nil
}

// Meta returns the authenticated header. It is authenticated as soon as the
// first chunk has been decrypted (the header hash is in every chunk's AAD).
func (s *StreamReader) Meta() StreamMeta { return s.meta }

// Read implements io.Reader. It returns ErrCorrupt for a truncated stream or
// trailing data, and ErrDecrypt for a tampered chunk.
func (s *StreamReader) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		if s.final {
			// Anything after the final chunk is tampering.
			var one [1]byte
			if n, _ := io.ReadFull(s.r, one[:]); n > 0 {
				s.err = fmt.Errorf("%w: data after final chunk", ErrCorrupt)
				return 0, s.err
			}
			s.err = io.EOF
			return 0, io.EOF
		}
		s.next()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *StreamReader) next() {
	var h [5]byte
	if _, err := io.ReadFull(s.r, h[:]); err != nil {
		s.err = fmt.Errorf("%w: stream ends before the final chunk", ErrCorrupt)
		return
	}
	flags := h[0]
	if flags&^chunkFinal != 0 {
		s.err = ErrCorrupt
		return
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n < chunkOverhead || int(n) > s.maxCT {
		s.err = ErrCorrupt
		return
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(s.r, ct); err != nil {
		s.err = fmt.Errorf("%w: truncated chunk", ErrCorrupt)
		return
	}
	pt, err := s.gcm.Open(nil, chunkNonce(s.index), ct, chunkAAD(s.hdrHash, s.index, flags))
	if err != nil {
		s.err = ErrDecrypt
		return
	}
	if flags&chunkFinal == 0 && len(pt) != s.meta.ChunkSize {
		s.err = fmt.Errorf("%w: short non-final chunk", ErrCorrupt)
		return
	}
	d := sha256.Sum256(pt)
	s.chunks = append(s.chunks, ChunkDigest{Index: s.index, Size: len(pt), SHA256: hex.EncodeToString(d[:]),
		MAC: chunkMAC(s.macKey, s.index, pt)})
	s.plain.Write(pt)
	s.plainMAC.Write(pt)
	s.plainN += int64(len(pt))
	s.index++
	s.final = flags&chunkFinal != 0
	s.pending = pt
}

// Summary returns the digests of the chunks read so far; after io.EOF it
// describes the whole stream. It carries both the keyed digests and the
// plain SHA-256 values, so it can be compared with version 1 and version 2
// summaries; it must not be stored where the plain digests matter.
func (s *StreamReader) Summary() StreamSummary {
	return StreamSummary{
		Chunks:    append([]ChunkDigest(nil), s.chunks...),
		PlainSize: s.plainN, PlainSHA256: hex.EncodeToString(s.plain.Sum(nil)), PlainMAC: hex.EncodeToString(s.plainMAC.Sum(nil)),
		CipherSize: s.tee.n, CipherSHA256: hex.EncodeToString(s.tee.h.Sum(nil)),
		KeyID: s.keyID,
	}
}

// ErrIntegrity reports a stream whose digests differ from the expected summary.
var ErrIntegrity = errors.New("backup: stream digests differ from the manifest")

// CompareSummary checks a summary obtained while reading against the
// expected one (from the manifest). A keyed (version 2) expectation is
// compared on the MACs, a version 1 expectation on the plain SHA-256
// values; got must carry what want uses (a StreamReader summary carries
// both).
func CompareSummary(got, want StreamSummary) error {
	keyed := want.Keyed()
	switch {
	case got.KeyID != want.KeyID:
		return fmt.Errorf("%w: key id %s, manifest %s", ErrIntegrity, got.KeyID, want.KeyID)
	case got.PlainSize != want.PlainSize:
		return fmt.Errorf("%w: plaintext size", ErrIntegrity)
	case keyed && !hmac.Equal([]byte(got.PlainMAC), []byte(want.PlainMAC)):
		return fmt.Errorf("%w: plaintext mac", ErrIntegrity)
	case !keyed && (want.PlainSHA256 == "" || got.PlainSHA256 != want.PlainSHA256):
		return fmt.Errorf("%w: plaintext sha256", ErrIntegrity)
	case got.CipherSize != want.CipherSize || got.CipherSHA256 != want.CipherSHA256:
		return fmt.Errorf("%w: stored size/sha256", ErrIntegrity)
	case len(got.Chunks) != len(want.Chunks):
		return fmt.Errorf("%w: %d chunks, manifest %d", ErrIntegrity, len(got.Chunks), len(want.Chunks))
	}
	for i := range got.Chunks {
		g, w := got.Chunks[i], want.Chunks[i]
		ok := g.Index == w.Index && g.Size == w.Size
		if keyed {
			ok = ok && w.MAC != "" && g.MAC == w.MAC
		} else {
			ok = ok && w.SHA256 != "" && g.SHA256 == w.SHA256
		}
		if !ok {
			return fmt.Errorf("%w: chunk %d", ErrIntegrity, i)
		}
	}
	return nil
}

// PutResult describes what PutStream stored.
type PutResult struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PutStream stores r under name without buffering it beyond what the
// backend needs (DirBackend and SFTPBackend copy, S3Backend buffers one
// multipart part), and returns the size and SHA-256 of the bytes consumed.
// If r returns an error, the backend's Put fails and nothing is committed
// under name. A stream is never retried after bytes were consumed.
func PutStream(ctx context.Context, b Backend, name string, r io.Reader) (PutResult, error) {
	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(r, h)}
	if err := b.Put(ctx, name, cr); err != nil {
		return PutResult{}, err
	}
	return PutResult{Size: cr.n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
