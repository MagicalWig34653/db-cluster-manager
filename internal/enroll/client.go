// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

// ErrManagerIdentity is returned when the enrollment server is not the
// pinned manager.
var ErrManagerIdentity = errors.New("enroll: manager identity rejected")

// ErrRejected wraps a refusal by the manager (HTTP status != 200).
var ErrRejected = errors.New("enroll: request rejected by the manager")

// Credentials are an agent's key, certificate chain and CA bundle (PEM).
type Credentials struct {
	KeyPEM   []byte
	ChainPEM []byte
	CAPEM    []byte
}

// TLSCertificate returns the key pair with its parsed leaf.
func (c *Credentials) TLSCertificate() (*tls.Certificate, error) {
	kp, err := tls.X509KeyPair(c.ChainPEM, c.KeyPEM)
	if err != nil {
		return nil, err
	}
	if kp.Leaf == nil {
		if kp.Leaf, err = x509.ParseCertificate(kp.Certificate[0]); err != nil {
			return nil, err
		}
	}
	return &kp, nil
}

// CAPool parses CAPEM.
func (c *Credentials) CAPool() (*x509.CertPool, error) { return certs.PoolFromBundle(c.CAPEM) }

// AgentConfig configures enrollment of one agent.
type AgentConfig struct {
	// ManagerURL is the enrollment endpoint base, https://host:port.
	ManagerURL   string
	Installation string
	HostID       string
	// CAFingerprint pins the manager's internal CA (SHA-256, any format
	// accepted by certs.NormalizeFingerprint).
	CAFingerprint string
	// Token is the plain enrollment token (from the Swarm secret).
	Token string
	// Info is the agent's runtime report (ADR 0024).
	Info agentrt.Info
	// Timeout bounds each request (default 30s).
	Timeout time.Duration
	Now     func() time.Time
}

func (c AgentConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// pinnedTLS verifies the server against the CA with the pinned
// fingerprint, which must be part of the presented chain.
func pinnedTLS(fingerprint, installation string, now func() time.Time, client *tls.Certificate) (*tls.Config, error) {
	pin, err := certs.NormalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // replaced by the pinned verification below
		VerifyConnection: func(cs tls.ConnectionState) error {
			var ca *x509.Certificate
			for _, c := range cs.PeerCertificates {
				if c.IsCA && certs.Fingerprint(c) == pin {
					ca = c
				}
			}
			if ca == nil {
				return fmt.Errorf("%w: no CA with the pinned fingerprint in the server chain", ErrManagerIdentity)
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca)
			return verifyManager(cs.PeerCertificates, roots, installation, now())
		},
	}
	if client != nil {
		cfg.Certificates = []tls.Certificate{*client}
	}
	return cfg, nil
}

func verifyManager(presented []*x509.Certificate, roots *x509.CertPool, installation string, now time.Time) error {
	if len(presented) == 0 {
		return fmt.Errorf("%w: no certificate", ErrManagerIdentity)
	}
	inter := x509.NewCertPool()
	for _, c := range presented[1:] {
		inter.AddCert(c)
	}
	if _, err := presented[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("%w: %w", ErrManagerIdentity, err)
	}
	if !certs.HasURI(presented[0], certs.ManagerURI(installation)) {
		return fmt.Errorf("%w: server certificate lacks %s", ErrManagerIdentity, certs.ManagerURI(installation))
	}
	return nil
}

func post(ctx context.Context, tc *tls.Config, base, path string, timeout time.Duration, body, out any) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("enroll: manager URL %q must be https://host:port", base)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	tr := &http.Transport{TLSClientConfig: tc, TLSHandshakeTimeout: 10 * time.Second, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e errorBody
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("%w: HTTP %d %s: %s", ErrRejected, resp.StatusCode, e.Error, e.Message)
	}
	return json.Unmarshal(data, out)
}

