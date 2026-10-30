// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	// ErrBadRecoveryInput reports unparsable grastate.dat or wsrep-recover
	// output, or inconsistent diagnosis input.
	ErrBadRecoveryInput = errors.New("galera: bad recovery input")
	// ErrNotFullOutage reports that a node is still a live Primary member,
	// so this is not a full-cluster outage.
	ErrNotFullOutage = errors.New("galera: a Primary component exists; not a full outage")
	// ErrNoCandidate reports a diagnosis without a bootstrap candidate.
	ErrNoCandidate = errors.New("galera: no bootstrap candidate")
	// ErrZeroUUIDPrimary reports an unsafe but still existing Primary component.
	ErrZeroUUIDPrimary = errors.New("galera: Primary component has zero cluster UUID; operator investigation required")
)

// ZeroUUID is the cluster state UUID of a node without Galera state.
const ZeroUUID = "00000000-0000-0000-0000-000000000000"

// Grastate is the content of a node's grastate.dat.
type Grastate struct {
	Version            string `json:"version,omitempty"`
	UUID               string `json:"uuid"`
	Seqno              int64  `json:"seqno"` // -1: unclean shutdown or unknown
	SafeToBootstrap    bool   `json:"safe_to_bootstrap"`
	HasSafeToBootstrap bool   `json:"has_safe_to_bootstrap"` // absent before grastate version 2.1
}

// Empty reports a node that holds no Galera state (for example a fresh
// datadir whose state transfer never completed).
func (g Grastate) Empty() bool { return g.UUID == ZeroUUID && g.Seqno < 0 }

// Galera state UUIDs are RFC 4122 UUIDs of any version (time-based in
// practice), lowercase hex.
var stateUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func parseSeqno(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < -1 {
		return 0, fmt.Errorf("%w: seqno %q", ErrBadRecoveryInput, s)
	}
	return n, nil
}

// ParseGrastate parses grastate.dat ("key: value" lines, '#' comments).
// uuid and seqno are required; safe_to_bootstrap must be 0 or 1 if present.
func ParseGrastate(content string) (Grastate, error) {
	var g Grastate
	seen := map[string]bool{}
	for line := range strings.Lines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return Grastate{}, fmt.Errorf("%w: grastate line %q", ErrBadRecoveryInput, line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if seen[k] {
			return Grastate{}, fmt.Errorf("%w: duplicate grastate key %q", ErrBadRecoveryInput, k)
		}
		seen[k] = true
		switch k {
		case "version":
			g.Version = v
		case "uuid":
			if !stateUUIDRe.MatchString(v) {
				return Grastate{}, fmt.Errorf("%w: grastate uuid %q", ErrBadRecoveryInput, v)
			}
			g.UUID = v
		case "seqno":
			n, err := parseSeqno(v)
			if err != nil {
				return Grastate{}, err
			}
			g.Seqno = n
		case "safe_to_bootstrap":
			switch v {
			case "0", "1":
				g.SafeToBootstrap, g.HasSafeToBootstrap = v == "1", true
			default:
				return Grastate{}, fmt.Errorf("%w: safe_to_bootstrap %q", ErrBadRecoveryInput, v)
			}
		}
	}
	if !seen["uuid"] || !seen["seqno"] {
		return Grastate{}, fmt.Errorf("%w: grastate lacks uuid or seqno", ErrBadRecoveryInput)
	}
	return g, nil
}

// Position is a Galera state position.
type Position struct {
	UUID  string `json:"uuid"`
	Seqno int64  `json:"seqno"`
}

var recoveredRe = regexp.MustCompile(`WSREP: Recovered position: ([^\s:]+):([^\s,]+)`)

