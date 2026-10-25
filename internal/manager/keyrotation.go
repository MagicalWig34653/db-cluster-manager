// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// KeyRotation is replicated with the keyring. Each resealed document and the
// progress counter commit in one transaction. A successor scans for remaining
// old IDs, so deletions, retries and insertion order cannot skip a secret.
type KeyRotation struct {
	From      string `json:"from"`
	To        string `json:"to"`
	By        string `json:"by"`
	Processed uint64 `json:"processed"`
	Complete  bool   `json:"complete"`
}

func rotationDigest(rec KeyringRecord, version uint64) string {
	b, _ := json.Marshal(struct {
		Current string
		Version uint64
	}{rec.Current, version})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// RotateDataKey previews on an empty digest; a matching digest is the explicit
// approval of this exact keyring revision. Concurrent changes invalidate it.
func (k *ReplicaService) RotateDataKey(ctx context.Context, actor authz.Subject, digest string) (api.KeyRotationPlan, error) {
	if err := k.Authorizer.AuthorizeAdministrator(actor); err != nil {
		return api.KeyRotationPlan{}, err
	}
	if k.Ring == nil {
		return api.KeyRotationPlan{}, api.ErrUnavailable
	}
	if !k.Svc.IsLeader() {
		return api.KeyRotationPlan{}, api.ErrNotLeader
	}
	doc, ok, err := Get[KeyringRecord](k.Svc.store, CollKeys, KeyringKey, Consistent())
	if err != nil {
		return api.KeyRotationPlan{}, err
	}
	if !ok {
		return api.KeyRotationPlan{}, api.ErrUnavailable
	}
	rec := doc.Value
	out := api.KeyRotationPlan{Digest: rotationDigest(rec, doc.Version), CurrentKey: rec.Current}
	if rec.Rotation != nil && !rec.Rotation.Complete {
		return out, api.ErrConflict
	}
	if digest == "" {
		return out, nil
	}
	if digest != out.Digest {
		return out, api.ErrApprovalMismatch
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return out, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return out, err
	}
	id := "dk-" + hex.EncodeToString(nonce[:])
	rec.Keys = append(rec.Keys, WrappedDataKey{ID: id, Created: k.Ring.now()})
	k.Ring.mu.Lock()
	k.Ring.cache[id] = key
	k.Ring.mu.Unlock()
	committed := false
	defer func() {
		if !committed {
			k.Ring.mu.Lock()
			delete(k.Ring.cache, id)
			k.Ring.mu.Unlock()
		}
	}()
	rec.Rotation = &KeyRotation{From: rec.Current, To: id, By: actor.ID}
	rec.Current = id
	if err := k.Ring.distribute(k.Svc, &rec); err != nil {
		return out, err
	}
	if err := k.requireRecoveryEnvelope(rec, id); err != nil {
		return out, err
	}
	if _, err := Put(k.Svc.store, CollKeys, KeyringKey, rec, IfVersion(doc.Version)); err != nil {
		return out, MapCommandError(err)
	}
	committed = true
	k.Svc.recordEvent("", "", api.SeverityWarning, fmt.Sprintf("%s approved data-key rotation to %s", actor.ID, id))
	out.CurrentKey, out.Started = id, true
	return out, nil
}

// requireRecoveryEnvelope prevents a rotation from dropping the recovery
// path once it has existed. The manager holds only the public recipient, so
// it verifies the envelope produced by wrapDataKey and its header before CAS;
// only the external recovery key can decrypt it end to end.
func (k *ReplicaService) requireRecoveryEnvelope(rec KeyringRecord, id string) error {
	wasConfigured := false
	for _, old := range rec.Keys {
		wasConfigured = wasConfigured || old.RecoveryKeyID != "" || len(old.Recovery) != 0
	}
	settings, ok, err := Get[Settings](k.Svc.store, CollSettings, SettingsKey, Consistent())
	if err != nil {
		return err
	}
	if !ok || settings.Value.Backup.RecipientKey == "" {
		if wasConfigured {
			return fmt.Errorf("%w: recovery recipient was configured but is now absent", ErrNoKey)
		}
		return nil
	}
	pub, err := backup.ParseStreamPublicKey(settings.Value.Backup.RecipientKey)
	if err != nil {
		return fmt.Errorf("%w: recovery recipient is invalid", ErrNoKey)
	}
	w := rec.key(id)
	if w == nil || len(w.Recovery) == 0 || w.RecoveryKeyID != pub.ID() {
		return fmt.Errorf("%w: new data key has no envelope for the recovery recipient", ErrNoKey)
	}
	meta, wrappedFor, err := backup.ReadStreamMeta(bytes.NewReader(w.Recovery))
	if err != nil || wrappedFor != pub.ID() || meta.Kind != envelopeKind || meta.BackupID != id || meta.ClusterID != recipientLabel("") {
		return fmt.Errorf("%w: new recovery envelope does not match the key and recipient", ErrNoKey)
	}
	return nil
}

// ResumeRotation performs at most limit document transactions. Old key envelopes
// remain recoverable, also after completion: historical snapshots need them.
func (k *Keyring) ResumeRotation(ctx context.Context, s *Service, limit int) error {
	if limit <= 0 {
		return nil
	}
	for range limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		doc, ok, err := Get[KeyringRecord](s.store, CollKeys, KeyringKey, Consistent())
		if err != nil {
			return err
		}
		if !ok || doc.Value.Rotation == nil || doc.Value.Rotation.Complete {
			return nil
		}
		rec := doc.Value
		var found bool
		for _, d := range s.node.AllDocs() {
			var sv sealed
			if json.Unmarshal(d.Value, &sv) != nil || sv.KeyID == "" || sv.KeyID == rec.Current {
				continue
			}
			var plain json.RawMessage
			if err := unseal(k, d.Collection, d.Key, sv, &plain); err != nil {
				return err
			}
			next, err := seal(k, d.Collection, d.Key, plain)
			if err != nil {
				return err
			}
			rec.Rotation.Processed++
			progress, err := PutOp(CollKeys, KeyringKey, rec, IfVersion(doc.Version))
			if err != nil {
				return err
			}
			secret, err := PutOp(d.Collection, d.Key, next, IfVersion(d.Version))
			if err != nil {
				return err
			}
			if _, err := s.store.Txn(secret, progress); err != nil {
				return err
			}
			found = true
			break
		}
		if !found {
			rec.Rotation.Complete = true
			if _, err := Put(s.store, CollKeys, KeyringKey, rec, IfVersion(doc.Version)); err != nil {
				return err
			}
			s.recordEvent("", "", api.SeverityInfo, fmt.Sprintf("data-key rotation to %s completed (%d documents)", rec.Current, rec.Rotation.Processed))
			return nil
		}
	}
	return nil
}
