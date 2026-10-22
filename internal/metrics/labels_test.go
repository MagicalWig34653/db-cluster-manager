// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"errors"
	"strings"
	"testing"
)

func TestLabelsSortedAndKey(t *testing.T) {
	a := MustLabels("up", "z", "1", "a", "2")
	if a[0].Name != "__name__" || a[1].Name != "a" || a[2].Name != "z" {
		t.Fatalf("not sorted: %v", a)
	}
	b := MustLabels("up", "a", "2", "z", "1")
	if a.Key() != b.Key() || !a.Equal(b) {
		t.Fatal("same set must have same key")
	}
	if a.Key() == MustLabels("up", "a", "2", "z", "2").Key() {
		t.Fatal("different values must differ")
	}
	if got := a.String(); got != `up{a="2",z="1"}` {
		t.Fatalf("String = %s", got)
	}
}

func TestLabelsValidate(t *testing.T) {
	o := DefaultOptions()
	bad := []Labels{
		nil,
		MustLabels("1bad"),
		MustLabels("ok", "bad-name", "x"),
		MustLabels("ok", "__reserved", "x"),
		MustLabels("ok", "a", "1", "a", "2"),
		MustLabels("ok", "v", strings.Repeat("x", o.MaxLabelValueLen+1)),
		MustLabels("ok", "v", "\xff\xfe"),
	}
	for i, l := range bad {
		if err := l.validate(&o); !errors.Is(err, ErrInvalidLabels) {
			t.Errorf("case %d: want ErrInvalidLabels, got %v", i, err)
		}
	}
	if err := MustLabels("ok", "a", "b").validate(&o); err != nil {
		t.Fatal(err)
	}
}

func TestParseSelector(t *testing.T) {
	sel, err := ParseSelector(`cpu_total{host="a",mode!="idle",job=~"db.*",env!~"te.t"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) != 5 {
		t.Fatalf("matchers = %d", len(sel))
	}
	yes := MustLabels("cpu_total", "host", "a", "mode", "user", "job", "db1", "env", "prod")
	no1 := MustLabels("cpu_total", "host", "a", "mode", "idle", "job", "db1", "env", "prod")
	no2 := MustLabels("cpu_total", "host", "a", "mode", "user", "job", "db1", "env", "test")
	no3 := MustLabels("cpu_total", "host", "a", "mode", "user", "job", "xdb1", "env", "prod")
	if !sel.Matches(yes) || sel.Matches(no1) || sel.Matches(no2) || sel.Matches(no3) {
		t.Fatal("selector semantics wrong")
	}
	// Absent label equals the empty string.
	s2, _ := ParseSelector(`up{region=""}`)
	if !s2.Matches(MustLabels("up")) || s2.Matches(MustLabels("up", "region", "x")) {
		t.Fatal("absent label should equal empty string")
	}
	if s3, err := ParseSelector(`{a="x,y}"}`); err != nil || !s3.Matches(MustLabels("m", "a", "x,y}")) {
		t.Fatalf("quoted special chars: %v", err)
	}
}

func TestParseSelectorErrors(t *testing.T) {
	for _, s := range []string{``, `{}`, `up{`, `up{a}`, `up{a="x"`, `up{a="x" b="y"}`, `up{a=~"("}`, `1up`, `up{a=x}`} {
		if _, err := ParseSelector(s); !errors.Is(err, ErrInvalidSelector) {
			t.Errorf("%q: want ErrInvalidSelector, got %v", s, err)
		}
	}
}
