// SPDX-License-Identifier: AGPL-3.0-only

package vuln

import (
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.10", -1},
		{"1.10", "1.9", 1},
		{"v1.2.0", "1.2.0", 0},
		{"1:1.0", "2.0", 1},
		{"1.0~rc1", "1.0", -1},
		{"1.2.3-r1", "1.2.3-r2", -1},
		{"1.2.3-r10", "1.2.3-r9", 1},
		{"8.3.12-r0", "8.3.9-r1", 1},
		{"1.0a", "1.0", 1},
		{"1.0.0", "1.0", 1},
		{"007", "7", 0},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
		if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("antisymmetry %q %q", c.a, c.b)
		}
	}
	if CompareIn("npm", "1.0.0-rc.1", "1.0.0") != -1 || CompareIn("npm", "1.0.0-beta", "1.0.0-rc") != -1 {
		t.Fatal("semver pre-release")
	}
	if CompareIn("alpine", "1.2_rc1", "1.2") != -1 || CompareIn("alpine", "1.2_p1", "1.2") != 1 {
		t.Fatal("alpine suffixes")
	}
	if Major("v2.3.4") != 2 || Major("1:3.0") != 3 || Major("abc") != -1 {
		t.Fatal("major")
	}
}

func TestRangesAndExpressions(t *testing.T) {
	r := Range{Introduced: "1.0", Fixed: "1.2.3"}
	for v, want := range map[string]bool{"0.9": false, "1.0": true, "1.2.2": true, "1.2.3": false, "2.0": false} {
		if r.Affects("go", v) != want {
			t.Errorf("%s", v)
		}
	}
	la := Range{Introduced: "0", LastAffected: "2.1"}
	if !la.Affects("go", "2.1") || la.Affects("go", "2.1.1") {
		t.Fatal("last affected")
	}
	if !(Range{}).Affects("go", "99") {
		t.Fatal("open range")
	}
	af := Affected{Ecosystem: "npm", Versions: []string{"3.0.1"}}
	if !af.Affects("3.0.1") || af.Affects("3.0.2") {
		t.Fatal("exact versions")
	}
	for expr, want := range map[string]Range{
		">= 1.0.0, < 1.2.3": {Introduced: "1.0.0", Fixed: "1.2.3"},
		"< 4.17.21":         {Fixed: "4.17.21"},
		"<= 2.1":            {LastAffected: "2.1"},
	} {
		got, _, ok := ParseRangeExpr(expr)
		if !ok || got != want {
			t.Errorf("%q -> %+v %v", expr, got, ok)
		}
	}
	if _, exact, ok := ParseRangeExpr("= 3.0.1"); !ok || exact != "3.0.1" {
		t.Fatal("exact")
	}
	for _, bad := range []string{"", "> 1.0", "~1.2", ">= 1.0 < 2", "= 1, < 2"} {
		if _, _, ok := ParseRangeExpr(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAdvisoryNormalizeAndMerge(t *testing.T) {
	a := Advisory{ID: "GHSA-xxxx-yyyy-zzzz", Aliases: []string{"cve-2026-1", "CVE-2026-1"}, Sources: []string{"github"},
		Summary: strings.Repeat("s", MaxSummaryBytes+10), Severity: "", CVSS: 9.8,
		Affect: []Affected{{Ecosystem: "Alpine:v3.20", Package: " PHP83 ", Ranges: []Range{{Fixed: "8.3.12-r0"}}}}}
	n := a.Normalize()
	if n.ID != "CVE-2026-1" || len(n.Aliases) != 1 || n.Aliases[0] != "GHSA-xxxx-yyyy-zzzz" {
		t.Fatalf("%+v", n)
	}
	if len(n.Summary) != MaxSummaryBytes || n.Severity != SeverityCritical {
		t.Fatal("cap or severity from CVSS")
	}
	if n.Affect[0].Ecosystem != "alpine" || n.Affect[0].Package != "php83" {
		t.Fatal(n.Affect)
	}
	b := Advisory{ID: "CVE-2026-1", Sources: []string{"nvd"}, KEV: true, EPSS: 0.4, Severity: SeverityHigh,
		Affect:    []Affected{{Ecosystem: "alpine", Package: "php83", Ranges: []Range{{Introduced: "8.3.0"}}}, {Ecosystem: "cpe", Package: "php:php"}},
		Published: time.Unix(10, 0)}.Normalize()
	m := Merge(n, b)
	if m.ID != "CVE-2026-1" || !m.KEV || m.EPSS != 0.4 || m.Severity != SeverityCritical || len(m.Sources) != 2 || len(m.Affect) != 2 {
		t.Fatalf("%+v", m)
	}
	if len(m.Affect[0].Ranges)+len(m.Affect[1].Ranges) != 2 {
		t.Fatal("ranges merged")
	}
	if fv := m.FixedVersions("alpine", "php83"); len(fv) != 1 || fv[0] != "8.3.12-r0" {
		t.Fatal(fv)
	}
	if ParseSeverity("Moderate") != SeverityMedium || ParseSeverity("x") != SeverityUnknown {
		t.Fatal("parse severity")
	}
}

func TestPrioritize(t *testing.T) {
	p := Prioritize(SeverityCritical, 9.8, true, 0.9, false)
	if p.Score != 99 || p.Priority != P1 || !strings.Contains(p.Basis, "known exploited") {
		t.Fatalf("%+v", p)
	}
	if Prioritize(SeverityLow, 0, false, 0, false).Priority != P4 {
		t.Fatal("low")
	}
	if Prioritize(SeverityHigh, 0, false, 0, true).Score != 55 {
		t.Fatal("stateful bump")
	}
	if Prioritize(SeverityUnknown, 7.5, false, 0, false).Score != 50 {
		t.Fatal("unknown severity uses CVSS")
	}
}
