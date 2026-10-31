// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// Point-in-time recovery jobs (ADR 0031).
//
// The archive job runs next to the source node of a full backup with the
// node's volume mounted read-only. It rotates the binary log, then encrypts
// every closed file from the backup's position on to the backup target, one
// archive per file (ArchiveBinlog). The replay job runs against an isolated
// inspection instance restored from the full backup: it authenticates the
// archived files, decrypts them into its scratch space and pipes
// mariadb-binlog into the client.

var binlogMagic = []byte{0xfe, 'b', 'i', 'n'}

const binlogEventHeader = 19

// scanBinlog walks the events of a closed binlog file of the given size and
// returns the first and the last event timestamp. It rejects a file whose
// event chain does not end exactly at size, so a truncated copy is never
// archived. Timestamp 0 (artificial events) is ignored.
func scanBinlog(r io.Reader, size int64) (first, last time.Time, err error) {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: binlog file: %s", ErrJob, fmt.Sprintf(f, a...)) }
	br := bufio.NewReader(r)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || !bytes.Equal(magic[:], binlogMagic) {
		return first, last, bad("not a binlog file")
	}
	off := int64(4)
	var hdr [binlogEventHeader]byte
	for off < size {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return first, last, bad("truncated event header at offset %d", off)
		}
		ts := int64(binary.LittleEndian.Uint32(hdr[0:4]))
		n := int64(binary.LittleEndian.Uint32(hdr[9:13]))
		next := int64(binary.LittleEndian.Uint32(hdr[13:17]))
		if n < binlogEventHeader || off+n > size || next != off+n {
			return first, last, bad("inconsistent event at offset %d", off)
		}
		if ts != 0 {
			t := time.Unix(ts, 0).UTC()
			if first.IsZero() {
				first = t
			}
			if t.After(last) {
				last = t
			}
		}
		if _, err := io.CopyN(io.Discard, br, n-binlogEventHeader); err != nil {
			return first, last, bad("truncated event at offset %d", off)
		}
		off += n
	}
	if first.IsZero() {
		return first, last, bad("no timestamped event")
	}
	return first, last, nil
}

// binlogIndex returns the file names of the server's binlog index, oldest
// first. The last entry is the file the server is writing.
func (e JobEnv) binlogIndex(datadir, prefix string) ([]string, error) {
	raw, err := e.read(path.Join(datadir, prefix+".index"))
	if err != nil {
		return nil, fmt.Errorf("%w: binlog index: %v", ErrJob, err)
	}
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, path.Base(line))
		}
	}
	return names, nil
}

func binlogName(prefix string, n uint64) string { return fmt.Sprintf("%s.%06d", prefix, n) }

// baseOf reads and checks the full backup a PITR job extends.
func baseOf(ctx context.Context, be backup.Backend, s JobSpec, clusterID string) (Manifest, error) {
	base, err := readManifest(ctx, be, s.Object)
	switch {
	case err != nil:
		return Manifest{}, err
	case base.BackupID != s.BackupID || base.Object != s.Object:
		return Manifest{}, fmt.Errorf("%w: manifest is for backup %s object %s", ErrIdentityMismatch, base.BackupID, base.Object)
	case base.ClusterID != clusterID:
		return Manifest{}, fmt.Errorf("%w: backup from %s, job expects %s", ErrIdentityMismatch, base.ClusterID, clusterID)
	case base.Method != MethodPhysical || base.BinlogBase == nil:
		return Manifest{}, fmt.Errorf("%w: backup %s has no binlog position (the binary log was off when it ran)", ErrJob, base.BackupID)
	}
	return base, nil
}

