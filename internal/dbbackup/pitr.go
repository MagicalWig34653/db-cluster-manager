// SPDX-License-Identifier: AGPL-3.0-only
package dbbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// BinlogPosition is a MariaDB file/byte boundary. Only event boundaries supplied
// by an inspected archive are meaningful; the groundwork does not parse events.
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
// claim that event timestamps are monotonic. No source failover is inferred.
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
	if !r.BasePosition.Valid() || r.BaseFinished.IsZero() || !backupIDRe.MatchString(r.BaseBackupID) || r.HistoryID == "" || r.Sequence == 0 || !r.Start.Valid() || r.Start.Offset != 4 || !r.End.Valid() || r.Start.File != r.End.File || r.End.Offset <= r.Start.Offset || r.From.IsZero() || !r.Until.After(r.From) {
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
// writing the manifest last. Callers must establish closure and event boundaries;
// no continuous archiver or MariaDB configuration is enabled by this function.
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

// VerifyBinlog checks the authenticated header binding and keyed digests before
// a fake replay receives a segment. A tampered range cannot redirect a replay.
func VerifyBinlog(ctx context.Context, be backup.Backend, m Manifest, key backup.RecoveryKey) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Method != MethodBinlog {
		return ErrInvalidManifest
	}
	raw, err := be.Get(ctx, m.Object)
	if err != nil {
		return err
	}
	defer raw.Close()
	stream, err := backup.OpenStream(raw, key)
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return err
	}
	meta := stream.Meta()
	if meta.Binding != m.Binlog.binding() || meta.Kind != string(MethodBinlog) || meta.BackupID != m.BackupID || meta.ClusterID != m.ClusterID || meta.SourceNodeUUID != m.SourceNodeUUID {
		return fmt.Errorf("%w: binlog metadata binding", ErrInvalidManifest)
	}
	return backup.CompareSummary(stream.Summary(), m.Stream)
}

// PITRTarget selects exactly one stop condition. Time replay needs MariaDB event
// semantics validation in the lab; it is deliberately available only to fakes.
type PITRTarget struct {
	Time     *time.Time      `json:"time,omitempty"`
	Position *BinlogPosition `json:"position,omitempty"`
}

type PITRPlan struct {
	BasePosition BinlogPosition   `json:"base_position"`
	Inspection   InspectionResult `json:"inspection"`
	Segments     []Manifest       `json:"segments"`
	Target       PITRTarget       `json:"target"`
	Digest       string           `json:"digest"`
}

