// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

func newIdentity(t *testing.T) backup.RecoveryKey {
	t.Helper()
	k, err := backup.GenerateRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func keyringCluster(t *testing.T, size int) (*cluster, map[string]*Keyring) {
	t.Helper()
	krs := map[string]*Keyring{}
	c := newCluster(t, size, func(id string, cfg *Config) {
		krs[id] = &Keyring{ReplicaID: id, Identity: newIdentity(t)}
		cfg.Keyring = krs[id]
	})
	return c, krs
}

type secretDoc struct{ V string }

// TestKeyringDistributesDataKeyToRegisteredReplicas: the first leader
// bootstraps the keyring; a follower cannot unseal anything until an
// administrator registers its public key (only voters, only with the
// settings update permission); then it unwraps the same key from the
// replicated keyring. No plaintext key is replicated.
func TestKeyringDistributesDataKeyToRegisteredReplicas(t *testing.T) {
	c, krs := keyringCluster(t, 3)
	l := c.leader()
	lk := krs[l.cfg.NodeID]
	id, key, err := lk.Current()
	if err != nil || len(key) != 32 {
		t.Fatalf("leader key: %q %v", id, err)
	}
	if err := PutSealed(l.Store(), lk, "secrets", "s1", secretDoc{V: "hunter2"}); err != nil {
		t.Fatal(err)
	}
	f := c.follower(l)
	fk := krs[f.cfg.NodeID]
	waitFor(t, "keyring replicated", func() bool {
		_, ok, _ := Get[KeyringRecord](f.Store(), CollKeys, KeyringKey)
		return ok
	})
	if _, _, err := fk.Current(); !errors.Is(err, ErrNoKey) {
		t.Fatalf("unregistered follower: %v", err)
	}
	raw, _ := l.Store().RawList(CollKeys)
	for _, d := range raw {
		if bytes.Contains(d.Value, key) || bytes.Contains(d.Value, []byte(base64.StdEncoding.EncodeToString(key))) {
			t.Fatal("plaintext data key in the store")
		}
	}

	ops := opsFor(t, l)
	ks := &ReplicaService{Svc: l, Ring: lk, Authorizer: ops.Authorizer}
	pub, _ := fk.PublicKey()
	if err := ops.Authorizer.Policy().Bind(authz.Binding{Subject: operatorSubject, Role: authz.RoleOperator, Scope: authz.Global}); err != nil {
		t.Fatal(err)
	}
	if _, err := ks.Register(operatorSubject, f.cfg.NodeID, pub); !errors.As(err, new(*authz.DeniedError)) {
		t.Fatalf("operator registration: %v", err)
	}
	if _, err := ks.Register(adminSubject, "m9", pub); !errors.Is(err, ErrNotVoter) {
		t.Fatalf("non-voter registration: %v", err)
	}
	if _, err := ks.Register(adminSubject, f.cfg.NodeID, "dbcmpk1:nope"); err == nil {
		t.Fatal("bad public key accepted")
	}
	if _, err := ks.Register(adminSubject, f.cfg.NodeID, pub); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "follower unwraps the key", func() bool {
		fid, fkey, err := fk.Current()
		return err == nil && fid == id && bytes.Equal(fkey, key)
	})
	waitFor(t, "follower unseals", func() bool {
		v, ok, err := GetSealed[secretDoc](f.Store(), fk, "secrets", "s1")
		return err == nil && ok && v.V == "hunter2"
	})
	st, err := ks.Status(adminSubject)
	if err != nil {
		t.Fatal(err)
	}
	have := 0
	for _, r := range st.Replicas {
		if r.HasCurrentKey {
			have++
		}
	}
	if st.CurrentKey != id || len(st.Replicas) != 3 || have != 2 || st.Recoverable {
		t.Fatalf("status %+v", st)
	}
}

