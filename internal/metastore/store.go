// SPDX-License-Identifier: AGPL-3.0-only

// Package metastore replicates the operation journal (package opjournal)
// across manager replicas with Raft (github.com/hashicorp/raft).
//
// Each committed Command is applied to an opjournal.Journal by a deterministic
// state machine. The consensus term of the leader is the fencing term:
// Command.Term carries the term the issuer believes it holds, and the journal
// rejects it with opjournal.ErrNotOwner when that term does not own the
// operation. The error of applying a command is returned to the caller of
// Node.Apply.
//
// This is a W03 prototype. Logs, stable state and snapshots are kept in memory
// behind the unexported storage interface, so a restart loses all state; a
// disk-backed store can replace it later. Only in-process transports are used
// in tests; wiring a real TCP transport (for example raft.NetworkTransport)
// for production is out of scope. Journal reads are local to the node and may
// lag behind the leader.
package metastore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// ErrNotLeader is returned by Apply on a node that is not the leader.
var ErrNotLeader = errors.New("metastore: not leader")

// Command is one replicated change to the operation journal.
type Command struct {
	Kind   string          // "begin", "start", "finish", "adopt", "abort"
	OpID   string          //
	OpKind string          // only for "begin"
	Steps  []string        // only for "begin"
	StepID string          // for "start", "finish"
	Result opjournal.State // only for "finish"
	Term   uint64          // fencing term the issuer believes it holds
}

// Transport carries Raft RPCs between nodes. It is the raft.Transport
// contract, named here so callers and tests can supply their own
// implementation (tests wrap the in-memory transport to inject partitions).
type Transport interface {
	raft.Transport
}

// Peer is one voting member of the initial cluster.
type Peer struct {
	ID      string
	Address string // transport address
}

// Config configures a Node. Zero durations use the library defaults.
type Config struct {
	ID        string
	Transport Transport
	// Peers is the full initial membership including this node; used only
	// when Bootstrap is set. All bootstrapping nodes must pass the same list.
	Peers     []Peer
	Bootstrap bool

	HeartbeatTimeout   time.Duration
	ElectionTimeout    time.Duration
	LeaderLeaseTimeout time.Duration
	CommitTimeout      time.Duration
	ApplyTimeout       time.Duration // default 5s

	LogOutput io.Writer // library logs; nil discards them
}

// storage provides Raft persistence. The prototype uses memStorage only.
type storage interface {
	logStore() raft.LogStore
	stableStore() raft.StableStore
	snapshotStore() raft.SnapshotStore
}

type memStorage struct {
	store *raft.InmemStore
	snaps *raft.InmemSnapshotStore
}

func newMemStorage() *memStorage {
	return &memStorage{store: raft.NewInmemStore(), snaps: raft.NewInmemSnapshotStore()}
}

func (m *memStorage) logStore() raft.LogStore           { return m.store }
func (m *memStorage) stableStore() raft.StableStore     { return m.store }
func (m *memStorage) snapshotStore() raft.SnapshotStore { return m.snaps }

// Node is one replica of the metadata store.
type Node struct {
	id           string
	raft         *raft.Raft
	fsm          *fsm
	applyTimeout time.Duration
}

// NewNode starts a node with in-memory persistence.
func NewNode(cfg Config) (*Node, error) {
	return newNode(cfg, newMemStorage())
}

func newNode(cfg Config, st storage) (*Node, error) {
	if cfg.ID == "" {
		return nil, errors.New("metastore: empty node ID")
	}
	if cfg.Transport == nil {
		return nil, errors.New("metastore: nil transport")
	}
	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(cfg.ID)
	setDuration(&rc.HeartbeatTimeout, cfg.HeartbeatTimeout)
	setDuration(&rc.ElectionTimeout, cfg.ElectionTimeout)
	setDuration(&rc.LeaderLeaseTimeout, cfg.LeaderLeaseTimeout)
	setDuration(&rc.CommitTimeout, cfg.CommitTimeout)
	rc.LogOutput = cfg.LogOutput
	if rc.LogOutput == nil {
		rc.LogOutput = io.Discard
	}

	f := newFSM()
	r, err := raft.NewRaft(rc, f, st.logStore(), st.stableStore(), st.snapshotStore(), cfg.Transport)
	if err != nil {
		return nil, fmt.Errorf("metastore: start raft: %w", err)
	}
	n := &Node{id: cfg.ID, raft: r, fsm: f, applyTimeout: 5 * time.Second}
	setDuration(&n.applyTimeout, cfg.ApplyTimeout)

	if cfg.Bootstrap {
		var servers []raft.Server
		for _, p := range cfg.Peers {
			servers = append(servers, raft.Server{
				Suffrage: raft.Voter,
				ID:       raft.ServerID(p.ID),
				Address:  raft.ServerAddress(p.Address),
			})
		}
		if err := r.BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
			_ = r.Shutdown().Error()
			return nil, fmt.Errorf("metastore: bootstrap: %w", err)
		}
	}
	return n, nil
}

