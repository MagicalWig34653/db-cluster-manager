// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galeraprobe"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// Health collection.
//
// The manager does not reach databases itself: the agent on a node's host
// reads the node's wsrep status locally (collect-health, over the socket in
// the node's volume or a port published on the host) and returns the raw
// variables; the manager derives galera.NodeHealth. Collection is a
// leader-only reconciler (HealthCollector) that stores timestamped
// observations in the cluster records; the inventory reports them as
// unknown once stale. Operation preconditions use live probes through the
// same path (AgentHealthProbe for galeraprobe.Checker), never the cache.

// healthOpID is the operation ID used for read-only health commands (the
// agent does not record read-only steps).
const healthOpID = "health"

// collectArgs returns the collect-health endpoint of a node.
func collectArgs(c ClusterRecord, n galera.Node, st Settings) (agent.CollectHealthArgs, error) {
	if port := c.MonitorPorts[n.Slot]; port > 0 {
		return agent.CollectHealthArgs{Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}, nil
	}
	sock := st.MonitorSocket
	if sock == "" {
		sock = DefaultMonitorSocket
	}
	if st.VolumeBase == "" || n.Volume == "" || !filepath.IsLocal(sock) {
		return agent.CollectHealthArgs{}, fmt.Errorf("%w: no health endpoint for %s/%s", ErrSettingsIncomplete, c.ID, n.Slot)
	}
	return agent.CollectHealthArgs{Socket: filepath.Join(st.VolumeBase, n.Volume, sock)}, nil
}

// probeNode sends collect-health to the node's agent under the leadership
// and derives the health. A transport failure is returned as an error; a
// database the agent cannot reach is an unhealthy observation.
func probeNode(ctx context.Context, l *Leadership, c ClusterRecord, n galera.Node, st Settings) (galera.NodeHealth, error) {
	args, err := collectArgs(c, n, st)
	if err != nil {
		return galera.NodeHealth{}, err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return galera.NodeHealth{}, err
	}
	res, err := l.Agents.Send(ctx, n.HostID, agent.Command{Term: l.Term, OpID: healthOpID, StepID: newID("h"),
		Kind: agent.KindCollectHealth, Args: raw})
	if err != nil {
		return galera.NodeHealth{}, err
	}
	switch {
	case res.Status == agent.StatusSucceeded:
		var out agent.HealthOutput
		if err := json.Unmarshal(res.Output, &out); err != nil {
			return galera.NodeHealth{}, fmt.Errorf("decode health output: %w", err)
		}
		return galera.HealthFromStatus(out.Status), nil
	case res.Code == agent.CodeDBUnavailable:
		return galera.NodeHealth{State: galera.HealthUnhealthy, Reasons: []string{"database unreachable: " + res.Message}}, nil
	}
	return galera.NodeHealth{}, fmt.Errorf("agent answered %s/%s: %s", res.Status, res.Code, res.Message)
}

// HealthCollector polls every cluster node's health through its agent while
// this replica leads.
type HealthCollector struct {
	Interval    time.Duration // default 15s
	Timeout     time.Duration // per node, default 5s
	Concurrency int           // parallel probes, default 8
	// Notify, if set, receives an event when a node stops or starts being
	// healthy (typically Monitoring.Notify). Events are recorded either way.
	Notify func(ctx context.Context, e notify.Event) bool
}

// Name implements Reconciler.
func (h HealthCollector) Name() string { return "health-collector" }

