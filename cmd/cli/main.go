// SPDX-License-Identifier: AGPL-3.0-only

// Command cli is the operator command-line client.
package main

import (
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, nil))
}
