// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// Drift detection: the desired state of a cluster (its record: node
// slots, hosts, service IDs) against what the Swarm reports for services
// labelled with the cluster. Every finding carries a proposed manual
// action; the manager changes nothing by itself, because "correcting"
// drift on stateful nodes (recreating, moving or removing a service) can
// destroy data or bypass the quorum rules. The DriftDetector reconciler
// re-checks periodically and records an event when the findings change.

// Drift finding kinds.
const (
	DriftMissingService    = "missing-service"
	DriftReplacedService   = "service-replaced-outside"
	DriftForeignLabels     = "foreign-labels"
	DriftPlacementChanged  = "placement-changed"
	DriftStopped           = "stopped"
	DriftNotRunning        = "not-running"
	DriftUnexpectedService = "unexpected-service"
)

// DriftObservation is the last drift check stored on a cluster record.
type DriftObservation struct {
	CheckedAt time.Time `json:"checkedAt"`
	Findings  int       `json:"findings"`
	Digest    string    `json:"digest"`
}

// CompareDrift compares a cluster record with the Swarm services labelled
// with its ID. inMaintenance lists hosts in maintenance (stopped nodes on
// them are expected) and busy reports an unfinished operation (stopped or
// starting nodes are then expected as well).
func CompareDrift(c ClusterRecord, services []agent.Object, installation string, inMaintenance []string, busy bool, now time.Time) api.DriftReport {
	rep := api.DriftReport{Cluster: c.ID, CheckedAt: now}
	byName := map[string]agent.Object{}
	for _, s := range services {
		if s.Labels[agent.LabelJob] != "" {
			continue // backup, restore and inspection jobs come and go
		}
		byName[s.Name] = s
	}
	expected := map[string]bool{}
	for _, n := range c.Nodes {
		name := galera.ServiceName(c.Name, n.Slot)
		expected[name] = true
		s, ok := byName[name]
		add := func(kind, detail, action string) {
			rep.Findings = append(rep.Findings, api.DriftFinding{Kind: kind, Slot: n.Slot, Service: name, Detail: detail, Action: action})
		}
		if !ok {
			add(DriftMissingService, "no service of this node exists in the Swarm",
				"find out why the service disappeared; replace the node through the manager (POST /clusters/{id}/nodes/{slot}/replace) only after checking its volume")
			continue
		}
		if want := c.ServiceID[n.Slot]; want != "" && s.ID != want {
			add(DriftReplacedService, fmt.Sprintf("service ID %s, recorded %s", s.ID, want),
				"the service was recreated outside the manager; inspect it, and replace the node through the manager instead of editing services by hand")
		}
		if installation != "" && s.Labels[agent.LabelInstallation] != installation {
			add(DriftForeignLabels, fmt.Sprintf("label %s=%q", agent.LabelInstallation, s.Labels[agent.LabelInstallation]),
				"the agents refuse services without this installation's label; restore the label or replace the node")
		}
		if want := "node.id==" + n.HostID; !slices.Contains(s.Constraints, want) {
			add(DriftPlacementChanged, fmt.Sprintf("constraints %v, want %s", s.Constraints, want),
				"restore the pinned placement; a node must not move away from its local volume")
		}
		expectedDown := busy || slices.Contains(inMaintenance, n.HostID)
		switch {
		case s.Replicas == 0 && !expectedDown:
			add(DriftStopped, "scaled to 0 replicas outside maintenance or an operation",
				"if this was not intended, start the node again by a maintenance end on its host or check the last operation")
		case s.Replicas > 0 && s.RunningTasks != nil && *s.RunningTasks == 0 && !expectedDown:
			add(DriftNotRunning, "no running task",
				"check the node's health and task errors; replace it if it failed")
		}
	}
	var extra []string
	for name := range byName {
		if !expected[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		rep.Findings = append(rep.Findings, api.DriftFinding{Kind: DriftUnexpectedService, Service: name,
			Detail: "service carries this cluster's label but is not one of its nodes",
			Action: "review it; the manager neither adopts nor removes it"})
	}
	rep.InSync = len(rep.Findings) == 0
	return rep
}

func driftDigest(rep api.DriftReport) string {
	var b strings.Builder
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, "%s|%s|%s|%s\n", f.Kind, f.Slot, f.Service, f.Detail)
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:8])
}

