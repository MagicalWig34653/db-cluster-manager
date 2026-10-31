// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// Collections in the replicated document store (shared KV with the vuln
// package: vuln.MetastoreKV in production, vuln.MemoryKV in tests).
const (
	CollHosts   = "patch_hosts"
	CollGroups  = "patch_groups"
	CollReports = "patch_reports"
	CollHistory = "patch_history"
	CollPlans   = "patch_plans"
	CollSetting = "patch_settings"

	maxHistoryPerHost = 200
	maxInstalled      = 20000
	maxUpdates        = 5000
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Host is the patch record of a host. The host itself is known to the
// manager from the enrollment flow; this adds grouping and a policy.
type Host struct {
	ID     string  `json:"id"`
	Name   string  `json:"name,omitempty"`
	Group  string  `json:"group,omitempty"`
	Policy *Policy `json:"policy,omitempty"` // override; nil = group/default
	// LastPollAt and LastPollError record the latest pull attempt.
	LastPollAt    time.Time `json:"lastPollAt,omitempty"`
	LastPollError string    `json:"lastPollError,omitempty"`
	// DenyDigest is the digest of the last certificate deny list the host's
	// patch agent confirmed (see DistributeDenyList).
	DenyDigest string `json:"denyDigest,omitempty"`
	// AgentRuntime is the runtime that answered the latest poll (ADR 0024),
	// set by the manager from the verified response.
	AgentRuntime agentrt.Runtime `json:"agentRuntime,omitempty"`
}

// Group is a host group with an optional policy.
type Group struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Policy *Policy `json:"policy,omitempty"`
}

// PlanState is the state of a patch plan.
type PlanState string

// Plan states.
const (
	PlanProposed  PlanState = "proposed"
	PlanApproved  PlanState = "approved"
	PlanRunning   PlanState = "running"
	PlanSucceeded PlanState = "succeeded"
	PlanFailed    PlanState = "failed"
	PlanDeclined  PlanState = "declined"
)

// Plan is a reviewed patch run for one host. Packages are explicit; the
// digest covers everything that will be sent to the host.
type Plan struct {
	ID           string     `json:"id"`
	HostID       string     `json:"hostId"`
	Manager      Manager    `json:"manager"`
	Packages     []string   `json:"packages"`
	SecurityOnly bool       `json:"securityOnly,omitempty"`
	Reboot       bool       `json:"reboot,omitempty"`
	Coordination string     `json:"coordination,omitempty"` // cluster planner procedure ID
	Simulation   Simulation `json:"simulation"`
	Digest       string     `json:"digest"`
	State        PlanState  `json:"state"`
	CreatedBy    string     `json:"createdBy"`
	CreatedAt    time.Time  `json:"createdAt"`
	ApprovedBy   string     `json:"approvedBy,omitempty"`
	ApprovedAt   time.Time  `json:"approvedAt,omitempty"`
	FinishedAt   time.Time  `json:"finishedAt,omitempty"`
	OpID         string     `json:"opId,omitempty"`
	Error        string     `json:"error,omitempty"`
	// Remaining lists planned packages that still had an update after the
	// run (the plan did not fully take effect).
	Remaining []string `json:"remaining,omitempty"`
	// RebootStatus: "", "requested", or "skipped: <reason>".
	RebootStatus string `json:"rebootStatus,omitempty"`
	// ContainerRuntime marks a plan for the container-runtime class (Docker
	// engine). Such a plan contains only that class and is subject to the
	// engine guard (ADR 0019 addendum in ADR 0023).
	ContainerRuntime bool `json:"containerRuntime,omitempty"`
	// AcknowledgeTasks names running Swarm tasks the approver accepts to
	// interrupt; covered by the digest.
	AcknowledgeTasks []string `json:"acknowledgeTasks,omitempty"`
	// EngineGuard is the guard result at proposal time and again at apply
	// time (the apply-time result replaces it).
	EngineGuard *EngineGuardResult `json:"engineGuard,omitempty"`
	// EngineVersionBefore and EngineVersionAfter are the engine versions
	// seen by the probe before and after the run.
	EngineVersionBefore string `json:"engineVersionBefore,omitempty"`
	EngineVersionAfter  string `json:"engineVersionAfter,omitempty"`
	// ExecutedBy is the agent runtime that applied the plan (always
	// "native"; recorded from the verified answer, ADR 0024).
	ExecutedBy agentrt.Runtime `json:"executedBy,omitempty"`
}

