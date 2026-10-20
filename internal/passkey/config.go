// SPDX-License-Identifier: AGPL-3.0-only

package passkey

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidConfig is returned for a relying-party configuration that
// violates the rules in Config.Validate.
var ErrInvalidConfig = errors.New("passkey: invalid relying-party configuration")

// DefaultChallengeTTL is the proposed lifetime of a pending ceremony.
const DefaultChallengeTTL = 5 * time.Minute

// DefaultMaxPendingCeremonies bounds the memory used by unfinished ceremonies.
const DefaultMaxPendingCeremonies = 10_000

// Config is the relying-party configuration.
//
// RPID is the relying-party identity that every passkey is scoped to by the
// authenticator. It must stay stable across manager recovery, failover and
// address changes: a credential registered under one RP ID can never be used
// under another, so changing it orphans every registered passkey and forces
// recovery for every account. Choose a DNS name the operator controls long
// term (for example the manager's service name), not an IP address or a
// per-node host name.
type Config struct {
	// RPID is a registrable DNS name such as "db.example.com", or
	// "localhost" for local development. No scheme, port or path.
	RPID string
	// RPDisplayName is shown by authenticators; defaults to RPID.
	RPDisplayName string
	// Origins are the exact web origins (scheme://host[:port]) the UI is
	// served from. Each must be https, except http://localhost[:port]; each
	// host must equal RPID or be a subdomain of it.
	Origins []string
	// ChallengeTTL limits how long a ceremony may stay pending. Zero means
	// DefaultChallengeTTL.
	ChallengeTTL time.Duration
	// MaxPendingCeremonies limits unfinished ceremonies. Zero means
	// DefaultMaxPendingCeremonies.
	MaxPendingCeremonies int
}

// normalized returns a copy with defaults applied and origins in their
// canonical serialization, or an error if the configuration is invalid.
func (c Config) normalized() (Config, error) {
	rpID, err := validateRPID(c.RPID)
	if err != nil {
		return Config{}, err
	}
	out := c
	out.RPID = rpID
	if out.RPDisplayName == "" {
		out.RPDisplayName = rpID
	}
	if out.ChallengeTTL == 0 {
		out.ChallengeTTL = DefaultChallengeTTL
	}
	if out.ChallengeTTL < 0 {
		return Config{}, fmt.Errorf("%w: negative challenge TTL", ErrInvalidConfig)
	}
	if out.MaxPendingCeremonies == 0 {
		out.MaxPendingCeremonies = DefaultMaxPendingCeremonies
	}
	if out.MaxPendingCeremonies < 0 {
		return Config{}, fmt.Errorf("%w: negative ceremony limit", ErrInvalidConfig)
	}
	if len(c.Origins) == 0 {
		return Config{}, fmt.Errorf("%w: no allowed origins", ErrInvalidConfig)
	}
	out.Origins = nil
	seen := map[string]bool{}
	for _, o := range c.Origins {
		n, err := validateOrigin(o, rpID)
		if err != nil {
			return Config{}, err
		}
		if !seen[n] {
			seen[n] = true
			out.Origins = append(out.Origins, n)
		}
	}
	return out, nil
}

// Validate checks the configuration without modifying it.
func (c Config) Validate() error {
	_, err := c.normalized()
	return err
}

// validateRPID accepts a lower-case DNS name of letters, digits and hyphens.
// IP addresses are rejected because WebAuthn requires a domain. Whether the
// name is a public suffix (for example "co.uk") is not checked; browsers
// reject such RP IDs at ceremony time.
func validateRPID(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: empty RP ID", ErrInvalidConfig)
	}
	if id != strings.ToLower(id) {
		return "", fmt.Errorf("%w: RP ID %q must be lower case", ErrInvalidConfig, id)
	}
	if len(id) > 253 || strings.HasSuffix(id, ".") {
		return "", fmt.Errorf("%w: RP ID %q is not a valid domain", ErrInvalidConfig, id)
	}
	if net.ParseIP(id) != nil {
		return "", fmt.Errorf("%w: RP ID %q is an IP address", ErrInvalidConfig, id)
	}
	labels := strings.Split(id, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", fmt.Errorf("%w: RP ID %q is not a valid domain", ErrInvalidConfig, id)
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", fmt.Errorf("%w: RP ID %q contains %q", ErrInvalidConfig, id, r)
			}
		}
	}
	if _, err := strconv.Atoi(labels[len(labels)-1]); err == nil {
		return "", fmt.Errorf("%w: RP ID %q has a numeric top-level label", ErrInvalidConfig, id)
	}
	return id, nil
}

// validateOrigin returns the canonical serialization of an origin
// (lower-case scheme and host, default port omitted).
func validateOrigin(raw, rpID string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: origin %q: %v", ErrInvalidConfig, raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery ||
		(u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%w: origin %q must be scheme://host[:port] only", ErrInvalidConfig, raw)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: origin %q has no host", ErrInvalidConfig, raw)
	}
	switch scheme {
	case "https":
	case "http":
		if host != "localhost" {
			return "", fmt.Errorf("%w: origin %q: http is only allowed for localhost", ErrInvalidConfig, raw)
		}
	default:
		return "", fmt.Errorf("%w: origin %q: scheme must be https", ErrInvalidConfig, raw)
	}
	if host != rpID && !strings.HasSuffix(host, "."+rpID) {
		return "", fmt.Errorf("%w: RP ID %q is not a registrable suffix of origin host %q", ErrInvalidConfig, rpID, host)
	}
	port := u.Port()
	if port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return "", fmt.Errorf("%w: origin %q has an invalid port", ErrInvalidConfig, raw)
		}
		if (scheme == "https" && p == 443) || (scheme == "http" && p == 80) {
			port = ""
		} else {
			port = strconv.Itoa(p)
		}
	}
	if port == "" {
		return scheme + "://" + host, nil
	}
	return scheme + "://" + host + ":" + port, nil
}
