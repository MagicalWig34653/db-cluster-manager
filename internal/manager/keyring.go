// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
)

// Data-key distribution across manager replicas and its recovery (B07).
//
// The data key that seals secrets at rest (secrets.go) lives in the
// replicated store only in wrapped form, in the keyring document
// keys/keyring:
//
//   - every replica has its own identity key (a backup.RecoveryKey kept in
//     <dataDir>/replica.key, mode 0600, never replicated); the keyring
//     holds one envelope of each data key per registered replica, sealed
//     for that replica's X25519 public half with the backup stream format
//     (ephemeral X25519, HKDF-SHA256, AES-256-GCM; the header binds the
//     key ID and the recipient);
//   - when the installation has a backup recovery key configured (its
//     public half, Settings.Backup.RecipientKey), every data key also gets
//     a recovery envelope for it. With the externally retained recovery
//     key and a copy of the store (Raft data directory or manager
//     configuration backup) RecoverDataKeys returns the data keys without
//     any replica.
//
// A replica's public key is registered either by the replica itself while
// it leads and holds the key (the first leader on bootstrap), or by an
// administrator through PUT /settings/replicas/{id}/data-key-recipient
// (settings update permission, audited) after comparing the key the
// replica prints. Only current Raft voters can be registered. The leader
// wraps every data key for every registered replica; a replica that leads
// without being able to unwrap the current key hands leadership to another
// voter instead of acting.
//
// Not done: rotation (adding a key ID is supported by the format and by
// KeyProvider, but nothing creates a second key), revoking a removed
// replica (it keeps what it already unwrapped), and membership changes.

// Keyring document coordinates.
const (
	CollKeys   = "keys"
	KeyringKey = "keyring"
)

// LegacyKeyID is the key ID FileKeyProvider uses; a keyring bootstrapped
// from an existing key file keeps it so sealed documents stay readable.
const LegacyKeyID = "file"

const envelopeKind = "manager-data-key"

// Errors of the keyring.
var (
	// ErrNotVoter: the replica is not a voter of the metastore.
	ErrNotVoter = errors.New("manager: replica is not a metastore voter")
	// ErrNoKeyring: the keyring has not been bootstrapped yet.
	ErrNoKeyring = errors.New("manager: keyring not bootstrapped")
)

// KeyringRecord is the replicated keyring. It holds no plaintext key.
type KeyringRecord struct {
	Current  string                         `json:"current"`
	Keys     []WrappedDataKey               `json:"keys"`
	Replicas map[string]ReplicaRegistration `json:"replicas"`
}

// ReplicaRegistration is a replica's registered public key.
type ReplicaRegistration struct {
	PublicKey    string    `json:"publicKey"`
	RegisteredBy string    `json:"registeredBy"`
	RegisteredAt time.Time `json:"registeredAt"`
}

// WrappedDataKey is one data key in wrapped form.
type WrappedDataKey struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	// Envelopes are keyed by replica ID.
	Envelopes map[string][]byte `json:"envelopes"`
	// Recovery is the envelope for the backup recovery key (B07), with the
	// ID of that key's public half; empty without a configured recovery key.
	Recovery      []byte `json:"recovery,omitempty"`
	RecoveryKeyID string `json:"recoveryKeyId,omitempty"`
}

func (r *KeyringRecord) key(id string) *WrappedDataKey {
	for i := range r.Keys {
		if r.Keys[i].ID == id {
			return &r.Keys[i]
		}
	}
	return nil
}

func recipientLabel(replica string) string {
	if replica == "" {
		return "recovery"
	}
	return "replica/" + replica
}