// ParseWsrepRecover extracts the last "WSREP: Recovered position: UUID:SEQNO"
// line from `mariadbd --wsrep-recover` output. An optional ",GTID" suffix is
// ignored.
func ParseWsrepRecover(out string) (Position, error) {
	m := recoveredRe.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		return Position{}, fmt.Errorf("%w: no recovered position in wsrep-recover output", ErrBadRecoveryInput)
	}
	last := m[len(m)-1]
	if !stateUUIDRe.MatchString(last[1]) {
		return Position{}, fmt.Errorf("%w: recovered uuid %q", ErrBadRecoveryInput, last[1])
	}
	n, err := parseSeqno(last[2])
	if err != nil {
		return Position{}, err
	}
	return Position{UUID: last[1], Seqno: n}, nil
}

// NodeInfo is the recovery evidence for one node, collected by the agent
// while the node is stopped. Nil fields were not available.
type NodeInfo struct {
	Slot      string     `json:"slot"`
	Grastate  *Grastate  `json:"grastate,omitempty"`
	Recovered *Position  `json:"recovered,omitempty"`
	Health    NodeHealth `json:"health"` // last observation, if any
}

// PositionSource tells where a node position came from.
type PositionSource string

// Position sources.
const (
	SourceUnknown      PositionSource = "unknown"
	SourceGrastate     PositionSource = "grastate"      // clean shutdown, seqno saved
	SourceWsrepRecover PositionSource = "wsrep-recover" // recovered from InnoDB after a crash
)

// NodePosition is the evaluated position of one node.
type NodePosition struct {
	Slot            string         `json:"slot"`
	UUID            string         `json:"uuid,omitempty"`
	Seqno           int64          `json:"seqno"`
	Source          PositionSource `json:"source"`
	SafeToBootstrap bool           `json:"safe_to_bootstrap"`
	Empty           bool           `json:"empty,omitempty"`
}

func (p NodePosition) known() bool { return p.Source != SourceUnknown && !p.Empty }

// RiskKind classifies a recovery risk.
type RiskKind string

// Risk kinds.
const (
	RiskMissingNode           RiskKind = "missing-node"            // no evidence from the node
	RiskUncleanShutdown       RiskKind = "unclean-shutdown"        // grastate seqno -1
	RiskPositionUnknown       RiskKind = "position-unknown"        // no usable seqno
	RiskRecoveredUUIDMismatch RiskKind = "recovered-uuid-mismatch" // wsrep-recover UUID differs from grastate
	RiskDivergingUUIDs        RiskKind = "diverging-uuids"         // nodes belong to different cluster histories
	RiskNoData                RiskKind = "no-data"                 // zero UUID: node holds no Galera state
	RiskSafeFlagMismatch      RiskKind = "safe-flag-mismatch"      // safe_to_bootstrap on a node that is behind
	RiskTie                   RiskKind = "tie"                     // several nodes at the highest seqno
)

// Risk is one finding the user must see before approving.
type Risk struct {
	Kind   RiskKind `json:"kind"`
	Slot   string   `json:"slot,omitempty"`
	Detail string   `json:"detail"`
}

// Diagnosis is the result of DiagnoseFullOutage. It is advice only.
type Diagnosis struct {
	// Candidate is the slot to bootstrap, or "" if none can be proposed.
	Candidate string         `json:"candidate,omitempty"`
	Positions []NodePosition `json:"positions"`
	Reasons   []string       `json:"reasons"`
	Risks     []Risk         `json:"risks,omitempty"`
	// RequiresApproval is always true: full-cluster recovery needs explicit
	// approval (A06).
	RequiresApproval bool `json:"requires_approval"`
	// Plan is the guided recovery plan without service specs (see
	// RecoveryPlan); empty when there is no candidate.
	Plan Plan `json:"plan"`
	// Kind is PlanZeroUUIDRecovery when every node holds the zero state
	// (the Galera group history is lost, ADR 0028); empty for the ordinary
	// full-outage recovery.
	Kind PlanKind `json:"recovery_kind,omitempty"`
	// AutoEligible reports that the per-cluster autoRecover policy may run
	// the recovery without a human approval: zero-UUID state, every
	// position readable and a unique highest seqno. It never waives the
	// approval by itself; the policy does, and it is always notified.
	AutoEligible bool `json:"auto_eligible"`
}

