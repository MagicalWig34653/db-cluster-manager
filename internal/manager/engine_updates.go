// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
)

// EngineVersionSource discovers installed and available package versions
// without changing a host. The shipping manager has no configured source yet.
type EngineVersionSource interface {
	Discover(ctx context.Context, host HostRecord) (EngineVersionSet, error)
}

type EngineVersionSet struct {
	Current   string
	Available []string
}

var exactEngineVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var safeNodeID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func newerEngine(current, target string) bool {
	if !exactEngineVersion.MatchString(current) || !exactEngineVersion.MatchString(target) {
		return false
	}
	a, b := strings.Split(current, "."), strings.Split(target, ".")
	if a[0] != b[0] {
		return false
	} // major boundaries remain an owner decision
	for i := range a {
		x, xErr := strconv.ParseUint(a[i], 10, 32)
		y, yErr := strconv.ParseUint(b[i], 10, 32)
		if xErr != nil || yErr != nil {
			return false
		}
		if y != x {
			return y > x
		}
	}
	return false
}

func engineUpdateResource(swarmID, nodeID string) authz.Resource {
	return authz.Resource{Type: authz.ResourceEngineUpdate, Scope: authz.SwarmScope(swarmID), ID: nodeID}
}

// EngineUpdatePlan discovers the recorded engine version, an injected version
// catalog, Swarm manager quorum and the unchanged Galera maintenance plan.
func (c *Control) EngineUpdatePlan(ctx context.Context, actor authz.Subject, swarmID, nodeID, target string) (api.EngineUpdatePlan, error) {
	if err := c.Ops.authorize(actor, authz.ActionRead, engineUpdateResource(swarmID, nodeID)); err != nil {
		return api.EngineUpdatePlan{}, MapCommandError(err)
	}
	h, ok, err := Get[HostRecord](c.Ops.Svc.store, CollHosts, HostKey(swarmID, nodeID), Consistent())
	if err != nil {
		return api.EngineUpdatePlan{}, MapCommandError(err)
	}
	if !ok {
		return api.EngineUpdatePlan{}, api.ErrNotFound
	}
	plan := api.EngineUpdatePlan{Swarm: swarmID, Host: nodeID, CurrentVersion: h.Value.EngineVersion,
		TargetVersion: target, PinnedManager: h.Value.Role == "manager", ApprovalRequired: true,
		AvailableVersions: []string{}, Steps: []string{}, Warnings: []string{}, Blockers: []string{}, RollbackNotes: []string{
			"Docker package downgrades are installation-method specific and may be unsupported; preserve package metadata and a host backup before changing the engine.",
			"If the node cannot rejoin, keep it out of service, diagnose Swarm and Galera state, and use guided recovery; never bootstrap Galera automatically.",
		}}
	if c.EngineVersions == nil {
		plan.Blockers = append(plan.Blockers, "No read-only version source is configured for this host")
	} else {
		versions, sourceErr := c.EngineVersions.Discover(ctx, h.Value)
		if sourceErr != nil {
			plan.Blockers = append(plan.Blockers, "Installed and available engine versions could not be discovered")
		} else {
			if versions.Current != h.Value.EngineVersion {
				plan.Blockers = append(plan.Blockers, "Discovered engine version differs from Swarm inventory; resync before planning")
			}
			for _, v := range versions.Available {
				if exactEngineVersion.MatchString(v) && !slices.Contains(plan.AvailableVersions, v) {
					plan.AvailableVersions = append(plan.AvailableVersions, v)
				}
			}
			slices.Sort(plan.AvailableVersions)
		}
	}
	if !exactEngineVersion.MatchString(h.Value.EngineVersion) {
		plan.Blockers = append(plan.Blockers, "Current engine version is unknown or not an exact release")
	}
	if target == "" {
		plan.Blockers = append(plan.Blockers, "Select an available target version")
	} else if !slices.Contains(plan.AvailableVersions, target) || !newerEngine(h.Value.EngineVersion, target) {
		plan.Blockers = append(plan.Blockers, "Target must be a newer exact version from the version source")
	} else {
		matrix := swarm.DefaultMatrix()
		finding := matrix.Check(nodeID, target)
		plan.Compatibility = finding.Message
		// The seed uses a 29.* prefix, but only the exact recommended
		// 29.9.1 release has lab evidence. Do not turn that prefix into
		// permission to export an untested patch or minor release.
		if finding.Level != swarm.LevelOK || target != matrix.Recommended {
			plan.Blockers = append(plan.Blockers, "Target Docker version has no tested, supported compatibility entry")
		}
	}
	hosts, err := List[HostRecord](c.Ops.Svc.store, CollHosts, Consistent())
	if err != nil {
		return api.EngineUpdatePlan{}, MapCommandError(err)
	}
	managerCount, readyOutside := 0, 0
	for _, item := range hosts {
		other := item.Value
		if other.Swarm != swarmID {
			continue
		}
		if other.NodeID != nodeID && other.Maintenance != nil {
			plan.Blockers = append(plan.Blockers, "Another host is already in maintenance; update only one host at a time")
		}
		if other.Role == "manager" {
			managerCount++
			if other.NodeID != nodeID && other.State == "ready" && other.Availability == "active" {
				readyOutside++
			}
		}
	}
	if h.Value.Role == "manager" && readyOutside <= managerCount/2 {
		plan.Blockers = append(plan.Blockers, "Swarm manager quorum cannot be maintained outside this host")
	}
	if h.Value.State != "ready" || h.Value.Availability != "active" {
		plan.Blockers = append(plan.Blockers, "Host is not an active, ready Swarm node")
	}
	if h.Value.ObservedAt.IsZero() || c.Ops.Svc.cfg.Now().Sub(h.Value.ObservedAt) > 5*time.Minute || h.Value.ObservedAt.After(c.Ops.Svc.cfg.Now().Add(time.Minute)) {
		plan.Blockers = append(plan.Blockers, "Swarm host inventory is missing or older than five minutes")
	}
	if h.Value.Role != "manager" && h.Value.Role != "worker" {
		plan.Blockers = append(plan.Blockers, "Swarm role is unknown")
	}
	if !safeNodeID.MatchString(nodeID) {
		plan.Blockers = append(plan.Blockers, "Host ID cannot be safely represented in an operator command")
	}
	if !safeNodeID.MatchString(swarmID) {
		plan.Blockers = append(plan.Blockers, "Swarm ID cannot be safely represented in a runbook")
	}
	if h.Value.Maintenance == nil {
		pv, pErr := c.Ops.MaintenancePreview(actor, swarmID, nodeID, false)
		if pErr != nil {
			return api.EngineUpdatePlan{}, MapCommandError(pErr)
		}
		plan.Maintenance = pv
		if !pv.Safe {
			plan.Blockers = append(plan.Blockers, "Galera maintenance refuses this host; inspect the maintenance plan")
		}
		plan.Warnings = append(plan.Warnings, "Enter guided host maintenance and wait until ready before draining the Swarm node")
		plan.Blockers = append(plan.Blockers, "Enter guided host maintenance and wait until ready before runbook export")
	} else {
		plan.Maintenance = api.MaintenancePreview{Host: nodeID, Safe: c.Ops.maintenanceView(h.Value.Maintenance).Ready, Clusters: []api.ClusterMaintenancePlan{}}
		if !plan.Maintenance.Safe {
			plan.Blockers = append(plan.Blockers, "Host maintenance operations have not succeeded")
		}
	}
	if plan.PinnedManager {
		plan.Warnings = append(plan.Warnings, "This Swarm manager may host a pinned cluster-manager replica; confirm its placement and an alternate manager endpoint before proceeding")
	}
	plan.Steps = []string{"Review target compatibility and package source; preserve rollback material", "Enter guided host maintenance and wait for ready", "Operator drains this one Swarm node", "Operator updates Docker Engine and restarts the daemon or host as required", "Verify Swarm manager quorum, engine version and node readiness", "Operator restores Swarm active availability, then leaves guided host maintenance and verifies Galera Synced"}
	plan.Exportable = target != "" && len(plan.Blockers) == 0
	encoded, _ := json.Marshal(plan)
	plan.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
	return plan, nil
}

