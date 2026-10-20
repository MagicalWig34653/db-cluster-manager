// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"crypto/tls"
	"errors"
	"sync/atomic"
)

// CertSource supplies the client-facing certificate. It is consulted for
// every TLS handshake, so replacing the certificate (renewal, reload) takes
// effect for new connections without touching established ones.
type CertSource interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

// CertFunc adapts a provider callback to CertSource.
type CertFunc func(*tls.ClientHelloInfo) (*tls.Certificate, error)

// GetCertificate calls f.
func (f CertFunc) GetCertificate(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return f(h) }

// AtomicCertSource holds a certificate that can be swapped at any time, for
// example with the result of internal/certs Issued.TLSCertificate after a
// renewal.
type AtomicCertSource struct {
	v atomic.Pointer[tls.Certificate]
}

// Set replaces the certificate for subsequent handshakes.
func (s *AtomicCertSource) Set(c tls.Certificate) { s.v.Store(&c) }

// GetCertificate returns the current certificate.
func (s *AtomicCertSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := s.v.Load()
	if c == nil {
		return nil, errors.New("no certificate loaded")
	}
	return c, nil
}

// FileCertSource loads a PEM certificate chain and key from files. Reload
// re-reads them; on failure the previous certificate stays in use.
type FileCertSource struct {
	AtomicCertSource
	certFile, keyFile string
}

// NewFileCertSource loads the files once.
func NewFileCertSource(certFile, keyFile string) (*FileCertSource, error) {
	s := &FileCertSource{certFile: certFile, keyFile: keyFile}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads the files.
func (s *FileCertSource) Reload() error {
	c, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return err
	}
	s.Set(c)
	return nil
}

func serverTLSConfig(src CertSource) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: src.GetCertificate,
	}
}
