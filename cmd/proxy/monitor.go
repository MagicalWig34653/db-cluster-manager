// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe"
	"github.com/MagicalWig34653/db-cluster-manager/internal/proxy"
)

// monitorStatus is the proxy's Galera status source. It is swapped on
// configuration reload so monitor settings (user, password source, TLS)
// follow SIGHUP.
type monitorStatus struct {
	cur atomic.Pointer[galeraprobe.ProxyStatus]
}

var errNoMonitor = errors.New("probe.sync is \"galera\" but probe.monitor is not configured")

// GaleraStatus implements proxy.GaleraStatusProber.
func (m *monitorStatus) GaleraStatus(ctx context.Context, addr string) (proxy.GaleraStatus, error) {
	s := m.cur.Load()
	if s == nil {
		return proxy.GaleraStatus{}, errNoMonitor
	}
	return s.GaleraStatus(ctx, addr)
}

// apply builds the prober for cfg. For "assume-synced" it clears the source.
func (m *monitorStatus) apply(cfg proxy.Config) error {
	if cfg.Probe.Sync != proxy.SyncGalera {
		m.cur.Store(nil)
		return nil
	}
	p, err := newMonitorProber(cfg.Probe)
	if err != nil {
		return err
	}
	m.cur.Store(&galeraprobe.ProxyStatus{Prober: p})
	return nil
}

func newMonitorProber(pc proxy.ProbeConfig) (*galeraprobe.Prober, error) {
	mc := pc.Monitor
	if mc == nil {
		return nil, errNoMonitor
	}
	var src galeraprobe.CredentialSource
	if mc.PasswordFile != "" {
		src = galeraprobe.FileCredentials{User: mc.User, Path: mc.PasswordFile}
	} else {
		src = galeraprobe.EnvCredentials{User: mc.User, Var: mc.PasswordEnv}
	}
	cfg := galeraprobe.Config{Credentials: src, Timeout: pc.Timeout.D()}
	if mc.TLS == proxy.MonitorTLSRequired {
		t := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: mc.ServerName}
		if mc.CAFile != "" {
			pem, err := os.ReadFile(mc.CAFile)
			if err != nil {
				return nil, fmt.Errorf("probe.monitor.ca_file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("probe.monitor.ca_file %s: no PEM certificates", mc.CAFile)
			}
			t.RootCAs = pool
		}
		cfg.TLS = t
	}
	return galeraprobe.New(cfg)
}