// ExportEngineRunbook binds operator approval to the current, rechecked plan.
// It never invokes Docker, a package manager, a service manager or an agent.
func (c *Control) ExportEngineRunbook(ctx context.Context, actor authz.Subject, swarmID, nodeID string, in api.EngineRunbookRequest) (api.EngineRunbook, error) {
	if err := c.Ops.authorize(actor, authz.ActionOperate, engineUpdateResource(swarmID, nodeID)); err != nil {
		return api.EngineRunbook{}, MapCommandError(err)
	}
	p, err := c.EngineUpdatePlan(ctx, actor, swarmID, nodeID, in.TargetVersion)
	if err != nil {
		return api.EngineRunbook{}, err
	}
	if p.Digest != in.Digest {
		return api.EngineRunbook{}, api.ErrApprovalMismatch
	}
	if !p.Exportable {
		return api.EngineRunbook{}, api.ErrConflict
	}
	if p.PinnedManager && !in.ConfirmPinnedManager {
		return api.EngineRunbook{}, api.ErrConflict
	}
	book := fmt.Sprintf("#!/bin/sh\n# cluster-manager Docker Engine update runbook; digest %s\n# This script prints operator steps and never upgrades a host.\ncat <<'RUNBOOK'\nHost: %s / %s\nDocker Engine: %s -> %s\n\n1. Keep the cluster-manager UI reachable from another replica; verify Swarm quorum and Galera maintenance ready.\n2. On a Swarm manager, run: docker node update --availability drain %s\n3. On the target host, use its approved package manager to install exactly Docker Engine %s. Restart Docker or the host if its packaging requires it.\n4. Verify docker version, docker node ls, and Galera health. If verification fails, keep the node drained and follow the rollback notes in the plan.\n5. On a Swarm manager, run: docker node update --availability active %s\n6. End guided host maintenance in cluster-manager; verify every affected Galera node is Synced.\nRUNBOOK\n", p.Digest, p.Swarm, p.Host, p.CurrentVersion, p.TargetVersion, p.Host, p.TargetVersion, p.Host)
	return api.EngineRunbook{Digest: p.Digest, Script: book}, nil
}
