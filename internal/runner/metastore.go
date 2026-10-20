// SPDX-License-Identifier: AGPL-3.0-only

package runner

import (
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
)

// MetastoreJournal implements Journal on a metastore node: writes are
// replicated through Node.Apply, reads come from the node's local journal
// (call Barrier first; the Runner does).
type MetastoreJournal struct {
	Node *metastore.Node
}

var (
	_ Journal = MetastoreJournal{}
	_ Leader  = (*metastore.Node)(nil)
)

func (m MetastoreJournal) apply(c metastore.Command) error { return m.Node.Apply(c) }

// Begin implements Journal.
func (m MetastoreJournal) Begin(opID, kind string, stepIDs []string, term uint64) error {
	return m.apply(metastore.Command{Kind: "begin", OpID: opID, OpKind: kind, Steps: stepIDs, Term: term})
}

// StartStep implements Journal.
func (m MetastoreJournal) StartStep(opID, stepID string, term uint64) error {
	return m.apply(metastore.Command{Kind: "start", OpID: opID, StepID: stepID, Term: term})
}

// FinishStep implements Journal.
func (m MetastoreJournal) FinishStep(opID, stepID string, result opjournal.State, term uint64) error {
	return m.apply(metastore.Command{Kind: "finish", OpID: opID, StepID: stepID, Result: result, Term: term})
}

// Adopt implements Journal.
func (m MetastoreJournal) Adopt(opID string, term uint64) error {
	return m.apply(metastore.Command{Kind: "adopt", OpID: opID, Term: term})
}

// Abort implements Journal.
func (m MetastoreJournal) Abort(opID string, term uint64) error {
	return m.apply(metastore.Command{Kind: "abort", OpID: opID, Term: term})
}

// Get implements Journal from the local replica.
func (m MetastoreJournal) Get(opID string) (opjournal.Operation, error) {
	return m.Node.Journal().Get(opID)
}

// Unfinished implements Journal from the local replica.
func (m MetastoreJournal) Unfinished() ([]opjournal.Operation, error) {
	return m.Node.Journal().Unfinished(), nil
}