// wrapDataKey seals a data key for pub; the header binds key ID and
// recipient.
func wrapDataKey(key []byte, keyID, recipient string, pub backup.StreamPublicKey, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	w, err := backup.NewSealWriter(&buf, backup.StreamMeta{Kind: envelopeKind, BackupID: keyID, ClusterID: recipient, Created: now, ChunkSize: 64}, pub)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(key); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// unwrapDataKey opens an envelope with the private identity and checks that
// it is the data key keyID for recipient.
func unwrapDataKey(env []byte, keyID, recipient string, id backup.RecoveryKey) ([]byte, error) {
	r, err := backup.OpenStream(bytes.NewReader(env), id)
	if err != nil {
		return nil, err
	}
	key, err := io.ReadAll(io.LimitReader(r, 64))
	if err != nil {
		return nil, err
	}
	m := r.Meta()
	if m.Kind != envelopeKind || m.BackupID != keyID || m.ClusterID != recipient || len(key) != 32 {
		return nil, fmt.Errorf("%w: envelope is not data key %q for %s", ErrNoKey, keyID, recipient)
	}
	return key, nil
}

// LoadReplicaIdentity reads the replica identity key from path, creating a
// random one (mode 0600) when it does not exist.
func LoadReplicaIdentity(path string) (backup.RecoveryKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		k, err := backup.GenerateRecoveryKey()
		if err != nil {
			return k, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return k, fmt.Errorf("manager: create identity dir: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return k, fmt.Errorf("manager: create replica identity: %w", err)
		}
		if _, err := f.WriteString(k.String() + "\n"); err != nil {
			_ = f.Close()
			return k, err
		}
		return k, f.Close()
	}
	if err != nil {
		return backup.RecoveryKey{}, fmt.Errorf("manager: read replica identity: %w", err)
	}
	return backup.ParseRecoveryKey(string(b))
}

// Keyring is the KeyProvider of a replica: it unwraps data keys from the
// replicated keyring with the replica's identity and caches them.
type Keyring struct {
	ReplicaID string
	Identity  backup.RecoveryKey
	// Seed is an existing data key (a key file of a single-instance
	// installation) adopted as LegacyKeyID when the keyring is
	// bootstrapped; nil generates a new key.
	Seed []byte
	Now  func() time.Time

	mu    sync.Mutex
	store *Store
	cache map[string][]byte
}

var _ KeyProvider = (*Keyring)(nil)

// Attach binds the keyring to the replica's store (after the Service is
// created).
func (k *Keyring) Attach(s *Store) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.store = s
}

func (k *Keyring) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// PublicKey is the replica's public identity (to register on the leader).
func (k *Keyring) PublicKey() (string, error) {
	p, err := k.Identity.StreamPublicKey()
	if err != nil {
		return "", err
	}
	return p.String(), nil
}

func (k *Keyring) record() (KeyringRecord, bool, error) {
	k.mu.Lock()
	s := k.store
	k.mu.Unlock()
	if s == nil {
		return KeyringRecord{}, false, fmt.Errorf("%w: keyring not attached", ErrNoKey)
	}
	v, ok, err := Get[KeyringRecord](s, CollKeys, KeyringKey)
	return v.Value, ok, err
}

// Current implements KeyProvider.
func (k *Keyring) Current() (string, []byte, error) {
	rec, ok, err := k.record()
	if err != nil {
		return "", nil, err
	}
	if !ok || rec.Current == "" {
		return "", nil, fmt.Errorf("%w: %v", ErrNoKey, ErrNoKeyring)
	}
	b, err := k.unwrap(rec, rec.Current)
	return rec.Current, b, err
}

// Key implements KeyProvider.
func (k *Keyring) Key(id string) ([]byte, error) {
	k.mu.Lock()
	if b, ok := k.cache[id]; ok {
		k.mu.Unlock()
		return b, nil
	}
	k.mu.Unlock()
	rec, _, err := k.record()
	if err != nil {
		return nil, err
	}
	return k.unwrap(rec, id)
}

func (k *Keyring) unwrap(rec KeyringRecord, id string) ([]byte, error) {
	k.mu.Lock()
	if b, ok := k.cache[id]; ok {
		k.mu.Unlock()
		return b, nil
	}
	k.mu.Unlock()
	w := rec.key(id)
	if w == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoKey, id)
	}
	env, ok := w.Envelopes[k.ReplicaID]
	if !ok {
		return nil, fmt.Errorf("%w: no envelope of %q for replica %s", ErrNoKey, id, k.ReplicaID)
	}
	b, err := unwrapDataKey(env, id, recipientLabel(k.ReplicaID), k.Identity)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoKey, err)
	}
	k.mu.Lock()
	if k.cache == nil {
		k.cache = map[string][]byte{}
	}
	k.cache[id] = b
	k.mu.Unlock()
	return b, nil
}

