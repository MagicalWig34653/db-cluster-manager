// SPDX-License-Identifier: AGPL-3.0-only
package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
)

func TestRotationRequiresConfiguredRecoveryEnvelope(t *testing.T) {
	c, rings := keyringCluster(t, 1)
	l := c.leader()
	svc := &ReplicaService{Svc: l, Ring: rings[l.cfg.NodeID], Authorizer: opsFor(t, l).Authorizer}
	recovery := newIdentity(t)
	pub, _ := recovery.StreamPublicKey()
	if _, err := Put(l.Store(), CollSettings, SettingsKey, Settings{Backup: BackupSettings{RecipientKey: pub.String()}}, nil); err != nil {
		t.Fatal(err)
	}
	old, _, _ := Get[KeyringRecord](l.Store(), CollKeys, KeyringKey)
	if err := svc.Ring.updateDistributed(l); err != nil {
		t.Fatal(err)
	}
	settings, _, _ := Get[Settings](l.Store(), CollSettings, SettingsKey)
	settings.Value.Backup.RecipientKey = ""
	if _, err := Put(l.Store(), CollSettings, SettingsKey, settings.Value, IfVersion(settings.Version)); err != nil {
		t.Fatal(err)
	}
	p, err := svc.RotateDataKey(context.Background(), adminSubject, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RotateDataKey(context.Background(), adminSubject, p.Digest); err == nil {
		t.Fatal("rotation committed without recovery recipient")
	}
	rec, _, _ := Get[KeyringRecord](l.Store(), CollKeys, KeyringKey)
	if rec.Value.Current != old.Value.Current || rec.Value.Rotation != nil {
		t.Fatal("failed rotation changed keyring")
	}
	settings, _, _ = Get[Settings](l.Store(), CollSettings, SettingsKey)
	settings.Value.Backup.RecipientKey = pub.String()
	if _, err := Put(l.Store(), CollSettings, SettingsKey, settings.Value, IfVersion(settings.Version)); err != nil {
		t.Fatal(err)
	}
	p, err = svc.RotateDataKey(context.Background(), adminSubject, "")
	if err != nil {
		t.Fatal(err)
	}
	started, err := svc.RotateDataKey(context.Background(), adminSubject, p.Digest)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ = Get[KeyringRecord](l.Store(), CollKeys, KeyringKey)
	w := rec.Value.key(started.CurrentKey)
	if w == nil || w.RecoveryKeyID != pub.ID() || len(w.Recovery) == 0 {
		t.Fatalf("missing new recovery envelope: %+v", w)
	}
	keys, err := RecoverDataKeys(rec.Value, recovery)
	if err != nil || len(keys[started.CurrentKey]) != 32 {
		t.Fatalf("unusable recovery envelope: %v", err)
	}
}

func TestRotationResumesAfterLeaderLoss(t *testing.T) {
	c, rings := keyringCluster(t, 3)
	l := c.leader()
	ops := opsFor(t, l)
	svc := &ReplicaService{Svc: l, Ring: rings[l.cfg.NodeID], Authorizer: ops.Authorizer}
	for _, s := range c.live(l.cfg.NodeID) {
		pub, _ := rings[s.cfg.NodeID].PublicKey()
		if _, err := svc.Register(adminSubject, s.cfg.NodeID, pub); err != nil {
			t.Fatal(err)
		}
	}
	recovery := newIdentity(t)
	pub, _ := recovery.StreamPublicKey()
	if _, err := Put(l.Store(), CollSettings, SettingsKey, Settings{Backup: BackupSettings{RecipientKey: pub.String()}}, nil); err != nil {
		t.Fatal(err)
	}
	cols := []string{CollSessions, CollRecoverySessions, CollRecoveryCodes, CollPasskeys, CollPKI, CollAISecrets, collRegistrySecrets, "backup-keys", "future-secret"}
	oldID, oldKey, _ := svc.Ring.Current()
	stale, err := seal(StaticKey{ID: oldID, Bytes: oldKey}, "future-secret", "late", secretDoc{V: "late"})
	if err != nil {
		t.Fatal(err)
	}
	for _, coll := range cols {
		if err := PutSealed(l.Store(), svc.Ring, coll, "one", secretDoc{V: coll}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ops.Authorizer.Policy().Bind(authz.Binding{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RotateDataKey(context.Background(), operatorSubject, ""); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("operator: %v", err)
	}
	plan, err := svc.RotateDataKey(context.Background(), adminSubject, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RotateDataKey(context.Background(), adminSubject, "wrong"); !errors.Is(err, api.ErrApprovalMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	started, err := svc.RotateDataKey(context.Background(), adminSubject, plan.Digest)
	if err != nil || !started.Started {
		t.Fatalf("start: %+v %v", started, err)
	}
	if _, err := Put(l.Store(), "future-secret", "late", stale, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write: %v", err)
	}
	if err := svc.Ring.ResumeRotation(context.Background(), l, 2); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := Get[KeyringRecord](l.Store(), CollKeys, KeyringKey)
	if rec.Value.Rotation.Processed != 2 || rec.Value.Rotation.Complete || len(rec.Value.Keys) != 2 {
		t.Fatalf("progress: %+v", rec.Value)
	}
	// Every replica already has both envelopes before the leader is stopped.
	for _, s := range c.live(l.cfg.NodeID) {
		waitFor(t, "new envelope", func() bool {
			id, _, err := rings[s.cfg.NodeID].Current()
			return err == nil && id == started.CurrentKey
		})
	}
	c.close(l)
	next := c.leader()
	ring := rings[next.cfg.NodeID]
	if err := ring.ResumeRotation(context.Background(), next, 100); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = Get[KeyringRecord](next.Store(), CollKeys, KeyringKey)
	if !rec.Value.Rotation.Complete || rec.Value.Rotation.Processed != uint64(len(cols)) {
		t.Fatalf("completion: %+v", rec.Value.Rotation)
	}
	keys, err := RecoverDataKeys(rec.Value, recovery)
	if err != nil || len(keys) != 2 {
		t.Fatalf("recovery: %v", err)
	}
	for _, coll := range cols {
		raw, _, _ := Get[sealed](next.Store(), coll, "one")
		if raw.Value.KeyID != started.CurrentKey {
			t.Fatalf("old key in %s", coll)
		}
		v, ok, err := GetSealed[secretDoc](next.Store(), ring, coll, "one")
		if err != nil || !ok || v.V != coll {
			t.Fatalf("%s: %+v %v", coll, v, err)
		}
	}
	if err := ring.ResumeRotation(context.Background(), next, 100); err != nil {
		t.Fatal(err)
	}
}

func TestRotationTamperedSecretKeepsOldKey(t *testing.T) {
	c, rings := keyringCluster(t, 1)
	l := c.leader()
	ring := rings[l.cfg.NodeID]
	svc := &ReplicaService{Svc: l, Ring: ring, Authorizer: opsFor(t, l).Authorizer}
	if err := PutSealed(l.Store(), ring, "secrets", "bad", secretDoc{V: "secret"}); err != nil {
		t.Fatal(err)
	}
	d, _, _ := Get[sealed](l.Store(), "secrets", "bad")
	d.Value.Data[0] ^= 1
	if _, err := Put(l.Store(), "secrets", "bad", d.Value, nil); err != nil {
		t.Fatal(err)
	}
	p, err := svc.RotateDataKey(context.Background(), adminSubject, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RotateDataKey(context.Background(), adminSubject, p.Digest); err != nil {
		t.Fatal(err)
	}
	if err := ring.ResumeRotation(context.Background(), l, 20); err == nil {
		t.Fatal("corruption ignored")
	}
	rec, _, _ := Get[KeyringRecord](l.Store(), CollKeys, KeyringKey)
	if rec.Value.Rotation.Complete || len(rec.Value.Keys) != 2 {
		t.Fatal("old key dropped on failure")
	}
}
