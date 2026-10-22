// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// Method is a backup method (ADR 0009).
type Method string

// Methods.
const (
	// MethodPhysical is a full physical backup (mariabackup, xbstream).
	MethodPhysical Method = "physical"
	// MethodLogical is a logical export of one database (mariadb-dump).
	MethodLogical Method = "logical"
)

// ManifestFormat is the manifest version written by the job helper.
const ManifestFormat = 1

// Errors.
var (
	ErrInvalidManifest = errors.New("dbbackup: invalid manifest")
	ErrInvalidRequest  = errors.New("dbbackup: invalid request")
	// ErrIdentityMismatch: the backup does not belong to the cluster named
	// in the request.
	ErrIdentityMismatch = errors.New("dbbackup: backup belongs to another cluster")
	// ErrProductionTarget: a restore would target live production identity,
	// names, networks or volumes.
	ErrProductionTarget = errors.New("dbbackup: restore target overlaps production")
	// ErrApprovalRequired: a destructive operation lacks an explicit approval.
	ErrApprovalRequired = errors.New("dbbackup: explicit approval required")
)

// ToolInfo identifies the tool that produced a backup.
type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Checkpoints are the mariabackup LSN checkpoints (xtrabackup_checkpoints).
type Checkpoints struct {
	BackupType string `json:"backup_type"`
	FromLSN    uint64 `json:"from_lsn"`
	ToLSN      uint64 `json:"to_lsn"`
	LastLSN    uint64 `json:"last_lsn"`
}

// GaleraPosition is the Galera state of the source at the backup point
// (xtrabackup_galera_info: "<state uuid>:<seqno>").
type GaleraPosition struct {
	StateUUID string `json:"state_uuid"`
	Seqno     int64  `json:"seqno"`
}

