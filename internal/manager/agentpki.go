// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
)

// Agent PKI (ADR 0010): the manager-operated internal CA that issues the
// agents' certificates (from their CSRs), the manager's client certificate
// for commands, and the enrollment endpoint's server certificate. It also
// implements enroll.Authority and enroll.Registry on the replicated store:
// enrollment tokens (digests only), agent identities in the host records
// and the serial deny list.
//
// The CA key is stored in the replicated store, sealed twice: the CA blob
// is passphrase-encrypted by internal/certs and the document is sealed with
// the manager's data key. Every replica therefore needs the same data key
// (key provisioning is still unsolved, see secrets.go). The CA is created
// once, by the leader, with compare-and-set; replicas load the same CA.

// PKIKeyAgentCA is the key of the sealed CA document in CollPKI.
const PKIKeyAgentCA = "agent-ca"

// Defaults of AgentPKI.
const (
	DefaultAgentCertValidity   = 30 * 24 * time.Hour
	DefaultManagerCertValidity = 7 * 24 * time.Hour
	DefaultCAValidity          = 10 * 365 * 24 * time.Hour
	DefaultTokenTTL            = 30 * 24 * time.Hour
)

// ErrNoInstallation is returned before the installation ID is configured.
var ErrNoInstallation = errors.New("manager: installation ID not configured")

type sealedCA struct {
	Passphrase string `json:"passphrase"`
	Blob       []byte `json:"blob"`
}

// EnrollTokenRecord is a stored enrollment token of one Swarm.
type EnrollTokenRecord struct {
	Swarm string       `json:"swarm"`
	Token enroll.Token `json:"token"`
}

// AgentPKI is the agent certificate authority of one manager replica.
type AgentPKI struct {
	Store *Store
	Keys  KeyProvider
	// ServerDNS and ServerIPs are the enrollment endpoint's names as the
	// agents dial them (the agents verify the manager identity URI SAN; the
	// names only help other TLS clients).
	ServerDNS []string
	ServerIPs []net.IP
	// RequireSourceAddr refuses an enrollment whose source IP is not the
	// claimed host's Swarm address (disable only behind NAT).
	RequireSourceAddr bool
	AgentValidity     time.Duration
	ManagerValidity   time.Duration
	Now               func() time.Time

	mu     sync.Mutex
	ca     *certs.CA
	client *tls.Certificate
	server *tls.Certificate
}

var (
	_ enroll.Authority = (*AgentPKI)(nil)
	_ enroll.Registry  = (*AgentPKI)(nil)
)

func (p *AgentPKI) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Installation returns the configured installation ID.
func (p *AgentPKI) Installation() (string, error) { return p.installation() }

func (p *AgentPKI) installation() (string, error) {
	st, ok, err := Get[Settings](p.Store, CollSettings, SettingsKey)
	if err != nil {
		return "", err
	}
	if !ok || st.Value.InstallationID == "" {
		return "", ErrNoInstallation
	}
	return st.Value.InstallationID, nil
}

// CA loads the CA from the store, creating it on first use (leader only).
func (p *AgentPKI) CA() (*certs.CA, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadCA()
}

func (p *AgentPKI) loadCA() (*certs.CA, error) {
	if p.ca != nil {
		return p.ca, nil
	}
	for range 3 {
		doc, ok, err := Get[sealed](p.Store, CollPKI, PKIKeyAgentCA, Consistent())
		if err != nil && !errors.Is(err, ErrNotLeader) {
			return nil, err
		}
		if err != nil {
			// A follower reads its local copy; it never creates a CA.
			if doc, ok, err = Get[sealed](p.Store, CollPKI, PKIKeyAgentCA); err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("%w: agent CA not created yet", ErrNotLeader)
			}
		}
		if ok {
			var sc sealedCA
			if err := unseal(p.Keys, CollPKI, PKIKeyAgentCA, doc.Value, &sc); err != nil {
				return nil, err
			}
			ca, err := certs.LoadCA(sc.Blob, sc.Passphrase, p.now)
			if err != nil {
				return nil, err
			}
			p.ca = ca
			return ca, nil
		}
		inst, err := p.installation()
		if err != nil {
			return nil, err
		}
		ca, err := certs.NewCA(certs.CAOptions{CommonName: "db-cluster-manager agent CA " + inst, Validity: DefaultCAValidity, Now: p.now})
		if err != nil {
			return nil, err
		}
		var pw [32]byte
		if _, err := rand.Read(pw[:]); err != nil {
			return nil, err
		}
		pass := hex.EncodeToString(pw[:])
		blob, err := ca.Seal(pass)
		if err != nil {
			return nil, err
		}
		sv, err := seal(p.Keys, CollPKI, PKIKeyAgentCA, sealedCA{Passphrase: pass, Blob: blob})
		if err != nil {
			return nil, err
		}
		if _, err := Put(p.Store, CollPKI, PKIKeyAgentCA, sv, IfVersion(0)); errors.Is(err, ErrConflict) {
			continue // another leader created it; load that one
		} else if err != nil {
			return nil, err
		}
		p.ca = ca
		return ca, nil
	}
	return nil, fmt.Errorf("%w: agent CA", ErrConflict)
}

