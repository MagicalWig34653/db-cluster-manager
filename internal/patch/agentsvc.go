// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
)

// CommandPath is the agent endpoint of the patch mode. It is served only on
// the mTLS transport (internal/agent.ServerTLSConfig).
const CommandPath = "/patch/v1/command"

// Command kinds.
const (
	CmdReport   = "report"
	CmdSimulate = "simulate"
	CmdApply    = "apply"
	CmdReboot   = "reboot"
	// CmdDenyList replaces the agent's certificate deny list. It is fenced
	// by term like the mutating commands but works on report-only agents.
	CmdDenyList = "deny-list"
)

// Errors of the agent side.
var (
	ErrReadOnly = errors.New("patch: agent runs in report-only mode (apply not enabled on this host)")
	ErrStale    = fencing.ErrStaleTerm
)

// AgentCommand is what the manager sends. Term fences mutating commands.
type AgentCommand struct {
	Kind         string   `json:"kind"`
	Term         uint64   `json:"term,omitempty"`
	StepID       string   `json:"stepId,omitempty"`
	Packages     []string `json:"packages,omitempty"`
	SecurityOnly bool     `json:"securityOnly,omitempty"`
	Refresh      bool     `json:"refresh,omitempty"`
	// DenyRevision and DenySerials carry CmdDenyList: revoked certificate
	// serials (lowercase hex) and a revision that grows with every change.
	DenyRevision int64    `json:"denyRevision,omitempty"`
	DenySerials  []string `json:"denySerials,omitempty"`
}

// AgentResult is the agent's answer.
type AgentResult struct {
	Report     *HostReport `json:"report,omitempty"`
	Simulation *Simulation `json:"simulation,omitempty"`
	// Output is a bounded tail of the package manager's output.
	Output string `json:"output,omitempty"`
}

// HostAgent is the manager's view of the patch agent of one host.
type HostAgent interface {
	Do(ctx context.Context, hostID string, c AgentCommand) (AgentResult, error)
}

// AgentService executes AgentCommands on the local host.
type AgentService struct {
	Collector Collector
	// Guard admits terms of mutating commands. Required when AllowApply.
	Guard *fencing.Guard
	// AllowApply enables apply and reboot. Default false: the agent reports
	// and simulates only.
	AllowApply bool
	Now        func() time.Time

	// State is the durable replay suppression and deny list (OpenAgentState).
	// Nil keeps them in memory only.
	State *AgentState

	mu sync.Mutex
}

func (s *AgentService) state() *AgentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.State == nil {
		s.State = NewMemoryAgentState()
	}
	return s.State
}

// Revoked reports whether a certificate serial is on the deny list the
// manager distributed. Pass it as MTLSServerConfig.Revoked.
func (s *AgentService) Revoked(serial *big.Int) bool { return s.state().Revoked(serial) }

func (s *AgentService) denyList(c AgentCommand) (AgentResult, error) {
	if s.Guard == nil {
		return AgentResult{}, fmt.Errorf("%w: deny list needs term fencing on this agent", ErrInvalid)
	}
	if c.Term == 0 {
		return AgentResult{}, fmt.Errorf("%w: deny list needs a term", ErrInvalid)
	}
	if err := checkDenyList(c.DenyRevision, c.DenySerials); err != nil {
		return AgentResult{}, err
	}
	if err := s.Guard.Admit(c.Term); err != nil {
		return AgentResult{}, err
	}
	kept, rev, err := s.state().SetDenyList(c.DenyRevision, c.DenySerials)
	if err != nil {
		return AgentResult{}, err
	}
	if kept {
		return AgentResult{Output: fmt.Sprintf("kept revision %d", rev)}, nil
	}
	return AgentResult{}, nil
}

const maxTail = 4000

func tail(s string) string {
	if len(s) > maxTail {
		return s[len(s)-maxTail:]
	}
	return s
}

// Handle runs one command.
func (s *AgentService) Handle(ctx context.Context, c AgentCommand) (AgentResult, error) {
	switch c.Kind {
	case CmdReport:
		col := s.Collector
		col.Refresh = c.Refresh
		rep, err := col.Collect(ctx)
		if err != nil {
			return AgentResult{}, err
		}
		return AgentResult{Report: &rep}, nil
	case CmdSimulate:
		ad, ups, err := s.updates(ctx)
		if err != nil {
			return AgentResult{}, err
		}
		sim, err := ad.Simulate(ctx, s.Collector.Runner, ups, Selection{Packages: c.Packages, SecurityOnly: c.SecurityOnly})
		if err != nil {
			return AgentResult{}, err
		}
		return AgentResult{Simulation: &sim}, nil
	case CmdApply, CmdReboot:
		return s.mutate(ctx, c)
	case CmdDenyList:
		return s.denyList(c)
	}
	return AgentResult{}, fmt.Errorf("%w: command kind %q", ErrInvalid, c.Kind)
}