// ensureLeader runs on every leadership start before anything else: it
// bootstraps the keyring when none exists, registers this replica when it
// holds the current key, and wraps every key for every registered replica
// and the recovery key. It returns ErrNoKey when this replica cannot
// unwrap the current key (another replica must lead).
func (k *Keyring) ensureLeader(s *Service) error {
	doc, ok, err := Get[KeyringRecord](s.store, CollKeys, KeyringKey, Consistent())
	if err != nil {
		return err
	}
	rec := doc.Value
	pub, err := k.PublicKey()
	if err != nil {
		return err
	}
	now := k.now()
	if !ok || rec.Current == "" {
		id, key := LegacyKeyID, k.Seed
		if len(key) == 0 {
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return err
			}
			var r [8]byte
			_, _ = rand.Read(r[:])
			id = "dk-" + hex.EncodeToString(r[:])
		}
		if len(key) != 32 {
			return fmt.Errorf("%w: seed key has %d bytes", ErrNoKey, len(key))
		}
		rec = KeyringRecord{Current: id, Keys: []WrappedDataKey{{ID: id, Created: now, Envelopes: map[string][]byte{}}},
			Replicas: map[string]ReplicaRegistration{k.ReplicaID: {PublicKey: pub, RegisteredBy: "bootstrap", RegisteredAt: now}}}
		k.mu.Lock()
		if k.cache == nil {
			k.cache = map[string][]byte{}
		}
		k.cache[id] = key
		k.mu.Unlock()
		if err := k.distribute(s, &rec); err != nil {
			return err
		}
		if _, err := Put(s.store, CollKeys, KeyringKey, rec, IfVersion(0)); err != nil {
			return err
		}
		s.recordEvent("", "", api.SeverityInfo, fmt.Sprintf("manager %s bootstrapped the data keyring (key %s)", k.ReplicaID, id))
		k.warnWithoutRecovery(s, rec)
		return nil
	}
	if _, err := k.unwrap(rec, rec.Current); err != nil {
		return err
	}
	_, _, err = Update(s.store, CollKeys, KeyringKey, func(cur KeyringRecord, exists bool) (KeyringRecord, error) {
		if !exists {
			return cur, ErrNoKeyring
		}
		if r, ok := cur.Replicas[k.ReplicaID]; !ok || r.PublicKey != pub {
			// This replica unwrapped the key with its identity, so the
			// registration follows that identity.
			if cur.Replicas == nil {
				cur.Replicas = map[string]ReplicaRegistration{}
			}
			cur.Replicas[k.ReplicaID] = ReplicaRegistration{PublicKey: pub, RegisteredBy: "self", RegisteredAt: now}
		}
		return cur, k.distribute(s, &cur)
	})
	if err == nil {
		rec, _, _ := k.record()
		k.warnWithoutRecovery(s, rec)
	}
	return err
}

// updateDistributed wraps missing envelopes and stores the keyring (CAS).
func (k *Keyring) updateDistributed(s *Service) error {
	_, _, err := Update(s.store, CollKeys, KeyringKey, func(cur KeyringRecord, exists bool) (KeyringRecord, error) {
		if !exists {
			return cur, ErrNoKeyring
		}
		return cur, k.distribute(s, &cur)
	})
	return err
}

func (k *Keyring) warnWithoutRecovery(s *Service, rec KeyringRecord) {
	if w := rec.key(rec.Current); w != nil && len(w.Recovery) == 0 {
		s.log.Warn("manager: the data key has no recovery envelope; configure a backup recovery key (settings.backup.recipientKey)")
	}
}

