// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// AutoActor is the actor recorded for work the service does under an
// automatic policy.
const AutoActor = "system:image-update"

// Scopes of a service in this feature.
const (
	ScopeOwned = "owned"
	ScopeOptIn = "opt-in"
)

// Service is the image update service. Writes must happen on the manager
// leader; Term is the fencing term of the leadership.
type Service struct {
	KV       vuln.KV
	Journal  *opjournal.Journal
	Source   Source
	Driver   Driver
	Resolver Resolver
	// Installation is the installation ID whose ownership label a service
	// must carry. Empty: no service is in scope.
	Installation string
	// InstallationFunc, when set, supplies the installation ID on each use
	// (the settings may appear after the service started); an error or an
	// empty ID leaves every service out of scope.
	InstallationFunc func() (string, error)
	// Term returns the current leader term; an error means not leader.
	Term   func() (uint64, error)
	Now    func() time.Time
	Notify func(context.Context, notify.Event)
	// Sleep waits between status polls (default time based).
	Sleep func(context.Context, time.Duration) error
	// PollEvery is the status poll interval (default 5 s).
	PollEvery time.Duration
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) pollEvery() time.Duration {
	if s.PollEvery > 0 {
		return s.PollEvery
	}
	return 5 * time.Second
}

func (s *Service) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func (s *Service) emit(ctx context.Context, sev notify.Severity, kind, resource, msg string, details map[string]string) {
	if s.Notify == nil {
		return
	}
	s.Notify(ctx, notify.Event{ID: newID("evt"), Time: s.now(), Severity: sev, Kind: kind, Resource: resource, Message: msg, Details: details})
}

func (s *Service) get(coll, key string, out any) (uint64, bool, error) {
	d, ok, err := s.KV.Get(coll, key)
	if err != nil || !ok {
		return 0, false, err
	}
	return d.Version, true, json.Unmarshal(d.Value, out)
}

func (s *Service) put(coll, key string, v any, ifVersion *uint64) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	err = s.KV.Txn(vuln.KVPut{Collection: coll, Key: key, Value: b, IfVersion: ifVersion})
	if errors.Is(err, vuln.ErrConflict) {
		return ErrConflict
	}
	return err
}