// TestKeyringReplicaWithoutKeyHandsLeadershipOver: a replica that cannot
// unwrap the current key never starts a leadership (it would fail to read
// sessions, passkeys and the agent CA) and transfers leadership to a
// replica that can.
func TestKeyringReplicaWithoutKeyHandsLeadershipOver(t *testing.T) {
	c, krs := keyringCluster(t, 3)
	l := c.leader()
	ops := opsFor(t, l)
	ks := &ReplicaService{Svc: l, Ring: krs[l.cfg.NodeID], Authorizer: ops.Authorizer}
	f := c.follower(l)
	pub, _ := krs[f.cfg.NodeID].PublicKey()
	if _, err := ks.Register(adminSubject, f.cfg.NodeID, pub); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "follower holds the key", func() bool { _, _, err := krs[f.cfg.NodeID].Current(); return err == nil })
	c.close(l)
	var third *Service
	for _, s := range c.live(f.cfg.NodeID) {
		third = s
	}
	// Whoever wins elections, only the registered replica completes a
	// leadership start.
	waitFor(t, "registered replica leads", func() bool { return f.IsLeader() && f.Current() != nil })
	for range 20 {
		if third.Current() != nil {
			t.Fatal("replica without the data key started a leadership")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestKeyringAdoptsExistingKeyAndRecoversWithRecoveryKey: a keyring
// bootstrapped from an existing key file keeps its key ID, so documents
// sealed before stay readable; once a recovery key is configured every
// data key gets a recovery envelope, and the externally retained recovery
// key alone recovers the data keys (B07).
func TestKeyringAdoptsExistingKeyAndRecoversWithRecoveryKey(t *testing.T) {
	seed, err := FileKeyProvider(filepath.Join(t.TempDir(), "data.key"))
	if err != nil {
		t.Fatal(err)
	}
	var kr *Keyring
	c := newCluster(t, 1, func(id string, cfg *Config) {
		kr = &Keyring{ReplicaID: id, Identity: newIdentity(t), Seed: seed.Bytes}
		cfg.Keyring = kr
	})
	l := c.leader()
	id, key, err := kr.Current()
	if err != nil || id != LegacyKeyID || !bytes.Equal(key, seed.Bytes) {
		t.Fatalf("adopted key %q %v", id, err)
	}
	// Sealed with the old file key provider, read through the keyring.
	if err := PutSealed(l.Store(), seed, "secrets", "old", secretDoc{V: "x"}); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := GetSealed[secretDoc](l.Store(), kr, "secrets", "old"); err != nil || !ok || v.V != "x" {
		t.Fatalf("old document: %+v %v %v", v, ok, err)
	}
	rec, _, _ := Get[KeyringRecord](l.Store(), CollKeys, KeyringKey)
	if _, err := RecoverDataKeys(rec.Value, newIdentity(t)); !errors.Is(err, ErrNoKey) {
		t.Fatalf("recovery without envelope: %v", err)
	}
	recovery := newIdentity(t)
	rpub, _ := recovery.StreamPublicKey()
	if _, err := Put(l.Store(), CollSettings, SettingsKey, Settings{Backup: BackupSettings{RecipientKey: rpub.String()}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := kr.updateDistributed(l); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = Get[KeyringRecord](l.Store(), CollKeys, KeyringKey)
	if _, err := RecoverDataKeys(rec.Value, newIdentity(t)); err == nil {
		t.Fatal("another key recovered the data key")
	}
	got, err := RecoverDataKeys(rec.Value, recovery)
	if err != nil || !bytes.Equal(got[LegacyKeyID], seed.Bytes) {
		t.Fatalf("recovered %v %v", got, err)
	}
	rk := RecoveredKeys{CurrentID: rec.Value.Current, Keys: got}
	if v, ok, err := GetSealed[secretDoc](l.Store(), rk, "secrets", "old"); err != nil || !ok || v.V != "x" {
		t.Fatalf("unseal with recovered keys: %+v %v %v", v, ok, err)
	}
	// An envelope is bound to its recipient: the recovery envelope does
	// not open as a replica envelope.
	if _, err := unwrapDataKey(rec.Value.Keys[0].Recovery, LegacyKeyID, recipientLabel(kr.ReplicaID), recovery); err == nil {
		t.Fatal("recovery envelope accepted for a replica")
	}
}

func TestLoadReplicaIdentityPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "replica.key")
	a, err := LoadReplicaIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadReplicaIdentity(p)
	if err != nil || a != b {
		t.Fatalf("reloaded identity differs: %v", err)
	}
}
