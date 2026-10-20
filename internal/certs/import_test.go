// SPDX-License-Identifier: AGPL-3.0-only

package certs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"
)

func issuedFor(t *testing.T, ca *CA, validity time.Duration, names ...string) (chain, key []byte) {
	t.Helper()
	iss, err := ca.Issue(context.Background(), Request{Kind: KindServer, DNSNames: names, Validity: validity})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := iss.KeyPEM()
	return iss.ChainPEM, k
}

func problems(t *testing.T, err error) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %v", err)
	}
	return ve
}

func TestImportValid(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	chain, key := issuedFor(t, ca, time.Hour*24, "db.test")
	imp, err := ValidateImported(chain, key, ImportOptions{Now: t0, Hostnames: []string{"db.test"}, Roots: ca.Pool(), RequireServerAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(imp.Chain) != 2 || !bytes.Equal(imp.ChainPEM(), chain) {
		t.Fatal("chain mismatch")
	}
	blob, err := imp.SealKey("pw")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("PRIVATE KEY")) {
		t.Fatal("plain key")
	}
	// Without roots only structural checks run.
	if _, err := ValidateImported(chain, key, ImportOptions{Now: t0}); err != nil {
		t.Fatal(err)
	}
}

func TestImportProblems(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	chain, key := issuedFor(t, ca, 24*time.Hour, "db.test")
	_, otherKey := issuedFor(t, ca, 24*time.Hour, "db.test")
	otherCA := newTestCA(t, clk, nil)

	// Reverse chain order.
	blocks := splitPEM(chain)
	reversed := append(append([]byte{}, blocks[1]...), blocks[0]...)

	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalECPrivateKey(ecKey)
	encKey := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("x")})

	cases := []struct {
		name       string
		chain, key []byte
		opts       ImportOptions
		want       ProblemCode
	}{
		{"key mismatch", chain, otherKey, ImportOptions{Now: t0}, ProblemKeyMismatch},
		{"expired", chain, key, ImportOptions{Now: t0.Add(48 * time.Hour)}, ProblemExpired},
		{"not yet valid", chain, key, ImportOptions{Now: t0.Add(-time.Hour)}, ProblemNotYetValid},
		{"hostname", chain, key, ImportOptions{Now: t0, Hostnames: []string{"nope.test"}}, ProblemHostnameMismatch},
		{"order", reversed, key, ImportOptions{Now: t0}, ProblemChainOrder},
		{"untrusted", chain, key, ImportOptions{Now: t0, Roots: otherCA.Pool()}, ProblemUntrusted},
		{"no cert", []byte("hello"), key, ImportOptions{Now: t0}, ProblemNoCertificate},
		{"bad cert", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}), key, ImportOptions{Now: t0}, ProblemCertParse},
		{"no key", chain, nil, ImportOptions{Now: t0}, ProblemKeyParse},
		{"encrypted key", chain, encKey, ImportOptions{Now: t0}, ProblemKeyEncrypted},
		{"bad key", chain, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("x")}), ImportOptions{Now: t0}, ProblemKeyParse},
		{"wrong ec key", chain, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER}), ImportOptions{Now: t0}, ProblemKeyMismatch},
	}
	for _, tc := range cases {
		imp, err := ValidateImported(tc.chain, tc.key, tc.opts)
		if err == nil || imp != nil {
			t.Errorf("%s: expected error", tc.name)
			continue
		}
		if ve := problems(t, err); !ve.Has(tc.want) {
			t.Errorf("%s: want %s, got %v", tc.name, tc.want, ve)
		}
	}
}

func TestImportClientCertRejectedForServerAuth(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	iss, _ := ca.Issue(context.Background(), Request{Kind: KindClient, CommonName: "c", Validity: time.Hour})
	k, _ := iss.KeyPEM()
	_, err := ValidateImported(iss.ChainPEM, k, ImportOptions{Now: t0, RequireServerAuth: true})
	if !problems(t, err).Has(ProblemKeyUsage) {
		t.Fatal("expected key usage problem")
	}
}

