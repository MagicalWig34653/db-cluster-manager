// SPDX-License-Identifier: AGPL-3.0-only

// Command agent is the per-host agent. It only reports its version so far;
// the manager protocol and Docker operations are not implemented.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if !*showVersion {
		fmt.Fprintln(os.Stderr, "agent: no operations implemented yet; use -version")
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(version.Get("agent"))
}
