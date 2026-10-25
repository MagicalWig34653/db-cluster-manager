// SPDX-License-Identifier: AGPL-3.0-only
package manager

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"slices"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

const CollPeerCertificates = "peer-certificates"
const CollMembershipChanges = "membership-changes"

// PeerCertificateRecord holds public provisioning data only. The joining peer
// generates its CSR key locally; the offline peer CA is injected for fake tests,
// never read from disk or implicitly replaced by the agent CA.
type PeerCertificateRecord struct {
	Peer        metastore.Peer `json:"peer"`
	PublicKey   string         `json:"publicKey"`
	Serial      string         `json:"serial"`
	Certificate []byte         `json:"certificate"`
	NotAfter    time.Time      `json:"notAfter"`
	Removed     bool           `json:"removed"`
}

type MembershipChange struct {
	Peer     metastore.Peer `json:"peer"`
	Remove   bool           `json:"remove"`
	Digest   string         `json:"digest"`
	By       string         `json:"by"`
	Complete bool           `json:"complete"`
}

// MembershipService is opt-in groundwork. A real runtime must provide a trusted
// peer CA and a fresh authenticated readiness checker before exposing changes.
// The default manager keeps these endpoints unavailable (503).
type MembershipService struct {
	Svc        *Service
	Ring       *Keyring
	PeerCA     *certs.CA
	Authorizer *authz.Authorizer
	Ready      metastore.PeerReady
}

func (m *MembershipService) authorize(actor authz.Subject) error {
	if err := m.Authorizer.AuthorizeAdministrator(actor); err != nil {
		return err
	}
	if !m.Svc.IsLeader() {
		return api.ErrNotLeader
	}
	if m.Ring == nil || m.PeerCA == nil || m.Ready == nil {
		return api.ErrUnavailable
	}
	return nil
}

