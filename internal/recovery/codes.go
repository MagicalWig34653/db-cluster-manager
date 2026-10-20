// SPDX-License-Identifier: AGPL-3.0-only

package recovery

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Recovery code format: 16 characters of RFC 4648 base32 (80 random bits),
// shown as four dash-separated groups, e.g. "ABCD-EFGH-IJKL-MNOP". Input is
// normalized (case, spaces, dashes) before verification.
const (
	codeRandomBytes = 10 // 80 bits
	codeChars       = 16
	saltBytes       = 16
	hashBytes       = 32
)

// DefaultCodeCount is the number of codes generated per set.
const DefaultCodeCount = 10

// KDF choice: PBKDF2-HMAC-SHA256 from the standard library (crypto/pbkdf2,
// FIPS-approved, no external dependency) with a random 16-byte salt per code.
//
// Recovery codes are machine-generated with 80 bits of entropy, unlike
// user-chosen passwords, so offline guessing against a stolen hash is
// infeasible even with a fast hash; the iteration count is defence in depth,
// not the primary protection. Verification must hash the input against every
// unused code of the user (codes carry no lookup identifier), so the cost is
// multiplied by the set size. DefaultCodeIterations keeps a full 10-code
// verification well below a second (measured: 10 x 20k iterations took about
// 50 ms on an Apple-silicon development Mac, so 100k is about 0.25 s). Online
// guessing is bounded by the Limiter.
// Argon2id (golang.org/x/crypto) would be preferable for user-chosen
// passwords; that decision belongs to the password credential, not here.
const DefaultCodeIterations = 100_000

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

var (
	// ErrInvalidCode is returned for a wrong, malformed or already used code.
	ErrInvalidCode = errors.New("recovery: invalid recovery code")
	// ErrNoCodes is returned when the user has no enrolled code set.
	ErrNoCodes = errors.New("recovery: no recovery codes enrolled")
)

// HashedCode is the stored form of one code. It contains no plaintext.
type HashedCode struct {
	Salt       []byte    `json:"salt"`
	Hash       []byte    `json:"hash"`
	Iterations int       `json:"iterations"`
	UsedAt     time.Time `json:"usedAt"` // zero while unused
}

// CodeSet is the stored set of a user's recovery codes.
type CodeSet struct {
	CreatedAt time.Time    `json:"createdAt"`
	Codes     []HashedCode `json:"codes"`
}

// Remaining returns the number of unused codes.
func (s CodeSet) Remaining() int {
	n := 0
	for _, c := range s.Codes {
		if c.UsedAt.IsZero() {
			n++
		}
	}
	return n
}

func deriveCode(normalized string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, normalized, salt, iter, hashBytes)
}

// NormalizeCode removes separators and whitespace and upper-cases the input.
func NormalizeCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r == '-' || r == ' ' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func formatCode(raw string) string {
	return raw[0:4] + "-" + raw[4:8] + "-" + raw[8:12] + "-" + raw[12:16]
}

// GenerateCodes creates n codes with crypto/rand. It returns the plaintext
// codes, to be shown to the user exactly once, and the hashed set to store.
// iterations <= 0 selects DefaultCodeIterations.
func GenerateCodes(n, iterations int, now time.Time) ([]string, CodeSet, error) {
	if n <= 0 {
		n = DefaultCodeCount
	}
	if iterations <= 0 {
		iterations = DefaultCodeIterations
	}
	plain := make([]string, 0, n)
	set := CodeSet{CreatedAt: now.UTC(), Codes: make([]HashedCode, 0, n)}
	for range n {
		buf := make([]byte, codeRandomBytes)
		if _, err := rand.Read(buf); err != nil {
			return nil, CodeSet{}, err
		}
		raw := b32.EncodeToString(buf)
		salt := make([]byte, saltBytes)
		if _, err := rand.Read(salt); err != nil {
			return nil, CodeSet{}, err
		}
		h, err := deriveCode(raw, salt, iterations)
		if err != nil {
			return nil, CodeSet{}, err
		}
		plain = append(plain, formatCode(raw))
		set.Codes = append(set.Codes, HashedCode{Salt: salt, Hash: h, Iterations: iterations})
	}
	return plain, set, nil
}

