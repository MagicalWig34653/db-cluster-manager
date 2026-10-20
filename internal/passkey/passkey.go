// SPDX-License-Identifier: AGPL-3.0-only

// Package passkey implements the WebAuthn (passkey) registration and
// authentication ceremonies for local accounts (U01) behind a small
// project-owned API. Cryptographic verification is delegated to
// github.com/go-webauthn/webauthn (see docs/decisions/0005-webauthn-library.md);
// this package owns challenge storage, relying-party configuration
// validation, the user-verification and sign-counter policies, and the link
// to the account model in internal/recovery.
//
// Policies:
//
//   - User verification is required for registration and login and is
//     checked twice: by the library and explicitly on the UV flag.
//   - Registration asks for a discoverable credential (resident key
//     required), so accounts can be passkey-only and log in without a user
//     name.
//   - Attestation conveyance is "none". Attestation statements that an
//     authenticator still sends (for example "packed" self attestation) are
//     verified by the library but never used for trust decisions; there is
//     no metadata service and no attestation root.
//   - Challenges are single-use, expire after Config.ChallengeTTL according
//     to the injected clock, and are bound to the caller-provided session
//     binding and, for registration and user-name login, to the account.
//   - Sign counter: if either the stored or the received counter is non-zero
//     and the received counter is not greater than the stored one, the
//     assertion is rejected with ErrSignCountRegression, the stored counter
//     is left unchanged and the event is recorded on the credential
//     (CloneWarnings, LastCloneWarningAt) for audit and notification. The
//     credential stays registered; a later assertion with a higher counter
//     succeeds. Authenticators that always report zero (common for synced
//     passkeys) are accepted.
//
// State is in memory only. HTTP handlers, persistence and replication are
// not implemented.
package passkey

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

// Clock returns the current time. Tests inject a fake.
type Clock func() time.Time

var (
	// ErrInvalidRequest is returned for a malformed Begin request.
	ErrInvalidRequest = errors.New("passkey: invalid request")
	// ErrVerificationFailed is returned when a ceremony response does not
	// verify. The wrapped text is diagnostic and must not be shown verbatim
	// to unauthenticated clients.
	ErrVerificationFailed = errors.New("passkey: verification failed")
	// ErrUserVerificationRequired is returned when the authenticator did not
	// set the user-verified flag.
	ErrUserVerificationRequired = errors.New("passkey: user verification required")
	// ErrSignCountRegression is returned when the signature counter did not
	// increase (possible cloned authenticator).
	ErrSignCountRegression = errors.New("passkey: signature counter did not increase")
	// ErrCredentialExists is returned when a credential ID is already
	// registered.
	ErrCredentialExists = errors.New("passkey: credential already registered")
	// ErrUnknownCredential is returned for a credential that is not
	// registered to the account.
	ErrUnknownCredential = errors.New("passkey: unknown credential")
	// ErrLastPasskey is recovery.ErrLastPasskey.
	ErrLastPasskey = recovery.ErrLastPasskey
)

// Challenge is the result of a Begin call. Options is the JSON the browser
// passes to navigator.credentials.create/get (the "publicKey" member).
type Challenge struct {
	CeremonyID string          `json:"ceremonyId"`
	Options    json.RawMessage `json:"options"`
	ExpiresAt  time.Time       `json:"expiresAt"`
}

// RegistrationRequest starts a registration ceremony.
type RegistrationRequest struct {
	// AccountID is the account the passkey is attached to.
	AccountID string
	// NewAccount creates a passkey-only account on success; the account
	// must not exist yet. Otherwise the account must exist.
	NewAccount bool
	// UserName and DisplayName are shown by the authenticator; they default
	// to AccountID and UserName.
	UserName    string
	DisplayName string
	// Label is the user's name for this passkey.
	Label string
	// Binding identifies the caller's session (for example a pre-auth or
	// authenticated browser session ID). Finish must present the same value.
	Binding string
}

// LoginRequest starts an authentication ceremony. An empty AccountID starts
// a discoverable (user-name-less) login; otherwise the allowed credentials
// are limited to that account.
type LoginRequest struct {
	AccountID string
	Binding   string
}

// LoginResult identifies the authenticated account and credential.
type LoginResult struct {
	AccountID    string `json:"accountId"`
	CredentialID string `json:"credentialId"`
	SignCount    uint32 `json:"signCount"`
}

