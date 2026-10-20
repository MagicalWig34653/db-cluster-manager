// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Webhook request headers.
const (
	HeaderTimestamp = "X-Notify-Timestamp"
	HeaderSignature = "X-Notify-Signature"
)

// ErrBlockedAddress is returned when the webhook target resolves to a
// loopback, link-local, private or otherwise non-public address and
// AllowPrivate is not set.
var ErrBlockedAddress = errors.New("webhook target address is not allowed")

// Resolver resolves a host name to IP addresses.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// WebhookConfig configures a Webhook channel.
type WebhookConfig struct {
	Name         string // channel name, default "webhook"
	URL          string
	Secret       []byte        // HMAC-SHA256 key, required
	Timeout      time.Duration // per attempt, default 10s
	MaxRetries   int           // retries after the first attempt
	Backoff      time.Duration // base delay, doubled per retry; default 1s
	AllowPrivate bool          // allow loopback/link-local/private targets
	Resolver     Resolver      // default: system resolver
	Sleep        func(time.Duration)
	Now          Clock
}

// Webhook delivers events as signed JSON POST requests.
type Webhook struct {
	cfg    WebhookConfig
	target *url.URL
}

// NewWebhook validates cfg and creates a Webhook.
func NewWebhook(cfg WebhookConfig) (*Webhook, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("webhook url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("webhook url must use http or https")
	}
	if u.Hostname() == "" {
		return nil, errors.New("webhook url has no host")
	}
	if u.User != nil {
		return nil, errors.New("webhook url must not contain credentials")
	}
	if len(cfg.Secret) == 0 {
		return nil, errors.New("webhook secret is required")
	}
	if cfg.Name == "" {
		cfg.Name = "webhook"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Resolver == nil {
		cfg.Resolver = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, len(addrs))
			for i, a := range addrs {
				ips[i] = a.IP
			}
			return ips, nil
		}
	}
	return &Webhook{cfg: cfg, target: u}, nil
}

// Name implements Channel.
func (w *Webhook) Name() string { return w.cfg.Name }

// Sign returns the signature header value for a timestamp and body.
func Sign(secret []byte, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// VerifySignature checks a received signature in constant time. Receivers
// should additionally reject stale timestamps.
func VerifySignature(secret []byte, timestamp string, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, timestamp, body)), []byte(signature))
}

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// Send implements Channel. It retries network errors, 429 and 5xx responses up
// to MaxRetries times with exponential backoff; other failures are permanent.
func (w *Webhook) Send(ctx context.Context, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	var last error
	delay := w.cfg.Backoff
	for attempt := 0; attempt <= w.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			w.cfg.Sleep(delay)
			delay *= 2
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		last = w.attempt(ctx, body)
		if last == nil {
			return nil
		}
		var perm permanentError
		if errors.Is(last, ErrBlockedAddress) || errors.As(last, &perm) {
			return last
		}
	}
	return fmt.Errorf("webhook failed after %d attempts: %w", w.cfg.MaxRetries+1, last)
}

func (w *Webhook) attempt(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.target.String(), bytes.NewReader(body))
	if err != nil {
		return permanentError{err}
	}
	ts := strconv.FormatInt(w.cfg.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "db-cluster-manager-notify")
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, Sign(w.cfg.Secret, ts, body))

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       w.dial,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	default:
		return permanentError{fmt.Errorf("webhook status %d", resp.StatusCode)}
	}
}

// dial resolves the host, validates every answer and then dials a validated IP
// directly, so a second DNS answer cannot redirect the connection (DNS
// rebinding).
func (w *Webhook) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		ips = []net.IP{ip}
	} else if ips, err = w.cfg.Resolver(ctx, host); err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for %q", host)
	}
	if !w.cfg.AllowPrivate {
		for _, ip := range ips {
			if !ipAllowed(ip) {
				return nil, fmt.Errorf("%w: %s", ErrBlockedAddress, ip)
			}
		}
	}
	var d net.Dialer
	var lastErr error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// ipAllowed reports whether ip is a public unicast address.
func ipAllowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip.IsMulticast(), ip.IsUnspecified():
		return false
	}
	return !cgnat.Contains(ip)
}
