// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func startSource(t *testing.T, dir, addr string, kr *manager.Keyring) *manager.Service {
	t.Helper()
	svc, err := manager.New(manager.Config{NodeID: "manager-1", DataDir: dir, BindAddr: addr, AdvertiseAddr: addr, Bootstrap: true, Keyring: kr,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if svc.Node().IsLeader() {
			return svc
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no leader")
	return nil
}

// TestMetastoreImportCommand restores a sealed export into a fresh data
// directory through the real command and refuses a wrong key and a
// non-empty directory.
func TestMetastoreImportCommand(t *testing.T) {
	// Source installation: the real leader loop bootstraps the keyring with a
	// recovery envelope because the settings name the recovery key.
	recovery, err := backup.GenerateRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := recovery.StreamPublicKey()
	oldReplica, _ := backup.GenerateRecoveryKey()
	srcKr := &manager.Keyring{ReplicaID: "manager-1", Identity: oldReplica}
	src := startSource(t, t.TempDir(), freeAddr(t), srcKr)
	if _, err := manager.Put(src.Store(), manager.CollSettings, manager.SettingsKey, manager.Settings{InstallationID: "lab", VolumeBase: "/srv",
		Backup: manager.BackupSettings{RecipientKey: pub.String()}}, nil); err != nil {
		t.Fatal(err)
	}
	src.Start()
	var dataKey []byte
	for range 200 {
		if _, k, err := srcKr.Current(); err == nil {
			dataKey = k
			if r, ok, _ := manager.Get[manager.KeyringRecord](src.Store(), manager.CollKeys, manager.KeyringKey); ok && len(r.Value.Keys) == 1 && len(r.Value.Keys[0].Recovery) > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dataKey == nil {
		t.Fatal("source keyring not bootstrapped")
	}
	now := time.Now()
	if _, err := manager.Put(src.Store(), "notes", "n1", map[string]string{"text": "restored-ok"}, nil); err != nil {
		t.Fatal(err)
	}
	exp, err := manager.ExportMetastore(src.Store(), now, "lab")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := exp.Seal(pub, "metastore-lab-test.dbcms")
	if err != nil {
		t.Fatal(err)
	}
	_ = src.Close()

	work := t.TempDir()
	obj := filepath.Join(work, "metastore-lab-test.dbcms")
	keyPath := filepath.Join(work, "recovery.key")
	badKeyPath := filepath.Join(work, "wrong.key")
	other, _ := backup.GenerateRecoveryKey()
	for p, content := range map[string][]byte{obj: sealed, keyPath: []byte(recovery.String() + "\n"), badKeyPath: []byte(other.String())} {
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	addr := freeAddr(t)
	cfgPath := filepath.Join(work, "manager.json")
	cfg, _ := json.Marshal(map[string]any{"manager": map[string]any{"dataDir": dataDir, "bind": addr, "advertise": addr, "keyring": true}})
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := runMetastoreImport([]string{"-config", cfgPath, "-in", obj}, &out, &errOut); err == nil {
		t.Fatal("missing -recovery-key-file accepted")
	}
	if err := runMetastoreImport([]string{"-config", cfgPath, "-in", obj, "-recovery-key-file", badKeyPath}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "cannot open") {
		t.Fatalf("wrong key: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dataDir, "metastore")); len(entries) != 0 {
		t.Fatal("a failed import left data behind")
	}
	if err := runMetastoreImport([]string{"-config", cfgPath, "-in", obj, "-recovery-key-file", keyPath}, &out, &errOut); err != nil {
		t.Fatalf("import: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "imported") || strings.Contains(out.String(), keyPath+"\n"+recovery.String()) {
		t.Fatalf("output %q", out.String())
	}
	if strings.Contains(out.String(), recovery.String()) {
		t.Fatal("the recovery key was printed")
	}
	// A second run refuses: the directory now holds a metastore.
	if err := runMetastoreImport([]string{"-config", cfgPath, "-in", obj, "-recovery-key-file", keyPath}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("second import: %v", err)
	}

	// The restored store opens with the replica identity the tool created and holds the data.
	ident, err := manager.LoadReplicaIdentity(filepath.Join(dataDir, "replica.key"))
	if err != nil {
		t.Fatal(err)
	}
	kr := &manager.Keyring{ReplicaID: "manager-1", Identity: ident}
	re := startSource(t, filepath.Join(dataDir, "metastore"), addr, kr)
	defer re.Close()
	id, key, err := kr.Current()
	if err != nil || !bytes.Equal(key, dataKey) {
		t.Fatalf("restored keyring: %q %v", id, err)
	}
	got, ok, _ := manager.Get[map[string]string](re.Store(), "notes", "n1")
	if !ok || got.Value["text"] != "restored-ok" {
		t.Fatalf("restored document %+v", got)
	}
}
