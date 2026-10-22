// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
)

// AgentStatusSource is the collect.StatusSource of the monitoring service:
// it reads a node's wsrep status through the node's agent (collect-health,
// read-only) under the current leadership, like HealthCollector. It
// returns the raw variables; the Galera collector derives the metrics.
type AgentStatusSource struct {
	Svc *Service
}

var _ collect.StatusSource = AgentStatusSource{}

// Status implements collect.StatusSource.
func (a AgentStatusSource) Status(ctx context.Context, t collect.Target) (map[string]string, time.Time, error) {
	l := a.Svc.Current()
	if l == nil || !l.Valid() {
		return nil, time.Time{}, ErrNotLeader
	}
	c, ok, err := Get[ClusterRecord](a.Svc.store, CollClusters, t.Cluster)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !ok {
		return nil, time.Time{}, collect.ErrNoObservation
	}
	st, _, err := Get[Settings](a.Svc.store, CollSettings, SettingsKey)
	if err != nil {
		return nil, time.Time{}, err
	}
	for _, n := range c.Value.Nodes {
		if n.Slot != t.Node {
			continue
		}
		args, err := collectArgs(c.Value, n, st.Value)
		if err != nil {
			return nil, time.Time{}, err
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, time.Time{}, err
		}
		res, err := l.Agents.Send(ctx, n.HostID, agent.Command{Term: l.Term, OpID: healthOpID, StepID: newID("m"),
			Kind: agent.KindCollectHealth, Args: raw})
		if err != nil {
			return nil, time.Time{}, err
		}
		if res.Status != agent.StatusSucceeded {
			return nil, time.Time{}, fmt.Errorf("agent answered %s/%s", res.Status, res.Code)
		}
		var out agent.HealthOutput
		if err := json.Unmarshal(res.Output, &out); err != nil {
			return nil, time.Time{}, fmt.Errorf("decode health output: %w", err)
		}
		return out.Status, a.Svc.cfg.Now(), nil
	}
	return nil, time.Time{}, collect.ErrNoObservation
}
