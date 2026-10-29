// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

func TestEnrollmentTokenCreatesTwoRuntimeSlots(t *testing.T) {
	ca, err := certs.NewCA(certs.CAOptions{Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, plain, err := NewToken(inst, []string{"node1"}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg := &registry{tokens: map[string]*Token{tok.ID: &tok}, serials: map[string]string{}, revoked: map[string]bool{}, now: now}
	s := &Server{Authority: &authority{ca: ca, validity: time.Hour}, Registry: reg, Installation: inst}
	enrollOne := func(info agentrt.Info) *httptest.ResponseRecorder {
		_, csr, err := certs.NewKeyAndCSR("agent")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(EnrollRequest{Token: plain, HostID: "node1", CSR: string(csr), Runtime: info.Runtime, Capabilities: info.Capabilities})
		r := httptest.NewRequest(http.MethodPost, PathEnroll, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, info := range []agentrt.Info{agentrt.ContainerInfo(false), agentrt.NativeInfo(false)} {
		if w := enrollOne(info); w.Code != http.StatusOK {
			t.Fatalf("%s enrollment: %d %s", info.Runtime, w.Code, w.Body.String())
		}
	}
	if reg.serials["node1"] == "" || reg.serials["node1/native"] == "" || reg.serials["node1"] == reg.serials["node1/native"] {
		t.Fatalf("identity slots: %+v", reg.serials)
	}
	if w := enrollOne(agentrt.NativeInfo(false)); w.Code != http.StatusForbidden {
		t.Fatalf("native replay: %d %s", w.Code, w.Body.String())
	}
}

func TestRenewalCannotSwitchRuntime(t *testing.T) {
	ca, err := certs.NewCA(certs.CAOptions{Validity: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	old, err := ca.Issue(t.Context(), certs.Request{Kind: certs.KindPeer, CommonName: "native", URIs: []*url.URL{certs.RuntimeAgentURI(inst, "node1", "native")}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	reg := &registry{serials: map[string]string{"node1": "a1", "node1/native": old.Cert.SerialNumber.Text(16)}, revoked: map[string]bool{}}
	s := &Server{Authority: &authority{ca: ca, validity: time.Hour}, Registry: reg, Installation: inst}
	_, csr, err := certs.NewKeyAndCSR("renewal")
	if err != nil {
		t.Fatal(err)
	}
	request := func(info agentrt.Info) *httptest.ResponseRecorder {
		body, _ := json.Marshal(RenewRequest{CSR: string(csr), Runtime: info.Runtime, Capabilities: info.Capabilities})
		r := httptest.NewRequest(http.MethodPost, PathRenew, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{old.Cert, ca.Certificate()}}}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := request(agentrt.ContainerInfo(false)); w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("runtime-mismatch")) {
		t.Fatalf("runtime switch: %d %s", w.Code, w.Body.String())
	}
	if reg.serials["node1/native"] != old.Cert.SerialNumber.Text(16) {
		t.Fatal("mismatch rotated native slot")
	}
	w := request(agentrt.NativeInfo(false))
	if w.Code != http.StatusOK {
		t.Fatalf("native renewal: %d %s", w.Code, w.Body.String())
	}
	if reg.serials["node1"] != "a1" || reg.serials["node1/native"] == old.Cert.SerialNumber.Text(16) {
		t.Fatalf("renewal changed wrong slot: %+v", reg.serials)
	}
}

func TestRuntimeFromAgentURIRejectsAmbiguousAndUnknownSlots(t *testing.T) {
	for _, tc := range []struct {
		uris []*url.URL
		want agentrt.Runtime
		ok   bool
	}{
		{uris: []*url.URL{certs.AgentURI(inst, "node1")}, want: agentrt.Container, ok: true},
		{uris: []*url.URL{certs.RuntimeAgentURI(inst, "node1", "native")}, want: agentrt.Native, ok: true},
		{uris: []*url.URL{certs.AgentURI(inst, "node1"), certs.RuntimeAgentURI(inst, "node1", "native")}},
		{uris: []*url.URL{certs.RuntimeAgentURI(inst, "node1", "other")}},
	} {
		host, rt, ok := RuntimeFromAgentURI(&x509.Certificate{URIs: tc.uris}, inst)
		if ok != tc.ok || ok && (host != "node1" || rt != tc.want) {
			t.Fatalf("URIs %v: %s %s %v", tc.uris, host, rt, ok)
		}
	}
}
