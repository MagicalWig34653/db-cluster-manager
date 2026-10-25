// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/ai"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
)

// Service errors.
var (
	ErrNotFound         = errors.New("vuln: not found")
	ErrNotLeader        = errors.New("vuln: not the leader")
	ErrApprovalMismatch = errors.New("vuln: approval does not match the current plan")
	ErrInvalidInput     = errors.New("vuln: invalid input")
	ErrClosed           = errors.New("vuln: the proposal is no longer open")
)

// Executor carries out an approved plan. ImageBuildExecutor is the only
// implementation; without an Executor approval is recorded and nothing is
// started.
type Executor interface {
	Execute(ctx context.Context, actor string, p Proposal) (string, error)
}

// ImageBuildExecutor starts the plan's image build through the image build
// service, which re-verifies the build plan digest itself. Rollout to
// services is not implemented.
type ImageBuildExecutor struct{ Builds *imagebuild.Service }

// Execute implements Executor.
func (x ImageBuildExecutor) Execute(ctx context.Context, actor string, p Proposal) (string, error) {
	if p.Plan.Build == nil {
		return "manual plan: nothing started", nil
	}
	req := *p.Plan.Build
	req.ApprovalDigest = p.Plan.BuildPlanDigest
	req.ExecutionID = p.ExecutionID
	job, err := x.Builds.Start(ctx, actor, req)
	if err != nil {
		return "", err
	}
	return "image build " + job.ID + " (" + string(job.Status) + "); rollout not automated", nil
}

// Service runs the vulnerability response.
type Service struct {
	Store     *Store
	Inventory InventorySource
	Feeds     []Feed
	// IsLeader gates polling and every write; nil means always leader
	// (tests and single replica).
	IsLeader func() bool
	// Secrets holds provider API keys; AIDeps are the provider transports.
	Secrets ai.SecretStore
	AIDeps  ai.Deps
	// AllowCLIProviders is set only by explicit manager configuration.
	AllowCLIProviders bool
	// OpenProvider overrides ai.Open (tests inject fakes).
	OpenProvider func(ctx context.Context, c ai.ProviderConfig) (ai.Provider, error)
	Budget       *ai.Budget
	// RedactNames returns environment identifiers to redact from prompts
	// (hostnames, registry hosts, Swarm names).
	RedactNames func() []string
	Executor    Executor
	Notifier    Notifier
	Catalog     *imagebuild.Catalog
	Now         func() time.Time
	// StaleAfter marks a feed stale when its last success is older
	// (default 24 h).
	StaleAfter time.Duration
	// BaseBackoff is the first retry delay after a feed failure (default
	// 5 min, doubling up to 6 h).
	BaseBackoff time.Duration

	mu sync.Mutex

	seqMu     sync.Mutex
	seq       uint64
	seqLoaded bool
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) leader() error {
	if s.IsLeader != nil && !s.IsLeader() {
		return ErrNotLeader
	}
	return nil
}

func (s *Service) audit(r AuditRecord) error {
	r.ID, r.At, r.Seq = newID("va-"), s.now(), s.nextSeq()
	if r.Result == "" {
		r.Result = "ok"
	}
	return s.Store.AppendAudit(r)
}

// nextSeq returns the next audit sequence number. It continues after the
// highest sequence in the store (read once per leadership by ResetSeq or on
// first use), so the order survives restarts and leader changes.
func (s *Service) nextSeq() uint64 {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	if !s.seqLoaded {
		s.seq, s.seqLoaded = s.Store.MaxAuditSeq(), true
	}
	s.seq++
	return s.seq
}

// ResetSeq makes the next audit write re-read the stored sequence (call it
// when this replica becomes leader).
func (s *Service) ResetSeq() {
	s.seqMu.Lock()
	s.seqLoaded = false
	s.seqMu.Unlock()
}

func (s *Service) notify(ctx context.Context, n Notification) {
	if s.Notifier != nil {
		_ = s.Notifier.Notify(ctx, n)
	}
}

// PollReport summarises one poll.
type PollReport struct {
	Feeds       []FeedState `json:"feeds"`
	Advisories  int         `json:"advisories"`
	Findings    int         `json:"findings"`
	NewFindings []string    `json:"newFindings"`
	Resolved    []string    `json:"resolved"`
	// Unknown lists inventory entries whose components are not known: they
	// are not covered, which is not the same as safe.
	Unknown []string `json:"unknown"`
}

