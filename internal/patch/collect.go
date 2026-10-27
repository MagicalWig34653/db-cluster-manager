// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"strings"
	"time"
)

// Collector builds a HostReport. It only reads, except for the optional
// index refresh.
type Collector struct {
	HostID  string
	Runner  Runner
	FS      FS
	Now     func() time.Time
	Refresh bool
}

// ParseOSRelease parses /etc/os-release.
func ParseOSRelease(s string) OSInfo {
	var o OSInfo
	for _, l := range lines(s) {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			o.ID = v
		case "VERSION_ID":
			o.VersionID = v
		case "PRETTY_NAME":
			o.PrettyName = v
		}
	}
	return o
}

// Collect gathers the report.
func (c Collector) Collect(ctx context.Context) (HostReport, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	rep := HostReport{HostID: c.HostID, CollectedAt: now().UTC(), RebootRequired: Unknown}
	if b, err := c.FS.ReadFile("/etc/os-release"); err == nil {
		rep.OS = ParseOSRelease(string(b))
	} else {
		rep.Warnings = append(rep.Warnings, "/etc/os-release unreadable")
	}
	if res, err := c.Runner.Run(ctx, "uname", "-rm"); err == nil && res.ExitCode == 0 {
		if f := strings.Fields(res.Stdout); len(f) == 2 {
			rep.OS.Kernel, rep.OS.Arch = f[0], f[1]
		}
	} else {
		rep.Warnings = append(rep.Warnings, "uname failed")
	}
	m, err := Detect(c.FS)
	if err != nil {
		return rep, err
	}
	ad, err := NewAdapter(m)
	if err != nil {
		return rep, err
	}
	rep.Manager = m
	if c.Refresh {
		if err := ad.Refresh(ctx, c.Runner); err != nil {
			rep.Warnings = append(rep.Warnings, "index refresh failed: "+err.Error())
		}
	}
	if rep.Installed, err = ad.Installed(ctx, c.Runner); err != nil {
		return rep, err
	}
	var warn []string
	if rep.Updates, rep.SecurityKnown, warn, err = ad.Updates(ctx, c.Runner); err != nil {
		return rep, err
	}
	rep.Warnings = append(rep.Warnings, warn...)
	rep.RebootRequired, rep.RebootReasons = ad.RebootRequired(ctx, c.Runner, c.FS, rep.Installed, rep.OS.Kernel)
	if rep.Updates == nil {
		rep.Updates = []Update{}
	}
	return rep, nil
}