func (p Plan) computeDigest() string {
	b, _ := json.Marshal(struct {
		ID, HostID   string
		Manager      Manager
		Packages     []string
		SecurityOnly bool
		Reboot       bool
		Coordination string
		Simulation   Simulation
		Runtime      bool     `json:",omitempty"`
		Ack          []string `json:",omitempty"`
	}{p.ID, p.HostID, p.Manager, p.Packages, p.SecurityOnly, p.Reboot, p.Coordination, p.Simulation, p.ContainerRuntime, p.AcknowledgeTasks})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ProposeInput asks for a plan.
type ProposeInput struct {
	Packages     []string `json:"packages,omitempty"` // empty = all (security only if so selected/required)
	SecurityOnly bool     `json:"securityOnly,omitempty"`
	Reboot       bool     `json:"reboot,omitempty"`
	Coordination string   `json:"coordination,omitempty"`
	// AcknowledgeTasks (container-runtime plans only) names running Swarm
	// tasks the approver accepts to interrupt.
	AcknowledgeTasks []string `json:"acknowledgeTasks,omitempty"`
}

// Service is the manager side of patch management. Writes must happen on the
// manager leader; Term is its fencing term and reaches the agents.
type Service struct {
	KV      vuln.KV
	Journal *opjournal.Journal
	Agents  HostAgent
	Guard   Guard
	// Engine probes the Docker engine state for container-runtime plans.
	// Nil makes the engine guard refuse (fail closed).
	Engine EngineProbe
	// Sleep waits between engine health probes (default time.Sleep).
	Sleep func(time.Duration)
	// Term returns the current leader term; an error means not leader.
	Term   func() (uint64, error)
	Now    func() time.Time
	Notify func(context.Context, notify.Event)
	// DenyList returns the current certificate deny list for the patch
	// agents. Nil disables distribution.
	DenyList func() (DenyList, error)
}

// DenyList is the distributable certificate deny list (revoked serials in
// lowercase hex). Digest changes whenever the content changes; Revision
// grows with every change so an agent never takes an older list over a
// newer one.
type DenyList struct {
	Revision int64
	Serials  []string
	Digest   string
}

// RecoverInterrupted marks plans that a previous leader left in "running"
// (failover or restart during the run) as failed. The apply journal is held
// in memory, so what ran on the host is unknown; the plan says so and the
// operator checks the host report. Nothing is sent to any host.
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
		_, _ = s.fail(ctx, p, "", errors.New("interrupted by a manager restart or failover; the outcome on the host is unknown, check its report"))
		n++
	}
	return n, nil
}

