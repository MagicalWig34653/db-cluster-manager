// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/MagicalWig34653/db-cluster-manager/internal/managertls"
)

// tlsConfig configures only the public API/UI listener, not peer/agent mTLS.
type tlsConfig struct {
	ACME           *managertls.Config `json:"acme,omitempty"`
	CertFile       string             `json:"certFile,omitempty"`
	KeyFile        string             `json:"keyFile,omitempty"`
	AllowPlainHTTP bool               `json:"allowPlainHttp"`
}

func (c *tlsConfig) validate() error {
	if c == nil {
		return nil
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("manager.tls requires both certFile and keyFile")
	}
	if c.ACME != nil {
		if c.CertFile != "" || c.AllowPlainHTTP {
			return errors.New("manager.tls.acme excludes static certificates and allowPlainHttp")
		}
		return c.ACME.Validate()
	}
	if c.CertFile != "" && c.AllowPlainHTTP {
		return errors.New("manager.tls certificates exclude allowPlainHttp")
	}
	return nil
}
func listenerTLS(cfg fileConfig) *tlsConfig {
	if cfg.Manager == nil {
		return nil
	}
	return cfg.Manager.TLS
}

// Validate before opening the metastore or any listener. Hostnames are refused
// because DNS changes could turn a supposedly local HTTP bind into exposure.
func validateListener(addr string, c *tlsConfig) error {
	if err := c.validate(); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("manager listen: %w", err)
	}
	if c != nil && (c.ACME != nil || c.CertFile != "" || c.AllowPlainHTTP) {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("plain HTTP beyond loopback requires explicit manager.tls.allowPlainHttp or TLS")
	}
	return nil
}
func staticTLS(c *tlsConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}