// CredentialInfo is the non-secret view of a registered passkey.
type CredentialInfo struct {
	ID                 string    `json:"id"`
	AccountID          string    `json:"accountId"`
	RPID               string    `json:"rpId"`
	Label              string    `json:"label,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	LastUsedAt         time.Time `json:"lastUsedAt,omitzero"`
	SignCount          uint32    `json:"signCount"`
	BackupEligible     bool      `json:"backupEligible"`
	BackupState        bool      `json:"backupState"`
	AttestationFormat  string    `json:"attestationFormat,omitempty"`
	CloneWarnings      int       `json:"cloneWarnings,omitempty"`
	LastCloneWarningAt time.Time `json:"lastCloneWarningAt,omitzero"`
}

type storedCredential struct {
	info   CredentialInfo
	record webauthn.Credential
}

// Manager runs passkey ceremonies. It is safe for concurrent use.
type Manager struct {
	cfg        Config
	wa         *webauthn.WebAuthn
	accounts   AccountStore
	now        Clock
	challenges *challengeStore

	mu       sync.Mutex
	handles  map[string][]byte // account ID -> WebAuthn user handle
	byHandle map[string]string // string(user handle) -> account ID
	creds    map[string]*storedCredential
}

// New validates cfg and returns a Manager. now may be nil for time.Now.
func New(cfg Config, accounts AccountStore, now Clock) (*Manager, error) {
	if accounts == nil {
		return nil, fmt.Errorf("%w: nil account store", ErrInvalidConfig)
	}
	n, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	// Expiry is enforced by challengeStore with the injected clock; the
	// library's own wall-clock enforcement stays off (Enforce false). The
	// timeouts are only the hint sent to the browser.
	to := webauthn.TimeoutConfig{Timeout: n.ChallengeTTL, TimeoutUVD: n.ChallengeTTL}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  n.RPID,
		RPDisplayName:         n.RPDisplayName,
		RPOrigins:             n.Origins,
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			RequireResidentKey: protocol.ResidentKeyRequired(),
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			UserVerification:   protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{Login: to, Registration: to},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return &Manager{
		cfg:        n,
		wa:         wa,
		accounts:   accounts,
		now:        now,
		challenges: newChallengeStore(now, n.ChallengeTTL, n.MaxPendingCeremonies),
		handles:    make(map[string][]byte),
		byHandle:   make(map[string]string),
		creds:      make(map[string]*storedCredential),
	}, nil
}

// Config returns the normalized relying-party configuration.
func (m *Manager) Config() Config {
	c := m.cfg
	c.Origins = slices.Clone(c.Origins)
	return c
}

// waUser adapts an account to webauthn.User.
type waUser struct {
	handle  []byte
	name    string
	display string
	creds   []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.handle }
func (u *waUser) WebAuthnName() string                       { return u.name }
func (u *waUser) WebAuthnDisplayName() string                { return u.display }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func encodeID(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// handleLocked returns the account's user handle, creating a random 32-byte
// one on first use. The handle is not the account ID, so authenticators do
// not learn internal identifiers.
func (m *Manager) handleLocked(accountID string) ([]byte, error) {
	if h, ok := m.handles[accountID]; ok {
		return h, nil
	}
	h := make([]byte, 32)
	if _, err := rand.Read(h); err != nil {
		return nil, err
	}
	m.handles[accountID] = h
	m.byHandle[string(h)] = accountID
	return h, nil
}

// usableLocked returns the account's credentials that the account store
// still lists and that were registered under the current RP ID.
func (m *Manager) usableLocked(accountID string) ([]*storedCredential, error) {
	ids, err := m.accounts.PasskeyIDs(accountID)
	if err != nil {
		return nil, err
	}
	var out []*storedCredential
	for _, id := range ids {
		sc, ok := m.creds[id]
		if ok && sc.info.AccountID == accountID && sc.info.RPID == m.cfg.RPID {
			out = append(out, sc)
		}
	}
	return out, nil
}

func (m *Manager) userLocked(accountID string) (*waUser, error) {
	usable, err := m.usableLocked(accountID)
	if err != nil {
		return nil, err
	}
	h, err := m.handleLocked(accountID)
	if err != nil {
		return nil, err
	}
	u := &waUser{handle: h, name: accountID, display: accountID}
	for _, sc := range usable {
		u.creds = append(u.creds, sc.record)
	}
	return u, nil
}

func verificationError(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return fmt.Errorf("%w: %s: %s", ErrVerificationFailed, pe.Details, pe.DevInfo)
	}
	return fmt.Errorf("%w: %v", ErrVerificationFailed, err)
}

// BeginRegistration starts a registration ceremony.
func (m *Manager) BeginRegistration(req RegistrationRequest) (Challenge, error) {
	if req.AccountID == "" || req.Binding == "" {
		return Challenge{}, fmt.Errorf("%w: account ID and binding are required", ErrInvalidRequest)
	}
	m.mu.Lock()
	_, err := m.accounts.PasskeyIDs(req.AccountID)
	switch {
	case req.NewAccount && err == nil:
		err = recovery.ErrAccountExists
	case req.NewAccount && errors.Is(err, recovery.ErrUnknownAccount):
		err = nil
	}
	var user *waUser
	if err == nil {
		if req.NewAccount {
			var h []byte
			if h, err = m.handleLocked(req.AccountID); err == nil {
				user = &waUser{handle: h}
			}
		} else {
			user, err = m.userLocked(req.AccountID)
		}
	}
	m.mu.Unlock()
	if err != nil {
		return Challenge{}, err
	}
	user.name = req.UserName
	if user.name == "" {
		user.name = req.AccountID
	}
	user.display = req.DisplayName
	if user.display == "" {
		user.display = user.name
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range user.creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, session, err := m.wa.BeginRegistration(user,
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithExclusions(exclude))
	if err != nil {
		return Challenge{}, err
	}
	opts, err := json.Marshal(creation.Response)
	if err != nil {
		return Challenge{}, err
	}
	id, exp, err := m.challenges.put(&pending{
		kind: kindRegistration, binding: req.Binding, accountID: req.AccountID,
		newAccount: req.NewAccount, label: req.Label, handle: user.handle, session: *session,
	})
	if err != nil {
		return Challenge{}, err
	}
	return Challenge{CeremonyID: id, Options: opts, ExpiresAt: exp}, nil
}

// FinishRegistration verifies the browser's PublicKeyCredential JSON
// (credential.toJSON()) and attaches the new passkey to the account.
func (m *Manager) FinishRegistration(ceremonyID, binding string, response []byte) (CredentialInfo, error) {
	p, err := m.challenges.take(ceremonyID, kindRegistration, binding)
	if err != nil {
		return CredentialInfo{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return CredentialInfo{}, verificationError(err)
	}
	if !parsed.Response.AttestationObject.AuthData.Flags.HasUserVerified() {
		return CredentialInfo{}, ErrUserVerificationRequired
	}
	cred, err := m.wa.CreateCredential(&waUser{handle: p.handle}, p.session, parsed)
	if err != nil {
		return CredentialInfo{}, verificationError(err)
	}
	cred.Authenticator.CloneWarning = false
	id := encodeID(cred.ID)
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.creds[id]; ok {
		return CredentialInfo{}, ErrCredentialExists
	}
	if h, ok := m.handles[p.accountID]; !ok || !bytes.Equal(h, p.handle) {
		// Cannot happen while handles are never deleted; refuse rather
		// than register a credential whose user handle maps elsewhere.
		return CredentialInfo{}, fmt.Errorf("%w: user handle changed during ceremony", ErrVerificationFailed)
	}
	rc := recovery.Credential{ID: id, Kind: recovery.CredentialPasskey, Label: p.label, CreatedAt: now}
	if p.newAccount {
		err = m.accounts.CreateWithPasskey(p.accountID, rc)
	} else {
		err = m.accounts.AddPasskey(p.accountID, rc)
	}
	if err != nil {
		return CredentialInfo{}, err
	}
	sc := &storedCredential{record: *cred, info: CredentialInfo{
		ID: id, AccountID: p.accountID, RPID: m.cfg.RPID, Label: p.label, CreatedAt: now,
		SignCount: cred.Authenticator.SignCount, BackupEligible: cred.Flags.BackupEligible,
		BackupState: cred.Flags.BackupState, AttestationFormat: cred.AttestationFormat,
	}}
	m.creds[id] = sc
	return sc.info, nil
}

// BeginLogin starts an authentication ceremony. With an AccountID, an
// unknown account returns recovery.ErrUnknownAccount; an HTTP layer must not
// expose that difference to unauthenticated clients (prefer discoverable
// login).
func (m *Manager) BeginLogin(req LoginRequest) (Challenge, error) {
	if req.Binding == "" {
		return Challenge{}, fmt.Errorf("%w: binding is required", ErrInvalidRequest)
	}
	uv := webauthn.WithUserVerification(protocol.VerificationRequired)
	var (
		assertion *protocol.CredentialAssertion
		session   *webauthn.SessionData
		err       error
	)
	if req.AccountID == "" {
		assertion, session, err = m.wa.BeginDiscoverableLogin(uv)
	} else {
		m.mu.Lock()
		user, uerr := m.userLocked(req.AccountID)
		m.mu.Unlock()
		if uerr != nil {
			return Challenge{}, uerr
		}
		if len(user.creds) == 0 {
			return Challenge{}, fmt.Errorf("%w: account has no usable passkey", ErrUnknownCredential)
		}
		assertion, session, err = m.wa.BeginLogin(user, uv)
	}
	if err != nil {
		return Challenge{}, err
	}
	opts, err := json.Marshal(assertion.Response)
	if err != nil {
		return Challenge{}, err
	}
	id, exp, err := m.challenges.put(&pending{
		kind: kindLogin, binding: req.Binding, accountID: req.AccountID, session: *session,
	})
	if err != nil {
		return Challenge{}, err
	}
	return Challenge{CeremonyID: id, Options: opts, ExpiresAt: exp}, nil
}

// FinishLogin verifies the browser's assertion JSON and returns the
// authenticated account. Session issuance is the caller's job (for example
// recovery.Sessions.Issue).
func (m *Manager) FinishLogin(ceremonyID, binding string, response []byte) (LoginResult, error) {
	p, err := m.challenges.take(ceremonyID, kindLogin, binding)
	if err != nil {
		return LoginResult{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return LoginResult{}, verificationError(err)
	}
	if !parsed.Response.AuthenticatorData.Flags.HasUserVerified() {
		return LoginResult{}, ErrUserVerificationRequired
	}

	// Verification runs under m.mu so the counter check and update are
	// atomic per credential. This serializes logins; acceptable for the
	// in-memory prototype.
	m.mu.Lock()
	defer m.mu.Unlock()
	accountID := p.accountID
	var cred *webauthn.Credential
	if accountID != "" {
		user, uerr := m.userLocked(accountID)
		if uerr != nil {
			return LoginResult{}, uerr
		}
		cred, err = m.wa.ValidateLogin(user, p.session, parsed)
	} else {
		handler := func(_, userHandle []byte) (webauthn.User, error) {
			id, ok := m.byHandle[string(userHandle)]
			if !ok {
				return nil, ErrUnknownCredential
			}
			accountID = id
			return m.userLocked(id)
		}
		_, cred, err = m.wa.ValidatePasskeyLogin(handler, p.session, parsed)
	}
	if err != nil {
		return LoginResult{}, verificationError(err)
	}
	id := encodeID(cred.ID)
	sc, ok := m.creds[id]
	if !ok || sc.info.AccountID != accountID {
		return LoginResult{}, ErrUnknownCredential
	}
	now := m.now()
	if cred.Authenticator.CloneWarning {
		sc.info.CloneWarnings++
		sc.info.LastCloneWarningAt = now
		return LoginResult{}, fmt.Errorf("%w: stored %d, received %d", ErrSignCountRegression,
			sc.record.Authenticator.SignCount, parsed.Response.AuthenticatorData.Counter)
	}
	sc.record = *cred
	sc.info.SignCount = cred.Authenticator.SignCount
	sc.info.BackupState = cred.Flags.BackupState
	sc.info.LastUsedAt = now
	return LoginResult{AccountID: accountID, CredentialID: id, SignCount: sc.info.SignCount}, nil
}

// ListCredentials returns the account's passkeys that the account store
// lists and whose key material this Manager holds, oldest first. Passkeys
// registered under a different RP ID are included (RPID shows it) but
// cannot be used to log in.
func (m *Manager) ListCredentials(accountID string) ([]CredentialInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids, err := m.accounts.PasskeyIDs(accountID)
	if err != nil {
		return nil, err
	}
	var out []CredentialInfo
	for _, id := range ids {
		if sc, ok := m.creds[id]; ok && sc.info.AccountID == accountID {
			out = append(out, sc.info)
		}
	}
	slices.SortFunc(out, func(a, b CredentialInfo) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// RemoveCredential removes a passkey. The account's last usable passkey
// cannot be removed (ErrLastPasskey); the account store enforces the same
// rule, including its pending-enrollment exception. Key material for a
// passkey the account store no longer lists is dropped locally.
func (m *Manager) RemoveCredential(accountID, credentialID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.creds[credentialID]
	if !ok || sc.info.AccountID != accountID {
		return ErrUnknownCredential
	}
	ids, err := m.accounts.PasskeyIDs(accountID)
	if err != nil {
		return err
	}
	if !slices.Contains(ids, credentialID) {
		delete(m.creds, credentialID)
		return nil
	}
	held := 0
	for _, id := range ids {
		if c, ok := m.creds[id]; ok && c.info.AccountID == accountID {
			held++
		}
	}
	if held <= 1 {
		return ErrLastPasskey
	}
	if err := m.accounts.RemovePasskey(accountID, credentialID); err != nil {
		return err
	}
	delete(m.creds, credentialID)
	return nil
}

// PruneCeremonies removes expired pending ceremonies.
func (m *Manager) PruneCeremonies() int { return m.challenges.prune() }
