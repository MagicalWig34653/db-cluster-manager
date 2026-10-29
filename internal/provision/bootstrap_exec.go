// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// HostKey is a host key observed by a probe. The operator confirms the
// fingerprint out of band before any credential is used.
type HostKey struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	KnownHosts  string `json:"-"`
}

// HostKeyProbe reads the host key of exactly one host without authenticating.
type HostKeyProbe interface {
	Probe(context.Context, Host) (HostKey, error)
}

// SSHHostKeyProbe connects to one host on port 22 (or Port), records the
// offered ed25519 (preferred) or other host key and aborts the handshake
// before any authentication. It sends no credential.
type SSHHostKeyProbe struct {
	Timeout time.Duration
	Port    int
}

// Probe implements HostKeyProbe.
func (p SSHHostKeyProbe) Probe(ctx context.Context, h Host) (HostKey, error) {
	if !safeName.MatchString(h.Address) {
		return HostKey{}, ErrInvalid
	}
	timeout, port := p.Timeout, p.Port
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if port <= 0 {
		port = 22
	}
	addr := net.JoinHostPort(h.Address, strconv.Itoa(port))
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return HostKey{}, errors.New("provision: SSH dial failed")
	}
	defer conn.Close()
	var got ssh.PublicKey
	cfg := &ssh.ClientConfig{User: "probe", Timeout: timeout,
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			got = k
			return errors.New("probe: key recorded")
		}}
	_, _, _, _ = ssh.NewClientConn(conn, addr, cfg)
	if got == nil {
		return HostKey{}, errors.New("provision: no host key offered")
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(got)))
	return HostKey{Type: got.Type(), Fingerprint: ssh.FingerprintSHA256(got), KnownHosts: addrName(h.Address, port) + " " + line + "\n"}, nil
}

func addrName(host string, port int) string {
	if port == 22 {
		return host
	}
	return "[" + host + "]:" + strconv.Itoa(port)
}

// TokenSource issues a one-time enrollment token for a host and runtime.
type TokenSource interface {
	EnrollToken(ctx context.Context, h Host, runtime string) (string, error)
}

// ManagerActions are the steps that run inside the manager, not on the host.
type ManagerActions interface {
	DeployContainerAgent(ctx context.Context, h Host) error
	StoreDedicatedKey(ctx context.Context, h Host, c Credential) error
}

// BootstrapDeps are the collaborators of the executor.
type BootstrapDeps struct {
	Runner  CommandRunner
	Tokens  TokenSource
	Agents  AgentScripts
	Manager ManagerActions
	Journal *opjournal.Journal
}

// BootstrapSecrets are write-only inputs of one run. They are never
// serialized, journaled or logged.
type BootstrapSecrets struct {
	Root      Credential
	Dedicated Credential
	JoinToken string
}

// StepProgress is the state of one step.
type StepProgress struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
}

// BootstrapProgress is the outcome of one executor call.
type BootstrapProgress struct {
	MainDone     bool           `json:"mainDone"`
	LockdownDone bool           `json:"lockdownDone"`
	FailedStep   string         `json:"failedStep,omitempty"`
	Steps        []StepProgress `json:"steps"`
}

// OpID is the journal operation ID of a bootstrap.
func OpID(planID string) string { return "bootstrap/" + planID }

func progressOf(plan BootstrapPlan, j *opjournal.Journal) BootstrapProgress {
	states := map[string]opjournal.State{}
	if op, err := j.Get(OpID(plan.ID)); err == nil {
		for _, s := range op.Steps {
			states[s.ID] = s.State
		}
	}
	pr := BootstrapProgress{MainDone: true, LockdownDone: false, Steps: []StepProgress{}}
	gated, gatedDone := 0, 0
	for _, s := range plan.Steps {
		st := states[s.ID]
		if st == "" {
			st = opjournal.Pending
		}
		pr.Steps = append(pr.Steps, StepProgress{ID: s.ID, Title: s.Title, State: string(st)})
		if s.Gate == "" && st != opjournal.Succeeded {
			pr.MainDone = false
		}
		if s.Gate != "" {
			gated++
			if st == opjournal.Succeeded {
				gatedDone++
			}
		}
	}
	pr.LockdownDone = gated > 0 && gated == gatedDone
	return pr
}

func parseDone(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "dbcm:done=yes" {
			return true
		}
	}
	return false
}

