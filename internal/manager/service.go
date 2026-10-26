// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// Reconciler is a leader-only background task. Run is started when this
// replica gains leadership and must return promptly when ctx ends (on loss
// of leadership or shutdown). It receives the Leadership it runs under; it
// must not send agent commands except through Leadership.Agents.
type Reconciler interface {
	Name() string
	Run(ctx context.Context, l *Leadership) error
}

// Config configures a Service.
type Config struct {
	// NodeID is this replica's metastore ID.
	NodeID string
	// DataDir holds the metastore (Raft log, snapshots). Empty keeps state
	// in memory (tests only).
	DataDir string
	// BindAddr/AdvertiseAddr configure the metastore TCP transport (no TLS:
	// loopback or isolated lab networks only). Transport replaces them in
	// tests.
	BindAddr      string
	AdvertiseAddr string
	Transport     metastore.Transport
	// PeerTLS makes the BindAddr transport mutually authenticated TLS 1.3
	// between replicas (metastore.PeerTLS).
	PeerTLS *metastore.PeerTLS
	// Keyring, when set, distributes the data key through the replicated
	// keyring (keyring.go): a new leader bootstraps or refreshes it before
	// anything else and hands leadership over when it cannot unwrap the
	// current key.
	Keyring *Keyring
	// Peers is the initial voting membership including this node. Empty
	// means single-instance mode: a one-member group of this node.
	Peers []metastore.Peer
	// Bootstrap writes the initial membership on first start (ignored when
	// state exists). Single-instance mode always bootstraps.
	Bootstrap bool
	// Tune adjusts the metastore configuration (timeouts) before start.
	Tune func(*metastore.Config)

	// Agents delivers agent commands. Nil refuses every command.
	Agents runner.AgentClient
	// Checker evaluates step preconditions; nil fails them closed.
	Checker runner.Checker
	// RunnerTune adjusts each new runner's configuration.
	RunnerTune func(*runner.Config)
	// AutoResume resumes taken-over operations automatically. The runner
	// re-queries in-doubt steps before anything new is sent.
	AutoResume bool
	// Reconcilers run while this replica leads.
	Reconcilers []Reconciler

	// PollInterval is the leadership check interval (default 100ms).
	PollInterval time.Duration
	// ViewSyncInterval is how often operation views are written to the
	// store while operations run (default 1s).
	ViewSyncInterval time.Duration
	// BarrierTimeout bounds the barrier on gaining leadership (default 5s).
	BarrierTimeout time.Duration

	Logger *slog.Logger
	Now    func() time.Time
}

// Service is one manager replica.
type Service struct {
	cfg   Config
	node  *metastore.Node
	store *Store
	log   *slog.Logger

	mu      sync.Mutex
	current *Leadership
	hooks   []func(*Leadership)

	stop    context.CancelFunc
	loopWG  sync.WaitGroup
	started bool
	closed  bool
}

// New starts the embedded metastore node. Call Start to run the leader loop.
func New(cfg Config) (*Service, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("manager: NodeID required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	if cfg.ViewSyncInterval <= 0 {
		cfg.ViewSyncInterval = time.Second
	}
	if cfg.BarrierTimeout <= 0 {
		cfg.BarrierTimeout = 5 * time.Second
	}
	mc := metastore.Config{
		ID: cfg.NodeID, Transport: cfg.Transport, BindAddr: cfg.BindAddr, AdvertiseAddr: cfg.AdvertiseAddr,
		DataDir: cfg.DataDir, Peers: cfg.Peers, Bootstrap: cfg.Bootstrap, TLS: cfg.PeerTLS,
	}
	if len(mc.Peers) == 0 {
		addr := cfg.AdvertiseAddr
		if addr == "" {
			addr = cfg.BindAddr
		}
		if cfg.Transport != nil {
			addr = string(cfg.Transport.LocalAddr())
		}
		if addr == "" {
			return nil, errors.New("manager: single-instance mode needs an advertise or bind address")
		}
		mc.Peers = []metastore.Peer{{ID: cfg.NodeID, Address: addr}}
		mc.Bootstrap = true
	}
	if cfg.Tune != nil {
		cfg.Tune(&mc)
	}
	node, err := metastore.NewNode(mc)
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, node: node, store: NewStore(node), log: cfg.Logger}
	if cfg.Keyring != nil {
		cfg.Keyring.Attach(s.store)
	}
	return s, nil
}

// Store returns the replicated store.
func (s *Service) Store() *Store { return s.store }

// Node returns the metastore node.
func (s *Service) Node() *metastore.Node { return s.node }

// IsLeader reports whether this replica is the metastore leader AND has
// completed its leadership start (barrier and take-over).
func (s *Service) IsLeader() bool {
	l := s.Current()
	return l != nil && s.node.IsLeader() && s.node.Term() == l.Term
}

// WriteBarrier proves a quorum still accepts this term before the HTTP layer
// dispatches a mutating request. Raft may report Leader briefly after an
// isolated leader loses contact; IsLeader alone is insufficient at that edge.
func (s *Service) WriteBarrier() error {
	l := s.Current()
	if l == nil || !l.Valid() {
		return ErrNotLeader
	}
	if err := s.node.Barrier(s.cfg.BarrierTimeout); err != nil {
		return err
	}
	if !l.Valid() {
		return ErrNotLeader
	}
	return nil
}

// LeaderAddress returns the ID and metastore address of the known leader.
func (s *Service) LeaderAddress() (id, address string) { return s.node.Leader() }

// Current returns the active leadership, or nil when this replica does not
// lead.
func (s *Service) Current() *Leadership {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// OnLeadership registers fn, called after each leadership start (with the
// new Leadership) and after each loss (with nil). It must not block.
func (s *Service) OnLeadership(fn func(*Leadership)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, fn)
}

// SetAgents replaces Config.Agents. It only has an effect before Start (an
// agent client that needs the Service's own store is created after New).
func (s *Service) SetAgents(a runner.AgentClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.cfg.Agents = a
	}
}

// Start runs the leader loop until Close.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.closed {
		return
	}
	s.started = true
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.loopWG.Add(1)
	go func() {
		defer s.loopWG.Done()
		s.leaderLoop(ctx)
	}()
}

// Close stops the leader loop (stepping down locally) and the node.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stop := s.stop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	s.loopWG.Wait()
	s.stepDown("shutdown")
	if err := s.node.Close(); err != nil {
		return fmt.Errorf("manager: close metastore: %w", err)
	}
	return nil
}
