// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"
)

// CABundle builds a PEM bundle of the given CA certificates for distribution
// to clients. Duplicates are removed and the output order is deterministic.
func CABundle(cas ...*x509.Certificate) ([]byte, error) {
	seen := map[string]*x509.Certificate{}
	for _, c := range cas {
		if c == nil {
			return nil, errors.New("certs: nil CA certificate")
		}
		if !c.IsCA {
			return nil, fmt.Errorf("certs: %q is not a CA certificate", c.Subject.CommonName)
		}
		seen[string(c.Raw)] = c
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []byte
	for _, k := range keys {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: seen[k].Raw})...)
	}
	return out, nil
}

// PoolFromBundle parses a PEM bundle into a trust pool.
func PoolFromBundle(bundle []byte) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(bundle) {
		return nil, errors.New("certs: no certificates in bundle")
	}
	return p, nil
}

// Revocation is one denied serial.
type Revocation struct {
	Serial    string    `json:"serial"` // hex
	RevokedAt time.Time `json:"revoked_at"`
	Reason    string    `json:"reason,omitempty"`
}

// DenyList is a simple CRL-like list of revoked serial numbers. It is not an
// X.509 CRL and not served over OCSP; distributing it to peers is future work.
type DenyList struct {
	mu      sync.RWMutex
	entries map[string]Revocation
}

// NewDenyList returns an empty list.
func NewDenyList() *DenyList { return &DenyList{entries: map[string]Revocation{}} }

// Revoke adds a serial. Re-revoking keeps the original entry.
func (d *DenyList) Revoke(serial *big.Int, at time.Time, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := serial.Text(16)
	if _, ok := d.entries[k]; !ok {
		d.entries[k] = Revocation{Serial: k, RevokedAt: at, Reason: reason}
	}
}

// IsRevoked reports whether serial is on the list.
func (d *DenyList) IsRevoked(serial *big.Int) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.entries[serial.Text(16)]
	return ok
}

// Entries returns the revocations sorted by serial.
func (d *DenyList) Entries() []Revocation {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Revocation, 0, len(d.entries))
	for _, e := range d.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// MarshalJSON encodes the list.
func (d *DenyList) MarshalJSON() ([]byte, error) { return json.Marshal(d.Entries()) }

// UnmarshalJSON decodes a list produced by MarshalJSON.
func (d *DenyList) UnmarshalJSON(b []byte) error {
	var es []Revocation
	if err := json.Unmarshal(b, &es); err != nil {
		return err
	}
	m := make(map[string]Revocation, len(es))
	for _, e := range es {
		if _, ok := new(big.Int).SetString(e.Serial, 16); !ok {
			return fmt.Errorf("certs: invalid serial %q", e.Serial)
		}
		m[e.Serial] = e
	}
	d.mu.Lock()
	d.entries = m
	d.mu.Unlock()
	return nil
}

// ErrRevoked is returned when a certificate in the chain is on the deny list.
var ErrRevoked = errors.New("certs: certificate revoked")

// VerifyPeerCertificate is a tls.Config.VerifyPeerCertificate callback that
// rejects any presented certificate whose serial is denied. It runs in
// addition to the normal chain verification.
func (d *DenyList) VerifyPeerCertificate(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return err
		}
		if d.IsRevoked(c.SerialNumber) {
			return fmt.Errorf("%w: serial %s", ErrRevoked, c.SerialNumber.Text(16))
		}
	}
	return nil
}

// VerifyLeaf verifies a leaf (with optional intermediates) against roots for
// the given usage at time now, and then checks the deny list (may be nil).
func VerifyLeaf(leaf *x509.Certificate, intermediates []*x509.Certificate, roots *x509.CertPool,
	usage x509.ExtKeyUsage, now time.Time, deny *DenyList) error {
	ip := x509.NewCertPool()
	for _, c := range intermediates {
		ip.AddCert(c)
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: ip, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage},
	})
	if err != nil {
		return err
	}
	if deny != nil {
		for _, c := range chains[0] {
			if deny.IsRevoked(c.SerialNumber) {
				return fmt.Errorf("%w: serial %s", ErrRevoked, c.SerialNumber.Text(16))
			}
		}
	}
	return nil
}