// runArchiveJob archives the closed binlog files that extend the full backup
// s.BackupID and are not archived yet. It is idempotent: with nothing new it
// succeeds without writing. A file the chain needs but the node no longer
// has is an error (the archive would have a gap).
func runArchiveJob(ctx context.Context, s JobSpec, env JobEnv, be backup.Backend) (bool, error) {
	pub, err := backup.ParseStreamPublicKey(s.Recipient)
	if err != nil {
		return false, err
	}
	base, err := baseOf(ctx, be, s, s.ClusterID)
	if err != nil {
		return false, err
	}
	if base.SourceNodeUUID != s.NodeUUID {
		return false, fmt.Errorf("%w: backup %s was taken from node %s, this job reads %s; take a new full backup", ErrIdentityMismatch, base.BackupID, base.SourceNodeUUID, s.NodeUUID)
	}
	existing, err := ListBinlogArchives(ctx, be, base.ClusterID, base.BackupID)
	if err != nil {
		return false, err
	}
	chain, gap, err := binlogChain(base, existing)
	if err != nil {
		return false, err
	}
	if gap != "" {
		return false, fmt.Errorf("%w: the archive of backup %s is broken (%s); take a new full backup", ErrJob, base.BackupID, gap)
	}
	prefix, baseNo, _ := binlogNumber(base.BinlogBase.File)
	next := uint64(len(chain)) + 1
	var prevUntil time.Time
	if len(chain) > 0 {
		prevUntil = chain[len(chain)-1].Binlog.Until
	}
	// Rotate first, so the file that was active is closed and archived too.
	flush := []string{"mariadb", "--defaults-extra-file=" + s.DBOptionFile, "--host=" + s.DBHost, "--execute=FLUSH BINARY LOGS"}
	if err := env.run(ctx, flush, nil, io.Discard); err != nil {
		return false, err
	}
	files, err := env.binlogIndex(s.DataDir, prefix)
	if err != nil {
		return false, err
	}
	if len(files) < 2 {
		return false, fmt.Errorf("%w: binlog index lists %d file(s) after the rotation", ErrJob, len(files))
	}
	closed := files[:len(files)-1]
	for _, name := range closed {
		if _, n, ok := binlogNumber(name); !ok || n < baseNo+next-1 {
			continue
		}
		if want := binlogName(prefix, baseNo+next-1); name != want {
			return false, fmt.Errorf("%w: binlog file %s is needed next but the node no longer has it (first available: %s); take a new full backup", ErrJob, want, name)
		}
		rel := strings.TrimPrefix(path.Join(s.DataDir, name), "/")
		info, err := fs.Stat(env.Files, rel)
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrJob, err)
		}
		f, err := env.Files.Open(rel)
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrJob, err)
		}
		first, last, err := scanBinlog(f, info.Size())
		_ = f.Close()
		if err != nil {
			return false, err
		}
		from := first
		if !prevUntil.IsZero() {
			from = prevUntil
		}
		r := BinlogRange{HistoryID: base.SourceNodeUUID, BaseBackupID: base.BackupID, Sequence: next,
			Start: BinlogPosition{File: name, Offset: 4}, End: BinlogPosition{File: name, Offset: uint64(info.Size())},
			From: from, Until: last}
		if last.Before(from) {
			r.Until = from // clocks stepped back: claim no new coverage
		}
		f, err = env.Files.Open(rel)
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrJob, err)
		}
		m, err := ArchiveBinlog(ctx, be, pub, base, BinlogArchiveID(base.BackupID, next), r, f, env.Now().UTC())
		_ = f.Close()
		if err != nil {
			return false, fmt.Errorf("archive %s: %w", name, err)
		}
		prevUntil = m.Binlog.Until
		next++
	}
	return true, nil
}

const (
	replayReadyTries = 90
	replayReadyWait  = 2 * time.Second
)