func TestImportReportsMultipleProblems(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	chain, _ := issuedFor(t, ca, time.Hour, "a.test")
	_, key2 := issuedFor(t, ca, time.Hour, "a.test")
	_, err := ValidateImported(chain, key2, ImportOptions{Now: t0.Add(24 * time.Hour), Hostnames: []string{"b.test"}})
	ve := problems(t, err)
	if !ve.Has(ProblemKeyMismatch) || !ve.Has(ProblemExpired) || !ve.Has(ProblemHostnameMismatch) {
		t.Fatalf("got %v", ve)
	}
}

func splitPEM(b []byte) [][]byte {
	var out [][]byte
	for {
		blk, rest := pem.Decode(b)
		if blk == nil {
			return out
		}
		out = append(out, pem.EncodeToMemory(blk))
		b = rest
	}
}

func TestCABundle(t *testing.T) {
	clk := &clock{t0}
	a, b := newTestCA(t, clk, nil), newTestCA(t, clk, nil)
	bundle, err := CABundle(a.Certificate(), b.Certificate(), a.Certificate())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(splitPEM(bundle)); n != 2 {
		t.Fatalf("want 2 certs, got %d", n)
	}
	again, _ := CABundle(b.Certificate(), a.Certificate())
	if !bytes.Equal(bundle, again) {
		t.Fatal("bundle not deterministic")
	}
	pool, err := PoolFromBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	chain, _ := issuedFor(t, b, time.Hour, "x.test")
	leaf, _ := x509.ParseCertificate(splitBlock(chain))
	if err := VerifyLeaf(leaf, nil, pool, x509.ExtKeyUsageServerAuth, t0, nil); err != nil {
		t.Fatal(err)
	}
	leafCA := leaf
	if _, err := CABundle(leafCA); err == nil {
		t.Fatal("non-CA accepted")
	}
	if _, err := CABundle(nil); err == nil {
		t.Fatal("nil accepted")
	}
	if _, err := PoolFromBundle([]byte("x")); err == nil {
		t.Fatal("empty bundle accepted")
	}
}

func splitBlock(chain []byte) []byte {
	b, _ := pem.Decode(chain)
	return b.Bytes
}

func TestDenyList(t *testing.T) {
	clk := &clock{t0}
	ca := newTestCA(t, clk, nil)
	iss, _ := ca.Issue(context.Background(), Request{Kind: KindServer, DNSNames: []string{"a.test"}, Validity: time.Hour})
	d := NewDenyList()
	if err := VerifyLeaf(iss.Cert, nil, ca.Pool(), x509.ExtKeyUsageServerAuth, t0, d); err != nil {
		t.Fatal(err)
	}
	d.Revoke(iss.Cert.SerialNumber, t0, "compromised")
	d.Revoke(iss.Cert.SerialNumber, t0.Add(time.Hour), "again")
	if got := d.Entries(); len(got) != 1 || got[0].Reason != "compromised" {
		t.Fatalf("entries %+v", got)
	}
	err := VerifyLeaf(iss.Cert, nil, ca.Pool(), x509.ExtKeyUsageServerAuth, t0, d)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("got %v", err)
	}
	if d.IsRevoked(big.NewInt(42)) {
		t.Fatal("unrelated serial revoked")
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	d2 := NewDenyList()
	if err := json.Unmarshal(raw, d2); err != nil {
		t.Fatal(err)
	}
	if !d2.IsRevoked(iss.Cert.SerialNumber) {
		t.Fatal("round trip lost entry")
	}
	if err := json.Unmarshal([]byte(`[{"serial":"zz"}]`), d2); err == nil {
		t.Fatal("bad serial accepted")
	}
	// Revoking the CA itself denies every chain through it.
	d.Revoke(ca.Certificate().SerialNumber, t0, "")
	d3 := NewDenyList()
	d3.Revoke(ca.Certificate().SerialNumber, t0, "")
	if err := VerifyLeaf(iss.Cert, nil, ca.Pool(), x509.ExtKeyUsageServerAuth, t0, d3); !errors.Is(err, ErrRevoked) {
		t.Fatalf("CA revocation not honoured: %v", err)
	}
}
