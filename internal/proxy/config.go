// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

// Duration is a time.Duration that is written in JSON as a Go duration
// string such as "1.5s" or "250ms".
type Duration time.Duration

// UnmarshalJSON accepts a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"1s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("duration %q must not be negative", s)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D returns the value as time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the proxy configuration file (JSON).
type Config struct {
	// WriteListen is the address of the write endpoint (required).
	WriteListen string `json:"write_listen"`
	// ReadListen is the optional read endpoint; it requires a policy.read
	// mode other than "disabled".
	ReadListen string `json:"read_listen,omitempty"`
	// ControlListen is the optional status endpoint. It must be a loopback
	// IP literal with port because it is unauthenticated.
	ControlListen string       `json:"control_listen,omitempty"`
	Nodes         []NodeConfig `json:"nodes"`
	Policy        PolicyConfig `json:"policy"`
	Probe         ProbeConfig  `json:"probe"`
	Limits        LimitsConfig `json:"limits"`
	TLS           *TLSConfig   `json:"tls,omitempty"`
}

// NodeConfig is one database node.
type NodeConfig struct {
	ID      string `json:"id"`
	Address string `json:"address"` // host:port of the MariaDB listener
	// Role is "writer-eligible", "reader", "standby" or "excluded".
	Role string `json:"role"`
}

// PolicyConfig maps to routing.Policy. Zero values take routing defaults.
type PolicyConfig struct {
	// Mode is "single-writer-stable" (default), "lowest-latency" or
	// "prefer-node".
	Mode        string `json:"mode,omitempty"`
	PreferredID string `json:"preferred_id,omitempty"`
	// Read is "disabled" (default), "exclude-writer", "include-writer" or
	// "fallback-to-writer".
	Read              string   `json:"read,omitempty"`
	StaleAfter        Duration `json:"stale_after,omitempty"`
	EWMAAlpha         float64  `json:"ewma_alpha,omitempty"`
	SwitchMargin      float64  `json:"switch_margin,omitempty"`
	SwitchConsecutive int      `json:"switch_consecutive,omitempty"`
	DrainDeadline     Duration `json:"drain_deadline,omitempty"`
}

// ProbeConfig configures health and latency measurement.
type ProbeConfig struct {
	// Liveness is "tcp" (default: TCP connect) or "mysql-greeting" (TCP
	// connect plus reading the server greeting, without authentication).
	Liveness string `json:"liveness,omitempty"`
	// Sync is required and selects where the Galera sync state comes from:
	// "galera" queries wsrep status (NOT IMPLEMENTED: always unknown, so no
	// node is ever usable) or "assume-synced", which treats every live node
	// as synced and is only safe where something else guarantees that.
	Sync string `json:"sync"`
	// Interval between probe rounds. Default 1s.
	Interval Duration `json:"interval,omitempty"`
	// Timeout of one probe. Default 1s; must not exceed Interval.
	Timeout Duration `json:"timeout,omitempty"`
	// Samples is the number of latency samples kept per node. Default 10.
	Samples int `json:"samples,omitempty"`
}

// LimitsConfig bounds resource use per listener and connection.
type LimitsConfig struct {
	// MaxConnections per listener. Default 1000.
	MaxConnections int `json:"max_connections,omitempty"`
	// MaxConnectionsPerClient per client IP and listener; 0 = unlimited.
	MaxConnectionsPerClient int `json:"max_connections_per_client,omitempty"`
	// IdleTimeout closes a connection with no traffic in either direction.
	// Default 8h (MariaDB's default wait_timeout).
	IdleTimeout Duration `json:"idle_timeout,omitempty"`
	// DialTimeout bounds the backend connect. Default 5s.
	DialTimeout Duration `json:"dial_timeout,omitempty"`
	// HandshakeTimeout bounds the MySQL/TLS handshake phase when TLS
	// termination is enabled. Default 10s.
	HandshakeTimeout Duration `json:"handshake_timeout,omitempty"`
	// ShutdownGrace is how long a graceful shutdown waits for clients
	// before closing connections. Default 10s.
	ShutdownGrace Duration `json:"shutdown_grace,omitempty"`
}