// Manifest is the integrity and provenance record of one backup object.
// It is stored next to the stream as ManifestName(Object) and written last.
//
// The manifest itself is not encrypted or signed: it holds no secrets, and
// every digest in it is checked against the authenticated stream on
// restore, so a tampered manifest makes a restore fail closed.
type Manifest struct {
	Format         int                  `json:"format"`
	BackupID       string               `json:"backup_id"`
	Method         Method               `json:"method"`
	ClusterID      string               `json:"cluster_id"`
	SourceNodeUUID string               `json:"source_node_uuid"`
	Database       string               `json:"database,omitempty"`
	Object         string               `json:"object"`
	Started        time.Time            `json:"started"`
	Finished       time.Time            `json:"finished"`
	Tool           ToolInfo             `json:"tool"`
	Stream         backup.StreamSummary `json:"stream"`
	Stored         backup.PutResult     `json:"stored"`
	Checkpoints    *Checkpoints         `json:"checkpoints,omitempty"`
	Galera         *GaleraPosition      `json:"galera,omitempty"`
	// Marker is the marker value written into dbcm_meta.backup_marker before
	// the backup (empty when markers are disabled); verification expects it.
	Marker   string   `json:"marker,omitempty"`
	Catalog  *Catalog `json:"catalog,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ManifestName is the object name of the manifest of object.
func ManifestName(object string) string { return object + ".manifest.json" }

// StatusName is the object name of a job status report for key (the object
// of a backup job, the job name of a restore job).
func StatusName(key string) string { return key + ".status.json" }

// Validate checks a manifest read from a backend.
func (m Manifest) Validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(f, a...)) }
	switch {
	case m.Format != ManifestFormat:
		return bad("format %d", m.Format)
	case !backupIDRe.MatchString(m.BackupID):
		return bad("backup id %q", m.BackupID)
	case m.ClusterID == "" || m.SourceNodeUUID == "" || m.Object == "":
		return bad("cluster, source node and object required")
	case m.Stream.KeyID == "" || len(m.Stream.Chunks) == 0:
		return bad("stream summary missing")
	case m.Stored.SHA256 != m.Stream.CipherSHA256 || m.Stored.Size != m.Stream.CipherSize:
		return bad("stored object digest differs from the stream summary")
	case m.Finished.Before(m.Started):
		return bad("finished before started")
	}
	switch m.Method {
	case MethodPhysical:
		if m.Database != "" {
			return bad("physical backup with database")
		}
		if m.Checkpoints == nil {
			return bad("physical backup without checkpoints")
		}
	case MethodLogical:
		if m.Database == "" {
			return bad("logical backup without database")
		}
	default:
		return bad("method %q", m.Method)
	}
	var sum int64
	for i, c := range m.Stream.Chunks {
		if c.Index != uint64(i) {
			return bad("chunk %d has index %d", i, c.Index)
		}
		sum += int64(c.Size)
	}
	if sum != m.Stream.PlainSize {
		return bad("chunk sizes do not add up to the plaintext size")
	}
	return nil
}

// DecodeManifest parses and validates a manifest.
func DecodeManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	return m, m.Validate()
}

// ParseCheckpoints parses an xtrabackup_checkpoints file ("key = value").
func ParseCheckpoints(s string) (Checkpoints, error) {
	var c Checkpoints
	seen := map[string]bool{}
	for line := range strings.Lines(s) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		var dst *uint64
		switch k {
		case "backup_type":
			c.BackupType = v
			seen[k] = true
			continue
		case "from_lsn":
			dst = &c.FromLSN
		case "to_lsn":
			dst = &c.ToLSN
		case "last_lsn":
			dst = &c.LastLSN
		default:
			continue
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return Checkpoints{}, fmt.Errorf("%w: checkpoints %s=%q", ErrInvalidManifest, k, v)
		}
		*dst = n
		seen[k] = true
	}
	for _, k := range []string{"backup_type", "from_lsn", "to_lsn"} {
		if !seen[k] {
			return Checkpoints{}, fmt.Errorf("%w: checkpoints lack %s", ErrInvalidManifest, k)
		}
	}
	if c.ToLSN < c.FromLSN || (seen["last_lsn"] && c.LastLSN < c.ToLSN) {
		return Checkpoints{}, fmt.Errorf("%w: checkpoint LSNs out of order", ErrInvalidManifest)
	}
	return c, nil
}

var galeraUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ParseGaleraInfo parses xtrabackup_galera_info. Its first token is
// "<uuid>:<seqno>"; MariaDB may append a GTID, which is ignored.
func ParseGaleraInfo(s string) (GaleraPosition, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return GaleraPosition{}, fmt.Errorf("%w: empty galera info", ErrInvalidManifest)
	}
	u, seq, ok := strings.Cut(f[0], ":")
	if !ok || !galeraUUIDRe.MatchString(strings.ToLower(u)) {
		return GaleraPosition{}, fmt.Errorf("%w: galera info %q", ErrInvalidManifest, f[0])
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || n < -1 {
		return GaleraPosition{}, fmt.Errorf("%w: galera seqno %q", ErrInvalidManifest, seq)
	}
	return GaleraPosition{StateUUID: strings.ToLower(u), Seqno: n}, nil
}

var toolVersionRe = regexp.MustCompile(`\b(\d+\.\d+\.\d+)(-MariaDB)?\b`)

// ParseToolVersion extracts the server version from "mariabackup --version"
// or "mariadb-dump --version" output.
func ParseToolVersion(out string) (string, error) {
	m := toolVersionRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("%w: no version in %q", ErrInvalidManifest, strings.TrimSpace(out))
	}
	return m[1] + m[2], nil
}

var backupIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// NewBackupID returns an ID like "b20261022t013000-1a2b3c4d" (lowercase, so
// it fits job and object names).
func NewBackupID(now time.Time) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "b" + strings.ToLower(now.UTC().Format("20060102t150405")) + "-" + hex.EncodeToString(b[:]), nil
}

// ObjectName is the backend object of a backup stream.
func ObjectName(clusterID, backupID string, m Method, database string) string {
	if m == MethodLogical {
		return clusterID + "-" + backupID + "-db-" + database + ".sql.dbcms"
	}
	return clusterID + "-" + backupID + ".xbs.dbcms"
}

// JobName is the job service name of a backup or restore step.
func JobName(backupID, suffix string) string {
	n := "dbcm-job-" + backupID
	if suffix != "" {
		n += "-" + strings.ToLower(strings.ReplaceAll(suffix, "_", "-"))
	}
	if len(n) > 89 {
		n = n[:89]
	}
	return strings.TrimRight(n, "-")
}
