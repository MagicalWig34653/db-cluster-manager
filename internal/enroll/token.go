// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Token errors. They are distinct for the manager's audit trail; the
// enrollment endpoint maps them to generic responses.
var (
	// ErrTokenInvalid: unknown, malformed, wrong secret, revoked or expired.
	ErrTokenInvalid = errors.New("enroll: enrollment token invalid")
	// ErrTokenReplayed: this host already used the token.
	ErrTokenReplayed = errors.New("enroll: enrollment token already used by this host")
	// ErrHostNotAllowed: the token does not cover the host.
	ErrHostNotAllowed = errors.New("enroll: host not allowed by the enrollment token")
)

const tokenPrefix = "dbcm-et1"

// Token is the stored form of an enrollment token. The secret itself is
// never stored, only its SHA-256 digest.
type Token struct {
	ID           string    `json:"id"`
	Digest       string    `json:"digest"` // hex SHA-256 of the secret part
	Installation string    `json:"installation"`
	CreatedAt    time.Time `json:"createdAt"`
	ExpiresAt    time.Time `json:"expiresAt"`
	// AllowedHosts are the host (Swarm node) IDs that may enroll with it.
	AllowedHosts []string `json:"allowedHosts"`
	// Used maps a host ID to the time it consumed the token: each host may
	// use the token once.
	Used    map[string]time.Time `json:"used,omitempty"`
	Revoked bool                 `json:"revoked,omitempty"`
}

// NewToken creates a token record and its plain form
// "dbcm-et1.<id>.<secret>". Only the record is stored by the manager; the
// plain form goes into the Swarm secret.
func NewToken(installation string, hosts []string, now time.Time, ttl time.Duration) (Token, string, error) {
	if installation == "" || ttl <= 0 {
		return Token{}, "", errors.New("enroll: token needs an installation and a positive TTL")
	}
	var id [8]byte
	var secret [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Token{}, "", err
	}
	if _, err := rand.Read(secret[:]); err != nil {
		return Token{}, "", err
	}
	s := base64.RawURLEncoding.EncodeToString(secret[:])
	t := Token{ID: hex.EncodeToString(id[:]), Digest: digest(s), Installation: installation, CreatedAt: now,
		ExpiresAt: now.Add(ttl), AllowedHosts: slices.Clone(hosts)}
	return t, tokenPrefix + "." + t.ID + "." + s, nil
}

func digest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ParseToken splits a plain token into its ID and secret.
func ParseToken(plain string) (id, secret string, err error) {
	parts := strings.Split(strings.TrimSpace(plain), ".")
	if len(parts) != 3 || parts[0] != tokenPrefix || parts[1] == "" || len(parts[2]) < 32 {
		return "", "", fmt.Errorf("%w: malformed", ErrTokenInvalid)
	}
	return parts[1], parts[2], nil
}

// Consume checks secret for host at now and marks the token used by host.
// It mutates t only on success; the caller stores t with compare-and-set.
func (t *Token) Consume(secret, host string, now time.Time) error {
	if subtle.ConstantTimeCompare([]byte(digest(secret)), []byte(t.Digest)) != 1 {
		return fmt.Errorf("%w: secret mismatch", ErrTokenInvalid)
	}
	switch {
	case t.Revoked:
		return fmt.Errorf("%w: revoked", ErrTokenInvalid)
	case !now.Before(t.ExpiresAt):
		return fmt.Errorf("%w: expired at %s", ErrTokenInvalid, t.ExpiresAt.Format(time.RFC3339))
	case !slices.Contains(t.AllowedHosts, host):
		return fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
	}
	if at, used := t.Used[host]; used {
		return fmt.Errorf("%w: %s at %s", ErrTokenReplayed, host, at.Format(time.RFC3339))
	}
	if t.Used == nil {
		t.Used = map[string]time.Time{}
	}
	t.Used[host] = now
	return nil
}
