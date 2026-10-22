// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/collect"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

// Persistence of the authentication and authorization state.
//
// The in-memory stores of internal/recovery, internal/passkey and
// internal/authz get write-through persisters that write into the
// replicated store. Accounts and the policy are stored in clear (they hold
// identifiers only); session digests, recovery-code hashes and passkey
// records are sealed (secrets.go). Writes need the metastore leader, so the
// auth API only works on the leader; a follower refuses state changes
// (they fail at persistence) and must not serve the API (see Service).
//
// An AuthStack is built from the store when this replica gains leadership
// and is discarded when it loses it, so a new leader always starts from the
// replicated state. Login ceremonies in flight and the rate limiter state
// are not replicated and start empty on a new leader.

// authPersister implements the persister interfaces on a Store.
type authPersister struct {
	store *Store
	keys  KeyProvider
	// sessColl is the collection of the session store it serves.
	sessColl string
}

var (
	_ recovery.AccountPersister   = authPersister{}
	_ recovery.SessionPersister   = authPersister{}
	_ recovery.CodePersister      = authPersister{}
	_ passkey.CredentialPersister = authPersister{}
	_ authz.PolicyPersister       = authPersister{}
)

func (p authPersister) PutAccount(a recovery.Account) error {
	_, err := Put(p.store, CollAccounts, a.ID, a, nil)
	return err
}

func (p authPersister) PutSession(r recovery.SessionRecord) error {
	return PutSealed(p.store, p.keys, p.sessColl, r.Digest, r)
}

func (p authPersister) DeleteSession(digest string) error {
	return Delete(p.store, p.sessColl, digest, nil)
}

type storedCodes struct {
	UserID string           `json:"userId"`
	Set    recovery.CodeSet `json:"set"`
}

func (p authPersister) PutCodes(userID string, set recovery.CodeSet) error {
	return PutSealed(p.store, p.keys, CollRecoveryCodes, userID, storedCodes{UserID: userID, Set: set})
}

func (p authPersister) DeleteCodes(userID string) error {
	return Delete(p.store, CollRecoveryCodes, userID, nil)
}

func (p authPersister) PutCredential(c passkey.StoredCredential) error {
	return PutSealed(p.store, p.keys, CollPasskeys, c.Info.ID, c)
}

func (p authPersister) DeleteCredential(id string) error {
	return Delete(p.store, CollPasskeys, id, nil)
}

func (p authPersister) PutPolicy(st authz.PolicyState) error {
	_, err := Put(p.store, CollPolicy, PolicyKey, st, nil)
	return err
}

// AuditStore is an authz.AuditSink that appends records to the replicated
// store. Every record is a document with a time-ordered key. Retention is
// bounded by MaxRecords (oldest deleted first; 0 means DefaultAuditRecords).
type AuditStore struct {
	Store      *Store
	MaxRecords int
	Now        func() time.Time
}

// DefaultAuditRecords is the default audit retention (a starting value).
const DefaultAuditRecords = 10000

// Record implements authz.AuditSink. It fails (and the Authorizer fails
// closed) when the record cannot be committed.
func (a AuditStore) Record(r authz.AuditRecord) error {
	at := r.Time
	if at.IsZero() {
		now := time.Now
		if a.Now != nil {
			now = a.Now
		}
		at = now()
	}
	if _, err := Put(a.Store, CollAudit, timeKey(at), r, IfVersion(0)); err != nil {
		return fmt.Errorf("manager: store audit record: %w", err)
	}
	a.trim()
	return nil
}

// trim deletes the oldest records above the retention bound (best effort).
func (a AuditStore) trim() {
	limit := a.MaxRecords
	if limit <= 0 {
		limit = DefaultAuditRecords
	}
	docs, err := a.Store.RawList(CollAudit)
	if err != nil || len(docs) <= limit {
		return
	}
	var ops []metastore.DocOp
	for _, d := range docs[:len(docs)-limit] {
		ops = append(ops, metastore.DocOp{Collection: CollAudit, Key: d.Key, Delete: true, IfVersion: IfVersion(d.Version)})
	}
	_, _ = a.Store.Txn(ops...)
}

// AuditRecords returns the stored audit records, oldest first.
func AuditRecords(s *Store) ([]authz.AuditRecord, error) {
	docs, err := List[authz.AuditRecord](s, CollAudit)
	if err != nil {
		return nil, err
	}
	out := make([]authz.AuditRecord, len(docs))
	for i, d := range docs {
		out[i] = d.Value
	}
	return out, nil
}

// AuthConfig configures an AuthStack.
type AuthConfig struct {
	Passkey passkey.Config
	// SessionPolicy and RecoverySessionPolicy default to
	// recovery.DefaultSessionPolicy and 10/15 minutes.
	SessionPolicy         recovery.SessionPolicy
	RecoverySessionPolicy recovery.SessionPolicy
	Now                   func() time.Time
	Logger                *slog.Logger
}

// AuthStack holds the auth stores the API needs, loaded from and persisted
// to the replicated store.
type AuthStack struct {
	Accounts         *recovery.Accounts
	Passkeys         *passkey.Manager
	Sessions         *recovery.Sessions
	RecoverySessions *recovery.Sessions
	Codes            *recovery.CodeVerifier
	URLTokens        *recovery.URLTokens
	Limiter          *recovery.Limiter
	Policy           *authz.Policy
	Authorizer       *authz.Authorizer
	Audit            AuditStore
}

