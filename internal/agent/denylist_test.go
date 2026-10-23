// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"
)

func TestSetDenyListPersistsAndKeepsNewerRevision(t *testing.T) {
	f := newFixture(t)
	s1, s2 := big.NewInt(0xabc), big.NewInt(0xdef)
	if f.exec.Revoked(s1) {
		t.Fatal("empty list denies")
	}
	r := f.mustDo(1, "dl-1", "h1", KindSetDenyList, SetDenyListArgs{Revision: 5, Serials: []string{"abc", "def"}})
	if r.Status != StatusSucceeded || !f.exec.Revoked(s1) || !f.exec.Revoked(s2) {
		t.Fatalf("%+v", r)
	}
	// An older list does not replace a newer one.
	r = f.mustDo(1, "dl-0", "h1", KindSetDenyList, SetDenyListArgs{Revision: 3, Serials: []string{}})
	var out DenyListOutput
	_ = json.Unmarshal(r.Output, &out)
	if !out.Kept || out.Revision != 5 || !f.exec.Revoked(s1) {
		t.Fatalf("older list applied: %+v", out)
	}
	// Survives a restart.
	f.restart()
	if !f.exec.Revoked(s2) || f.exec.DenyRevision() != 5 {
		t.Fatal("deny list lost on restart")
	}
	f.mustDo(1, "dl-2", "h1", KindSetDenyList, SetDenyListArgs{Revision: 6, Serials: []string{"def"}})
	if f.exec.Revoked(s1) || !f.exec.Revoked(s2) {
		t.Fatal("newer list not applied")
	}
	for _, bad := range []SetDenyListArgs{{Revision: -1}, {Revision: 7, Serials: []string{"ABC"}}, {Revision: 7, Serials: []string{"xyz"}}} {
		if _, err := f.do(1, "dl-bad", "h1", KindSetDenyList, bad); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	// Fenced like every command.
	f.mustDo(9, "x", "y", KindListOwned, ListOwnedArgs{ObjectKind: KindService})
	if _, err := f.do(2, "dl-3", "h1", KindSetDenyList, SetDenyListArgs{Revision: 8}); err == nil {
		t.Fatal("stale term accepted")
	}
}
