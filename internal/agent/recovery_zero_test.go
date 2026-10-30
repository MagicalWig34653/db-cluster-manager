// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

const grastateZero = "# GALERA saved state\nversion: 2.1\nuuid:    00000000-0000-0000-0000-000000000000\nseqno:   -1\nsafe_to_bootstrap: 0\n"

func zeroArgs(root string, seqno int64) MarkSafeArgs {
	return MarkSafeArgs{VolumeRoot: root, Expected: testIdentity(), UUID: galera.ZeroUUID, Seqno: seqno, AllowZeroUUID: true}
}

// The zero-UUID recovery (ADR 0028) bootstraps a node whose grastate.dat
// has the zero UUID after a zero-UUID incident: only the flag is set, the
// file keeps the zero state so Galera creates a new state UUID.
func TestMarkSafeToBootstrapZeroUUIDState(t *testing.T) {
	f := newFixture(t)
	root := f.volume("shop-n1")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, root, testIdentity())
	file := filepath.Join(root, "data", GrastateFileName)
	writeRaw(t, file, grastateZero)

	// Without the explicit flag a zero UUID is never accepted.
	plain := zeroArgs(root, -1)
	plain.AllowZeroUUID = false
	if _, err := f.do(1, "op", "z0", KindMarkSafeToBootstrap, plain); err == nil {
		t.Fatal("zero UUID accepted without allow_zero_uuid")
	}
	// The flag cannot carry a real UUID or a seqno to write.
	realUUID := markArgs(root, -1)
	realUUID.AllowZeroUUID = true
	if _, err := f.do(1, "op", "z1", KindMarkSafeToBootstrap, realUUID); err == nil {
		t.Fatal("allow_zero_uuid with a real UUID accepted")
	}
	seq := int64(7)
	withSeq := zeroArgs(root, -1)
	withSeq.SetSeqno = &seq
	if _, err := f.do(1, "op", "z2", KindMarkSafeToBootstrap, withSeq); err == nil {
		t.Fatal("allow_zero_uuid with set_seqno accepted")
	}
	// A different position than approved is refused, file untouched.
	wantStatus(t, f.mustDo(1, "op", "z3", KindMarkSafeToBootstrap, zeroArgs(root, 12)), StatusRefused, CodePositionChanged)
	if string(readFile(t, file)) != grastateZero {
		t.Fatal("file changed on a refused mark")
	}
	r := f.mustDo(1, "op", "z4", KindMarkSafeToBootstrap, zeroArgs(root, -1))
	wantStatus(t, r, StatusSucceeded, "")
	if got := string(readFile(t, file)); got != SetSafeToBootstrap(grastateZero) {
		t.Fatalf("grastate = %q", got)
	}
	// Idempotent replay.
	r = f.mustDo(1, "op", "z5", KindMarkSafeToBootstrap, zeroArgs(root, -1))
	if !strings.Contains(string(r.Output), `"changed":"false"`) {
		t.Fatalf("output %s", r.Output)
	}
	// A file that holds a real history is refused even with the flag.
	writeRaw(t, file, grastateCrash)
	wantStatus(t, f.mustDo(1, "op", "z6", KindMarkSafeToBootstrap, zeroArgs(root, -1)), StatusRefused, CodePositionChanged)
}