func errClass(err error) string {
	var rl *RateLimitError
	switch {
	case errors.As(err, &rl):
		return "rate_limited"
	case errors.Is(err, ErrFeedTooBig):
		return "too_large"
	case errors.Is(err, ErrFeedFormat):
		return "malformed"
	case errors.Is(err, ErrFeedStatus):
		return "http_status"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "transport"
}

// Poll fetches every due feed, stores advisories, matches them against
// the inventory and reconciles findings. A failing feed is backed off and
// marked; the other feeds still run. Leader only.
func (s *Service) Poll(ctx context.Context, actor string) (PollReport, error) {
	if err := s.leader(); err != nil {
		return PollReport{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	inv, err := s.Inventory.Inventory(ctx)
	if err != nil {
		return PollReport{}, err
	}
	var comps []Component
	rep := PollReport{NewFindings: []string{}, Resolved: []string{}, Unknown: []string{}}
	for _, e := range inv {
		if err := e.Validate(); err != nil {
			return PollReport{}, err
		}
		c := e.components()
		if len(c) == 0 {
			rep.Unknown = append(rep.Unknown, e.ID)
		}
		comps = append(comps, c...)
	}
	for _, f := range s.Feeds {
		st, err := s.Store.FeedState(f.Name())
		if err != nil {
			return PollReport{}, err
		}
		if !st.NextAttempt.IsZero() && now.Before(st.NextAttempt) {
			rep.Feeds = append(rep.Feeds, s.withStale(st))
			continue
		}
		st.LastAttempt = now
		res, ferr := f.Fetch(ctx, st, comps)
		if ferr != nil {
			st.Failures++
			st.LastError = errClass(ferr)
			st.NextAttempt = now.Add(s.backoff(st.Failures, ferr))
		} else {
			st = res.State
			st.Name, st.LastAttempt, st.LastSuccess, st.LastError, st.Failures, st.NextAttempt = f.Name(), now, now, "", 0, time.Time{}
			if !res.NotModified {
				n, err := s.Store.UpsertAdvisories(res.Advisories)
				if err != nil {
					return PollReport{}, err
				}
				st.Advisories = n
				if res.KEV != nil {
					if err := s.Store.PutKEV(res.KEV); err != nil {
						return PollReport{}, err
					}
				}
			}
		}
		if err := s.Store.PutFeedState(st); err != nil {
			return PollReport{}, err
		}
		rep.Feeds = append(rep.Feeds, s.withStale(st))
	}
	advs, err := s.Store.Advisories()
	if err != nil {
		return PollReport{}, err
	}
	kev, err := s.Store.KEV()
	if err != nil {
		return PollReport{}, err
	}
	for i := range advs {
		if kev[advs[i].ID] || slices.ContainsFunc(advs[i].Aliases, func(a string) bool { return kev[a] }) {
			advs[i].KEV = true
		}
	}
	rep.Advisories = len(advs)
	fresh := Match(advs, inv, now)
	stored, err := s.Store.Findings()
	if err != nil {
		return PollReport{}, err
	}
	changed, created := Reconcile(stored, fresh, now)
	if err := s.Store.PutFindings(changed); err != nil {
		return PollReport{}, err
	}
	byID := map[string]Finding{}
	for _, f := range changed {
		byID[f.ID] = f
		if f.Status == StatusFixed && f.StatusBy == "" && f.ResolvedAt.Equal(now) {
			rep.Resolved = append(rep.Resolved, f.ID)
			_ = s.audit(AuditRecord{Actor: actor, Event: EventFindingResolved, FindingID: f.ID, Detail: "no longer matched"})
		}
	}
	for _, id := range created {
		f := byID[id]
		rep.NewFindings = append(rep.NewFindings, id)
		_ = s.audit(AuditRecord{Actor: actor, Event: EventFindingCreated, FindingID: id,
			Detail: fmt.Sprintf("%s in %s %s (%s)", f.AdvisoryID, f.Component.Name, f.Component.Version, f.Priority)})
		s.notify(ctx, Notification{Kind: EventFindingCreated, FindingID: id, Severity: f.Severity, Priority: f.Priority,
			Title: fmt.Sprintf("%s affects %s %s in %s", f.AdvisoryID, f.Component.Name, f.Component.Version, f.Image)})
	}
	s.checkMitigations(ctx, now)
	all, err := s.Store.Findings()
	if err != nil {
		return PollReport{}, err
	}
	for _, f := range all {
		if f.Status.Open() {
			rep.Findings++
		}
	}
	sort.Strings(rep.Unknown)
	if err := s.audit(AuditRecord{Actor: actor, Event: EventPoll,
		Detail: fmt.Sprintf("%d feeds, %d advisories, %d new findings, %d resolved", len(rep.Feeds), rep.Advisories, len(rep.NewFindings), len(rep.Resolved))}); err != nil {
		return PollReport{}, err
	}
	return rep, nil
}

// checkMitigations re-alerts approved mitigations that expire within 7
// days.
func (s *Service) checkMitigations(ctx context.Context, now time.Time) {
	ps, err := s.Store.Proposals()
	if err != nil {
		return
	}
	for _, p := range ps {
		if p.Kind != KindMitigation || p.Status != StatusApproved || p.Mitigation == nil {
			continue
		}
		if left := p.Mitigation.ExpiresAt.Sub(now); left < 7*24*time.Hour {
			s.notify(ctx, Notification{Kind: EventMitigationExpiring, FindingID: p.FindingID, ProposalID: p.ID,
				Title: fmt.Sprintf("mitigation %s=%s expires at %s", p.Mitigation.Setting, p.Mitigation.Value, p.Mitigation.ExpiresAt.Format(time.RFC3339))})
		}
	}
}

func (s *Service) backoff(failures int, err error) time.Duration {
	var rl *RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return min(rl.RetryAfter, 24*time.Hour)
	}
	base := s.BaseBackoff
	if base <= 0 {
		base = 5 * time.Minute
	}
	d := base
	for i := 1; i < failures && d < 6*time.Hour; i++ {
		d *= 2
	}
	return min(d, 6*time.Hour)
}

func (s *Service) withStale(st FeedState) FeedState {
	after := s.StaleAfter
	if after <= 0 {
		after = 24 * time.Hour
	}
	st.Stale = st.LastSuccess.IsZero() || s.now().Sub(st.LastSuccess) > after
	return st
}

// FeedStates returns the polling state of the configured feeds with the
// stale flag computed now.
func (s *Service) FeedStates() ([]FeedState, error) {
	out := make([]FeedState, 0, len(s.Feeds))
	for _, f := range s.Feeds {
		st, err := s.Store.FeedState(f.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, s.withStale(st))
	}
	return out, nil
}

// Findings returns the findings, highest priority first.
func (s *Service) Findings() ([]Finding, error) {
	fs, err := s.Store.Findings()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Score != fs[j].Score {
			return fs[i].Score > fs[j].Score
		}
		return fs[i].ID < fs[j].ID
	})
	return fs, nil
}

