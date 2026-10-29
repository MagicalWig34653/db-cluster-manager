// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Status of a bootstrap.
type Status string

// Bootstrap statuses.
const (
	StatusAwaitingFingerprint Status = "awaiting-fingerprint"
	StatusPlanned             Status = "planned"
	StatusBlocked             Status = "blocked"
	StatusApproved            Status = "approved"
	StatusRunning             Status = "running"
	StatusPaused              Status = "paused"
	StatusMainComplete        Status = "main-complete"
	StatusCompleted           Status = "completed"
	StatusCancelled           Status = "cancelled"
)

// CredentialTTL is how long write-only credentials stay in memory without
// activity.
const CredentialTTL = time.Hour

// DraftInput is the write-only input of a draft.
type DraftInput struct {
	Request    BootstrapRequest
	Credential Credential
	JoinToken  string
}

// BootstrapView is the secret-free state of a bootstrap.
type BootstrapView struct {
	ID               string         `json:"id"`
	Status           Status         `json:"status"`
	Host             Host           `json:"host"`
	HostKey          *HostKey       `json:"hostKey,omitempty"`
	Plan             *BootstrapPlan `json:"plan,omitempty"`
	Progress         []StepProgress `json:"progress"`
	Error            string         `json:"error,omitempty"`
	MainApproved     bool           `json:"mainApproved"`
	LockdownApproved bool           `json:"lockdownApproved"`
	CredentialsHeld  bool           `json:"credentialsHeld"`
}

type bootDraft struct {
	req              BootstrapRequest
	status           Status
	hostKey          HostKey
	plan             *BootstrapPlan
	secrets          BootstrapSecrets
	expires          time.Time
	mainApproved     bool
	lockdownApproved bool
	errText          string
}

// BootstrapService holds drafts in memory and drives the flow: draft, host
// key confirmation, plan, approve, run, lockdown approval. Drafts and their
// credentials are lost on a manager restart by design (ADR 0027).
type BootstrapService struct {
	deps   BootstrapDeps
	env    BootstrapEnv
	probe  HostKeyProbe
	agents AgentScripts

	// Spawn runs the executor; the default starts a goroutine. Term returns
	// the current leader term. Now is the clock. Tests replace all three.
	Spawn func(func())
	Term  func() uint64
	Now   func() time.Time

	mu     sync.Mutex
	drafts map[string]*bootDraft
}

// NewBootstrapService returns a service.
func NewBootstrapService(deps BootstrapDeps, env BootstrapEnv, probe HostKeyProbe, agents AgentScripts) *BootstrapService {
	if deps.Journal == nil {
		panic("provision: bootstrap service needs a journal")
	}
	return &BootstrapService{deps: deps, env: env, probe: probe, agents: agents, drafts: map[string]*bootDraft{},
		Spawn: func(f func()) { go f() }, Term: func() uint64 { return 1 }, Now: time.Now}
}

func (s *BootstrapService) wipe(d *bootDraft) {
	d.secrets = BootstrapSecrets{}
}

func (s *BootstrapService) expire(d *bootDraft) {
	if d.secrets != (BootstrapSecrets{}) && s.Now().After(d.expires) {
		s.wipe(d)
	}
}

func (s *BootstrapService) hasSecrets(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.drafts[id]
	return d != nil && d.secrets != (BootstrapSecrets{})
}

func (s *BootstrapService) get(id string) (*bootDraft, error) {
	d := s.drafts[id]
	if d == nil {
		return nil, ErrBootstrapNotFound
	}
	s.expire(d)
	return d, nil
}

func (s *BootstrapService) view(d *bootDraft) BootstrapView {
	v := BootstrapView{ID: d.req.ID, Status: d.status, Host: d.req.Host, Progress: []StepProgress{}, Error: d.errText,
		MainApproved: d.mainApproved, LockdownApproved: d.lockdownApproved, CredentialsHeld: d.secrets != (BootstrapSecrets{})}
	if d.hostKey.Fingerprint != "" {
		hk := d.hostKey
		v.HostKey = &hk
	}
	if d.plan != nil {
		p := *d.plan
		v.Plan = &p
		v.Progress = progressOf(p, s.deps.Journal).Steps
	}
	return v
}

