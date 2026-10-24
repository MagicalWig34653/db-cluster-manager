// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// TestDeleteSurvivesLeaderLossWhileVolumeBusy reproduces lab E4
// cleanuprestart (re-run A3): the deletion removed the services, Docker
// refused the first volume removal ("volume in use", HTTP 409) while Swarm
// removed the task containers, and the leading manager died during the
// retry backoff. The deletion failed after the restart. Now the new leader
// retries the volume step, which stays busy for two more attempts and is
// then free, and the deletion succeeds; the definition it resumes still
// carries the retry code.
func TestDeleteSurvivesLeaderLossWhileVolumeBusy(t *testing.T) {
	agents := newFakeAgents()
	var slow atomic.Bool // set: the runner's backoff waits until its leadership ends
	var mu sync.Mutex
	var volIDs []string // agent step IDs of the first volume step
	firstVol := ""
	busy := make(chan struct{})
	agents.answer = func(_ string, cmd agent.Command) (agent.Result, bool) {
		if cmd.Kind != agent.KindRemoveVolume {
			return agent.Result{}, false
		}
		mu.Lock()
		defer mu.Unlock()
		base, _, _ := cutAgentStep(cmd.StepID)
		if firstVol == "" {
			firstVol = base
		}
		if base != firstVol {
			return agent.Result{}, false
		}
		volIDs = append(volIDs, cmd.StepID)
		if len(volIDs) <= 3 {
			if len(volIDs) == 1 {
				slow.Store(true)
				close(busy)
			}
			return agent.Result{Status: agent.StatusFailed, Code: agent.CodeVolumeInUse,
				Message: "dockerclient: DELETE /v1.47/volumes/x: HTTP 409: volume is in use"}, true
		}
		return agent.Result{}, false
	}
	checkers := map[string]*RoutingChecker{}
	c := newClusterWith(t, 3, func(id string, cfg *Config) {
		checkers[id] = &RoutingChecker{Cluster: okChecker{}}
		cfg.Agents, cfg.Checker, cfg.AutoResume = agents, checkers[id], true
		cfg.RunnerTune = func(rc *runner.Config) {
			fastRunner(rc)
			rc.Sleep = func(ctx context.Context, d time.Duration) error {
				if slow.Load() {
					<-ctx.Done()
					return ctx.Err()
				}
				select {
				case <-time.After(5 * time.Millisecond):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}, func(id string, s *Service) { checkers[id].Store, checkers[id].Ops = s.Store(), &Operations{Svc: s} })
	l := c.leader()
	ops := seed(t, l, 3)
	ops.MaxRetries = 5
	create, err := ops.CreateCluster(adminSubject, createReq())
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, create.ID, runner.StatusSucceeded)
	ctl := &Control{Ops: ops, Swarms: &Swarms{}}
	ctx := context.Background()
	d, err := ctl.DeleteCluster(ctx, adminSubject, create.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	waitOpStatus(t, l, d.ID, runner.StatusPaused)
	if _, err := ctl.ApproveOperation(ctx, adminSubject, d.ID, d.Approval.Digest); err != nil {
		t.Fatal(err)
	}
	select {
	case <-busy:
	case <-time.After(waitTimeout):
		t.Fatal("no volume removal")
	}
	// The services are removed and the volume step waits to retry.
	waitFor(t, "the retry journaled as due", func() bool {
		jop, err := l.Node().Journal().Get(d.ID)
		if err != nil {
			return false
		}
		for _, s := range jop.Steps {
			if s.Retry {
				return true
			}
		}
		return false
	})
	rec := opRecord(t, l, d.ID)
	for _, s := range rec.Definition.Steps {
		if s.Command == agent.KindRemoveVolume && !slices.Equal(s.RetryCodes, []agent.Code{agent.CodeVolumeInUse}) {
			t.Fatalf("stored definition of %s lost its retry codes: %v", s.ID, s.RetryCodes)
		}
	}
	slow.Store(false)
	c.close(l) // the manager dies during the backoff
	nl := c.leader()
	final := waitOpStatus(t, nl, d.ID, runner.StatusSucceeded)
	waitFor(t, "cluster record deleted", func() bool {
		_, ok, _ := Get[ClusterRecord](nl.Store(), CollClusters, create.Cluster)
		return !ok
	})
	mu.Lock()
	ids := slices.Clone(volIDs)
	mu.Unlock()
	if len(ids) != 4 || len(slices.Compact(slices.Clone(ids))) != 4 {
		t.Fatalf("volume step attempts %v; want 4 distinct agent step IDs (busy 3 times, then free)", ids)
	}
	if final.EndedAt.IsZero() || final.View.Term == rec.View.Term {
		t.Errorf("deletion not finished by the new leader: ended %v term %d", final.EndedAt, final.View.Term)
	}
}

// cutAgentStep splits an agent step ID into the runner step ID and the
// attempt suffix ("05-x~a2" -> "05-x", "a2").
func cutAgentStep(id string) (string, string, bool) {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '~' {
			return id[:i], id[i+1:], true
		}
	}
	return id, "", false
}
