// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/fencing"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Leader-only execution.
//
// Leadership is per metastore term. On gaining it the Service runs a Raft
// barrier, creates a fresh runner, takes over every unfinished journal
// operation (runner.TakeOver: Running steps become Unknown, nothing is
// sent), optionally resumes them, and starts the reconcilers. When the node
// stops being leader, or its term changes, the Leadership's context is
// cancelled, every goroutine it started is awaited, and its agent client
// refuses all further commands locally. A later term starts over from the
// replicated state.

// Leadership is one term of leadership of this replica.
type Leadership struct {
	Term uint64
	// Runner executes operations for this term only.
	Runner *runner.Runner
	// Agents is the fenced agent client of this term: it refuses every
	// command once the term is over.
	Agents runner.AgentClient
	// Adopted lists operations taken over when the term started.
	Adopted []string

	svc    *Service
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Context ends when the leadership ends.
func (l *Leadership) Context() context.Context { return l.ctx }

// Valid reports whether the leadership is still current.
func (l *Leadership) Valid() bool {
	return l.ctx.Err() == nil && l.svc.node.IsLeader() && l.svc.node.Term() == l.Term
}

// Go runs fn in a goroutine bound to the leadership; stepping down cancels
// its context and waits for it.
func (l *Leadership) Go(fn func(ctx context.Context)) bool {
	l.svc.mu.Lock()
	defer l.svc.mu.Unlock()
	if l.ctx.Err() != nil {
		return false
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		fn(l.ctx)
	}()
	return true
}

// errNoAgents is returned when no agent transport is configured. It wraps
// agent.ErrInvalidCommand so the runner treats it as a rejection without
// effect (nothing was sent).
var errNoAgents = fmt.Errorf("%w: manager has no agent transport configured", agent.ErrInvalidCommand)

// fencedAgents refuses commands outside the leadership term.
type fencedAgents struct {
	inner runner.AgentClient
	l     *Leadership
}

func (f fencedAgents) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	if !f.l.Valid() || cmd.Term != f.l.Term {
		return agent.Result{}, fmt.Errorf("%w: manager is not the leader of term %d; command not sent", fencing.ErrStaleTerm, cmd.Term)
	}
	if f.inner == nil {
		return agent.Result{}, errNoAgents
	}
	return f.inner.Send(ctx, target, cmd)
}

func (s *Service) leaderLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		s.checkLeadership(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) checkLeadership(ctx context.Context) {
	isLeader := s.node.IsLeader()
	if cur := s.Current(); cur != nil && (!isLeader || s.node.Term() != cur.Term) {
		s.stepDown("leadership lost")
	}
	if isLeader && s.Current() == nil && ctx.Err() == nil {
		if err := s.becomeLeader(ctx); err != nil {
			s.log.Warn("manager: leadership start failed; retrying", "err", err)
		}
	}
}

func (s *Service) becomeLeader(parent context.Context) error {
	term := s.node.Term()
	if err := s.node.Barrier(s.cfg.BarrierTimeout); err != nil {
		return err
	}
	if !s.node.IsLeader() || s.node.Term() != term {
		return fmt.Errorf("%w: term changed during barrier", ErrNotLeader)
	}
	ctx, cancel := context.WithCancel(parent)
	l := &Leadership{Term: term, svc: s, ctx: ctx, cancel: cancel}
	l.Agents = fencedAgents{inner: s.cfg.Agents, l: l}
	rc := runner.Config{
		Leader: s.node, Journal: runner.MetastoreJournal{Node: s.node}, Agents: l.Agents,
		Checker: s.cfg.Checker, Now: s.cfg.Now, BarrierTimeout: s.cfg.BarrierTimeout,
	}
	if s.cfg.RunnerTune != nil {
		s.cfg.RunnerTune(&rc)
	}
	r, err := runner.New(rc)
	if err != nil {
		cancel()
		return err
	}
	l.Runner = r
	adopted, err := r.TakeOver(func(id string) (runner.Operation, bool) {
		rec, ok, err := Get[OperationRecord](s.store, CollOperations, id)
		if err != nil || !ok {
			return runner.Operation{}, false
		}
		return rec.Value.Definition, true
	})
	if err != nil {
		cancel()
		return fmt.Errorf("manager: take over: %w", err)
	}
	l.Adopted = adopted

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return errors.New("manager: closed")
	}
	s.current = l
	hooks := append([]func(*Leadership){}, s.hooks...)
	s.mu.Unlock()

	s.log.Info("manager: leadership started", "term", term, "adopted", len(adopted))
	s.recordEvent("", "", api.SeverityInfo, fmt.Sprintf("manager %s leads term %d; %d unfinished operation(s) taken over", s.cfg.NodeID, term, len(adopted)))
	for _, id := range adopted {
		if v, err := r.Snapshot(id); err == nil {
			s.saveView(v)
		}
		if s.cfg.AutoResume {
			id := id
			l.Go(func(ctx context.Context) {
				v, err := r.Resume(ctx, id)
				if err != nil && v.ID == "" {
					s.log.Info("manager: operation not resumed", "op", id, "err", err)
					if snap, serr := r.Snapshot(id); serr == nil {
						s.saveView(snap)
					}
					return
				}
				s.saveView(v)
			})
		}
	}
	for _, rec := range s.cfg.Reconcilers {
		rec := rec
		l.Go(func(ctx context.Context) {
			if err := rec.Run(ctx, l); err != nil && ctx.Err() == nil {
				s.log.Warn("manager: reconciler stopped", "name", rec.Name(), "err", err)
			}
		})
	}
	l.Go(func(ctx context.Context) { s.syncViews(ctx, l) })
	for _, h := range hooks {
		h(l)
	}
	return nil
}

