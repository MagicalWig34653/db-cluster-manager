// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

func TestIssuePeerCerts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	if err := issuePeerCerts(dir, "inst-1", "m1,m2", "short", time.Hour, time.Now); err == nil {
		t.Fatal("short passphrase accepted")
	}
	if err := issuePeerCerts(dir, "inst-1", "m1,bad/id", "a long passphrase", time.Hour, time.Now); err == nil {
		t.Fatal("bad replica ID accepted")
	}
	if err := issuePeerCerts(dir, "inst-1", "m1,m2", "a long passphrase", time.Hour, time.Now); err != nil {
		t.Fatal(err)
	}
	if err := issuePeerCerts(dir, "inst-1", "m1", "a long passphrase", time.Hour, time.Now); err == nil {
		t.Fatal("existing CA overwritten")
	}
	for _, id := range []string{"m1", "m2"} {
		b, err := os.ReadFile(filepath.Join(dir, id, "peer.pem"))
		if err != nil {
			t.Fatal(err)
		}
		blk, _ := pem.Decode(b)
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := metastore.PeerIDOf(c, "inst-1"); !ok || got != id {
			t.Fatalf("%s identity %q %v", id, got, ok)
		}
		if st, err := os.Stat(filepath.Join(dir, id, "peer-key.pem")); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode %v %v", st, err)
		}
		if st, err := os.Stat(filepath.Join(dir, id, "peer-bundle.pem")); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("bundle file mode %v %v", st, err)
		}
	}
	if st, err := os.Stat(filepath.Join(dir, "ca.sealed")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("sealed CA %v %v", st, err)
	}
}