func setDuration(dst *time.Duration, v time.Duration) {
	if v > 0 {
		*dst = v
	}
}

// Close stops the node.
func (n *Node) Close() error { return n.raft.Shutdown().Error() }

// IsLeader reports whether this node currently believes it is the leader.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// Term returns the current consensus term of this node.
func (n *Node) Term() uint64 { return n.raft.CurrentTerm() }

// Journal returns the local replica of the journal for reading. Restoring a
// snapshot replaces the journal, so callers should not keep the pointer.
// Mutating it directly bypasses replication and must not be done.
func (n *Node) Journal() *opjournal.Journal { return n.fsm.journal() }

// Apply replicates cmd and returns the journal error of applying it. It
// returns ErrNotLeader on a follower. If leadership is lost while the command
// is in flight, the outcome is unknown and the library error is returned
// wrapped (raft.ErrLeadershipLost).
func (n *Node) Apply(cmd Command) error {
	if !n.IsLeader() {
		return ErrNotLeader
	}
	data, err := encodeCommand(cmd)
	if err != nil {
		return err
	}
	fut := n.raft.Apply(data, n.applyTimeout)
	if err := fut.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipTransferInProgress) {
			return fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
		return fmt.Errorf("metastore: apply outcome unknown: %w", err)
	}
	res, ok := fut.Response().(applyResult)
	if !ok {
		return fmt.Errorf("metastore: unexpected apply response %T", fut.Response())
	}
	return res.err
}

func encodeCommand(cmd Command) ([]byte, error) {
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("metastore: encode command: %w", err)
	}
	return data, nil
}

// applyResult is the FSM response for one log entry.
type applyResult struct{ err error }

// fsm applies commands to a journal. It records the commands that changed the
// journal so a snapshot can be restored by deterministic replay; opjournal has
// no export API. The record grows without bound (prototype limitation).
type fsm struct {
	mu      sync.Mutex
	j       *opjournal.Journal
	applied []Command
}

func newFSM() *fsm { return &fsm{j: opjournal.New()} }

func (f *fsm) journal() *opjournal.Journal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.j
}

// Apply implements raft.FSM.
func (f *fsm) Apply(l *raft.Log) interface{} {
	var cmd Command
	if err := json.Unmarshal(l.Data, &cmd); err != nil {
		return applyResult{fmt.Errorf("metastore: decode command at index %d: %w", l.Index, err)}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	err := applyTo(f.j, cmd)
	if err == nil {
		f.applied = append(f.applied, cmd)
	}
	return applyResult{err}
}

// applyTo applies cmd to j. Every opjournal error leaves j unchanged.
func applyTo(j *opjournal.Journal, cmd Command) error {
	switch cmd.Kind {
	case "begin":
		return j.Begin(cmd.OpID, cmd.OpKind, cmd.Steps, cmd.Term)
	case "start":
		return j.StartStep(cmd.OpID, cmd.StepID, cmd.Term)
	case "finish":
		return j.FinishStep(cmd.OpID, cmd.StepID, cmd.Result, cmd.Term)
	case "adopt":
		return j.Adopt(cmd.OpID, cmd.Term)
	case "abort":
		return j.Abort(cmd.OpID, cmd.Term)
	default:
		return fmt.Errorf("metastore: unknown command kind %q", cmd.Kind)
	}
}

// Snapshot implements raft.FSM.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fsmSnapshot{cmds: append([]Command(nil), f.applied...)}, nil
}

// Restore implements raft.FSM. It replays the snapshot into a fresh journal.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var cmds []Command
	if err := json.NewDecoder(rc).Decode(&cmds); err != nil {
		return fmt.Errorf("metastore: decode snapshot: %w", err)
	}
	j := opjournal.New()
	for i, cmd := range cmds {
		if err := applyTo(j, cmd); err != nil {
			return fmt.Errorf("metastore: replay snapshot entry %d: %w", i, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.j, f.applied = j, cmds
	return nil
}

type fsmSnapshot struct{ cmds []Command }

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s.cmds); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("metastore: persist snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
