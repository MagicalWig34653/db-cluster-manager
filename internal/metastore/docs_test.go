// SPDX-License-Identifier: AGPL-3.0-only

package metastore

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

func ifVersion(v uint64) *uint64 { return &v }

func waitForDoc(t *testing.T, nodes []*Node, coll, key string, version uint64) {
	t.Helper()
	for _, n := range nodes {
		waitFor(t, waitTimeout, "document on "+n.id, func() bool {
			d, ok := n.Doc(coll, key)
			return ok && d.Version == version
		})
	}
}

func TestDocsPutReplicatesAndVersions(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	res, err := leader.ApplyDocs(DocOp{Collection: "swarms", Key: "s1", Value: json.RawMessage(`{"name":"a"}`), IfVersion: ifVersion(0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Version == 0 {
		t.Fatalf("result %+v", res)
	}
	waitForDoc(t, c.live(), "swarms", "s1", res[0].Version)
	f := c.follower(leader)
	d, _ := f.Doc("swarms", "s1")
	if string(d.Value) != `{"name":"a"}` || d.Collection != "swarms" || d.Key != "s1" {
		t.Fatalf("follower doc %+v", d)
	}
	// Update with the right version advances it; versions are global
	// revisions, so they increase strictly.
	res2, err := leader.ApplyDocs(DocOp{Collection: "swarms", Key: "s1", Value: json.RawMessage(`{"name":"b"}`), IfVersion: ifVersion(res[0].Version)})
	if err != nil {
		t.Fatal(err)
	}
	if res2[0].Version <= res[0].Version {
		t.Fatalf("version did not advance: %d -> %d", res[0].Version, res2[0].Version)
	}
}

func TestDocsCASConflicts(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	put := func(v string, ifv *uint64) ([]Document, error) {
		return leader.ApplyDocs(DocOp{Collection: "c", Key: "k", Value: json.RawMessage(v), IfVersion: ifv})
	}
	first, err := put(`1`, ifVersion(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := put(`2`, ifVersion(0)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("create over existing: %v", err)
	}
	if _, err := put(`2`, ifVersion(first[0].Version+7)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if d, _ := leader.Doc("c", "k"); string(d.Value) != `1` {
		t.Fatalf("conflict changed value: %s", d.Value)
	}
	// Unconditional write.
	if _, err := put(`3`, nil); err != nil {
		t.Fatal(err)
	}
	// Delete with a stale version fails, then succeeds with the current one.
	cur, _ := leader.Doc("c", "k")
	if _, err := leader.ApplyDocs(DocOp{Collection: "c", Key: "k", Delete: true, IfVersion: ifVersion(first[0].Version)}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	if _, err := leader.ApplyDocs(DocOp{Collection: "c", Key: "k", Delete: true, IfVersion: ifVersion(cur.Version)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := leader.Doc("c", "k"); ok {
		t.Fatal("document survived delete")
	}
	// Deleting a missing document with a version condition conflicts.
	if _, err := leader.ApplyDocs(DocOp{Collection: "c", Key: "k", Delete: true, IfVersion: ifVersion(cur.Version)}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("delete missing: %v", err)
	}
	// A re-created document never reuses an old version (no ABA).
	again, err := put(`4`, ifVersion(0))
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Version <= cur.Version {
		t.Fatalf("re-created version %d not above %d", again[0].Version, cur.Version)
	}
}

func TestDocsTransactionIsAtomic(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	if _, err := leader.ApplyDocs(DocOp{Collection: "c", Key: "a", Value: json.RawMessage(`1`)}); err != nil {
		t.Fatal(err)
	}
	_, err := leader.ApplyDocs(
		DocOp{Collection: "c", Key: "b", Value: json.RawMessage(`2`)},
		DocOp{Collection: "c", Key: "a", Value: json.RawMessage(`9`), IfVersion: ifVersion(0)},
	)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if _, ok := leader.Doc("c", "b"); ok {
		t.Fatal("partial transaction applied")
	}
	if got := leader.Docs("c"); len(got) != 1 || got[0].Key != "a" {
		t.Fatalf("docs %+v", got)
	}
}

func TestDocsValidation(t *testing.T) {
	c := newTestCluster(t, 1)
	leader := c.waitForLeader()
	for _, op := range []DocOp{
		{Collection: "", Key: "k", Value: json.RawMessage(`1`)},
		{Collection: "c", Key: "", Value: json.RawMessage(`1`)},
		{Collection: "c", Key: "k"},
		{Collection: "c", Key: "k", Value: json.RawMessage(`{bad`)},
	} {
		if _, err := leader.ApplyDocs(op); !errors.Is(err, ErrInvalidDoc) {
			t.Errorf("%+v: %v", op, err)
		}
	}
	if _, err := leader.ApplyDocs(); !errors.Is(err, ErrInvalidDoc) {
		t.Errorf("empty transaction: %v", err)
	}
}

func TestDocsOnFollowerReturnsErrNotLeader(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	if _, err := c.follower(leader).ApplyDocs(DocOp{Collection: "c", Key: "k", Value: json.RawMessage(`1`)}); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("got %v", err)
	}
}

func TestDocsSurviveLeaderChange(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.waitForLeader()
	res, err := leader.ApplyDocs(DocOp{Collection: "c", Key: "k", Value: json.RawMessage(`"x"`)})
	if err != nil {
		t.Fatal(err)
	}
	waitForDoc(t, c.live(), "c", "k", res[0].Version)
	c.stop(leader.id)
	next := c.waitForLeader(leader.id)
	if err := next.Barrier(waitTimeout); err != nil {
		t.Fatal(err)
	}
	d, ok := next.Doc("c", "k")
	if !ok || d.Version != res[0].Version {
		t.Fatalf("new leader doc %+v %v", d, ok)
	}
	// CAS against the replicated version works on the new leader.
	if _, err := next.ApplyDocs(DocOp{Collection: "c", Key: "k", Value: json.RawMessage(`"y"`), IfVersion: ifVersion(d.Version)}); err != nil {
		t.Fatal(err)
	}
}

func TestDocsAndJournalSnapshotRoundTrip(t *testing.T) {
	src := newFSM()
	apply := func(cmd Command) {
		t.Helper()
		data, _ := json.Marshal(cmd)
		res := src.Apply(&raft.Log{Index: 1, Data: data}).(applyResult)
		if res.err != nil {
			t.Fatal(res.err)
		}
	}
	apply(Command{Kind: "begin", OpID: "op1", OpKind: "k", Steps: []string{"s1"}, Term: 1})
	apply(Command{Kind: KindDocs, Docs: []DocOp{
		{Collection: "a", Key: "1", Value: json.RawMessage(`{"x":1}`)},
		{Collection: "b", Key: "2", Value: json.RawMessage(`[1,2]`)},
	}})
	apply(Command{Kind: KindDocs, Docs: []DocOp{{Collection: "a", Key: "1", Delete: true}}})
	snap, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	dst := newFSM()
	if err := dst.Restore(io.NopCloser(bytes.NewReader(sink.buf.Bytes()))); err != nil {
		t.Fatal(err)
	}
	if got := dst.docList("a"); len(got) != 0 {
		t.Fatalf("deleted doc restored: %+v", got)
	}
	b := dst.docList("b")
	if len(b) != 1 || string(b[0].Value) != `[1,2]` {
		t.Fatalf("restored docs %+v", b)
	}
	if dst.revision() != src.revision() {
		t.Fatalf("revision %d != %d", dst.revision(), src.revision())
	}
	if _, err := dst.journal().Get("op1"); err != nil {
		t.Fatalf("journal not restored: %v", err)
	}
	// Equal state gives equal snapshot bytes.
	snap2, _ := dst.Snapshot()
	sink2 := &memSink{}
	_ = snap2.Persist(sink2)
	if !bytes.Equal(sink.buf.Bytes(), sink2.buf.Bytes()) {
		t.Fatal("snapshot not deterministic")
	}
}

func TestRestoreAcceptsLegacyJournalSnapshot(t *testing.T) {
	j := opjournal.New()
	if err := j.Begin("op1", "k", []string{"s1"}, 1); err != nil {
		t.Fatal(err)
	}
	legacy, err := j.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	f := newFSM()
	if err := f.Restore(io.NopCloser(bytes.NewReader(legacy))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.journal().Get("op1"); err != nil {
		t.Fatal(err)
	}
	if f.revision() != 0 || len(f.docList("any")) != 0 {
		t.Fatal("legacy restore invented documents")
	}
}

func TestRestoreRejectsUnknownSnapshotFormat(t *testing.T) {
	f := newFSM()
	err := f.Restore(io.NopCloser(bytes.NewReader([]byte(`{"format":"dcm-metastore/99","journal":{}}`))))
	if err == nil {
		t.Fatal("unknown format accepted")
	}
}

// putDocOnLeader writes one unconditional document through applyOnLeader and
// returns its version. An unknown outcome is settled by reading the leader's
// replica after a barrier: the test writes every key once with a distinct
// value, so a present key with that value means the put committed.
func (c *tcpCluster) putDocOnLeader(coll, key, value string) uint64 {
	c.t.Helper()
	op := DocOp{Collection: coll, Key: key, Value: json.RawMessage(value)}
	present := func(n *Node) bool {
		d, ok := n.Doc(coll, key)
		return ok && string(d.Value) == value
	}
	l, err := c.applyOnLeader("put "+coll+"/"+key, func(n *Node) error {
		_, err := n.ApplyDocs(op)
		return err
	}, present)
	if err != nil {
		c.t.Fatalf("put %s/%s on %s: %v", coll, key, l.id, err)
	}
	// After a successful apply or a barrier the leader has applied the put.
	d, ok := l.Doc(coll, key)
	if !ok {
		c.t.Fatalf("put %s/%s: not on the settling leader %s", coll, key, l.id)
	}
	return d.Version
}

func TestDocsRestartFromDiskWithSnapshot(t *testing.T) {
	c := newTCPCluster(t, 3)
	var last uint64
	for i, v := range []string{`1`, `2`, `3`} {
		last = c.putDocOnLeader("c", string(rune('a'+i)), v)
	}
	waitForDoc(t, c.live(), "c", "c", last)
	for _, n := range c.live() {
		if err := n.Snapshot(); err != nil {
			t.Fatalf("snapshot %s: %v", n.id, err)
		}
	}
	dv := c.putDocOnLeader("c", "d", `4`)
	waitForDoc(t, c.live(), "c", "d", dv)
	for _, id := range c.ids {
		c.stop(id)
	}
	for _, id := range c.ids {
		c.start(id)
	}
	l := c.barrierLeader()
	if got := l.Docs("c"); len(got) != 4 {
		t.Fatalf("after restart: %+v", got)
	}
	waitForDoc(t, c.live(), "c", "d", dv)
}

type memSink struct{ buf bytes.Buffer }

func (s *memSink) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *memSink) Close() error                { return nil }
func (s *memSink) ID() string                  { return "mem" }
func (s *memSink) Cancel() error               { return nil }
