// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Kind selects the key usages of an issued leaf certificate.
type Kind string

const (
	// KindServer certificates carry the serverAuth extended key usage.
	KindServer Kind = "server"
	// KindClient certificates carry the clientAuth extended key usage (mTLS).
	KindClient Kind = "client"
)

const clockSkew = time.Minute

// Request describes a leaf certificate to issue.
type Request struct {
	Kind       Kind
	CommonName string
	DNSNames   []string
	IPs        []net.IP
	// Validity of the leaf; capped at the CA's NotAfter.
	Validity time.Duration
}

// Names returns all SAN identities as strings (DNS names, then IPs).
func (r Request) Names() []string {
	out := append([]string(nil), r.DNSNames...)
	for _, ip := range r.IPs {
		out = append(out, ip.String())
	}
	return out
}

// CAOptions configures CA creation.
type CAOptions struct {
	CommonName string
	Validity   time.Duration
	// PermittedDNSDomains and PermittedIPRanges are optional X.509 name
	// constraints; issuing outside them fails.
	PermittedDNSDomains []string
	PermittedIPRanges   []*net.IPNet
	// Now is the injected clock; time.Now when nil.
	Now func() time.Time
}

// CA is a manager-operated internal certificate authority (ECDSA P-256).
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
	now     func() time.Time
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// NewCA creates a self-signed CA.
func NewCA(opts CAOptions) (*CA, error) {
	if opts.Validity <= 0 {
		return nil, errors.New("certs: CA validity must be positive")
	}
	if opts.CommonName == "" {
		opts.CommonName = "db-cluster-manager internal CA"
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	t := now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: opts.CommonName},
		NotBefore:             t.Add(-clockSkew),
		NotAfter:              t.Add(opts.Validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		PermittedDNSDomains:   opts.PermittedDNSDomains,
		PermittedIPRanges:     opts.PermittedIPRanges,
	}
	if len(opts.PermittedDNSDomains) > 0 || len(opts.PermittedIPRanges) > 0 {
		tmpl.PermittedDNSDomainsCritical = true
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return newCA(der, key, now)
}

func newCA(der []byte, key *ecdsa.PrivateKey, now func() time.Time) (*CA, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert: cert, key: key, now: now,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// Certificate returns the CA certificate.
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// CertificatePEM returns the public CA certificate in PEM form.
func (c *CA) CertificatePEM() []byte { return append([]byte(nil), c.certPEM...) }

// Pool returns a trust pool containing only this CA.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// Seal serializes the CA (certificate plus passphrase-encrypted key) for
// storage. The private key never appears in plain text in the result.
func (c *CA) Seal(passphrase string) ([]byte, error) {
	return c.sealIter(passphrase, DefaultPBKDF2Iterations)
}

func (c *CA) sealIter(passphrase string, iter int) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.key)
	if err != nil {
		return nil, err
	}
	return seal(der, c.certPEM, passphrase, iter)
}

// LoadCA restores a CA from the output of Seal. now may be nil.
func LoadCA(blob []byte, passphrase string, now func() time.Time) (*CA, error) {
	if now == nil {
		now = time.Now
	}
	der, public, err := open(blob, passphrase)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("certs: sealed CA key: %w", err)
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("certs: sealed CA key is not ECDSA")
	}
	block, _ := pem.Decode(public)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("certs: sealed CA has no certificate")
	}
	ca, err := newCA(block.Bytes, key, now)
	if err != nil {
		return nil, err
	}
	if !key.PublicKey.Equal(ca.cert.PublicKey) {
		return nil, errors.New("certs: sealed CA key does not match certificate")
	}
	return ca, nil
}

// Issued is a freshly issued leaf certificate with its key.
type Issued struct {
	// ChainPEM holds the leaf followed by the issuing CA, PEM encoded.
	ChainPEM []byte
	Cert     *x509.Certificate
	key      *ecdsa.PrivateKey
}

// CertPEM returns the leaf certificate only.
func (i *Issued) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: i.Cert.Raw})
}

// KeyPEM returns the unencrypted PKCS#8 key. It is an explicit in-memory
// handoff (for example to write a tmpfs secret for a proxy); never persist it.
func (i *Issued) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(i.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// SealKey returns the key encrypted under passphrase, suitable for storage.
func (i *Issued) SealKey(passphrase string) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(i.key)
	if err != nil {
		return nil, err
	}
	return seal(der, i.ChainPEM, passphrase, DefaultPBKDF2Iterations)
}

// TLSCertificate returns the pair for use in a tls.Config.
func (i *Issued) TLSCertificate() (tls.Certificate, error) {
	k, err := i.KeyPEM()
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(i.ChainPEM, k)
}

// Issue creates a leaf certificate. The result is verified against the CA
// before it is returned, so name-constraint violations fail here.
func (c *CA) Issue(ctx context.Context, req Request) (*Issued, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var eku x509.ExtKeyUsage
	switch req.Kind {
	case KindServer:
		eku = x509.ExtKeyUsageServerAuth
		if len(req.DNSNames)+len(req.IPs) == 0 {
			return nil, errors.New("certs: server certificate needs at least one SAN")
		}
	case KindClient:
		eku = x509.ExtKeyUsageClientAuth
		if req.CommonName == "" {
			return nil, errors.New("certs: client certificate needs a common name")
		}
	default:
		return nil, fmt.Errorf("certs: unknown certificate kind %q", req.Kind)
	}
	if req.Validity <= 0 {
		return nil, errors.New("certs: validity must be positive")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	t := c.now()
	notAfter := t.Add(req.Validity)
	if notAfter.After(c.cert.NotAfter) {
		notAfter = c.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: req.CommonName},
		NotBefore:             t.Add(-clockSkew),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{eku},
		BasicConstraintsValid: true,
		DNSNames:              req.DNSNames,
		IPAddresses:           req.IPs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: c.Pool(), CurrentTime: t, KeyUsages: []x509.ExtKeyUsage{eku},
	}); err != nil {
		return nil, fmt.Errorf("certs: issued certificate does not verify (name constraints?): %w", err)
	}
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), c.certPEM...)
	return &Issued{ChainPEM: chain, Cert: leaf, key: key}, nil
}
