// SPDX-License-Identifier: AGPL-3.0-only

// Package dockerclient is a minimal Docker Engine API client that implements
// agent.DockerAPI. It uses only the standard library (net/http and
// encoding/json) and speaks to the daemon over a unix socket or plain TCP.
//
// The request and response shapes follow the Docker Engine API reference,
// version 1.47 (https://docs.docker.com/reference/api/engine/version/v1.47/,
// swagger file v1.47.yaml). Only the fields the agent needs are encoded or
// decoded. The client negotiates the API version with the daemon: it uses
// min(MaxAPIVersion, daemon ApiVersion) and refuses daemons that cannot
// serve at least MinAPIVersion.
//
// Outcome semantics for mutating requests (create, update, delete): a
// response with a 4xx status is a definite failure. A 5xx status, a timeout,
// a cancelled context, a connection lost after the request may have been
// sent, or an unreadable success response is reported wrapped in
// agent.ErrOutcomeUnknown, because the daemon may have applied the change.
// A failure to dial is a definite failure.
//
// Plain TCP (tcp://) has no TLS and no authentication; use it only on an
// isolated lab network. TLS is not implemented.
package dockerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// API versions this client can speak. MaxAPIVersion is the reference version
// the request shapes were written against; MinAPIVersion is the oldest
// version that has every field used here (Limit.Pids needs 1.41).
const (
	MaxAPIVersion = "1.47"
	MinAPIVersion = "1.41"
)

// DefaultHost is the conventional local daemon socket.
const DefaultHost = "unix:///var/run/docker.sock"

// DefaultTimeout bounds a request whose context has no deadline.
const DefaultTimeout = 30 * time.Second

const maxResponseBody = 16 << 20

// Options configure a Client.
type Options struct {
	// Host is "unix:///path/to/docker.sock" or "tcp://host:port".
	Host string
	// APIVersion, if set, pins the API version and disables negotiation.
	APIVersion string
	// Timeout bounds requests whose context has no deadline. Zero means
	// DefaultTimeout; a negative value disables the bound.
	Timeout time.Duration
}

// Client is a Docker Engine API client. It is safe for concurrent use.
type Client struct {
	hc      *http.Client
	base    string // scheme://authority used in request URLs
	timeout time.Duration

	mu      sync.Mutex
	version string // negotiated or pinned; empty until negotiated
}

var _ agent.DockerAPI = (*Client)(nil)

// New returns a Client for o.Host. It does not contact the daemon; the API
// version is negotiated on first use or by NegotiateAPIVersion.
func New(o Options) (*Client, error) {
	if o.Host == "" {
		o.Host = DefaultHost
	}
	u, err := url.Parse(o.Host)
	if err != nil {
		return nil, fmt.Errorf("dockerclient: host %q: %w", o.Host, err)
	}
	tr := &http.Transport{
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // bounded by the request context
		DisableCompression:    true,
	}
	var base string
	switch u.Scheme {
	case "unix":
		path := u.Path
		if u.Host != "" || path == "" {
			return nil, fmt.Errorf("dockerclient: host %q: want unix:///absolute/path", o.Host)
		}
		d := net.Dialer{Timeout: 5 * time.Second}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", path)
		}
		base = "http://docker"
	case "tcp":
		if u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("dockerclient: host %q: want tcp://host:port", o.Host)
		}
		if _, _, err := net.SplitHostPort(u.Host); err != nil {
			return nil, fmt.Errorf("dockerclient: host %q: %w", o.Host, err)
		}
		d := net.Dialer{Timeout: 5 * time.Second}
		tr.DialContext = d.DialContext
		base = "http://" + u.Host
	default:
		return nil, fmt.Errorf("dockerclient: host %q: unsupported scheme (want unix or tcp)", o.Host)
	}
	if o.APIVersion != "" {
		if _, _, err := parseVersion(o.APIVersion); err != nil {
			return nil, err
		}
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		hc:      &http.Client{Transport: tr, CheckRedirect: noRedirect},
		base:    base,
		timeout: timeout,
		version: o.APIVersion,
	}, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Close releases idle connections.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// APIVersion returns the negotiated or pinned API version, or "" before
// negotiation.
func (c *Client) APIVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// PingInfo is the result of GET /_ping.
type PingInfo struct {
	APIVersion string // Api-Version response header
	Swarm      string // Swarm response header, for example "active/manager"
}

// Ping calls GET /_ping (unversioned).
func (c *Client) Ping(ctx context.Context) (PingInfo, error) {
	resp, err := c.send(ctx, http.MethodGet, "/_ping", nil, nil, false)
	if err != nil {
		return PingInfo{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return PingInfo{APIVersion: resp.Header.Get("Api-Version"), Swarm: resp.Header.Get("Swarm")}, nil
}

// VersionInfo is the subset of the SystemVersion object used here.
type VersionInfo struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
}

// Version calls GET /version (unversioned).
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var v VersionInfo
	err := c.doRaw(ctx, http.MethodGet, "/version", nil, nil, &v, false)
	return v, err
}