// IssuePeerCertificate provisions a candidate, not a voter. Its address, CSR
// public key and data-key recipient must be compared out of band by the admin.
// Replacing an existing enrollment is refused; explicit removal precedes reuse.
func (m *MembershipService) IssuePeerCertificate(ctx context.Context, actor authz.Subject, id string, in api.PeerCertificateInput) (api.PeerCertificateResult, error) {
	if err := m.authorize(actor); err != nil {
		return api.PeerCertificateResult{}, err
	}
	if !peerIDPattern(id) {
		return api.PeerCertificateResult{}, api.ErrInvalidInput
	}
	if _, _, err := net.SplitHostPort(in.Address); err != nil {
		return api.PeerCertificateResult{}, api.ErrInvalidInput
	}
	csr, err := certs.ParseCSR([]byte(in.CSR))
	if err != nil {
		return api.PeerCertificateResult{}, api.ErrInvalidInput
	}
	pub, err := backup.ParseStreamPublicKey(in.PublicKey)
	if err != nil {
		return api.PeerCertificateResult{}, api.ErrInvalidInput
	}
	prev, exists, err := Get[PeerCertificateRecord](m.Svc.store, CollPeerCertificates, id, Consistent())
	if err != nil {
		return api.PeerCertificateResult{}, err
	}
	// IDs with removal intents stay tombstoned; use a new replica identity.
	if exists {
		return api.PeerCertificateResult{}, api.ErrConflict
	}
	settings, ok, err := Get[Settings](m.Svc.store, CollSettings, SettingsKey, Consistent())
	if err != nil {
		return api.PeerCertificateResult{}, err
	}
	if !ok || settings.Value.InstallationID == "" {
		return api.PeerCertificateResult{}, api.ErrSettingsIncomplete
	}
	signed, err := m.PeerCA.SignCSR(ctx, csr, certs.Request{Kind: certs.KindPeer, CommonName: "dbcm metastore " + id, URIs: []*url.URL{metastore.PeerURI(settings.Value.InstallationID, id)}, Validity: 24 * time.Hour})
	if err != nil {
		return api.PeerCertificateResult{}, err
	}
	rec := PeerCertificateRecord{Peer: metastore.Peer{ID: id, Address: in.Address}, PublicKey: pub.String(), Serial: signed.Cert.SerialNumber.Text(16), Certificate: signed.ChainPEM, NotAfter: signed.Cert.NotAfter}
	op, err := PutOp(CollPeerCertificates, id, rec, IfVersion(prev.Version))
	if err != nil {
		return api.PeerCertificateResult{}, err
	}
	if _, err := m.Svc.store.Txn(op); err != nil {
		return api.PeerCertificateResult{}, MapCommandError(err)
	}
	m.Svc.recordEvent("", "", api.SeverityWarning, fmt.Sprintf("%s issued peer certificate for candidate %s", actor.ID, id))
	return api.PeerCertificateResult{Certificate: string(signed.ChainPEM), Serial: rec.Serial, NotAfter: rec.NotAfter}, nil
}
func peerIDPattern(id string) bool {
	if len(id) == 0 || len(id) > 63 || id == "active" {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func membershipDigest(v metastore.MembershipView, p PeerCertificateRecord, remove bool) string {
	b, _ := json.Marshal(struct {
		View   metastore.MembershipView
		Peer   PeerCertificateRecord
		Remove bool
	}{v, p, remove})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ChangeMembership previews on an empty digest. Approval creates a durable
// intent before Raft or revocation effects. Retrying an unfinished intent after
// leader loss reconciles the actual configuration instead of undoing changes.
func (m *MembershipService) ChangeMembership(ctx context.Context, actor authz.Subject, id string, in api.MembershipInput) (api.MembershipResult, error) {
	if err := m.authorize(actor); err != nil {
		return api.MembershipResult{}, err
	}
	cert, exists, err := Get[PeerCertificateRecord](m.Svc.store, CollPeerCertificates, id, Consistent())
	if err != nil {
		return api.MembershipResult{}, err
	}
	if !exists {
		return api.MembershipResult{}, api.ErrNotFound
	}
	v, err := m.Svc.node.Membership()
	if err != nil {
		return api.MembershipResult{}, err
	}
	pending, has, err := Get[MembershipChange](m.Svc.store, CollMembershipChanges, id, Consistent())
	if err != nil {
		return api.MembershipResult{}, err
	}
	if has && pending.Value.Complete && pending.Value.Remove == in.Remove && pending.Value.Digest == in.Digest && in.Digest != "" {
		return api.MembershipResult{Digest: in.Digest, Complete: true}, nil
	}
	digest := membershipDigest(v, cert.Value, in.Remove)
	out := api.MembershipResult{Digest: digest}
	if has && !pending.Value.Complete {
		if pending.Value.Remove != in.Remove {
			return out, api.ErrConflict
		}
		digest = pending.Value.Digest
		out.Digest = digest
	}
	if in.Digest == "" {
		return out, nil
	}
	if in.Digest != digest {
		return out, api.ErrApprovalMismatch
	}
	// Completed requests can be retried without repeating their effects.
	if has && pending.Value.Complete && pending.Value.Digest == in.Digest {
		out.Complete = true
		return out, nil
	}
	present := slices.ContainsFunc(v.Voters, func(p metastore.Peer) bool { return p.ID == id && p.Address == cert.Value.Peer.Address })
	if !has || pending.Value.Complete {
		if cert.Value.Removed || present != in.Remove {
			return out, api.ErrConflict
		}
		if err := m.Svc.node.CheckMembership(ctx, v, cert.Value.Peer, in.Remove, m.Ready); err != nil {
			return out, mapMembershipError(err)
		}
		change := MembershipChange{Peer: cert.Value.Peer, Remove: in.Remove, Digest: digest, By: actor.ID}
		intent, err := PutOp(CollMembershipChanges, id, change, IfVersion(pending.Version))
		if err != nil {
			return out, err
		}
		lock, err := PutOp(CollMembershipChanges, "active", id, IfVersion(0))
		if err != nil {
			return out, err
		}
		if _, err := m.Svc.store.Txn(intent, lock); err != nil {
			return out, MapCommandError(err)
		}
		pending, _, err = Get[MembershipChange](m.Svc.store, CollMembershipChanges, id, Consistent())
		if err != nil {
			return out, err
		}
	}
	if in.Remove {
		if present {
			if err := m.Svc.node.CheckMembership(ctx, v, cert.Value.Peer, true, m.Ready); err != nil {
				return out, mapMembershipError(err)
			}
		}

	} else {
		if !m.Svc.cfg.Now().Before(cert.Value.NotAfter) {
			return out, api.ErrConflict
		}
		if err := m.prepareAddition(cert.Value, actor); err != nil {
			return out, err
		}
	}
	if present == in.Remove {
		if err := m.Svc.node.ChangeMembership(ctx, v, cert.Value.Peer, in.Remove, m.Ready); err != nil {
			return out, mapMembershipError(err)
		}
	}
	if in.Remove {
		if err := m.prepareRemoval(cert); err != nil {
			return out, err
		}
	}
	// A leadership loss after Raft commits leaves the intent incomplete; the next
	// leader sees membership already changed and finishes the metadata transition.
	change := pending.Value
	change.Complete = true
	finish, err := PutOp(CollMembershipChanges, id, change, IfVersion(pending.Version))
	if err != nil {
		return out, err
	}
	active, ok, err := Get[string](m.Svc.store, CollMembershipChanges, "active", Consistent())
	if err != nil {
		return out, err
	}
	if !ok || active.Value != id {
		return out, api.ErrConflict
	}
	if _, err := m.Svc.store.Txn(finish, metastore.DocOp{Collection: CollMembershipChanges, Key: "active", Delete: true, IfVersion: IfVersion(active.Version)}); err != nil {
		return out, MapCommandError(err)
	}
	m.Svc.recordEvent("", "", api.SeverityWarning, fmt.Sprintf("%s completed membership change for %s (remove=%t)", actor.ID, id, in.Remove))
	out.Complete = true
	return out, nil
}

func (m *MembershipService) prepareAddition(cert PeerCertificateRecord, actor authz.Subject) error {
	_, _, err := Update(m.Svc.store, CollKeys, KeyringKey, func(rec KeyringRecord, ok bool) (KeyringRecord, error) {
		if !ok {
			return rec, ErrNoKeyring
		}
		if rec.Replicas == nil {
			rec.Replicas = map[string]ReplicaRegistration{}
		}
		rec.Replicas[cert.Peer.ID] = ReplicaRegistration{PublicKey: cert.PublicKey, RegisteredBy: actor.ID, RegisteredAt: m.Svc.cfg.Now()}
		return rec, m.Ring.distribute(m.Svc, &rec)
	})
	return err
}

// prepareRemoval revokes the peer and all recorded manager certificates and
// removes future key envelopes atomically after the Raft removal. A failure
// leaves a durable, fail-closed intent that a later leader can resume.
func (m *MembershipService) prepareRemoval(cert Versioned[PeerCertificateRecord]) error {
	id := cert.Value.Peer.ID
	doc, ok, err := Get[KeyringRecord](m.Svc.store, CollKeys, KeyringKey, Consistent())
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoKeyring
	}
	rec := doc.Value
	delete(rec.Replicas, id)
	for i := range rec.Keys {
		delete(rec.Keys[i].Envelopes, id)
	}
	keyOp, err := PutOp(CollKeys, KeyringKey, rec, IfVersion(doc.Version))
	if err != nil {
		return err
	}
	cr := cert.Value
	cr.Removed = true
	certOp, err := PutOp(CollPeerCertificates, id, cr, IfVersion(cert.Version))
	if err != nil {
		return err
	}
	serials := []string{cr.Serial}
	managers, err := List[ManagerCertRecord](m.Svc.store, CollManagerCerts, Consistent())
	if err != nil {
		return err
	}
	for _, r := range managers {
		if r.Value.Replica == id {
			serials = append(serials, r.Key)
		}
	}
	ops := []metastore.DocOp{keyOp, certOp}
	for _, serial := range serials {
		op, err := PutOp(CollRevocations, serial, certs.Revocation{Serial: serial, RevokedAt: m.Svc.cfg.Now(), Reason: "replica removed: " + id}, nil)
		if err != nil {
			return err
		}
		ops = append(ops, op)
	}
	_, err = m.Svc.store.Txn(ops...)
	return err
}

func mapMembershipError(err error) error {
	if err == metastore.ErrUnsafeMembership || err == metastore.ErrMembershipChanged {
		return fmt.Errorf("%w: %v", api.ErrConflict, err)
	}
	return MapCommandError(err)
}

// PeerRevoked is a follower-safe deny-list hook for metastore TLS handshakes.
// Failure to decode a revocation fails closed. Existing streams require closure
// by the runtime; this hook only authenticates new connections.
func PeerRevoked(store *Store, c *x509.Certificate) bool {
	_, ok, err := store.Raw(CollRevocations, c.SerialNumber.Text(16))
	return err != nil || ok
}

// PeerAllowed tombstones identities once their Raft removal is visible.
// Peers provisioned offline remain accepted unless explicitly tombstoned.
func PeerAllowed(store *Store, id string) bool {
	if change, ok, err := Get[MembershipChange](store, CollMembershipChanges, id); err != nil {
		return false
	} else if ok && change.Value.Remove {
		members, err := store.node.Members()
		if err != nil {
			return false
		}
		if !slices.ContainsFunc(members, func(p metastore.Peer) bool { return p.ID == id }) {
			return false
		}
	}
	peer, ok, err := Get[PeerCertificateRecord](store, CollPeerCertificates, id)
	return err == nil && (!ok || !peer.Value.Removed)
}
