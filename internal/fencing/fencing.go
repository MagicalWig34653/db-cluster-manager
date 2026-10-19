// SPDX-License-Identifier: AGPL-3.0-only

// Package fencing implements the agent-side term guard. Every manager command
// carries the Raft term of the issuing leader; the guard rejects commands from
// older terms so a partitioned former leader cannot act.
package fencing

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ErrStaleTerm is returned when a command carries a term lower than the
// highest admitted term, or the invalid term 0.
var ErrStaleTerm = errors.New("fencing: stale term")

// Persister stores the highest admitted term durably.
type Persister interface {
	Load() (uint64, error) // returns 0, nil when nothing is stored yet
	Save(term uint64) error
}

type filePersister struct{ path string }

// NewFilePersister returns a Persister backed by a file holding the decimal
// term and a newline. Writes are atomic (temp file, fsync, rename).
func NewFilePersister(path string) Persister { return &filePersister{path: path} }

func (f *filePersister) Load() (uint64, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("fencing: read term file: %w", err)
	}
	s := strings.TrimSuffix(string(b), "\n")
	term, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("fencing: corrupt term file %q: %w", f.path, err)
	}
	return term, nil
}

func (f *filePersister) Save(term uint64) (err error) {
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".term-*")
	if err != nil {
		return fmt.Errorf("fencing: create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.WriteString(strconv.FormatUint(term, 10) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fencing: write term: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fencing: sync term: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("fencing: close term file: %w", err)
	}
	if err = os.Rename(tmp.Name(), f.path); err != nil {
		return fmt.Errorf("fencing: rename term file: %w", err)
	}
	// Persist the rename itself; best effort where directory sync is unsupported.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Guard admits commands only from the highest term seen so far.
type Guard struct {
	mu      sync.Mutex
	p       Persister
	highest uint64
}

// NewGuard creates a Guard and loads the stored term from p.
func NewGuard(p Persister) (*Guard, error) {
	term, err := p.Load()
	if err != nil {
		return nil, err
	}
	return &Guard{p: p, highest: term}, nil
}

// Admit accepts a command issued under term. It returns ErrStaleTerm if
// term < highest admitted term. If term > highest it persists the new value
// BEFORE returning nil. term == highest is admitted. term 0 is always rejected.
func (g *Guard) Admit(term uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if term == 0 || term < g.highest {
		return fmt.Errorf("%w: got %d, highest %d", ErrStaleTerm, term, g.highest)
	}
	if term > g.highest {
		if err := g.p.Save(term); err != nil {
			return err
		}
		g.highest = term
	}
	return nil
}

// Highest returns the highest admitted term.
func (g *Guard) Highest() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.highest
}