// TLSConfig enables client-facing TLS termination inside the MySQL protocol.
type TLSConfig struct {
	// CertFile and KeyFile are PEM files (chain first). Relative paths are
	// resolved against the configuration file's directory. They are
	// re-read on reload (SIGHUP).
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	// AllowPlaintext lets clients that do not request TLS through. Default
	// false: application-to-endpoint TLS is the confirmed default.
	AllowPlaintext bool `json:"allow_plaintext,omitempty"`
}

// Defaults not provided by internal/routing.
const (
	DefaultProbeInterval    = time.Second
	DefaultProbeTimeout     = time.Second
	DefaultProbeSamples     = 10
	DefaultMaxConnections   = 1000
	DefaultIdleTimeout      = 8 * time.Hour
	DefaultDialTimeout      = 5 * time.Second
	DefaultHandshakeTimeout = 10 * time.Second
	DefaultShutdownGrace    = 10 * time.Second
)

// Probe sources.
const (
	LivenessTCP           = "tcp"
	LivenessMySQLGreeting = "mysql-greeting"
	SyncGalera            = "galera"
	SyncAssumeSynced      = "assume-synced"
)

// LoadConfig reads, parses and validates a configuration file. Unknown
// fields are rejected. Relative TLS paths are resolved against the file's
// directory.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c, err := ParseConfig(b)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.TLS != nil {
		dir := filepath.Dir(path)
		for _, p := range []*string{&c.TLS.CertFile, &c.TLS.KeyFile} {
			if !filepath.IsAbs(*p) {
				*p = filepath.Join(dir, *p)
			}
		}
	}
	return c, nil
}

// ParseConfig parses and validates JSON and fills defaults.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, err
	}
	if dec.More() {
		return Config{}, errors.New("trailing data after configuration object")
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.Probe.Liveness == "" {
		c.Probe.Liveness = LivenessTCP
	}
	if c.Probe.Interval <= 0 {
		c.Probe.Interval = Duration(DefaultProbeInterval)
	}
	if c.Probe.Timeout <= 0 {
		c.Probe.Timeout = Duration(DefaultProbeTimeout)
	}
	if c.Probe.Samples <= 0 {
		c.Probe.Samples = DefaultProbeSamples
	}
	l := &c.Limits
	if l.MaxConnections <= 0 {
		l.MaxConnections = DefaultMaxConnections
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = Duration(DefaultIdleTimeout)
	}
	if l.DialTimeout <= 0 {
		l.DialTimeout = Duration(DefaultDialTimeout)
	}
	if l.HandshakeTimeout <= 0 {
		l.HandshakeTimeout = Duration(DefaultHandshakeTimeout)
	}
	if l.ShutdownGrace <= 0 {
		l.ShutdownGrace = Duration(DefaultShutdownGrace)
	}
}

