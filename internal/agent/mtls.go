// SPDX-License-Identifier: AGPL-3.0-only

package agent

// Authenticated manager-agent transport: HTTP/JSON over mutually
// authenticated TLS 1.3 on a host-reachable TCP address.
//
//   - The agent serves with its own certificate (issued by the manager's
//     internal CA for the agent's CSR during enrollment; URI SAN
//     dbcm://<installation>/agent/<host ID>). The certificate is read through
//     a callback on every handshake, so a renewed certificate takes effect
//     without a restart.
//   - The agent requires a client certificate that chains to the CA bundle
//     it received (and pinned by fingerprint) at enrollment, carries the
//     clientAuth usage and the pinned manager identity URI SAN
//     dbcm://<installation>/manager, and is not on the deny list.
//   - The manager-side client verifies the agent's certificate against the
//     same CA, the serverAuth usage and the agent identity URI SAN of the
//     host it means to reach; host names are not used for identity.
//
// The request handling is the same as the lab transport's minus the
// loopback checks.

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

// ErrPeerIdentity is returned when a TLS peer does not present the expected
// identity.
var ErrPeerIdentity = errors.New("agent: TLS peer identity rejected")

// MTLSServerConfig configures the agent's mTLS listener.
type MTLSServerConfig struct {
	// Certificate returns the agent's current certificate (called per
	// handshake).
	Certificate func() (*tls.Certificate, error)
	// ClientCAs is the CA bundle client certificates must chain to.
	ClientCAs *x509.CertPool
	// ManagerIdentity is the URI SAN the manager's client certificate must
	// carry.
	ManagerIdentity *url.URL
	// Revoked, if set, rejects denied serials anywhere in the chain.
	Revoked func(serial *big.Int) bool
	// Now is the clock for certificate validity; time.Now when nil.
	Now func() time.Time
}

// MTLSClientConfig configures the manager-side client of one agent.
type MTLSClientConfig struct {
	// Certificate returns the manager's client certificate.
	Certificate func() (*tls.Certificate, error)
	// RootCAs is the CA bundle the agent certificate must chain to.
	RootCAs *x509.CertPool
	// AgentIdentity is the URI SAN the agent must present.
	AgentIdentity *url.URL
	Revoked       func(serial *big.Int) bool
	Now           func() time.Time
	// Timeout bounds one request; default 5 minutes (commands may scale
	// services). The caller's context applies as well.
	Timeout time.Duration
}

func revokedChain(chain []*x509.Certificate, revoked func(*big.Int) bool) error {
	if revoked == nil {
		return nil
	}
	for _, c := range chain {
		if revoked(c.SerialNumber) {
			return fmt.Errorf("%w: %w: serial %s", ErrPeerIdentity, certs.ErrRevoked, c.SerialNumber.Text(16))
		}
	}
	return nil
}

// ServerTLSConfig returns the TLS configuration of the agent listener.
func ServerTLSConfig(c MTLSServerConfig) (*tls.Config, error) {
	if c.Certificate == nil || c.ClientCAs == nil || c.ManagerIdentity == nil {
		return nil, errors.New("agent: mTLS server needs Certificate, ClientCAs and ManagerIdentity")
	}
	want := c.ManagerIdentity
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  c.ClientCAs,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.Certificate()
		},
		// The standard verification (chain to ClientCAs, validity at Time,
		// clientAuth usage) ran before this callback.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
				return fmt.Errorf("%w: no verified client chain", ErrPeerIdentity)
			}
			chain := cs.VerifiedChains[0]
			if !certs.HasURI(chain[0], want) {
				return fmt.Errorf("%w: client certificate lacks identity %s", ErrPeerIdentity, want)
			}
			return revokedChain(chain, c.Revoked)
		},
	}
	if c.Now != nil {
		cfg.Time = c.Now
	}
	return cfg, nil
}

// NewMTLSServer returns an http.Server serving h over mTLS. Serve it with
// ServeTLS(listener, "", "") on any TCP listener.
func NewMTLSServer(h Handler, c MTLSServerConfig) (*http.Server, error) {
	tc, err := ServerTLSConfig(c)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Handler: newCommandHandler(h, func(r *http.Request) string {
			// Defence in depth: the TLS layer already required a verified
			// manager certificate.
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
				return "no verified client certificate"
			}
			return ""
		}),
		TLSConfig:         tc,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       time.Minute,
	}, nil
}

// ClientTLSConfig returns the TLS configuration for reaching one agent.
// Host names are not verified; the agent identity URI SAN is.
func ClientTLSConfig(c MTLSClientConfig) (*tls.Config, error) {
	if c.Certificate == nil || c.RootCAs == nil || c.AgentIdentity == nil {
		return nil, errors.New("agent: mTLS client needs Certificate, RootCAs and AgentIdentity")
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	want := c.AgentIdentity
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return c.Certificate()
		},
		// InsecureSkipVerify only disables the host-name based default;
		// VerifyConnection below performs the complete verification.
		InsecureSkipVerify: true, //nolint:gosec // replaced by VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			return VerifyPeer(cs.PeerCertificates, c.RootCAs, x509.ExtKeyUsageServerAuth, want, now(), c.Revoked)
		},
		Time: now,
	}, nil
}

// VerifyPeer verifies a presented chain (leaf first) against roots for the
// usage at now, requires the identity URI SAN and rejects revoked serials.
func VerifyPeer(presented []*x509.Certificate, roots *x509.CertPool, usage x509.ExtKeyUsage, identity *url.URL, now time.Time, revoked func(*big.Int) bool) error {
	if len(presented) == 0 {
		return fmt.Errorf("%w: no certificate presented", ErrPeerIdentity)
	}
	inter := x509.NewCertPool()
	for _, c := range presented[1:] {
		inter.AddCert(c)
	}
	chains, err := presented[0].Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage},
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPeerIdentity, err)
	}
	if !certs.HasURI(presented[0], identity) {
		return fmt.Errorf("%w: certificate lacks identity %s", ErrPeerIdentity, identity)
	}
	return revokedChain(chains[0], revoked)
}

// NewMTLSClient returns a Client for the agent at baseURL
// ("https://host:port") that authenticates both sides.
func NewMTLSClient(baseURL string, c MTLSClientConfig) (*Client, error) {
	tc, err := ClientTLSConfig(c)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("agent: mTLS base URL %q must be https://host:port", baseURL)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	tr := &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: false, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: time.Minute, TLSHandshakeTimeout: 10 * time.Second}
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Transport: tr, Timeout: timeout}}, nil
}
