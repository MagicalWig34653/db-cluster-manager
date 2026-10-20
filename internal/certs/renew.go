// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Policy decides when a certificate is renewed: when the remaining lifetime
// drops below max(Fraction * total lifetime, Floor).
type Policy struct {
	// Fraction of the total lifetime (0..1) that should remain at renewal.
	Fraction float64
	// Floor is the minimum remaining lifetime that always triggers renewal.
	Floor time.Duration
}

// NeedsRenewal is a pure function of the certificate, the time and the policy.
// Expired or not-yet-valid-with-no-lifetime certificates always need renewal.
func NeedsRenewal(cert *x509.Certificate, now time.Time, p Policy) bool {
	remaining := cert.NotAfter.Sub(now)
	if remaining <= 0 {
		return true
	}
	total := cert.NotAfter.Sub(cert.NotBefore)
	threshold := time.Duration(float64(total) * p.Fraction)
	if p.Floor > threshold {
		threshold = p.Floor
	}
	return remaining < threshold
}

// Issuer issues certificates for a Request. The internal CA implements it.
type Issuer interface {
	Issue(ctx context.Context, req Request) (*Issued, error)
}

var _ Issuer = (*CA)(nil)

// ACMEDNS01Issuer is the extension point for ACME DNS-01 issuance (N05, R2).
// It is deliberately NOT implemented: it needs a library decision (see
// docs; candidates are golang.org/x/crypto/acme and go-acme/lego) and a DNS
// provider interface. Implementations must return an Issued whose chain was
// obtained from the ACME server.
type ACMEDNS01Issuer interface {
	Issuer
	// SolveDNS01 publishes the TXT record for the challenge of domain and
	// returns a cleanup function.
	SolveDNS01(ctx context.Context, domain, token, keyAuth string) (cleanup func(), err error)
}

// Entry is a managed certificate.
type Entry struct {
	Name    string
	Request Request
	Current *Issued
	// Previous is the certificate replaced by the last successful renewal. It
	// stays available for rollback until the next renewal replaces it.
	Previous *Issued
}

// Store persists managed certificates.
type Store interface {
	List(ctx context.Context) ([]Entry, error)
	Get(ctx context.Context, name string) (Entry, bool, error)
	Put(ctx context.Context, e Entry) error
}

// MemoryStore is an in-memory Store.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]Entry
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]Entry{}} }

// List returns all entries sorted by name.
func (s *MemoryStore) List(context.Context) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one entry.
func (s *MemoryStore) Get(_ context.Context, name string) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[name]
	return e, ok, nil
}

// Put inserts or replaces an entry.
func (s *MemoryStore) Put(_ context.Context, e Entry) error {
	if e.Name == "" {
		return errors.New("certs: entry needs a name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[e.Name] = e
	return nil
}

// EventType classifies a renewal event.
type EventType string

const (
	// EventRenewed: a new certificate was issued, validated and stored.
	EventRenewed EventType = "renewed"
	// EventFailed: renewal failed; the old certificate was kept.
	EventFailed EventType = "failed"
	// EventExpiring: renewal is due but did not succeed and the current
	// certificate is still valid (or already expired, see Event.Expired).
	EventExpiring EventType = "expiring"
)

// Event is emitted by the Renewer.
type Event struct {
	Type      EventType
	Name      string
	NotAfter  time.Time // of the certificate that is current after the event
	Remaining time.Duration
	Expired   bool
	Err       error
}

// Renewer scans a Store and reissues certificates that need renewal.
type Renewer struct {
	Store  Store
	Issuer Issuer
	Policy Policy
	// Roots are used to validate newly issued certificates before they replace
	// the old ones. Required.
	Roots *x509.CertPool
	// Now is the injected clock; time.Now when nil.
	Now func() time.Time
	// OnEvent receives events; may be nil. It is called synchronously.
	OnEvent func(Event)
}

func (r *Renewer) emit(e Event) {
	if r.OnEvent != nil {
		r.OnEvent(e)
	}
}

// ScanOnce checks every entry once. Per-entry failures are reported through
// events and do not stop the scan; the returned error covers only store
// failures and context cancellation.
func (r *Renewer) ScanOnce(ctx context.Context) error {
	if r.Store == nil || r.Issuer == nil || r.Roots == nil {
		return errors.New("certs: renewer needs Store, Issuer and Roots")
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	entries, err := r.Store.List(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		t := now()
		if e.Current != nil && !NeedsRenewal(e.Current.Cert, t, r.Policy) {
			continue
		}
		if err := r.renew(ctx, e, t); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func (r *Renewer) renew(ctx context.Context, e Entry, now time.Time) error {
	fail := func(err error) error {
		ev := Event{Type: EventFailed, Name: e.Name, Err: err}
		if e.Current != nil {
			ev.NotAfter = e.Current.Cert.NotAfter
			ev.Remaining = ev.NotAfter.Sub(now)
			ev.Expired = ev.Remaining <= 0
		}
		r.emit(ev)
		ev.Type = EventExpiring
		r.emit(ev)
		return err
	}
	iss, err := r.Issuer.Issue(ctx, e.Request)
	if err != nil {
		return fail(fmt.Errorf("issue: %w", err))
	}
	if err := validateIssued(iss, e.Request, r.Roots, now, e.Current); err != nil {
		return fail(fmt.Errorf("validate new certificate: %w", err))
	}
	ne := e
	ne.Previous, ne.Current = e.Current, iss
	if err := r.Store.Put(ctx, ne); err != nil {
		return fail(fmt.Errorf("store: %w", err))
	}
	r.emit(Event{
		Type: EventRenewed, Name: e.Name,
		NotAfter: iss.Cert.NotAfter, Remaining: iss.Cert.NotAfter.Sub(now),
	})
	return nil
}

func validateIssued(n *Issued, req Request, roots *x509.CertPool, now time.Time, old *Issued) error {
	if n == nil || n.Cert == nil {
		return errors.New("issuer returned no certificate")
	}
	usage := x509.ExtKeyUsageServerAuth
	if req.Kind == KindClient {
		usage = x509.ExtKeyUsageClientAuth
	}
	if err := VerifyLeaf(n.Cert, nil, roots, usage, now, nil); err != nil {
		return err
	}
	for _, name := range req.Names() {
		if err := n.Cert.VerifyHostname(name); err != nil {
			return err
		}
	}
	if _, err := n.TLSCertificate(); err != nil { // key matches certificate
		return err
	}
	if old != nil && !n.Cert.NotAfter.After(old.Cert.NotAfter) && now.Before(old.Cert.NotAfter) {
		return errors.New("new certificate does not extend the lifetime of the current one")
	}
	return nil
}