func (e JobEnv) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runReplayJob applies the archived binlog files to the inspection instance
// s.DBHost. It re-derives the chain from the backend and refuses anything the
// manager's plan could not have produced: a segment list that is not the
// contiguous chain of this backup, or one that does not reach the stop time.
func runReplayJob(ctx context.Context, s JobSpec, env JobEnv, be backup.Backend) (bool, error) {
	if env.WriteFile == nil {
		return false, fmt.Errorf("%w: no scratch writer", ErrJob)
	}
	raw, err := env.read(s.KeyFile)
	if err != nil {
		return false, fmt.Errorf("%w: key file: %v", ErrJob, err)
	}
	key, err := backup.ParseRecoveryKey(string(raw))
	if err != nil {
		return false, err
	}
	base, err := baseOf(ctx, be, s, s.SourceClusterID)
	if err != nil {
		return false, err
	}
	stop, err := time.Parse("2006-01-02 15:04:05", s.StopDatetime)
	if err != nil {
		return false, fmt.Errorf("%w: stop time: %v", ErrJob, err)
	}
	var segs []Manifest
	for _, o := range s.Segments {
		m, err := readManifest(ctx, be, o)
		if err != nil {
			return false, err
		}
		if m.Object != o || m.Method != MethodBinlog {
			return false, fmt.Errorf("%w: %s is not a binlog archive", ErrInvalidManifest, o)
		}
		segs = append(segs, m)
	}
	chain, gap, err := binlogChain(base, segs)
	switch {
	case err != nil:
		return false, err
	case gap != "" || len(chain) != len(segs):
		return false, fmt.Errorf("%w: segments are not a contiguous chain of backup %s (%s)", ErrJob, base.BackupID, gap)
	case !stop.Add(-time.Second).Before(chain[len(chain)-1].Binlog.Until):
		return false, fmt.Errorf("%w: the archive ends at %s, before the target time", ErrJob, chain[len(chain)-1].Binlog.Until.Format(time.RFC3339))
	case s.StartOffset != base.BinlogBase.Offset:
		return false, fmt.Errorf("%w: start offset %d is not the backup position %d", ErrJob, s.StartOffset, base.BinlogBase.Offset)
	}
	dir := path.Join(env.TempDir, "binlog")
	var files []string
	for _, m := range chain {
		r, check, closeFn, err := openBinlog(ctx, be, m, key)
		if err != nil {
			return false, err
		}
		file := path.Join(dir, m.Binlog.Start.File)
		err = env.WriteFile(file, r)
		_ = closeFn()
		if err == nil {
			err = check()
		}
		if err != nil {
			return false, fmt.Errorf("segment %s: %w", m.Object, err)
		}
		files = append(files, file)
	}
	client := []string{"mariadb", "--defaults-extra-file=" + s.DBOptionFile, "--host=" + s.DBHost}
	if err := waitReady(ctx, env, client); err != nil {
		return true, err
	}
	argv := append([]string{"mariadb-binlog", "--start-position=" + strconv.FormatUint(s.StartOffset, 10), "--stop-datetime=" + s.StopDatetime}, files...)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := env.run(ctx, argv, nil, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	cerr := env.run(ctx, client, pr, io.Discard)
	_ = pr.CloseWithError(errors.New("client ended"))
	if berr := <-done; berr != nil {
		return true, fmt.Errorf("mariadb-binlog: %w", berr)
	}
	if cerr != nil {
		return true, fmt.Errorf("apply binlog: %w", cerr)
	}
	// The replay account has done its job; the init file recreates it only if
	// the instance restarts, and its password is no longer stored anywhere.
	return true, env.run(ctx, append(slices.Clone(client), "--execute=DROP USER IF EXISTS '"+ReplayAccount+"'@'%'"), nil, io.Discard)
}

// waitReady polls the instance (it was created a moment ago) until it
// answers.
func waitReady(ctx context.Context, env JobEnv, client []string) error {
	probe := append(slices.Clone(client), "--execute=SELECT 1")
	var err error
	for i := 0; i < replayReadyTries; i++ {
		if err = env.run(ctx, probe, nil, io.Discard); err == nil {
			return nil
		}
		if serr := env.sleep(ctx, replayReadyWait); serr != nil {
			return serr
		}
	}
	return fmt.Errorf("%w: the instance %s did not answer: %v", ErrJob, client[2], err)
}
