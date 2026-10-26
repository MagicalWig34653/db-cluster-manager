// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"
	"time"
)

func TestSampleLine(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 1, 250e6, time.UTC)
	got := sampleLine(at, "pa", 200, []byte(`{"writer":"n1","has_writer":true,"coordination":{"epoch":3,"pending":"n2"}}`))
	if want := "12:00:01.250 pa 200 n1 true 3 n2"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got = sampleLine(at, "pb", 200, []byte(`{"writer":"","has_writer":false}`))
	if want := "12:00:01.250 pb 200 - false 0 -"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got = sampleLine(at, "pb", 503, []byte(`x`)); got != "12:00:01.250 pb 503 unparsable - - - -" {
		t.Fatalf("got %q", got)
	}
}