// LoadAuth builds an AuthStack from the store. Call it on the leader after
// a barrier (Service does), so the stack reflects every committed change.
func LoadAuth(store *Store, keys KeyProvider, cfg AuthConfig) (*AuthStack, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	var now recovery.Clock
	if cfg.Now != nil {
		now = cfg.Now
	}
	onErr := func(err error) { log.Warn("manager: best-effort auth persistence failed", "err", err) }

	accounts := recovery.NewAccounts()
	accDocs, err := List[recovery.Account](store, CollAccounts)
	if err != nil {
		return nil, err
	}
	accs := make([]recovery.Account, len(accDocs))
	for i, d := range accDocs {
		accs[i] = d.Value
	}
	if err := accounts.Load(accs); err != nil {
		return nil, fmt.Errorf("manager: load accounts: %w", err)
	}

	var pnow passkey.Clock
	if cfg.Now != nil {
		pnow = cfg.Now
	}
	pk, err := passkey.New(cfg.Passkey, passkey.RecoveryAccounts{Accounts: accounts}, pnow)
	if err != nil {
		return nil, err
	}
	credDocs, err := ListSealed[passkey.StoredCredential](store, keys, CollPasskeys)
	if err != nil {
		return nil, err
	}
	creds := make([]passkey.StoredCredential, len(credDocs))
	for i, d := range credDocs {
		creds[i] = d.Value
	}
	if err := pk.Load(creds); err != nil {
		return nil, fmt.Errorf("manager: load passkeys: %w", err)
	}

	sp := cfg.SessionPolicy
	if sp.IdleTimeout == 0 {
		sp = recovery.DefaultSessionPolicy
	}
	rp := cfg.RecoverySessionPolicy
	if rp.IdleTimeout == 0 {
		rp = recovery.SessionPolicy{IdleTimeout: 10 * time.Minute, AbsoluteTimeout: 15 * time.Minute}
	}
	loadSessions := func(coll string, pol recovery.SessionPolicy) (*recovery.Sessions, error) {
		s := recovery.NewSessions(pol, accounts, now)
		docs, err := ListSealed[recovery.SessionRecord](store, keys, coll)
		if err != nil {
			return nil, err
		}
		recs := make([]recovery.SessionRecord, len(docs))
		for i, d := range docs {
			recs[i] = d.Value
		}
		if err := s.Load(recs); err != nil {
			return nil, err
		}
		s.SetPersister(authPersister{store: store, keys: keys, sessColl: coll}, onErr)
		return s, nil
	}
	sessions, err := loadSessions(CollSessions, sp)
	if err != nil {
		return nil, err
	}
	recSessions, err := loadSessions(CollRecoverySessions, rp)
	if err != nil {
		return nil, err
	}

	codeLimiter := recovery.NewLimiter(recovery.DefaultLockoutPolicy, now)
	codes := recovery.NewCodeVerifier(codeLimiter, now)
	codeDocs, err := ListSealed[storedCodes](store, keys, CollRecoveryCodes)
	if err != nil {
		return nil, err
	}
	sets := map[string]recovery.CodeSet{}
	for _, d := range codeDocs {
		sets[d.Value.UserID] = d.Value.Set
	}
	codes.Load(sets)

	policy := authz.NewPolicy()
	if st, ok, err := Get[authz.PolicyState](store, CollPolicy, PolicyKey); err != nil {
		return nil, err
	} else if ok {
		if err := policy.Load(st.Value); err != nil {
			return nil, fmt.Errorf("manager: load policy: %w", err)
		}
	}

	p := authPersister{store: store, keys: keys}
	accounts.SetPersister(p)
	pk.SetPersister(p, onErr)
	codes.SetPersister(p, onErr)
	policy.SetPersister(p)
	audit := AuditStore{Store: store, Now: cfg.Now}
	return &AuthStack{
		Accounts:         accounts,
		Passkeys:         pk,
		Sessions:         sessions,
		RecoverySessions: recSessions,
		Codes:            codes,
		URLTokens:        recovery.NewURLTokens(now),
		Limiter:          recovery.NewLimiter(recovery.DefaultLockoutPolicy, now),
		Policy:           policy,
		Authorizer:       authz.NewAuthorizer(policy, audit, cfg.Now),
		Audit:            audit,
	}, nil
}

// AdministratorExists reports whether an existing account holds the
// administrator role at global scope.
func (a *AuthStack) AdministratorExists() bool {
	for _, acc := range a.Accounts.List() {
		for _, b := range a.Policy.Bindings(authz.Subject{ID: acc.ID, Kind: authz.SubjectUser}) {
			if b.Role == authz.RoleAdministrator && b.Scope == authz.Global {
				return true
			}
		}
	}
	return false
}

// Prune removes expired ceremonies, sessions, tokens and limiter entries.
func (a *AuthStack) Prune() {
	a.Passkeys.PruneCeremonies()
	a.Sessions.Prune()
	a.RecoverySessions.Prune()
	a.URLTokens.Prune()
	a.Limiter.Prune()
}

// EnsureSystemBinding binds the manager's own subject (collect.SystemSubject)
// to the manager system role at global scope if it is not bound yet. Alert
// evaluation and scheduled backups run as that subject; without the
// binding they would see nothing and start nothing (fail closed). It
// reports whether a binding was added.
func EnsureSystemBinding(p *authz.Policy) (bool, error) {
	b := authz.Binding{Subject: collect.SystemSubject, Role: authz.RoleManagerSystem, Scope: authz.Global}
	for _, x := range p.Bindings(b.Subject) {
		if x == b {
			return false, nil
		}
	}
	if err := p.Bind(b); err != nil && !errors.Is(err, authz.ErrBindingExists) {
		return false, err
	}
	return true, nil
}
