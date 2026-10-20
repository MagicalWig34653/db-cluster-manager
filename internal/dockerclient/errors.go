// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// Sentinel errors matched by APIError.Is. ErrNotFound is
// agent.ErrObjectNotFound, so the executor maps it without knowing this
// package.
var (
	ErrNotFound    = agent.ErrObjectNotFound
	ErrConflict    = errors.New("dockerclient: conflict")
	ErrBadRequest  = errors.New("dockerclient: bad request")
	ErrForbidden   = errors.New("dockerclient: forbidden")
	ErrServer      = errors.New("dockerclient: daemon error")
	ErrUnavailable = errors.New("dockerclient: daemon unavailable") // 503, for example "node is not part of a swarm"
	// ErrInvalidArgument is returned before any request for an argument the
	// client refuses to send (for example an empty or malformed object ID).
	ErrInvalidArgument = errors.New("dockerclient: invalid argument")
)

// APIError is a non-2xx response from the daemon. Message is the "message"
// field of the ErrorResponse object, or the raw body if it is not JSON.
type APIError struct {
	StatusCode int
	Message    string
	Method     string
	Path       string
	// Mutating is true for create, update and delete requests. A 5xx on a
	// mutating request also matches agent.ErrOutcomeUnknown.
	Mutating bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("dockerclient: %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// Is maps the status code to the sentinel errors of this package and to
// agent.ErrOutcomeUnknown.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrConflict:
		return e.StatusCode == http.StatusConflict
	case ErrBadRequest:
		return e.StatusCode == http.StatusBadRequest
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrUnavailable:
		return e.StatusCode == http.StatusServiceUnavailable
	case ErrServer:
		return e.StatusCode >= 500
	case agent.ErrOutcomeUnknown:
		return e.Mutating && e.StatusCode >= 500
	}
	return false
}
