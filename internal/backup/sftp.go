// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SFTPConfig configures an SFTP destination (B01/B07).
type SFTPConfig struct {
	Addr string // host:port
	User string
	// Dir is the remote directory holding the objects; it is created (0700)
	// on first use if missing.
	Dir string

	// Host key verification is mandatory: set exactly one of
	// KnownHostsFile (OpenSSH known_hosts format) or HostKeyFingerprint
	// ("SHA256:..." as printed by ssh-keygen -l).
	KnownHostsFile     string
	HostKeyFingerprint string

	// Authentication: at least one of Password and PrivateKeyPEM.
	Password      string
	PrivateKeyPEM []byte
	Passphrase    []byte

	// MaxConcurrency bounds simultaneous operations (default 4).
	MaxConcurrency int
	DialTimeout    time.Duration // default 15s
}

// SFTPBackend stores objects as files in one remote directory. Uploads go to
// a temporary name and are renamed into place.
type SFTPBackend struct {
	cfg  SFTPConfig
	auth []ssh.AuthMethod
	hkcb ssh.HostKeyCallback
	sem  chan struct{}

	mu     sync.Mutex
	ssh    *ssh.Client
	client *sftp.Client
	made   bool
}

// NewSFTPBackend validates cfg; it does no network I/O.
func NewSFTPBackend(cfg SFTPConfig) (*SFTPBackend, error) {
	if cfg.Addr == "" || cfg.User == "" || cfg.Dir == "" {
		return nil, errors.New("backup: sftp config needs addr, user and dir")
	}
	b := &SFTPBackend{cfg: cfg}
	switch {
	case cfg.KnownHostsFile != "" && cfg.HostKeyFingerprint != "":
		return nil, errors.New("backup: sftp: set either known_hosts file or fingerprint, not both")
	case cfg.KnownHostsFile != "":
		cb, err := knownhosts.New(cfg.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("backup: sftp known_hosts: %w", err)
		}
		b.hkcb = cb
	case cfg.HostKeyFingerprint != "":
		if !strings.HasPrefix(cfg.HostKeyFingerprint, "SHA256:") {
			return nil, errors.New("backup: sftp fingerprint must look like SHA256:...")
		}
		want := cfg.HostKeyFingerprint
		b.hkcb = func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(key); got != want {
				return fmt.Errorf("backup: sftp host key mismatch: got %s, want %s", got, want)
			}
			return nil
		}
	default:
		return nil, errors.New("backup: sftp host key verification is required (known_hosts file or fingerprint)")
	}
	if cfg.Password != "" {
		b.auth = append(b.auth, ssh.Password(cfg.Password))
	}
	if len(cfg.PrivateKeyPEM) > 0 {
		var s ssh.Signer
		var err error
		if len(cfg.Passphrase) > 0 {
			s, err = ssh.ParsePrivateKeyWithPassphrase(cfg.PrivateKeyPEM, cfg.Passphrase)
		} else {
			s, err = ssh.ParsePrivateKey(cfg.PrivateKeyPEM)
		}
		if err != nil {
			return nil, fmt.Errorf("backup: sftp private key: %w", err)
		}
		b.auth = append(b.auth, ssh.PublicKeys(s))
	}
	if len(b.auth) == 0 {
		return nil, errors.New("backup: sftp needs a password or private key")
	}
	n := cfg.MaxConcurrency
	if n <= 0 {
		n = 4
	}
	b.sem = make(chan struct{}, n)
	if b.cfg.DialTimeout <= 0 {
		b.cfg.DialTimeout = 15 * time.Second
	}
	return b, nil
}

// Close drops the connection.
func (b *SFTPBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reset()
}

func (b *SFTPBackend) reset() error {
	var err error
	if b.client != nil {
		_ = b.client.Close()
	}
	if b.ssh != nil {
		err = b.ssh.Close()
	}
	b.client, b.ssh = nil, nil
	return err
}

