// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

func TestParseCheckpoints(t *testing.T) {
	c, err := ParseCheckpoints("backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 98765\nlast_lsn = 98770\nrecover_binlog_info = 0\n")
	if err != nil || c != (Checkpoints{BackupType: "full-backuped", ToLSN: 98765, LastLSN: 98770}) {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{
		"", "from_lsn = 0\nto_lsn = 1\n", "backup_type = full\nfrom_lsn = x\nto_lsn = 1\n",
		"backup_type = full\nfrom_lsn = 5\nto_lsn = 1\n", "backup_type = full\nfrom_lsn = 0\nto_lsn = 10\nlast_lsn = 9\n",
	} {
		if _, err := ParseCheckpoints(bad); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestParseGaleraInfo(t *testing.T) {
	g, err := ParseGaleraInfo("6F2B1A3C-9d8e-11ef-8a1b-0242ac120002:1234 0-1-77\n")
	if err != nil || g.StateUUID != "6f2b1a3c-9d8e-11ef-8a1b-0242ac120002" || g.Seqno != 1234 {
		t.Fatalf("%+v %v", g, err)
	}
	for _, bad := range []string{"", "nouuid:1", "6f2b1a3c-9d8e-11ef-8a1b-0242ac120002", "6f2b1a3c-9d8e-11ef-8a1b-0242ac120002:x", "6f2b1a3c-9d8e-11ef-8a1b-0242ac120002:-5"} {
		if _, err := ParseGaleraInfo(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestParseToolVersion(t *testing.T) {
	cases := map[string]string{
		"mariabackup based on MariaDB server 10.11.6-MariaDB Linux (x86_64)":  "10.11.6-MariaDB",
		"mariadb-backup Ver 11.4.3-MariaDB for Linux on aarch64":              "11.4.3-MariaDB",
		"mariadb-dump  Ver 10.19 Distrib 10.6.18-MariaDB, for Linux (x86_64)": "10.6.18-MariaDB",
	}
	for in, want := range cases {
		if got, err := ParseToolVersion(in); err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	if _, err := ParseToolVersion("command not found"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func validManifest() Manifest {
	return Manifest{
		Format: ManifestFormat, BackupID: "b1", Method: MethodPhysical, ClusterID: testCluster, SourceNodeUUID: uuid2,
		Object: "o.xbs.dbcms", Started: t0, Finished: t0.Add(1),
		Stream: backup.StreamSummary{KeyID: "k", PlainSize: 10, PlainMAC: "mm", CipherSize: 99, CipherSHA256: "aa",
			Chunks: []backup.ChunkDigest{{Index: 0, Size: 10, MAC: "bb"}}},
		Stored:      backup.PutResult{Size: 99, SHA256: "aa"},
		Checkpoints: &Checkpoints{BackupType: "full-backuped", ToLSN: 5},
	}
}

func TestManifestValidate(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Manifest){
		"format":        func(m *Manifest) { m.Format = 3 },
		"v2 plain hash": func(m *Manifest) { m.Stream.PlainSHA256 = "cc" },
		"v2 chunk hash": func(m *Manifest) { m.Stream.Chunks[0].SHA256 = "cc" },
		"v2 no mac":     func(m *Manifest) { m.Stream.Chunks[0].MAC = "" },
		"v1 with mac":   func(m *Manifest) { m.Format = ManifestFormatV1 },
		"id":            func(m *Manifest) { m.BackupID = "B/1" },
		"cluster":       func(m *Manifest) { m.ClusterID = "" },
		"stored digest": func(m *Manifest) { m.Stored.SHA256 = "cc" },
		"chunks":        func(m *Manifest) { m.Stream.Chunks = nil },
		"chunk index":   func(m *Manifest) { m.Stream.Chunks[0].Index = 1 },
		"chunk sizes":   func(m *Manifest) { m.Stream.PlainSize = 11 },
		"time":          func(m *Manifest) { m.Finished = t0.Add(-1) },
		"checkpoints":   func(m *Manifest) { m.Checkpoints = nil },
		"method":        func(m *Manifest) { m.Method = "snapshot" },
		"logical db":    func(m *Manifest) { m.Method = MethodLogical },
		"physical db":   func(m *Manifest) { m.Database = "shop" },
	}
	for name, mut := range cases {
		m := validManifest()
		m.Stream.Chunks = append([]backup.ChunkDigest(nil), m.Stream.Chunks...)
		mut(&m)
		if err := m.Validate(); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := DecodeManifest([]byte("{")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal(err)
	}
	// Format 1 (plaintext chunk hashes) stays readable.
	v1 := validManifest()
	v1.Format, v1.Stream.PlainMAC, v1.Stream.PlainSHA256 = ManifestFormatV1, "", "pp"
	v1.Stream.Chunks = []backup.ChunkDigest{{Index: 0, Size: 10, SHA256: "bb"}}
	if err := v1.Validate(); err != nil {
		t.Fatalf("format 1: %v", err)
	}
}

func TestNamesAndIDs(t *testing.T) {
	id, err := NewBackupID(t0)
	if err != nil || !regexp.MustCompile(`^b20261022t010000-[0-9a-f]{8}$`).MatchString(id) || !backupIDRe.MatchString(id) {
		t.Fatalf("%q %v", id, err)
	}
	if n := JobName("b1", "Shop_DB"); n != "dbcm-job-b1-shop-db" {
		t.Fatal(n)
	}
	if n := JobName("b1", strings.Repeat("x", 200)); len(n) > 89 {
		t.Fatal(len(n))
	}
	if ObjectName("c", "b", MethodLogical, "shop") == ObjectName("c", "b", MethodPhysical, "") {
		t.Fatal("object names collide")
	}
	if ManifestName("o") == StatusName("o") {
		t.Fatal("manifest and status names collide")
	}
}
