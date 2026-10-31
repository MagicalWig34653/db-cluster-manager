// SPDX-License-Identifier: AGPL-3.0-only
package dbbackup

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// Point-in-time recovery (ADR 0021, ADR 0031): closed binlog files of the
// backup's source node are archived next to the full backup (archive job),
// and a restore replays them on top of an isolated restore of that backup
// (replay job). This file holds the archive model and the pure chain logic.

// BinlogPosition is a MariaDB file/byte boundary.
type BinlogPosition struct {
	File   string `json:"file"`
	Offset uint64 `json:"offset"`
}

func binlogNumber(file string) (string, uint64, bool) {
	prefix, suffix, ok := strings.Cut(file, ".")
	if !ok || !backupIDRe.MatchString(prefix) || len(suffix) != 6 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(suffix, 10, 64)
	return prefix, n, err == nil && n > 0
}
func (p BinlogPosition) Valid() bool { _, _, ok := binlogNumber(p.File); return ok && p.Offset >= 4 }

// ParseBinlogInfo reads mariabackup's filename, offset and optional GTID.
func ParseBinlogInfo(s string) (BinlogPosition, error) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return BinlogPosition{}, fmt.Errorf("%w: binlog_info", ErrInvalidManifest)
	}
	n, err := strconv.ParseUint(fields[1], 10, 64)
	p := BinlogPosition{File: fields[0], Offset: n}
	if err != nil || !p.Valid() {
		return p, fmt.Errorf("%w: binlog_info", ErrInvalidManifest)
	}
	return p, nil
}

// BinlogRange describes a closed file, explicitly tied to one full backup and
// one source history. From/Until are conservative coverage boundaries, not a
// claim that event timestamps are monotonic: From is the previous file's
// Until (the first file's first event), Until the file's last event. No
// source failover is inferred.
type BinlogRange struct {
	BasePosition BinlogPosition `json:"base_position"`
	BaseFinished time.Time      `json:"base_finished"`
	BaseBackupID string         `json:"base_backup_id"`
	HistoryID    string         `json:"history_id"`
	Sequence     uint64         `json:"sequence"`
	Start        BinlogPosition `json:"start"`
	End          BinlogPosition `json:"end"`
	From         time.Time      `json:"from"`
	Until        time.Time      `json:"until"`
}

