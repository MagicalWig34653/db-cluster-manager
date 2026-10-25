// SPDX-License-Identifier: AGPL-3.0-only

package galera

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Approval-gated cluster operation kinds.
const (
	PlanScaleOut       PlanKind = "scale-out"
	PlanScaleIn        PlanKind = "scale-in"
	PlanRollingRestart PlanKind = "rolling-restart"
	PlanMinorUpgrade   PlanKind = "minor-upgrade"
)

// Planning refusals never authorize recovery or a relaxed quorum.
var (
	ErrUnsafeOperation = errors.New("galera: operation requires a fully Synced Primary component and a surviving majority")
	ErrMajorUpgrade    = errors.New("galera: major or series upgrade requires product-level approval and a curated upgrade path")
)

// OperationRequest changes one node at a time. Maintenance is a rolling
// restart; host maintenance with an operator-controlled pause is a separate flow.
type OperationRequest struct {
	Kind  PlanKind `json:"kind"`
	Slot  string   `json:"slot,omitempty"`
	Image string   `json:"image,omitempty"`
}

// PlanOperation plans only from fresh, consistent evidence. Runtime gates
// recheck health before every stop; no bootstrap or recovery step is emitted.
func PlanOperation(st ClusterState, req OperationRequest, pol ReplacementPolicy) (Plan, error) {
	if pol.Now.IsZero() {
		pol.Now = time.Now()
	}
	if (req.Kind != PlanMinorUpgrade && req.Image != "") || ((req.Kind == PlanMinorUpgrade || req.Kind == PlanRollingRestart) && req.Slot != "") {
		return Plan{}, ErrInvalidTopology
	}
	size := len(st.Nodes)
	if size < MinClusterSize || st.LastKnownSize != size || len(st.Cluster.Members) != size {
		return Plan{}, ErrUnsafeOperation
	}
	p := Plan{Kind: req.Kind, ClusterID: st.Cluster.ClusterID, Cluster: st.Cluster, RequiresApproval: true}
	p.Cluster.Members = slices.Clone(st.Cluster.Members)
	uuid := ""
	slots, identities, hosts := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, n := range st.Nodes {
		if _, err := BuildServiceSpec(st.Cluster, n.Node, SpecOptions{}); err != nil {
			return Plan{}, err
		}
		h := pol.fresh(n)
		if slots[n.Slot] || identities[n.NodeUUID] || !slices.Contains(st.Cluster.Members, n.Slot) || !h.InPrimary() || !h.DonorCapable() || h.ClusterSize != size || h.StateUUID == "" || (uuid != "" && uuid != h.StateUUID) || slices.Contains(pol.PausedHosts, n.HostID) {
			return Plan{}, ErrUnsafeOperation
		}
		slots[n.Slot], identities[n.NodeUUID], hosts[n.HostID], uuid = true, true, true, h.StateUUID
		p.Nodes = append(p.Nodes, n.Node)
	}
	var l stepList
	// Approval is the first gate, before even volume preparation.
	l.add(Step{Kind: StepVerifyDonor, Note: "explicit approval of this exact operation definition"}, Precondition{Kind: PreApproved})
	switch req.Kind {
	case PlanScaleOut:
		if req.Slot == "" || len(req.Slot) > 48 || slots[req.Slot] || !dnsLabel.MatchString(req.Slot) {
			return Plan{}, fmt.Errorf("%w: new unique slot required", ErrInvalidTopology)
		}
		demand := st.Nodes[0].Resources.Demand()
		if demand.MilliCPU <= 0 || demand.MemoryBytes <= 0 {
			return Plan{}, fmt.Errorf("%w: CPU and memory limits are required for scale-out", ErrInvalidTopology)
		}
		placement := pol.Placement
		placement.Excluded = append(slices.Clone(placement.Excluded), pol.PausedHosts...)
		pl := newPlacer(st.Hosts, placement, forCreate)
		for h := range hosts {
			pl.used[h] = true
		}
		if _, err := placement.validate(st.Hosts); err != nil {
			return Plan{}, err
		}
		h, err := pl.choose(req.Slot, st.Nodes[0].Resources.Demand())
		if err != nil {
			return Plan{}, err
		}
		existing := []string{}
		for u := range identities {
			existing = append(existing, u)
		}
		u, err := newUUIDSource(pol.NewUUID, existing...).next()
		if err != nil {
			return Plan{}, err
		}
		n := Node{Slot: req.Slot, HostID: h.ID, NodeUUID: u, Volume: VolumeName(p.Cluster.Name, req.Slot, u), Resources: st.Nodes[0].Resources}
		p.Cluster.Members = append(p.Cluster.Members, n.Slot)
		p.Nodes = append(p.Nodes, n)
		l.addNewVolume(p.Cluster, n)
		spec, err := BuildServiceSpec(p.Cluster, n, SpecOptions{})
		if err != nil {
			return Plan{}, err
		}
		l.add(nodeStep(StepCreateService, n, &spec), componentPre(p.Nodes[:size], size)...)
		waitComponent(&l, p.Nodes, size+1)
	case PlanScaleIn:
		i := slices.IndexFunc(p.Nodes, func(n Node) bool { return n.Slot == req.Slot })
		if i < 0 {
			return Plan{}, ErrUnknownNode
		}
		if size-1 < MinClusterSize || size-1 <= size/2 {
			return Plan{}, ErrUnsafeOperation
		}
		n := p.Nodes[i]
		spec, err := BuildServiceSpec(p.Cluster, n, SpecOptions{})
		if err != nil {
			return Plan{}, err
		}
		l.add(nodeStep(StepStopAll, n, &spec), componentPre(p.Nodes, size)...)
		l.add(nodeStep(StepAwaitStopped, n, &spec))
		p.Nodes = slices.Delete(p.Nodes, i, i+1)
		p.Cluster.Members = slices.DeleteFunc(p.Cluster.Members, func(s string) bool { return s == n.Slot })
		waitComponent(&l, p.Nodes, size-1)
		l.add(nodeStep(StepRemoveService, n, nil), componentPre(p.Nodes, size-1)...)
		l.add(nodeStep(StepRetainVolume, n, nil))
		p.Warnings = append(p.Warnings, "removed node volume retained for inspection; deletion requires a separate approved operation")
	case PlanMinorUpgrade:
		if err := validateUpgrade(p.Cluster.Image, req.Image); err != nil {
			return Plan{}, err
		}
		p.Cluster.Image = req.Image
	case PlanRollingRestart:
	default:
		return Plan{}, fmt.Errorf("%w: unknown operation kind", ErrInvalidTopology)
	}
	// Topology changes also roll the surviving services to converge their seed
	// addresses. Every stop has its own live gates, including after takeover.
	for _, n := range p.Nodes {
		if req.Kind == PlanScaleOut && n.Slot == req.Slot {
			continue
		}
		spec, err := BuildServiceSpec(p.Cluster, n, SpecOptions{})
		if err != nil {
			return Plan{}, err
		}
		if len(p.Nodes)-1 <= len(p.Nodes)/2 {
			return Plan{}, ErrUnsafeOperation
		}
		l.add(nodeStep(StepStopAll, n, &spec), componentPre(p.Nodes, len(p.Nodes))...)
		l.add(nodeStep(StepAwaitStopped, n, &spec))
		survivors := slices.DeleteFunc(slices.Clone(p.Nodes), func(o Node) bool { return o.Slot == n.Slot })
		waitComponent(&l, survivors, len(survivors))
		l.add(nodeStep(StepVerifyIdentity, n, nil))
		// The agent refuses replica changes in update-service. Update while
		// stopped, then use the explicit start command after identity checks.
		stoppedSpec := spec
		stoppedSpec.Replicas = 0
		l.add(nodeStep(StepUpdateService, n, &stoppedSpec), componentPre(survivors, len(survivors))...)
		l.add(nodeStep(StepStartNode, n, &spec), componentPre(survivors, len(survivors))...)
		l.add(nodeStep(StepAwaitUpdate, n, &spec))
		waitComponent(&l, p.Nodes, len(p.Nodes))
	}
	for _, n := range p.Nodes {
		s, err := BuildServiceSpec(p.Cluster, n, SpecOptions{})
		if err != nil {
			return Plan{}, err
		}
		p.Desired = append(p.Desired, s)
	}
	if len(p.Nodes)%2 == 0 {
		p.Warnings = append(p.Warnings, "even cluster size: a symmetric partition leaves no majority")
	}
	p.Steps = l.steps
	p.Warnings = append(p.Warnings, "direct client connections are interrupted; proxies exclude stopped nodes through health checks", "failed operations retain their state for inspection and resume; no automatic downgrade or bootstrap")
	return p, nil
}