// Draft starts a bootstrap: it validates the request, reads the host key of
// that single host without authenticating and waits for the operator to
// confirm the fingerprint. No command is run and no credential is used.
func (s *BootstrapService) Draft(ctx context.Context, in DraftInput) (BootstrapView, error) {
	if err := in.Request.Validate(); err != nil {
		return BootstrapView{}, err
	}
	if in.Credential.PrivateKey == "" && in.Credential.Password == "" {
		return BootstrapView{}, ErrInvalid
	}
	s.mu.Lock()
	if _, ok := s.drafts[in.Request.ID]; ok {
		s.mu.Unlock()
		return BootstrapView{}, ErrConflict
	}
	s.mu.Unlock()
	hk, err := s.probe.Probe(ctx, in.Request.Host)
	if err != nil {
		return BootstrapView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.drafts[in.Request.ID]; ok {
		return BootstrapView{}, ErrConflict
	}
	cred := in.Credential
	cred.KnownHosts = ""
	d := &bootDraft{req: in.Request, status: StatusAwaitingFingerprint, hostKey: hk,
		secrets: BootstrapSecrets{Root: cred, JoinToken: in.JoinToken}, expires: s.Now().Add(CredentialTTL)}
	s.drafts[in.Request.ID] = d
	return s.view(d), nil
}

// ConfirmFingerprint pins the host key if the operator-typed fingerprint
// equals the observed one, then runs the read-only preflight and builds the
// plan. A mismatch cancels the draft and wipes its credentials before any
// connection with them is made.
func (s *BootstrapService) ConfirmFingerprint(ctx context.Context, id, fingerprint string) (BootstrapView, error) {
	s.mu.Lock()
	d, err := s.get(id)
	if err != nil {
		s.mu.Unlock()
		return BootstrapView{}, err
	}
	if d.status != StatusAwaitingFingerprint {
		s.mu.Unlock()
		return BootstrapView{}, ErrBootstrapState
	}
	if fingerprint == "" || fingerprint != d.hostKey.Fingerprint {
		s.wipe(d)
		d.status, d.errText = StatusCancelled, "host key fingerprint mismatch"
		s.mu.Unlock()
		return BootstrapView{}, ErrHostKeyMismatch
	}
	if d.secrets == (BootstrapSecrets{}) {
		s.mu.Unlock()
		return BootstrapView{}, ErrCredentialsExpired
	}
	req, root := d.req, d.secrets.Root
	root.KnownHosts = d.hostKey.KnownHosts
	hk := d.hostKey
	s.mu.Unlock()

	chk, ierr := BootstrapInspect(ctx, s.deps.Runner, req.Host, root, req.Profile)
	var dedPriv, dedPub string
	if ierr == nil && req.Profile.DedicatedKey {
		dedPriv, dedPub, ierr = GenerateDedicatedKey("dbcm-" + req.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d.secrets.Root = root
	if ierr != nil {
		d.status, d.errText = StatusBlocked, ierr.Error()
		return s.view(d), ierr
	}
	req.HostKeyFingerprint = hk.Fingerprint
	d.req = req
	plan, err := BuildBootstrapPlan(req, chk, s.env, s.agents, dedPub)
	if err != nil {
		d.status, d.errText = StatusBlocked, err.Error()
		return s.view(d), err
	}
	d.plan = &plan
	if dedPriv != "" {
		d.secrets.Dedicated = Credential{PrivateKey: dedPriv, KnownHosts: hk.KnownHosts}
	}
	d.status = StatusPlanned
	if plan.Blocked {
		d.status = StatusBlocked
	}
	d.expires = s.Now().Add(CredentialTTL)
	return s.view(d), nil
}

// Preview returns the reviewed plan with all scripts.
func (s *BootstrapService) Preview(id string) (BootstrapPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.get(id)
	if err != nil {
		return BootstrapPlan{}, err
	}
	if d.plan == nil {
		return BootstrapPlan{}, ErrBootstrapState
	}
	return *d.plan, nil
}

// Approve approves the main phase by plan digest.
func (s *BootstrapService) Approve(id, digest string) (BootstrapView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.get(id)
	if err != nil {
		return BootstrapView{}, err
	}
	if d.plan == nil || (d.status != StatusPlanned && d.status != StatusBlocked) {
		return BootstrapView{}, ErrBootstrapState
	}
	if d.plan.Blocked {
		return BootstrapView{}, ErrBootstrapBlocked
	}
	if digest != d.plan.Digest {
		return BootstrapView{}, ErrDigest
	}
	d.mainApproved, d.status = true, StatusApproved
	return s.view(d), nil
}

// ApproveLockdown approves the dedicated-key and root-login phase by its own
// digest. It is only possible after the main phase completed.
func (s *BootstrapService) ApproveLockdown(id, digest string) (BootstrapView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.get(id)
	if err != nil {
		return BootstrapView{}, err
	}
	if d.plan == nil || d.plan.LockdownDigest == "" || d.status != StatusMainComplete {
		return BootstrapView{}, ErrBootstrapState
	}
	if digest != d.plan.LockdownDigest {
		return BootstrapView{}, ErrDigest
	}
	if d.secrets.Dedicated == (Credential{}) {
		return BootstrapView{}, ErrCredentialsExpired
	}
	d.lockdownApproved = true
	return s.view(d), nil
}

// SupplyCredentials gives a draft whose credentials were wiped a new root
// credential for the pinned host key.
func (s *BootstrapService) SupplyCredentials(id string, c Credential) (BootstrapView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.get(id)
	if err != nil {
		return BootstrapView{}, err
	}
	if d.status == StatusCancelled || d.status == StatusCompleted || (c.PrivateKey == "" && c.Password == "") {
		return BootstrapView{}, ErrBootstrapState
	}
	c.KnownHosts = d.hostKey.KnownHosts
	d.secrets.Root = c
	d.expires = s.Now().Add(CredentialTTL)
	return s.view(d), nil
}

// Run starts or resumes the approved phase. The result is the state right
// after the start; poll Status for progress.
func (s *BootstrapService) Run(ctx context.Context, id string) (BootstrapView, error) {
	s.mu.Lock()
	d, err := s.get(id)
	if err != nil {
		s.mu.Unlock()
		return BootstrapView{}, err
	}
	if d.plan == nil || !d.mainApproved {
		s.mu.Unlock()
		return BootstrapView{}, ErrNotApproved
	}
	lockdown := false
	switch d.status {
	case StatusApproved, StatusPaused:
		if progressOf(*d.plan, s.deps.Journal).MainDone {
			lockdown = true
		}
	case StatusMainComplete:
		lockdown = true
	default:
		s.mu.Unlock()
		return BootstrapView{}, ErrBootstrapState
	}
	if lockdown && !d.lockdownApproved {
		s.mu.Unlock()
		return BootstrapView{}, ErrNotApproved
	}
	if d.secrets == (BootstrapSecrets{}) {
		s.mu.Unlock()
		return BootstrapView{}, ErrCredentialsExpired
	}
	d.status, d.errText = StatusRunning, ""
	plan, sec, term := *d.plan, d.secrets, s.Term()
	deps := s.deps
	s.mu.Unlock()

	s.Spawn(func() {
		pr, err := ExecuteBootstrap(context.WithoutCancel(ctx), deps, plan, sec, term, lockdown)
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case err != nil:
			d.status, d.errText = StatusPaused, err.Error()
		case lockdown || plan.LockdownDigest == "":
			d.status = StatusCompleted
			s.wipe(d)
		case pr.MainDone:
			d.status = StatusMainComplete
		}
		d.expires = s.Now().Add(CredentialTTL)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(d), nil
}

// Status returns the current view; an unknown ID yields an empty view.
func (s *BootstrapService) Status(id string) BootstrapView {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.get(id)
	if err != nil {
		return BootstrapView{ID: id, Progress: []StepProgress{}}
	}
	return s.view(d)
}

// List returns the views of all drafts.
func (s *BootstrapService) List() []BootstrapView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []BootstrapView{}
	for _, d := range s.drafts {
		s.expire(d)
		out = append(out, s.view(d))
	}
	return out
}

// Cancel wipes the credentials and ends the draft. A running bootstrap
// cannot be cancelled; wait until it pauses or finishes.
func (s *BootstrapService) Cancel(id string) (BootstrapView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.get(id)
	if err != nil {
		return BootstrapView{}, err
	}
	if d.status == StatusRunning {
		return BootstrapView{}, ErrBootstrapState
	}
	s.wipe(d)
	d.status = StatusCancelled
	return s.view(d), nil
}

var _ = errors.New