func (p PITRPlan) digest() string {
	p.Digest = ""
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// PlanPITR preserves all inspection isolation and approval gates. Its replay
// description is separate from agent commands: there is no production executor.
func PlanPITR(req InspectionRequest, inv Inventory, archives []Manifest, target PITRTarget) (PITRPlan, error) {
	bad := func() (PITRPlan, error) {
		return PITRPlan{}, fmt.Errorf("%w: PITR gap, identity or target", ErrInvalidRequest)
	}
	base := req.Source.Manifest
	if base.BinlogBase == nil || (target.Time == nil) == (target.Position == nil) {
		return bad()
	}
	if target.Time != nil && target.Time.Before(base.Finished) {
		return bad()
	}
	if target.Position != nil && !target.Position.Valid() {
		return bad()
	}
	files := append([]Manifest(nil), archives...)
	sort.Slice(files, func(i, j int) bool {
		if files[i].Binlog == nil {
			return false
		}
		if files[j].Binlog == nil {
			return true
		}
		return files[i].Binlog.Sequence < files[j].Binlog.Sequence
	})
	plan := PITRPlan{Target: target, BasePosition: *base.BinlogBase}
	var prev *BinlogRange
	reached := false
	for _, m := range files {
		if err := m.Validate(); err != nil {
			return PITRPlan{}, err
		}
		r := m.Binlog
		if r == nil || m.ClusterID != base.ClusterID || m.SourceNodeUUID != base.SourceNodeUUID || r.BaseBackupID != base.BackupID || r.BasePosition != *base.BinlogBase || !r.BaseFinished.Equal(base.Finished) {
			return bad()
		}
		if prev == nil {
			if r.Sequence != 1 || r.Start.File != base.BinlogBase.File || r.Start.Offset > base.BinlogBase.Offset || r.End.Offset < base.BinlogBase.Offset || r.From.After(base.Finished) {
				return bad()
			}
		} else {
			prefix, n, _ := binlogNumber(prev.Start.File)
			nextPrefix, nextN, _ := binlogNumber(r.Start.File)
			if r.Sequence != prev.Sequence+1 || r.HistoryID != prev.HistoryID || prefix != nextPrefix || nextN != n+1 || r.Start.Offset != 4 || !r.From.Equal(prev.Until) {
				return bad()
			}
		}
		plan.Segments = append(plan.Segments, m)
		if target.Time != nil {
			reached = !target.Time.After(r.Until)
		} else {
			reached = target.Position.File == r.Start.File && target.Position.Offset >= r.Start.Offset && target.Position.Offset <= r.End.Offset
			if prev == nil && target.Position.File == base.BinlogBase.File && target.Position.Offset < base.BinlogBase.Offset {
				return bad()
			}
		}
		if reached {
			break
		}
		prev = r
	}
	if !reached {
		return bad()
	}
	inspection, err := InspectionRestorePlan(req, inv)
	if err != nil {
		return PITRPlan{}, err
	}
	// PITR always includes the existing agent target guard explicitly, even
	// when job and target are on the same host. RunRestore still checks again.
	for i, step := range inspection.Operation.Steps {
		if step.Command != agent.KindRunRestore {
			continue
		}
		if i > 0 && inspection.Operation.Steps[i-1].Command == agent.KindCheckRestoreTarget {
			break
		}
		var a agent.RunRestoreArgs
		if err := json.Unmarshal(step.Args, &a); err != nil {
			return PITRPlan{}, err
		}
		guard := command("pitr-check-restore-target", req.Host.ID, agent.KindCheckRestoreTarget, agent.CheckRestoreTargetArgs{TargetVolume: a.TargetVolume, TargetVolumeRoot: a.TargetVolumeRoot, Expected: a.Expected})
		guard.Preconditions = append([]runner.Condition(nil), step.Preconditions...)
		guard.Timeout, guard.MaxRetries = step.Timeout, step.MaxRetries
		inspection.Operation.Steps[i].Preconditions = append(inspection.Operation.Steps[i].Preconditions, runner.Condition{Kind: runner.CondStepSucceeded, StepID: guard.ID})
		inspection.Operation.Steps = slices.Insert(inspection.Operation.Steps, i, guard)
		break
	}
	if err := inspection.Operation.Validate(); err != nil {
		return PITRPlan{}, err
	}
	plan.Inspection = inspection
	plan.Digest = plan.digest()
	return plan, nil
}

// PITRFakeExecutor is intentionally not an agent transport. Implementations in
// tests record target checks, the isolated restore and ordered replay requests.
// There is no implementation that runs mariadb-binlog or promotes a cluster.
type PITRFakeExecutor interface {
	CheckRestoreTarget(context.Context, agent.CheckRestoreTargetArgs) error
	InspectionRestore(context.Context, InspectionResult) error
	Replay(context.Context, Manifest, BinlogPosition, PITRTarget) error
}

func ExecutePITRFake(ctx context.Context, p PITRPlan, approvedDigest string, be backup.Backend, key backup.RecoveryKey, fake PITRFakeExecutor) error {
	if approvedDigest == "" || approvedDigest != p.Digest || p.Digest != p.digest() {
		return ErrApprovalRequired
	}
	// Verify all segments before the fake target or restore can have effects.
	for _, m := range p.Segments {
		if err := VerifyBinlog(ctx, be, m, key); err != nil {
			return err
		}
	}
	s := p.Inspection.Session
	var expected agent.Identity
	for _, step := range p.Inspection.Operation.Steps {
		if step.Command == agent.KindRunRestore {
			b, _ := json.Marshal(step.Args)
			var a agent.RunRestoreArgs
			if err := json.Unmarshal(b, &a); err != nil {
				return err
			}
			expected = a.Expected
		}
	}
	if !strings.HasPrefix(expected.ClusterID, InspectionPrefix) {
		return ErrProductionTarget
	}
	if err := fake.CheckRestoreTarget(ctx, agent.CheckRestoreTargetArgs{TargetVolume: s.Volume, TargetVolumeRoot: s.VolumeRoot, Expected: expected}); err != nil {
		return err
	}
	if err := fake.InspectionRestore(ctx, p.Inspection); err != nil {
		return err
	}
	for i, m := range p.Segments {
		start := m.Binlog.Start
		if i == 0 {
			start = p.BasePosition
		}
		if err := fake.Replay(ctx, m, start, p.Target); err != nil {
			return err
		}
	}
	return nil
}
