// SPDX-License-Identifier: AGPL-3.0-only

package opjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// snapshotVersion is the version of the snapshot document format.
const snapshotVersion = 1

// snapshotDoc is the snapshot document. It holds the current state of every
// operation, not the history of changes, so its size depends on the number
// and size of operations only.
type snapshotDoc struct {
	Version    int          `json:"version"`
	Operations []snapshotOp `json:"operations"`
}

type snapshotOp struct {
	Operation
	Aborted bool `json:"aborted,omitempty"`
}

// Snapshot returns a JSON document with the complete current state of the
// journal: every operation (including finished and aborted ones) sorted by
// ID. Equal journals produce equal bytes. Restore reads it back.
//
// Operations are never removed from the journal, so the snapshot grows with
// the number of operations ever begun; it does not grow with the number of
// commands applied to them.
func (j *Journal) Snapshot() ([]byte, error) {
	j.mu.Lock()
	doc := snapshotDoc{Version: snapshotVersion, Operations: make([]snapshotOp, 0, len(j.ops))}
	for _, e := range j.ops {
		doc.Operations = append(doc.Operations, snapshotOp{Operation: clone(e.op), Aborted: e.aborted})
	}
	j.mu.Unlock()
	sort.Slice(doc.Operations, func(a, b int) bool { return doc.Operations[a].ID < doc.Operations[b].ID })
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("opjournal: encode snapshot: %w", err)
	}
	return data, nil
}

// Restore builds a new Journal from a document written by Snapshot. It
// rejects unknown versions, duplicate or empty operation IDs, unknown step
// states, negative attempt counts and trailing data.
func Restore(data []byte) (*Journal, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc snapshotDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("opjournal: decode snapshot: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("opjournal: decode snapshot: trailing data")
	}
	if doc.Version != snapshotVersion {
		return nil, fmt.Errorf("opjournal: unsupported snapshot version %d", doc.Version)
	}
	j := New()
	for _, so := range doc.Operations {
		if so.ID == "" {
			return nil, errors.New("opjournal: snapshot operation with empty ID")
		}
		if _, ok := j.ops[so.ID]; ok {
			return nil, fmt.Errorf("opjournal: duplicate operation %q in snapshot", so.ID)
		}
		for _, s := range so.Steps {
			if !validState(s.State) {
				return nil, fmt.Errorf("opjournal: operation %q step %q has invalid state %q", so.ID, s.ID, s.State)
			}
			if s.Attempts < 0 {
				return nil, fmt.Errorf("opjournal: operation %q step %q has negative attempts", so.ID, s.ID)
			}
		}
		op := clone(so.Operation)
		if op.Steps == nil {
			op.Steps = []Step{}
		}
		j.ops[so.ID] = &entry{op: op, aborted: so.Aborted}
	}
	return j, nil
}

func validState(s State) bool {
	switch s {
	case Pending, Running, Succeeded, Failed, Unknown, Aborted:
		return true
	}
	return false
}