// ErrAPIVersion is returned when client and daemon share no API version.
var ErrAPIVersion = errors.New("dockerclient: no common API version")

// NegotiateAPIVersion asks the daemon for its version range and selects
// min(MaxAPIVersion, daemon ApiVersion). It fails with ErrAPIVersion if the
// result is below MinAPIVersion or below the daemon's MinAPIVersion. With a
// pinned version it only returns that version.
func (c *Client) NegotiateAPIVersion(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != "" {
		return c.version, nil
	}
	v, err := c.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("dockerclient: negotiate API version: %w", err)
	}
	chosen, err := negotiate(v.APIVersion, v.MinAPIVersion)
	if err != nil {
		return "", err
	}
	c.version = chosen
	return chosen, nil
}

func negotiate(server, serverMin string) (string, error) {
	if server == "" {
		return "", fmt.Errorf("%w: daemon reported no ApiVersion", ErrAPIVersion)
	}
	if _, _, err := parseVersion(server); err != nil {
		return "", fmt.Errorf("%w: daemon ApiVersion: %v", ErrAPIVersion, err)
	}
	chosen := MaxAPIVersion
	if versionLess(server, chosen) {
		chosen = server
	}
	if versionLess(chosen, MinAPIVersion) {
		return "", fmt.Errorf("%w: daemon API %s is older than the minimum %s", ErrAPIVersion, server, MinAPIVersion)
	}
	if serverMin != "" {
		if _, _, err := parseVersion(serverMin); err != nil {
			return "", fmt.Errorf("%w: daemon MinAPIVersion: %v", ErrAPIVersion, err)
		}
		if versionLess(chosen, serverMin) {
			return "", fmt.Errorf("%w: daemon requires API >= %s, client supports <= %s", ErrAPIVersion, serverMin, MaxAPIVersion)
		}
	}
	return chosen, nil
}

func parseVersion(v string) (int, int, error) {
	maj, min, ok := strings.Cut(v, ".")
	a, err1 := strconv.Atoi(maj)
	b, err2 := strconv.Atoi(min)
	if !ok || err1 != nil || err2 != nil || a < 0 || b < 0 {
		return 0, 0, fmt.Errorf("dockerclient: malformed API version %q", v)
	}
	return a, b, nil
}

// versionLess compares two well-formed "major.minor" versions.
func versionLess(a, b string) bool {
	am, an, _ := parseVersion(a)
	bm, bn, _ := parseVersion(b)
	return am < bm || (am == bm && an < bn)
}

// do sends a request to a versioned path (for example "/services/create").
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any, mutating bool) error {
	ver, err := c.NegotiateAPIVersion(ctx)
	if err != nil {
		return err
	}
	return c.doRaw(ctx, method, "/v"+ver+path, query, body, out, mutating)
}

// doRaw sends a request and decodes a JSON success body into out (if not nil).
func (c *Client) doRaw(ctx context.Context, method, path string, query url.Values, body, out any, mutating bool) error {
	if c.timeout > 0 {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.timeout)
			defer cancel()
		}
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("dockerclient: encode request: %w", err)
		}
	}
	if query != nil {
		path += "?" + query.Encode()
	}
	resp, err := c.send(ctx, method, path, payload, body, mutating)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		err = fmt.Errorf("dockerclient: decode %s %s response: %w", method, stripQuery(path), err)
		if mutating {
			return fmt.Errorf("%w: %w", agent.ErrOutcomeUnknown, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", ctxErr, err)
		}
		return err
	}
	return nil
}

// send performs the HTTP exchange and turns transport failures and error
// statuses into typed errors. On success the caller owns resp.Body.
func (c *Client) send(ctx context.Context, method, path string, payload []byte, body any, mutating bool) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, fmt.Errorf("dockerclient: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "dbcm-agent")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, transportError(ctx, method, stripQuery(path), err, mutating)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	apiErr := &APIError{StatusCode: resp.StatusCode, Method: method, Path: stripQuery(path), Mutating: mutating}
	var er struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &er) == nil && er.Message != "" {
		apiErr.Message = er.Message
	} else {
		apiErr.Message = strings.TrimSpace(string(raw))
	}
	return nil, apiErr
}

func transportError(ctx context.Context, method, path string, err error, mutating bool) error {
	base := fmt.Errorf("dockerclient: %s %s: %w", method, path, err)
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		base = fmt.Errorf("%w: %w", ctxErr, base)
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return base // never reached the daemon
	}
	if mutating {
		return fmt.Errorf("%w: %w", agent.ErrOutcomeUnknown, base)
	}
	return base
}

func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