func (s *Service) stepDown(reason string) {
	s.mu.Lock()
	l := s.current
	s.current = nil
	hooks := append([]func(*Leadership){}, s.hooks...)
	if l != nil {
		l.cancel()
	}
	s.mu.Unlock()
	if l == nil {
		return
	}
	l.wg.Wait()
	s.log.Info("manager: leadership ended", "term", l.Term, "reason", reason)
	for _, h := range hooks {
		h(nil)
	}
}

// syncViews writes changed operation views to the store while leading.
func (s *Service) syncViews(ctx context.Context, l *Leadership) {
	last := map[string]string{}
	t := time.NewTicker(s.cfg.ViewSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, v := range l.Runner.List() {
			b, _ := json.Marshal(v)
			if last[v.ID] == string(b) {
				continue
			}
			if s.saveView(v) == nil {
				last[v.ID] = string(b)
			}
		}
	}
}

func terminal(st runner.Status) bool {
	return st == runner.StatusSucceeded || st == runner.StatusAborted || st == runner.StatusFailed
}

// saveView stores an operation's runner view in its record and records an
// event when the operation first reaches a terminal status.
func (s *Service) saveView(v runner.View) error {
	if v.ID == "" {
		return nil
	}
	var ended bool
	rec, _, err := Update(s.store, CollOperations, v.ID, func(cur OperationRecord, exists bool) (OperationRecord, error) {
		if !exists {
			return cur, ErrNotFound
		}
		ended = false
		vc := v
		cur.View = &vc
		cur.UpdatedAt = s.cfg.Now()
		if terminal(v.Status) && cur.EndedAt.IsZero() {
			cur.EndedAt = cur.UpdatedAt
			ended = true
		}
		return cur, nil
	})
	if err != nil {
		return err
	}
	if ended {
		sev := api.SeverityInfo
		if v.Status != runner.StatusSucceeded {
			sev = api.SeverityWarning
		}
		s.recordEvent(rec.Swarm, rec.Cluster, sev, fmt.Sprintf("operation %s (%s) %s", v.ID, v.Kind, v.Status))
		if v.Status == runner.StatusSucceeded && rec.Kind == OpClusterDelete {
			// Here rather than in a start callback: an approved deletion
			// usually finishes in a resumed run (approvals.go), which has
			// no callback.
			s.finishClusterDelete(rec)
		}
	}
	return nil
}

// recordEvent stores an event (best effort: events are informational).
func (s *Service) recordEvent(swarm, cluster string, sev api.Severity, msg string) {
	now := s.cfg.Now()
	key := timeKey(now)
	op, err := PutOp(CollEvents, key, EventRecord{ID: key, Time: now, Swarm: swarm, Cluster: cluster, Severity: sev, Message: msg}, IfVersion(0))
	if err != nil {
		return
	}
	if _, err := s.store.Txn(op); err != nil && !errors.Is(err, metastore.ErrNotLeader) {
		s.log.Warn("manager: event not stored", "err", err)
	}
}
