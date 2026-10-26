// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

const marker = "TOP-SECRET-CLUSTER-MARKER"

// metaEnv builds a leader whose store holds a keyring with a recovery
// envelope (as a configured installation has), a sealed-looking document,
// a session and an operation record that must not be exported.
func metaEnv(t *testing.T) (env *backupEnv, recovery backup.RecoveryKey, dataKey []byte, n *notes) {
	t.Helper()
	env = newBackupEnv(t, true)
	var err error
	if recovery, err = backup.GenerateRecoveryKey(); err != nil {
		t.Fatal(err)
	}
	pub, _ := recovery.StreamPublicKey()
	replica, _ := backup.GenerateRecoveryKey()
	replicaPub, _ := replica.StreamPublicKey()
	dataKey = make([]byte, 32)
	_, _ = rand.Read(dataKey)
	now := env.clock.Now()
	wk := WrappedDataKey{ID: "k1", Created: now, RecoveryKeyID: pub.ID()}
	if wk.Recovery, err = wrapDataKey(dataKey, "k1", recipientLabel(""), pub, now); err != nil {
		t.Fatal(err)
	}
	env1, err := wrapDataKey(dataKey, "k1", recipientLabel("old"), replicaPub, now)
	if err != nil {
		t.Fatal(err)
	}
	wk.Envelopes = map[string][]byte{"old": env1}
	ring := KeyringRecord{Current: "k1", Keys: []WrappedDataKey{wk}, Replicas: map[string]ReplicaRegistration{"old": {PublicKey: replicaPub.String()}}}
	st := env.svc.Store()
	if _, err := Put(st, CollKeys, KeyringKey, ring, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, CollSessions, "tok", map[string]string{"user": "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Put(st, "notes", "n1", map[string]string{"text": marker}, nil); err != nil {
		t.Fatal(err)
	}
	_, _, err = Update(st, CollSettings, SettingsKey, func(s Settings, _ bool) (Settings, error) {
		s.Backup.RecipientKey = pub.String()
		return s, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	env.b.Keys = backup.StaticKeys{CurrentID: "k1", Keys: map[string][]byte{"k1": k}}
	n = &notes{}
	env.b.Notify = n.Notify
	return env, recovery, dataKey, n
}

func TestMetastoreBackupExportImportRoundTrip(t *testing.T) {
	env, recovery, dataKey, _ := metaEnv(t)
	ctx := context.Background()
	if _, err := env.b.CreateMetastoreBackup(ctx, nobody); !errors.Is(err, api.ErrForbidden) {
		t.Fatalf("unauthorized backup: %v", err)
	}
	mb, err := env.b.CreateMetastoreBackup(ctx, adminSubject)
	if err != nil {
		t.Fatal(err)
	}
	if !mb.Verified || mb.Documents == 0 || mb.By != adminSubject.ID || !strings.HasPrefix(mb.Name, "metastore-inst-1-") {
		t.Fatalf("backup %+v", mb)
	}
	list, err := env.b.MetastoreBackups(ctx)
	if err != nil || len(list) != 1 || !list[0].Verified {
		t.Fatalf("list %v %+v", err, list)
	}

	// The stored object is ciphertext; the running manager cannot read it.
	raw, err := readBackend(env.be, mb.Name)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(marker)) || bytes.Contains(raw, []byte("alice")) {
		t.Fatal("plaintext in the stored export")
	}
	other, _ := backup.GenerateRecoveryKey()
	if _, err := OpenMetaExport(raw, other); err == nil {
		t.Fatal("a wrong recovery key opened the export")
	}
	exp, err := OpenMetaExport(raw, recovery)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range exp.Docs {
		if metaExcluded[d.Collection] {
			t.Fatalf("excluded collection %s exported", d.Collection)
		}
	}
	if len(exp.Excluded) == 0 {
		t.Fatal("exclusions not recorded")
	}

	// Import into a fresh store of a new replica.
	fresh := newCluster(t, 1, nil).leader()
	newReplica, _ := backup.GenerateRecoveryKey()
	newPub, _ := newReplica.StreamPublicKey()
	if _, err := ImportMetastore(fresh.Store(), exp, other, "new", newPub, env.clock.Now()); err == nil {
		t.Fatal("import with the wrong recovery key succeeded")
	}
	for _, d := range fresh.Node().AllDocs() {
		if !metaExcluded[d.Collection] {
			t.Fatalf("a failed import wrote %s/%s", d.Collection, d.Key)
		}
	}
	res, err := ImportMetastore(fresh.Store(), exp, recovery, "new", newPub, env.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Documents != len(exp.Docs) || len(res.DataKeys) != 1 {
		t.Fatalf("result %+v", res)
	}
	got, ok, _ := Get[map[string]string](fresh.Store(), "notes", "n1")
	if !ok || got.Value["text"] != marker {
		t.Fatalf("imported document %+v", got)
	}
	if _, ok, _ := Get[map[string]string](fresh.Store(), CollSessions, "tok"); ok {
		t.Fatal("session imported")
	}
	// The new replica can unwrap the very same data key; the old replica's envelope is gone.
	kr := &Keyring{ReplicaID: "new", Identity: newReplica}
	kr.Attach(fresh.Store())
	id, key, err := kr.Current()
	if err != nil || id != "k1" || !bytes.Equal(key, dataKey) {
		t.Fatalf("imported keyring: %q %v", id, err)
	}
	ring, _, _ := Get[KeyringRecord](fresh.Store(), CollKeys, KeyringKey)
	if _, old := ring.Value.Keys[0].Envelopes["old"]; old || len(ring.Value.Replicas) != 1 || len(ring.Value.Keys[0].Recovery) == 0 {
		t.Fatalf("keyring after import %+v", ring.Value)
	}
	// A second import refuses: the store is no longer empty.
	if _, err := ImportMetastore(fresh.Store(), exp, recovery, "new", newPub, env.clock.Now()); !errors.Is(err, ErrStoreNotEmpty) {
		t.Fatalf("second import: %v", err)
	}
}

func TestMetastoreBackupTamperedRecordIsNotOffered(t *testing.T) {
	env, _, _, _ := metaEnv(t)
	ctx := context.Background()
	mb, err := env.b.CreateMetastoreBackup(ctx, adminSubject)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := readBackend(env.be, mb.Name+metaRecordSuffix)
	if err := env.be.Put(ctx, mb.Name+metaRecordSuffix, bytes.NewReader(bytes.Replace(rec, []byte(`"payload":"`), []byte(`"payload":"AA`), 1))); err != nil {
		t.Fatal(err)
	}
	if list, _ := env.b.MetastoreBackups(ctx); len(list) != 0 {
		t.Fatalf("tampered record listed: %+v", list)
	}
	// A truncated object is listed as not verified.
	env2, _, _, _ := metaEnv(t)
	mb2, _ := env2.b.CreateMetastoreBackup(ctx, adminSubject)
	obj, _ := readBackend(env2.be, mb2.Name)
	_ = env2.be.Put(ctx, mb2.Name, bytes.NewReader(obj[:len(obj)-1]))
	if list, _ := env2.b.MetastoreBackups(ctx); len(list) != 1 || list[0].Verified {
		t.Fatalf("truncated object: %+v", list)
	}
}

func TestScheduledMetastoreBackupRetentionAndFailureNotice(t *testing.T) {
	env, _, _, n := metaEnv(t)
	ctx := context.Background()
	env.b.MetaInterval, env.b.MetaKeep = 24*time.Hour, 2
	if _, err := EnsureSystemBinding(env.ops.Authorizer.Policy()); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		env.clock.Set(time.Date(2026, 10, 22+i, 1, 0, 0, 0, time.UTC))
		env.b.RunMetastoreBackup(ctx, collect.SystemSubject)
		env.b.RunMetastoreBackup(ctx, collect.SystemSubject) // not due again
	}
	list, _ := env.b.MetastoreBackups(ctx)
	if len(list) != 2 || list[0].By != collect.SystemSubject.ID {
		t.Fatalf("kept %+v", list)
	}
	if infos, _ := env.be.List(ctx, metaObjectPrefix); len(infos) != 4 {
		t.Fatalf("objects after retention: %d", len(infos))
	}
	// A failing target is reported, not retried on every pass.
	env.b.Backend = nil
	env.clock.Set(time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC))
	env.b.RunMetastoreBackup(ctx, collect.SystemSubject)
	env.b.RunMetastoreBackup(ctx, collect.SystemSubject)
	if k := n.kinds(); len(k) != 1 || k[0] != KindMetastoreBackupFailed {
		t.Fatalf("notifications %v", k)
	}
	if n.ev[0].Severity != notify.SeverityCritical {
		t.Fatalf("severity %v", n.ev[0].Severity)
	}
}
