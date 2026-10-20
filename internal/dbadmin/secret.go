// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Secret wraps a credential so it cannot leak through formatting, JSON or
// structured logging by accident.
type Secret struct{ v string }

// NewSecret wraps v.
func NewSecret(v string) Secret { return Secret{v: v} }

// Reveal returns the plaintext. Call it only where the value is consumed.
func (s Secret) Reveal() string { return s.v }

const redacted = "[redacted]"

func (s Secret) String() string               { return redacted }
func (s Secret) GoString() string             { return redacted }
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (s Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }

// PasswordOptions configures GeneratePassword.
type PasswordOptions struct {
	Length  int       // default 24, allowed 12..128
	Charset string    // default DefaultPasswordCharset
	Rand    io.Reader // default crypto/rand.Reader
}

const (
	lower = "abcdefghijklmnopqrstuvwxyz"
	upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digit = "0123456789"
	// DefaultPasswordCharset is alphanumeric plus a few symbols that need no
	// escaping in SQL, option files or shells.
	DefaultPasswordCharset = lower + upper + digit + "-_.+=@"
	// passwordSymbols are the printable characters accepted in passwords.
	passwordSymbols = "!#$%&()*+,-./:;<=>?@[]^_{|}~"
)

func allowedPasswordByte(c byte) bool {
	return strings.IndexByte(lower+upper+digit+passwordSymbols, c) >= 0
}

// ValidatePassword rejects passwords that would need quoting or that contain
// characters with special meaning in SQL strings or option files.
func ValidatePassword(p string) error {
	if len(p) < 8 || len(p) > 128 {
		return invalid("password", "length must be 8..128")
	}
	for i := 0; i < len(p); i++ {
		if !allowedPasswordByte(p[i]) {
			return invalid("password", "contains a character outside the allowed set at position %d", i)
		}
	}
	return nil
}

// GeneratePassword returns a random password using unbiased rejection
// sampling. When the charset offers lower/upper/digit classes, each of them
// appears at least once.
func GeneratePassword(o PasswordOptions) (Secret, error) {
	if o.Length == 0 {
		o.Length = 24
	}
	if o.Length < 12 || o.Length > 128 {
		return Secret{}, invalid("password length", "must be 12..128, got %d", o.Length)
	}
	if o.Charset == "" {
		o.Charset = DefaultPasswordCharset
	}
	if o.Rand == nil {
		o.Rand = rand.Reader
	}
	seen := map[byte]bool{}
	for i := 0; i < len(o.Charset); i++ {
		c := o.Charset[i]
		if !allowedPasswordByte(c) {
			return Secret{}, invalid("password charset", "contains a disallowed character at position %d", i)
		}
		if seen[c] {
			return Secret{}, invalid("password charset", "contains duplicate characters")
		}
		seen[c] = true
	}
	if len(o.Charset) < 8 {
		return Secret{}, invalid("password charset", "must contain at least 8 characters")
	}
	classes := []string{}
	for _, cl := range []string{lower, upper, digit} {
		if strings.ContainsAny(o.Charset, cl) {
			classes = append(classes, cl)
		}
	}
	n := len(o.Charset)
	limit := 256 - 256%n
	for attempt := 0; attempt < 100; attempt++ {
		out := make([]byte, 0, o.Length)
		buf := make([]byte, o.Length*2)
		for len(out) < o.Length {
			if _, err := io.ReadFull(o.Rand, buf); err != nil {
				return Secret{}, fmt.Errorf("dbadmin: random source: %w", err)
			}
			for _, b := range buf {
				if int(b) >= limit {
					continue
				}
				out = append(out, o.Charset[int(b)%n])
				if len(out) == o.Length {
					break
				}
			}
		}
		ok := true
		for _, cl := range classes {
			if !strings.ContainsAny(string(out), cl) {
				ok = false
				break
			}
		}
		if ok {
			return NewSecret(string(out)), nil
		}
	}
	return Secret{}, fmt.Errorf("dbadmin: could not generate password satisfying character classes")
}
