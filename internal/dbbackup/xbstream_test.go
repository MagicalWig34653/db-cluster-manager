// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// xbChunk encodes one synthetic xbstream chunk (layout as documented in
// xbstream.go). A nil data slice writes an end-of-file chunk.
func xbChunk(path string, offset uint64, data []byte, eof bool) []byte {
	var b bytes.Buffer
	b.WriteString(xbMagic)
	b.WriteByte(0)
	if eof {
		b.WriteByte('E')
	} else {
		b.WriteByte('P')
	}
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(path)))
	b.WriteString(path)
	if eof {
		return b.Bytes()
	}
	_ = binary.Write(&b, binary.LittleEndian, uint64(len(data)))
	_ = binary.Write(&b, binary.LittleEndian, offset)
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(data))
	b.Write(data)
	return b.Bytes()
}

const testGaleraInfo = "6f2b1a3c-9d8e-11ef-8a1b-0242ac120002:98765 0-1-42\n"

func syntheticStream(galeraName string) []byte {
	var s []byte
	big := bytes.Repeat([]byte("x"), 70000)
	s = append(s, xbChunk("ibdata1", 0, big, false)...)
	s = append(s, xbChunk("ibdata1", 70000, big[:5], false)...)
	s = append(s, xbChunk("ibdata1", 0, nil, true)...)
	s = append(s, xbChunk(galeraName, 0, []byte(testGaleraInfo[:20]), false)...)
	s = append(s, xbChunk(galeraName, 20, []byte(testGaleraInfo[20:]), false)...)
	s = append(s, xbChunk(galeraName, 0, nil, true)...)
	s = append(s, xbChunk("mariadb_backup_info", 0, []byte("tool_name = mariabackup\n"), false)...)
	return append(s, xbChunk("mariadb_backup_info", 0, nil, true)...)
}

func TestGaleraInfoTapFindsFileInStream(t *testing.T) {
	for _, name := range []string{"mariadb_backup_galera_info", "xtrabackup_galera_info"} {
		stream := syntheticStream(name)
		// Feed in awkward pieces: every boundary falls inside some field.
		tap := newGaleraInfoTap()
		for i := 0; i < len(stream); i += 7 {
			n, err := tap.Write(stream[i:min(i+7, len(stream))])
			if err != nil || n != min(7, len(stream)-i) {
				t.Fatalf("write: %d %v", n, err)
			}
		}
		got, ok := tap.Content()
		if !ok || got != testGaleraInfo {
			t.Fatalf("%s: %q %v", name, got, ok)
		}
		gp, err := ParseGaleraInfo(got)
		if err != nil || gp.Seqno != 98765 {
			t.Fatalf("%+v %v", gp, err)
		}
	}
}

func TestGaleraInfoTapFailsClosed(t *testing.T) {
	// A corrupted checksum, a stream that is not xbstream and a truncated
	// file never yield a position.
	bad := syntheticStream("mariadb_backup_galera_info")
	i := bytes.Index(bad, []byte(testGaleraInfo[:20]))
	bad[i] ^= 0xff
	for name, s := range map[string][]byte{
		"checksum":  bad,
		"not xbs":   []byte(strings.Repeat("garbage", 100)),
		"truncated": syntheticStream("mariadb_backup_galera_info")[:len(xbChunk("ibdata1", 0, bytes.Repeat([]byte("x"), 70000), false))+40],
	} {
		tap := newGaleraInfoTap()
		if n, err := tap.Write(s); err != nil || n != len(s) {
			t.Fatalf("%s: the tap must accept every write: %d %v", name, n, err)
		}
		if got, ok := tap.Content(); ok {
			t.Fatalf("%s: position %q from a bad stream", name, got)
		}
	}
	var nilTap *galeraInfoTap
	if _, ok := nilTap.Content(); ok {
		t.Fatal("nil tap")
	}
}

// Lab E15: MariaDB 11.4 streams the binary log position in the xbstream and
// writes nothing to --extra-lsndir.
func TestInfoTapFindsBinlogInfoNextToGaleraInfo(t *testing.T) {
	const info = "dbcm-bin.000002\t383\t0-1-5\n"
	var s []byte
	s = append(s, xbChunk("ibdata1", 0, []byte("data"), false)...)
	s = append(s, xbChunk("mariadb_backup_binlog_info", 0, []byte(info[:9]), false)...)
	s = append(s, xbChunk("mariadb_backup_binlog_info", 9, []byte(info[9:]), false)...)
	s = append(s, xbChunk("mariadb_backup_binlog_info", 0, nil, true)...)
	s = append(s, syntheticStream("mariadb_backup_galera_info")...)
	tap := newGaleraInfoTap()
	for i := 0; i < len(s); i += 5 {
		_, _ = tap.Write(s[i:min(i+5, len(s))])
	}
	got, ok := tap.BinlogInfo()
	if !ok || got != info {
		t.Fatalf("%q %v", got, ok)
	}
	if g, ok := tap.Content(); !ok || g != testGaleraInfo {
		t.Fatalf("galera info lost: %q %v", g, ok)
	}
	pos, err := ParseBinlogInfo(got)
	if err != nil || pos.File != "dbcm-bin.000002" || pos.Offset != 383 {
		t.Fatalf("%+v %v", pos, err)
	}
	plain := newGaleraInfoTap()
	_, _ = plain.Write(syntheticStream("mariadb_backup_galera_info"))
	if _, ok := plain.BinlogInfo(); ok {
		t.Fatal("binlog info from a stream without it")
	}
}

// TestBackupJobGaleraPositionFromStream: lab E4 (MariaDB 11.4.13) found no
// galera_info in --extra-lsndir; the job now takes it from the stream.
func TestBackupJobGaleraPositionFromStream(t *testing.T) {
	key := testKey(t)
	be := backup.NewMemBackend()
	files := fstest.MapFS{}
	tools := newFakeTools(files, syntheticStream("mariadb_backup_galera_info"))
	tools.noGalera, tools.lsnPrefix = true, "mariadb_backup_"
	env, _ := jobEnv(t, be, key, tools, files)
	if err := RunJob(context.Background(), physicalJobArgs(t, key), env); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(context.Background(), be, "prod-b1.xbs.dbcms")
	if err != nil {
		t.Fatal(err)
	}
	if m.Galera == nil || m.Galera.Seqno != 98765 || m.Galera.StateUUID != "6f2b1a3c-9d8e-11ef-8a1b-0242ac120002" ||
		m.Galera.Source != GaleraFromStream || len(m.Warnings) != 0 {
		t.Fatalf("%+v %+v", m.Galera, m.Warnings)
	}
}
