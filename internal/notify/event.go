// SPDX-License-Identifier: AGPL-3.0-only

package notify

import "time"

// Severity classifies an event. The zero value is SeverityUnknown.
type Severity int

// Severity levels.
const (
	SeverityUnknown Severity = iota
	SeverityInfo
	SeverityWarning
	SeverityError
	SeverityCritical
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	case SeverityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// rank orders severities for threshold filtering. Unknown ranks as warning so
// that unclassified or unobserved state is never filtered out as harmless.
func (s Severity) rank() int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning, SeverityUnknown:
		return 2
	case SeverityError:
		return 3
	case SeverityCritical:
		return 4
	default:
		return 2
	}
}

// Event is one audit/operation/health occurrence. Details must not contain
// secrets.
type Event struct {
	ID       string            `json:"id"`
	Time     time.Time         `json:"time"`
	Severity Severity          `json:"severity"`
	Kind     string            `json:"kind"`
	Resource string            `json:"resource"`
	Message  string            `json:"message"`
	Details  map[string]string `json:"details,omitempty"`
}

func (e Event) clone() Event {
	if e.Details != nil {
		d := make(map[string]string, len(e.Details))
		for k, v := range e.Details {
			d[k] = v
		}
		e.Details = d
	}
	return e
}

// Clock returns the current time; injected for tests.
type Clock func() time.Time
