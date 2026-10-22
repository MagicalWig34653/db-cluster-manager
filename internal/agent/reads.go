// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// Read-only evidence collection for the manager's health model and the
// guided full-outage diagnosis.

// GrastateFileName is the Galera state file in the datadir.
const GrastateFileName = "grastate.dat"

// DefaultDataDir is the datadir relative to the volume root (galera.DataDir
// below galera.VolumeTarget).
const DefaultDataDir = "data"

const maxGrastateSize = 64 << 10

func (e *Executor) collectHealth(ctx context.Context, a CollectHealthArgs) Result {
	if e.cfg.Status == nil {
		return Result{Status: StatusRefused, Code: CodeNotConfigured, Message: "agent has no monitoring credentials configured"}
	}
	network, addr := "tcp", a.Address
	if a.Socket != "" {
		// The socket must exist and, with every symlink resolved, stay
		// below the volume base (a dangling link is refused: its target
		// could appear anywhere later).
		resolved, err := filepath.EvalSymlinks(a.Socket)
		if err != nil {
			return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: err.Error()}
		}
		if !within(e.volumeBase, resolved) {
			return Result{Status: StatusRefused, Code: CodeInvalidPath,
				Message: fmt.Sprintf("socket %s resolves to %s outside the volume base", a.Socket, resolved)}
		}
		network, addr = "unix", resolved
	}
	vars, err := e.cfg.Status.Status(ctx, network, addr)
	if err != nil {
		if ctx.Err() != nil {
			return Result{Status: StatusUnknown, Code: CodeInDoubt, Message: ctx.Err().Error()}
		}
		return Result{Status: StatusFailed, Code: CodeDBUnavailable, Message: err.Error()}
	}
	out := HealthOutput{Status: map[string]string{}}
	for k, v := range vars {
		if strings.HasPrefix(k, "wsrep_") {
			out.Status[k] = v
		}
	}
	return succeeded(out)
}

// readGrastate verifies the node identity, then reads <root>/<datadir>/
// grastate.dat through an os.Root, so neither ".." nor a symbolic link can
// leave the volume root. Only a regular file of bounded size is read.
func (e *Executor) readGrastate(a ReadGrastateArgs) Result {
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(root, a.Expected); err != nil {
		return outcome(err)
	}
	dataDir := a.DataDir
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return outcome(err)
	}
	defer r.Close()
	f, err := r.Open(filepath.Join(dataDir, GrastateFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return succeeded(GrastateOutput{Present: false})
	}
	if err != nil {
		return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: err.Error()}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return outcome(err)
	}
	if !fi.Mode().IsRegular() {
		return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: GrastateFileName + " is not a regular file"}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxGrastateSize+1))
	if err != nil {
		return outcome(err)
	}
	if len(b) > maxGrastateSize {
		return Result{Status: StatusFailed, Code: CodeIO, Message: fmt.Sprintf("%s larger than %d bytes", GrastateFileName, maxGrastateSize)}
	}
	out := GrastateOutput{Present: true, Content: string(b)}
	if g, err := galera.ParseGrastate(out.Content); err != nil {
		out.ParseError = err.Error()
	} else {
		out.Grastate = &g
	}
	return succeeded(out)
}