func evaluate(n NodeInfo) (NodePosition, []Risk) {
	p := NodePosition{Slot: n.Slot, Seqno: -1, Source: SourceUnknown}
	var risks []Risk
	g := n.Grastate
	if g == nil && n.Recovered == nil {
		return p, []Risk{{Kind: RiskMissingNode, Slot: n.Slot, Detail: "no grastate.dat or wsrep-recover result"}}
	}
	if g != nil {
		p.UUID, p.SafeToBootstrap = g.UUID, g.SafeToBootstrap
		if g.Empty() {
			p.Empty = true
			d := "zero cluster UUID: the node holds no Galera state"
			if g.SafeToBootstrap {
				d += "; its safe_to_bootstrap: 1 is meaningless and ignored"
			}
			return p, []Risk{{Kind: RiskNoData, Slot: n.Slot, Detail: d}}
		}
		if g.Seqno >= 0 {
			p.Seqno, p.Source = g.Seqno, SourceGrastate
			return p, nil
		}
		risks = append(risks, Risk{Kind: RiskUncleanShutdown, Slot: n.Slot, Detail: "grastate seqno -1: the node did not shut down cleanly"})
	}
	r := n.Recovered
	switch {
	case r == nil:
		risks = append(risks, Risk{Kind: RiskPositionUnknown, Slot: n.Slot, Detail: "seqno unknown; run mariadbd --wsrep-recover on this node"})
	case r.UUID == ZeroUUID:
		p.Empty = true
		risks = append(risks, Risk{Kind: RiskNoData, Slot: n.Slot, Detail: "wsrep-recover found no Galera state"})
	case g != nil && r.UUID != g.UUID:
		risks = append(risks, Risk{Kind: RiskRecoveredUUIDMismatch, Slot: n.Slot,
			Detail: fmt.Sprintf("wsrep-recover UUID %s differs from grastate UUID %s", r.UUID, g.UUID)})
	case r.Seqno < 0:
		risks = append(risks, Risk{Kind: RiskPositionUnknown, Slot: n.Slot, Detail: "wsrep-recover returned seqno -1"})
	default:
		p.UUID, p.Seqno, p.Source = r.UUID, r.Seqno, SourceWsrepRecover
	}
	if p.SafeToBootstrap && p.Source != SourceGrastate {
		risks = append(risks, Risk{Kind: RiskSafeFlagMismatch, Slot: n.Slot, Detail: "safe_to_bootstrap: 1 without a saved seqno"})
	}
	return p, risks
}

