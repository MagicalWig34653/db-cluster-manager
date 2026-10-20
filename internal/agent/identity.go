// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// MarkerFileName is the identity marker's file name at the volume root. The
// marker lives outside the database datadir because an SST empties the
// datadir (lab experiment E1).
const MarkerFileName = "dbcm-identity.json"

const (
	markerTempPattern = ".dbcm-identity-*.tmp"
	markerFormat      = 1
	markerMaxSize     = 64 << 10
	lostAndFound      = "lost+found"
)

// Identity is the durable identity of one database node's local volume.
type Identity struct {
	InstallationID string `json:"installation_id"`
	ClusterID      string `json:"cluster_id"`
	NodeUUID       string `json:"node_uuid"`
}

func (id Identity) complete() bool {
	return id.InstallationID != "" && id.ClusterID != "" && validUUID(id.NodeUUID)
}

// NewIdentityToken authorizes initializing an empty volume with a new node
// identity. The manager issues it in its explicit new-identity operation
// (ADR 0004 item 4). The executor accepts each TokenID only once.
type NewIdentityToken struct {
	TokenID  string   `json:"token_id"`
	Identity Identity `json:"identity"`
}

type markerFile struct {
	Format int `json:"format"`
	Identity
}

// Identity decision errors. The comments give the exit status of
// tests/p0/marker-guard.sh for the same decision.
var (
	ErrIdentityNotConfigured = errors.New("agent: expected identity not configured")                           // 64
	ErrForeignMarker         = errors.New("agent: identity marker belongs to another installation or cluster") // 65
	ErrIdentityMismatch      = errors.New("agent: identity marker belongs to another node")                    // 65
	ErrMarkerCorrupt         = errors.New("agent: identity marker unreadable or incomplete")                   // 65
	ErrUnknownData           = errors.New("agent: marker missing but volume not empty")                        // 66
	ErrEmptyVolume           = errors.New("agent: marker missing and volume empty")                            // 67
)

// IdentityError describes a refused identity decision. Kind is one of the
// Err* identity sentinels; errors.Is matches against it.
type IdentityError struct {
	Kind       error
	VolumeRoot string
	Expected   Identity
	Found      *Identity // marker content, when a marker was parsed
	Detail     string
}

func (e *IdentityError) Error() string {
	var b strings.Builder
	b.WriteString(e.Kind.Error())
	fmt.Fprintf(&b, " (volume %s", e.VolumeRoot)
	if e.Found != nil {
		fmt.Fprintf(&b, "; found node_uuid=%s installation=%s cluster=%s",
			e.Found.NodeUUID, e.Found.InstallationID, e.Found.ClusterID)
	}
	fmt.Fprintf(&b, "; expected node_uuid=%s installation=%s cluster=%s",
		e.Expected.NodeUUID, e.Expected.InstallationID, e.Expected.ClusterID)
	if e.Detail != "" {
		b.WriteString("; ")
		b.WriteString(e.Detail)
	}
	b.WriteString(")")
	return b.String()
}

func (e *IdentityError) Unwrap() error { return e.Kind }

// Verify allows a database start on volumeRoot only if the marker there
// matches expected in installation, cluster and node UUID. It fails closed:
//
//   - expected incomplete: ErrIdentityNotConfigured
//   - marker for another installation or cluster: ErrForeignMarker
//   - marker for another node of the same cluster: ErrIdentityMismatch
//   - marker not a regular file, unparsable or incomplete: ErrMarkerCorrupt
//   - no marker, volume holds data: ErrUnknownData
//   - no marker, volume empty or absent: ErrEmptyVolume; only Initialize
//     with a new-identity token may resolve this.
//
// A volume counts as empty when it contains no non-directory entry anywhere,
// ignoring lost+found and leftover marker temp files at the root.
func Verify(volumeRoot string, expected Identity) error {
	if !expected.complete() {
		return &IdentityError{Kind: ErrIdentityNotConfigured, VolumeRoot: volumeRoot, Expected: expected}
	}
	found, err := readMarker(volumeRoot, expected)
	if err != nil {
		return err
	}
	if found != nil {
		return compareMarker(volumeRoot, expected, *found)
	}
	return missingMarkerError(volumeRoot, expected)
}

