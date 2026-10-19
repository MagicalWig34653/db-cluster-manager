// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes an external command given as an argv (never a shell
// string) and returns its standard output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// NewExecRunner returns a Runner backed by os/exec. Standard output is
// captured; on failure the error wraps the exec error and includes stderr.
func NewExecRunner() Runner { return execRunner{} }

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		argv := strings.Join(append([]string{name}, args...), " ")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.String(), fmt.Errorf("lab: %s: %w: %s", argv, err, msg)
		}
		return stdout.String(), fmt.Errorf("lab: %s: %w", argv, err)
	}
	return stdout.String(), nil
}
