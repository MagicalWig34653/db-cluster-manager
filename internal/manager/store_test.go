// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

type counter struct{ N int }

func TestStoreCASConflictAndUpdateRetries(t *testing.T) {
	c := newCluster(t, 3, nil)
	l := c.leader()
	st := l.Store()
	v1, err := Put(st, "c", "k", counter{1}, IfVersion(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, "c", "k", counter{2}, IfVersion(0)); !errors.Is(err, ErrConflict) {
		t.Fatalf("create over existing: %v", err)
	}
	if _, err := Put(st, "c", "k", counter{2}, IfVersion(v1+100)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	// Concurrent read-modify-write converges through CAS retries.
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := Update(st, "c", "k", func(cur counter, _ bool) (counter, error) {
				cur.N++
				return cur, nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		if err != nil {
			if !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			failed++
		}
	}
	got, _, _ := Get[counter](st, "c", "k", Consistent())
	if got.Value.N != 1+10-failed {
		t.Fatalf("counter %d, failed %d", got.Value.N, failed)
	}
}

func TestStoreConsistentReadNeedsLeader(t *testing.T) {
	c := newCluster(t, 3, nil)
	l := c.leader()
	if _, err := Put(l.Store(), "c", "k", counter{7}, nil); err != nil {
		t.Fatal(err)
	}
	f := c.follower(l)
	if _, _, err := Get[counter](f.Store(), "c", "k", Consistent()); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("consistent read on follower: %v", err)
	}
	waitFor(t, "follower replica", func() bool {
		v, ok, _ := Get[counter](f.Store(), "c", "k")
		return ok && v.Value.N == 7
	})
	if _, err := Put(f.Store(), "c", "k", counter{8}, nil); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("write on follower: %v", err)
	}
}

func TestStoreSurvivesLeaderChange(t *testing.T) {
	c := newCluster(t, 3, nil)
	l := c.leader()
	ver, err := Put(l.Store(), "c", "k", counter{1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range c.live() {
		waitFor(t, "replication", func() bool { _, ok, _ := s.Store().Raw("c", "k"); return ok })
	}
	c.close(l)
	n := c.leader(l.cfg.NodeID)
	got, ok, err := Get[counter](n.Store(), "c", "k", Consistent())
	if err != nil || !ok || got.Version != ver {
		t.Fatalf("new leader: %+v %v %v", got, ok, err)
	}
	if _, err := Put(n.Store(), "c", "k", counter{2}, IfVersion(ver)); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRestartFromDiskWithSnapshot(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	start := func() *Service {
		s, err := New(Config{NodeID: "solo", DataDir: dir, BindAddr: addr, Tune: fastRaft, PollInterval: 10 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		s.Start()
		waitFor(t, "single-instance leader", s.IsLeader)
		return s
	}
	s := start()
	if _, err := Put(s.Store(), "c", "a", counter{1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := PutSealed(s.Store(), testKey, CollSessions, "d1", counter{42}); err != nil {
		t.Fatal(err)
	}
	if err := s.Node().Snapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(s.Store(), "c", "b", counter{2}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = start()
	defer s.Close()
	docs, err := List[counter](s.Store(), "c", Consistent())
	if err != nil || len(docs) != 2 || docs[1].Value.N != 2 {
		t.Fatalf("after restart %+v %v", docs, err)
	}
	sealedDocs, err := ListSealed[counter](s.Store(), testKey, CollSessions)
	if err != nil || len(sealedDocs) != 1 || sealedDocs[0].Value.N != 42 {
		t.Fatalf("sealed after restart %+v %v", sealedDocs, err)
	}
}

func TestSealedValuesAreEncryptedAndBound(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	type secret struct{ Token string }
	if err := PutSealed(s.Store(), testKey, CollSessions, "k", secret{"plain-secret-value"}); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := s.Store().Raw(CollSessions, "k")
	if bytes.Contains(raw.Value, []byte("plain-secret-value")) {
		t.Fatal("sealed value stored in clear")
	}
	// The value cannot be moved to another key.
	if _, err := s.Store().Txn(mustPutRaw(t, CollSessions, "other", raw.Value)); err != nil {
		t.Fatal(err)
	}
	if _, err := ListSealed[secret](s.Store(), testKey, CollSessions); err == nil {
		t.Fatal("moved sealed value decrypted")
	}
	other := StaticKey{ID: "k1", Bytes: []byte("ffffffffffffffffffffffffffffffff")}
	var out secret
	var sv sealed
	_ = jsonUnmarshal(raw.Value, &sv)
	if err := unseal(other, CollSessions, "k", sv, &out); err == nil {
		t.Fatal("wrong key decrypted")
	}
	if err := unseal(StaticKey{ID: "k2", Bytes: testKey.Bytes}, CollSessions, "k", sv, &out); !errors.Is(err, ErrNoKey) {
		t.Fatalf("unknown key ID: %v", err)
	}
}

func TestFileKeyProviderCreatesAndReloads(t *testing.T) {
	p := t.TempDir() + "/keys/data.key"
	k1, err := FileKeyProvider(p)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := FileKeyProvider(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1.Bytes, k2.Bytes) || len(k1.Bytes) != 32 {
		t.Fatal("key not stable across loads")
	}
}
