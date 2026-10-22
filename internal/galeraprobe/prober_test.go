// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe/fakemysql"
)

const testPassword = "s3cr3t-pw-Q9"

func newProber(t *testing.T, mod func(*Config)) *Prober {
	t.Helper()
	cfg := Config{Credentials: StaticCredentials{User: "dbcm_monitor", Password: testPassword}, Timeout: 2 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeStates(t *testing.T) {
	base := fakemysql.SyncedVars()
	tests := []struct {
		name  string
		vars  map[string]string
		mode  string
		state galera.HealthState
	}{
		{"synced", base, "", galera.HealthHealthy},
		{"donor", fakemysql.WithVars(base, "wsrep_local_state", "2", "wsrep_local_state_comment", "Donor/Desynced"), "", galera.HealthDegraded},
		{"joined", fakemysql.WithVars(base, "wsrep_local_state", "3", "wsrep_local_state_comment", "Joined"), "", galera.HealthDegraded},
		{"joining", fakemysql.WithVars(base, "wsrep_local_state", "1", "wsrep_local_state_comment", "Joining: receiving State Transfer"), "", galera.HealthUnhealthy},
		{"non-primary", fakemysql.WithVars(base, "wsrep_cluster_status", "non-Primary", "wsrep_ready", "OFF"), "", galera.HealthUnhealthy},
		{"disconnected", fakemysql.WithVars(base, "wsrep_cluster_status", "Disconnected", "wsrep_connected", "OFF", "wsrep_ready", "OFF", "wsrep_cluster_size", "0"), "", galera.HealthUnhealthy},
		{"missing variable", fakemysql.WithVars(base, "wsrep_ready", ""), "", galera.HealthUnknown},
		{"unexpected status value", fakemysql.WithVars(base, "wsrep_cluster_status", "banana"), "", galera.HealthUnknown},
		{"non-wsrep node (no variables)", map[string]string{}, "", galera.HealthUnknown},
		{"NULL rows", map[string]string{}, "badrows", galera.HealthUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fakemysql.New(t, "dbcm_monitor", testPassword, tc.vars, func(f *fakemysql.Server) { f.Mode = tc.mode })
			h, err := newProber(t, nil).Probe(context.Background(), f.Addr())
			if err != nil {
				t.Fatal(err)
			}
			if h.State != tc.state {
				t.Fatalf("state = %q (%v), want %q", h.State, h.Reasons, tc.state)
			}
			if tc.state != galera.HealthHealthy && h.Healthy() {
				t.Fatal("non-healthy answer counted healthy")
			}
			if got := <-f.QueryLog; got != "SHOW GLOBAL STATUS LIKE 'wsrep_%'" {
				t.Fatalf("query = %q", got)
			}
		})
	}
}

func TestProbeHealthyFields(t *testing.T) {
	f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars())
	h, err := newProber(t, nil).Probe(context.Background(), f.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if !h.Healthy() || !h.InPrimary() || !h.DonorCapable() || h.ClusterSize != 3 || h.StateUUID != "uuid-1" || h.LastCommitted != 42 {
		t.Fatalf("health = %+v", h)
	}
}

func TestOneConnectionPerProbe(t *testing.T) {
	f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars())
	p := newProber(t, nil)
	for range 3 {
		if _, err := p.Probe(context.Background(), f.Addr()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.Accepted.Load(); n != 3 {
		t.Fatalf("accepted %d connections for 3 probes, want 3 (no reuse)", n)
	}
}

func TestAuthFailureIsUnknownAndRedacted(t *testing.T) {
	f := fakemysql.New(t, "dbcm_monitor", "another-password", fakemysql.SyncedVars())
	h, err := newProber(t, nil).Probe(context.Background(), f.Addr())
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
	if h.State != galera.HealthUnknown || h.Healthy() {
		t.Fatalf("health = %+v", h)
	}
	if strings.Contains(err.Error(), testPassword) || strings.Contains(fmt.Sprint(h.Reasons), testPassword) {
		t.Fatalf("password in error: %v", err)
	}
}

func TestErrorTextNeverContainsPassword(t *testing.T) {
	// An error from the driver that echoes the secret must be redacted.
	p := newProber(t, nil)
	err := p.classify("x:1", fmt.Errorf("boom %s boom", testPassword), testPassword)
	if strings.Contains(err.Error(), testPassword) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("err = %v", err)
	}
	// And the credentials value formats without the secret.
	c := Credentials{User: "u", Password: testPassword}
	if s := fmt.Sprintf("%v|%+v|%#v|%s", c, c, c, c); strings.Contains(s, testPassword) {
		t.Fatalf("credentials formatting leaks the password: %s", s)
	}
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Info("x", "cred", c)
	if strings.Contains(b.String(), testPassword) {
		t.Fatalf("slog leaks the password: %s", b.String())
	}
}

func TestTimeoutSilentServer(t *testing.T) {
	f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars(), func(f *fakemysql.Server) { f.Mode = "silent" })
	p := newProber(t, func(c *Config) { c.Timeout = 300 * time.Millisecond })
	start := time.Now()
	h, err := p.Probe(context.Background(), f.Addr())
	if err == nil {
		t.Fatal("silent server answered")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("probe took %s with a 300ms timeout", d)
	}
	if h.State != galera.HealthUnknown {
		t.Fatalf("health = %+v", h)
	}
}

func TestContextCancelStopsProbe(t *testing.T) {
	f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars(), func(f *fakemysql.Server) { f.Mode = "silent" })
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := newProber(t, func(c *Config) { c.Timeout = 10 * time.Second }).Probe(ctx, f.Addr()); err == nil {
		t.Fatal("expected error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("probe ignored the context deadline: %s", d)
	}
}

