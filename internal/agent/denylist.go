// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"math/big"
	"regexp"
)

// Certificate deny list on the agent.
//
// The manager distributes the serials it revoked (superseded or revoked
// agent certificates and revoked manager client certificates of a removed
// or compromised replica) with set-deny-list. The agent persists the list
// in its state file and its mTLS listener rejects any presented chain with
// a denied serial (MTLSServerConfig.Revoked = Executor.Revoked), so a
// revoked manager certificate cannot command it even before it expires.
// A list with a lower revision than the stored one is ignored (a stale
// leader is fenced anyway; this also keeps an older replayed list from
// shrinking the stored one).

// KindSetDenyList replaces the stored deny list (args: SetDenyListArgs).
const KindSetDenyList Kind = "set-deny-list"

// maxDenied bounds the list size.
const maxDenied = 100_000

var serialRe = regexp.MustCompile(`^[0-9a-f]{1,40}$`)

// SetDenyListArgs are the arguments of KindSetDenyList: revoked
// certificate serials (lowercase hex) and a revision that grows with
// every change.
type SetDenyListArgs struct {
	Revision int64    `json:"revision"`
	Serials  []string `json:"serials"`
}

// DenyListOutput is the output of KindSetDenyList.
type DenyListOutput struct {
	Revision int64 `json:"revision"`
	Count    int   `json:"count"`
	// Kept is true when the stored list was newer and stayed in place.
	Kept bool `json:"kept,omitempty"`
}

func checkDenyList(a *SetDenyListArgs) error {
	if a.Revision < 0 || len(a.Serials) > maxDenied {
		return invalid("revision must be >= 0 and at most %d serials", maxDenied)
	}
	for _, s := range a.Serials {
		if !serialRe.MatchString(s) {
			return invalid("serial %q is not lowercase hex", s)
		}
	}
	return nil
}

func denySet(serials []string) map[string]bool {
	m := make(map[string]bool, len(serials))
	for _, s := range serials {
		m[s] = true
	}
	return m
}

// setDenyList runs under the executor lock.
func (e *Executor) setDenyList(a SetDenyListArgs) Result {
	if a.Revision < e.state.DenyRevision {
		return succeeded(DenyListOutput{Revision: e.state.DenyRevision, Count: len(e.state.Denied), Kept: true})
	}
	e.state.DenyRevision = a.Revision
	e.state.Denied = append([]string(nil), a.Serials...)
	m := denySet(a.Serials)
	e.deny.Store(&m)
	return succeeded(DenyListOutput{Revision: a.Revision, Count: len(a.Serials)})
}

// Revoked reports whether a certificate serial is on the distributed deny
// list. It is safe to call from TLS handshakes while a command runs.
func (e *Executor) Revoked(serial *big.Int) bool {
	m := e.deny.Load()
	return m != nil && (*m)[serial.Text(16)]
}

// DenyRevision returns the revision of the stored deny list.
func (e *Executor) DenyRevision() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.DenyRevision
}