// distribute adds every missing envelope to rec. The leader must hold all
// keys it wraps.
func (k *Keyring) distribute(s *Service, rec *KeyringRecord) error {
	var recovery backup.StreamPublicKey
	if st, ok, err := Get[Settings](s.store, CollSettings, SettingsKey); err == nil && ok && st.Value.Backup.RecipientKey != "" {
		if p, err := backup.ParseStreamPublicKey(st.Value.Backup.RecipientKey); err == nil {
			recovery = p
		}
	}
	now := k.now()
	for i := range rec.Keys {
		w := &rec.Keys[i]
		key, err := k.unwrap(*rec, w.ID)
		if err != nil {
			return err
		}
		if w.Envelopes == nil {
			w.Envelopes = map[string][]byte{}
		}
		for id, reg := range rec.Replicas {
			if _, ok := w.Envelopes[id]; ok {
				continue
			}
			p, err := backup.ParseStreamPublicKey(reg.PublicKey)
			if err != nil {
				return fmt.Errorf("manager: replica %s public key: %w", id, err)
			}
			if w.Envelopes[id], err = wrapDataKey(key, w.ID, recipientLabel(id), p, now); err != nil {
				return err
			}
		}
		if recovery.ID() != "" && w.RecoveryKeyID != recovery.ID() {
			if w.Recovery, err = wrapDataKey(key, w.ID, recipientLabel(""), recovery, now); err != nil {
				return err
			}
			w.RecoveryKeyID = recovery.ID()
		}
	}
	return nil
}

// RecoverDataKeys unwraps every data key of a keyring with the externally
// retained backup recovery key (B07), for example from a copy of the store
// taken out of a manager configuration backup.
func RecoverDataKeys(rec KeyringRecord, recovery backup.RecoveryKey) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, w := range rec.Keys {
		if len(w.Recovery) == 0 {
			return nil, fmt.Errorf("%w: key %q has no recovery envelope", ErrNoKey, w.ID)
		}
		b, err := unwrapDataKey(w.Recovery, w.ID, recipientLabel(""), recovery)
		if err != nil {
			return nil, fmt.Errorf("manager: recover key %q: %w", w.ID, err)
		}
		out[w.ID] = b
	}
	return out, nil
}

// RecoveredKeys is a KeyProvider over recovered keys (current = the
// keyring's current key).
type RecoveredKeys struct {
	CurrentID string
	Keys      map[string][]byte
}

// Current implements KeyProvider.
func (r RecoveredKeys) Current() (string, []byte, error) {
	b, err := r.Key(r.CurrentID)
	return r.CurrentID, b, err
}

// Key implements KeyProvider.
func (r RecoveredKeys) Key(id string) ([]byte, error) {
	if b, ok := r.Keys[id]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrNoKey, id)
}

// ReplicaKeyStatus is one replica in the keyring view.
type ReplicaKeyStatus struct {
	ID           string    `json:"id"`
	Address      string    `json:"address"`
	Leader       bool      `json:"leader"`
	PublicKeyID  string    `json:"publicKeyId,omitempty"`
	RegisteredBy string    `json:"registeredBy,omitempty"`
	RegisteredAt time.Time `json:"registeredAt,omitzero"`
	// HasCurrentKey reports whether the current data key is wrapped for it.
	HasCurrentKey bool `json:"hasCurrentKey"`
}

// KeyringStatus is the non-secret view of the keyring.
type KeyringStatus struct {
	CurrentKey  string             `json:"currentKey"`
	Recoverable bool               `json:"recoverable"`
	Replicas    []ReplicaKeyStatus `json:"replicas"`
}

// KeyringService registers replicas and reports the keyring.
type KeyringService struct {
	Svc        *Service
	Keyring    *Keyring
	Authorizer *authz.Authorizer
}

func (k *KeyringService) authorize(actor authz.Subject, act authz.Action) error {
	if d := k.Authorizer.Authorize(actor, act, settingsRes); !d.Allowed {
		return &authz.DeniedError{Decision: d}
	}
	return nil
}