func TestPeerCertificateRenewalBeforeExpiryWithoutReload(t *testing.T) {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := start
	dir := filepath.Join(t.TempDir(), "peer")
	if err := issuePeerCerts(dir, "inst-1", "m1", "a long passphrase", 48*time.Hour, func() time.Time { return clock }); err != nil {
		t.Fatal(err)
	}
	pass := filepath.Join(t.TempDir(), "passphrase")
	if err := os.WriteFile(pass, []byte("a long passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &peerTLSConfig{Installation: "inst-1", CAFile: filepath.Join(dir, "ca.pem"),
		CertFile: filepath.Join(dir, "m1", "peer-bundle.pem"), KeyFile: filepath.Join(dir, "m1", "peer-bundle.pem"),
		Renewal: &peerRenewalConfig{CAFile: filepath.Join(dir, "ca.sealed"), PassphraseFile: pass, BeforeDays: 1, ValidityDays: 30}}
	tlsConfig, err := p.load()
	if err != nil {
		t.Fatal(err)
	}
	old, err := tlsConfig.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := p.renewOnce(context.Background(), func() time.Time { return clock }); err != nil || changed {
		t.Fatalf("early renewal: changed=%v err=%v", changed, err)
	}
	clock = start.Add(25 * time.Hour)
	if changed, err := p.renewOnce(context.Background(), func() time.Time { return clock }); err != nil || !changed {
		t.Fatalf("due renewal: changed=%v err=%v", changed, err)
	}
	newCert, err := tlsConfig.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	if string(newCert.Certificate[0]) == string(old.Certificate[0]) {
		t.Fatal("cached certificate did not reload")
	}
	leaf, err := x509.ParseCertificate(newCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := metastore.PeerIDOf(leaf, "inst-1"); !ok || id != "m1" || !leaf.NotAfter.After(start.Add(48*time.Hour)) {
		t.Fatalf("renewed identity or expiry: %s %v %s", id, ok, leaf.NotAfter)
	}
	clock = start.Add(49 * time.Hour) // old certificate is expired
	tlsConfig.Now = func() time.Time { return clock }
	if err := tlsConfig.ClientConfig("m1:7445").VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err != nil {
		t.Fatalf("renewed certificate rejected after old expiry: %v", err)
	}
}

func TestForwardConfigurationRejectsAmbiguousReplicaAddresses(t *testing.T) {
	base := func() managerConfig {
		return managerConfig{NodeID: "m1", DataDir: "/tmp/m1", Bind: "m1:7445", Advertise: "m1:7445",
			PeerTLS: &peerTLSConfig{}, APIForwardBind: "m1:7447", Peers: []peerConfig{
				{ID: "m1", Address: "m1:7445", APIAddress: "m1:7447"},
				{ID: "m2", Address: "m2:7445", APIAddress: "m2:7447"},
				{ID: "m3", Address: "m3:7445", APIAddress: "m3:7447"},
			}}
	}
	valid := base()
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*managerConfig){
		"duplicate":        func(m *managerConfig) { m.Peers[2].APIAddress = m.Peers[1].APIAddress },
		"self mismatch":    func(m *managerConfig) { m.Peers[0].APIAddress = "m1:7448" },
		"missing TLS":      func(m *managerConfig) { m.PeerTLS = nil },
		"missing listener": func(m *managerConfig) { m.APIForwardBind = "" },
	} {
		m := base()
		change(&m)
		if err := m.validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestManagedModeKeyringAdoptsKeyFileOverPeerTLS: an installation that
// used the plain key file switches to the replicated keyring and the TLS
// metastore transport; the administrator sealed before stays readable
// (the key file became the first keyring key) and the keyring endpoint
// reports this replica as holding the key.
func TestManagedModeKeyringAdoptsKeyFileOverPeerTLS(t *testing.T) {
	dir := t.TempDir()
	bind := freeLoopback(t)
	pki := filepath.Join(t.TempDir(), "pki")
	if err := issuePeerCerts(pki, "inst-1", "manager-1", "a long passphrase", time.Hour, time.Now); err != nil {
		t.Fatal(err)
	}
	auth := map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}}
	plain := writeConfig(t, map[string]any{"auth": auth, "manager": map[string]any{"dataDir": dir, "bind": bind}})
	cfg, err := loadConfig(plain)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rt, err := newManagedRuntime(cfg, quietLog(), &out)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "leader API", func() bool { return rt.stack.Load() != nil })
	st := rt.stack.Load()
	if err := st.Accounts.Create(recovery.Account{ID: "admin", Credentials: []recovery.Credential{{ID: "pk", Kind: recovery.CredentialPasskey}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Policy.Bind(authz.Binding{Subject: authz.Subject{ID: "admin", Kind: authz.SubjectUser}, Role: authz.RoleAdministrator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}

	md := filepath.Join(pki, "manager-1")
	withKeyring := writeConfig(t, map[string]any{"auth": auth, "manager": map[string]any{"dataDir": dir, "bind": bind, "keyring": true,
		"peerTLS": map[string]any{"installation": "inst-1", "caFile": filepath.Join(md, "ca.pem"),
			"certFile": filepath.Join(md, "peer.pem"), "keyFile": filepath.Join(md, "peer-key.pem")}}})
	cfg, err = loadConfig(withKeyring)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	rt, err = newManagedRuntime(cfg, quietLog(), &out)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	mux := newMux(rt)
	waitUntil(t, "leader API with keyring", func() bool { return rt.stack.Load() != nil })
	if rec := serve(t, mux, "GET", "/api/v1/setup/status", ""); !strings.Contains(rec.Body.String(), `"setupRequired":false`) {
		t.Fatalf("administrator lost after switching to the keyring: %s", rec.Body.String())
	}
	if out.Len() != 0 {
		t.Fatalf("setup token printed: %q", out.String())
	}
	rec, ok, err := manager.Get[manager.KeyringRecord](rt.svc.Store(), manager.CollKeys, manager.KeyringKey)
	if err != nil || !ok || rec.Value.Current != manager.LegacyKeyID {
		t.Fatalf("keyring %+v %v %v", rec.Value, ok, err)
	}
	if _, ok := rec.Value.Keys[0].Envelopes["manager-1"]; !ok {
		t.Fatal("no envelope for this replica")
	}
	var pk bytes.Buffer
	if err := printReplicaKey(cfg, &pk); err != nil || !strings.Contains(pk.String(), "replica manager-1 public key dbcmpk1:") {
		t.Fatalf("print key %q %v", pk.String(), err)
	}
	// Without a session the keyring endpoint is authenticated, not 503.
	if r := serve(t, mux, "GET", "/api/v1/settings/replicas", ""); r.Code != 401 {
		t.Fatalf("replicas without session = %d", r.Code)
	}
}
