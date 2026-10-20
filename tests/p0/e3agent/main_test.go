// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"
)

func call(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestFencingAndIdempotency(t *testing.T) {
	d := t.TempDir()
	steps := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"exec", "-state", d, "-term", "2", "-op", "o", "-step", "s1"}, 0, "EXECUTED o/s1 term=2"},
		{[]string{"query", "-state", d, "-op", "o", "-step", "s2"}, 0, "NOT-EXECUTED o/s2"},
		{[]string{"exec", "-state", d, "-term", "3", "-op", "o", "-step", "s1"}, 0, "ALREADY o/s1 executed under term=2"},
		{[]string{"exec", "-state", d, "-term", "2", "-op", "o", "-step", "s2"}, 3, "REJECT o/s2 term=2"},
		{[]string{"query", "-state", d, "-op", "o", "-step", "s2"}, 0, "NOT-EXECUTED o/s2"},
		{[]string{"exec", "-state", d, "-term", "0", "-op", "o", "-step", "s2"}, 3, "REJECT"},
		{[]string{"exec", "-state", d, "-term", "3", "-op", "o", "-step", "s2"}, 0, "EXECUTED o/s2 term=3"},
		{[]string{"highest", "-state", d}, 0, "highest=3"},
		{[]string{"exec", "-state", d, "-op", "o"}, 2, "required"},
		{[]string{"bogus", "-state", d}, 2, "unknown command"},
		{nil, 2, "usage"},
	}
	for i, s := range steps {
		code, out := call(t, s.args...)
		if code != s.code || !strings.Contains(out, s.want) {
			t.Fatalf("step %d %q: exit %d out %q; want exit %d containing %q", i, s.args, code, out, s.code, s.want)
		}
	}
}