func TestConnectionRefused(t *testing.T) {
	f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars())
	addr := f.Addr()
	f.StopListening()
	h, err := newProber(t, nil).Probe(context.Background(), addr)
	if err == nil || h.State != galera.HealthUnknown {
		t.Fatalf("err = %v, health = %+v", err, h)
	}
}

func TestGarbageResponseIsUnknown(t *testing.T) {
	f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars(), func(f *fakemysql.Server) { f.Mode = "garbage" })
	h, err := newProber(t, nil).Probe(context.Background(), f.Addr())
	if err == nil {
		t.Fatalf("garbage accepted: %+v", h)
	}
	if h.State != galera.HealthUnknown || h.Healthy() || h.InPrimary() {
		t.Fatalf("health = %+v", h)
	}
}

func TestTLS(t *testing.T) {
	srvCfg, pool := fakemysql.SelfSigned(t)
	t.Run("required and offered", func(t *testing.T) {
		f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars(), func(f *fakemysql.Server) { f.TLSConfig = srvCfg })
		p := newProber(t, func(c *Config) {
			c.TLS = &tls.Config{RootCAs: pool, ServerName: "fake-mariadb", MinVersion: tls.VersionTLS12}
		})
		h, err := p.Probe(context.Background(), f.Addr())
		if err != nil || !h.Healthy() {
			t.Fatalf("health = %+v, err = %v", h, err)
		}
	})
	t.Run("required but not offered", func(t *testing.T) {
		f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars()) // no SSL capability
		p := newProber(t, func(c *Config) { c.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12} })
		h, err := p.Probe(context.Background(), f.Addr())
		if !errors.Is(err, ErrTLSUnavailable) {
			t.Fatalf("err = %v, want ErrTLSUnavailable", err)
		}
		if h.State != galera.HealthUnknown {
			t.Fatalf("health = %+v", h)
		}
		if f.Queries.Load() != 0 {
			t.Fatal("a query was sent over a plaintext fallback")
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars(), func(f *fakemysql.Server) { f.TLSConfig = srvCfg })
		p := newProber(t, func(c *Config) { c.TLS = &tls.Config{MinVersion: tls.VersionTLS12} }) // system roots
		if _, err := p.Probe(context.Background(), f.Addr()); err == nil {
			t.Fatal("untrusted certificate accepted")
		}
	})
	t.Run("server name defaults to the host", func(t *testing.T) {
		f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars(), func(f *fakemysql.Server) { f.TLSConfig = srvCfg }) // certificate has IP SAN 127.0.0.1
		p := newProber(t, func(c *Config) { c.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12} })
		if h, err := p.Probe(context.Background(), f.Addr()); err != nil || !h.Healthy() {
			t.Fatalf("health = %+v, err = %v", h, err)
		}
	})
}

func TestConcurrentProbes(t *testing.T) {
	f := fakemysql.New(t, "dbcm_monitor", testPassword, fakemysql.SyncedVars())
	p := newProber(t, nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h, err := p.Probe(context.Background(), f.Addr()); err != nil || !h.Healthy() {
				t.Errorf("health = %+v, err = %v", h, err)
			}
		}()
	}
	wg.Wait()
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("missing credentials accepted")
	}
	if _, err := New(Config{Credentials: StaticCredentials{}, Timeout: -1}); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

func TestCredentialSources(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := FileCredentials{User: "u", Path: path}.Credentials(context.Background())
	if err != nil || c.Password != "file-secret" || c.User != "u" {
		t.Fatalf("file: %+v %v", c, err)
	}
	if err := os.WriteFile(path, []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := (FileCredentials{User: "u", Path: path}).Credentials(context.Background()); c.Password != "rotated" {
		t.Fatalf("rotation not picked up: %+v", c)
	}
	_ = os.WriteFile(path, []byte("\n"), 0o600)
	if _, err := (FileCredentials{User: "u", Path: path}).Credentials(context.Background()); err == nil {
		t.Fatal("empty password file accepted")
	}
	if _, err := (FileCredentials{User: "u", Path: filepath.Join(dir, "missing")}).Credentials(context.Background()); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := (FileCredentials{Path: path}).Credentials(context.Background()); err == nil {
		t.Fatal("empty user accepted")
	}
	t.Setenv("DBCM_TEST_PW", "env-secret")
	if c, err := (EnvCredentials{User: "u", Var: "DBCM_TEST_PW"}).Credentials(context.Background()); err != nil || c.Password != "env-secret" {
		t.Fatalf("env: %+v %v", c, err)
	}
	if _, err := (EnvCredentials{User: "u", Var: "DBCM_TEST_UNSET_X"}).Credentials(context.Background()); err == nil {
		t.Fatal("unset variable accepted")
	}
}

func TestCredentialErrorFailsProbe(t *testing.T) {
	p := newProber(t, func(c *Config) { c.Credentials = FileCredentials{User: "u", Path: "/nonexistent/pw"} })
	h, err := p.Probe(context.Background(), "127.0.0.1:1")
	if err == nil || h.State != galera.HealthUnknown {
		t.Fatalf("err = %v, health = %+v", err, h)
	}
}

func TestStatusNetUnixSocket(t *testing.T) {
	// Short path: unix socket paths are limited to about 104 bytes on macOS.
	dir, err := os.MkdirTemp("", "gp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "mysqld.sock")
	fakemysql.NewUnix(t, sock, "dbcm_monitor", testPassword, fakemysql.SyncedVars())
	vars, err := newProber(t, nil).StatusNet(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if galera.HealthFromStatus(vars).State != galera.HealthHealthy {
		t.Fatalf("vars %v", vars)
	}
	if _, err := newProber(t, nil).StatusNet(context.Background(), "udp", sock); err == nil {
		t.Fatal("udp accepted")
	}
}