func nodeStep(k StepKind, n Node, s *ServiceSpec) Step {
	return Step{Kind: k, Slot: n.Slot, HostID: n.HostID, NodeUUID: n.NodeUUID, Volume: n.Volume, Spec: s}
}
func componentPre(nodes []Node, size int) []Precondition {
	pre := []Precondition{{Kind: PreQuorum, ClusterSize: size}}
	for _, n := range nodes {
		pre = append(pre, synced(n.Slot, size))
	}
	return pre
}
func waitComponent(l *stepList, nodes []Node, size int) {
	l.add(Step{Kind: StepVerifyDonor, Note: "all remaining members Synced in one Primary component"}, componentPre(nodes, size)...)
}

var releaseImage = regexp.MustCompile(`^(.+):([0-9]+)\.([0-9]+)\.([0-9]+)@sha256:([a-f0-9]{64})$`)

// MariaDB series (for example 11.4 -> 11.8) need a separately curated path.
// This planner supports increasing patch releases within one series only.
func validateUpgrade(from, to string) error {
	a, b := releaseImage.FindStringSubmatch(from), releaseImage.FindStringSubmatch(to)
	if a == nil || b == nil {
		return fmt.Errorf("%w: source and target require explicit x.y.z tags and immutable sha256 digests", ErrInvalidSpec)
	}
	if a[2] != b[2] || a[3] != b[3] {
		return ErrMajorUpgrade
	}
	x, _ := strconv.ParseUint(a[4], 10, 32)
	y, err := strconv.ParseUint(b[4], 10, 32)
	if err != nil || a[1] != b[1] || !strings.HasSuffix(a[1], "mariadb") || y <= x {
		return fmt.Errorf("%w: only increasing patch releases of the same MariaDB repository are supported", ErrInvalidSpec)
	}
	return nil
}