// CAFingerprint returns the SHA-256 fingerprint agents pin.
func (p *AgentPKI) CAFingerprint() (string, error) {
	ca, err := p.CA()
	if err != nil {
		return "", err
	}
	return certs.Fingerprint(ca.Certificate()), nil
}

// CABundlePEM implements enroll.Authority.
func (p *AgentPKI) CABundlePEM() []byte {
	ca, err := p.CA()
	if err != nil {
		return nil
	}
	return ca.CertificatePEM()
}

// ClientCAs implements enroll.Authority.
func (p *AgentPKI) ClientCAs() *x509.CertPool {
	ca, err := p.CA()
	if err != nil {
		return x509.NewCertPool()
	}
	return ca.Pool()
}

func (p *AgentPKI) cached(slot **tls.Certificate, req func(inst string) certs.Request) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := *slot; c != nil && !certs.NeedsRenewal(c.Leaf, p.now(), certs.Policy{Fraction: 0.3}) {
		return c, nil
	}
	ca, err := p.loadCA()
	if err != nil {
		return nil, err
	}
	inst, err := p.installation()
	if err != nil {
		return nil, err
	}
	iss, err := ca.Issue(context.Background(), req(inst))
	if err != nil {
		return nil, err
	}
	c, err := iss.TLSCertificate()
	if err != nil {
		return nil, err
	}
	c.Leaf = iss.Cert
	*slot = &c
	return &c, nil
}

func (p *AgentPKI) managerValidity() time.Duration {
	if p.ManagerValidity > 0 {
		return p.ManagerValidity
	}
	return DefaultManagerCertValidity
}

// ManagerClientCertificate returns the manager's client certificate for
// agent commands (identity certs.ManagerURI), reissued before it expires.
// It is held in memory only.
func (p *AgentPKI) ManagerClientCertificate() (*tls.Certificate, error) {
	return p.cached(&p.client, func(inst string) certs.Request {
		return certs.Request{Kind: certs.KindClient, CommonName: "db-cluster-manager", URIs: []*url.URL{certs.ManagerURI(inst)},
			Validity: p.managerValidity()}
	})
}

// EnrollmentServerCertificate returns the enrollment endpoint certificate.
func (p *AgentPKI) EnrollmentServerCertificate() (*tls.Certificate, error) {
	return p.cached(&p.server, func(inst string) certs.Request {
		return certs.Request{Kind: certs.KindServer, CommonName: "db-cluster-manager enrollment", DNSNames: p.ServerDNS,
			IPs: p.ServerIPs, URIs: []*url.URL{certs.ManagerURI(inst)}, Validity: p.managerValidity()}
	})
}

// hostByNode finds the enrolled host record of a Swarm node ID.
func (p *AgentPKI) hostByNode(nodeID string, opts ...ReadOption) (Versioned[HostRecord], error) {
	hosts, err := List[HostRecord](p.Store, CollHosts, opts...)
	if err != nil {
		return Versioned[HostRecord]{}, err
	}
	for _, h := range hosts {
		if h.Value.NodeID == nodeID && h.Value.Enrollment == EnrollEnrolled {
			return h, nil
		}
	}
	return Versioned[HostRecord]{}, fmt.Errorf("%w: host %s is not enrolled", enroll.ErrUnknownAgent, nodeID)
}

