// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/notify"
)

// Backup of the manager's own metastore (B07/B08, disaster recovery).
//
// The export holds the replicated documents that describe the installation:
// Swarms, hosts, clusters, backup policies and records, settings, accounts,
// roles, passkeys, agent PKI, the keyring (with its recovery envelopes),
// sealed secrets, approvals-free configuration. It deliberately leaves out
// state that must not come back to life after a restore:
//
//   - sessions and recovery sessions (logins),
//   - operations and approvals (an old operation could resume a destructive
//     step on a world that has changed since the export),
//   - events (history).
//
// The export is sealed with the backup stream format for the public half of
// the backup recovery key (Settings.Backup.RecipientKey): the running
// manager can write it but never read it again. Restoring needs the
// externally retained recovery key. The sealed documents inside stay sealed
// with the data keys; the keyring in the export carries a recovery envelope
// of each data key, so the recovery key alone also yields those keys
// (RecoverDataKeys).
//
// Import (ImportMetastore) writes into an EMPTY store only. It re-wraps the
// data keys for the importing replica's identity, registers only that
// replica and clears an unfinished key rotation. Everything about the other
// replicas, sessions and operations is gone on purpose; additional
// replicas are added afterwards through the normal membership procedure.

// Export constants.
const (
	MetaExportFormat    = "dcm-metastore-export/1"
	metaStreamKind      = "manager-metastore"
	metaObjectPrefix    = "metastore-"
	metaObjectSuffix    = ".dbcms"
	metaRecordSuffix    = ".record.json"
	metaSignPurpose     = "metastore-backup"
	metaImportBatchSize = 100
	// DefaultMetaKeep is the number of metastore backups retention keeps.
	DefaultMetaKeep = 14
)

// KindMetastoreBackupFailed is the notification kind of a failed scheduled
// metastore backup.
const KindMetastoreBackupFailed = "backup.metastore_failed"

// Errors of metastore backups.
var (
	// ErrStoreNotEmpty: an import needs an empty store.
	ErrStoreNotEmpty = errors.New("manager: metastore is not empty; an import only writes into a fresh store")
	// ErrBadExport: the export is malformed or not for this purpose.
	ErrBadExport = errors.New("manager: invalid metastore export")
)

var metaExcluded = map[string]bool{
	CollSessions: true, CollRecoverySessions: true, CollOperations: true, CollApprovals: true, CollEvents: true,
}

// MetaDoc is one exported document (versions are not kept).
type MetaDoc struct {
	Collection string          `json:"collection"`
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value"`
}

// MetaExport is the plaintext of a metastore backup.
type MetaExport struct {
	Format       string    `json:"format"`
	Created      time.Time `json:"created"`
	Installation string    `json:"installation,omitempty"`
	// Revision is the document revision of the source at export time.
	Revision uint64    `json:"revision"`
	Excluded []string  `json:"excluded"`
	Docs     []MetaDoc `json:"docs"`
}

// Collections returns the number of distinct collections.
func (e MetaExport) Collections() int {
	seen := map[string]bool{}
	for _, d := range e.Docs {
		seen[d.Collection] = true
	}
	return len(seen)
}

// ExportMetastore reads a coherent copy of the store. The caller must lead
// (Barrier first), otherwise the copy could be stale.
func ExportMetastore(s *Store, now time.Time, installation string) (MetaExport, error) {
	e := MetaExport{Format: MetaExportFormat, Created: now.UTC(), Installation: installation, Revision: s.node.Revision()}
	for c := range metaExcluded {
		e.Excluded = append(e.Excluded, c)
	}
	slices.Sort(e.Excluded)
	for _, d := range s.node.AllDocs() {
		if metaExcluded[d.Collection] {
			continue
		}
		e.Docs = append(e.Docs, MetaDoc{Collection: d.Collection, Key: d.Key, Value: d.Value})
	}
	if len(e.Docs) == 0 {
		return e, fmt.Errorf("%w: the store is empty", ErrBadExport)
	}
	return e, nil
}

