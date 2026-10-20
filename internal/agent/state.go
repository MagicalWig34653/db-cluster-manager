// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
)

const stateFormat = 1

// stepRecord is the durable record of one (OpID, StepID). Result is nil
// while the step executes; a nil Result after a restart means the outcome
// is unknown.
type stepRecord struct {
	Term       uint64  `json:"term"`
	Kind       Kind    `json:"kind"`
	ArgsDigest string  `json:"args_digest"`
	Result     *Result `json:"result,omitempty"`
}

// persistentState is the agent's state file.
type persistentState struct {
	Format int                               `json:"format"`
	Steps  map[string]map[string]*stepRecord `json:"steps"` // OpID -> StepID -> record
	// Allowed holds object IDs added at runtime: objects the executor created
	// and IDs pushed by the manager through allow-objects.
	Allowed []string `json:"allowed_ids"`
	// ConsumedTokens maps a used new-identity token ID to its node UUID.
	ConsumedTokens map[string]string `json:"consumed_tokens"`
}

func newPersistentState() *persistentState {
	return &persistentState{
		Format:         stateFormat,
		Steps:          map[string]map[string]*stepRecord{},
		ConsumedTokens: map[string]string{},
	}
}

func loadState(path string) (*persistentState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newPersistentState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: read state file: %w", err)
	}
	st := newPersistentState()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("agent: corrupt state file %q: %w", path, err)
	}
	if st.Format != stateFormat {
		return nil, fmt.Errorf("agent: unsupported state file format %d", st.Format)
	}
	if st.Steps == nil {
		st.Steps = map[string]map[string]*stepRecord{}
	}
	if st.ConsumedTokens == nil {
		st.ConsumedTokens = map[string]string{}
	}
	return st, nil
}

func (s *persistentState) save(path string) error {
	sort.Strings(s.Allowed)
	// Compact encoding: indentation would also reformat recorded outputs.
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("agent: encode state: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o600, ".agent-state-*.tmp"); err != nil {
		return fmt.Errorf("agent: write state file: %w", err)
	}
	return nil
}

func (s *persistentState) lookup(opID, stepID string) *stepRecord {
	return s.Steps[opID][stepID]
}

func (s *persistentState) put(opID, stepID string, r *stepRecord) {
	m := s.Steps[opID]
	if m == nil {
		m = map[string]*stepRecord{}
		s.Steps[opID] = m
	}
	m[stepID] = r
}

func (s *persistentState) drop(opID, stepID string) {
	delete(s.Steps[opID], stepID)
	if len(s.Steps[opID]) == 0 {
		delete(s.Steps, opID)
	}
}
