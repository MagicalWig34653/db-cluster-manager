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
// Storage: with Config.DataDir set, the Raft log and stable state (current
// term, vote) are kept in a bbolt file (github.com/hashicorp/raft-boltdb/v2)
// and snapshots in raft's file snapshot store, so a node restarts from disk.
// With an empty DataDir everything is kept in memory and lost on Close. A
// snapshot is the journal's own state export (opjournal.Journal.Snapshot), so
// its size follows the journal state, not the number of applied commands.
//
// Transport: either an injected Transport (tests wrap the in-memory one) or,
// with Config.BindAddr, raft's plain TCP transport. That TCP transport has no
// TLS, no peer authentication and no encryption; it is for loopback and
// isolated lab networks only. TLS/mTLS between managers is out of scope here.
//
// Journal reads are local to the node and may lag behind the leader; a new
// leader calls Node.Barrier before it trusts its journal.
//
// The state machine also holds a generic versioned document store with
// atomic compare-and-set transactions (docs.go), replicated and snapshotted
// together with the journal.
//
// This is a prototype (W03, extended for VM experiments). It is not
// production hardened: no membership changes after bootstrap, no metrics, no
// tuned snapshot thresholds, no disk-full or fsync fault testing.
package metastore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// ErrNotLeader is returned by Apply and Barrier on a node that is not the
// leader.
var ErrNotLeader = errors.New("metastore: not leader")

// ErrOutcomeUnknown is returned by Apply when the command was handed to Raft
// but its commit could not be confirmed (for example leadership was lost or
// the apply timed out). The command may still commit; callers reconcile.
var ErrOutcomeUnknown = errors.New("metastore: apply outcome unknown")

// Command is one replicated change to the operation journal.
type Command struct {
	Kind   string          // "begin", "start", "finish", "adopt", "abort", KindDocs
	OpID   string          //
	OpKind string          // only for "begin"
	Steps  []string        // only for "begin"
	StepID string          // for "start", "finish"
	Result opjournal.State // only for "finish"
	Term   uint64          // fencing term the issuer believes it holds
	// Docs is the transaction of a KindDocs command (see docs.go). It is
	// omitted from the encoding of journal commands, which therefore encode
	// exactly as before.
	Docs []DocOp `json:",omitempty"`
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
	ID string

	// Exactly one of Transport and BindAddr must be set. Transport is used as
	// is. BindAddr ("host:port") creates raft's TCP transport without TLS.
	Transport Transport
	BindAddr  string
	// AdvertiseAddr is the address peers dial; it defaults to the bound
	// address and is required when BindAddr is unspecified (0.0.0.0).
	AdvertiseAddr string

	// DataDir enables disk storage (raft.db and snapshots/ inside it). Empty
	// keeps all state in memory.
	DataDir string
	// SnapshotRetain is the number of file snapshots kept (default 2).
	SnapshotRetain int

	// Peers is the full initial membership including this node; used only
	// when Bootstrap is set. All bootstrapping nodes must pass the same list.
	// Bootstrap is skipped when the node already has Raft state on disk, so
	// a restarted node can keep the flag.
	Peers     []Peer
	Bootstrap bool

	HeartbeatTimeout   time.Duration
	ElectionTimeout    time.Duration
	LeaderLeaseTimeout time.Duration
	CommitTimeout      time.Duration
	ApplyTimeout       time.Duration // default 5s

	LogOutput io.Writer // library logs; nil discards them
}

// storage provides Raft persistence.
type storage interface {
	logStore() raft.LogStore
	stableStore() raft.StableStore
	snapshotStore() raft.SnapshotStore
	close() error
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
func (m *memStorage) close() error                      { return nil }

// diskStorage keeps the log and stable state in one bbolt file and snapshots
// in raft's file snapshot store. bbolt holds an exclusive file lock, so a
// second node on the same directory fails to open instead of corrupting it.
type diskStorage struct {
	bolt  *raftboltdb.BoltStore
	snaps *raft.FileSnapshotStore
}

// boltOpenTimeout bounds the wait for the bbolt file lock (a variable so
// tests can shorten it).
var boltOpenTimeout = 5 * time.Second

func newDiskStorage(dir string, retain int, logOutput io.Writer) (*diskStorage, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("metastore: create data dir: %w", err)
	}
	if retain <= 0 {
		retain = 2
	}
	snaps, err := raft.NewFileSnapshotStore(dir, retain, logOutput)
	if err != nil {
		return nil, fmt.Errorf("metastore: open snapshot store: %w", err)
	}
	opts := raftboltdb.Options{Path: filepath.Join(dir, "raft.db")}
	bo := *bbolt.DefaultOptions
	bo.Timeout = boltOpenTimeout
	opts.BoltOptions = &bo
	bolt, err := raftboltdb.New(opts)
	if err != nil {
		return nil, fmt.Errorf("metastore: open log store: %w", err)
	}
	return &diskStorage{bolt: bolt, snaps: snaps}, nil
}