// Seal encrypts the export for the recovery key's public half.
func (e MetaExport) Seal(recipient backup.StreamPublicKey, name string) ([]byte, error) {
	plain, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := backup.NewSealWriter(&buf, backup.StreamMeta{Kind: metaStreamKind, BackupID: name, ClusterID: e.Installation, Created: e.Created}, recipient)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// OpenMetaExport decrypts and validates a sealed export.
func OpenMetaExport(data []byte, recovery backup.RecoveryKey) (MetaExport, error) {
	r, err := backup.OpenStream(bytes.NewReader(data), recovery)
	if err != nil {
		return MetaExport{}, err
	}
	if r.Meta().Kind != metaStreamKind {
		return MetaExport{}, fmt.Errorf("%w: stream kind %q", ErrBadExport, r.Meta().Kind)
	}
	plain, err := io.ReadAll(io.LimitReader(r, 1<<30))
	if err != nil {
		return MetaExport{}, err
	}
	var e MetaExport
	if err := json.Unmarshal(plain, &e); err != nil || e.Format != MetaExportFormat {
		return MetaExport{}, fmt.Errorf("%w: format", ErrBadExport)
	}
	seen := map[[2]string]bool{}
	for _, d := range e.Docs {
		k := [2]string{d.Collection, d.Key}
		if d.Collection == "" || d.Key == "" || !json.Valid(d.Value) || metaExcluded[d.Collection] || seen[k] {
			return MetaExport{}, fmt.Errorf("%w: document %s/%s", ErrBadExport, d.Collection, d.Key)
		}
		seen[k] = true
	}
	return e, nil
}

// ImportResult reports what an import did.
type ImportResult struct {
	Documents   int
	Collections int
	DataKeys    []string
	Warnings    []string
}

// ImportMetastore writes e into the empty store s for the replica
// replicaID with public identity replicaPub. recovery is the retained
// backup recovery key; it unwraps the data keys, which are re-wrapped for
// the replica (and for the recovery key again). Nothing is written when any
// step before the writes fails.
func ImportMetastore(s *Store, e MetaExport, recovery backup.RecoveryKey, replicaID string, replicaPub backup.StreamPublicKey, now time.Time) (ImportResult, error) {
	var res ImportResult
	// Only history written by a starting leader (events) may exist.
	for _, d := range s.node.AllDocs() {
		if !metaExcluded[d.Collection] {
			return res, ErrStoreNotEmpty
		}
	}
	if replicaID == "" || replicaPub.ID() == "" {
		return res, fmt.Errorf("%w: replica ID and public key required", ErrBadExport)
	}
	ringIdx := slices.IndexFunc(e.Docs, func(d MetaDoc) bool { return d.Collection == CollKeys && d.Key == KeyringKey })
	if ringIdx < 0 {
		// A single-instance installation with a key file: sealed documents
		// stay readable with the original data.key, which is not part of
		// the export.
		res.Warnings = append(res.Warnings, "the export has no keyring (key file installation): restore the original data key file, otherwise sealed documents stay unreadable")
		return importDocs(s, e, e.Docs, res)
	}
	var ring KeyringRecord
	if err := json.Unmarshal(e.Docs[ringIdx].Value, &ring); err != nil {
		return res, fmt.Errorf("%w: keyring: %v", ErrBadExport, err)
	}
	keys, err := RecoverDataKeys(ring, recovery)
	if err != nil {
		return res, fmt.Errorf("manager: the recovery key does not open the exported keyring: %w", err)
	}
	if ring.Rotation != nil {
		res.Warnings = append(res.Warnings, "a data key rotation was in progress at export time; it was cleared (all keys are kept)")
		ring.Rotation = nil
	}
	label := recipientLabel(replicaID)
	ring.Replicas = map[string]ReplicaRegistration{replicaID: {PublicKey: replicaPub.String(), RegisteredBy: "metastore-import", RegisteredAt: now.UTC()}}
	for i := range ring.Keys {
		w := &ring.Keys[i]
		env, err := wrapDataKey(keys[w.ID], w.ID, label, replicaPub, now)
		if err != nil {
			return res, err
		}
		w.Envelopes = map[string][]byte{replicaID: env}
		res.DataKeys = append(res.DataKeys, w.ID)
	}
	ringJSON, err := json.Marshal(ring)
	if err != nil {
		return res, err
	}
	docs := slices.Clone(e.Docs)
	docs[ringIdx].Value = ringJSON
	return importDocs(s, e, docs, res)
}

// importDocs writes docs (the keyring first) in batches.
func importDocs(s *Store, e MetaExport, docs []MetaDoc, res ImportResult) (ImportResult, error) {
	docs = slices.Clone(docs)
	// The keyring goes first, so a partial import is never left with sealed
	// documents and no way to read them.
	slices.SortStableFunc(docs, func(a, b MetaDoc) int {
		ra, rb := a.Collection == CollKeys, b.Collection == CollKeys
		switch {
		case ra && !rb:
			return -1
		case rb && !ra:
			return 1
		}
		return 0
	})
	for start := 0; start < len(docs); start += metaImportBatchSize {
		end := min(start+metaImportBatchSize, len(docs))
		ops := make([]metastore.DocOp, 0, end-start)
		for _, d := range docs[start:end] {
			ops = append(ops, metastore.DocOp{Collection: d.Collection, Key: d.Key, IfVersion: IfVersion(0), Value: d.Value})
		}
		if _, err := s.node.ApplyDocs(ops...); err != nil {
			return res, fmt.Errorf("manager: import batch at document %d: %w (the store is partly written; delete its data directory and start again)", start, err)
		}
	}
	res.Documents = len(docs)
	res.Collections = e.Collections()
	return res, nil
}

// MetaRecord is the signed record next to a metastore backup object.
type MetaRecord struct {
	Name         string    `json:"name"`
	Created      time.Time `json:"created"`
	Installation string    `json:"installation,omitempty"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256"`
	Documents    int       `json:"documents"`
	Collections  int       `json:"collections"`
	Revision     uint64    `json:"revision"`
	By           string    `json:"by"`
}

func metaObjectName(created time.Time, installation string) string {
	inst := installation
	if inst == "" {
		inst = "dbcm"
	}
	return metaObjectPrefix + inst + "-" + strings.ToLower(created.UTC().Format("20060102t150405")) + metaObjectSuffix
}

func (b *Backups) metaBackend() backup.Backend {
	if b.MetaTarget != "" {
		return b.Targets[b.MetaTarget]
	}
	return b.Backend
}

func (b *Backups) metaVerified(ctx context.Context, be backup.Backend, rec MetaRecord, infos map[string]int64) bool {
	size, ok := infos[rec.Name]
	return ok && size == rec.Size && b.Keys != nil && be != nil
}

// readMetaRecord reads and verifies one signed record.
func (b *Backups) readMetaRecord(ctx context.Context, be backup.Backend, name string) (MetaRecord, error) {
	rc, err := be.Get(ctx, name)
	if err != nil {
		return MetaRecord{}, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return MetaRecord{}, err
	}
	signed, err := backup.DecodeSigned(raw)
	if err != nil {
		return MetaRecord{}, err
	}
	payload, err := signed.Verify(b.Keys, metaSignPurpose)
	if err != nil {
		return MetaRecord{}, err
	}
	var rec MetaRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return MetaRecord{}, err
	}
	return rec, nil
}

