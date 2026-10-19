// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"errors"
	"strings"
	"testing"
)

func TestMaxCPUs(t *testing.T) {
	for in, want := range map[int]int{14: 7, 1: 1, 2: 1, 3: 1, 0: 1} {
		if got := (Host{LogicalCPUs: in}).MaxCPUs(); got != want {
			t.Errorf("MaxCPUs(%d) = %d, want %d", in, got, want)
		}
	}
}

func specs(n, cpus int, ram, disk uint64) []VMSpec {
	out := make([]VMSpec, n)
	for i := range out {
		out[i] = VMSpec{Name: "vm", CPUs: cpus, RAMBytes: ram, DiskBytes: disk}
	}
	return out
}

func TestCheckFits(t *testing.T) {
	b := Budget{Host: Host{LogicalCPUs: 14, RAMBytes: 64 << 30}}
	if err := b.Check(specs(3, 2, 4<<30, 20<<30)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckExceeded(t *testing.T) {
	b := Budget{Host: Host{LogicalCPUs: 14, RAMBytes: 64 << 30}}
	cases := []struct {
		name  string
		specs []VMSpec
		word  string
		nums  []string
	}{
		{"cpu", specs(1, 8, 1<<30, 1<<30), "cpu", []string{"8", "7"}},
		{"ram", specs(1, 1, MaxRAMBytes+1, 1<<30), "ram", []string{"17179869185", "17179869184"}},
		{"disk", specs(1, 1, 1<<30, MaxDiskBytes+1), "disk", []string{"107374182401", "107374182400"}},
	}
	for _, c := range cases {
		err := b.Check(c.specs)
		if err == nil {
			t.Errorf("%s: expected error", c.name)
			continue
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, c.word) {
			t.Errorf("%s: error %q lacks resource name", c.name, msg)
		}
		for _, n := range c.nums {
			if !strings.Contains(msg, n) {
				t.Errorf("%s: error %q lacks number %s", c.name, msg, n)
			}
		}
	}
}

func TestCheckRejectsZeroSpec(t *testing.T) {
	b := Budget{Host: Host{LogicalCPUs: 14}}
	if err := b.Check([]VMSpec{{Name: "x"}}); err == nil {
		t.Fatal("expected error for zero-value spec")
	}
}

func TestDetectHost(t *testing.T) {
	h, err := DetectHost(func(name string, args ...string) (string, error) {
		if name != "sysctl" {
			t.Errorf("unexpected command %q", name)
		}
		return "51539607552\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.LogicalCPUs < 1 {
		t.Errorf("LogicalCPUs = %d", h.LogicalCPUs)
	}
	if h.RAMBytes != 51539607552 {
		t.Errorf("RAMBytes = %d", h.RAMBytes)
	}
}

func TestDetectHostErrors(t *testing.T) {
	if _, err := DetectHost(func(string, ...string) (string, error) { return "abc\n", nil }); err == nil {
		t.Error("expected parse error")
	}
	if _, err := DetectHost(func(string, ...string) (string, error) { return "", errors.New("boom") }); err == nil {
		t.Error("expected run error")
	}
}