func (s *AgentService) updates(ctx context.Context) (Adapter, []Update, error) {
	m, err := Detect(s.Collector.FS)
	if err != nil {
		return nil, nil, err
	}
	ad, err := NewAdapter(m)
	if err != nil {
		return nil, nil, err
	}
	ups, _, _, err := ad.Updates(ctx, s.Collector.Runner)
	return ad, ups, err
}

func (s *AgentService) mutate(ctx context.Context, c AgentCommand) (AgentResult, error) {
	if !s.AllowApply || s.Guard == nil {
		return AgentResult{}, ErrReadOnly
	}
	if c.StepID == "" || c.Term == 0 {
		return AgentResult{}, fmt.Errorf("%w: mutating commands need a step ID and a term", ErrInvalid)
	}
	if err := s.Guard.Admit(c.Term); err != nil {
		return AgentResult{}, err
	}
	st := s.state()
	if r, done, known := st.Lookup(c.StepID); known {
		if done {
			return r, nil
		}
		return AgentResult{}, ErrStepInterrupted
	}
	// The start is on disk before the command runs; without it nothing runs.
	if err := st.Begin(c.StepID); err != nil {
		return AgentResult{}, err
	}
	var res AgentResult
	switch c.Kind {
	case CmdApply:
		if len(c.Packages) == 0 {
			return AgentResult{}, fmt.Errorf("%w: apply needs explicit packages", ErrInvalid)
		}
		ad, ups, err := s.updates(ctx)
		if err != nil {
			_ = st.Abort(c.StepID)
			return AgentResult{}, err
		}
		out, err := ad.Apply(ctx, s.Collector.Runner, ups, Selection{Packages: c.Packages, SecurityOnly: c.SecurityOnly})
		res.Output = tail(out.Stdout + out.Stderr)
		if err != nil {
			_ = st.Abort(c.StepID)
			return res, err
		}
	case CmdReboot:
		out, err := s.Collector.Runner.Run(ctx, "systemctl", "reboot")
		res.Output = tail(out.Stdout + out.Stderr)
		if err != nil {
			_ = st.Abort(c.StepID)
			return res, err
		}
		if out.ExitCode != 0 {
			_ = st.Abort(c.StepID)
			return res, fmt.Errorf("patch: systemctl reboot exited %d", out.ExitCode)
		}
	}
	// A failed write leaves the step marked as started: a replay is then
	// refused instead of repeating a command that did run.
	_ = st.Finish(c.StepID, res)
	return res, nil
}

// Handler serves CommandPath. peer, when set, may veto a request (for
// example when the TLS layer did not verify a client certificate).
func (s *AgentService) Handler(peer func(*http.Request) string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(CommandPath, func(w http.ResponseWriter, r *http.Request) {
		if peer != nil {
			if why := peer(r); why != "" {
				http.Error(w, why, http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var c AgentCommand
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			http.Error(w, "bad command", http.StatusBadRequest)
			return
		}
		res, err := s.Handle(r.Context(), c)
		w.Header().Set("Content-Type", "application/json")
		code := http.StatusOK
		var body any = res
		if err != nil {
			code = http.StatusInternalServerError
			switch {
			case errors.Is(err, ErrReadOnly):
				code = http.StatusForbidden
			case errors.Is(err, ErrStale):
				code = http.StatusConflict
			case errors.Is(err, ErrStepInterrupted):
				code = http.StatusUnprocessableEntity
			case errors.Is(err, ErrInvalid):
				code = http.StatusBadRequest
			}
			body = struct {
				AgentResult
				Error string `json:"error"`
			}{res, err.Error()}
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
	return mux
}

// HTTPHostAgent talks to the patch agents over an http.Client that carries
// the mTLS configuration. URL maps a host ID to the agent's base URL.
type HTTPHostAgent struct {
	Client *http.Client
	URL    func(hostID string) (string, bool)
}

// Do implements HostAgent.
func (h HTTPHostAgent) Do(ctx context.Context, hostID string, c AgentCommand) (AgentResult, error) {
	base, ok := h.URL(hostID)
	if !ok {
		return AgentResult{}, fmt.Errorf("%w: host %q", ErrNotFound, hostID)
	}
	b, _ := json.Marshal(c)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+CommandPath, bytes.NewReader(b))
	if err != nil {
		return AgentResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return AgentResult{}, err
	}
	defer resp.Body.Close()
	var out struct {
		AgentResult
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil && resp.StatusCode == http.StatusOK {
		return AgentResult{}, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return out.AgentResult, nil
	case resp.StatusCode == http.StatusForbidden:
		return out.AgentResult, fmt.Errorf("%w: %s", ErrReadOnly, out.Error)
	case resp.StatusCode == http.StatusConflict:
		return out.AgentResult, fmt.Errorf("%w: %s", ErrStale, out.Error)
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return out.AgentResult, fmt.Errorf("%w: %s", ErrStepInterrupted, out.Error)
	}
	return out.AgentResult, fmt.Errorf("patch: agent answered %d: %s", resp.StatusCode, out.Error)
}
