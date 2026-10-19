// SPDX-License-Identifier: AGPL-3.0-only

// Package version carries build identification shared by manager, agent and CLI.
package version

import "runtime"

// Version and Commit are overridden at link time:
//
//	-ldflags "-X github.com/MagicalWig34653/db-cluster-manager/internal/version.Version=..."
var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
)

// Info describes a running binary.
type Info struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build information for the named component.
func Get(component string) Info {
	return Info{
		Component: component,
		Version:   Version,
		Commit:    Commit,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}