// Status reports the voters and their key state (settings read).
func (k *KeyringService) Status(actor authz.Subject) (KeyringStatus, error) {
	if err := k.authorize(actor, authz.ActionRead); err != nil {
		return KeyringStatus{}, err
	}
	members, err := k.Svc.node.Members()
	if err != nil {
		return KeyringStatus{}, err
	}
	rec, _, err := Get[KeyringRecord](k.Svc.store, CollKeys, KeyringKey)
	if err != nil {
		return KeyringStatus{}, err
	}
	leader, _ := k.Svc.node.Leader()
	out := KeyringStatus{CurrentKey: rec.Value.Current}
	cur := rec.Value.key(rec.Value.Current)
	out.Recoverable = cur != nil && len(cur.Recovery) > 0
	for _, m := range members {
		st := ReplicaKeyStatus{ID: m.ID, Address: m.Address, Leader: m.ID == leader}
		if r, ok := rec.Value.Replicas[m.ID]; ok {
			if p, err := backup.ParseStreamPublicKey(r.PublicKey); err == nil {
				st.PublicKeyID = p.ID()
			}
			st.RegisteredBy, st.RegisteredAt = r.RegisteredBy, r.RegisteredAt
		}
		if cur != nil {
			_, st.HasCurrentKey = cur.Envelopes[m.ID]
		}
		out.Replicas = append(out.Replicas, st)
	}
	sort.Slice(out.Replicas, func(i, j int) bool { return out.Replicas[i].ID < out.Replicas[j].ID })
	return out, nil
}

// Register records a voter's public key and wraps every data key for it
// (settings update, audited; leader only).
func (k *KeyringService) Register(actor authz.Subject, replicaID, publicKey string) (ReplicaKeyStatus, error) {
	if err := k.authorize(actor, authz.ActionUpdate); err != nil {
		return ReplicaKeyStatus{}, err
	}
	if !k.Svc.node.IsLeader() || k.Svc.Current() == nil {
		return ReplicaKeyStatus{}, ErrNotLeader
	}
	p, err := backup.ParseStreamPublicKey(publicKey)
	if err != nil {
		return ReplicaKeyStatus{}, fmt.Errorf("%w: public key: %v", api.ErrInvalidInput, err)
	}
	members, err := k.Svc.node.Members()
	if err != nil {
		return ReplicaKeyStatus{}, err
	}
	if !slices.ContainsFunc(members, func(m metastore.Peer) bool { return m.ID == replicaID }) {
		return ReplicaKeyStatus{}, fmt.Errorf("%w: %q", ErrNotVoter, replicaID)
	}
	now := k.Svc.cfg.Now()
	_, _, err = Update(k.Svc.store, CollKeys, KeyringKey, func(cur KeyringRecord, exists bool) (KeyringRecord, error) {
		if !exists {
			return cur, ErrNoKeyring
		}
		if cur.Replicas == nil {
			cur.Replicas = map[string]ReplicaRegistration{}
		}
		if prev, ok := cur.Replicas[replicaID]; ok && prev.PublicKey != p.String() {
			// A new identity: the old envelopes are for the old key.
			for i := range cur.Keys {
				delete(cur.Keys[i].Envelopes, replicaID)
			}
		}
		cur.Replicas[replicaID] = ReplicaRegistration{PublicKey: p.String(), RegisteredBy: actor.ID, RegisteredAt: now}
		return cur, k.Keyring.distribute(k.Svc, &cur)
	})
	if err != nil {
		return ReplicaKeyStatus{}, err
	}
	k.Svc.recordEvent("", "", api.SeverityWarning, fmt.Sprintf("%s registered the data-key recipient of manager replica %s (key %s)", actor.ID, replicaID, p.ID()))
	return ReplicaKeyStatus{ID: replicaID, PublicKeyID: p.ID(), RegisteredBy: actor.ID, RegisteredAt: now, HasCurrentKey: true}, nil
}

// KeyringReconciler refreshes the keyring while this replica leads, so a
// recovery key configured later gets its envelope and every registered
// replica stays covered.
type KeyringReconciler struct {
	Keyring  *Keyring
	Interval time.Duration // default 30s
}

// Name implements Reconciler.
func (r KeyringReconciler) Name() string { return "keyring" }

// Run implements Reconciler.
func (r KeyringReconciler) Run(ctx context.Context, l *Leadership) error {
	iv := r.Interval
	if iv <= 0 {
		iv = 30 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := r.Keyring.updateDistributed(l.svc); err != nil && ctx.Err() == nil {
			l.svc.log.Warn("manager: keyring refresh failed", "err", err)
		}
	}
}
