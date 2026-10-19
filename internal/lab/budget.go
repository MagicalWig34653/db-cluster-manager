// SPDX-License-Identifier: AGPL-3.0-only

// Package lab provides tooling for the dedicated local test environments:
// resource budgeting, host capacity detection and VM lifecycle helpers.
package lab

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

const (
	// MaxRAMBytes is the combined RAM ceiling for all lab VMs.
	MaxRAMBytes uint64 = 16 << 30
	// MaxDiskBytes is the combined disk ceiling for all lab VMs.
	MaxDiskBytes uint64 = 100 << 30
)

// Host describes the capacity of the machine running the lab.
type Host struct {
	LogicalCPUs int    // detected core count
	RAMBytes    uint64 // physical memory
}

// MaxCPUs returns floor(LogicalCPUs/2), at least 1.
func (h Host) MaxCPUs() int {
	if n := h.LogicalCPUs / 2; n > 1 {
		return n
	}
	return 1
}

// VMSpec is the resource request of one lab VM.
type VMSpec struct {
	Name      string
	CPUs      int
	RAMBytes  uint64
	DiskBytes uint64
}

// Budget enforces the combined lab resource ceiling on a host.
type Budget struct{ Host Host }

// Check validates that the sum over specs fits MaxCPUs, MaxRAMBytes and
// MaxDiskBytes and that each spec has positive values.
func (b Budget) Check(specs []VMSpec) error {
	var cpus int
	var ram, disk uint64
	for _, s := range specs {
		if s.CPUs <= 0 || s.RAMBytes == 0 || s.DiskBytes == 0 {
			return fmt.Errorf("lab: VM %q needs positive cpu, ram and disk values", s.Name)
		}
		cpus += s.CPUs
		ram += s.RAMBytes
		disk += s.DiskBytes
	}
	if max := b.Host.MaxCPUs(); cpus > max {
		return fmt.Errorf("lab: cpu budget exceeded: requested %d, limit %d", cpus, max)
	}
	if ram > MaxRAMBytes {
		return fmt.Errorf("lab: ram budget exceeded: requested %d bytes, limit %d bytes", ram, MaxRAMBytes)
	}
	if disk > MaxDiskBytes {
		return fmt.Errorf("lab: disk budget exceeded: requested %d bytes, limit %d bytes", disk, MaxDiskBytes)
	}
	return nil
}

// DetectHost returns the host capacity using runtime.NumCPU() and
// `sysctl -n hw.memsize` executed through run.
func DetectHost(run func(name string, args ...string) (string, error)) (Host, error) {
	out, err := run("sysctl", "-n", "hw.memsize")
	if err != nil {
		return Host{}, fmt.Errorf("lab: detect memory: %w", err)
	}
	ram, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return Host{}, fmt.Errorf("lab: parse hw.memsize %q: %w", strings.TrimSpace(out), err)
	}
	return Host{LogicalCPUs: runtime.NumCPU(), RAMBytes: ram}, nil
}