// Finding returns one finding.
func (s *Service) Finding(id string) (Finding, error) {
	f, _, ok, err := s.Store.Finding(id)
	if err != nil {
		return Finding{}, err
	}
	if !ok {
		return Finding{}, ErrNotFound
	}
	return f, nil
}

// SetFindingStatus records an admin's VEX decision.
func (s *Service) SetFindingStatus(_ context.Context, actor, id string, st Status, justification string) (Finding, error) {
	if err := s.leader(); err != nil {
		return Finding{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ver, ok, err := s.Store.Finding(id)
	if err != nil {
		return Finding{}, err
	}
	if !ok {
		return Finding{}, ErrNotFound
	}
	if err := f.SetStatus(st, justification, actor); err != nil {
		return Finding{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if st == StatusFixed {
		f.ResolvedAt = s.now()
	} else {
		f.ResolvedAt = time.Time{}
	}
	if err := s.Store.PutFinding(f, ver); err != nil {
		return Finding{}, err
	}
	return f, s.audit(AuditRecord{Actor: actor, Event: EventFindingStatus, FindingID: id, Detail: string(st)})
}

// ProposeOptions selects the proposer.
type ProposeOptions struct {
	// UseAI asks the policy's provider and model for an analysis; without
	// it the deterministic proposer runs.
	UseAI bool `json:"useAi"`
}

func (s *Service) context(ctx context.Context, findingID string) (Finding, Advisory, InventoryEntry, error) {
	f, _, ok, err := s.Store.Finding(findingID)
	if err != nil {
		return Finding{}, Advisory{}, InventoryEntry{}, err
	}
	if !ok {
		return Finding{}, Advisory{}, InventoryEntry{}, ErrNotFound
	}
	advs, err := s.Store.Advisories()
	if err != nil {
		return Finding{}, Advisory{}, InventoryEntry{}, err
	}
	var adv Advisory
	found := false
	for _, a := range advs {
		if a.ID == f.AdvisoryID {
			adv, found = a, true
			break
		}
	}
	if !found {
		return Finding{}, Advisory{}, InventoryEntry{}, fmt.Errorf("%w: advisory of the finding", ErrNotFound)
	}
	inv, err := s.Inventory.Inventory(ctx)
	if err != nil {
		return Finding{}, Advisory{}, InventoryEntry{}, err
	}
	for _, e := range inv {
		if e.ID == f.EntryID {
			return f, adv, e, nil
		}
	}
	return Finding{}, Advisory{}, InventoryEntry{}, fmt.Errorf("%w: the inventory entry of the finding is gone", ErrNotFound)
}

// Propose creates a remediation proposal for an open finding. It always
// returns an approvable plan or a proposal marked invalid with the
// validator's reasons; it never changes anything. Under auto_for_policy a
// proposal that passes every rule is approved and executed at once.
func (s *Service) Propose(ctx context.Context, actor, findingID string, o ProposeOptions) (Proposal, error) {
	if err := s.leader(); err != nil {
		return Proposal{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, adv, e, err := s.context(ctx, findingID)
	if err != nil {
		return Proposal{}, err
	}
	if !f.Status.Open() {
		return Proposal{}, fmt.Errorf("%w: the finding is %s", ErrInvalidInput, f.Status)
	}
	pol, _, err := s.Store.Policy()
	if err != nil {
		return Proposal{}, err
	}
	now := s.now()
	p := Proposal{ID: newID("vp-"), FindingID: f.ID, AdvisoryID: f.AdvisoryID, EntryID: e.ID, CreatedBy: actor, CreatedAt: now}
	var c change
	if o.UseAI {
		c, err = s.analyse(ctx, actor, &p, pol, f, adv, e, now)
		if err != nil {
			return Proposal{}, err
		}
	} else {
		var why string
		c, why = deterministicChange(f)
		p.Origin, p.Rationale = OriginDeterministic, why
	}
	p.Kind, p.Component, p.TargetVersion, p.Mitigation = c.Kind, c.Component, c.TargetVersion, c.Mitigation
	if p.Validation.Errors == nil {
		p.Validation.Errors = validateChange(c, f, adv, e)
	}
	if len(p.Validation.Errors) == 0 {
		plan, perr := buildPlan(c, f, e, s.Catalog)
		if perr != nil {
			p.Validation.Errors = []string{perr.Error()}
		} else {
			p.Plan = plan
		}
	}
	p.Validation.OK = len(p.Validation.Errors) == 0
	if !p.Validation.OK {
		p.Status, p.Plan = StatusInvalid, Plan{Steps: []imagebuild.Step{}, Warnings: []string{}}
		p.Decision = Decision{Mode: pol.Mode, Action: ActionNotify, Reasons: []string{"rejected by the validator"}}
		if err := s.Store.PutProposal(p, 0); err != nil {
			return Proposal{}, err
		}
		_ = s.audit(AuditRecord{Actor: actor, Event: EventProposalInvalid, FindingID: f.ID, ProposalID: p.ID, Provider: p.Provider,
			Model: p.Model, PromptSHA: p.PromptSHA256, ResponseSHA: p.ResponseSHA256, Result: "rejected", Detail: strings.Join(p.Validation.Errors, "; ")})
		return p, nil
	}
	p.Rating = rate(c, f, p.Plan)
	autoToday, err := s.autoApprovalsToday(now)
	if err != nil {
		return Proposal{}, err
	}
	p.Decision = pol.Decide(p, f, autoToday)
	switch p.Decision.Action {
	case ActionNotify:
		p.Status = StatusProposed
	default:
		p.Status = StatusAwaitingApproval
	}
	if err := s.Store.PutProposal(p, 0); err != nil {
		return Proposal{}, err
	}
	if err := s.audit(AuditRecord{Actor: actor, Event: EventProposalCreated, FindingID: f.ID, ProposalID: p.ID, Provider: p.Provider,
		Model: p.Model, PromptSHA: p.PromptSHA256, ResponseSHA: p.ResponseSHA256, PlanDigest: p.Plan.Digest,
		Detail: fmt.Sprintf("%s %s, decision %s", p.Origin, p.Kind, p.Decision.Action)}); err != nil {
		return Proposal{}, err
	}
	s.notify(ctx, Notification{Kind: EventProposalCreated, FindingID: f.ID, ProposalID: p.ID, Severity: f.Severity, Priority: f.Priority,
		Title: fmt.Sprintf("%s proposal for %s: %s", p.Kind, f.AdvisoryID, p.Decision.Action)})
	if p.Decision.Action == ActionAutoApprove {
		return s.approveLocked(ctx, "policy:auto_for_policy", p.ID, p.Plan.Digest, true)
	}
	return p, nil
}

// analyse runs the AI analysis and converts its answer into a change. A
// malformed answer marks the proposal invalid (Validation.Errors); provider
// errors are returned.
func (s *Service) analyse(ctx context.Context, actor string, p *Proposal, pol Policy, f Finding, adv Advisory, e InventoryEntry, now time.Time) (change, error) {
	if pol.Provider == "" {
		return change{}, fmt.Errorf("%w: the policy selects no AI provider", ErrInvalidInput)
	}
	cfg, _, ok, err := s.Store.ProviderConfig(pol.Provider)
	if err != nil {
		return change{}, err
	}
	if !ok {
		return change{}, fmt.Errorf("%w: provider %s", ErrNotFound, pol.Provider)
	}
	if !cfg.ModelAllowed(pol.Model) {
		return change{}, ai.ErrUnknownModel
	}
	var prov ai.Provider
	if cfg.Consent == nil {
		return change{}, ai.ErrNoConsent
	}
	prov, err = s.openProvider(ctx, cfg)
	if err != nil {
		return change{}, err
	}
	in := s.promptInput(f, adv, e)
	var names []string
	if s.RedactNames != nil {
		names = s.RedactNames()
	}
	prompt, err := ai.BuildPrompt(in, ai.Redactor{Names: names})
	if err != nil {
		return change{}, err
	}
	if s.Budget != nil {
		if err := s.Budget.Reserve(cfg.ID, cfg.Limits); err != nil {
			_ = s.audit(AuditRecord{Actor: actor, Event: EventAnalysis, FindingID: f.ID, Provider: cfg.ID, Model: pol.Model, Result: "budget_exceeded"})
			return change{}, err
		}
	}
	req := ai.Request{Model: pol.Model, System: prompt.System, User: prompt.User, MaxTokens: cfg.OutputTokens()}
	resp, err := prov.Complete(ctx, req)
	if err != nil {
		_ = s.audit(AuditRecord{Actor: actor, Event: EventAnalysis, FindingID: f.ID, Provider: cfg.ID, Model: pol.Model, PromptSHA: prompt.SHA256, Result: "provider_error"})
		return change{}, err
	}
	if s.Budget != nil {
		tokens, err := ai.UsageTokens(resp, req)
		if err != nil {
			return change{}, err
		}
		if err := s.Budget.Record(cfg.ID, tokens); err != nil {
			return change{}, err
		}
	}
	p.Origin, p.Provider, p.Model, p.PromptSHA256, p.ResponseSHA256 = OriginAI, cfg.ID, pol.Model, prompt.SHA256, ai.HashText(resp.Text)
	_ = s.audit(AuditRecord{Actor: actor, Event: EventAnalysis, FindingID: f.ID, Provider: cfg.ID, Model: pol.Model,
		PromptSHA: prompt.SHA256, ResponseSHA: p.ResponseSHA256, Detail: fmt.Sprintf("%d redactions, %d+%d tokens", prompt.Redactions, resp.InputTokens, resp.OutputTokens)})
	parsed, perr := ai.ParseProposal(resp.Text)
	if perr != nil {
		p.Validation.Errors = []string{perr.Error()}
		return change{Kind: KindNoOp, Component: f.Component}, nil
	}
	p.Rationale = parsed.Rationale
	p.AIAssessment = &AIAssessment{Affected: parsed.Affected, Benefit: parsed.Benefit, Risk: parsed.Risk}
	c := aiChange(parsed, f, pol.mitigationDays(), now)
	// An AI-proposed version must be one the advisory states as fixed.
	if c.Kind == KindPatchedRebuild && !slices.Contains(f.FixedVersions, c.TargetVersion) {
		p.Validation.Errors = append([]string{"the AI target version is not a fixed version stated by the advisory"}, validateChange(c, f, adv, e)...)
	}
	return c, nil
}

func (s *Service) openProvider(ctx context.Context, cfg ai.ProviderConfig) (ai.Provider, error) {
	if cfg.Kind.CLI() && !s.AllowCLIProviders {
		return nil, fmt.Errorf("%w: CLI provider requires manager.vuln.allowCLIProviders=true", ErrInvalidInput)
	}
	if s.OpenProvider != nil {
		return s.OpenProvider(ctx, cfg)
	}
	return ai.Open(ctx, cfg, s.Secrets, s.AIDeps)
}

func (s *Service) promptInput(f Finding, adv Advisory, e InventoryEntry) ai.PromptInput {
	name, tag := splitImageRef(e.Image)
	img := name
	if tag != "" {
		img += ":" + tag
	}
	in := ai.PromptInput{
		AdvisoryID: adv.ID, Aliases: adv.Aliases, Severity: string(adv.Severity), Summary: adv.Summary, Details: adv.Details,
		Ecosystem: f.Component.Ecosystem, Package: f.Component.Name, Version: f.Component.Version, FixedVersions: f.FixedVersions,
		Image: img, ServiceKind: string(e.ServiceKind), AllowedSettings: []string{},
	}
	if e.Build != nil && e.Build.Spec.PHP.Version != "" {
		in.AllowedSettings = MitigationSettings()
		if len(e.Build.Spec.PHP.INI) > 0 {
			in.CurrentSettings = map[string]string{}
			for k, v := range e.Build.Spec.PHP.INI {
				in.CurrentSettings[k] = v
			}
		}
	}
	return in
}

func (s *Service) autoApprovalsToday(now time.Time) (int, error) {
	recs, err := s.Store.AuditRecords()
	if err != nil {
		return 0, err
	}
	day := now.Format("2006-01-02")
	n := 0
	for _, r := range recs {
		if r.Event == EventAutoApproved && r.At.UTC().Format("2006-01-02") == day {
			n++
		}
	}
	return n, nil
}

// Proposals returns all proposals, newest first.
func (s *Service) Proposals() ([]Proposal, error) {
	ps, err := s.Store.Proposals()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].CreatedAt.After(ps[j].CreatedAt) })
	return ps, nil
}

// Approve verifies the digest against the plan recomputed from the current
// finding and inventory and hands the plan to the Executor. notify_only
// refuses approval.
func (s *Service) Approve(ctx context.Context, actor, id, digest string) (Proposal, error) {
	if err := s.leader(); err != nil {
		return Proposal{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.approveLocked(ctx, actor, id, digest, false)
}

func (s *Service) approveLocked(ctx context.Context, actor, id, digest string, auto bool) (Proposal, error) {
	p, ver, ok, err := s.Store.Proposal(id)
	if err != nil {
		return Proposal{}, err
	}
	if !ok {
		return Proposal{}, ErrNotFound
	}
	pol, _, err := s.Store.Policy()
	if err != nil {
		return Proposal{}, err
	}
	if !auto && pol.Mode == ModeNotifyOnly {
		return Proposal{}, fmt.Errorf("%w: notify_only", ErrPolicyForbids)
	}
	if (p.Status == StatusApproved || p.Status == StatusFailed) && p.ExecutionID != "" && digest == p.Plan.Digest {
		if p.Execution != "" && p.Status == StatusApproved {
			return p, nil
		}
		return s.executeIntent(ctx, actor, p, ver, EventApproved)
	}
	if p.Status != StatusAwaitingApproval && p.Status != StatusProposed {
		return Proposal{}, ErrClosed
	}
	if !p.Validation.OK {
		return Proposal{}, ErrClosed
	}
	f, _, e, err := s.context(ctx, p.FindingID)
	if err != nil {
		return Proposal{}, err
	}
	plan, err := buildPlan(change{Kind: p.Kind, Component: p.Component, TargetVersion: p.TargetVersion, Mitigation: p.Mitigation}, f, e, s.Catalog)
	if err != nil || digest == "" || digest != p.Plan.Digest || plan.Digest != p.Plan.Digest {
		_ = s.audit(AuditRecord{Actor: actor, Event: EventApproved, FindingID: p.FindingID, ProposalID: p.ID, PlanDigest: digest, Result: "digest_mismatch"})
		return Proposal{}, ErrApprovalMismatch
	}
	ev := EventApproved
	if auto {
		ev = EventAutoApproved
	}
	p.Status, p.DecidedBy, p.DecidedAt = StatusApproved, actor, s.now()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Proposal{}, err
	}
	p.ExecutionID = "vx-" + hex.EncodeToString(nonce[:])
	if err := s.audit(AuditRecord{Actor: actor, Event: ev, FindingID: p.FindingID, ProposalID: p.ID, PlanDigest: p.Plan.Digest}); err != nil {
		return Proposal{}, err
	}
	if err := s.Store.PutProposal(p, ver); err != nil {
		return Proposal{}, err
	}
	_, ver, _, err = s.Store.Proposal(id)
	if err != nil {
		return Proposal{}, err
	}
	return s.executeIntent(ctx, actor, p, ver, ev)
}

// executeIntent reconciles the durable execution ID. The build service
// atomically claims that ID before calling the external builder.
func (s *Service) executeIntent(ctx context.Context, actor string, p Proposal, ver uint64, ev string) (Proposal, error) {
	if s.Executor == nil {
		p.Execution = "no executor is wired: approval recorded, nothing started"
	} else {
		ref, xerr := s.Executor.Execute(ctx, actor, p)
		if xerr != nil {
			p.Status, p.Execution = StatusFailed, "execution failed"
			_ = s.audit(AuditRecord{Actor: actor, Event: EventExecution, FindingID: p.FindingID, ProposalID: p.ID, PlanDigest: p.Plan.Digest, Result: "failed"})
		} else {
			p.Execution = ref
			_ = s.audit(AuditRecord{Actor: actor, Event: EventExecution, FindingID: p.FindingID, ProposalID: p.ID, PlanDigest: p.Plan.Digest, Detail: ref})
		}
	}
	if err := s.Store.PutProposal(p, ver); err != nil {
		return Proposal{}, err
	}
	s.notify(ctx, Notification{Kind: ev, FindingID: p.FindingID, ProposalID: p.ID, Title: fmt.Sprintf("proposal %s %s: %s", p.ID, p.Status, p.Execution)})
	return p, nil
}

// Decline closes a proposal without change.
func (s *Service) Decline(_ context.Context, actor, id, reason string) (Proposal, error) {
	if err := s.leader(); err != nil {
		return Proposal{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ver, ok, err := s.Store.Proposal(id)
	if err != nil {
		return Proposal{}, err
	}
	if !ok {
		return Proposal{}, ErrNotFound
	}
	if p.Status != StatusAwaitingApproval && p.Status != StatusProposed {
		return Proposal{}, ErrClosed
	}
	if len(reason) > 1024 {
		return Proposal{}, ErrInvalidInput
	}
	p.Status, p.DecidedBy, p.DecidedAt = StatusDeclined, actor, s.now()
	if err := s.Store.PutProposal(p, ver); err != nil {
		return Proposal{}, err
	}
	return p, s.audit(AuditRecord{Actor: actor, Event: EventDeclined, FindingID: p.FindingID, ProposalID: p.ID, Detail: reason})
}

// Policy returns the current policy and its version.
func (s *Service) Policy() (Policy, uint64, error) { return s.Store.Policy() }

// SetPolicy replaces the policy. ifVersion must be the version read (0
// when none is stored yet). A selected provider must exist and allow the
// model.
func (s *Service) SetPolicy(_ context.Context, actor string, p Policy, ifVersion uint64) (Policy, error) {
	if err := s.leader(); err != nil {
		return Policy{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Mode == ModeAutoForPolicy && p.AutoOptInBy == "" {
		p.AutoOptInBy = actor
	}
	if p.Mode != ModeAutoForPolicy {
		p.AutoOptInBy = ""
	}
	if err := p.Validate(); err != nil {
		return Policy{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if p.Provider != "" {
		cfg, _, ok, err := s.Store.ProviderConfig(p.Provider)
		if err != nil {
			return Policy{}, err
		}
		if !ok || !cfg.ModelAllowed(p.Model) {
			return Policy{}, fmt.Errorf("%w: provider or model not configured", ErrInvalidInput)
		}
	}
	p.UpdatedBy, p.UpdatedAt = actor, s.now()
	if err := s.Store.PutPolicy(p, ifVersion); err != nil {
		return Policy{}, err
	}
	return p, s.audit(AuditRecord{Actor: actor, Event: EventPolicyChanged,
		Detail: fmt.Sprintf("mode %s, provider %q, model %q, kill switch %t", p.Mode, p.Provider, p.Model, p.KillSwitch)})
}

func (s *Service) view(c ai.ProviderConfig) ProviderView {
	v := ProviderView{ProviderConfig: c, CredentialSet: c.CredentialRef != "", Local: c.Local(), Egress: c.EgressStatement()}
	v.CredentialRef = ""
	if s.Budget != nil {
		v.Usage = s.Budget.Usage(c.ID)
	}
	return v
}

// Providers returns the provider configurations (no secrets).
func (s *Service) Providers() ([]ProviderView, error) {
	cs, err := s.Store.ProviderConfigs()
	if err != nil {
		return nil, err
	}
	out := make([]ProviderView, 0, len(cs))
	for _, c := range cs {
		out = append(out, s.view(c))
	}
	return out, nil
}

// AddProvider stores a provider configuration and its API key (sealed by
// the SecretStore). The provider stays unusable until consent.
func (s *Service) AddProvider(ctx context.Context, actor string, c ai.ProviderConfig, key ai.Secret) (ProviderView, error) {
	if err := s.leader(); err != nil {
		return ProviderView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Kind.CLI() && !s.AllowCLIProviders {
		return ProviderView{}, fmt.Errorf("%w: CLI provider requires manager.vuln.allowCLIProviders=true", ErrInvalidInput)
	}
	c.Consent, c.CreatedBy, c.CreatedAt, c.CredentialRef = nil, actor, s.now(), ""
	if !key.IsZero() {
		if c.Kind.CLI() {
			return ProviderView{}, fmt.Errorf("%w: CLI logins take no API key", ErrInvalidInput)
		}
		c.CredentialRef = "vuln/ai/" + c.ID + "/key"
	}
	if err := c.Validate(); err != nil {
		return ProviderView{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if _, _, ok, err := s.Store.ProviderConfig(c.ID); err != nil {
		return ProviderView{}, err
	} else if ok {
		return ProviderView{}, ErrConflict
	}
	if c.CredentialRef != "" {
		if s.Secrets == nil {
			return ProviderView{}, fmt.Errorf("%w: no secret store", ErrInvalidInput)
		}
		if err := s.Secrets.Put(ctx, c.CredentialRef, key); err != nil {
			return ProviderView{}, err
		}
	}
	if err := s.Store.PutProviderConfig(c, 0); err != nil {
		if c.CredentialRef != "" {
			_ = s.Secrets.Delete(ctx, c.CredentialRef)
		}
		return ProviderView{}, err
	}
	return s.view(c), s.audit(AuditRecord{Actor: actor, Event: EventProviderAdded, Provider: c.ID, Detail: string(c.Kind)})
}

// SetConsent grants (statement must equal the provider's current egress
// statement, so the admin consented to what is actually sent) or revokes
// data-egress consent.
func (s *Service) SetConsent(_ context.Context, actor, id string, grant bool, statement string) (ProviderView, error) {
	if err := s.leader(); err != nil {
		return ProviderView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ver, ok, err := s.Store.ProviderConfig(id)
	if err != nil {
		return ProviderView{}, err
	}
	if !ok {
		return ProviderView{}, ErrNotFound
	}
	if grant {
		if statement != c.EgressStatement() {
			return ProviderView{}, ErrApprovalMismatch
		}
		c.Consent = &ai.Consent{By: actor, At: s.now(), Statement: statement}
	} else {
		c.Consent = nil
	}
	if err := s.Store.PutProviderConfig(c, ver); err != nil {
		return ProviderView{}, err
	}
	res := "granted"
	if !grant {
		res = "revoked"
	}
	return s.view(c), s.audit(AuditRecord{Actor: actor, Event: EventProviderConsent, Provider: id, Result: res})
}

// Audit returns the audit trail.
func (s *Service) Audit() ([]AuditRecord, error) { return s.Store.AuditRecords() }