// checkDrift reads the Swarm and compares (no permission check).
func (o *Operations) checkDrift(ctx context.Context, c ClusterRecord) (api.DriftReport, error) {
	if o.Swarms == nil || o.Swarms.Dial == nil {
		return api.DriftReport{}, errNoDial
	}
	sw, ok, err := Get[SwarmRecord](o.Svc.store, CollSwarms, c.Swarm)
	if err != nil {
		return api.DriftReport{}, err
	}
	if !ok {
		return api.DriftReport{}, fmt.Errorf("%w: swarm %q", api.ErrNotFound, c.Swarm)
	}
	conn, err := o.Swarms.Dial(sw.Value.DockerHost)
	if err != nil {
		return api.DriftReport{}, err
	}
	services, err := conn.ListByLabel(ctx, agent.KindService, galera.LabelCluster, c.ID)
	if err != nil {
		return api.DriftReport{}, err
	}
	st, _ := o.settings()
	paused, err := o.pausedHosts(c.Swarm)
	if err != nil {
		return api.DriftReport{}, err
	}
	busy, err := o.unfinishedFor(c.ID)
	if err != nil {
		return api.DriftReport{}, err
	}
	return CompareDrift(c, services, st.InstallationID, paused, busy != "", o.Svc.cfg.Now()), nil
}

// ClusterDrift compares one cluster with its Swarm (read on the cluster).
func (o *Operations) ClusterDrift(ctx context.Context, actor authz.Subject, clusterID string) (api.DriftReport, error) {
	cur, ok, err := Get[ClusterRecord](o.Svc.store, CollClusters, clusterID)
	if err != nil {
		return api.DriftReport{}, err
	}
	if !ok {
		return api.DriftReport{}, fmt.Errorf("%w: cluster %q", api.ErrNotFound, clusterID)
	}
	if err := o.authorize(actor, authz.ActionRead, clusterRes(cur.Value.Swarm, cur.Value.ID)); err != nil {
		return api.DriftReport{}, err
	}
	return o.checkDrift(ctx, cur.Value)
}

// ClusterDrift implements api.Maintenance.
func (c *Control) ClusterDrift(ctx context.Context, actor authz.Subject, clusterID string) (api.DriftReport, error) {
	ops := *c.Ops
	ops.Swarms = c.swarms()
	rep, err := ops.ClusterDrift(ctx, actor, clusterID)
	return rep, MapCommandError(err)
}

// DriftDetector checks every cluster periodically while this replica
// leads and records an event when a cluster's findings change.
type DriftDetector struct {
	Swarms   *Swarms
	Interval time.Duration // default 5 minutes
}

// Name implements Reconciler.
func (DriftDetector) Name() string { return "drift-detector" }

// Run implements Reconciler.
func (d DriftDetector) Run(ctx context.Context, l *Leadership) error {
	iv := d.Interval
	if iv <= 0 {
		iv = 5 * time.Minute
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := d.CheckOnce(ctx, l); err != nil && ctx.Err() == nil {
			l.svc.log.Info("manager: drift check incomplete", "err", err)
		}
	}
}

// CheckOnce checks every cluster once.
func (d DriftDetector) CheckOnce(ctx context.Context, l *Leadership) error {
	sw := *d.Swarms
	sw.Svc = l.svc
	ops := &Operations{Svc: l.svc, Swarms: &sw}
	clusters, err := List[ClusterRecord](l.svc.store, CollClusters)
	if err != nil {
		return err
	}
	var firstErr error
	for _, c := range clusters {
		if c.Value.DeleteOp != "" {
			continue
		}
		rep, err := ops.checkDrift(ctx, c.Value)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("cluster %s: %w", c.Key, err)
			}
			continue
		}
		digest := driftDigest(rep)
		var changed bool
		if _, _, err := Update(l.svc.store, CollClusters, c.Key, func(cur ClusterRecord, exists bool) (ClusterRecord, error) {
			if !exists {
				return cur, ErrNotFound
			}
			changed = cur.Drift == nil || cur.Drift.Digest != digest
			cur.Drift = &DriftObservation{CheckedAt: rep.CheckedAt, Findings: len(rep.Findings), Digest: digest}
			return cur, nil
		}); err != nil {
			continue
		}
		if !changed {
			continue
		}
		if rep.InSync {
			l.svc.recordEvent(c.Value.Swarm, c.Value.ID, api.SeverityInfo, fmt.Sprintf("cluster %s matches its Swarm services again", c.Value.Name))
			continue
		}
		kinds := make([]string, 0, len(rep.Findings))
		for _, f := range rep.Findings {
			kinds = append(kinds, f.Kind+" "+f.Service)
		}
		l.svc.recordEvent(c.Value.Swarm, c.Value.ID, api.SeverityWarning,
			fmt.Sprintf("cluster %s drifted from its desired state: %s (see GET /clusters/%s/drift; nothing is changed automatically)",
				c.Value.Name, strings.Join(kinds, ", "), c.Value.ID))
	}
	return firstErr
}
