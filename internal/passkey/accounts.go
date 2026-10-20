// SPDX-License-Identifier: AGPL-3.0-only

package passkey

import (
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

// AccountStore is the account model the passkey layer attaches credentials
// to. The account store is authoritative for which passkeys an account has:
// a passkey whose ID it no longer lists (for example after
// recovery.Accounts.BeginPasskeyReenrollment) cannot be used to log in, even
// though its key material may still be held by the Manager.
//
// Implementations must enforce the "last passkey cannot be removed" rule in
// RemovePasskey; the Manager additionally checks it itself.
type AccountStore interface {
	// PasskeyIDs lists the passkey credential IDs of an account. It returns
	// recovery.ErrUnknownAccount for a missing account.
	PasskeyIDs(accountID string) ([]string, error)
	// CreateWithPasskey creates a passkey-only account holding c.
	CreateWithPasskey(accountID string, c recovery.Credential) error
	// AddPasskey attaches c to an existing account.
	AddPasskey(accountID string, c recovery.Credential) error
	// RemovePasskey detaches a passkey from an account.
	RemovePasskey(accountID, credentialID string) error
}

// RecoveryAccounts adapts *recovery.Accounts to AccountStore. Passkeys map
// to recovery.CredentialPasskey entries whose ID is the base64url
// (unpadded) WebAuthn credential ID; recovery stores no key material.
type RecoveryAccounts struct {
	Accounts *recovery.Accounts
}

var _ AccountStore = RecoveryAccounts{}

// PasskeyIDs implements AccountStore.
func (r RecoveryAccounts) PasskeyIDs(accountID string) ([]string, error) {
	a, err := r.Accounts.Get(accountID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range a.Credentials {
		if c.Kind == recovery.CredentialPasskey {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// CreateWithPasskey implements AccountStore.
func (r RecoveryAccounts) CreateWithPasskey(accountID string, c recovery.Credential) error {
	return r.Accounts.Create(recovery.Account{ID: accountID, Credentials: []recovery.Credential{c}})
}

// AddPasskey implements AccountStore.
func (r RecoveryAccounts) AddPasskey(accountID string, c recovery.Credential) error {
	return r.Accounts.AddCredential(accountID, c)
}

// RemovePasskey implements AccountStore.
func (r RecoveryAccounts) RemovePasskey(accountID, credentialID string) error {
	return r.Accounts.RemoveCredential(accountID, credentialID)
}
