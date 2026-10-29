// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/certs"
)

// HTTP paths of the enrollment endpoint.
const (
	PathEnroll = "/v1/agent/enroll"
	PathRenew  = "/v1/agent/renew"
)

const maxBody = 64 << 10

// EnrollRequest is the body of PathEnroll.
type EnrollRequest struct {
	Token  string `json:"token"`
	HostID string `json:"host_id"`
	CSR    string `json:"csr"` // PEM
	// Runtime and Capabilities are the agent's runtime report (ADR 0024).
	// Omitted by agents built before it.
	Runtime      agentrt.Runtime      `json:"runtime,omitempty"`
	Capabilities []agentrt.Capability `json:"capabilities,omitempty"`
}

// RenewRequest is the body of PathRenew (sent with the current certificate).
type RenewRequest struct {
	CSR          string               `json:"csr"`
	Runtime      agentrt.Runtime      `json:"runtime,omitempty"`
	Capabilities []agentrt.Capability `json:"capabilities,omitempty"`
}

// ReportRegistry is an optional Registry extension that stores the validated
// runtime report of an agent.
type ReportRegistry interface {
	RecordReport(ctx context.Context, hostID string, info agentrt.Info) error
}

// reportOf validates a request's report. present is false when none was sent.
func reportOf(rt agentrt.Runtime, caps []agentrt.Capability) (info agentrt.Info, present bool, err error) {
	if rt == "" && len(caps) == 0 {
		return agentrt.Info{}, false, nil
	}
	info = agentrt.Info{Runtime: rt, Capabilities: caps}
	return info, true, info.Validate()
}

func (s *Server) recordReport(ctx context.Context, host string, info agentrt.Info, present bool) {
	if rr, ok := s.Registry.(ReportRegistry); ok && present {
		if err := rr.RecordReport(ctx, host, info); err != nil {
			s.log().Warn("enroll: runtime report not recorded", "host", host, "err", err)
		}
	}
}

// Response is the answer to both requests.
type Response struct {
	// Certificate is the agent certificate followed by the CA, PEM.
	Certificate string `json:"certificate"`
	// CA is the CA bundle that signs manager and agent certificates, PEM.
	CA string `json:"ca"`
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// Errors of Registry implementations that the server maps to responses.
var (
	// ErrUnavailable: the registry cannot write now (for example this
	// manager replica is not the leader). Answered with 503.
	ErrUnavailable = errors.New("enroll: registry unavailable")
	// ErrSourceMismatch: the request does not come from the host's address.
	ErrSourceMismatch = errors.New("enroll: request source does not match the host")
	// ErrUnknownAgent: renewal for a host or serial the registry does not
	// hold as current.
	ErrUnknownAgent = errors.New("enroll: agent identity not current")
)

// Authority signs agent certificates. Every identity in the certificate
// comes from the manager's records for hostID, never from the CSR.
type Authority interface {
	SignAgent(ctx context.Context, hostID string, runtime agentrt.Runtime, csr *x509.CertificateRequest) (*certs.Signed, error)
	// CABundlePEM is sent to the agent; it must contain the pinned CA.
	CABundlePEM() []byte
	// ClientCAs verifies agent certificates presented for renewal.
	ClientCAs() *x509.CertPool
}

// Registry holds tokens and agent identities.
type Registry interface {
	// ConsumeToken validates the plain token for hostID and marks it used,
	// atomically. source is the request's peer IP.
	ConsumeToken(ctx context.Context, plain, hostID string, runtime agentrt.Runtime, source net.IP) error
	// CurrentSerial returns the serial (hex) recorded for hostID; ok is
	// false when the host is not enrolled.
	CurrentSerial(ctx context.Context, hostID string, runtime agentrt.Runtime) (serial string, ok bool, err error)
	// RecordIssued stores cert as hostID's current agent identity.
	RecordIssued(ctx context.Context, hostID string, runtime agentrt.Runtime, cert *x509.Certificate) error
	// Revoked reports a denied serial.
	Revoked(serial *big.Int) bool
}

// Server is the manager-side enrollment endpoint.
type Server struct {
	Authority    Authority
	Registry     Registry
	Installation string
	Log          *slog.Logger
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TLSConfig returns the listener configuration: TLS 1.3, the manager's
// enrollment certificate, and optional client certificates (required by
// the renewal path) verified against the CA.
func (s *Server) TLSConfig(cert func() (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert() },
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      s.Authority.ClientCAs(),
	}
}

// Handler serves PathEnroll and PathRenew.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathEnroll, s.enroll)
	mux.HandleFunc(PathRenew, s.renew)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: code, Message: msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fail(w, http.StatusMethodNotAllowed, "method-not-allowed", "use POST")
		return false
	}
	if r.TLS == nil {
		fail(w, http.StatusForbidden, "tls-required", "enrollment requires TLS")
		return false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "unsupported-media-type", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		fail(w, http.StatusBadRequest, "bad-request", "malformed request body")
		return false
	}
	return true
}

func peerIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var req EnrollRequest
	if !decode(w, r, &req) {
		return
	}
	if req.HostID == "" || len(req.HostID) > 128 || strings.ContainsAny(req.HostID, "/ \t\r\n") {
		fail(w, http.StatusBadRequest, "bad-request", "invalid host_id")
		return
	}
	csr, err := certs.ParseCSR([]byte(req.CSR))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad-csr", err.Error())
		return
	}
	info, present, err := reportOf(req.Runtime, req.Capabilities)
	if err != nil {
		// A mismatched report is refused before the token is consumed.
		s.log().Warn("enroll: runtime report refused", "host", req.HostID, "err", err)
		fail(w, http.StatusBadRequest, "runtime-mismatch", err.Error())
		return
	}
	// Parse before consuming: a malformed request does not burn the token.
	runtime := info.Runtime
	if runtime == "" {
		runtime = agentrt.Container
	}
	if err := s.Registry.ConsumeToken(r.Context(), req.Token, req.HostID, runtime, peerIP(r)); err != nil {
		s.log().Warn("enroll: enrollment refused", "host", req.HostID, "peer", r.RemoteAddr, "err", err)
		switch {
		case errors.Is(err, ErrUnavailable):
			fail(w, http.StatusServiceUnavailable, "unavailable", "try another manager replica")
		case errors.Is(err, ErrTokenInvalid):
			fail(w, http.StatusUnauthorized, "token-invalid", "enrollment token invalid")
		default:
			// Replay, host not allowed, source mismatch: same answer.
			fail(w, http.StatusForbidden, "forbidden", "enrollment not allowed for this host")
		}
		return
	}
	s.issue(w, r, req.HostID, csr, "enrolled", info, present)
}

func (s *Server) issue(w http.ResponseWriter, r *http.Request, host string, csr *x509.CertificateRequest, what string, info agentrt.Info, present bool) {
	runtime := info.Runtime
	if runtime == "" {
		runtime = agentrt.Container
	}
	signed, err := s.Authority.SignAgent(r.Context(), host, runtime, csr)
	if err != nil {
		s.log().Error("enroll: signing failed", "host", host, "err", err)
		fail(w, http.StatusInternalServerError, "sign-failed", "certificate could not be issued")
		return
	}
	if err := s.Registry.RecordIssued(r.Context(), host, runtime, signed.Cert); err != nil {
		s.log().Error("enroll: identity not recorded; certificate discarded", "host", host, "err", err)
		status := http.StatusInternalServerError
		if errors.Is(err, ErrUnavailable) {
			status = http.StatusServiceUnavailable
		}
		fail(w, status, "record-failed", "agent identity could not be recorded")
		return
	}
	s.recordReport(r.Context(), host, info, present)
	s.log().Info("enroll: agent "+what, "host", host, "serial", signed.Cert.SerialNumber.Text(16),
		"not_after", signed.Cert.NotAfter.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, Response{Certificate: string(signed.ChainPEM), CA: string(s.Authority.CABundlePEM())})
}

// HostFromAgentURI extracts the host ID from an agent identity of the
// installation; ok is false for anything else.
func HostFromAgentURI(cert *x509.Certificate, installation string) (string, bool) {
	host, _, ok := RuntimeFromAgentURI(cert, installation)
	return host, ok
}

// RuntimeFromAgentURI returns the host and runtime of exactly one agent URI.
// The historical URI without a runtime suffix is the container slot.
func RuntimeFromAgentURI(cert *x509.Certificate, installation string) (string, agentrt.Runtime, bool) {
	prefix := certs.AgentURI(installation, "").String()
	host := ""
	runtime := agentrt.Container
	for _, u := range cert.URIs {
		if s := u.String(); strings.HasPrefix(s, prefix) && len(s) > len(prefix) {
			if host != "" {
				return "", "", false // ambiguous
			}
			parts := strings.Split(s[len(prefix):], "/")
			if len(parts) == 1 {
				host = parts[0]
			} else if len(parts) == 2 && parts[1] == string(agentrt.Native) {
				host, runtime = parts[0], agentrt.Native
			} else {
				return "", "", false
			}
		}
	}
	return host, runtime, host != ""
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	var req RenewRequest
	if !decode(w, r, &req) {
		return
	}
	if len(r.TLS.VerifiedChains) == 0 {
		fail(w, http.StatusUnauthorized, "certificate-required", "renewal requires the current agent certificate")
		return
	}
	chain := r.TLS.VerifiedChains[0]
	leaf := chain[0]
	for _, c := range chain {
		if s.Registry.Revoked(c.SerialNumber) {
			s.log().Warn("enroll: renewal with revoked certificate", "serial", c.SerialNumber.Text(16), "peer", r.RemoteAddr)
			fail(w, http.StatusForbidden, "revoked", "certificate revoked")
			return
		}
	}
	host, runtime, ok := RuntimeFromAgentURI(leaf, s.Installation)
	if !ok {
		fail(w, http.StatusForbidden, "forbidden", "not an agent certificate of this installation")
		return
	}
	cur, ok, err := s.Registry.CurrentSerial(r.Context(), host, runtime)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "unavailable", "registry unavailable")
		return
	}
	if !ok || cur != leaf.SerialNumber.Text(16) {
		s.log().Warn("enroll: renewal with a certificate that is not current", "host", host, "serial", leaf.SerialNumber.Text(16))
		fail(w, http.StatusForbidden, "forbidden", fmt.Sprintf("certificate is not the current identity of %s", host))
		return
	}
	csr, err := certs.ParseCSR([]byte(req.CSR))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad-csr", err.Error())
		return
	}
	info, present, err := reportOf(req.Runtime, req.Capabilities)
	if err != nil {
		fail(w, http.StatusBadRequest, "runtime-mismatch", err.Error())
		return
	}
	if present && info.Runtime != runtime {
		fail(w, http.StatusBadRequest, "runtime-mismatch", "renewal runtime differs from certificate identity")
		return
	}
	if !present {
		info.Runtime = runtime
	}
	s.issue(w, r, host, csr, "renewed", info, present)
}
