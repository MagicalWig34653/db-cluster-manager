// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// CSR-based issuance: the key holder (an agent) generates its key locally
// and sends only a certificate signing request; the private key never
// leaves the host. The CA takes nothing but the public key from the CSR:
// subject, SANs and usages come from the Request the caller builds from
// its own records, so a key holder cannot ask for an identity it was not
// assigned.

// ErrBadCSR is returned for a CSR that cannot be parsed, has an invalid
// signature or an unacceptable key.
var ErrBadCSR = errors.New("certs: invalid certificate signing request")

const maxCSRPEM = 16 << 10

// NewKeyAndCSR generates an ECDSA P-256 key and a CSR for it. The CSR carries
// only commonName; the CA decides the certificate's identities.
func NewKeyAndCSR(commonName string) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// MarshalKeyPEM encodes a private key as unencrypted PKCS#8 PEM. Callers
// write it only to a file readable by its owner (mode 0600).
func MarshalKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseCSR decodes one PEM CSR, checks its self-signature and accepts only
// ECDSA P-256/P-384, Ed25519 or RSA >= 2048 bit keys.
func ParseCSR(pemBytes []byte) (*x509.CertificateRequest, error) {
	if len(pemBytes) > maxCSRPEM {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrBadCSR, maxCSRPEM)
	}
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("%w: no CERTIFICATE REQUEST PEM block", ErrBadCSR)
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("%w: trailing data after the CSR", ErrBadCSR)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrBadCSR, err)
	}
	switch k := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() {
			return nil, fmt.Errorf("%w: ECDSA curve %s not accepted", ErrBadCSR, k.Curve.Params().Name)
		}
	case ed25519.PublicKey:
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, fmt.Errorf("%w: RSA key shorter than 2048 bit", ErrBadCSR)
		}
	default:
		return nil, fmt.Errorf("%w: key type %T not accepted", ErrBadCSR, csr.PublicKey)
	}
	return csr, nil
}

// Signed is a certificate issued for a CSR. The CA never sees its key.
type Signed struct {
	Cert *x509.Certificate
	// ChainPEM holds the leaf followed by the issuing CA, PEM encoded.
	ChainPEM []byte
}

// SignCSR issues a certificate for the CSR's public key with the identities
// of req. Subject and extensions requested in the CSR are ignored.
func (c *CA) SignCSR(ctx context.Context, csr *x509.CertificateRequest, req Request) (*Signed, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if csr == nil {
		return nil, fmt.Errorf("%w: nil", ErrBadCSR)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrBadCSR, err)
	}
	leaf, chain, err := c.sign(req, csr.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Signed{Cert: leaf, ChainPEM: chain}, nil
}

// Fingerprint is the lowercase hex SHA-256 of a certificate's DER encoding.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// NormalizeFingerprint accepts a SHA-256 fingerprint in hex with optional
// "sha256:" prefix and ':' separators, in any case, and returns it in the
// form of Fingerprint.
func NormalizeFingerprint(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.ReplaceAll(s, ":", "")
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		return "", errors.New("certs: fingerprint must be a SHA-256 hex digest")
	}
	return s, nil
}

// URIScheme is the scheme of machine identities in URI SANs.
const URIScheme = "dbcm"

// AgentURI is the identity of the agent on a host of an installation:
// dbcm://<installation>/agent/<hostID>.
func AgentURI(installation, hostID string) *url.URL {
	return &url.URL{Scheme: URIScheme, Host: installation, Path: "/agent/" + hostID}
}

// ManagerURI is the identity of the manager of an installation:
// dbcm://<installation>/manager.
func ManagerURI(installation string) *url.URL {
	return &url.URL{Scheme: URIScheme, Host: installation, Path: "/manager"}
}

// HasURI reports whether cert carries exactly the URI SAN u (compared as
// strings; the identities above have one canonical form).
func HasURI(cert *x509.Certificate, u *url.URL) bool {
	want := u.String()
	return slices.ContainsFunc(cert.URIs, func(x *url.URL) bool { return x.String() == want })
}