// Validate checks a configuration with defaults applied.
func (c Config) Validate() error {
	if c.WriteListen == "" {
		return errors.New("write_listen is required")
	}
	for _, a := range []struct{ name, v string }{{"write_listen", c.WriteListen}, {"read_listen", c.ReadListen}} {
		if a.v == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(a.v); err != nil {
			return fmt.Errorf("%s: %w", a.name, err)
		}
	}
	if c.ControlListen != "" {
		host, _, err := net.SplitHostPort(c.ControlListen)
		if err != nil {
			return fmt.Errorf("control_listen: %w", err)
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("control_listen %q must be a loopback IP literal (the status endpoint is unauthenticated)", c.ControlListen)
		}
	}
	if len(c.Nodes) == 0 {
		return errors.New("nodes: at least one node is required")
	}
	seen := map[string]bool{}
	for i, n := range c.Nodes {
		if n.ID == "" {
			return fmt.Errorf("nodes[%d]: id is required", i)
		}
		if seen[n.ID] {
			return fmt.Errorf("nodes[%d]: duplicate id %q", i, n.ID)
		}
		seen[n.ID] = true
		if _, _, err := net.SplitHostPort(n.Address); err != nil {
			return fmt.Errorf("nodes[%d] %s: address: %w", i, n.ID, err)
		}
		if _, err := ParseRole(n.Role); err != nil {
			return fmt.Errorf("nodes[%d] %s: %w", i, n.ID, err)
		}
	}
	p, err := c.RoutingPolicy()
	if err != nil {
		return err
	}
	if p.Mode == routing.WriterPreferNode && !seen[p.PreferredID] {
		return fmt.Errorf("policy.preferred_id %q is not a configured node", p.PreferredID)
	}
	if c.ReadListen != "" && p.Read == routing.ReadDisabled {
		return errors.New("read_listen requires policy.read other than \"disabled\"")
	}
	switch c.Probe.Liveness {
	case LivenessTCP, LivenessMySQLGreeting:
	default:
		return fmt.Errorf("probe.liveness %q: want %q or %q", c.Probe.Liveness, LivenessTCP, LivenessMySQLGreeting)
	}
	switch c.Probe.Sync {
	case SyncGalera, SyncAssumeSynced:
	case "":
		return fmt.Errorf("probe.sync is required: %q or %q", SyncGalera, SyncAssumeSynced)
	default:
		return fmt.Errorf("probe.sync %q: want %q or %q", c.Probe.Sync, SyncGalera, SyncAssumeSynced)
	}
	if c.Probe.Timeout > c.Probe.Interval {
		return fmt.Errorf("probe.timeout %s must not exceed probe.interval %s", c.Probe.Timeout.D(), c.Probe.Interval.D())
	}
	if p.StaleAfter <= c.Probe.Interval.D()+c.Probe.Timeout.D() {
		return fmt.Errorf("policy.stale_after %s must exceed probe.interval + probe.timeout (%s), or every report would be stale",
			p.StaleAfter, c.Probe.Interval.D()+c.Probe.Timeout.D())
	}
	if c.Limits.MaxConnectionsPerClient < 0 {
		return errors.New("limits.max_connections_per_client must not be negative")
	}
	if c.TLS != nil && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return errors.New("tls: cert_file and key_file are required")
	}
	return nil
}

// ParseRole maps a configuration role name to routing.Role. An empty role is
// rejected rather than defaulted.
func ParseRole(s string) (routing.Role, error) {
	for _, r := range []routing.Role{routing.RoleWriterEligible, routing.RoleReader, routing.RoleStandby, routing.RoleExcluded} {
		if r.String() == s {
			return r, nil
		}
	}
	return routing.RoleExcluded, fmt.Errorf("role %q: want writer-eligible, reader, standby or excluded", s)
}

// RoutingPolicy converts the policy section (routing defaults apply to zero
// fields).
func (c Config) RoutingPolicy() (routing.Policy, error) {
	pc := c.Policy
	p := routing.Policy{
		PreferredID:       pc.PreferredID,
		StaleAfter:        pc.StaleAfter.D(),
		EWMAAlpha:         pc.EWMAAlpha,
		SwitchMargin:      pc.SwitchMargin,
		SwitchConsecutive: pc.SwitchConsecutive,
		DrainDeadline:     pc.DrainDeadline.D(),
	}
	switch strings.TrimSpace(pc.Mode) {
	case "", routing.WriterStable.String():
		p.Mode = routing.WriterStable
	case routing.WriterLowestLatency.String():
		p.Mode = routing.WriterLowestLatency
	case routing.WriterPreferNode.String():
		p.Mode = routing.WriterPreferNode
	default:
		return p, fmt.Errorf("policy.mode %q: want single-writer-stable, lowest-latency or prefer-node", pc.Mode)
	}
	switch pc.Read {
	case "", "disabled":
		p.Read = routing.ReadDisabled
	case "exclude-writer":
		p.Read = routing.ReadExcludeWriter
	case "include-writer":
		p.Read = routing.ReadIncludeWriter
	case "fallback-to-writer":
		p.Read = routing.ReadFallbackToWriter
	default:
		return p, fmt.Errorf("policy.read %q: want disabled, exclude-writer, include-writer or fallback-to-writer", pc.Read)
	}
	if pc.EWMAAlpha < 0 || pc.EWMAAlpha > 1 {
		return p, fmt.Errorf("policy.ewma_alpha %v must be in (0,1]", pc.EWMAAlpha)
	}
	if pc.SwitchMargin < 0 || pc.SwitchMargin >= 1 {
		return p, fmt.Errorf("policy.switch_margin %v must be in (0,1)", pc.SwitchMargin)
	}
	if pc.SwitchConsecutive < 0 {
		return p, errors.New("policy.switch_consecutive must not be negative")
	}
	return routing.New(p).Policy(), nil
}