// checkResponse verifies that the issued certificate is for key, chains to
// a CA in the returned bundle that also verifies against trust (the pinned
// CA or the current bundle), and carries the agent's identity.
func checkResponse(resp Response, key *ecdsa.PrivateKey, installation, host string, trust func(*x509.Certificate) bool, now time.Time) (*Credentials, error) {
	keyPEM, err := certs.MarshalKeyPEM(key)
	if err != nil {
		return nil, err
	}
	cr := &Credentials{KeyPEM: keyPEM, ChainPEM: []byte(resp.Certificate), CAPEM: []byte(resp.CA)}
	kp, err := cr.TLSCertificate()
	if err != nil {
		return nil, fmt.Errorf("enroll: issued certificate does not match the key: %w", err)
	}
	pool := x509.NewCertPool()
	trusted := false
	rest := cr.CAPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("enroll: CA bundle: %w", err)
		}
		if trust(c) {
			trusted = true
			pool.AddCert(c)
		}
	}
	if !trusted {
		return nil, fmt.Errorf("%w: returned CA bundle does not contain the trusted CA", ErrManagerIdentity)
	}
	if _, err := kp.Leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("enroll: issued certificate: %w", err)
	}
	if !certs.HasURI(kp.Leaf, certs.AgentURI(installation, host)) {
		return nil, fmt.Errorf("enroll: issued certificate lacks %s", certs.AgentURI(installation, host))
	}
	return cr, nil
}

// Enroll generates a key, submits a CSR with the token and returns the
// credentials. The private key never leaves this process.
func Enroll(ctx context.Context, c AgentConfig) (*Credentials, error) {
	if c.Installation == "" || c.HostID == "" || c.Token == "" {
		return nil, errors.New("enroll: installation, host ID and token required")
	}
	tc, err := pinnedTLS(c.CAFingerprint, c.Installation, c.now, nil)
	if err != nil {
		return nil, err
	}
	key, csr, err := certs.NewKeyAndCSR("dbcm agent " + c.HostID)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := post(ctx, tc, c.ManagerURL, PathEnroll, c.Timeout, EnrollRequest{Token: c.Token, HostID: c.HostID, CSR: string(csr), Runtime: c.Info.Runtime, Capabilities: c.Info.Capabilities}, &resp); err != nil {
		return nil, err
	}
	pin, _ := certs.NormalizeFingerprint(c.CAFingerprint)
	return checkResponse(resp, key, c.Installation, c.HostID, func(x *x509.Certificate) bool {
		return x.IsCA && certs.Fingerprint(x) == pin
	}, c.now())
}

// RenewConfig configures a renewal.
type RenewConfig struct {
	ManagerURL   string
	Installation string
	HostID       string
	// Current are the credentials to authenticate with and whose CA bundle
	// the manager must chain to.
	Current *Credentials
	// Info is the agent's runtime report, resent on every renewal.
	Info    agentrt.Info
	Timeout time.Duration
	Now     func() time.Time
}

// Renew obtains a new certificate for a new local key, authenticating with
// the current certificate.
func Renew(ctx context.Context, c RenewConfig) (*Credentials, error) {
	if c.Current == nil {
		return nil, errors.New("enroll: renewal needs current credentials")
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	cur, err := c.Current.TLSCertificate()
	if err != nil {
		return nil, err
	}
	roots, err := c.Current.CAPool()
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{*cur},
		InsecureSkipVerify: true, //nolint:gosec // replaced by verifyManager
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyManager(cs.PeerCertificates, roots, c.Installation, now())
		},
	}
	key, csr, err := certs.NewKeyAndCSR("dbcm agent " + c.HostID)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := post(ctx, tc, c.ManagerURL, PathRenew, c.Timeout, RenewRequest{CSR: string(csr), Runtime: c.Info.Runtime, Capabilities: c.Info.Capabilities}, &resp); err != nil {
		return nil, err
	}
	return checkResponse(resp, key, c.Installation, c.HostID, func(x *x509.Certificate) bool {
		if !x.IsCA {
			return false
		}
		_, err := x.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now()})
		return err == nil
	}, now())
}
