// SPDX-License-Identifier: AGPL-3.0-only

package agent

// UNAUTHENTICATED LAB TRANSPORT. This file adapts Handler to HTTP/JSON for
// lab use only. It has no authentication, no authorization and no
// encryption. Any local process on the host can send commands. The only
// protections are: listening on loopback IP literals only, rejecting
// non-loopback peers, requiring a loopback Host header (against DNS
// rebinding) and requiring Content-Type application/json (so a browser
// cannot send a simple cross-origin request). Production transport with
// mTLS and agent enrollment is future work.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

// LabCommandPath is the HTTP path of the lab command endpoint.
const LabCommandPath = "/v1/commands"

const maxLabRequestBody = 1 << 20

// Error codes of the lab transport.
const (
	httpCodeInvalid   = "invalid-command"
	httpCodeStale     = "stale-term"
	httpCodeConflict  = "step-conflict"
	httpCodePersist   = "persist-failed"
	httpCodeCancelled = "cancelled"
	httpCodeInternal  = "internal"
	httpCodeForbidden = "forbidden"
	httpCodeMethod    = "method-not-allowed"
	httpCodeMedia     = "unsupported-media-type"
)

type labError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type labResponse struct {
	Result *Result   `json:"result,omitempty"`
	Error  *labError `json:"error,omitempty"`
}

// ErrNotLoopback is returned for a non-loopback listen address or peer.
var ErrNotLoopback = errors.New("agent: lab transport is loopback-only")

// ListenLoopback listens on addr, which must be a loopback IP literal with a
// port (for example "127.0.0.1:0" or "[::1]:7070"). Host names, including
// "localhost", are refused because their resolution is not under the agent's
// control.
func ListenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("agent: listen address %q: %w", addr, err)
	}
	if !loopbackIP(host) {
		return nil, fmt.Errorf("%w: %q", ErrNotLoopback, addr)
	}
	return net.Listen("tcp", addr)
}

// NewLabServer returns an http.Server with conservative timeouts serving
// NewLabHTTPHandler(h). Serve it on a listener from ListenLoopback.
func NewLabServer(h Handler) *http.Server {
	return &http.Server{
		Handler:           NewLabHTTPHandler(h),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       time.Minute,
	}
}

// NewLabHTTPHandler exposes h as POST LabCommandPath with a JSON Command body.
// It is an unauthenticated lab transport; see the comment at the top of this
// file.
func NewLabHTTPHandler(h Handler) http.Handler {
	return newCommandHandler(h, func(r *http.Request) string {
		if !loopbackHostPort(r.RemoteAddr) {
			return "peer is not loopback"
		}
		if !loopbackHostHeader(r.Host) {
			return "Host header is not a loopback address"
		}
		return ""
	})
}

// newCommandHandler serves POST LabCommandPath. peer returns a non-empty
// reason to refuse the request with 403 before anything is decoded.
func newCommandHandler(h Handler, peer func(*http.Request) string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(LabCommandPath, func(w http.ResponseWriter, r *http.Request) {
		// Every answer (also a refusal) names the agent build so the manager
		// can show the version of a host's agent.
		w.Header().Set(VersionHeader, version.Version)
		if reason := peer(r); reason != "" {
			writeLab(w, http.StatusForbidden, labResponse{Error: &labError{httpCodeForbidden, reason}})
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeLab(w, http.StatusMethodNotAllowed, labResponse{Error: &labError{httpCodeMethod, "use POST"}})
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			writeLab(w, http.StatusUnsupportedMediaType, labResponse{Error: &labError{httpCodeMedia, "Content-Type must be application/json"}})
			return
		}
		var cmd Command
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLabRequestBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cmd); err != nil {
			writeLab(w, http.StatusBadRequest, labResponse{Error: &labError{httpCodeInvalid, "decode command: " + err.Error()}})
			return
		}
		if dec.More() {
			writeLab(w, http.StatusBadRequest, labResponse{Error: &labError{httpCodeInvalid, "trailing data after command"}})
			return
		}
		res, err := h.Handle(r.Context(), cmd)
		if err == nil {
			writeLab(w, http.StatusOK, labResponse{Result: &res})
			return
		}
		status, code := http.StatusInternalServerError, httpCodeInternal
		var resp labResponse
		switch {
		case errors.Is(err, ErrInvalidCommand):
			status, code = http.StatusBadRequest, httpCodeInvalid
		case errors.Is(err, fencing.ErrStaleTerm):
			status, code = http.StatusConflict, httpCodeStale
		case errors.Is(err, ErrStepConflict):
			status, code = http.StatusConflict, httpCodeConflict
		case errors.Is(err, ErrPersist):
			code = httpCodePersist
			if res.Status != "" {
				resp.Result = &res
			}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			status, code = http.StatusServiceUnavailable, httpCodeCancelled
		}
		resp.Error = &labError{Code: code, Message: err.Error()}
		writeLab(w, status, resp)
	})
	return mux
}