// DiagnoseFullOutage evaluates the recovery evidence of every node of a
// cluster that has no Primary component, and proposes the node to bootstrap.
// It never executes anything and always requires approval.
//
// Rules:
//   - A node's position is its grastate seqno after a clean shutdown, else
//     the wsrep-recover position (whose UUID must match grastate), else
//     unknown. A node with the zero UUID holds no state and is never a
//     candidate, even with safe_to_bootstrap: 1.
//   - No candidate is proposed if positions of fewer than a strict majority
//     of the nodes are known, or if known positions carry different cluster
//     UUIDs. The user must collect more evidence or decide manually.
//   - Otherwise the candidate has the highest seqno. Ties prefer
//     safe_to_bootstrap: 1, then the lowest slot name (equal seqno means the
//     same committed state).
//   - A node with safe_to_bootstrap: 1 below the highest seqno is a risk:
//     the flag alone does not decide.
func DiagnoseFullOutage(nodes []NodeInfo) (Diagnosis, error) {
	if len(nodes) == 0 {
		return Diagnosis{}, fmt.Errorf("%w: no nodes", ErrBadRecoveryInput)
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if n.Slot == "" || seen[n.Slot] {
			return Diagnosis{}, fmt.Errorf("%w: empty or duplicate slot %q", ErrBadRecoveryInput, n.Slot)
		}
		seen[n.Slot] = true
		if n.Health.ReportsPrimary() && n.Health.StateUUID == ZeroUUID {
			return Diagnosis{}, fmt.Errorf("%w: %s", ErrZeroUUIDPrimary, n.Slot)
		}
		if n.Health.ReportsPrimary() {
			return Diagnosis{}, fmt.Errorf("%w: %s reports Primary", ErrNotFullOutage, n.Slot)
		}
	}
	if zeroStateOutage(nodes) {
		return diagnoseZeroUUID(nodes), nil
	}
	d := Diagnosis{RequiresApproval: true}
	var known []NodePosition
	uuids := map[string][]string{}
	for _, n := range nodes {
		p, risks := evaluate(n)
		d.Positions = append(d.Positions, p)
		d.Risks = append(d.Risks, risks...)
		if p.known() {
			known = append(known, p)
			uuids[p.UUID] = append(uuids[p.UUID], p.Slot)
		}
	}
	majority := len(nodes)/2 + 1
	if len(uuids) > 1 {
		var parts []string
		for _, u := range sortedKeys(uuids) {
			parts = append(parts, u+": "+strings.Join(uuids[u], ","))
		}
		d.Risks = append(d.Risks, Risk{Kind: RiskDivergingUUIDs, Detail: "nodes hold different cluster histories (" + strings.Join(parts, "; ") + ")"})
		d.Reasons = append(d.Reasons, "no candidate: diverging cluster UUIDs need a manual decision")
		return d, nil
	}
	if len(known) < majority {
		d.Reasons = append(d.Reasons, fmt.Sprintf("no candidate: positions known for %d of %d nodes, need %d; collect grastate.dat and wsrep-recover results first", len(known), len(nodes), majority))
		return d, nil
	}
	slices.SortStableFunc(known, func(a, b NodePosition) int {
		return cmp.Or(
			cmp.Compare(b.Seqno, a.Seqno),
			compareBool(b.SafeToBootstrap, a.SafeToBootstrap),
			cmp.Compare(a.Slot, b.Slot),
		)
	})
	c := known[0]
	d.Candidate = c.Slot
	d.Reasons = append(d.Reasons, fmt.Sprintf("%s has the highest seqno %d (source %s) of %d known positions", c.Slot, c.Seqno, c.Source, len(known)))
	var tied []string
	for _, p := range known[1:] {
		if p.Seqno == c.Seqno {
			tied = append(tied, p.Slot)
		}
		if p.SafeToBootstrap && p.Seqno < c.Seqno {
			d.Risks = append(d.Risks, Risk{Kind: RiskSafeFlagMismatch, Slot: p.Slot,
				Detail: fmt.Sprintf("safe_to_bootstrap: 1 but seqno %d is behind %s (%d)", p.Seqno, c.Slot, c.Seqno)})
		}
	}
	if len(tied) > 0 {
		if c.SafeToBootstrap {
			d.Reasons = append(d.Reasons, "tie at seqno "+strconv.FormatInt(c.Seqno, 10)+" broken by safe_to_bootstrap: 1")
		} else {
			d.Risks = append(d.Risks, Risk{Kind: RiskTie, Detail: fmt.Sprintf("%s ties with %s at seqno %d; lowest slot chosen", c.Slot, strings.Join(tied, ","), c.Seqno)})
		}
	}
	if len(known) < len(nodes) {
		d.Reasons = append(d.Reasons, fmt.Sprintf("positions of %d node(s) are unknown; the candidate may not hold the latest commit", len(nodes)-len(known)))
	}
	if !c.SafeToBootstrap {
		d.Reasons = append(d.Reasons, c.Slot+" has safe_to_bootstrap: 0 (normal after a crash of all nodes); the plan sets it after approval")
	}
	d.Plan = recoverySteps(nodes, c, false)
	return d, nil
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

func sortedKeys(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// recoverySteps builds the guided recovery order: stop every node and wait
// until no task runs, verify every identity marker on its host, re-check the
// candidate's grastate position and mark it safe, bootstrap the candidate
// (spec with the bootstrap flag, then one replica), start the other nodes
// one at a time, then drop the bootstrap flag from the candidate. The first
// step waits for the explicit approval (A06).
//
// The candidate's grastate.dat must have been read: mark-safe-to-bootstrap
// binds the bootstrap to the position the diagnosis saw (seqno -1 after a
// crash whose position came from wsrep-recover).
func recoverySteps(nodes []NodeInfo, c NodePosition, zero bool) Plan {
	order := []string{c.Slot}
	for _, n := range nodes {
		if n.Slot != c.Slot {
			order = append(order, n.Slot)
		}
	}
	mark := Position{UUID: c.UUID, Seqno: c.Seqno}
	for _, n := range nodes {
		if n.Slot == c.Slot && n.Grastate != nil {
			mark = Position{UUID: n.Grastate.UUID, Seqno: n.Grastate.Seqno}
		}
	}
	if zero {
		// The zero state stays as it is; only safe_to_bootstrap is set and
		// Galera creates a new state UUID at the bootstrap (E12).
		mark = Position{UUID: ZeroUUID, Seqno: -1}
	}
	var l stepList
	for i, s := range order {
		var pre []Precondition
		if i == 0 {
			pre = append(pre, Precondition{Kind: PreApproved})
		}
		l.add(Step{Kind: StepStopAll, Slot: s, Note: "scale the database service to 0 (idempotent)"}, pre...)
	}
	for _, s := range order {
		l.add(Step{Kind: StepAwaitStopped, Slot: s, Note: "no task of the service runs"})
	}
	for _, s := range order {
		l.add(Step{Kind: StepVerifyIdentity, Slot: s})
	}
	// A snapshot of every data directory precedes any change (owner
	// decision 2026-10-30); the agent refuses without free space.
	for _, s := range order {
		l.add(Step{Kind: StepSnapshotData, Slot: s, Note: "copy the data directory to a snapshot on the same volume (idempotent per name)"},
			Precondition{Kind: PreNoNodeRunning})
	}
	note := "set safe_to_bootstrap: 1 in the candidate's grastate.dat if it still holds the diagnosed position"
	if c.SafeToBootstrap {
		note = "re-check that the candidate's grastate.dat still holds the diagnosed position (already safe_to_bootstrap: 1)"
	}
	ms := Step{Kind: StepMarkSafe, Slot: c.Slot, Position: &mark, Note: note}
	if zero {
		ms.ZeroUUID = true
		ms.Note = "set safe_to_bootstrap: 1 in the candidate's zero-state grastate.dat; the bootstrap creates a new state UUID (recovered local seqno " +
			strconv.FormatInt(c.Seqno, 10) + ")"
	} else if c.Source == SourceWsrepRecover && mark.Seqno < 0 {
		// Lab E6: a bootstrap from "seqno: -1" (safe_to_bootstrap set, no
		// start position) started a new cluster state UUID; the joiners then
		// took a full SST from it. Writing the recovered seqno keeps the
		// cluster history.
		ms.Recovered = &Position{UUID: c.UUID, Seqno: c.Seqno}
		ms.Note += "; write the recovered seqno " + strconv.FormatInt(c.Seqno, 10) + " (wsrep-recover) so the bootstrap keeps the cluster history"
	}
	l.add(ms, Precondition{Kind: PreNoNodeRunning})
	l.add(Step{Kind: StepUpdateService, Slot: c.Slot, Note: "bootstrap spec with " + BootstrapFlag + ", still 0 replicas"},
		Precondition{Kind: PreNoNodeRunning})
	l.add(Step{Kind: StepStartNode, Slot: c.Slot, Note: "bootstrap the candidate"}, Precondition{Kind: PreNoNodeRunning})
	l.add(Step{Kind: StepWaitSynced, Slot: c.Slot, ClusterSize: 1})
	for i, s := range order[1:] {
		l.add(Step{Kind: StepStartNode, Slot: s, Note: "start; rejoins by IST or SST"}, synced(order[i], i+1))
		l.add(Step{Kind: StepWaitSynced, Slot: s, ClusterSize: i + 2})
	}
	all := make([]Precondition, len(order))
	for i, s := range order {
		all[i] = synced(s, len(order))
	}
	l.add(Step{Kind: StepUpdateService, Slot: c.Slot, Note: "steady-state spec without " + BootstrapFlag}, all...)
	l.add(Step{Kind: StepAwaitUpdate, Slot: c.Slot, Note: "Swarm replaced the bootstrap task"})
	l.add(Step{Kind: StepWaitSynced, Slot: c.Slot, ClusterSize: len(order)})
	l.add(Step{Kind: StepCheckState, ClusterSize: len(order),
		Note: "state check: every node Synced in one Primary component with the same non-zero state UUID"},
		Precondition{Kind: PreStateRecovered, ClusterSize: len(order)})
	kind := PlanFullOutageRecovery
	if zero {
		kind = PlanZeroUUIDRecovery
	}
	return Plan{Kind: kind, Steps: l.steps, RequiresApproval: true}
}

// RecoveryPlan attaches service specs, host IDs and node UUIDs to the
// diagnosis plan. Node identities are unchanged: recovery restarts the
// existing nodes on their existing volumes.
func RecoveryPlan(d Diagnosis, c Cluster, nodes []Node) (Plan, error) {
	if d.Candidate == "" || len(d.Plan.Steps) == 0 {
		return Plan{}, ErrNoCandidate
	}
	return attachPlan(d.Plan, d.Candidate, d.Positions, c, nodes)
}

// attachPlan binds the host, node and service facts to a plan skeleton.
// Every position's slot and every step's slot must name a node.
func attachPlan(skeleton Plan, candidate string, positions []NodePosition, c Cluster, nodes []Node) (Plan, error) {
	bySlot := map[string]Node{}
	for _, n := range nodes {
		bySlot[n.Slot] = n
	}
	p := skeleton
	p.ClusterID, p.Cluster, p.Nodes, p.RequiresApproval = c.ClusterID, c, slices.Clone(nodes), true
	p.Steps = slices.Clone(skeleton.Steps)
	for _, pos := range positions {
		if _, ok := bySlot[pos.Slot]; !ok {
			return Plan{}, fmt.Errorf("%w: no node for slot %s", ErrBadRecoveryInput, pos.Slot)
		}
	}
	for _, n := range nodes {
		s, err := BuildServiceSpec(c, n, SpecOptions{})
		if err != nil {
			return Plan{}, err
		}
		p.Desired = append(p.Desired, s)
	}
	bootDone := false
	for i := range p.Steps {
		s := &p.Steps[i]
		if s.Slot == "" {
			continue
		}
		n, ok := bySlot[s.Slot]
		if !ok {
			return Plan{}, fmt.Errorf("%w: no node for slot %s", ErrBadRecoveryInput, s.Slot)
		}
		s.HostID, s.NodeUUID, s.Volume = n.HostID, n.NodeUUID, n.Volume
		switch s.Kind {
		case StepStopAll, StepAwaitStopped, StepStartNode, StepAwaitUpdate:
			// These steps address the service by name; the spec is the
			// steady-state one.
			spec, err := BuildServiceSpec(c, n, SpecOptions{})
			if err != nil {
				return Plan{}, err
			}
			s.Spec = specPtr(spec)
			continue
		case StepUpdateService:
		default:
			continue
		}
		boot := s.Slot == candidate && !bootDone
		spec, err := BuildServiceSpec(c, n, SpecOptions{Bootstrap: boot})
		if err != nil {
			return Plan{}, err
		}
		if boot {
			// The service is stopped: the bootstrap spec keeps 0 replicas
			// (update-service never changes the replica count); the
			// following start-node step starts it.
			spec.Replicas = 0
		}
		bootDone = bootDone || boot
		s.Spec = specPtr(spec)
	}
	return p, nil
}

// WithSnapshot returns the plan with every snapshot-data step naming the
// snapshot. The manager derives the name from the operation ID so a
// repeated step finds its snapshot again.
func (p Plan) WithSnapshot(name string) Plan {
	p.Steps = slices.Clone(p.Steps)
	for i := range p.Steps {
		if p.Steps[i].Kind == StepSnapshotData {
			p.Steps[i].Snapshot = name
		}
	}
	return p
}
