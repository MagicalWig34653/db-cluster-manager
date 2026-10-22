// SPDX-License-Identifier: AGPL-3.0-only

package swarm

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Docker compatibility matrix (A09).
//
// compat.json is the maintained matrix shipped with the manager. Each entry
// matches engine versions by prefix ("29." matches 29.x.y) with a status:
//
//   - "tested": seen working in this project's lab experiments;
//   - "known-issue": works with a documented problem (warning);
//   - "unsupported": known not to work (error).
//
// A version that matches no entry, or that cannot be parsed, is a WARNING,
// never an error: host restarts and Docker upgrades are the user's
// responsibility (A09) and an untested version is not known to be broken.
// The seed contains only what the lab actually ran (Docker 29.9.1).

//go:embed compat.json
var compatJSON []byte

// Level is the severity of a compatibility finding.
type Level string

// Finding levels.
const (
	LevelOK      Level = "ok"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
)

// MatrixEntry is one row of the matrix.
type MatrixEntry struct {
	Versions string `json:"versions"` // version prefix
	Status   string `json:"status"`   // tested, known-issue, unsupported
	Note     string `json:"note"`
}

// Matrix is the compatibility matrix.
type Matrix struct {
	Format      int           `json:"format"`
	Updated     string        `json:"updated"`
	Docker      []MatrixEntry `json:"docker"`
	Recommended string        `json:"recommended"`
}

// DefaultMatrix returns the embedded matrix.
func DefaultMatrix() Matrix {
	m, err := ParseMatrix(compatJSON)
	if err != nil {
		panic("swarm: embedded compat.json: " + err.Error())
	}
	return m
}

// ParseMatrix decodes and validates a matrix document.
func ParseMatrix(b []byte) (Matrix, error) {
	var m Matrix
	if err := json.Unmarshal(b, &m); err != nil {
		return Matrix{}, fmt.Errorf("swarm: compatibility matrix: %w", err)
	}
	if m.Format != 1 {
		return Matrix{}, fmt.Errorf("swarm: compatibility matrix format %d unsupported", m.Format)
	}
	for _, e := range m.Docker {
		switch e.Status {
		case "tested", "known-issue", "unsupported":
		default:
			return Matrix{}, fmt.Errorf("swarm: compatibility entry %q: status %q", e.Versions, e.Status)
		}
		if e.Versions == "" {
			return Matrix{}, fmt.Errorf("swarm: compatibility entry without versions")
		}
	}
	return m, nil
}

// Finding is the compatibility result of one engine.
type Finding struct {
	NodeID  string `json:"nodeId"`
	Version string `json:"version"`
	Level   Level  `json:"level"`
	Message string `json:"message"`
}

var engineVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?([-+~][0-9A-Za-z.\-+~]*)?$`)

// Check evaluates one engine version. The longest matching prefix wins.
func (m Matrix) Check(nodeID, version string) Finding {
	f := Finding{NodeID: nodeID, Version: version}
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if !engineVersion.MatchString(v) {
		f.Level, f.Message = LevelWarning, fmt.Sprintf("engine version %q not recognized; compatibility unknown", version)
		return f
	}
	var best *MatrixEntry
	for i := range m.Docker {
		e := &m.Docker[i]
		if strings.HasPrefix(v, e.Versions) && (best == nil || len(e.Versions) > len(best.Versions)) {
			best = e
		}
	}
	rec := ""
	if m.Recommended != "" {
		rec = "; recommended: " + m.Recommended
	}
	switch {
	case best == nil:
		f.Level, f.Message = LevelWarning, "Docker "+v+" is not in the compatibility matrix (untested)"+rec
	case best.Status == "tested":
		f.Level, f.Message = LevelOK, "Docker "+v+" tested: "+best.Note
	case best.Status == "known-issue":
		f.Level, f.Message = LevelWarning, "Docker "+v+" has a known issue: "+best.Note+rec
	default:
		f.Level, f.Message = LevelError, "Docker "+v+" is unsupported: "+best.Note+rec
	}
	return f
}
