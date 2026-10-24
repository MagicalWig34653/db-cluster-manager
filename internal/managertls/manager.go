// SPDX-License-Identifier: AGPL-3.0-only

package managertls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

const requestTimeout = 30 * time.Second
const renewBefore = 30 * 24 * time.Hour

var ErrMissing = errors.New("ACME: stored value missing")

// Storage must seal values before replication. Reads must also work on followers;
// writes must be leader-only. A missing value returns ErrMissing.
type Storage interface {
	Get(context.Context, string) ([]byte, error)
	Put(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

type bundle struct{ Certificate, Key []byte }
type challengeRecord struct {
	Domain, Token, Response string
	Bundle                  bundle
	Expires                 time.Time
}

// Manager never contacts a CA on a handshake. Only Run/Ensure issue certificates.
// All replicas read the shared certificate and active challenge records.
type Manager struct {
	config  Config
	store   Storage
	factory Factory
	prefix  string
	mu      sync.Mutex
}

func New(c Config, s Storage, factory Factory) (*Manager, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("ACME storage is required")
	}
	c = c.Defaults()
	if factory == nil {
		factory = NewClient
	}
	// Keep CA accounts independent; certificate/challenge identity also includes
	// the exact domain list so a configuration change never serves an old name.
	h := sha256.Sum256([]byte(c.DirectoryURL + "\x00" + c.Email + "\x00" + strings.Join(c.Domains, "\x00") + "\x00" + c.Challenge))
	return &Manager{config: c, store: s, factory: factory, prefix: fmt.Sprintf("%x/", h)}, nil
}

func (m *Manager) allowed(domain string) bool {
	for _, d := range m.config.Domains {
		if domain == d {
			return true
		}
	}
	return false
}
func (m *Manager) key(s string) string { return m.prefix + s }
func encodeCertificate(c tls.Certificate) (bundle, error) {
	b := bundle{}
	for _, der := range c.Certificate {
		b.Certificate = append(b.Certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	der, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		return b, err
	}
	b.Key = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return b, nil
}
func (m *Manager) put(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.store.Put(ctx, m.key(key), b)
}
func (m *Manager) get(ctx context.Context, key string, v any) error {
	b, err := m.store.Get(ctx, m.key(key))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func (m *Manager) certificate(ctx context.Context) (*tls.Certificate, error) {
	var b bundle
	if err := m.get(ctx, "certificate", &b); err != nil {
		return nil, err
	}
	c, err := tls.X509KeyPair(b.Certificate, b.Key)
	if err != nil {
		return nil, err
	}
	if c.Leaf == nil {
		c.Leaf, err = x509.ParseCertificate(c.Certificate[0])
	}
	return &c, err
}

// TLSConfig serves both ordinary TLS and the isolated acme-tls/1 protocol.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1", acme.ALPNProto}, GetCertificate: m.GetCertificate}
}
func (m *Manager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	domain := strings.ToLower(hello.ServerName)
	if !m.allowed(domain) {
		return nil, errors.New("ACME: SNI is not a configured domain")
	}
	ctx := hello.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	for _, p := range hello.SupportedProtos {
		if p != acme.ALPNProto {
			continue
		}
		// RFC 8737 clients MUST offer only acme-tls/1. Never leak a challenge
		// certificate into a normal application handshake.
		if len(hello.SupportedProtos) != 1 {
			return nil, errors.New("ACME: invalid ALPN challenge offer")
		}
		var r challengeRecord
		if err := m.get(ctx, "challenge/"+domain, &r); err != nil {
			return nil, err
		}
		if m.config.Challenge != "tls-alpn-01" || !time.Now().Before(r.Expires) || r.Domain != domain {
			return nil, errors.New("ACME: challenge inactive")
		}
		c, err := tls.X509KeyPair(r.Bundle.Certificate, r.Bundle.Key)
		return &c, err
	}
	c, err := m.certificate(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.Leaf.VerifyHostname(domain); err != nil {
		return nil, err
	}
	if time.Now().Before(c.Leaf.NotBefore) || !time.Now().Before(c.Leaf.NotAfter) {
		return nil, errors.New("ACME: certificate outside validity period")
	}
	return c, nil
}

// HTTPHandler exposes challenge tokens only: it never serves API/UI over HTTP.
func (m *Manager) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if r.Method != http.MethodGet || !m.allowed(host) || m.config.Challenge != "http-01" {
			http.NotFound(w, r)
			return
		}
		var v challengeRecord
		if err := m.get(r.Context(), "challenge/"+host, &v); err != nil || !time.Now().Before(v.Expires) || v.Domain != host || r.URL.Path != "/.well-known/acme-challenge/"+v.Token {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(v.Response))
	})
}