func (r BinlogRange) Validate() error {
	if !r.BasePosition.Valid() || r.BaseFinished.IsZero() || !backupIDRe.MatchString(r.BaseBackupID) || r.HistoryID == "" || r.Sequence == 0 || !r.Start.Valid() || r.Start.Offset != 4 || !r.End.Valid() || r.Start.File != r.End.File || r.End.Offset <= r.Start.Offset || r.From.IsZero() || r.Until.Before(r.From) {
		return fmt.Errorf("%w: closed binlog range", ErrInvalidManifest)
	}
	return nil
}
func (r BinlogRange) binding() string {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ArchiveBinlog encrypts supplied closed-file bytes to the full backup's target,
// writing the manifest last. Callers must establish closure and event boundaries.
func ArchiveBinlog(ctx context.Context, be backup.Backend, pub backup.StreamPublicKey, base Manifest, id string, r BinlogRange, input io.Reader, now time.Time) (Manifest, error) {
	if err := base.Validate(); err != nil {
		return Manifest{}, err
	}
	if base.Method != MethodPhysical || base.BinlogBase == nil {
		return Manifest{}, ErrInvalidRequest
	}
	r.BasePosition, r.BaseFinished = *base.BinlogBase, base.Finished
	if r.BaseBackupID != base.BackupID || r.Validate() != nil || !backupIDRe.MatchString(id) || now.IsZero() {
		return Manifest{}, fmt.Errorf("%w: binlog base/range", ErrInvalidRequest)
	}
	m := Manifest{Format: ManifestFormat, BackupID: id, Method: MethodBinlog, ClusterID: base.ClusterID, SourceNodeUUID: base.SourceNodeUUID, Object: ObjectName(base.ClusterID, id, MethodBinlog, ""), Started: now, Finished: now, Tool: ToolInfo{Name: "closed-binlog", Version: base.Tool.Version}, Binlog: &r}
	pr, pw := io.Pipe()
	type result struct {
		sum backup.StreamSummary
		err error
	}
	done := make(chan result, 1)
	go func() {
		sw, err := backup.NewSealWriter(pw, backup.StreamMeta{Kind: string(MethodBinlog), BackupID: id, ClusterID: m.ClusterID, SourceNodeUUID: m.SourceNodeUUID, Created: now, Binding: r.binding()}, pub)
		var sum backup.StreamSummary
		if err == nil {
			_, err = io.Copy(sw, input)
			if err == nil {
				err = sw.Close()
				sum = sw.Summary()
			}
		}
		_ = pw.CloseWithError(err)
		done <- result{sum, err}
	}()
	stored, err := backup.PutStream(ctx, be, m.Object, pr)
	_ = pr.CloseWithError(fmt.Errorf("binlog upload ended"))
	res := <-done
	if err != nil {
		return Manifest{}, err
	}
	if res.err != nil {
		return Manifest{}, res.err
	}
	m.Stream, m.Stored = res.sum, stored
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, err
	}
	if err := be.Put(ctx, ManifestName(m.Object), bytes.NewReader(b)); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// openBinlog opens a segment, authenticates it against its manifest (header
// binding, keyed chunk digests) and returns the plaintext reader. The caller
// must read to EOF and then call check, which compares the totals.
func openBinlog(ctx context.Context, be backup.Backend, m Manifest, key backup.RecoveryKey) (io.Reader, func() error, func() error, error) {
	if err := m.Validate(); err != nil {
		return nil, nil, nil, err
	}
	if m.Method != MethodBinlog {
		return nil, nil, nil, ErrInvalidManifest
	}
	raw, err := be.Get(ctx, m.Object)
	if err != nil {
		return nil, nil, nil, err
	}
	stream, err := backup.OpenStream(raw, key)
	if err != nil {
		_ = raw.Close()
		return nil, nil, nil, err
	}
	meta := stream.Meta()
	if meta.Binding != m.Binlog.binding() || meta.Kind != string(MethodBinlog) || meta.BackupID != m.BackupID || meta.ClusterID != m.ClusterID || meta.SourceNodeUUID != m.SourceNodeUUID {
		_ = raw.Close()
		return nil, nil, nil, fmt.Errorf("%w: binlog metadata binding", ErrInvalidManifest)
	}
	return stream, func() error { return backup.CompareSummary(stream.Summary(), m.Stream) }, raw.Close, nil
}

// VerifyBinlog checks the authenticated header binding and keyed digests of
// an archived segment. A tampered range cannot redirect a replay.
func VerifyBinlog(ctx context.Context, be backup.Backend, m Manifest, key backup.RecoveryKey) error {
	r, check, closeFn, err := openBinlog(ctx, be, m, key)
	if err != nil {
		return err
	}
	defer closeFn()
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	return check()
}

// ErrPITRNotCovered reports that the binlog archive cannot restore a backup
// to the requested time. The message names the reason (no archive, a missing
// file, a changed history, or a time later than the last archived event).
var ErrPITRNotCovered = errors.New("dbbackup: point in time not covered by the binlog archive")

func notCovered(f string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrPITRNotCovered, fmt.Sprintf(f, a...))
}

// BinlogArchivePrefix is the object-name prefix of all closed-binlog
// archives of one full backup.
func BinlogArchivePrefix(clusterID, baseBackupID string) string {
	return clusterID + "-" + baseBackupID + "-l"
}

// BinlogArchiveID is the backup ID of the closed file with the given
// sequence (1 = the file that holds the base position).
func BinlogArchiveID(baseBackupID string, sequence uint64) string {
	return fmt.Sprintf("%s-l%06d", baseBackupID, sequence)
}

// ListBinlogArchives returns the manifests of all closed-binlog archives of
// one full backup, in sequence order. A manifest that cannot be read or is
// not an archive of this backup is an error: skipping it would look like a gap.
func ListBinlogArchives(ctx context.Context, be backup.Backend, clusterID, baseBackupID string) ([]Manifest, error) {
	objs, err := be.List(ctx, BinlogArchivePrefix(clusterID, baseBackupID))
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, o := range objs {
		object, ok := strings.CutSuffix(o.Name, ".manifest.json")
		if !ok {
			continue
		}
		m, err := readManifest(ctx, be, object)
		if err != nil {
			return nil, err
		}
		if m.Method != MethodBinlog || m.Binlog.BaseBackupID != baseBackupID {
			return nil, fmt.Errorf("%w: %s is not a binlog archive of %s", ErrInvalidManifest, object, baseBackupID)
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Manifest) int { return cmp.Compare(a.Binlog.Sequence, b.Binlog.Sequence) })
	return out, nil
}

// BinlogCoverage is what the archive of one full backup can restore: every
// time from From (the backup's end) to just before Until.
type BinlogCoverage struct {
	Segments int       `json:"segments"`
	From     time.Time `json:"from"`
	Until    time.Time `json:"until,omitzero"`
	// Gap is why the chain stops early; empty when every archived file is
	// part of the chain.
	Gap string `json:"gap,omitempty"`
}

