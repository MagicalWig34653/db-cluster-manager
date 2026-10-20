// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProblemCode identifies one kind of validation problem.
type ProblemCode string

// Problem codes reported for imported certificate material.
const (
	ProblemCertParse        ProblemCode = "cert_parse"
	ProblemNoCertificate    ProblemCode = "no_certificate"
	ProblemKeyParse         ProblemCode = "key_parse"
	ProblemKeyEncrypted     ProblemCode = "key_encrypted"
	ProblemKeyMismatch      ProblemCode = "key_mismatch"
	ProblemChainOrder       ProblemCode = "chain_order"
	ProblemExpired          ProblemCode = "expired"
	ProblemNotYetValid      ProblemCode = "not_yet_valid"
	ProblemHostnameMismatch ProblemCode = "hostname_mismatch"
	ProblemUntrusted        ProblemCode = "untrusted"
	ProblemKeyUsage         ProblemCode = "key_usage"
)

// Problem is one typed validation finding.
type Problem struct {
	Code   ProblemCode
	Detail string
}

func (p Problem) Error() string { return string(p.Code) + ": " + p.Detail }

// ValidationError aggregates all problems found in imported material.
type ValidationError struct{ Problems []Problem }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Error()
	}
	return "certs: invalid certificate material: " + strings.Join(parts, "; ")
}

// Has reports whether a problem with the given code is present.
func (e *ValidationError) Has(code ProblemCode) bool {
	for _, p := range e.Problems {
		if p.Code == code {
			return true
		}
	}
	return false
}

// ImportOptions control validation of imported material.
type ImportOptions struct {
	Now time.Time
	// Hostnames (DNS names or IP literals) the leaf must cover.
	Hostnames []string
	// Roots, when set, requires the chain to verify to these roots. When nil
	// only structural checks (order, signatures, validity) are made, because
	// the roots of a public chain are not necessarily known to the manager.
	Roots *x509.CertPool
	// RequireServerAuth rejects leaves lacking the serverAuth usage.
	RequireServerAuth bool
}

// Imported is validated imported material.
type Imported struct {
	Chain []*x509.Certificate // leaf first
	Leaf  *x509.Certificate
	key   crypto.Signer
}

// ChainPEM returns the validated chain in leaf-first order.
func (i *Imported) ChainPEM() []byte {
	var out []byte
	for _, c := range i.Chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

// SealKey returns the key encrypted for storage.
func (i *Imported) SealKey(passphrase string) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(i.key)
	if err != nil {
		return nil, err
	}
	return seal(der, i.ChainPEM(), passphrase, DefaultPBKDF2Iterations)
}

// ValidateImported parses and validates a PEM chain and key. It returns a
// *ValidationError listing every problem found, or the validated material.
func ValidateImported(chainPEM, keyPEM []byte, opts ImportOptions) (*Imported, error) {
	var probs []Problem
	add := func(c ProblemCode, f string, a ...any) {
		probs = append(probs, Problem{c, fmt.Sprintf(f, a...)})
	}

	var chain []*x509.Certificate
	rest := chainPEM
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			add(ProblemCertParse, "certificate %d: %v", len(chain)+1, err)
			continue
		}
		chain = append(chain, c)
	}
	if len(chain) == 0 && len(probs) == 0 {
		add(ProblemNoCertificate, "no PEM certificate found")
	}

	key, kerr := parseKey(keyPEM)
	if kerr != nil {
		probs = append(probs, *kerr)
	}

	var leaf *x509.Certificate
	if len(chain) > 0 {
		leaf = chain[0]
		if key != nil && !publicKeyEqual(key.Public(), leaf.PublicKey) {
			add(ProblemKeyMismatch, "private key does not match leaf certificate")
		}
		for i := 0; i+1 < len(chain); i++ {
			if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
				add(ProblemChainOrder, "certificate %d is not issued by certificate %d: %v", i+1, i+2, err)
			}
		}
		for i, c := range chain {
			switch {
			case opts.Now.After(c.NotAfter):
				add(ProblemExpired, "certificate %d (%s) expired at %s", i+1, c.Subject.CommonName, c.NotAfter.UTC().Format(time.RFC3339))
			case opts.Now.Before(c.NotBefore):
				add(ProblemNotYetValid, "certificate %d (%s) valid from %s", i+1, c.Subject.CommonName, c.NotBefore.UTC().Format(time.RFC3339))
			}
		}
		for _, h := range opts.Hostnames {
			if err := leaf.VerifyHostname(h); err != nil {
				add(ProblemHostnameMismatch, "%v", err)
			}
		}
		if opts.RequireServerAuth && !hasEKU(leaf, x509.ExtKeyUsageServerAuth) {
			add(ProblemKeyUsage, "leaf certificate lacks serverAuth extended key usage")
		}
		if opts.Roots != nil {
			ip := x509.NewCertPool()
			for _, c := range chain[1:] {
				ip.AddCert(c)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{
				Roots: opts.Roots, Intermediates: ip, CurrentTime: opts.Now,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			}); err != nil {
				add(ProblemUntrusted, "%v", err)
			}
		}
	}
	if len(probs) > 0 {
		return nil, &ValidationError{Problems: probs}
	}
	return &Imported{Chain: chain, Leaf: leaf, key: key}, nil
}

func hasEKU(c *x509.Certificate, e x509.ExtKeyUsage) bool {
	if len(c.ExtKeyUsage) == 0 {
		return true // unrestricted
	}
	for _, u := range c.ExtKeyUsage {
		if u == e || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

var errNoKey = errors.New("no PEM private key found")

func parseKey(keyPEM []byte) (crypto.Signer, *Problem) {
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return nil, &Problem{ProblemKeyParse, errNoKey.Error()}
	}
	if strings.Contains(b.Type, "ENCRYPTED") || b.Headers["DEK-Info"] != "" {
		return nil, &Problem{ProblemKeyEncrypted, "encrypted private keys are not accepted; supply an unencrypted key"}
	}
	if k, err := x509.ParsePKCS8PrivateKey(b.Bytes); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
	}
	if k, err := x509.ParseECPrivateKey(b.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
		return k, nil
	}
	return nil, &Problem{ProblemKeyParse, "unsupported or malformed private key"}
}

func publicKeyEqual(a, b crypto.PublicKey) bool {
	switch k := a.(type) {
	case *ecdsa.PublicKey:
		return k.Equal(b)
	case *rsa.PublicKey:
		return k.Equal(b)
	case ed25519.PublicKey:
		return k.Equal(b)
	}
	return false
}
