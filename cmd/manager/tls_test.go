// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/managertls"
)

func TestListenerRequiresExplicitExposure(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "127.10.1.2:8080"} {
		if err := validateListener(addr, nil); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "192.0.2.1:8080", "localhost:8080", "manager.example.test:8080"} {
		if err := validateListener(addr, nil); err == nil {
			t.Errorf("HTTP exposed on %s", addr)
		}
		if err := validateListener(addr, &tlsConfig{AllowPlainHTTP: true}); err != nil {
			t.Errorf("explicit HTTP rejected: %v", err)
		}
		if err := validateListener(addr, &tlsConfig{ACME: &managertls.Config{Email: "ops@example.test", Domains: []string{"manager.example.test"}}}); err != nil {
			t.Errorf("ACME rejected: %v", err)
		}
	}
	for _, c := range []*tlsConfig{
		{CertFile: "cert"}, {KeyFile: "key"},
		{CertFile: "cert", KeyFile: "key", AllowPlainHTTP: true},
		{ACME: &managertls.Config{}, AllowPlainHTTP: true},
		{ACME: &managertls.Config{}, CertFile: "cert", KeyFile: "key"},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("invalid TLS configuration accepted: %+v", c)
		}
	}
}
func TestACMEConfigLoadedAndValidatedBeforeStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager.json")
	if err := os.WriteFile(path, []byte(`{"manager":{"tls":{"acme":{"email":"ops@example.test","domains":["manager.example.test"]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateListener(":443", cfg.Manager.TLS); err != nil {
		t.Fatal(err)
	}
	if cfg.Manager.TLS.ACME.Defaults().DirectoryURL != managertls.StagingDirectory {
		t.Fatal("production CA used by default")
	}
	if err := run([]string{"-listen", ":8080"}, func(string) string { return "" }, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("expected pre-start HTTP refusal: %v", err)
	}
}
