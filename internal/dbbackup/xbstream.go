// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"path"
)

// Galera position from the backup stream.
//
// mariabackup --galera-info writes the Galera position of the backup point
// into the backup as xtrabackup_galera_info (MariaDB 11.x:
// mariadb_backup_galera_info). With --stream it is a file inside the
// xbstream; MariaDB 11.4 does not copy it to --extra-lsndir (lab E4: "galera
// _info not found"). galeraInfoTap reads the plaintext stream on its way to
// the encrypting writer and keeps only that file.
//
// xbstream chunk layout (MariaDB extra/mariabackup/xbstream*.cc):
//
//	magic "XBSTCK01" | flags (1) | type (1: 'P' payload, 'E' end of file)
//	| path length (4, LE) | path
//	payload only: length (8, LE) | offset (8, LE) | CRC-32 (4, LE) | data
//
// The tap never fails the backup: an unexpected chunk ends the scan and the
// position is then reported as not recorded. Verified only against
// synthetic streams in tests, not against real mariabackup output.

const (
	xbMagic       = "XBSTCK01"
	xbHeaderLen   = len(xbMagic) + 1 + 1 + 4
	xbPayloadLen  = 8 + 8 + 4
	xbMaxPath     = 4096
	maxGaleraInfo = 4096
)

var (
	galeraInfoFiles = map[string]bool{"xtrabackup_galera_info": true, "mariadb_backup_galera_info": true}
	// binlogInfoFiles hold the binary log position of the backup point
	// (lab E15: MariaDB 11.4 streams mariadb_backup_binlog_info and writes
	// nothing to --extra-lsndir).
	binlogInfoFiles = map[string]bool{"xtrabackup_binlog_info": true, "mariadb_backup_binlog_info": true}
)

func wantedInfo(name string) bool {
	b := path.Base(name)
	return galeraInfoFiles[b] || binlogInfoFiles[b]
}

var errXBStream = errors.New("dbbackup: unexpected xbstream chunk")

type xbState int

const (
	xbHeader xbState = iota
	xbPath
	xbPayloadHeader
	xbData
)

// galeraInfoTap is an io.Writer that scans an xbstream for the Galera info
// file. It always accepts the full write.
type galeraInfoTap struct {
	state     xbState
	need      int
	buf       []byte
	typ       byte
	path      string
	remaining int64
	offset    uint64
	checksum  uint32
	collect   bool
	chunk     []byte
	content   map[string][]byte // by base name of the info file
	complete  map[string]bool
	err       error
}

func newGaleraInfoTap() *galeraInfoTap {
	return &galeraInfoTap{state: xbHeader, need: xbHeaderLen, content: map[string][]byte{}, complete: map[string]bool{}}
}

// Write implements io.Writer; it never returns an error.
func (t *galeraInfoTap) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && t.err == nil {
		if t.state == xbData {
			k := int(min(int64(len(p)), t.remaining))
			if t.collect {
				t.chunk = append(t.chunk, p[:k]...)
			}
			t.remaining -= int64(k)
			p = p[k:]
			if t.remaining == 0 {
				t.endChunk()
			}
			continue
		}
		k := min(t.need-len(t.buf), len(p))
		t.buf = append(t.buf, p[:k]...)
		p = p[k:]
		if len(t.buf) == t.need {
			t.advance()
		}
	}
	return n, nil
}

func (t *galeraInfoTap) expect(s xbState, n int) {
	t.state, t.need, t.buf = s, n, t.buf[:0]
}

func (t *galeraInfoTap) advance() {
	b := t.buf
	switch t.state {
	case xbHeader:
		if string(b[:len(xbMagic)]) != xbMagic {
			t.err = errXBStream
			return
		}
		t.typ = b[len(xbMagic)+1]
		n := binary.LittleEndian.Uint32(b[len(xbMagic)+2:])
		if n == 0 || n > xbMaxPath {
			t.err = errXBStream
			return
		}
		t.expect(xbPath, int(n))
	case xbPath:
		t.path = string(b)
		switch t.typ {
		case 'E':
			if b := path.Base(t.path); wantedInfo(b) && t.content[b] != nil {
				t.complete[b] = true
			}
			t.expect(xbHeader, xbHeaderLen)
		case 'P':
			t.expect(xbPayloadHeader, xbPayloadLen)
		default:
			t.err = errXBStream
		}
	case xbPayloadHeader:
		length := binary.LittleEndian.Uint64(b[0:8])
		t.offset = binary.LittleEndian.Uint64(b[8:16])
		t.checksum = binary.LittleEndian.Uint32(b[16:20])
		if length > 1<<40 {
			t.err = errXBStream
			return
		}
		t.remaining = int64(length)
		t.collect = wantedInfo(t.path) && length <= maxGaleraInfo
		t.chunk = t.chunk[:0]
		t.state, t.buf = xbData, t.buf[:0]
		if t.remaining == 0 {
			t.endChunk()
		}
	}
}

func (t *galeraInfoTap) endChunk() {
	if t.collect {
		b := path.Base(t.path)
		switch {
		case crc32.ChecksumIEEE(t.chunk) != t.checksum:
			t.content[b], t.collect = nil, false
		case t.offset == uint64(len(t.content[b])) && len(t.content[b])+len(t.chunk) <= maxGaleraInfo:
			t.content[b] = append(t.content[b], t.chunk...)
		default:
			t.content[b] = nil
		}
	}
	t.collect = false
	t.expect(xbHeader, xbHeaderLen)
}

func (t *galeraInfoTap) info(names map[string]bool) (string, bool) {
	if t == nil {
		return "", false
	}
	for n := range names {
		if t.complete[n] && len(t.content[n]) > 0 {
			return string(t.content[n]), true
		}
	}
	return "", false
}

// Content returns the Galera info file once the stream carried it
// completely (payload chunks and its end-of-file chunk) with valid
// checksums.
func (t *galeraInfoTap) Content() (string, bool) { return t.info(galeraInfoFiles) }

// BinlogInfo is Content for the binary log position file.
func (t *galeraInfoTap) BinlogInfo() (string, bool) { return t.info(binlogInfoFiles) }