// binlogChain returns the contiguous prefix of the archives that starts at
// the base backup's position, and the reason the chain stops early (empty if
// all archives are in it). Archives whose identity does not match the base
// are an error, not a gap: they are not this backup's history.
func binlogChain(base Manifest, archives []Manifest) (chain []Manifest, gap string, err error) {
	if base.Method != MethodPhysical || base.BinlogBase == nil {
		return nil, "", notCovered("backup %s has no binlog position (the binary log was off when it ran)", base.BackupID)
	}
	files := slices.Clone(archives)
	for _, m := range files {
		if m.Binlog == nil {
			return nil, "", fmt.Errorf("%w: %s is not a binlog archive", ErrInvalidManifest, m.BackupID)
		}
	}
	slices.SortFunc(files, func(a, b Manifest) int { return cmp.Compare(a.Binlog.Sequence, b.Binlog.Sequence) })
	var prev *BinlogRange
	for _, m := range files {
		if err := m.Validate(); err != nil {
			return nil, "", err
		}
		r := m.Binlog
		if m.ClusterID != base.ClusterID || m.SourceNodeUUID != base.SourceNodeUUID || r.BaseBackupID != base.BackupID || r.BasePosition != *base.BinlogBase || !r.BaseFinished.Equal(base.Finished) {
			return nil, "", fmt.Errorf("%w: archive %s does not belong to backup %s", ErrIdentityMismatch, m.BackupID, base.BackupID)
		}
		switch {
		case prev == nil && r.Sequence != 1:
			gap = fmt.Sprintf("the first archived file has sequence %d; file %s (sequence 1) is missing", r.Sequence, base.BinlogBase.File)
		case prev == nil && (r.Start.File != base.BinlogBase.File || r.End.Offset < base.BinlogBase.Offset || r.From.After(base.Finished)):
			gap = fmt.Sprintf("archived file %s does not contain the backup position %s:%d", r.Start.File, base.BinlogBase.File, base.BinlogBase.Offset)
		case prev != nil && r.Sequence != prev.Sequence+1:
			gap = fmt.Sprintf("sequence %d is missing after %d", prev.Sequence+1, prev.Sequence)
		case prev != nil && r.HistoryID != prev.HistoryID:
			gap = fmt.Sprintf("the binlog history changed after sequence %d (source node or log reset)", prev.Sequence)
		case prev != nil && !followsFile(prev.Start.File, r.Start.File):
			gap = fmt.Sprintf("file %s does not follow %s", r.Start.File, prev.Start.File)
		case prev != nil && !r.From.Equal(prev.Until):
			gap = fmt.Sprintf("time coverage is not continuous after sequence %d", prev.Sequence)
		}
		if gap != "" {
			return chain, gap, nil
		}
		chain = append(chain, m)
		prev = r
	}
	return chain, "", nil
}

func followsFile(prev, next string) bool {
	prefix, n, _ := binlogNumber(prev)
	nextPrefix, nextN, _ := binlogNumber(next)
	return prefix == nextPrefix && nextN == n+1
}

// CoverageOf summarizes what the archives of a full backup cover.
func CoverageOf(base Manifest, archives []Manifest) (BinlogCoverage, error) {
	chain, gap, err := binlogChain(base, archives)
	if err != nil {
		return BinlogCoverage{}, err
	}
	c := BinlogCoverage{Segments: len(chain), From: base.Finished, Gap: gap}
	if len(chain) > 0 {
		c.Until = chain[len(chain)-1].Binlog.Until
	}
	return c, nil
}

// ResolvePITR returns the segments to replay on top of base to reach at: the
// chain from the base position up to the first file whose last event is later
// than at. Replay includes every event up to and including the second of at.
// Event times are only second-precise and not assumed monotonic, so a time at
// or after the last archived event is not covered.
func ResolvePITR(base Manifest, archives []Manifest, at time.Time) ([]Manifest, error) {
	if at.IsZero() {
		return nil, fmt.Errorf("%w: target time required", ErrInvalidRequest)
	}
	if at.Before(base.Finished) {
		return nil, notCovered("%s is before backup %s finished (%s); pick an older time or a newer backup", at.UTC().Format(time.RFC3339), base.BackupID, base.Finished.UTC().Format(time.RFC3339))
	}
	chain, gap, err := binlogChain(base, archives)
	if err != nil {
		return nil, err
	}
	for i, m := range chain {
		if at.Before(m.Binlog.Until) {
			return chain[:i+1], nil
		}
	}
	switch {
	case gap != "":
		return nil, notCovered("%s", gap)
	case len(chain) == 0:
		return nil, notCovered("no closed binlog file is archived for backup %s", base.BackupID)
	}
	return nil, notCovered("the archive ends at %s; later changes are not archived yet", chain[len(chain)-1].Binlog.Until.UTC().Format(time.RFC3339))
}
