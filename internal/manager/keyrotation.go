// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
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
	rec.Rotation = &KeyRotation{From: rec.Current, To: id, By: actor.ID}
	rec.Current = id
	if err := k.Ring.distribute(k.Svc, &rec); err != nil {
		return out, err
	}
	if _, err := Put(k.Svc.store, CollKeys, KeyringKey, rec, IfVersion(doc.Version)); err != nil {
		return out, MapCommandError(err)
	}
	k.Svc.recordEvent("", "", api.SeverityWarning, fmt.Sprintf("%s approved data-key rotation to %s", actor.ID, id))
	out.CurrentKey, out.Started = id, true
	return out, nil
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
