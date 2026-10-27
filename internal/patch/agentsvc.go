// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	mu   sync.Mutex
	done map[string]AgentResult // by step ID: a replayed step is not re-run
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
	s.mu.Lock()
	if r, ok := s.done[c.StepID]; ok {
		s.mu.Unlock()
		return r, nil
	}
	s.mu.Unlock()
	var res AgentResult
	switch c.Kind {
	case CmdApply:
		if len(c.Packages) == 0 {
			return AgentResult{}, fmt.Errorf("%w: apply needs explicit packages", ErrInvalid)
		}
		ad, ups, err := s.updates(ctx)
		if err != nil {
			return AgentResult{}, err
		}
		out, err := ad.Apply(ctx, s.Collector.Runner, ups, Selection{Packages: c.Packages, SecurityOnly: c.SecurityOnly})
		res.Output = tail(out.Stdout + out.Stderr)
		if err != nil {
			return res, err
		}
	case CmdReboot:
		out, err := s.Collector.Runner.Run(ctx, "systemctl", "reboot")
		res.Output = tail(out.Stdout + out.Stderr)
		if err != nil {
			return res, err
		}
		if out.ExitCode != 0 {
			return res, fmt.Errorf("patch: systemctl reboot exited %d", out.ExitCode)
		}
	}
	s.mu.Lock()
	if s.done == nil {
		s.done = map[string]AgentResult{}
	}
	s.done[c.StepID] = res
	s.mu.Unlock()
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
	}
	return out.AgentResult, fmt.Errorf("patch: agent answered %d: %s", resp.StatusCode, out.Error)
}
