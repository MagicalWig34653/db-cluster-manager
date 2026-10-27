// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/patch"
	"github.com/MagicalWig34653/db-cluster-manager/internal/vuln"
)

// patchConfig is the "manager.patch" section (X15, ADR 0023). The OS patch
// management is off unless "enabled" is true. It needs auth.enabled and the
// mTLS agent transport: the manager reaches the native patch agents
// ("agent patch") of enrolled hosts over the same CA and manager client
// certificate as the main agents, on its own port. Nothing is contacted until
// a host record is created, and every host starts report-only.
type patchConfig struct {
	Enabled bool `json:"enabled"`
	// AgentPort is the listen port of the patch agents (default 7445).
	AgentPort int `json:"agentPort"`
	// PollIntervalSeconds is how often the leader looks for hosts whose
	// report interval elapsed and for deny list changes (default 300,
	// minimum 60).
	PollIntervalSeconds int `json:"pollIntervalSeconds"`
}

const (
	defaultPatchPort     = 7445
	defaultPatchInterval = 300
)

func (c *patchConfig) on() bool { return c != nil && c.Enabled }

func (c *patchConfig) port() int {
	if c.AgentPort > 0 {
		return c.AgentPort
	}
	return defaultPatchPort
}

func (c *patchConfig) interval() time.Duration {
	if c.PollIntervalSeconds > 0 {
		return time.Duration(c.PollIntervalSeconds) * time.Second
	}
	return defaultPatchInterval * time.Second
}

func (c *patchConfig) validate(m *managerConfig, authEnabled bool) error {
	if !c.on() {
		return nil
	}
	if c.AgentPort < 0 || c.AgentPort > 65535 {
		return errors.New("manager.patch.agentPort must be between 1 and 65535")
	}
	if c.PollIntervalSeconds != 0 && c.PollIntervalSeconds < 60 {
		return errors.New("manager.patch.pollIntervalSeconds must be at least 60")
	}
	if m.AgentTransport == nil {
		return errors.New("manager.patch needs manager.agentTransport (the patch agents use the mTLS agent transport)")
	}
	if !authEnabled {
		return errors.New("manager.patch needs auth.enabled")
	}
	return nil
}

// newPatchService builds the patch service of one leadership: records in the
// replicated metastore, agent commands over mTLS fenced by the leader term,
// and the Galera guard backed by the cluster planner's guided host
// maintenance.
func (rt *managedRuntime) newPatchService() *patch.Service {
	store := rt.svc.Store()
	pg := &manager.PatchGaleraGuard{Store: store}
	return &patch.Service{
		KV:      vuln.MetastoreKV{Node: store.Node()},
		Journal: opjournal.New(),
		Agents:  &manager.PatchAgentDirectory{Dir: &manager.AgentDirectory{Store: store, PKI: rt.pki, NativePort: rt.patchCfg.port()}},
		Guard:   patch.GaleraGuard{Directory: pg, Coordinator: pg},
		Term: func() (uint64, error) {
			l := rt.svc.Current()
			if l == nil || !l.Valid() {
				return 0, manager.ErrNotLeader
			}
			return l.Term, nil
		},
		Notify: func(ctx context.Context, e notify.Event) {
			if rt.monitoring != nil {
				rt.monitoring.Notify(ctx, e)
			}
		},
		DenyList: func() (patch.DenyList, error) {
			dl, err := manager.CurrentDenyList(store)
			if err != nil {
				return patch.DenyList{}, err
			}
			return patch.DenyList{Revision: dl.Revision, Serials: dl.Serials, Digest: dl.Digest}, nil
		},
	}
}

// pollPatch is the scheduler's run of the current leadership's service:
// deliver a changed deny list first, then poll the hosts that are due.
func (rt *managedRuntime) pollPatch(ctx context.Context) error {
	svc := rt.patch.Load()
	if svc == nil {
		return nil
	}
	var errs []error
	if err := svc.DistributeDenyList(ctx); err != nil {
		errs = append(errs, fmt.Errorf("deny list: %w", err))
	}
	if _, err := svc.PollDue(ctx); err != nil {
		errs = append(errs, fmt.Errorf("poll: %w", err))
	}
	return errors.Join(errs...)
}