// SignAgent implements enroll.Authority. Identities come from the host
// record: the agent URI SAN, the Swarm address as IP SAN and the hostname.
func (p *AgentPKI) SignAgent(ctx context.Context, nodeID string, csr *x509.CertificateRequest) (*certs.Signed, error) {
	h, err := p.hostByNode(nodeID, Consistent())
	if err != nil {
		return nil, err
	}
	inst, err := p.installation()
	if err != nil {
		return nil, err
	}
	ca, err := p.CA()
	if err != nil {
		return nil, err
	}
	req := certs.Request{Kind: certs.KindPeer, CommonName: "dbcm agent " + nodeID, URIs: []*url.URL{certs.AgentURI(inst, nodeID)},
		Validity: p.AgentValidity}
	if req.Validity <= 0 {
		req.Validity = DefaultAgentCertValidity
	}
	if ip := net.ParseIP(h.Value.Addr); ip != nil && !ip.IsUnspecified() {
		req.IPs = []net.IP{ip}
	}
	return ca.SignCSR(ctx, csr, req)
}

// mapEnrollStoreErr maps store errors to the enroll package error domain.
func mapEnrollStoreErr(err error) error {
	if errors.Is(err, ErrNotLeader) {
		return fmt.Errorf("%w: %w", enroll.ErrUnavailable, err)
	}
	return err
}

// IssueEnrollmentToken creates a token for the enrolled hosts of a Swarm
// and returns its plain form (to be put into a Swarm secret, never stored).
func (p *AgentPKI) IssueEnrollmentToken(swarmID string, ttl time.Duration) (string, EnrollTokenRecord, error) {
	inst, err := p.installation()
	if err != nil {
		return "", EnrollTokenRecord{}, err
	}
	hosts, err := p.enrolledNodes(swarmID)
	if err != nil {
		return "", EnrollTokenRecord{}, err
	}
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	tok, plain, err := enroll.NewToken(inst, hosts, p.now(), ttl)
	if err != nil {
		return "", EnrollTokenRecord{}, err
	}
	rec := EnrollTokenRecord{Swarm: swarmID, Token: tok}
	if _, err := Put(p.Store, CollEnrollTokens, tok.ID, rec, IfVersion(0)); err != nil {
		return "", EnrollTokenRecord{}, err
	}
	return plain, rec, nil
}