// Run implements Reconciler.
func (h HealthCollector) Run(ctx context.Context, l *Leadership) error {
	iv := h.Interval
	if iv <= 0 {
		iv = 15 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := h.CollectOnce(ctx, l); err != nil && ctx.Err() == nil {
			l.svc.log.Info("manager: health collection incomplete", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// CollectOnce probes every node once (bounded concurrency, per-node
// timeout) and stores the observations. A node whose agent does not answer
// gets an unknown observation with the reason.
func (h HealthCollector) CollectOnce(ctx context.Context, l *Leadership) error {
	store := l.svc.store
	st, _, err := Get[Settings](store, CollSettings, SettingsKey)
	if err != nil {
		return err
	}
	clusters, err := List[ClusterRecord](store, CollClusters)
	if err != nil {
		return err
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	conc := h.Concurrency
	if conc <= 0 {
		conc = 8
	}
	sem := make(chan struct{}, conc)
	type key struct{ cluster, slot string }
	var mu sync.Mutex
	obs := map[key]NodeObservation{}
	var wg sync.WaitGroup
	for _, c := range clusters {
		for _, n := range c.Value.Nodes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				pctx, cancel := context.WithTimeout(ctx, timeout)
				health, err := probeNode(pctx, l, c.Value, n, st.Value)
				cancel()
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					health = galera.NodeHealth{State: galera.HealthUnknown, Reasons: []string{"no answer from agent: " + err.Error()}}
				}
				mu.Lock()
				obs[key{c.Key, n.Slot}] = NodeObservation{Health: health, ObservedAt: l.svc.cfg.Now()}
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	var errs []error
	for _, c := range clusters {
		var changes []nodeTransition
		var prevState, nextState api.State
		_, _, err := Update(store, CollClusters, c.Key, func(cur ClusterRecord, exists bool) (ClusterRecord, error) {
			changes = changes[:0]
			prevState, nextState = "", ""
			if !exists {
				return cur, ErrNotFound
			}
			if cur.Observed != nil {
				prevState = cur.Observed.State
			}
			if cur.Health == nil {
				cur.Health = map[string]NodeObservation{}
			}
			var nodes []NodeObservation
			for _, n := range cur.Nodes {
				o, ok := obs[key{c.Key, n.Slot}]
				// Only nodes that were probed as this identity; a node
				// replaced meanwhile keeps no stale entry.
				if !ok || !sameNode(c.Value, cur, n) {
					continue
				}
				if prev, had := cur.Health[n.Slot]; had {
					if t, changed := transition(prev.Health, o.Health); changed {
						t.slot = n.Slot
						changes = append(changes, t)
					}
				}
				cur.Health[n.Slot] = o
				nodes = append(nodes, o)
			}
			if len(nodes) > 0 {
				cur.Observed = clusterObservation(cur, nodes[0].ObservedAt)
				nextState = cur.Observed.State
			}
			return cur, nil
		})
		if err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, fmt.Errorf("cluster %s: %w", c.Key, err))
		}
		if err == nil {
			h.report(ctx, l, c.Value, changes, clusterChange(prevState, nextState))
		}
	}
	return errors.Join(errs...)
}

// nodeTransition is a change of a node between healthy and not healthy.
type nodeTransition struct {
	slot      string
	recovered bool
	state     galera.HealthState
	reason    string
}

// transition reports whether a node changed between healthy and not
// healthy (unhealthy or unknown) from one observation to the next. Lab E4:
// a 30-40 s outage of a node raised no event and no notification.
func transition(prev, next galera.NodeHealth) (nodeTransition, bool) {
	was, is := prev.Healthy(), next.Healthy()
	if was == is {
		return nodeTransition{}, false
	}
	t := nodeTransition{recovered: is, state: next.State}
	if t.state == galera.HealthUnknown {
		t.state = "unknown"
	}
	if len(next.Reasons) > 0 {
		t.reason = next.Reasons[0]
	}
	return t, true
}

// Notification kinds of node and cluster health transitions.
const (
	KindNodeUnavailable = "health.node_unavailable"
	KindNodeRecovered   = "health.node_recovered"
	// KindClusterOutage: no node of the cluster is in a Primary component
	// and no majority is unknown, so the cluster serves no writes (lab E6).
	// Recovery is never started automatically (A06); the notification
	// points to the read-only outage diagnosis.
	KindClusterOutage = "health.cluster_outage"
	// KindClusterOutageEnded: a cluster left the unavailable state.
	KindClusterOutageEnded = "health.cluster_outage_ended"
)

// clusterOutageChange is a change of the aggregated cluster state into or
// out of unavailable.
type clusterOutageChange int

const (
	outageNone clusterOutageChange = iota
	outageStarted
	outageEnded
)

// clusterChange compares two aggregated cluster states. The first
// observation of a cluster (prev empty) is no transition, like for nodes.
// A change between unavailable and unknown is none either: unknown means
// the manager cannot tell (for example agents unreachable).
func clusterChange(prev, next api.State) clusterOutageChange {
	switch {
	case prev == "" || next == "" || prev == next:
		return outageNone
	case next == api.StateUnavailable && prev != api.StateUnknown:
		return outageStarted
	case prev == api.StateUnavailable && (next == api.StateHealthy || next == api.StateDegraded):
		return outageEnded
	}
	return outageNone
}

// report records an event for every transition and hands it to Notify
// (the monitoring dispatcher: UI inbox and configured channels).
func (h HealthCollector) report(ctx context.Context, l *Leadership, c ClusterRecord, changes []nodeTransition, cc clusterOutageChange) {
	if (len(changes) > 0 || cc != outageNone) && c.DeleteOp != "" {
		// Nodes going away during a deletion are expected.
		if op, ok, err := Get[OperationRecord](l.svc.store, CollOperations, c.DeleteOp); err == nil && ok && op.Value.EndedAt.IsZero() {
			return
		}
	}
	for _, t := range changes {
		sev, nsev, kind := api.SeverityWarning, notify.SeverityWarning, KindNodeUnavailable
		msg := fmt.Sprintf("node %s of cluster %s became %s", t.slot, c.Name, t.state)
		if t.recovered {
			sev, nsev, kind = api.SeverityInfo, notify.SeverityInfo, KindNodeRecovered
			msg = fmt.Sprintf("node %s of cluster %s is healthy again", t.slot, c.Name)
		} else if t.reason != "" {
			msg += ": " + t.reason
		}
		l.svc.recordEvent(c.Swarm, c.ID, sev, msg)
		if h.Notify == nil {
			continue
		}
		h.Notify(ctx, notify.Event{Time: l.svc.cfg.Now(), Severity: nsev, Kind: kind,
			Resource: "swarm/" + c.Swarm + "/cluster/" + c.ID + "/node/" + t.slot, Message: msg,
			Details: map[string]string{"swarm": c.Swarm, "cluster": c.ID, "node": t.slot, "state": string(t.state)}})
	}
	h.reportCluster(ctx, l, c, cc)
}

// reportCluster records and notifies the start or end of a full-cluster
// outage. It suggests the diagnosis; it never plans a recovery.
func (h HealthCollector) reportCluster(ctx context.Context, l *Leadership, c ClusterRecord, cc clusterOutageChange) {
	var sev api.Severity
	var nsev notify.Severity
	var kind, msg string
	switch cc {
	case outageStarted:
		sev, nsev, kind = api.SeverityCritical, notify.SeverityCritical, KindClusterOutage
		msg = fmt.Sprintf("cluster %s has no Primary component (full outage); run the outage diagnosis, recovery needs an approved plan", c.Name)
	case outageEnded:
		sev, nsev, kind = api.SeverityInfo, notify.SeverityInfo, KindClusterOutageEnded
		msg = fmt.Sprintf("cluster %s has a Primary component again", c.Name)
	default:
		return
	}
	l.svc.recordEvent(c.Swarm, c.ID, sev, msg)
	if h.Notify == nil {
		return
	}
	h.Notify(ctx, notify.Event{Time: l.svc.cfg.Now(), Severity: nsev, Kind: kind,
		Resource: "swarm/" + c.Swarm + "/cluster/" + c.ID, Message: msg,
		Details: map[string]string{"swarm": c.Swarm, "cluster": c.ID}})
}

func sameNode(before, now ClusterRecord, n galera.Node) bool {
	for _, b := range before.Nodes {
		if b.Slot == n.Slot {
			return b.NodeUUID == n.NodeUUID && b.HostID == n.HostID
		}
	}
	return false
}

// clusterObservation aggregates node observations: healthy when every node
// is healthy, degraded while a majority is in the Primary component,
// unknown when the missing majority includes unknown nodes, unavailable
// otherwise.
func clusterObservation(c ClusterRecord, at time.Time) *Observation {
	total := len(c.Nodes)
	healthy, inPrimary, unknown := 0, 0, 0
	for _, n := range c.Nodes {
		h, ok := c.Health[n.Slot]
		switch {
		case !ok || h.Health.State == galera.HealthUnknown:
			unknown++
		case h.Health.Healthy():
			healthy++
		}
		if ok && h.Health.InPrimary() {
			inPrimary++
		}
	}
	o := &Observation{At: at}
	switch {
	case total > 0 && healthy == total:
		o.State = api.StateHealthy
	case inPrimary*2 > total:
		o.State = api.StateDegraded
	case unknown > 0:
		o.State = api.StateUnknown
	default:
		o.State = api.StateUnavailable
	}
	o.Detail = fmt.Sprintf("%d/%d healthy, %d in Primary, %d unknown", healthy, total, inPrimary, unknown)
	return o
}

// AgentHealthProbe is the galeraprobe.HealthProber and galeraprobe.Topology
// of operation preconditions: an "address" names a node of an operation as
// "<operation ID>#<slot>", resolved from the operation record's plan nodes
// (so a replacement's new node is probed on its new host and volume), and
// every probe is a live collect-health through the current leadership.
type AgentHealthProbe struct {
	Svc *Service
}

var (
	_ galeraprobe.HealthProber = AgentHealthProbe{}
	_ galeraprobe.Topology     = AgentHealthProbe{}
)

func (p AgentHealthProbe) opNodes(opID string) (ClusterRecord, []galera.Node, error) {
	rec, ok, err := Get[OperationRecord](p.Svc.store, CollOperations, opID)
	if err != nil {
		return ClusterRecord{}, nil, err
	}
	if !ok || rec.Value.Cluster == "" {
		return ClusterRecord{}, nil, fmt.Errorf("%w: operation %s has no cluster", api.ErrNotFound, opID)
	}
	c, ok, err := Get[ClusterRecord](p.Svc.store, CollClusters, rec.Value.Cluster)
	if err != nil {
		return ClusterRecord{}, nil, err
	}
	if !ok {
		return ClusterRecord{}, nil, fmt.Errorf("%w: cluster %s", api.ErrNotFound, rec.Value.Cluster)
	}
	nodes := rec.Value.Nodes
	if len(nodes) == 0 {
		nodes = c.Value.Nodes
	}
	return c.Value, nodes, nil
}

// SlotAddress implements galeraprobe.Topology.
func (p AgentHealthProbe) SlotAddress(_ context.Context, opID, slot string) (string, error) {
	_, nodes, err := p.opNodes(opID)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Slot == slot {
			return opID + "#" + slot, nil
		}
	}
	return "", fmt.Errorf("%w: slot %s in operation %s", api.ErrNotFound, slot, opID)
}

// ClusterAddresses implements galeraprobe.Topology.
func (p AgentHealthProbe) ClusterAddresses(_ context.Context, opID string) ([]string, error) {
	_, nodes, err := p.opNodes(opID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, opID+"#"+n.Slot)
	}
	return out, nil
}

// Probe implements galeraprobe.HealthProber.
func (p AgentHealthProbe) Probe(ctx context.Context, addr string) (galera.NodeHealth, error) {
	opID, slot, ok := strings.Cut(addr, "#")
	if !ok {
		return galera.NodeHealth{}, fmt.Errorf("manager: bad probe address %q", addr)
	}
	l := p.Svc.Current()
	if l == nil || !l.Valid() {
		return galera.NodeHealth{}, ErrNotLeader
	}
	c, nodes, err := p.opNodes(opID)
	if err != nil {
		return galera.NodeHealth{}, err
	}
	st, _, err := Get[Settings](p.Svc.store, CollSettings, SettingsKey)
	if err != nil {
		return galera.NodeHealth{}, err
	}
	for _, n := range nodes {
		if n.Slot == slot {
			return probeNode(ctx, l, c, n, st.Value)
		}
	}
	return galera.NodeHealth{}, fmt.Errorf("%w: slot %s", api.ErrNotFound, slot)
}

// CollectOutageEvidence reads every node's grastate.dat through its agent
// (verifying the volume identity first) and adds the last health
// observation, for DiagnoseOutage. It is read-only and needs read
// permission on the cluster and the current leadership. A node whose agent
// does not answer is returned without grastate (the diagnosis then cannot
// recommend a bootstrap node).
func (o *Operations) CollectOutageEvidence(ctx context.Context, actor authz.Subject, clusterID string) ([]galera.NodeInfo, error) {
	return o.collectOutageEvidence(ctx, actor, clusterID, false)
}

// wsrepRecoverTimeout bounds one node's mariadbd --wsrep-recover (InnoDB
// crash recovery of the node's data); the agent transport allows 5 min.
const wsrepRecoverTimeout = 4 * time.Minute

// collectOutageEvidence reads grastate.dat on every node. With
// recoverPositions it also runs mariadbd --wsrep-recover (agent
// KindWsrepRecover) on every node whose grastate.dat has no saved seqno
// (-1 after a crash): that needs operate permission (audited), is refused
// while a node was last seen in a Primary component, and the agent refuses
// it while a container on the host mounts the volume.
func (o *Operations) collectOutageEvidence(ctx context.Context, actor authz.Subject, clusterID string, recoverPositions bool) ([]galera.NodeInfo, error) {
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	if d := o.Authorizer.Check(actor, authz.ActionRead, clusterRes(cur.Value.Swarm, clusterID)); !d.Allowed {
		return nil, &authz.DeniedError{Decision: d}
	}
	if recoverPositions {
		if err := o.authorize(actor, authz.ActionOperate, clusterRes(cur.Value.Swarm, clusterID)); err != nil {
			return nil, err
		}
		for slot, h := range cur.Value.Health {
			if h.Health.InPrimary() {
				return nil, fmt.Errorf("%w: %s was last seen in a Primary component", galera.ErrNotFullOutage, slot)
			}
		}
	}
	l, err := o.leadership()
	if err != nil {
		return nil, err
	}
	st, err := o.settings()
	if err != nil {
		return nil, err
	}
	c := cur.Value
	out := make([]galera.NodeInfo, len(c.Nodes))
	var wg sync.WaitGroup
	for i, n := range c.Nodes {
		out[i] = galera.NodeInfo{Slot: n.Slot}
		if h, ok := c.Health[n.Slot]; ok {
			out[i].Health = h.Health
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := json.Marshal(agent.ReadGrastateArgs{VolumeRoot: filepath.Join(st.VolumeBase, n.Volume),
				Expected: agent.Identity{InstallationID: st.InstallationID, ClusterID: c.ID, NodeUUID: n.NodeUUID}})
			if err != nil {
				return
			}
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			res, err := l.Agents.Send(pctx, n.HostID, agent.Command{Term: l.Term, OpID: "diagnose-" + clusterID, StepID: newID("g"),
				Kind: agent.KindReadGrastate, Args: raw})
			if err != nil || res.Status != agent.StatusSucceeded {
				return
			}
			var g agent.GrastateOutput
			if json.Unmarshal(res.Output, &g) == nil && g.Grastate != nil {
				out[i].Grastate = g.Grastate
			}
			if !recoverPositions || out[i].Grastate == nil || out[i].Grastate.Seqno >= 0 || out[i].Grastate.Empty() {
				return
			}
			uid, gid := c.Spec.DBOwner()
			raw, err = json.Marshal(agent.WsrepRecoverArgs{VolumeRoot: filepath.Join(st.VolumeBase, n.Volume), Volume: n.Volume,
				Expected: agent.Identity{InstallationID: st.InstallationID, ClusterID: c.ID, NodeUUID: n.NodeUUID},
				Image:    c.Spec.Image, UID: uid, GID: gid})
			if err != nil {
				return
			}
			rctx, rcancel := context.WithTimeout(ctx, wsrepRecoverTimeout)
			defer rcancel()
			res, err = l.Agents.Send(rctx, n.HostID, agent.Command{Term: l.Term, OpID: "diagnose-" + clusterID, StepID: newID("w"),
				Kind: agent.KindWsrepRecover, Args: raw})
			if err != nil || res.Status != agent.StatusSucceeded {
				msg := ""
				if err != nil {
					msg = err.Error()
				} else {
					msg = res.Message
				}
				o.Svc.log.Warn("manager: wsrep-recover failed", "cluster", clusterID, "slot", n.Slot, "err", msg)
				return
			}
			var w agent.WsrepRecoverOutput
			if json.Unmarshal(res.Output, &w) == nil && w.Position != nil {
				out[i].Recovered = w.Position
			}
		}()
	}
	wg.Wait()
	return out, nil
}

// DiagnoseOutageCollected collects the evidence and diagnoses (read-only;
// the resulting plan always requires explicit approval).
func (o *Operations) DiagnoseOutageCollected(ctx context.Context, actor authz.Subject, clusterID string) (galera.Diagnosis, []galera.NodeInfo, error) {
	return o.diagnoseOutage(ctx, actor, clusterID, false)
}

// DiagnoseOutageRecovering is DiagnoseOutageCollected plus mariadbd
// --wsrep-recover on every node without a saved seqno (see
// collectOutageEvidence); it needs operate permission.
func (o *Operations) DiagnoseOutageRecovering(ctx context.Context, actor authz.Subject, clusterID string) (galera.Diagnosis, []galera.NodeInfo, error) {
	return o.diagnoseOutage(ctx, actor, clusterID, true)
}

func (o *Operations) diagnoseOutage(ctx context.Context, actor authz.Subject, clusterID string, recoverPositions bool) (galera.Diagnosis, []galera.NodeInfo, error) {
	ev, err := o.collectOutageEvidence(ctx, actor, clusterID, recoverPositions)
	if err != nil {
		return galera.Diagnosis{}, nil, err
	}
	d, err := o.DiagnoseOutage(actor, clusterID, ev)
	return d, ev, err
}