// DistributeDenyList sends the current deny list to every patch host whose
// agent has not confirmed it yet. It needs the leader term (the command is
// fenced) and returns the joined per-host failures; a failed host is retried
// on the next call.
func (s *Service) DistributeDenyList(ctx context.Context) error {
	if s.DenyList == nil {
		return nil
	}
	term, err := s.Term()
	if err != nil {
		return err
	}
	dl, err := s.DenyList()
	if err != nil {
		return err
	}
	hs, err := s.Hosts()
	if err != nil {
		return err
	}
	step := "deny-list-" + dl.Digest
	if len(step) > 40 {
		step = step[:40]
	}
	var errs []error
	for _, h := range hs {
		if h.DenyDigest == dl.Digest {
			continue
		}
		_, err := s.Agents.Do(ctx, h.ID, AgentCommand{Kind: CmdDenyList, Term: term, StepID: step + "/" + h.ID,
			DenyRevision: dl.Revision, DenySerials: append([]string{}, dl.Serials...)})
		if err != nil {
			errs = append(errs, fmt.Errorf("host %s: %w", h.ID, err))
			continue
		}
		cur, err := s.Host(h.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("host %s: %w", h.ID, err))
			continue
		}
		cur.DenyDigest = dl.Digest
		if err := s.put(CollHosts, cur.ID, cur, nil); err != nil {
			errs = append(errs, fmt.Errorf("host %s: %w", h.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
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

// --- settings, groups, hosts ---

// DefaultPolicy returns the installation default (report-only if unset).
func (s *Service) DefaultPolicy() (Policy, error) {
	var p Policy
	if _, _, err := s.get(CollSetting, "default-policy", &p); err != nil {
		return Policy{}, err
	}
	return p.Normalized(), nil
}

// SetDefaultPolicy replaces the default policy.
func (s *Service) SetDefaultPolicy(p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.put(CollSetting, "default-policy", p.Normalized(), nil)
}

// UpsertGroup creates or replaces a group.
func (s *Service) UpsertGroup(g Group) (Group, error) {
	if !idRe.MatchString(g.ID) || len(g.Name) > 200 {
		return Group{}, ErrInvalid
	}
	if g.Policy != nil {
		if err := g.Policy.Validate(); err != nil {
			return Group{}, err
		}
	}
	return g, s.put(CollGroups, g.ID, g, nil)
}

// Groups lists groups.
func (s *Service) Groups() ([]Group, error) { return list[Group](s, CollGroups) }

// DeleteGroup removes an empty group.
func (s *Service) DeleteGroup(id string) error {
	hs, err := s.Hosts()
	if err != nil {
		return err
	}
	for _, h := range hs {
		if h.Group == id {
			return fmt.Errorf("%w: group %q still has hosts", ErrConflict, id)
		}
	}
	if _, ok, err := s.get(CollGroups, id, &Group{}); err != nil || !ok {
		return firstErr(err, ErrNotFound)
	}
	return s.KV.Txn(vuln.KVPut{Collection: CollGroups, Key: id, Delete: true})
}

// UpsertHost creates or updates the patch record of a host. Poll state is
// preserved.
func (s *Service) UpsertHost(h Host) (Host, error) {
	if !idRe.MatchString(h.ID) || len(h.Name) > 200 {
		return Host{}, ErrInvalid
	}
	if h.Policy != nil {
		if err := h.Policy.Validate(); err != nil {
			return Host{}, err
		}
	}
	if h.Group != "" {
		if _, ok, err := s.get(CollGroups, h.Group, &Group{}); err != nil || !ok {
			return Host{}, firstErr(err, fmt.Errorf("%w: group %q", ErrInvalid, h.Group))
		}
	}
	var old Host
	if _, ok, err := s.get(CollHosts, h.ID, &old); err != nil {
		return Host{}, err
	} else if ok {
		h.LastPollAt, h.LastPollError, h.DenyDigest, h.AgentRuntime = old.LastPollAt, old.LastPollError, old.DenyDigest, old.AgentRuntime
	}
	return h, s.put(CollHosts, h.ID, h, nil)
}

// Hosts lists host records.
func (s *Service) Hosts() ([]Host, error) {
	hs, err := list[Host](s, CollHosts)
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })
	return hs, err
}

// Host returns one host record.
func (s *Service) Host(id string) (Host, error) {
	var h Host
	_, ok, err := s.get(CollHosts, id, &h)
	if err != nil {
		return Host{}, err
	}
	if !ok {
		return Host{}, ErrNotFound
	}
	return h, nil
}

// EffectivePolicy resolves host override, group and default.
func (s *Service) EffectivePolicy(h Host) (Policy, error) {
	def, err := s.DefaultPolicy()
	if err != nil {
		return Policy{}, err
	}
	var gp *Policy
	if h.Group != "" {
		var g Group
		if _, ok, err := s.get(CollGroups, h.Group, &g); err != nil {
			return Policy{}, err
		} else if ok {
			gp = g.Policy
		}
	}
	return Resolve(h.Policy, gp, def), nil
}

// --- reports, history, overview ---

func validateReport(r HostReport) error {
	if len(r.Installed) > maxInstalled || len(r.Updates) > maxUpdates {
		return fmt.Errorf("%w: report too large", ErrInvalid)
	}
	switch r.RebootRequired {
	case Yes, No, Unknown:
	default:
		return fmt.Errorf("%w: rebootRequired", ErrInvalid)
	}
	if _, err := NewAdapter(r.Manager); err != nil {
		return err
	}
	for _, u := range r.Updates {
		if u.Name == "" || u.Available == "" {
			return fmt.Errorf("%w: update entry", ErrInvalid)
		}
	}
	return nil
}

// Report returns the latest report of a host.
func (s *Service) Report(hostID string) (HostReport, error) {
	var r HostReport
	_, ok, err := s.get(CollReports, hostID, &r)
	if err != nil {
		return r, err
	}
	if !ok {
		return r, ErrNotFound
	}
	return r, nil
}

// Ingest stores a report for a host the manager authenticated (hostID comes
// from the mTLS identity, never from the payload), records the diff in the
// history and notifies about new security updates.
func (s *Service) Ingest(ctx context.Context, hostID string, r HostReport) (Diff, error) {
	if !idRe.MatchString(hostID) {
		return Diff{}, ErrInvalid
	}
	if err := validateReport(r); err != nil {
		return Diff{}, err
	}
	r.HostID = hostID
	for i := range r.Updates {
		r.Updates[i].Class = Classify(r.Updates[i].Name)
	}
	sortUpdates(r.Updates)
	sortPackages(r.Installed)
	var prev HostReport
	_, had, err := s.get(CollReports, hostID, &prev)
	if err != nil {
		return Diff{}, err
	}
	var pp *HostReport
	if had {
		pp = &prev
	}
	d := DiffReports(pp, r)
	if err := s.put(CollReports, hostID, r, nil); err != nil {
		return Diff{}, err
	}
	if !had || !d.Empty() {
		if err := s.addHistory(HistoryEntry{HostID: hostID, At: s.now(), Kind: "report", Diff: slimDiff(d), UpdatesPending: len(r.Updates),
			SecurityCount: len(r.SecurityUpdates()), RebootRequired: r.RebootRequired}); err != nil {
			return d, err
		}
	}
	if len(d.NewSecurity) > 0 {
		var ns []string
		for _, u := range d.NewSecurity {
			ns = append(ns, u.Name)
		}
		if len(ns) > 20 {
			ns = append(ns[:20], "...")
		}
		s.emit(ctx, notify.SeverityWarning, "patch.security_updates", "host/"+hostID,
			fmt.Sprintf("%d new security update(s) for %s", len(d.NewSecurity), hostID),
			map[string]string{"host": hostID, "count": fmt.Sprint(len(d.NewSecurity)), "packages": strings.Join(ns, ",")})
	}
	return d, nil
}

// slimDiff drops the bulky new-installed list from stored history.
func slimDiff(d Diff) Diff {
	if len(d.Installed) > 50 {
		d.Installed = d.Installed[:50]
	}
	if len(d.NewUpdates) > 200 {
		d.NewUpdates = d.NewUpdates[:200]
	}
	return d
}

func (s *Service) addHistory(e HistoryEntry) error {
	key := e.HostID + "/" + e.At.Format("20060102T150405.000000000Z")
	b, _ := json.Marshal(e)
	puts := []vuln.KVPut{{Collection: CollHistory, Key: key, Value: b}}
	docs, err := s.KV.List(CollHistory)
	if err != nil {
		return err
	}
	var mine []string
	for _, d := range docs {
		if strings.HasPrefix(d.Key, e.HostID+"/") {
			mine = append(mine, d.Key)
		}
	}
	sort.Strings(mine)
	for len(mine) >= maxHistoryPerHost {
		puts = append(puts, vuln.KVPut{Collection: CollHistory, Key: mine[0], Delete: true})
		mine = mine[1:]
	}
	return s.KV.Txn(puts...)
}

// History returns the host's history, newest first.
func (s *Service) History(hostID string) ([]HistoryEntry, error) {
	docs, err := s.KV.List(CollHistory)
	if err != nil {
		return nil, err
	}
	var out []HistoryEntry
	for _, d := range docs {
		if !strings.HasPrefix(d.Key, hostID+"/") {
			continue
		}
		var e HistoryEntry
		if err := json.Unmarshal(d.Value, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// HostSummary is the per-host dashboard row.
type HostSummary struct {
	Host           Host      `json:"host"`
	Policy         Policy    `json:"policy"`
	OS             OSInfo    `json:"os"`
	Manager        Manager   `json:"manager,omitempty"`
	CollectedAt    time.Time `json:"collectedAt,omitempty"`
	Stale          bool      `json:"stale"`
	UpdatesPending int       `json:"updatesPending"`
	SecurityCount  int       `json:"securityPending"`
	RebootRequired Tristate  `json:"rebootRequired"`
	SecurityKnown  bool      `json:"securityKnown"`
}

// Overview lists all hosts with their latest state. A host without a report
// or with an old one is stale, never "up to date".
func (s *Service) Overview() ([]HostSummary, error) {
	hs, err := s.Hosts()
	if err != nil {
		return nil, err
	}
	out := make([]HostSummary, 0, len(hs))
	for _, h := range hs {
		pol, err := s.EffectivePolicy(h)
		if err != nil {
			return nil, err
		}
		sum := HostSummary{Host: h, Policy: pol, RebootRequired: Unknown, Stale: true}
		if r, err := s.Report(h.ID); err == nil {
			sum.OS, sum.Manager, sum.CollectedAt = r.OS, r.Manager, r.CollectedAt
			sum.UpdatesPending, sum.SecurityCount = len(r.Updates), len(r.SecurityUpdates())
			sum.RebootRequired, sum.SecurityKnown = r.RebootRequired, r.SecurityKnown
			sum.Stale = s.now().Sub(r.CollectedAt) > 3*pol.ReportEvery()
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, nil
}

// PackageView is the per-package dashboard row.
type PackageView struct {
	Name     string   `json:"name"`
	Hosts    []string `json:"hosts"`
	Security bool     `json:"security"`
	Versions []string `json:"versions"`
}

// Packages lists packages with pending updates across hosts.
func (s *Service) Packages() ([]PackageView, error) {
	hs, err := s.Hosts()
	if err != nil {
		return nil, err
	}
	by := map[string]*PackageView{}
	for _, h := range hs {
		r, err := s.Report(h.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		for _, u := range r.Updates {
			pv := by[u.Name]
			if pv == nil {
				pv = &PackageView{Name: u.Name}
				by[u.Name] = pv
			}
			pv.Hosts = append(pv.Hosts, h.ID)
			pv.Security = pv.Security || u.Security
			if !slices.Contains(pv.Versions, u.Available) {
				pv.Versions = append(pv.Versions, u.Available)
			}
		}
	}
	out := make([]PackageView, 0, len(by))
	for _, v := range by {
		sort.Strings(v.Versions)
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Security != out[j].Security {
			return out[i].Security
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// --- polling ---

// Poll pulls a report from the host's agent and ingests it. A failure is
// recorded on the host and notified once per transition into failure.
func (s *Service) Poll(ctx context.Context, hostID string, refresh bool) (HostReport, error) {
	h, err := s.Host(hostID)
	if err != nil {
		return HostReport{}, err
	}
	res, err := s.Agents.Do(ctx, hostID, AgentCommand{Kind: CmdReport, Refresh: refresh})
	if err == nil && res.Report == nil {
		err = errors.New("patch: agent returned no report")
	}
	wasFailing := h.LastPollError != ""
	h.LastPollAt = s.now()
	if err != nil {
		h.LastPollError = truncate(err.Error(), 300)
		_ = s.put(CollHosts, h.ID, h, nil)
		if !wasFailing {
			s.emit(ctx, notify.SeverityWarning, "patch.poll_failed", "host/"+hostID, "patch report failed for "+hostID, map[string]string{"host": hostID, "error": h.LastPollError})
		}
		return HostReport{}, err
	}
	h.LastPollError = ""
	h.AgentRuntime = res.Runtime
	if err := s.put(CollHosts, h.ID, h, nil); err != nil {
		return HostReport{}, err
	}
	if _, err := s.Ingest(ctx, hostID, *res.Report); err != nil {
		return HostReport{}, err
	}
	return s.Report(hostID)
}

// PollDue polls every host whose last attempt is older than its policy's
// report interval and returns the IDs it polled.
func (s *Service) PollDue(ctx context.Context) ([]string, error) {
	hs, err := s.Hosts()
	if err != nil {
		return nil, err
	}
	var done []string
	for _, h := range hs {
		pol, err := s.EffectivePolicy(h)
		if err != nil {
			return done, err
		}
		if !h.LastPollAt.IsZero() && s.now().Sub(h.LastPollAt) < pol.ReportEvery() {
			continue
		}
		done = append(done, h.ID)
		_, _ = s.Poll(ctx, h.ID, true) // failure is recorded on the host
	}
	return done, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// --- plans ---

// Plans lists plans, newest first.
func (s *Service) Plans() ([]Plan, error) {
	ps, err := list[Plan](s, CollPlans)
	sort.Slice(ps, func(i, j int) bool { return ps[i].CreatedAt.After(ps[j].CreatedAt) })
	return ps, err
}

// Plan returns one plan.
func (s *Service) Plan(id string) (Plan, error) {
	var p Plan
	_, ok, err := s.get(CollPlans, id, &p)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, ErrNotFound
	}
	return p, nil
}

// Propose builds a plan from the latest report and the host's simulation. It
// is read-only on the host. Report-only hosts get proposals too (to preview
// the effect) but those cannot be approved.
func (s *Service) Propose(ctx context.Context, actor, hostID string, in ProposeInput) (Plan, error) {
	h, err := s.Host(hostID)
	if err != nil {
		return Plan{}, err
	}
	pol, err := s.EffectivePolicy(h)
	if err != nil {
		return Plan{}, err
	}
	rep, err := s.Report(hostID)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: no report for host %q yet", ErrNotFound, hostID)
	}
	if in.Reboot && pol.Reboot == RebootNever {
		return Plan{}, ErrRebootDenied
	}
	secOnly := in.SecurityOnly || pol.SecurityOnly
	if secOnly && !rep.SecurityKnown {
		return Plan{}, fmt.Errorf("%w: security-only needs a package manager that classifies security updates", ErrInvalid)
	}
	chosen, err := Select(rep.Updates, Selection{Packages: in.Packages, SecurityOnly: secOnly})
	if err != nil {
		return Plan{}, err
	}
	if len(chosen) == 0 {
		return Plan{}, fmt.Errorf("%w: nothing to update", ErrInvalid)
	}
	// The container-runtime class is never part of a default selection; it
	// has to be named explicitly and gets its own plan.
	var engineSel, osSel []Update
	for _, u := range chosen {
		if Classify(u.Name) == ClassContainerRuntime {
			engineSel = append(engineSel, u)
		} else {
			osSel = append(osSel, u)
		}
	}
	if len(in.Packages) == 0 {
		chosen, engineSel = osSel, nil
		if len(chosen) == 0 {
			return Plan{}, fmt.Errorf("%w: nothing to update (container-runtime packages must be named explicitly)", ErrInvalid)
		}
	}
	if len(engineSel) > 0 && len(osSel) > 0 {
		return Plan{}, fmt.Errorf("%w: container-runtime packages need their own plan", ErrInvalid)
	}
	if len(in.AcknowledgeTasks) > 0 && len(engineSel) == 0 {
		return Plan{}, fmt.Errorf("%w: acknowledged tasks only apply to container-runtime plans", ErrInvalid)
	}
	ack := append([]string(nil), in.AcknowledgeTasks...)
	sort.Strings(ack)
	if len(ack) > 200 {
		return Plan{}, fmt.Errorf("%w: too many acknowledged tasks", ErrInvalid)
	}
	for _, t := range ack {
		if !idRe.MatchString(t) {
			return Plan{}, fmt.Errorf("%w: task %q", ErrInvalid, t)
		}
	}
	pkgs := names(chosen)
	sort.Strings(pkgs)
	if in.Coordination != "" && !idRe.MatchString(in.Coordination) {
		return Plan{}, ErrInvalid
	}
	res, err := s.Agents.Do(ctx, hostID, AgentCommand{Kind: CmdSimulate, Packages: pkgs, SecurityOnly: secOnly})
	if err != nil {
		return Plan{}, fmt.Errorf("simulate: %w", err)
	}
	if res.Simulation == nil {
		return Plan{}, errors.New("patch: agent returned no simulation")
	}
	p := Plan{ID: newID("pp"), HostID: hostID, Manager: rep.Manager, Packages: pkgs, SecurityOnly: secOnly, Reboot: in.Reboot,
		Coordination: in.Coordination, Simulation: *res.Simulation, State: PlanProposed, CreatedBy: actor, CreatedAt: s.now()}
	if len(engineSel) > 0 {
		p.ContainerRuntime, p.AcknowledgeTasks = true, ack
		g, _ := s.engineGuard(ctx, hostID, ack)
		p.EngineGuard = &g
	}
	p.Digest = p.computeDigest()
	zero := uint64(0)
	return p, s.put(CollPlans, p.ID, p, &zero)
}

// Approve approves the plan with this digest. It refuses report-only hosts.
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
	h, err := s.Host(p.HostID)
	if err != nil {
		return Plan{}, err
	}
	pol, err := s.EffectivePolicy(h)
	if err != nil {
		return Plan{}, err
	}
	if pol.Mode == ModeReportOnly {
		return Plan{}, ErrReportOnly
	}
	if p.Reboot && pol.Reboot == RebootNever {
		return Plan{}, ErrRebootDenied
	}
	if p.ContainerRuntime && pol.ContainerRuntime != ModeApprovalRequired {
		return Plan{}, ErrRuntimeReportOnly
	}
	p.State, p.ApprovedBy, p.ApprovedAt = PlanApproved, actor, s.now()
	return p, s.put(CollPlans, p.ID, p, &ver)
}

// Decline declines a proposed or approved plan.
func (s *Service) Decline(planID string) (Plan, error) {
	p, ver, err := s.loadPlan(planID)
	if err != nil {
		return Plan{}, err
	}
	if p.State != PlanProposed && p.State != PlanApproved {
		return Plan{}, fmt.Errorf("%w: plan is %s", ErrConflict, p.State)
	}
	p.State = PlanDeclined
	return p, s.put(CollPlans, p.ID, p, &ver)
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

// Apply runs an approved plan. Checks, in order: plan approved and digest
// intact; policy mode and maintenance window; Galera guard; leader term. The
// plan moves to running with a conditional write, so two concurrent applies
// cannot both start. Steps are journaled and carry the leader term to the
// agent, which refuses stale terms.
func (s *Service) Apply(ctx context.Context, planID string) (Plan, error) {
	p, ver, err := s.loadPlan(planID)
	if err != nil {
		return Plan{}, err
	}
	if p.State != PlanApproved || p.computeDigest() != p.Digest {
		return Plan{}, fmt.Errorf("%w: plan is %s", ErrConflict, p.State)
	}
	h, err := s.Host(p.HostID)
	if err != nil {
		return Plan{}, err
	}
	pol, err := s.EffectivePolicy(h)
	if err != nil {
		return Plan{}, err
	}
	if err := pol.CheckApply(s.now()); err != nil {
		return p, err
	}
	if s.Guard == nil {
		return p, fmt.Errorf("%w: no guard configured", ErrGaleraBlocked)
	}
	if err := s.Guard.Check(ctx, p.HostID, ActionApply, p.Coordination); err != nil {
		return p, err
	}
	steps, kind := []string{"apply", "verify"}, "patch/apply"
	if p.ContainerRuntime {
		if pol.ContainerRuntime != ModeApprovalRequired {
			return p, ErrRuntimeReportOnly
		}
		g, st := s.engineGuard(ctx, p.HostID, p.AcknowledgeTasks)
		if !g.Allowed {
			return p, fmt.Errorf("%w: %s", ErrEngineGuard, g.Failed())
		}
		p.EngineGuard, p.EngineVersionBefore = &g, st.Version
		steps, kind = []string{"apply", "verify", "engine-verify"}, "patch/engine-apply"
	}
	term, err := s.Term()
	if err != nil {
		return p, err
	}
	p.State, p.OpID = PlanRunning, newID("op")
	if err := s.put(CollPlans, p.ID, p, &ver); err != nil {
		return Plan{}, err
	}
	if err := s.Journal.Begin(p.OpID, kind, steps, term); err != nil {
		return s.fail(ctx, p, "", err)
	}
	if err := s.Journal.StartStep(p.OpID, "apply", term); err != nil {
		return s.fail(ctx, p, "", err)
	}
	applied, err := s.Agents.Do(ctx, p.HostID, AgentCommand{Kind: CmdApply, Term: term, StepID: p.OpID + "/apply", Packages: p.Packages, SecurityOnly: p.SecurityOnly})
	if err != nil {
		return s.fail(ctx, p, "apply", err)
	}
	p.ExecutedBy = applied.Runtime
	_ = s.Journal.FinishStep(p.OpID, "apply", opjournal.Succeeded, term)
	if err := s.Journal.StartStep(p.OpID, "verify", term); err != nil {
		return s.fail(ctx, p, "", err)
	}
	res, err := s.Agents.Do(ctx, p.HostID, AgentCommand{Kind: CmdReport})
	if err != nil || res.Report == nil {
		return s.fail(ctx, p, "verify", firstErr(err, errors.New("no report")))
	}
	if _, err := s.Ingest(ctx, p.HostID, *res.Report); err != nil {
		return s.fail(ctx, p, "verify", err)
	}
	_ = s.Journal.FinishStep(p.OpID, "verify", opjournal.Succeeded, term)
	if p.ContainerRuntime {
		if err := s.Journal.StartStep(p.OpID, "engine-verify", term); err != nil {
			return s.fail(ctx, p, "", err)
		}
		post, bad := s.verifyEngine(ctx, p.HostID)
		p.EngineVersionAfter = post.Version
		if bad != "" {
			return s.fail(ctx, p, "engine-verify", fmt.Errorf("engine health verification failed after the update (%s -> %s): %s", p.EngineVersionBefore, post.Version, bad))
		}
		_ = s.Journal.FinishStep(p.OpID, "engine-verify", opjournal.Succeeded, term)
	}
	still := map[string]bool{}
	for _, u := range res.Report.Updates {
		still[u.Name] = true
	}
	for _, n := range p.Packages {
		if still[n] {
			p.Remaining = append(p.Remaining, n)
		}
	}
	p.State, p.FinishedAt = PlanSucceeded, s.now()
	if len(p.Remaining) > 0 {
		p.Error = "some packages still have updates after the run"
	}
	if err := s.afterApplyReboot(ctx, &p, pol, res.Report.RebootRequired, term); err != nil {
		p.RebootStatus = "failed: " + truncate(err.Error(), 200)
	}
	result := string(p.State)
	if p.ContainerRuntime {
		result += fmt.Sprintf("; engine %s -> %s", p.EngineVersionBefore, p.EngineVersionAfter)
	}
	_ = s.addHistory(HistoryEntry{HostID: p.HostID, At: s.now(), Kind: "apply", PlanID: p.ID, Result: result, UpdatesPending: len(res.Report.Updates),
		SecurityCount: len(res.Report.SecurityUpdates()), RebootRequired: res.Report.RebootRequired})
	if err := s.putAny(p); err != nil {
		return p, err
	}
	if len(p.Remaining) > 0 {
		s.emit(ctx, notify.SeverityWarning, "patch.run_incomplete", "host/"+p.HostID, "patch run left updates pending on "+p.HostID, map[string]string{"host": p.HostID, "plan": p.ID})
	}
	return p, nil
}

func (s *Service) putAny(p Plan) error { return s.put(CollPlans, p.ID, p, nil) }

// afterApplyReboot runs the reboot step if policy and plan allow it. The
// guard is evaluated again: the state of the cluster may have changed during
// the run. A denial is not a failure of the patch run.
func (s *Service) afterApplyReboot(ctx context.Context, p *Plan, pol Policy, required Tristate, term uint64) error {
	if err := pol.CheckReboot(s.now(), p.Reboot, required); err != nil {
		if required == Yes {
			p.RebootStatus = "skipped: " + err.Error()
		}
		return nil
	}
	if required == No {
		p.RebootStatus = "skipped: host does not report reboot-required"
		return nil
	}
	if err := s.Guard.Check(ctx, p.HostID, ActionReboot, p.Coordination); err != nil {
		p.RebootStatus = "skipped: " + err.Error()
		return nil
	}
	rid := p.OpID + "-reboot"
	if err := s.Journal.Begin(rid, "patch/reboot", []string{"reboot"}, term); err != nil {
		return err
	}
	if err := s.Journal.StartStep(rid, "reboot", term); err != nil {
		return err
	}
	if _, err := s.Agents.Do(ctx, p.HostID, AgentCommand{Kind: CmdReboot, Term: term, StepID: rid + "/reboot"}); err != nil {
		_ = s.Journal.FinishStep(rid, "reboot", opjournal.Unknown, term)
		return err
	}
	_ = s.Journal.FinishStep(rid, "reboot", opjournal.Succeeded, term)
	p.RebootStatus = "requested"
	return nil
}

func (s *Service) fail(ctx context.Context, p Plan, step string, cause error) (Plan, error) {
	if step != "" {
		if term, err := s.Term(); err == nil {
			st := opjournal.Failed
			if !errors.Is(cause, ErrStale) && !errors.Is(cause, ErrReadOnly) && isLostConnection(cause) {
				st = opjournal.Unknown
			}
			_ = s.Journal.FinishStep(p.OpID, step, st, term)
		}
	}
	p.State, p.FinishedAt, p.Error = PlanFailed, s.now(), truncate(cause.Error(), 400)
	_ = s.putAny(p)
	_ = s.addHistory(HistoryEntry{HostID: p.HostID, At: s.now(), Kind: "apply", PlanID: p.ID, Result: "failed: " + p.Error + engineHistory(p), RebootRequired: Unknown})
	s.emit(ctx, notify.SeverityError, "patch.run_failed", "host/"+p.HostID, "patch run failed on "+p.HostID, map[string]string{"host": p.HostID, "plan": p.ID, "error": p.Error})
	return p, cause
}

func engineHistory(p Plan) string {
	if !p.ContainerRuntime {
		return ""
	}
	return fmt.Sprintf("; engine %s -> %s", p.EngineVersionBefore, orUnknown(p.EngineVersionAfter))
}

// isLostConnection reports an error where the command may have run.
func isLostConnection(err error) bool {
	var ne interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne)
}

// --- vuln inventory bridge ---

// VulnInventory exposes the latest host reports as vuln inventory entries,
// so CVE feeds can match installed OS packages (X13).
type VulnInventory struct{ Svc *Service }

// Inventory implements vuln.InventorySource.
func (v VulnInventory) Inventory(context.Context) ([]vuln.InventoryEntry, error) {
	hs, err := v.Svc.Hosts()
	if err != nil {
		return nil, err
	}
	var out []vuln.InventoryEntry
	for _, h := range hs {
		r, err := v.Svc.Report(h.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, ReportInventoryEntry(r))
	}
	return out, nil
}

// OSEcosystem maps an os-release ID and version to a vuln ecosystem, or ""
// when no advisory feed uses one for the distribution.
func OSEcosystem(o OSInfo) string {
	major := o.VersionID
	if i := strings.IndexByte(major, '.'); i > 0 {
		major = major[:i]
	}
	switch strings.ToLower(o.ID) {
	case "debian":
		return "Debian:" + major
	case "ubuntu":
		return "Ubuntu:" + o.VersionID
	case "alpine":
		parts := strings.Split(o.VersionID, ".")
		if len(parts) >= 2 {
			return "Alpine:v" + parts[0] + "." + parts[1]
		}
		return "Alpine"
	case "rocky":
		return "Rocky Linux:" + major
	case "almalinux":
		return "AlmaLinux:" + major
	case "opensuse-leap", "sles":
		return "SUSE:" + o.VersionID
	}
	return ""
}

// ReportInventoryEntry converts a report to a vuln inventory entry. A
// distribution without a known ecosystem gets coverage "unknown": it is
// reported as unknown, never as safe.
func ReportInventoryEntry(r HostReport) vuln.InventoryEntry {
	e := vuln.InventoryEntry{ID: "host-" + r.HostID, Image: "host:" + r.HostID, Services: []string{r.HostID},
		ServiceKind: vuln.ServiceKindGeneric, Coverage: vuln.CoverageUnknown}
	eco := OSEcosystem(r.OS)
	if eco == "" {
		return e
	}
	e.Coverage = vuln.CoverageSBOM
	for _, p := range r.Installed {
		e.Components = append(e.Components, vuln.Component{Ecosystem: eco, Name: p.Name, Version: p.Version})
	}
	return e
}