// consume checks code against every code in the set, without stopping at a
// match, and marks the matching code used. A match on an already used code is
// rejected (replay).
func (s *CodeSet) consume(code string, now time.Time) error {
	norm := NormalizeCode(code)
	if len(norm) != codeChars {
		// Hash anyway so malformed input costs the same as a wrong code.
		norm = strings.Repeat("A", codeChars) + "!"
	}
	match := -1
	for i, c := range s.Codes {
		h, err := deriveCode(norm, c.Salt, c.Iterations)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(h, c.Hash) == 1 {
			match = i
		}
	}
	if match < 0 || !s.Codes[match].UsedAt.IsZero() {
		return ErrInvalidCode
	}
	s.Codes[match].UsedAt = now.UTC()
	return nil
}

type codeEntry struct {
	mu  sync.Mutex
	set CodeSet
}

// CodeVerifier holds users' code sets and verifies codes under a Limiter.
// Verification of one user is serialized, so a code cannot be consumed twice
// by concurrent requests.
type CodeVerifier struct {
	mu      sync.Mutex
	entries map[string]*codeEntry
	limiter *Limiter
	now     Clock
}

// NewCodeVerifier returns a verifier. now may be nil for time.Now.
func NewCodeVerifier(l *Limiter, now Clock) *CodeVerifier {
	if now == nil {
		now = time.Now
	}
	return &CodeVerifier{entries: make(map[string]*codeEntry), limiter: l, now: now}
}

func limiterKey(userID string) string { return "recovery-code:" + userID }

func (v *CodeVerifier) entry(userID string, create bool) *codeEntry {
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.entries[userID]
	if e == nil && create {
		e = &codeEntry{}
		v.entries[userID] = e
	}
	return e
}

// Enroll stores (or replaces) a user's code set. Replacing invalidates every
// previous code. The caller must also advance the account's credential epoch.
func (v *CodeVerifier) Enroll(userID string, set CodeSet) {
	e := v.entry(userID, true)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.set = CodeSet{CreatedAt: set.CreatedAt, Codes: append([]HashedCode(nil), set.Codes...)}
}

// Revoke removes a user's code set.
func (v *CodeVerifier) Revoke(userID string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.entries, userID)
}

// Remaining returns the number of unused codes of a user.
func (v *CodeVerifier) Remaining(userID string) int {
	e := v.entry(userID, false)
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set.Remaining()
}

// Snapshot returns a copy of a user's stored set, for persistence.
func (v *CodeVerifier) Snapshot(userID string) (CodeSet, bool) {
	e := v.entry(userID, false)
	if e == nil {
		return CodeSet{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return CodeSet{CreatedAt: e.set.CreatedAt, Codes: append([]HashedCode(nil), e.set.Codes...)}, true
}

// Verify consumes a recovery code. It returns a *LockedError while the user
// is locked out, ErrNoCodes without an enrolled set and ErrInvalidCode for a
// wrong or reused code. Callers should present all three identically to the
// client. A success resets the user's limiter state.
func (v *CodeVerifier) Verify(userID, code string) error {
	key := limiterKey(userID)
	if err := v.limiter.Begin(key); err != nil {
		return err
	}
	e := v.entry(userID, false)
	if e == nil {
		return ErrNoCodes
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.set.Codes) == 0 {
		return ErrNoCodes
	}
	if err := e.set.consume(code, v.now()); err != nil {
		if errors.Is(err, ErrInvalidCode) {
			return err
		}
		return fmt.Errorf("recovery: verify code: %w", err)
	}
	v.limiter.Succeed(key)
	return nil
}
