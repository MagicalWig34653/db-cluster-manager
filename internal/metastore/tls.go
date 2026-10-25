// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/raft"
)

// Peer encryption and authentication (mTLS between metastore replicas).
//
// With Config.TLS set, BindAddr creates raft's network transport over a TLS
// 1.3 stream layer instead of plain TCP. Both directions present a
// certificate and verify the other side:
//
//   - the chain must verify against TLS.Roots (the installation's peer CA)
//     with the clientAuth (inbound) or serverAuth (outbound) usage at the
//     current time;
//   - the leaf must carry a peer identity URI SAN
//     dbcm://<installation>/metastore/<replica ID> of this installation
//     (certs.MetastorePeerURI), and on outbound connections the replica ID
//     must be the one the dialed address belongs to when the address is
//     listed in TLS.PeerIDs;
//   - TLS.Revoked, when set, rejects a denied serial anywhere in the chain.
//
// Host names and IPs are not checked: Raft dials configured addresses and
// identities are URI SANs, so verification is done in VerifyConnection with
// the default host name check switched off.

// PeerTLS configures mutual TLS between replicas.
type PeerTLS struct {
	// Installation is the installation ID in the peer identity URIs.
	Installation string
	// Certificate returns this replica's certificate (called per handshake,
	// so a renewed certificate is picked up without a restart).
	Certificate func() (*tls.Certificate, error)
	// Roots verifies peer certificates (the peer CA).
	Roots *x509.CertPool
	// PeerIDs maps transport addresses to the replica ID expected there
	// (outbound check). Addresses not listed accept any replica of the
	// installation.
	PeerIDs map[string]string
	// Revoked reports whether a certificate serial is denied; nil denies
	// none.
	Revoked func(c *x509.Certificate) bool
	// PeerAllowed can tombstone a removed replica identity, including certificates
	// issued offline whose serial was never recorded by the manager.
	PeerAllowed func(id string) bool
	// Now is the verification clock; time.Now when nil.
	Now func() time.Time
}

// ErrPeerIdentity is returned when a peer certificate does not carry the
// expected metastore identity.
var ErrPeerIdentity = errors.New("metastore: peer certificate has no valid metastore identity")

// peerURIPrefix is the path prefix of metastore peer identities.
const peerURIPrefix = "/metastore/"

// PeerIDOf returns the replica ID of a metastore peer identity URI of
// installation in cert, or false.
func PeerIDOf(cert *x509.Certificate, installation string) (string, bool) {
	var id string
	n := 0
	for _, u := range cert.URIs {
		if u.Scheme != "dbcm" || u.Host != installation || !strings.HasPrefix(u.Path, peerURIPrefix) {
			continue
		}
		id = strings.TrimPrefix(u.Path, peerURIPrefix)
		n++
	}
	if n != 1 || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// PeerURI is the identity of a metastore replica of an installation:
// dbcm://<installation>/metastore/<id>.
func PeerURI(installation, id string) *url.URL {
	return &url.URL{Scheme: "dbcm", Host: installation, Path: peerURIPrefix + id}
}

func (p *PeerTLS) validate() error {
	switch {
	case p.Installation == "":
		return errors.New("metastore: peer TLS needs an installation ID")
	case p.Certificate == nil:
		return errors.New("metastore: peer TLS needs a certificate")
	case p.Roots == nil:
		return errors.New("metastore: peer TLS needs a root pool")
	}
	return nil
}

func (p *PeerTLS) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// verify checks a presented chain for usage and returns the replica ID.
func (p *PeerTLS) verify(cs tls.ConnectionState, usage x509.ExtKeyUsage) (string, error) {
	if len(cs.PeerCertificates) == 0 {
		return "", fmt.Errorf("%w: no certificate", ErrPeerIdentity)
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: p.Roots, Intermediates: inter, CurrentTime: p.now(),
		KeyUsages: []x509.ExtKeyUsage{usage}})
	if err != nil {
		return "", fmt.Errorf("metastore: peer certificate: %w", err)
	}
	if p.Revoked != nil {
		for _, c := range chains[0] {
			if p.Revoked(c) {
				return "", fmt.Errorf("metastore: peer certificate serial %s is revoked", c.SerialNumber.Text(16))
			}
		}
	}
	id, ok := PeerIDOf(leaf, p.Installation)
	if !ok || (p.PeerAllowed != nil && !p.PeerAllowed(id)) {
		return "", ErrPeerIdentity
	}
	return id, nil
}

func (p *PeerTLS) serverConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAnyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.Certificate()
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := p.verify(cs, x509.ExtKeyUsageClientAuth)
			return err
		},
	}
}

// ServerConfig authenticates replica connections to a separate manager API
// forwarding listener with the same peer identities as the Raft transport.
func (p *PeerTLS) ServerConfig() *tls.Config { return p.serverConfig() }

func (p *PeerTLS) clientConfig(address string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Identities are URI SANs; VerifyConnection does the full chain
		// and identity verification instead of the host name check.
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return p.Certificate()
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			id, err := p.verify(cs, x509.ExtKeyUsageServerAuth)
			if err != nil {
				return err
			}
			if want, ok := p.PeerIDs[address]; ok && want != id {
				return fmt.Errorf("%w: %s answered as %q, want %q", ErrPeerIdentity, address, id, want)
			}
			return nil
		},
	}
}

// ClientConfig authenticates the replica at the configured Raft address.
// It can be used when dialing that replica's separate API forwarding port.
func (p *PeerTLS) ClientConfig(raftAddress string) *tls.Config {
	return p.clientConfig(raftAddress)
}

// tlsStreamLayer is a raft.StreamLayer over TLS 1.3 with mutual
// authentication.
type tlsStreamLayer struct {
	net.Listener
	advertise net.Addr
	cfg       *PeerTLS
}

func newTLSStreamLayer(bind, advertise string, cfg *PeerTLS) (*tlsStreamLayer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("metastore: listen: %w", err)
	}
	adv := ln.Addr()
	if advertise != "" {
		a, err := net.ResolveTCPAddr("tcp", advertise)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("metastore: advertise address: %w", err)
		}
		adv = a
	} else if t, ok := adv.(*net.TCPAddr); ok && t.IP.IsUnspecified() {
		_ = ln.Close()
		return nil, errors.New("metastore: advertise address required for an unspecified bind address")
	}
	return &tlsStreamLayer{Listener: tls.NewListener(ln, cfg.serverConfig()), advertise: adv, cfg: cfg}, nil
}

// Accepted connections handshake on their first read (in raft's
// per-connection goroutine, so a slow client does not block Accept); a
// failed verification fails that read and raft drops the connection before
// decoding anything.

// Addr implements net.Listener with the advertised address.
func (l *tlsStreamLayer) Addr() net.Addr { return l.advertise }

// Dial implements raft.StreamLayer.
func (l *tlsStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: l.cfg.clientConfig(string(address))}
	return d.Dial("tcp", string(address))
}

func newTLSTransport(bind, advertise string, cfg *PeerTLS, logOutput io.Writer) (Transport, error) {
	layer, err := newTLSStreamLayer(bind, advertise, cfg)
	if err != nil {
		return nil, err
	}
	return raft.NewNetworkTransport(layer, tcpPoolSize, tcpTimeout, logOutput), nil
}

// peerIDsOf maps the addresses of peers to their IDs.
func peerIDsOf(peers []Peer) map[string]string {
	m := make(map[string]string, len(peers))
	for _, p := range peers {
		m[p.Address] = p.ID
	}
	return m
}
