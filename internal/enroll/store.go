// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

// File names in the agent's credential directory.
const (
	KeyFile   = "agent.key"
	ChainFile = "agent.crt"
	CAFile    = "ca.crt"
)

// FileStore keeps an agent's credentials in a directory (mode 0700; the key
// file 0600). Save replaces all three files; each write is atomic, and the
// certificate is written last so a crash leaves either the old pair or a
// key whose certificate does not match (detected on Load).
type FileStore struct{ Dir string }

// ErrNoCredentials is returned by Load when the agent has not enrolled.
var ErrNoCredentials = errors.New("enroll: no stored credentials")

// Load reads and checks the stored credentials.
func (s FileStore) Load() (*Credentials, error) {
	var c Credentials
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{{KeyFile, &c.KeyPEM}, {ChainFile, &c.ChainPEM}, {CAFile, &c.CAPEM}} {
		b, err := os.ReadFile(filepath.Join(s.Dir, f.name))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoCredentials
		}
		if err != nil {
			return nil, err
		}
		*f.dst = b
	}
	if _, err := c.TLSCertificate(); err != nil {
		return nil, fmt.Errorf("enroll: stored key and certificate do not match: %w", err)
	}
	if _, err := c.CAPool(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the credentials.
func (s FileStore) Save(c *Credentials) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{{CAFile, c.CAPEM, 0o644}, {KeyFile, c.KeyPEM, 0o600}, {ChainFile, c.ChainPEM, 0o644}} {
		if err := writeAtomic(filepath.Join(s.Dir, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Holder serves the current credentials to TLS callbacks and swaps them on
// renewal.
type Holder struct {
	mu   sync.RWMutex
	cred *Credentials
	cert *tls.Certificate
}

// NewHolder checks c and returns a Holder.
func NewHolder(c *Credentials) (*Holder, error) {
	h := &Holder{}
	return h, h.Set(c)
}

// Set replaces the credentials.
func (h *Holder) Set(c *Credentials) error {
	cert, err := c.TLSCertificate()
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.cred, h.cert = c, cert
	h.mu.Unlock()
	return nil
}

// Certificate returns the current key pair.
func (h *Holder) Certificate() (*tls.Certificate, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cert, nil
}

// Credentials returns the current credentials.
func (h *Holder) Credentials() *Credentials {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cred
}

// Renewer renews the agent certificate before it expires.
type Renewer struct {
	Config RenewConfig // Current is taken from Holder
	Holder *Holder
	Store  FileStore
	Policy certs.Policy
	// Interval between checks (default 1h); RetryInterval after a failure
	// (default 5m).
	Interval, RetryInterval time.Duration
	Log                     *slog.Logger
}

// Once renews if the policy says so. It reports whether it renewed.
func (r *Renewer) Once(ctx context.Context) (bool, error) {
	cur := r.Holder.Credentials()
	cert, err := cur.TLSCertificate()
	if err != nil {
		return false, err
	}
	now := time.Now()
	if r.Config.Now != nil {
		now = r.Config.Now()
	}
	if !certs.NeedsRenewal(cert.Leaf, now, r.Policy) {
		return false, nil
	}
	cfg := r.Config
	cfg.Current = cur
	next, err := Renew(ctx, cfg)
	if err != nil {
		return false, err
	}
	// Persist first: a restart must not fall back to the old key once the
	// manager recorded the new serial.
	if err := r.Store.Save(next); err != nil {
		return false, err
	}
	return true, r.Holder.Set(next)
}

// Run checks periodically until ctx ends.
func (r *Renewer) Run(ctx context.Context) {
	iv, retry := r.Interval, r.RetryInterval
	if iv <= 0 {
		iv = time.Hour
	}
	if retry <= 0 {
		retry = 5 * time.Minute
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	for {
		wait := iv
		renewed, err := r.Once(ctx)
		switch {
		case err != nil:
			log.Warn("agent: certificate renewal failed", "err", err)
			wait = retry
		case renewed:
			log.Info("agent: certificate renewed")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
