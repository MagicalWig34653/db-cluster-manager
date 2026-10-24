// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/managertls"
)

func TestACMESealedReplicationAndTermFence(t *testing.T) {
	c := newCluster(t, 3, nil)
	leader := c.leader()
	leadership := leader.Current()
	keys := StaticKey{ID: "acme-test", Bytes: bytes.Repeat([]byte{7}, 32)}
	writer := ACMEStorage{Store: leader.Store(), Keys: keys, CanWrite: leadership.Valid}
	ctx := context.Background()
	secret := []byte("synthetic ACME account and private certificate key")
	if err := writer.Put(ctx, "test/certificate", secret); err != nil {
		t.Fatal(err)
	}
	raw, ok, err := leader.Store().Raw(CollACME, "test/certificate")
	if err != nil || !ok || bytes.Contains(raw.Value, secret) {
		t.Fatalf("unsealed or missing ACME record: %v", err)
	}
	follower := c.follower(leader)
	reader := ACMEStorage{Store: follower.Store(), Keys: keys}
	waitFor(t, "ACME replicated", func() bool {
		v, err := reader.Get(ctx, "test/certificate")
		return err == nil && bytes.Equal(v, secret)
	})
	if err := reader.Put(ctx, "test/certificate", secret); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower write: %v", err)
	}
	// Collection/key AAD protects against moving encrypted ACME records.
	if _, err := Put(leader.Store(), CollACME, "wrong-key", json.RawMessage(raw.Value), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Get(ctx, "wrong-key"); err == nil {
		t.Fatal("moved sealed value accepted")
	}
	c.close(leader)
	successor := c.leader(leader.cfg.NodeID)
	next := ACMEStorage{Store: successor.Store(), Keys: keys, CanWrite: successor.Current().Valid}
	if v, err := next.Get(ctx, "test/certificate"); err != nil || !bytes.Equal(v, secret) {
		t.Fatalf("successor read: %v", err)
	}
	if err := writer.Put(ctx, "test/certificate", []byte("old term")); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("old term write: %v", err)
	}
	if err := writer.Delete(ctx, "test/certificate"); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("old term cleanup: %v", err)
	}
	if err := next.Put(ctx, "test/certificate", []byte("renewed synthetic key")); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := next.Put(canceled, "test/certificate", secret); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write: %v", err)
	}
	if err := next.Delete(ctx, "test/certificate"); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Get(ctx, "test/certificate"); !errors.Is(err, managertls.ErrMissing) {
		t.Fatalf("missing: %v", err)
	}
}
