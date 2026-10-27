// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Durable state of the patch agent: replay suppression of mutating steps and
// the certificate deny list.
//
// A mutating step is recorded as started (and flushed) before it runs and as
// finished with its result afterwards. A replay of a finished step returns
// the stored result. A replay of a step that was started but has no result
// (the agent or host died while it ran, for example during the reboot step)
// is refused with ErrStepInterrupted: the outcome is unknown and the manager
// must not blindly repeat an apply or a reboot. A step that failed is
// removed again so the manager may retry it.

// ErrStepInterrupted means the step was started earlier and has no recorded
// result.
var ErrStepInterrupted = errors.New("patch: step was started earlier and its outcome is unknown; not re-run")

const maxDenied = 100_000

// maxStoredSteps bounds the remembered steps (a variable for tests).
var maxStoredSteps = 1000

var serialRe = regexp.MustCompile(`^[0-9a-f]{1,40}$`)

type stepRecord struct {
	Done   bool        `json:"done"`
	Result AgentResult `json:"result,omitempty"`
}

type agentStateFile struct {
	Version      int                   `json:"version"`
	DenyRevision int64                 `json:"denyRevision,omitempty"`
	Denied       []string              `json:"denied,omitempty"`
	Steps        map[string]stepRecord `json:"steps,omitempty"`
	Order        []string              `json:"order,omitempty"` // step IDs, oldest first
}

// AgentState is the agent's durable state. A zero path keeps it in memory.
type AgentState struct {
	mu   sync.Mutex
	path string
	f    agentStateFile
	deny map[string]bool
}

// NewMemoryAgentState returns a non-durable state (tests, report-only use).
func NewMemoryAgentState() *AgentState {
	return &AgentState{f: agentStateFile{Version: 1, Steps: map[string]stepRecord{}}, deny: map[string]bool{}}
}

// OpenAgentState loads (or creates) the state file. A corrupt file is an
// error: the agent must not start with forgotten steps or a forgotten deny
// list.
func OpenAgentState(path string) (*AgentState, error) {
	s := NewMemoryAgentState()
	s.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("patch: read agent state: %w", err)
	}
	if err := json.Unmarshal(b, &s.f); err != nil {
		return nil, fmt.Errorf("patch: corrupt agent state %s: %w", path, err)
	}
	if s.f.Steps == nil {
		s.f.Steps = map[string]stepRecord{}
	}
	s.deny = denySet(s.f.Denied)
	return s, nil
}

func denySet(serials []string) map[string]bool {
	m := make(map[string]bool, len(serials))
	for _, v := range serials {
		m[v] = true
	}
	return m
}

// save writes the file atomically (temp file, fsync, rename, directory sync).
// The caller holds s.mu.
func (s *AgentState) save() (err error) {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(s.f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".patch-state-*")
	if err != nil {
		return fmt.Errorf("patch: agent state: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Lookup reports a known step: done with its result, or started only.
func (s *AgentState) Lookup(id string) (res AgentResult, done, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.f.Steps[id]
	return r.Result, r.Done, ok
}

// Begin records the step as started and flushes it. It fails when the step
// is already known, and when the record cannot be written (fail closed: a
// step that cannot be recorded is not run).
func (s *AgentState) Begin(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.f.Steps[id]; ok {
		return ErrStepInterrupted
	}
	s.f.Steps[id] = stepRecord{}
	s.f.Order = append(s.f.Order, id)
	if err := s.save(); err != nil {
		s.remove(id)
		return err
	}
	return nil
}

func (s *AgentState) remove(id string) {
	delete(s.f.Steps, id)
	for i, v := range s.f.Order {
		if v == id {
			s.f.Order = append(s.f.Order[:i], s.f.Order[i+1:]...)
			break
		}
	}
}

// Finish stores the result of a started step and trims the oldest finished
// steps beyond the bound.
func (s *AgentState) Finish(id string, res AgentResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Steps[id] = stepRecord{Done: true, Result: res}
	for len(s.f.Order) > maxStoredSteps {
		old := ""
		for _, v := range s.f.Order {
			if s.f.Steps[v].Done {
				old = v
				break
			}
		}
		if old == "" {
			break
		}
		s.remove(old)
	}
	return s.save()
}

// Abort forgets a started step after a failure so it may be retried.
func (s *AgentState) Abort(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.f.Steps[id]; !ok || r.Done {
		return nil
	}
	s.remove(id)
	return s.save()
}

// StepCount returns the number of stored steps.
func (s *AgentState) StepCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.f.Steps)
}

func checkDenyList(revision int64, serials []string) error {
	if revision < 0 || len(serials) > maxDenied {
		return fmt.Errorf("%w: deny list revision must be >= 0 and at most %d serials", ErrInvalid, maxDenied)
	}
	for _, v := range serials {
		if !serialRe.MatchString(v) {
			return fmt.Errorf("%w: serial %q is not lowercase hex", ErrInvalid, v)
		}
	}
	return nil
}

// SetDenyList replaces the stored list unless the stored revision is newer.
// kept is true then. The new list is on disk before it takes effect.
func (s *AgentState) SetDenyList(revision int64, serials []string) (kept bool, stored int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision < s.f.DenyRevision {
		return true, s.f.DenyRevision, nil
	}
	prevRev, prev := s.f.DenyRevision, s.f.Denied
	s.f.DenyRevision, s.f.Denied = revision, append([]string(nil), serials...)
	if err := s.save(); err != nil {
		s.f.DenyRevision, s.f.Denied = prevRev, prev
		return false, prevRev, err
	}
	s.deny = denySet(s.f.Denied)
	return false, revision, nil
}

// Revoked reports whether a certificate serial is on the deny list. It is
// safe to call from TLS handshakes.
func (s *AgentState) Revoked(serial *big.Int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deny[serial.Text(16)]
}