func (d *diskStorage) logStore() raft.LogStore           { return d.bolt }
func (d *diskStorage) stableStore() raft.StableStore     { return d.bolt }
func (d *diskStorage) snapshotStore() raft.SnapshotStore { return d.snaps }
func (d *diskStorage) close() error                      { return d.bolt.Close() }

// tcpPoolSize and tcpTimeout configure raft's TCP transport.
const (
	tcpPoolSize = 3
	tcpTimeout  = 10 * time.Second
)

func newTCPTransport(bind, advertise string, logOutput io.Writer) (Transport, error) {
	var adv net.Addr
	if advertise != "" {
		a, err := net.ResolveTCPAddr("tcp", advertise)
		if err != nil {
			return nil, fmt.Errorf("metastore: advertise address: %w", err)
		}
		adv = a
	}
	tr, err := raft.NewTCPTransport(bind, adv, tcpPoolSize, tcpTimeout, logOutput)
	if err != nil {
		return nil, fmt.Errorf("metastore: tcp transport: %w", err)
	}
	return tr, nil
}

// Node is one replica of the metadata store.
type Node struct {
	id           string
	raft         *raft.Raft
	fsm          *fsm
	st           storage
	applyTimeout time.Duration
}

// NewNode starts a node. Storage is chosen by cfg.DataDir and the transport
// by cfg.Transport or cfg.BindAddr.
func NewNode(cfg Config) (*Node, error) {
	if cfg.ID == "" {
		return nil, errors.New("metastore: empty node ID")
	}
	if (cfg.Transport == nil) == (cfg.BindAddr == "") {
		return nil, errors.New("metastore: set exactly one of Transport and BindAddr")
	}
	logOutput := cfg.LogOutput
	if logOutput == nil {
		logOutput = io.Discard
	}
	var st storage = newMemStorage()
	if cfg.DataDir != "" {
		ds, err := newDiskStorage(cfg.DataDir, cfg.SnapshotRetain, logOutput)
		if err != nil {
			return nil, err
		}
		st = ds
	}
	if cfg.Transport == nil {
		tr, err := newTCPTransport(cfg.BindAddr, cfg.AdvertiseAddr, logOutput)
		if err != nil {
			_ = st.close()
			return nil, err
		}
		cfg.Transport = tr
	}
	n, err := newNode(cfg, st)
	if err != nil {
		if c, ok := cfg.Transport.(raft.WithClose); ok && cfg.BindAddr != "" {
			_ = c.Close()
		}
		_ = st.close()
		return nil, err
	}
	return n, nil
}

func newNode(cfg Config, st storage) (*Node, error) {
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

	// Bootstrap writes the initial configuration into the stores before Raft
	// starts. Bootstrapping a running node instead races with AppendEntries
	// from peers that bootstrapped and elected a leader first.
	if cfg.Bootstrap {
		existing, err := raft.HasExistingState(st.logStore(), st.stableStore(), st.snapshotStore())
		if err != nil {
			return nil, fmt.Errorf("metastore: inspect stored state: %w", err)
		}
		if !existing {
			var servers []raft.Server
			for _, p := range cfg.Peers {
				servers = append(servers, raft.Server{
					Suffrage: raft.Voter,
					ID:       raft.ServerID(p.ID),
					Address:  raft.ServerAddress(p.Address),
				})
			}
			err := raft.BootstrapCluster(rc, st.logStore(), st.stableStore(), st.snapshotStore(),
				cfg.Transport, raft.Configuration{Servers: servers})
			if err != nil {
				return nil, fmt.Errorf("metastore: bootstrap: %w", err)
			}
		}
	}

	f := newFSM()
	r, err := raft.NewRaft(rc, f, st.logStore(), st.stableStore(), st.snapshotStore(), cfg.Transport)
	if err != nil {
		return nil, fmt.Errorf("metastore: start raft: %w", err)
	}
	n := &Node{id: cfg.ID, raft: r, fsm: f, st: st, applyTimeout: 5 * time.Second}
	setDuration(&n.applyTimeout, cfg.ApplyTimeout)
	return n, nil
}

func setDuration(dst *time.Duration, v time.Duration) {
	if v > 0 {
		*dst = v
	}
}

// Close stops the node, its transport and its storage.
func (n *Node) Close() error {
	err := n.raft.Shutdown().Error()
	if cerr := n.st.close(); err == nil {
		err = cerr
	}
	return err
}

// ID returns the node ID.
func (n *Node) ID() string { return n.id }

// IsLeader reports whether this node currently believes it is the leader.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// Leader returns the ID and transport address of the leader this node knows
// of; both are empty when it knows none.
func (n *Node) Leader() (id, address string) {
	addr, sid := n.raft.LeaderWithID()
	return string(sid), string(addr)
}

// Term returns the current consensus term of this node.
func (n *Node) Term() uint64 { return n.raft.CurrentTerm() }