func list[T any](s *Service, coll string) ([]T, error) {
	docs, err := s.KV.List(coll)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(docs))
	for _, d := range docs {
		var v T
		if err := json.Unmarshal(d.Value, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// --- scope ---

// scope decides whether a service is in scope. Fail closed: an unknown
// installation, a missing or foreign ownership label, or a foreign service
// without the explicit opt-in is out of scope.
func (s *Service) scope(ms ManagedService) (string, bool) {
	if ms.Foreign {
		if ms.Labels[LabelImageUpdate] == OptInValue {
			return ScopeOptIn, true
		}
		return "", false
	}
	inst := s.Installation
	if s.InstallationFunc != nil {
		inst, _ = s.InstallationFunc()
	}
	if inst != "" && ms.Labels[LabelInstallation] == inst {
		return ScopeOwned, true
	}
	return "", false
}

func (s *Service) lookup(ctx context.Context, id string) (ManagedService, string, error) {
	all, err := s.Source.Services(ctx)
	if err != nil {
		return ManagedService{}, "", err
	}
	for _, ms := range all {
		if ms.ID != id {
			continue
		}
		sc, ok := s.scope(ms)
		if !ok {
			return ManagedService{}, "", ErrNotOwned
		}
		return ms, sc, nil
	}
	return ManagedService{}, "", fmt.Errorf("%w: service %q", ErrNotFound, id)
}

// --- policies and state ---

// Policy returns the policy of a service (manual when none is stored).
func (s *Service) Policy(serviceID string) (Policy, error) {
	var p Policy
	if _, _, err := s.get(CollPolicies, serviceID, &p); err != nil {
		return Policy{}, err
	}
	p.ServiceID = serviceID
	return p.normalized(), nil
}

// SetPolicy validates and stores the policy of a service in scope.
func (s *Service) SetPolicy(ctx context.Context, actor string, p Policy) (Policy, error) {
	ms, _, err := s.lookup(ctx, p.ServiceID)
	if err != nil {
		return Policy{}, err
	}
	if err := p.Validate(ms); err != nil {
		return Policy{}, err
	}
	p = p.normalized()
	if err := s.put(CollPolicies, p.ServiceID, p, nil); err != nil {
		return Policy{}, err
	}
	s.emit(ctx, notify.SeverityInfo, "imageupdate.policy_changed", "service/"+ms.ID,
		fmt.Sprintf("image update policy of %s set to %s/%s by %s", ms.Name, p.Mode, p.Track, actor), map[string]string{"service": ms.ID, "mode": string(p.Mode), "track": string(p.Track)})
	return p, nil
}

// DeletePolicy removes the policy (the service becomes manual).
func (s *Service) DeletePolicy(ctx context.Context, serviceID string) error {
	if _, _, err := s.lookup(ctx, serviceID); err != nil {
		return err
	}
	return s.KV.Txn(vuln.KVPut{Collection: CollPolicies, Key: serviceID, Delete: true})
}

// State returns the bookkeeping of a service.
func (s *Service) State(serviceID string) (ServiceState, error) {
	st := ServiceState{ServiceID: serviceID}
	_, _, err := s.get(CollState, serviceID, &st)
	return st, err
}

func (s *Service) putState(st ServiceState) error { return s.put(CollState, st.ServiceID, st, nil) }

// ServiceView is one service in scope with its policy and state.
type ServiceView struct {
	Service  ManagedService `json:"service"`
	Scope    string         `json:"scope"`
	Database bool           `json:"database"`
	Policy   Policy         `json:"policy"`
	State    ServiceState   `json:"state"`
	// OpenPlan is the ID of the open plan, if any.
	OpenPlan string `json:"openPlan,omitempty"`
}

// Services lists the services in scope. Services without the ownership
// labels (or foreign ones without the opt-in) are not shown at all.
func (s *Service) Services(ctx context.Context) ([]ServiceView, error) {
	all, err := s.Source.Services(ctx)
	if err != nil {
		return nil, err
	}
	plans, err := s.Plans()
	if err != nil {
		return nil, err
	}
	open := map[string]string{}
	for _, p := range plans {
		if p.State.open() || p.State == PlanRunning {
			open[p.ServiceID] = p.ID
		}
	}
	var out []ServiceView
	for _, ms := range all {
		sc, ok := s.scope(ms)
		if !ok {
			continue
		}
		pol, err := s.Policy(ms.ID)
		if err != nil {
			return nil, err
		}
		st, err := s.State(ms.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ServiceView{Service: ms, Scope: sc, Database: IsDatabase(ms.Type, repoOf(ms.Image)), Policy: pol, State: st, OpenPlan: open[ms.ID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service.ID < out[j].Service.ID })
	return out, nil
}

// --- plans ---

// Plans returns all plans, newest first.
func (s *Service) Plans() ([]Plan, error) {
	ps, err := list[Plan](s, CollPlans)
	sort.Slice(ps, func(i, j int) bool { return ps[i].CreatedAt.After(ps[j].CreatedAt) })
	return ps, err
}

// Plan returns one plan.
func (s *Service) Plan(id string) (Plan, error) {
	p, _, err := s.loadPlan(id)
	return p, err
}

func (s *Service) loadPlan(id string) (Plan, uint64, error) {
	var p Plan
	ver, ok, err := s.get(CollPlans, id, &p)
	if err != nil {
		return p, 0, err
	}
	if !ok {
		return p, 0, ErrNotFound
	}
	return p, ver, nil
}

func (s *Service) putAny(p Plan) error { return s.put(CollPlans, p.ID, p, nil) }

// CheckOptions modify a check.
type CheckOptions struct {
	// Actor is recorded as the creator of a plan (default AutoActor).
	Actor string
	// Force proposes a candidate again although an earlier plan for the
	// same target was declined, failed or rolled back.
	Force bool
}

// CheckResult is the outcome of a check.
type CheckResult struct {
	// Plan is the open plan for the best candidate, nil when there is none.
	Plan    *Plan  `json:"plan,omitempty"`
	Message string `json:"message,omitempty"`
}

// Check looks for a newer image of one service in scope. It contacts the
// registry only (read only), records the digest baseline, and creates or
// reuses a plan for the best candidate. It never touches the service.
func (s *Service) Check(ctx context.Context, serviceID string, opt CheckOptions) (CheckResult, error) {
	ms, sc, err := s.lookup(ctx, serviceID)
	if err != nil {
		return CheckResult{}, err
	}
	pol, err := s.Policy(ms.ID)
	if err != nil {
		return CheckResult{}, err
	}
	st, err := s.State(ms.ID)
	if err != nil {
		return CheckResult{}, err
	}
	res, cerr := s.check(ctx, ms, sc, pol, &st, opt)
	st.LastCheckAt = s.now()
	prev := st.LastError
	if cerr != nil {
		st.LastError = redact(cerr.Error())
	} else {
		st.LastError = ""
	}
	if err := s.putState(st); err != nil && cerr == nil {
		cerr = err
	}
	if cerr != nil {
		if st.LastError != prev {
			s.emit(ctx, notify.SeverityWarning, "imageupdate.check_failed", "service/"+ms.ID,
				"image update check of "+ms.Name+" failed: "+st.LastError, map[string]string{"service": ms.ID})
		}
		return CheckResult{}, fmt.Errorf("imageupdate: check of %s failed: %s", ms.Name, st.LastError)
	}
	return res, nil
}

func (s *Service) check(ctx context.Context, ms ManagedService, sc string, pol Policy, st *ServiceState, opt CheckOptions) (CheckResult, error) {
	cur, err := ParseRef(ms.Image)
	if err != nil {
		return CheckResult{}, err
	}
	if cur.Tag == "" {
		return CheckResult{Message: "the image is pinned by digest only; there is no tag to follow"}, nil
	}
	db := IsDatabase(ms.Type, cur.Repo)
	target, kind := cur.Tag, KindDigest
	if pol.Track != TrackDigest {
		tags, err := s.Resolver.Tags(ctx, cur)
		if err != nil {
			return CheckResult{}, err
		}
		if t, k, ok := PickTag(cur, tags, pol.Track, db); ok {
			target, kind = t, k
		}
	}
	digest, err := s.Resolver.Digest(ctx, cur.Pinned(target, ""))
	if err != nil {
		return CheckResult{}, err
	}
	if !ValidDigest(digest) {
		return CheckResult{}, errors.New("registry returned no valid digest")
	}
	currentDigest := cur.Digest
	if currentDigest == "" && st.Baseline != nil && st.Baseline.Tag == cur.Tag {
		currentDigest = st.Baseline.Digest
	}
	if kind == KindDigest {
		switch {
		case currentDigest == "":
			// Swarm resolved the tag at deploy time and the manager cannot
			// know which digest runs. Record the registry's answer as the
			// baseline; later changes of the tag are candidates.
			st.Baseline = &Baseline{Tag: cur.Tag, Digest: digest, At: s.now()}
			return CheckResult{Message: "baseline digest recorded"}, nil
		case currentDigest == digest:
			return CheckResult{Message: "up to date"}, nil
		}
	}
	actor := opt.Actor
	if actor == "" {
		actor = AutoActor
	}
	p, err := s.makePlan(ctx, actor, ms, sc, pol, cur, currentDigest, Image{Ref: cur.Pinned(target, digest).String(), Tag: target, Digest: digest}, kind, SourceRegistry, "", opt.Force)
	if err != nil {
		return CheckResult{}, err
	}
	if p == nil {
		return CheckResult{Message: "an earlier plan for this image was declined or did not succeed; use a forced check to propose it again"}, nil
	}
	return CheckResult{Plan: p}, nil
}

// makePlan creates (or reuses) the plan for a target image. It returns nil
// when an earlier plan for the same target was declined or did not succeed
// and force is not set.
func (s *Service) makePlan(ctx context.Context, actor string, ms ManagedService, sc string, pol Policy, cur Ref, curDigest string, to Image, kind Kind, source, reference string, force bool) (*Plan, error) {
	plans, err := s.Plans()
	if err != nil {
		return nil, err
	}
	for _, p := range plans {
		if p.ServiceID != ms.ID || p.To.Digest != to.Digest || p.To.Tag != to.Tag {
			continue
		}
		if p.State.open() {
			q := p
			return &q, nil
		}
		if !force && (p.State == PlanDeclined || p.State == PlanFailed || p.State == PlanRolledBack) {
			return nil, nil
		}
	}
	for _, p := range plans {
		if p.ServiceID == ms.ID && p.State.open() {
			p.State = PlanSuperseded
			if err := s.putAny(p); err != nil {
				return nil, err
			}
		}
	}
	db := IsDatabase(ms.Type, cur.Repo)
	route := RouteServiceUpdate
	switch {
	case sc == ScopeOptIn:
		route = RouteReportOnly
	case ms.Type == "galera":
		route = RoutePlanner
	}
	p := Plan{ID: newID("iu"), ServiceID: ms.ID, ServiceName: ms.Name, Swarm: ms.Swarm, ServiceType: ms.Type, Kind: kind, Source: source, Reference: reference,
		From: Image{Ref: ms.Image, Tag: cur.Tag, Digest: curDigest}, To: to, Strategy: pol.Strategy, Database: db, Route: route,
		State: PlanProposed, CreatedBy: actor, CreatedAt: s.now()}
	p.Digest = p.computeDigest()
	if pol.Mode == ModeAuto && autoEligible(p) {
		p.State, p.Auto, p.ApprovedBy, p.ApprovedAt = PlanApproved, true, AutoActor, s.now()
	}
	zero := uint64(0)
	if err := s.put(CollPlans, p.ID, p, &zero); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("%s: newer image %s available (%s, was %s)", ms.Name, to.Ref, kind, ms.Image)
	switch {
	case route == RoutePlanner:
		msg += "; this is a database cluster, use the rolling or minor-upgrade planner"
	case route == RouteReportOnly:
		msg += "; the service is not managed here, nothing is applied"
	case p.Auto:
		msg += "; the policy applies it in the next maintenance window"
	default:
		msg += "; review and approve the plan"
	}
	s.emit(ctx, notify.SeverityInfo, "imageupdate.candidate", "service/"+ms.ID, msg,
		map[string]string{"service": ms.ID, "plan": p.ID, "from": p.From.Ref, "to": to.Ref, "kind": string(kind), "route": route})
	return &p, nil
}

// autoEligible: what a policy may approve by itself. Registry candidates of
// the digest and patch kind, and minor versions of non-database services.
// Never a major version, a built image, a rollback, a database minor jump,
// a cluster that goes through its planner or a service this installation
// does not own.
func autoEligible(p Plan) bool {
	if p.Route != RouteServiceUpdate || p.Source != SourceRegistry || p.ServiceType == "galera" {
		return false
	}
	switch p.Kind {
	case KindDigest, KindPatch:
		return true
	case KindMinor:
		return !p.Database
	}
	return false
}

// Approve approves the plan with this digest. Plans that cannot run
// (cluster planner, foreign service) are refused.
func (s *Service) Approve(ctx context.Context, actor, planID, digest string) (Plan, error) {
	p, ver, err := s.loadPlan(planID)
	if err != nil {
		return Plan{}, err
	}
	if p.State != PlanProposed {
		return Plan{}, fmt.Errorf("%w: plan is %s", ErrConflict, p.State)
	}
	if digest == "" || digest != p.Digest || p.computeDigest() != p.Digest {
		return Plan{}, ErrDigest
	}
	if err := runnable(p); err != nil {
		return Plan{}, err
	}
	p.State, p.ApprovedBy, p.ApprovedAt = PlanApproved, actor, s.now()
	if err := s.put(CollPlans, p.ID, p, &ver); err != nil {
		return Plan{}, err
	}
	return p, nil
}

func runnable(p Plan) error {
	switch {
	case p.Route == RoutePlanner || p.ServiceType == "galera":
		return ErrRoutedToPlanner
	case p.Route != RouteServiceUpdate:
		return ErrForeign
	}
	return nil
}

// Decline declines an open plan.
func (s *Service) Decline(_ context.Context, _, planID string) (Plan, error) {
	p, ver, err := s.loadPlan(planID)
	if err != nil {
		return Plan{}, err
	}
	if !p.State.open() {
		return Plan{}, fmt.Errorf("%w: plan is %s", ErrConflict, p.State)
	}
	p.State = PlanDeclined
	return p, s.put(CollPlans, p.ID, p, &ver)
}

// ProposeRollback proposes the update back to the image before a succeeded
// plan. The rollback needs its own approval and uses the manager's rollback
// of the service update (previous spec).
func (s *Service) ProposeRollback(ctx context.Context, actor, planID string) (Plan, error) {
	orig, _, err := s.loadPlan(planID)
	if err != nil {
		return Plan{}, err
	}
	if orig.State != PlanSucceeded || orig.Source == SourceRollback {
		return Plan{}, fmt.Errorf("%w: only a succeeded update can be rolled back", ErrConflict)
	}
	ms, sc, err := s.lookup(ctx, orig.ServiceID)
	if err != nil {
		return Plan{}, err
	}
	if ms.Image != orig.To.Ref {
		return Plan{}, fmt.Errorf("%w: the service no longer runs the image of this plan", ErrStale)
	}
	pol, err := s.Policy(ms.ID)
	if err != nil {
		return Plan{}, err
	}
	cur, err := ParseRef(ms.Image)
	if err != nil {
		return Plan{}, err
	}
	// A rollback is never automatic, whatever the policy says.
	pol.Mode = ModeManual
	p, err := s.makePlan(ctx, actor, ms, sc, pol, cur, orig.To.Digest, orig.From, KindRollback, SourceRollback, orig.ID, true)
	if err != nil {
		return Plan{}, err
	}
	if p == nil {
		return Plan{}, ErrConflict
	}
	return *p, nil
}

// Apply runs an approved plan: policy window, drift check, leader term and
// a conditional write come first, then the service update operation is
// started and approved with the plan's approval and watched until it
// ends. The outcome (succeeded, rolled back by Swarm, failed, unknown after
// the wait) is recorded on the plan; only a failure to start returns an
// error with the plan.
func (s *Service) Apply(ctx context.Context, actor, planID string) (Plan, error) {
	p, ver, err := s.loadPlan(planID)
	if err != nil {
		return Plan{}, err
	}
	if p.State != PlanApproved || p.computeDigest() != p.Digest {
		return p, fmt.Errorf("%w: plan is %s", ErrConflict, p.State)
	}
	if err := runnable(p); err != nil {
		return p, err
	}
	ms, _, err := s.lookup(ctx, p.ServiceID)
	if err != nil {
		return p, err
	}
	if ms.Image != p.From.Ref {
		p.State, p.Error = PlanSuperseded, "the running image changed since the plan was made"
		if err := s.put(CollPlans, p.ID, p, &ver); err != nil {
			return p, err
		}
		return p, ErrStale
	}
	pol, err := s.Policy(ms.ID)
	if err != nil {
		return p, err
	}
	if p.Auto && pol.Mode != ModeAuto {
		return p, fmt.Errorf("%w: the policy of the service is no longer automatic; approve the plan again", ErrConflict)
	}
	if !pol.inWindow(s.now()) {
		return p, ErrOutsideWindow
	}
	term, err := s.Term()
	if err != nil {
		return p, err
	}
	p.State, p.JournalID = PlanRunning, newID("iuop")
	if err := s.put(CollPlans, p.ID, p, &ver); err != nil {
		return Plan{}, err
	}
	_ = s.Journal.Begin(p.JournalID, "imageupdate/apply", []string{"update", "verify"}, term)
	_ = s.Journal.StartStep(p.JournalID, "update", term)

	var started Started
	if p.Source == SourceRollback {
		started, err = s.Driver.StartRollback(ctx, actor, p.ServiceID, p.Strategy)
	} else {
		started, err = s.Driver.StartUpdate(ctx, actor, p.ServiceID, p.To.Ref, p.Strategy)
	}
	if err == nil {
		err = s.Driver.Approve(ctx, actor, started.OperationID, started.Digest)
		p.OperationID = started.OperationID
	}
	if err != nil {
		_ = s.Journal.FinishStep(p.JournalID, "update", opjournal.Failed, term)
		if errors.Is(err, ErrBusy) {
			_ = s.Journal.Abort(p.JournalID, term)
			p.State, p.JournalID = PlanApproved, ""
			if perr := s.putAny(p); perr != nil {
				return p, perr
			}
			return p, ErrBusy
		}
		return s.fail(ctx, p, ms, "start: "+err.Error(), true)
	}
	_ = s.Journal.FinishStep(p.JournalID, "update", opjournal.Succeeded, term)
	_ = s.Journal.StartStep(p.JournalID, "verify", term)
	if err := s.putAny(p); err != nil {
		return p, err
	}
	return s.watch(ctx, p, ms, pol, term)
}

// watch polls the operation until it ends or the wait is over.
func (s *Service) watch(ctx context.Context, p Plan, ms ManagedService, pol Policy, term uint64) (Plan, error) {
	polls := int(time.Duration(pol.WaitMinutes) * time.Minute / s.pollEvery())
	for i := 0; i < polls; i++ {
		st, err := s.Driver.Status(ctx, p.OperationID)
		if err == nil && st.State != OpRunning && st.State != "" {
			p.ExecutedBy = st.Runtime
			switch st.State {
			case OpSucceeded:
				_ = s.Journal.FinishStep(p.JournalID, "verify", opjournal.Succeeded, term)
				return s.succeed(ctx, p, ms)
			case OpRolledBack:
				_ = s.Journal.FinishStep(p.JournalID, "verify", opjournal.Failed, term)
				p.State, p.FinishedAt = PlanRolledBack, s.now()
				p.Error = redact(st.Detail)
				p.Rollback = &RollbackRecord{Automatic: true, Restored: p.From.Ref, Reason: redact(st.Detail), At: s.now()}
				_ = s.putAny(p)
				s.emit(ctx, notify.SeverityWarning, "imageupdate.rolled_back", "service/"+ms.ID,
					fmt.Sprintf("%s: the update to %s failed its health gate and Swarm rolled back to %s", ms.Name, p.To.Ref, p.From.Ref),
					map[string]string{"service": ms.ID, "plan": p.ID, "from": p.From.Ref, "to": p.To.Ref})
				return p, nil
			default:
				_ = s.Journal.FinishStep(p.JournalID, "verify", opjournal.Failed, term)
				return s.fail(ctx, p, ms, st.Detail, false)
			}
		}
		if err := s.sleep(ctx, s.pollEvery()); err != nil {
			return s.fail(ctx, p, ms, "interrupted: outcome unknown, check the service", false)
		}
	}
	_ = s.Journal.FinishStep(p.JournalID, "verify", opjournal.Unknown, term)
	return s.fail(ctx, p, ms, fmt.Sprintf("no result after %d minutes: outcome unknown, check the service", pol.WaitMinutes), false)
}

func (s *Service) succeed(ctx context.Context, p Plan, ms ManagedService) (Plan, error) {
	p.State, p.FinishedAt, p.Error = PlanSucceeded, s.now(), ""
	if err := s.putAny(p); err != nil {
		return p, err
	}
	st, err := s.State(ms.ID)
	if err == nil {
		st.Baseline = &Baseline{Tag: p.To.Tag, Digest: p.To.Digest, At: s.now(), PlanID: p.ID}
		_ = s.putState(st)
	}
	if p.Source == SourceRollback && p.Reference != "" {
		if orig, _, err := s.loadPlan(p.Reference); err == nil {
			orig.Rollback = &RollbackRecord{Automatic: false, Restored: p.To.Ref, Reason: "rolled back by " + p.ApprovedBy, At: s.now(), PlanID: p.ID}
			_ = s.putAny(orig)
		}
	}
	s.emit(ctx, notify.SeverityInfo, "imageupdate.applied", "service/"+ms.ID,
		fmt.Sprintf("%s updated to %s", ms.Name, p.To.Ref), map[string]string{"service": ms.ID, "plan": p.ID, "from": p.From.Ref, "to": p.To.Ref, "runtime": string(p.ExecutedBy)})
	return p, nil
}

func (s *Service) fail(ctx context.Context, p Plan, ms ManagedService, cause string, asError bool) (Plan, error) {
	p.State, p.FinishedAt, p.Error = PlanFailed, s.now(), redact(cause)
	if err := s.putAny(p); err != nil {
		return p, err
	}
	s.emit(ctx, notify.SeverityError, "imageupdate.failed", "service/"+ms.ID,
		fmt.Sprintf("%s: update to %s failed: %s", ms.Name, p.To.Ref, p.Error), map[string]string{"service": ms.ID, "plan": p.ID, "to": p.To.Ref})
	if asError {
		return p, fmt.Errorf("imageupdate: %s", p.Error)
	}
	return p, nil
}

// RecoverInterrupted marks plans that a previous leader left running as
// failed with an unknown outcome. The apply journal is in memory, so what
// happened to the service is unknown; the operator checks it. Nothing is
// sent to the driver.
func (s *Service) RecoverInterrupted(ctx context.Context) (int, error) {
	ps, err := s.Plans()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range ps {
		if p.State != PlanRunning {
			continue
		}
		p.State, p.FinishedAt = PlanFailed, s.now()
		p.Error = "interrupted by a manager restart or failover: outcome unknown, check the service"
		if err := s.putAny(p); err != nil {
			return n, err
		}
		n++
		s.emit(ctx, notify.SeverityError, "imageupdate.failed", "service/"+p.ServiceID, p.ServiceName+": "+p.Error, map[string]string{"service": p.ServiceID, "plan": p.ID})
	}
	return n, nil
}

// RunDue is the scheduler's run: it checks the services whose policy is
// notify or auto and whose interval elapsed, then applies automatic plans
// inside their windows. It returns the number of checked services. Services
// outside the ownership scope are never looked at.
func (s *Service) RunDue(ctx context.Context) (int, error) {
	views, err := s.Services(ctx)
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, v := range views {
		if v.Policy.Mode != ModeNotify && v.Policy.Mode != ModeAuto {
			continue
		}
		if !v.State.LastCheckAt.IsZero() && s.now().Sub(v.State.LastCheckAt) < time.Duration(v.Policy.CheckEveryMinutes)*time.Minute {
			continue
		}
		n++
		if _, err := s.Check(ctx, v.Service.ID, CheckOptions{}); err != nil {
			errs = append(errs, err)
		}
	}
	plans, err := s.Plans()
	if err != nil {
		return n, errors.Join(append(errs, err)...)
	}
	for _, p := range plans {
		if p.State != PlanApproved || !p.Auto {
			continue
		}
		if _, err := s.Apply(ctx, AutoActor, p.ID); err != nil && !errors.Is(err, ErrOutsideWindow) && !errors.Is(err, ErrBusy) {
			errs = append(errs, fmt.Errorf("plan %s: %w", p.ID, err))
		}
	}
	return n, errors.Join(errs...)
}