// ExecuteBootstrap runs the main phase (lockdown false) or the lockdown phase
// (lockdown true) of an approved plan. It verifies every digest first, journals
// each step under the given leader term (a lower term is fenced out by the
// journal), skips steps that the host already satisfies, stops at the first
// failure and can be called again to resume. An unknown outcome of a mutating
// command leaves the step Unknown; the resume re-checks it before running.
func ExecuteBootstrap(ctx context.Context, d BootstrapDeps, plan BootstrapPlan, sec BootstrapSecrets, term uint64, lockdown bool) (BootstrapProgress, error) {
	if err := plan.VerifyDigests(); err != nil {
		return BootstrapProgress{}, err
	}
	if plan.Blocked {
		return BootstrapProgress{}, ErrBootstrapBlocked
	}
	if d.Journal == nil || d.Runner == nil {
		return BootstrapProgress{}, ErrNoHook
	}
	ids := make([]string, len(plan.Steps))
	for i, s := range plan.Steps {
		ids[i] = s.ID
	}
	op := OpID(plan.ID)
	if err := d.Journal.Begin(op, "bootstrap", ids, term); err != nil {
		if !errors.Is(err, opjournal.ErrExists) {
			return BootstrapProgress{}, err
		}
		if err := d.Journal.Adopt(op, term); err != nil {
			return BootstrapProgress{}, err
		}
	}
	if lockdown && !progressOf(plan, d.Journal).MainDone {
		return progressOf(plan, d.Journal), fmt.Errorf("%w: main phase is not complete", ErrBootstrapState)
	}
	cur, _ := d.Journal.Get(op)
	state := map[string]opjournal.State{}
	for _, s := range cur.Steps {
		state[s.ID] = s.State
	}
	for _, st := range plan.Steps {
		if (st.Gate != "") != lockdown || state[st.ID] == opjournal.Succeeded {
			continue
		}
		if err := d.Journal.StartStep(op, st.ID, term); err != nil {
			return progressOf(plan, d.Journal), err
		}
		_, err := runStep(ctx, d, plan, st, sec)
		finish := opjournal.Succeeded
		switch {
		case err == nil:
		case errors.Is(err, ErrOutcomeUnknown):
			finish = opjournal.Unknown
		default:
			finish = opjournal.Failed
		}
		if ferr := d.Journal.FinishStep(op, st.ID, finish, term); ferr != nil {
			return progressOf(plan, d.Journal), ferr
		}
		if err != nil {
			pr := progressOf(plan, d.Journal)
			pr.FailedStep = st.ID
			return pr, err
		}
	}
	return progressOf(plan, d.Journal), nil
}

func stepTarget(plan BootstrapPlan, st BootstrapStep, sec BootstrapSecrets) (Host, Credential) {
	h := plan.Host
	if st.RunAs == "dedicated" {
		h.User = plan.Profile.ServiceUser
		return h, sec.Dedicated
	}
	return h, sec.Root
}

func wrap(cmd string, root bool) string {
	if root {
		return cmd
	}
	return "sudo -n sh -c " + shellQuote(cmd)
}

func runStep(ctx context.Context, d BootstrapDeps, plan BootstrapPlan, st BootstrapStep, sec BootstrapSecrets) (string, error) {
	if st.ManagerAction != "" {
		if d.Manager == nil {
			return "", ErrNoHook
		}
		switch st.ManagerAction {
		case "deploy-container-agent":
			if err := d.Manager.DeployContainerAgent(ctx, plan.Host); err != nil {
				return "", fmt.Errorf("provision: deploy container agent: %w", ErrOutcomeUnknown)
			}
		case "store-dedicated-key":
			if err := d.Manager.StoreDedicatedKey(ctx, plan.Host, sec.Dedicated); err != nil {
				return "", errors.New("provision: sealing the dedicated key failed")
			}
		default:
			return "", ErrInvalid
		}
		return "", nil
	}
	h, cred := stepTarget(plan, st, sec)
	asRoot := h.User == "root" // other logins run the script through sudo
	if st.Check != "" {
		out, err := d.Runner.Run(ctx, h, cred, wrap(st.Check, asRoot), "")
		if err != nil {
			return "", errors.New("provision: step check failed: " + st.ID)
		}
		if parseDone(out) {
			return "", nil
		}
	}
	stdin := ""
	switch st.Stdin {
	case "enrollment":
		if d.Tokens == nil {
			return "", ErrNoHook
		}
		tok, err := d.Tokens.EnrollToken(ctx, plan.Host, "native")
		if err != nil || tok == "" || strings.ContainsAny(tok, " \n\r\t") {
			return "", errors.New("provision: enrollment token unavailable")
		}
		stdin = tok + "\n"
	case "join":
		if sec.JoinToken == "" || strings.ContainsAny(sec.JoinToken, " \n\r\t") {
			return "", errors.New("provision: join token missing")
		}
		stdin = sec.JoinToken + "\n"
	}
	cmd := st.Script
	if st.Unprivileged {
		_, err := d.Runner.Run(ctx, h, cred, cmd, stdin)
		if err != nil {
			return "", errors.New("provision: step failed: " + st.ID)
		}
		return "", nil
	}
	if _, err := d.Runner.Run(ctx, h, cred, wrap(cmd, asRoot), stdin); err != nil {
		if st.Mutating {
			return "", fmt.Errorf("%w: step %s", ErrOutcomeUnknown, st.ID)
		}
		return "", errors.New("provision: step failed: " + st.ID)
	}
	return "", nil
}
