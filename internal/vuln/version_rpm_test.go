// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import "testing"

func TestCompareRPM(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"1.0", "2.0", -1},
		{"1.0-1.el9", "1.0-2.el9", -1},
		{"1.0-10.el9", "1.0-9.el9", 1},
		{"1.0-1.el9_2", "1.0-1.el9", 1},
		{"1.0-1.el9_2.1", "1.0-1.el9_2", 1},
		{"1:1.0-1", "2.0-1", 1},    // epoch wins
		{"0:1.0-1", "1.0-1", 0},    // missing epoch is 0
		{"1.0~rc1-1", "1.0-1", -1}, // tilde sorts before
		{"1.0^git1-1", "1.0-1", 1}, // caret sorts after the end
		{"1.0^git1-1", "1.0.1-1", -1},
		{"1.0a-1", "1.0-1", 1},
		{"1.01", "1.1", 0},     // leading zeros
		{"1.2.10", "1.2.9", 1}, // numeric, not lexical
		{"1.a", "1.1", -1},     // a number beats letters
		{"1.0.1", "1.0", 1},    // the longer one wins
		{"1.0_1", "1.0.1", 0},  // separators are ignored
		{"3.0.7-1.el9", "3.0.7-27.el9", -1},
		{"6.1.0-1", "6.1.0", 0}, // a missing release is not compared
	} {
		if got := CompareRPM(c.a, c.b); got != c.want {
			t.Errorf("CompareRPM(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
		if got := CompareRPM(c.b, c.a); got != -c.want {
			t.Errorf("antisymmetry %q %q = %d", c.a, c.b, got)
		}
	}
}

func TestCompareInUsesRPMForRPMEcosystems(t *testing.T) {
	for _, eco := range []string{"almalinux", "rocky linux", "red hat", "suse", "opensuse", "fedora", "centos", "oracle linux"} {
		if CompareIn(eco, "1.0^git1-1", "1.0-1") != 1 {
			t.Errorf("%s: caret must sort after the plain version", eco)
		}
		if CompareIn(eco, "1.0-1.el9_2", "1.0-1.el9") != 1 {
			t.Errorf("%s: errata release must be newer", eco)
		}
	}
	// The Debian algorithm sorts "^" like any other non-letter after the end
	// of the string only by accident of its byte order; its rules stay as is.
	if CompareIn("debian", "1.0-1", "1.0-1") != 0 {
		t.Fatal("debian ordering must be unchanged")
	}
}

func TestAffectsRPMRange(t *testing.T) {
	r := Range{Fixed: "3.0.7-27.el9"}
	if !r.Affects("almalinux", "3.0.7-9.el9") || r.Affects("almalinux", "3.0.7-27.el9") || r.Affects("almalinux", "3.0.7-27.el9_2") {
		t.Fatal("range with release comparison")
	}
	if (Range{Fixed: "1.0-1.el9"}).Affects("rocky linux", "1.0-1.el9_2") {
		t.Fatal("el9_2 is newer than el9")
	}
}

func TestFixedVersionsSortedByEcosystem(t *testing.T) {
	a := Advisory{Affect: []Affected{{Ecosystem: "almalinux", Package: "p", Ranges: []Range{{Fixed: "1.0-10.el9"}, {Fixed: "1.0-9.el9"}, {Fixed: "1.0~rc1-1"}}}}}
	got := a.FixedVersions("almalinux", "p")
	if len(got) != 3 || got[0] != "1.0~rc1-1" || got[1] != "1.0-9.el9" || got[2] != "1.0-10.el9" {
		t.Fatalf("got %v", got)
	}
}