func (p *AgentPKI) enrolledNodes(swarmID string) ([]string, error) {
	hosts, err := List[HostRecord](p.Store, CollHosts, Consistent())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range hosts {
		if h.Value.Swarm == swarmID && h.Value.Enrollment == EnrollEnrolled {
			out = append(out, h.Value.NodeID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// RefreshTokenHosts sets the allowed hosts of a Swarm's unexpired tokens to
// its currently enrolled hosts (hosts enrolled after the agent service was
// deployed use the token already in the Swarm secret).
func (p *AgentPKI) RefreshTokenHosts(swarmID string) error {
	hosts, err := p.enrolledNodes(swarmID)
	if err != nil {
		return err
	}
	recs, err := List[EnrollTokenRecord](p.Store, CollEnrollTokens, Consistent())
	if err != nil {
		return err
	}
	now := p.now()
	for _, r := range recs {
		if r.Value.Swarm != swarmID || r.Value.Token.Revoked || !now.Before(r.Value.Token.ExpiresAt) {
			continue
		}
		_, _, err := Update(p.Store, CollEnrollTokens, r.Key, func(cur EnrollTokenRecord, exists bool) (EnrollTokenRecord, error) {
			if !exists {
				return cur, ErrNotFound
			}
			cur.Token.AllowedHosts = hosts
			return cur, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ConsumeToken implements enroll.Registry.
func (p *AgentPKI) ConsumeToken(_ context.Context, plain, nodeID string, source net.IP) error {
	id, secret, err := enroll.ParseToken(plain)
	if err != nil {
		return err
	}
	h, err := p.hostByNode(nodeID, Consistent())
	if err != nil {
		if errors.Is(err, enroll.ErrUnknownAgent) {
			return fmt.Errorf("%w: %s", enroll.ErrHostNotAllowed, nodeID)
		}
		return mapEnrollStoreErr(err)
	}
	if p.RequireSourceAddr {
		want := net.ParseIP(h.Value.Addr)
		if want == nil || source == nil || !want.Equal(source) {
			return fmt.Errorf("%w: %s from %v, Swarm address %q", enroll.ErrSourceMismatch, nodeID, source, h.Value.Addr)
		}
	}
	_, _, err = Update(p.Store, CollEnrollTokens, id, func(cur EnrollTokenRecord, exists bool) (EnrollTokenRecord, error) {
		if !exists {
			return cur, fmt.Errorf("%w: unknown token", enroll.ErrTokenInvalid)
		}
		if cur.Swarm != h.Value.Swarm {
			return cur, fmt.Errorf("%w: token of another swarm", enroll.ErrHostNotAllowed)
		}
		if err := cur.Token.Consume(secret, nodeID, p.now()); err != nil {
			return cur, err
		}
		return cur, nil
	})
	return mapEnrollStoreErr(err)
}

// CurrentSerial implements enroll.Registry.
func (p *AgentPKI) CurrentSerial(_ context.Context, nodeID string) (string, bool, error) {
	h, err := p.hostByNode(nodeID, Consistent())
	if errors.Is(err, enroll.ErrUnknownAgent) {
		return "", false, nil
	}
	if err != nil {
		return "", false, mapEnrollStoreErr(err)
	}
	return h.Value.AgentSerial, h.Value.AgentSerial != "", nil
}

// RecordIssued implements enroll.Registry. The previous serial of the host
// is added to the deny list: after a renewal only the new certificate is
// accepted.
func (p *AgentPKI) RecordIssued(_ context.Context, nodeID string, cert *x509.Certificate) error {
	h, err := p.hostByNode(nodeID, Consistent())
	if err != nil {
		return mapEnrollStoreErr(err)
	}
	var previous string
	_, _, err = Update(p.Store, CollHosts, h.Key, func(cur HostRecord, exists bool) (HostRecord, error) {
		if !exists || cur.Enrollment != EnrollEnrolled {
			return cur, fmt.Errorf("%w: host %s", enroll.ErrUnknownAgent, nodeID)
		}
		previous = cur.AgentSerial
		cur.AgentSerial = cert.SerialNumber.Text(16)
		cur.AgentNotAfter = cert.NotAfter
		cur.AgentIdentityAt = p.now()
		return cur, nil
	})
	if err != nil {
		return mapEnrollStoreErr(err)
	}
	if previous != "" && previous != cert.SerialNumber.Text(16) {
		return mapEnrollStoreErr(p.revokeSerial(previous, "superseded by renewal"))
	}
	return nil
}

func (p *AgentPKI) revokeSerial(serial, reason string) error {
	_, err := Put(p.Store, CollRevocations, serial, certs.Revocation{Serial: serial, RevokedAt: p.now(), Reason: reason}, nil)
	return err
}

// Revoked implements enroll.Registry and the deny-list callbacks of the
// agent transport. It reads the local replica.
func (p *AgentPKI) Revoked(serial *big.Int) bool {
	_, ok, err := Get[certs.Revocation](p.Store, CollRevocations, serial.Text(16))
	// Fail closed: an unreadable deny list rejects.
	return err != nil || ok
}

// RevokeAgent revokes the current agent identity of a host: its serial is
// denied and the record cleared, so the agent must enroll again with a
// new token. Permission: update on the host.
func (p *AgentPKI) RevokeAgent(actor authz.Subject, az *authz.Authorizer, swarmID, nodeID, reason string) error {
	if d := az.Authorize(actor, authz.ActionUpdate, authz.Resource{Type: authz.ResourceHost, Scope: authz.SwarmScope(swarmID), ID: nodeID}); !d.Allowed {
		return &authz.DeniedError{Decision: d}
	}
	var serial string
	_, _, err := Update(p.Store, CollHosts, HostKey(swarmID, nodeID), func(cur HostRecord, exists bool) (HostRecord, error) {
		if !exists {
			return cur, ErrNotFound
		}
		serial = cur.AgentSerial
		cur.AgentSerial, cur.AgentNotAfter = "", time.Time{}
		return cur, nil
	})
	if err != nil || serial == "" {
		return err
	}
	return p.revokeSerial(serial, reason)
}
