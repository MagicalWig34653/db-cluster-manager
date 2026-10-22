// SPDX-License-Identifier: AGPL-3.0-only

// Package api implements the versioned HTTP API of the manager.
//
// The contract is described in api/openapi.yaml; TestRoutesMatchOpenAPI keeps
// the route table (Routes) and the contract in sync.
//
// Two handlers exist:
//
//   - NewHandler is the unauthenticated prototype: only health and version.
//   - New mounts the full API: first-run setup, passkey login, sessions,
//     recovery, and authorized read-only resource listings. It requires a
//     relying-party configuration and the in-memory stores of
//     internal/recovery, internal/passkey and internal/authz.
//
// Security model of New (see server.go for details):
//
//   - Every endpoint declares how it is accessed (public, any session,
//     re-enrolment session, permission-filtered listing, or an explicit
//     authz action/resource). An endpoint without a declaration is a
//     programming error and panics at construction: deny by default.
//   - State-changing requests must carry an Origin header that exactly
//     matches a configured passkey origin (CSRF and cross-origin defence).
//   - Unauthenticated failures return generic error bodies; details go to
//     the server log only.
//   - Every state change writes an audit record first; if the audit sink
//     fails, the request is denied.
//
// No persistence exists: accounts, sessions, policies and audit records live
// in whatever stores the caller passes in (in memory today).
package api

import (
	"encoding/json"
	"net/http"
)

// Health is the liveness response. It reports only that the process serves requests.
type Health struct {
	Status string `json:"status"`
}

// Route is one registered endpoint. Path is relative to /api/v1 and uses the
// OpenAPI template syntax, for example "/clusters/{id}".
type Route struct {
	Method string
	Path   string
}

// Prefix is the mount point of the versioned API.
const Prefix = "/api/v1"

// Routes returns every route the full API (New) registers, in registration
// order. NewHandler registers only the public process endpoints.
func Routes() []Route {
	eps := (*server)(nil).endpoints()
	out := make([]Route, 0, len(eps))
	for _, ep := range eps {
		out = append(out, Route{Method: ep.method, Path: ep.path})
	}
	return out
}

// NewHandler returns the unauthenticated prototype handler, rooted at /api/v1.
// It serves only health and version; every other API path is a JSON 404.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	var s *server
	for _, ep := range s.endpoints() {
		if !ep.prototype {
			continue
		}
		h := ep.handle
		mux.HandleFunc(ep.method+" "+Prefix+ep.path, func(w http.ResponseWriter, r *http.Request) {
			h(w, r, nil)
		})
	}
	mux.HandleFunc("/api/", notFound)
	return mux
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, errNotFound)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error codes returned in {"error": code}. They are deliberately coarse:
// unauthenticated callers learn only the class of failure, never which
// account, credential or check failed.
const (
	errNotFound          = "not_found"
	errConflict          = "conflict"
	errBadRequest        = "bad_request"
	errAuthFailed        = "authentication_failed"
	errUnauthenticated   = "unauthenticated"
	errForbidden         = "forbidden"
	errEnrollmentNeeded  = "passkey_enrollment_required"
	errOriginRejected    = "origin_rejected"
	errRateLimited       = "rate_limited"
	errUnavailable       = "unavailable"
	errSetupUnavailable  = "setup_unavailable"
	errInternal          = "internal_error"
	errMethodNotAllowed  = "method_not_allowed"
	errRequestTooLarge   = "request_too_large"
	errUnsupportedFormat = "unsupported_media_type"
)

// ErrorBody is the JSON body of every error response.
type ErrorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, ErrorBody{Error: code})
}