// MetastoreBackups implements api.MetastoreBackups: the backups on the
// target with a record that verifies; others are not listed.
func (b *Backups) MetastoreBackups(ctx context.Context) ([]api.MetastoreBackup, error) {
	be := b.metaBackend()
	if be == nil || b.Keys == nil {
		return nil, MapCommandError(fmt.Errorf("%w: a backup backend and the data keyring are required", ErrSettingsIncomplete))
	}
	recs, err := be.List(ctx, metaObjectPrefix)
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	for _, o := range recs {
		sizes[o.Name] = o.Size
	}
	out := []api.MetastoreBackup{}
	for _, o := range recs {
		if !strings.HasSuffix(o.Name, metaRecordSuffix) {
			continue
		}
		rec, err := b.readMetaRecord(ctx, be, o.Name)
		if err != nil {
			continue // a record that does not verify is not offered
		}
		out = append(out, api.MetastoreBackup{Name: rec.Name, Target: b.MetaTarget, CreatedAt: rec.Created, Size: rec.Size,
			Documents: rec.Documents, Collections: rec.Collections, By: rec.By, Verified: b.metaVerified(ctx, be, rec, sizes)})
	}
	slices.SortFunc(out, func(a, c api.MetastoreBackup) int { return c.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// CreateMetastoreBackup implements api.MetastoreBackups. It needs the
// backup permission on the manager settings (audited).
func (b *Backups) CreateMetastoreBackup(ctx context.Context, actor authz.Subject) (api.MetastoreBackup, error) {
	out, err := b.createMetastoreBackup(ctx, actor)
	return out, MapCommandError(err)
}

func (b *Backups) createMetastoreBackup(ctx context.Context, actor authz.Subject) (api.MetastoreBackup, error) {
	if _, err := b.Ops.leadership(); err != nil {
		return api.MetastoreBackup{}, err
	}
	if err := b.Ops.authorize(actor, authz.ActionBackup, authz.Resource{Type: authz.ResourceSettings, Scope: authz.Global}); err != nil {
		return api.MetastoreBackup{}, err
	}
	be := b.metaBackend()
	st, err := b.Ops.settings()
	if err != nil {
		return api.MetastoreBackup{}, err
	}
	if be == nil || b.Keys == nil || st.Backup.RecipientKey == "" {
		return api.MetastoreBackup{}, fmt.Errorf("%w: a backup backend, the data keyring and backup.recipientKey are required", ErrSettingsIncomplete)
	}
	pub, err := backup.ParseStreamPublicKey(st.Backup.RecipientKey)
	if err != nil {
		return api.MetastoreBackup{}, fmt.Errorf("%w: backup.recipientKey: %v", ErrSettingsIncomplete, err)
	}
	if err := b.Ops.Svc.node.Barrier(b.Ops.Svc.cfg.BarrierTimeout); err != nil {
		return api.MetastoreBackup{}, err
	}
	now := b.now()
	exp, err := ExportMetastore(b.store(), now, st.InstallationID)
	if err != nil {
		return api.MetastoreBackup{}, err
	}
	name := metaObjectName(now, st.InstallationID)
	sealed, err := exp.Seal(pub, name)
	if err != nil {
		return api.MetastoreBackup{}, err
	}
	sum := sha256.Sum256(sealed)
	rec := MetaRecord{Name: name, Created: now.UTC(), Installation: st.InstallationID, Size: int64(len(sealed)), SHA256: hex.EncodeToString(sum[:]),
		Documents: len(exp.Docs), Collections: exp.Collections(), Revision: exp.Revision, By: actor.ID}
	payload, err := json.Marshal(rec)
	if err != nil {
		return api.MetastoreBackup{}, err
	}
	signed, err := backup.Sign(b.Keys, metaSignPurpose, payload)
	if err != nil {
		return api.MetastoreBackup{}, err
	}
	enc, err := backup.EncodeSigned(signed)
	if err != nil {
		return api.MetastoreBackup{}, err
	}
	// Object first, record last: the record marks a complete backup.
	if err := be.Put(ctx, name, bytes.NewReader(sealed)); err != nil {
		return api.MetastoreBackup{}, err
	}
	if err := be.Put(ctx, name+metaRecordSuffix, bytes.NewReader(enc)); err != nil {
		return api.MetastoreBackup{}, err
	}
	b.Ops.Svc.recordEvent("", "", api.SeverityInfo, fmt.Sprintf("%s backed up the manager metastore (%d documents) as %s", actor.ID, rec.Documents, name))
	if err := b.pruneMetastoreBackups(ctx, be); err != nil {
		b.log("manager: metastore backup retention incomplete", "err", err)
	}
	return api.MetastoreBackup{Name: name, Target: b.MetaTarget, CreatedAt: rec.Created, Size: rec.Size, Documents: rec.Documents,
		Collections: rec.Collections, By: rec.By, Verified: true}, nil
}

// pruneMetastoreBackups keeps the newest MetaKeep backups. Records of
// deleted objects go first, so a listed backup always has its object.
func (b *Backups) pruneMetastoreBackups(ctx context.Context, be backup.Backend) error {
	keep := b.MetaKeep
	if keep <= 0 {
		keep = DefaultMetaKeep
	}
	infos, err := be.List(ctx, metaObjectPrefix)
	if err != nil {
		return err
	}
	var objs []backup.ObjectInfo
	for _, o := range infos {
		if strings.HasSuffix(o.Name, metaObjectSuffix) {
			objs = append(objs, o)
		}
	}
	// Names embed the UTC time, so name order is time order.
	slices.SortFunc(objs, func(a, c backup.ObjectInfo) int { return strings.Compare(c.Name, a.Name) })
	var errs []error
	for _, o := range objs[min(keep, len(objs)):] {
		for _, n := range []string{o.Name + metaRecordSuffix, o.Name} {
			if err := be.Delete(ctx, n); err != nil && !errors.Is(err, backup.ErrNotFound) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// RunMetastoreBackup creates a metastore backup when MetaInterval has
// passed since the newest verified one. The scheduler calls it on every
// pass; a failure records an event and a notification.
func (b *Backups) RunMetastoreBackup(ctx context.Context, actor authz.Subject) {
	if b.MetaInterval <= 0 {
		return
	}
	list, err := b.MetastoreBackups(ctx)
	if err == nil && len(list) > 0 && b.now().Sub(list[0].CreatedAt) < b.MetaInterval {
		return
	}
	b.metaMu.Lock()
	if !b.metaLast.IsZero() && b.now().Sub(b.metaLast) < b.MetaInterval/10 {
		b.metaMu.Unlock()
		return // do not retry a failing backup on every pass
	}
	b.metaLast = b.now()
	b.metaMu.Unlock()
	if _, err := b.CreateMetastoreBackup(ctx, actor); err != nil && ctx.Err() == nil {
		msg := "scheduled backup of the manager metastore failed: " + err.Error()
		b.Ops.Svc.recordEvent("", "", api.SeverityWarning, msg)
		b.notify(ctx, notify.SeverityCritical, KindMetastoreBackupFailed, "manager/metastore", msg, nil)
	}
}
