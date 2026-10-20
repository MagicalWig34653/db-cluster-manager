// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

const validJSON = `{
  "write_listen": "0.0.0.0:3306",
  "read_listen": "0.0.0.0:3307",
  "control_listen": "127.0.0.1:9306",
  "nodes": [
    {"id": "db1", "address": "10.0.0.1:3306", "role": "writer-eligible"},
    {"id": "db2", "address": "10.0.0.2:3306", "role": "reader"}
  ],
  "policy": {"mode": "prefer-node", "preferred_id": "db1", "read": "fallback-to-writer", "drain_deadline": "5s"},
  "probe": {"sync": "assume-synced", "liveness": "mysql-greeting", "interval": "500ms", "timeout": "250ms"},
  "tls": {"cert_file": "proxy.pem", "key_file": "proxy.key"}
}`

func TestParseConfigDefaultsAndPolicy(t *testing.T) {
	c, err := ParseConfig([]byte(validJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxConnections != DefaultMaxConnections || c.Limits.IdleTimeout.D() != DefaultIdleTimeout ||
		c.Limits.DialTimeout.D() != DefaultDialTimeout || c.Probe.Samples != DefaultProbeSamples {
		t.Fatalf("defaults not applied: %+v", c.Limits)
	}
	p, err := c.RoutingPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != routing.WriterPreferNode || p.PreferredID != "db1" || p.Read != routing.ReadFallbackToWriter ||
		p.DrainDeadline != 5*time.Second || p.StaleAfter != routing.DefaultStaleAfter {
		t.Fatalf("policy = %+v", p)
	}
}

func TestLoadConfigResolvesTLSPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(path, []byte(validJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.TLS.CertFile != filepath.Join(dir, "proxy.pem") {
		t.Fatalf("cert path %q", c.TLS.CertFile)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"unknown field":         {`"write_listen"`, `"bogus": 1, "write_listen"`, "unknown field"},
		"non-loopback control":  {`127.0.0.1:9306`, `0.0.0.0:9306`, "loopback"},
		"hostname control":      {`127.0.0.1:9306`, `localhost:9306`, "loopback"},
		"missing sync":          {`"sync": "assume-synced",`, ``, "probe.sync is required"},
		"bad role":              {`"role": "reader"`, `"role": ""`, "role"},
		"duplicate id":          {`"id": "db2"`, `"id": "db1"`, "duplicate"},
		"preferred unknown":     {`"preferred_id": "db1"`, `"preferred_id": "dbX"`, "preferred_id"},
		"read without policy":   {`"read": "fallback-to-writer",`, ``, "read_listen requires"},
		"bad mode":              {`"prefer-node"`, `"round-robin"`, "policy.mode"},
		"timeout over interval": {`"timeout": "250ms"`, `"timeout": "2s"`, "must not exceed"},
		"stale too short":       {`"drain_deadline": "5s"`, `"drain_deadline": "5s", "stale_after": "600ms"`, "stale_after"},
		"bad duration":          {`"5s"`, `"5 seconds"`, "duration"},
		"bad address":           {`10.0.0.2:3306`, `10.0.0.2`, "address"},
		"tls without key":       {`"key_file": "proxy.key"`, `"key_file": ""`, "key_file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := strings.Replace(validJSON, tc.from, tc.to, 1)
			if in == validJSON {
				t.Fatalf("replacement %q not found", tc.from)
			}
			_, err := ParseConfig([]byte(in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := ParseConfig([]byte(validJSON + "{}")); err == nil {
		t.Fatal("trailing data accepted")
	}
}