// Initialize writes a new marker for tok.Identity to an empty volume without
// marker. It is idempotent: if the marker already holds tok.Identity it
// returns nil. It refuses a volume with data or with any other marker. The
// caller is responsible for accepting each token only once.
func Initialize(volumeRoot string, tok NewIdentityToken) error {
	if tok.TokenID == "" || !tok.Identity.complete() {
		return &IdentityError{Kind: ErrIdentityNotConfigured, VolumeRoot: volumeRoot, Expected: tok.Identity,
			Detail: "new-identity token incomplete"}
	}
	st, err := os.Stat(volumeRoot)
	if err != nil {
		return fmt.Errorf("agent: volume root: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("agent: volume root %s is not a directory", volumeRoot)
	}
	found, err := readMarker(volumeRoot, tok.Identity)
	if err != nil {
		return err
	}
	if found != nil {
		return compareMarker(volumeRoot, tok.Identity, *found)
	}
	empty, err := volumeEmpty(volumeRoot)
	if err != nil {
		return fmt.Errorf("agent: inspect volume %s: %w", volumeRoot, err)
	}
	if !empty {
		return &IdentityError{Kind: ErrUnknownData, VolumeRoot: volumeRoot, Expected: tok.Identity}
	}
	if err := removeMarkerTemps(volumeRoot); err != nil {
		return err
	}
	data, err := json.Marshal(markerFile{Format: markerFormat, Identity: tok.Identity})
	if err != nil {
		return fmt.Errorf("agent: encode marker: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(volumeRoot, MarkerFileName), data, 0o644, markerTempPattern); err != nil {
		return fmt.Errorf("agent: write marker: %w", err)
	}
	return nil
}

// readMarker returns the parsed marker, nil if no marker exists, or an
// IdentityError for a corrupt marker.
func readMarker(volumeRoot string, expected Identity) (*Identity, error) {
	path := filepath.Join(volumeRoot, MarkerFileName)
	corrupt := func(detail string) error {
		return &IdentityError{Kind: ErrMarkerCorrupt, VolumeRoot: volumeRoot, Expected: expected, Detail: detail}
	}
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: stat marker: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, corrupt("marker is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("agent: open marker: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, markerMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("agent: read marker: %w", err)
	}
	if len(data) > markerMaxSize {
		return nil, corrupt("marker too large")
	}
	var m markerFile
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, corrupt("marker is not valid JSON")
	}
	if m.InstallationID == "" || m.ClusterID == "" || m.NodeUUID == "" {
		id := m.Identity
		e := corrupt("marker misses installation_id, cluster_id or node_uuid").(*IdentityError)
		e.Found = &id
		return nil, e
	}
	id := m.Identity
	return &id, nil
}

func compareMarker(volumeRoot string, expected, found Identity) error {
	switch {
	case found.InstallationID != expected.InstallationID || found.ClusterID != expected.ClusterID:
		return &IdentityError{Kind: ErrForeignMarker, VolumeRoot: volumeRoot, Expected: expected, Found: &found}
	case found.NodeUUID != expected.NodeUUID:
		return &IdentityError{Kind: ErrIdentityMismatch, VolumeRoot: volumeRoot, Expected: expected, Found: &found}
	}
	return nil
}

func missingMarkerError(volumeRoot string, expected Identity) error {
	empty, err := volumeEmpty(volumeRoot)
	if err != nil {
		return fmt.Errorf("agent: inspect volume %s: %w", volumeRoot, err)
	}
	if empty {
		return &IdentityError{Kind: ErrEmptyVolume, VolumeRoot: volumeRoot, Expected: expected,
			Detail: "volume lost or replaced; needs an explicit new-identity operation"}
	}
	return &IdentityError{Kind: ErrUnknownData, VolumeRoot: volumeRoot, Expected: expected,
		Detail: "data of unknown identity"}
}

var errNotEmpty = errors.New("not empty")

// volumeEmpty reports whether root holds no non-directory entry. A missing
// root counts as empty, as in marker-guard.sh.
func volumeEmpty(root string) (bool, error) {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if path == root {
			return nil
		}
		if filepath.Dir(path) == root {
			if d.Name() == lostAndFound && d.IsDir() {
				return fs.SkipDir
			}
			if ok, _ := filepath.Match(markerTempPattern, d.Name()); ok && d.Type().IsRegular() {
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		return errNotEmpty
	})
	if errors.Is(err, errNotEmpty) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func removeMarkerTemps(root string) error {
	matches, err := filepath.Glob(filepath.Join(root, markerTempPattern))
	if err != nil {
		return fmt.Errorf("agent: list marker temp files: %w", err)
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("agent: remove marker temp file: %w", err)
		}
	}
	return nil
}

// validUUID accepts the canonical 8-4-4-4-12 hexadecimal form.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}
