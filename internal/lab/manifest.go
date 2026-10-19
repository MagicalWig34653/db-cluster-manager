// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NamePrefix is required on every resource the lab creates, so that unrelated
// VMs, profiles and contexts are never touched.
const NamePrefix = "dbcm-lab-"

const manifestVersion = 1

// Resource is one lab-owned object recorded in the ownership manifest.
type Resource struct {
	Kind    string    `json:"kind"` // "lima-vm", "colima-profile", "docker-context", "directory"
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Spec    *VMSpec   `json:"spec,omitempty"`
}

// Manifest is the ownership inventory of lab resources.
type Manifest struct {
	Version   int        `json:"version"`
	Resources []Resource `json:"resources"`
}

// LoadManifest reads the manifest at path. A missing file yields an empty
// manifest; invalid JSON or an unknown version is an error.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{Version: manifestVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lab: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("lab: parse manifest %s: %w", path, err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("lab: unsupported manifest version %d", m.Version)
	}
	return &m, nil
}

// Save writes the manifest atomically (temp file + rename), creating parent
// directories with mode 0o700. The file has mode 0o600.
func (m *Manifest) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lab: create manifest dir: %w", err)
	}
	out := *m
	out.Version = manifestVersion
	if out.Resources == nil {
		out.Resources = []Resource{}
	}
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return fmt.Errorf("lab: encode manifest: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("lab: create temp manifest: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("lab: chmod temp manifest: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("lab: write manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("lab: sync manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("lab: close manifest: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("lab: replace manifest: %w", err)
	}
	return nil
}

// Add records r. Names without NamePrefix and duplicates of the same
// kind and name are rejected.
func (m *Manifest) Add(r Resource) error {
	if !strings.HasPrefix(r.Name, NamePrefix) {
		return fmt.Errorf("lab: resource name %q lacks required prefix %q", r.Name, NamePrefix)
	}
	if _, ok := m.Find(r.Kind, r.Name); ok {
		return fmt.Errorf("lab: resource %s %q already recorded", r.Kind, r.Name)
	}
	m.Resources = append(m.Resources, r)
	return nil
}

// Remove deletes the matching resource, if present.
func (m *Manifest) Remove(kind, name string) {
	kept := m.Resources[:0]
	for _, r := range m.Resources {
		if r.Kind != kind || r.Name != name {
			kept = append(kept, r)
		}
	}
	m.Resources = kept
}

// Find returns the matching resource.
func (m *Manifest) Find(kind, name string) (Resource, bool) {
	for _, r := range m.Resources {
		if r.Kind == kind && r.Name == name {
			return r, true
		}
	}
	return Resource{}, false
}