// Ensure is called under one leadership's cancellable context. Failed renewal
// preserves the previous certificate. The caller must fence this context by term.
func (m *Manager) Ensure(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := m.certificate(ctx)
	if err == nil && time.Until(c.Leaf.NotAfter) > renewBefore {
		valid := !time.Now().Before(c.Leaf.NotBefore)
		for _, d := range m.config.Domains {
			valid = valid && c.Leaf.VerifyHostname(d) == nil
		}
		if valid {
			return nil
		}
	}
	if err != nil && !errors.Is(err, ErrMissing) {
		return err
	}
	var account []byte
	if err := m.get(ctx, "account", &account); err != nil {
		if !errors.Is(err, ErrMissing) {
			return err
		}
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		account, err = x509.MarshalECPrivateKey(k)
		if err != nil {
			return err
		}
		// Commit before the first CA request: a successor reuses the same account.
		if err := m.put(ctx, "account", account); err != nil {
			return err
		}
	}
	signer, err := x509.ParseECPrivateKey(account)
	if err != nil {
		return err
	}
	client := m.factory(m.config.DirectoryURL, signer)
	if err := client.Register(ctx, m.config.Email); err != nil {
		return err
	}
	order, err := client.Order(ctx, m.config.Domains)
	if err != nil {
		return err
	}
	for _, uri := range order.AuthzURLs {
		auth, err := client.Authorization(ctx, uri)
		if err != nil {
			return err
		}
		if auth.Identifier.Type != "dns" || !m.allowed(auth.Identifier.Value) || auth.Wildcard {
			return errors.New("ACME: unexpected authorization identifier")
		}
		if auth.Status == acme.StatusValid {
			continue
		}
		var selected *acme.Challenge
		for _, ch := range auth.Challenges {
			if ch.Type == m.config.Challenge {
				selected = ch
				break
			}
		}
		if selected == nil {
			return errors.New("ACME: configured challenge not offered by CA")
		}
		if err := m.authorize(ctx, client, uri, auth.Identifier.Value, selected); err != nil {
			return err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{}, DNSNames: m.config.Domains}, key)
	if err != nil {
		return err
	}
	chain, err := client.Finalize(ctx, order.FinalizeURL, csr)
	if err != nil {
		return err
	}
	b, err := encodeCertificate(tls.Certificate{Certificate: chain, PrivateKey: key})
	if err != nil {
		return err
	}
	issued, err := tls.X509KeyPair(b.Certificate, b.Key)
	if err != nil {
		return err
	}
	if issued.Leaf == nil {
		issued.Leaf, err = x509.ParseCertificate(issued.Certificate[0])
		if err != nil {
			return err
		}
	}
	for _, domain := range m.config.Domains {
		if err := issued.Leaf.VerifyHostname(domain); err != nil {
			return err
		}
	}
	if time.Now().Before(issued.Leaf.NotBefore) || !time.Now().Before(issued.Leaf.NotAfter) {
		return errors.New("ACME: CA returned certificate outside validity period")
	}
	return m.put(ctx, "certificate", b)
}
func (m *Manager) authorize(ctx context.Context, c Client, uri, domain string, ch *acme.Challenge) error {
	r := challengeRecord{Domain: domain, Token: ch.Token, Expires: time.Now().Add(15 * time.Minute)}
	var err error
	if ch.Type == "http-01" {
		r.Response, err = c.HTTPResponse(ch.Token)
	} else {
		var cert tls.Certificate
		cert, err = c.ALPNCertificate(ch.Token, domain)
		if err == nil {
			r.Bundle, err = encodeCertificate(cert)
		}
	}
	if err != nil {
		return err
	}
	if err := m.put(ctx, "challenge/"+domain, r); err != nil {
		return err
	}
	// A lost leader cannot clean up a successor's record: storage fences writes.
	defer func() { _ = m.store.Delete(ctx, m.key("challenge/"+domain)) }()
	if err := c.Accept(ctx, ch); err != nil {
		return err
	}
	return c.WaitAuthorization(ctx, uri)
}

// Run renews immediately on election, then hourly. Errors back off for five
// minutes, preserving valid certificates and preventing handshake retry storms.
func (m *Manager) Run(ctx context.Context, report func(error)) {
	for {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := m.Ensure(attempt)
		cancel()
		delay := time.Hour
		if err != nil {
			delay = 5 * time.Minute
			if ctx.Err() == nil && report != nil {
				report(err)
			}
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
