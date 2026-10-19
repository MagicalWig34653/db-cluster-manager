// SPDX-License-Identifier: AGPL-3.0-only

// Command cli is the command-line client. Only "version" exists so far.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		_ = json.NewEncoder(os.Stdout).Encode(version.Get("cli"))
		return
	}
	fmt.Fprintln(os.Stderr, "usage: cli version")
	os.Exit(2)
}