func writeLab(w http.ResponseWriter, status int, body labResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Client sends commands to an agent command endpoint: the lab HTTP endpoint
// (BaseURL http://, HTTP nil) or the mTLS endpoint (NewMTLSClient). It
// implements Handler. A transport error means unknown completion: the
// caller must re-send the same (OpID, StepID) to learn the outcome, never a
// new step.
type Client struct {
	BaseURL string // for example "http://127.0.0.1:7070" or "https://10.0.0.5:7443"
	HTTP    *http.Client
	// OnResponse, when set, is called after every HTTP response of the
	// agent (also an error answer) with the version the agent reported
	// ("" when it sent none). It is not called on transport errors.
	OnResponse func(agentVersion string)
}

// VersionHeader is the response header that carries the agent build version.
const VersionHeader = "X-Dbcm-Agent-Version"

// LabClient is the name of Client used with the lab transport.
type LabClient = Client

var _ Handler = (*Client)(nil)

// Handle implements Handler over HTTP. Server-side rejections are mapped back
// to ErrInvalidCommand, fencing.ErrStaleTerm, ErrStepConflict and ErrPersist.
func (c *Client) Handle(ctx context.Context, cmd Command) (Result, error) {
	body, err := json.Marshal(cmd)
	if err != nil {
		return Result{}, fmt.Errorf("agent: encode command: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+LabCommandPath, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if c.OnResponse != nil {
		c.OnResponse(resp.Header.Get(VersionHeader))
	}
	var lr labResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLabRequestBody)).Decode(&lr); err != nil {
		return Result{}, fmt.Errorf("agent: decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	var res Result
	if lr.Result != nil {
		res = *lr.Result
	}
	if resp.StatusCode == http.StatusOK && lr.Error == nil && lr.Result != nil {
		return res, nil
	}
	if lr.Error == nil {
		return Result{}, fmt.Errorf("agent: unexpected response HTTP %d", resp.StatusCode)
	}
	var sentinel error
	switch lr.Error.Code {
	case httpCodeInvalid:
		sentinel = ErrInvalidCommand
	case httpCodeStale:
		sentinel = fencing.ErrStaleTerm
	case httpCodeConflict:
		sentinel = ErrStepConflict
	case httpCodePersist:
		sentinel = ErrPersist
	case httpCodeCancelled:
		sentinel = context.Canceled
	}
	if sentinel != nil {
		return res, fmt.Errorf("%w (remote: %s)", sentinel, lr.Error.Message)
	}
	return res, fmt.Errorf("agent: remote error %s (HTTP %d): %s", lr.Error.Code, resp.StatusCode, lr.Error.Message)
}

func loopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackHostPort(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	return err == nil && loopbackIP(host)
}

// loopbackHostHeader accepts a loopback IP literal with or without port.
func loopbackHostHeader(h string) bool {
	if loopbackHostPort(h) {
		return true
	}
	return loopbackIP(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))
}
