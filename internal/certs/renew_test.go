// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"
	"time"
)

func fakeCert(notBefore, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{NotBefore: notBefore, NotAfter: notAfter}
}

func TestNeedsRenewal(t *testing.T) {
	day := 24 * time.Hour
	c := fakeCert(t0, t0.Add(90*day))
	p := Policy{Fraction: 1.0 / 3, Floor: 7 * day}
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"fresh", t0, false},
		{"just above fraction", t0.Add(60*day - time.Minute), false},
		{"below fraction", t0.Add(60*day + time.Minute), true},
		{"expired", t0.Add(91 * day), true},
		{"exactly expiry", t0.Add(90 * day), true},
	}
	for _, tc := range cases {
		if got := NeedsRenewal(c, tc.now, p); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
	// Floor dominates for short-lived certificates.
	short := fakeCert(t0, t0.Add(10*day))
	if !NeedsRenewal(short, t0.Add(4*day), p) { // 6d left < 7d floor
		t.Error("floor should trigger")
	}
	if NeedsRenewal(short, t0.Add(2*day), Policy{Fraction: 0.1, Floor: 3 * day}) {
		t.Error("8d left, threshold 3d")
	}
}

type recorder struct{ evs []Event }

func (r *recorder) on(e Event) { r.evs = append(r.evs, e) }

func setup(t *testing.T, validity time.Duration) (*clock, *CA, *MemoryStore, *Renewer, *recorder, Request) {
	t.Helper()
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	st := NewMemoryStore()
	rec := &recorder{}
	req := Request{Kind: KindServer, DNSNames: []string{"db.test"}, Validity: validity}
	iss, err := ca.Issue(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Put(context.Background(), Entry{Name: "endpoint", Request: req, Current: iss})
	r := &Renewer{Store: st, Issuer: ca, Policy: Policy{Fraction: 1.0 / 3, Floor: time.Hour},
		Roots: ca.Pool(), Now: clk.Now, OnEvent: rec.on}
	return clk, ca, st, r, rec, req
}

func TestRenewerRenewsAndKeepsPrevious(t *testing.T) {
	clk, _, st, r, rec, _ := setup(t, 90*24*time.Hour)
	ctx := context.Background()
	before, _, _ := st.Get(ctx, "endpoint")

	if err := r.ScanOnce(ctx); err != nil || len(rec.evs) != 0 {
		t.Fatalf("fresh cert touched: %v %v", err, rec.evs)
	}
	clk.t = t0.Add(70 * 24 * time.Hour)
	if err := r.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Type != EventRenewed {
		t.Fatalf("events %+v", rec.evs)
	}
	after, _, _ := st.Get(ctx, "endpoint")
	if after.Current.Cert.SerialNumber.Cmp(before.Current.Cert.SerialNumber) == 0 {
		t.Fatal("not reissued")
	}
	if after.Previous == nil || after.Previous.Cert.SerialNumber.Cmp(before.Current.Cert.SerialNumber) != 0 {
		t.Fatal("previous not kept")
	}
	if !after.Current.Cert.NotAfter.After(before.Current.Cert.NotAfter) {
		t.Fatal("lifetime not extended")
	}
}

type failIssuer struct{ err error }

func (f failIssuer) Issue(context.Context, Request) (*Issued, error) { return nil, f.err }

func TestRenewerFailureKeepsOldAndEmits(t *testing.T) {
	clk, _, st, r, rec, _ := setup(t, 90*24*time.Hour)
	ctx := context.Background()
	before, _, _ := st.Get(ctx, "endpoint")
	boom := errors.New("boom")
	r.Issuer = failIssuer{boom}
	clk.t = t0.Add(80 * 24 * time.Hour)
	if err := r.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rec.evs) != 2 || rec.evs[0].Type != EventFailed || rec.evs[1].Type != EventExpiring {
		t.Fatalf("events %+v", rec.evs)
	}
	if !errors.Is(rec.evs[0].Err, boom) || rec.evs[1].Expired || rec.evs[1].Remaining <= 0 {
		t.Fatalf("event details %+v", rec.evs[1])
	}
	after, _, _ := st.Get(ctx, "endpoint")
	if after.Current != before.Current || after.Previous != nil {
		t.Fatal("old certificate replaced despite failure")
	}
	// Expired flag after expiry.
	rec.evs = nil
	clk.t = t0.Add(95 * 24 * time.Hour)
	_ = r.ScanOnce(ctx)
	if !rec.evs[1].Expired {
		t.Fatal("expected Expired flag")
	}
}

// badIssuer returns a certificate from a different CA, which must be rejected.
type badIssuer struct{ other *CA }

func (b badIssuer) Issue(ctx context.Context, r Request) (*Issued, error) {
	return b.other.Issue(ctx, r)
}

func TestRenewerRejectsInvalidNewCertificate(t *testing.T) {
	clk, _, st, r, rec, _ := setup(t, 90*24*time.Hour)
	ctx := context.Background()
	before, _, _ := st.Get(ctx, "endpoint")
	r.Issuer = badIssuer{newTestCA(t, clk, nil)}
	clk.t = t0.Add(80 * 24 * time.Hour)
	_ = r.ScanOnce(ctx)
	if len(rec.evs) == 0 || rec.evs[0].Type != EventFailed {
		t.Fatalf("events %+v", rec.evs)
	}
	after, _, _ := st.Get(ctx, "endpoint")
	if after.Current != before.Current {
		t.Fatal("untrusted certificate installed")
	}
}

func TestRenewerExpiredCertGetsReplaced(t *testing.T) {
	clk, _, st, r, rec, _ := setup(t, 24*time.Hour)
	clk.t = t0.Add(48 * time.Hour)
	if err := r.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.evs) != 1 || rec.evs[0].Type != EventRenewed {
		t.Fatalf("events %+v", rec.evs)
	}
	e, _, _ := st.Get(context.Background(), "endpoint")
	if e.Current.Cert.NotAfter.Before(clk.t) {
		t.Fatal("still expired")
	}
}

func TestRenewerConfigAndCancel(t *testing.T) {
	if err := (&Renewer{}).ScanOnce(context.Background()); err == nil {
		t.Fatal("expected config error")
	}
	_, _, _, r, _, _ := setup(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.ScanOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if err := NewMemoryStore().Put(context.Background(), Entry{}); err == nil {
		t.Fatal("nameless entry accepted")
	}
}
