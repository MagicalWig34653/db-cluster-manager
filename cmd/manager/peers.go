// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

// peerTLSConfig is the "manager.peerTLS" section: PEM files written by
// `manager -issue-peer-certs` for this replica.
type peerTLSConfig struct {
	Installation string             `json:"installation"`
	CAFile       string             `json:"caFile"`
	CertFile     string             `json:"certFile"`
	KeyFile      string             `json:"keyFile"`
	Renewal      *peerRenewalConfig `json:"renew,omitempty"`
}

// peerRenewalConfig opts in to online custody of the sealed peer CA. Both
// the sealed CA and its passphrase must be provisioned as separate secrets.
type peerRenewalConfig struct {
	CAFile         string `json:"caFile"`
	PassphraseFile string `json:"passphraseFile"`
	BeforeDays     int    `json:"beforeDays,omitempty"`
	ValidityDays   int    `json:"validityDays,omitempty"`
	IntervalHours  int    `json:"intervalHours,omitempty"`
}

func (r *peerRenewalConfig) before() time.Duration {
	if r.BeforeDays > 0 {
		return time.Duration(r.BeforeDays) * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}
func (r *peerRenewalConfig) validity() time.Duration {
	if r.ValidityDays > 0 {
		return time.Duration(r.ValidityDays) * 24 * time.Hour
	}
	return 365 * 24 * time.Hour
}
func (r *peerRenewalConfig) interval() time.Duration {
	if r.IntervalHours > 0 {
		return time.Duration(r.IntervalHours) * time.Hour
	}
	return 12 * time.Hour
}

// load reads the files and returns the metastore peer TLS configuration.
// The certificate is re-read when its file changes, so a renewed
// certificate is used without a restart.
func (p *peerTLSConfig) load() (*metastore.PeerTLS, error) {
	if p.Installation == "" || p.CAFile == "" || p.CertFile == "" || p.KeyFile == "" {
		return nil, errors.New("manager.peerTLS needs installation, caFile, certFile and keyFile")
	}
	ca, err := os.ReadFile(p.CAFile)
	if err != nil {
		return nil, fmt.Errorf("manager.peerTLS: %w", err)
	}
	roots, err := certs.PoolFromBundle(ca)
	if err != nil {
		return nil, fmt.Errorf("manager.peerTLS: ca: %w", err)
	}
	r := &reloadingCert{certFile: p.CertFile, keyFile: p.KeyFile}
	if _, err := r.get(); err != nil {
		return nil, err
	}
	return &metastore.PeerTLS{Installation: p.Installation, Roots: roots, Certificate: r.get}, nil
}

// renewOnce replaces the local key pair before expiry. It uses the identity
// already present in the certificate, never a caller-supplied replica ID.
// New HA configurations use a single bundle, atomically replaced so the
// certificate callback observes either complete key pair.
func (p *peerTLSConfig) renewOnce(ctx context.Context, now func() time.Time) (bool, error) {
	if p.Renewal == nil {
		return false, nil
	}
	r := p.Renewal
	if r.CAFile == "" || r.PassphraseFile == "" || r.before() >= r.validity() || r.interval() <= 0 {
		return false, errors.New("manager.peerTLS.renew needs caFile, passphraseFile and beforeDays less than validityDays")
	}
	current, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile)
	if err != nil {
		return false, err
	}
	leaf, err := x509.ParseCertificate(current.Certificate[0])
	if err != nil {
		return false, err
	}
	id, ok := metastore.PeerIDOf(leaf, p.Installation)
	if !ok {
		return false, metastore.ErrPeerIdentity
	}
	if now().Add(r.before()).Before(leaf.NotAfter) {
		return false, nil
	}
	sealed, err := os.ReadFile(r.CAFile)
	if err != nil {
		return false, err
	}
	pass, err := os.ReadFile(r.PassphraseFile)
	if err != nil {
		return false, err
	}
	ca, err := certs.LoadCA(sealed, strings.TrimSuffix(strings.TrimSuffix(string(pass), "\n"), "\r"), now)
	if err != nil {
		return false, err
	}
	trusted, err := os.ReadFile(p.CAFile)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(bytes.TrimSpace(ca.CertificatePEM()), bytes.TrimSpace(trusted)) {
		return false, errors.New("manager.peerTLS.renew: sealed CA differs from trusted CA")
	}
	iss, err := ca.Issue(ctx, certs.Request{Kind: certs.KindPeer, CommonName: "dbcm metastore " + id,
		URIs: []*url.URL{metastore.PeerURI(p.Installation, id)}, Validity: r.validity()})
	if err != nil {
		return false, err
	}
	key, err := iss.KeyPEM()
	if err != nil {
		return false, err
	}
	if p.CertFile == p.KeyFile {
		bundle := append(append([]byte(nil), iss.ChainPEM...), key...)
		if err := replacePrivateFile(p.CertFile, bundle); err != nil {
			return false, err
		}
	} else {
		// Existing separate-file configurations remain supported, but a
		// crash between the two renames needs manual repair. New HA configs
		// use one atomically replaced PEM bundle.
		if err := replacePrivateFile(p.KeyFile, key); err != nil {
			return false, err
		}
		if err := replacePrivateFile(p.CertFile, iss.ChainPEM); err != nil {
			return false, err
		}
	}
	return true, nil
}

func replacePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".peer-renew-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// reloadingCert serves a key pair from files and reloads it when the
// certificate file's modification time changes.
type reloadingCert struct {
	mu                sync.Mutex
	certFile, keyFile string
	mod               time.Time
	cur               *tls.Certificate
}

func (r *reloadingCert) get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := os.Stat(r.certFile)
	if err != nil {
		if r.cur != nil {
			return r.cur, nil
		}
		return nil, fmt.Errorf("manager.peerTLS: %w", err)
	}
	if r.cur != nil && st.ModTime().Equal(r.mod) {
		return r.cur, nil
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cur != nil {
			return r.cur, nil
		}
		return nil, fmt.Errorf("manager.peerTLS: key pair: %w", err)
	}
	r.cur, r.mod = &c, st.ModTime()
	return r.cur, nil
}

// dataKeys returns the key provider: the replicated keyring with this
// replica's identity, or the plain key file.
func (m *managerConfig) dataKeys(nodeID string) (manager.KeyProvider, *manager.Keyring, error) {
	keyFile := m.KeyFile
	if keyFile == "" {
		keyFile = filepath.Join(m.DataDir, "data.key")
	}
	if !m.Keyring {
		k, err := manager.FileKeyProvider(keyFile)
		return k, nil, err
	}
	id, err := manager.LoadReplicaIdentity(filepath.Join(m.DataDir, "replica.key"))
	if err != nil {
		return nil, nil, err
	}
	kr := &manager.Keyring{ReplicaID: nodeID, Identity: id}
	// An existing key file is adopted when this replica bootstraps the
	// keyring; no new key file is created.
	if b, err := os.ReadFile(keyFile); err == nil {
		if len(b) != 32 {
			return nil, nil, fmt.Errorf("%s holds %d bytes, want 32", keyFile, len(b))
		}
		kr.Seed = b
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	return kr, kr, nil
}

// printReplicaKey prints this replica's public identity for registration
// (PUT /settings/replicas/{id}/data-key-recipient), creating the identity
// if needed.
func printReplicaKey(cfg fileConfig, out io.Writer) error {
	if cfg.Manager == nil || cfg.Manager.DataDir == "" {
		return errors.New("-print-replica-key needs a config with manager.dataDir")
	}
	id, err := manager.LoadReplicaIdentity(filepath.Join(cfg.Manager.DataDir, "replica.key"))
	if err != nil {
		return err
	}
	pub, err := id.StreamPublicKey()
	if err != nil {
		return err
	}
	nodeID := cfg.Manager.NodeID
	if nodeID == "" {
		nodeID = "manager-1"
	}
	_, err = fmt.Fprintf(out, "replica %s public key %s (fingerprint %s)\n", nodeID, pub.String(), pub.ID())
	return err
}

var peerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// issuePeerCerts creates a peer CA and one certificate per replica in dir:
// ca.pem, ca.sealed (the CA, encrypted with passphrase, for later
// issuance) and <id>/peer.pem, <id>/peer-key.pem. Each replica directory
// is copied to its host; ca.sealed and the passphrase stay offline.
func issuePeerCerts(dir, installation, ids, passphrase string, validity time.Duration, now func() time.Time) error {
	if installation == "" || ids == "" {
		return errors.New("-issue-peer-certs needs -installation and -peer-ids")
	}
	if len(passphrase) < 12 {
		return errors.New("-issue-peer-certs needs DBCM_PEER_CA_PASSPHRASE with at least 12 characters")
	}
	list := strings.Split(ids, ",")
	for _, id := range list {
		if !peerIDPattern.MatchString(id) {
			return fmt.Errorf("invalid replica ID %q", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); err == nil {
		return fmt.Errorf("%s already holds a peer CA; refusing to overwrite", dir)
	}
	ca, err := certs.NewCA(certs.CAOptions{CommonName: "dbcm metastore peers " + installation, Validity: 10 * 365 * 24 * time.Hour, Now: now})
	if err != nil {
		return err
	}
	sealedCA, err := ca.Seal(passphrase)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), ca.CertificatePEM(), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.sealed"), sealedCA, 0o600); err != nil {
		return err
	}
	for _, id := range list {
		iss, err := ca.Issue(context.Background(), certs.Request{Kind: certs.KindPeer, CommonName: "dbcm metastore " + id,
			URIs: []*url.URL{metastore.PeerURI(installation, id)}, Validity: validity})
		if err != nil {
			return err
		}
		key, err := iss.KeyPEM()
		if err != nil {
			return err
		}
		pd := filepath.Join(dir, id)
		if err := os.MkdirAll(pd, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(pd, "ca.pem"), ca.CertificatePEM(), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(pd, "peer.pem"), iss.ChainPEM, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(pd, "peer-key.pem"), key, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(pd, "peer-bundle.pem"), append(append([]byte(nil), iss.ChainPEM...), key...), 0o600); err != nil {
			return err
		}
	}
	return nil
}
