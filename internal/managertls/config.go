// SPDX-License-Identifier: AGPL-3.0-only

// Package managertls manages the public manager listener's ACME certificates.
package managertls

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"strings"
)

const StagingDirectory = "https://acme-staging-v02.api.letsencrypt.org/directory"

// Config is manager.tls.acme. Presence enables ACME and accepts the CA terms.
type Config struct {
	DirectoryURL string   `json:"directoryUrl"`
	Email        string   `json:"email"`
	Domains      []string `json:"domains"`
	Challenge    string   `json:"challenge"`
	HTTPListen   string   `json:"httpListen"`
}

// Defaults never selects the production CA implicitly.
func (c Config) Defaults() Config {
	if c.DirectoryURL == "" {
		c.DirectoryURL = StagingDirectory
	}
	if c.Challenge == "" {
		c.Challenge = "tls-alpn-01"
	}
	if c.HTTPListen == "" {
		c.HTTPListen = ":80"
	}
	c.Domains = append([]string(nil), c.Domains...)
	return c
}

func (c Config) Validate() error {
	c = c.Defaults()
	u, err := url.Parse(c.DirectoryURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("ACME directoryUrl must be an absolute HTTPS URL")
	}
	a, err := mail.ParseAddress(c.Email)
	if err != nil || a.Address != c.Email {
		return errors.New("ACME email must be a plain email address")
	}
	if len(c.Domains) == 0 {
		return errors.New("ACME domains are required")
	}
	seen := map[string]bool{}
	for _, d := range c.Domains {
		if len(d) > 253 || !strings.Contains(d, ".") || net.ParseIP(d) != nil || d != strings.ToLower(d) || seen[d] {
			return fmt.Errorf("invalid or duplicate ACME domain %q", d)
		}
		for _, label := range strings.Split(d, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return fmt.Errorf("invalid ACME domain %q", d)
			}
			for _, r := range label {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					return fmt.Errorf("invalid ACME domain %q (use ASCII/punycode; wildcards require DNS-01)", d)
				}
			}
		}
		seen[d] = true
	}
	if c.Challenge != "http-01" && c.Challenge != "tls-alpn-01" {
		return errors.New("ACME challenge must be http-01 or tls-alpn-01")
	}
	if _, _, err := net.SplitHostPort(c.HTTPListen); err != nil {
		return fmt.Errorf("ACME httpListen: %w", err)
	}
	return nil
}