func (b *SFTPBackend) acquire(ctx context.Context) (func(), error) {
	select {
	case b.sem <- struct{}{}:
		return func() { <-b.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *SFTPBackend) conn(ctx context.Context) (*sftp.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		return b.client, nil
	}
	d := net.Dialer{Timeout: b.cfg.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", b.cfg.Addr)
	if err != nil {
		return nil, err
	}
	// The handshake honors ctx by closing the connection on cancellation.
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	_ = nc.SetDeadline(time.Now().Add(b.cfg.DialTimeout))
	sc, chans, reqs, err := ssh.NewClientConn(nc, b.cfg.Addr, &ssh.ClientConfig{
		User: b.cfg.User, Auth: b.auth, HostKeyCallback: b.hkcb, Timeout: b.cfg.DialTimeout,
	})
	if !stop() && err == nil {
		_ = sc.Close()
		err = ctx.Err()
	}
	if err != nil {
		_ = nc.Close()
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	sshc := ssh.NewClient(sc, chans, reqs)
	cl, err := sftp.NewClient(sshc)
	if err != nil {
		_ = sshc.Close()
		return nil, err
	}
	b.ssh, b.client = sshc, cl
	if !b.made {
		if err := cl.MkdirAll(b.cfg.Dir); err != nil {
			_ = cl.Close()
			_ = sshc.Close()
			b.client, b.ssh = nil, nil
			return nil, fmt.Errorf("backup: sftp mkdir %s: %w", b.cfg.Dir, err)
		}
		b.made = true
	}
	return cl, nil
}

// op runs f with a connection, retrying once on a fresh connection if the
// old one was dead.
func (b *SFTPBackend) op(ctx context.Context, f func(*sftp.Client) error) error {
	rel, err := b.acquire(ctx)
	if err != nil {
		return err
	}
	defer rel()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		cl, err := b.conn(ctx)
		if err != nil {
			return err
		}
		err = f(cl)
		if err != nil && attempt == 0 && connDead(err) {
			b.mu.Lock()
			if b.client == cl {
				_ = b.reset()
			}
			b.mu.Unlock()
			continue
		}
		if cerr := ctx.Err(); cerr != nil && err != nil {
			return cerr
		}
		return err
	}
}

func connDead(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		strings.Contains(err.Error(), "connection lost") || strings.Contains(err.Error(), "EOF")
}

func (b *SFTPBackend) p(name string) string { return path.Join(b.cfg.Dir, name) }

func notExist(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var se *sftp.StatusError
	return errors.As(err, &se) && se.FxCode() == sftp.ErrSSHFxNoSuchFile
}

// Put implements Backend.
func (b *SFTPBackend) Put(ctx context.Context, name string, r io.Reader) error {
	if err := validName(name); err != nil {
		return err
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := b.p(".tmp-" + hex.EncodeToString(rnd[:]))
	return b.op(ctx, func(cl *sftp.Client) (err error) {
		f, err := cl.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			return err
		}
		defer func() {
			if err != nil {
				_ = f.Close()
				_ = cl.Remove(tmp)
			}
		}()
		if _, err = io.Copy(f, &ctxReader{ctx, r}); err != nil {
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		dst := b.p(name)
		if err = cl.PosixRename(tmp, dst); err != nil {
			// Servers without posix-rename: replace non-atomically.
			_ = cl.Remove(dst)
			err = cl.Rename(tmp, dst)
		}
		return err
	})
}

type ctxReadCloser struct {
	ctx context.Context
	rc  io.ReadCloser
	rel func()
}

func (c *ctxReadCloser) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.rc.Read(p)
}

func (c *ctxReadCloser) Close() error {
	err := c.rc.Close()
	c.rel()
	return err
}

// Get implements Backend. The returned reader holds one concurrency slot
// until closed.
func (b *SFTPBackend) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	rel, err := b.acquire(ctx)
	if err != nil {
		return nil, err
	}
	var rc io.ReadCloser
	for attempt := 0; ; attempt++ {
		if err = ctx.Err(); err != nil {
			break
		}
		var cl *sftp.Client
		if cl, err = b.conn(ctx); err != nil {
			break
		}
		var f *sftp.File
		f, err = cl.Open(b.p(name))
		if err == nil {
			rc = f
			break
		}
		if attempt == 0 && connDead(err) {
			b.mu.Lock()
			if b.client == cl {
				_ = b.reset()
			}
			b.mu.Unlock()
			continue
		}
		break
	}
	if err != nil {
		rel()
		if notExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &ctxReadCloser{ctx, rc, rel}, nil
}

// List implements Backend.
func (b *SFTPBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := b.op(ctx, func(cl *sftp.Client) error {
		out = out[:0]
		fis, err := cl.ReadDir(b.cfg.Dir)
		if err != nil {
			return err
		}
		for _, fi := range fis {
			n := fi.Name()
			if fi.IsDir() || strings.HasPrefix(n, ".tmp-") || !strings.HasPrefix(n, prefix) {
				continue
			}
			out = append(out, ObjectInfo{Name: n, Size: fi.Size(), ModTime: fi.ModTime()})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete implements Backend.
func (b *SFTPBackend) Delete(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	err := b.op(ctx, func(cl *sftp.Client) error { return cl.Remove(b.p(name)) })
	if err != nil && notExist(err) {
		return ErrNotFound
	}
	return err
}

var _ Backend = (*SFTPBackend)(nil)