// Journal returns the local replica of the journal for reading. Restoring a
// snapshot replaces the journal, so callers should not keep the pointer.
// Mutating it directly bypasses replication and must not be done.
func (n *Node) Journal() *opjournal.Journal { return n.fsm.journal() }

// Barrier blocks until every entry committed before the call has been
// applied to this node's journal. It must run on the leader; a new leader
// calls it before trusting its journal. It returns ErrNotLeader on a
// follower or when leadership is lost while waiting.
func (n *Node) Barrier(timeout time.Duration) error {
	if !n.IsLeader() {
		return ErrNotLeader
	}
	if err := n.raft.Barrier(timeout).Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) ||
			errors.Is(err, raft.ErrLeadershipTransferInProgress) {
			return fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
		return fmt.Errorf("metastore: barrier: %w", err)
	}
	return nil
}

// Apply replicates cmd and returns the journal error of applying it. It
// returns ErrNotLeader on a follower. If leadership is lost while the command
// is in flight, the outcome is unknown: the error wraps both
// ErrOutcomeUnknown and the library error (for example raft.ErrLeadershipLost).
func (n *Node) Apply(cmd Command) error {
	res, err := n.apply(cmd)
	if err != nil {
		return err
	}
	return res.err
}

// apply replicates cmd. The returned error is a replication error; the
// state machine's error is in applyResult.err.
func (n *Node) apply(cmd Command) (applyResult, error) {
	if !n.IsLeader() {
		return applyResult{}, ErrNotLeader
	}
	data, err := encodeCommand(cmd)
	if err != nil {
		return applyResult{}, err
	}
	fut := n.raft.Apply(data, n.applyTimeout)
	if err := fut.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipTransferInProgress) {
			return applyResult{}, fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
		return applyResult{}, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
	res, ok := fut.Response().(applyResult)
	if !ok {
		return applyResult{}, fmt.Errorf("metastore: unexpected apply response %T", fut.Response())
	}
	return res, nil
}

// Snapshot asks Raft to take a snapshot now (for tests and operators). It
// returns the library error, for example raft.ErrNothingNewToSnapshot.
func (n *Node) Snapshot() error { return n.raft.Snapshot().Error() }

func encodeCommand(cmd Command) ([]byte, error) {
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("metastore: encode command: %w", err)
	}
	return data, nil
}

// applyResult is the FSM response for one log entry.
type applyResult struct {
	err  error
	docs []Document // KindDocs only
}

// fsm applies commands to a journal and a document store. Snapshots are the
// journal's state export, wrapped in an envelope with the documents when
// there are any (see docs.go).
type fsm struct {
	mu   sync.Mutex
	j    *opjournal.Journal
	docs *docStore
}

func newFSM() *fsm { return &fsm{j: opjournal.New(), docs: newDocStore()} }

func (f *fsm) docList(coll string) []Document {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.docs.list(coll)
}

func (f *fsm) revision() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.docs.rev
}

func (f *fsm) journal() *opjournal.Journal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.j
}

// Apply implements raft.FSM.
func (f *fsm) Apply(l *raft.Log) interface{} {
	var cmd Command
	if err := json.Unmarshal(l.Data, &cmd); err != nil {
		return applyResult{err: fmt.Errorf("metastore: decode command at index %d: %w", l.Index, err)}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cmd.Kind == KindDocs {
		docs, err := f.docs.apply(cmd.Docs)
		return applyResult{err: err, docs: docs}
	}
	return applyResult{err: applyTo(f.j, cmd)}
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

// Snapshot implements raft.FSM. The journal export is taken under the FSM
// lock, so it matches the last applied index raft records for the snapshot.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := f.j.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("metastore: snapshot journal: %w", err)
	}
	if f.docs.empty() {
		return &fsmSnapshot{data: data}, nil
	}
	rev, docs := f.docs.export()
	env, err := json.Marshal(snapshotEnvelope{Format: snapshotFormatV2, Journal: data, Revision: rev, Docs: docs})
	if err != nil {
		return nil, fmt.Errorf("metastore: snapshot documents: %w", err)
	}
	return &fsmSnapshot{data: env}, nil
}

// Restore implements raft.FSM. It replaces the journal with the snapshot.
func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("metastore: read snapshot: %w", err)
	}
	docs := newDocStore()
	if format, ok := isEnvelope(data); ok {
		if format != snapshotFormatV2 {
			return fmt.Errorf("metastore: unsupported snapshot format %q", format)
		}
		var env snapshotEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("metastore: decode snapshot: %w", err)
		}
		if docs, err = restoreDocStore(env.Revision, env.Docs); err != nil {
			return err
		}
		data = env.Journal
	}
	j, err := opjournal.Restore(data)
	if err != nil {
		return fmt.Errorf("metastore: restore snapshot: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.j = j
	f.docs = docs
	return nil
}

type fsmSnapshot struct{ data []byte }

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("metastore: persist snapshot: %w", err)
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
