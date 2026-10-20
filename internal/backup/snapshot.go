// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"encoding/json"
	"fmt"
)

// Snapshot is the manager-configuration payload of a backup. It is
// deliberately independent of other internal packages; the manager maps its
// own state into these sections.
type Snapshot struct {
	Users       []Record          `json:"users"`
	Permissions []Record          `json:"permissions"`
	Clusters    []Record          `json:"clusters"`
	Secrets     []Secret          `json:"secrets"`
	Settings    map[string]string `json:"settings,omitempty"`
}

// Record is one configuration object. Data is opaque serialized content owned
// by the manager.
type Record struct {
	ID   string `json:"id"`
	Data []byte `json:"data"`
}

// Secret is an already encrypted credential (opaque to this package). Its
// plaintext never enters a Snapshot.
type Secret struct {
	ID    string `json:"id"`
	Value []byte `json:"value"`
}

// CreateBackup serializes and seals a snapshot into a container.
func CreateBackup(s Snapshot, meta Meta, key RecoveryKey) ([]byte, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("backup: encode snapshot: %w", err)
	}
	return Seal(payload, meta, key)
}

func decodeSnapshot(data []byte, key RecoveryKey) (Snapshot, Header, error) {
	pt, h, err := Open(data, key)
	if err != nil {
		return Snapshot{}, h, err
	}
	var s Snapshot
	if err := json.Unmarshal(pt, &s); err != nil {
		return Snapshot{}, h, fmt.Errorf("%w: payload: %v", ErrCorrupt, err)
	}
	return s, h, nil
}
