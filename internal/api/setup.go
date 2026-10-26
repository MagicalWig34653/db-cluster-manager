// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

// First-run setup (docs/DESIGN.md "First run", new installation): create the
// first administrator with a passkey and a recovery-code set. Setup requires
// the one-time setup token and is disabled for good once an administrator
// exists. Restoring a manager configuration is not implemented.

// maxPendingSetups bounds unfinished setup ceremonies.
const maxPendingSetups = 16

// SetupStatus reports whether first-run setup is still possible.
type SetupStatus struct {
	SetupRequired bool `json:"setupRequired"`
}

// SetupResult is returned once, when the first administrator is created.
type SetupResult struct {
	AccountID string `json:"accountId"`
	// RecoveryCodes are shown exactly once; only hashes are stored.
	RecoveryCodes []string `json:"recoveryCodes"`
}

func (s *server) setupAvailable() bool {
	if !s.cfg.SetupToken.Active() {
		return false
	}
	if s.cfg.AdministratorExists != nil && s.cfg.AdministratorExists() {
		return false
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return !s.setupDone
}

func (s *server) handleSetupStatus(w http.ResponseWriter, _ *http.Request, _ *caller) {
	writeJSON(w, http.StatusOK, SetupStatus{SetupRequired: s.setupAvailable()})
}

// checkSetupToken verifies the setup token under the per-IP limiter.
func (s *server) checkSetupToken(w http.ResponseWriter, r *http.Request, token string) bool {
	key := "setup|" + clientIP(r)
	if err := s.cfg.Limiter.Begin(key); err != nil {
		s.writeLocked(w, r, err)
		return false
	}
	if !s.cfg.SetupToken.Verify(token) {
		s.log.Warn("api: wrong setup token", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return false
	}
	s.cfg.Limiter.Succeed(key)
	return true
}

func (s *server) handleSetupBegin(w http.ResponseWriter, r *http.Request, _ *caller) {
	var req struct {
		SetupToken   string `json:"setupToken"`
		AccountID    string `json:"accountId"`
		DisplayName  string `json:"displayName"`
		PasskeyLabel string `json:"passkeyLabel"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if !s.setupAvailable() {
		writeError(w, http.StatusConflict, errSetupUnavailable)
		return
	}
	if !s.checkSetupToken(w, r, req.SetupToken) {
		return
	}
	if !validAccountID(req.AccountID) || len(req.DisplayName) > 128 || len(req.PasskeyLabel) > 64 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	binding, err := s.newCeremonyBinding(w)
	if err != nil {
		s.writeCeremonyStartError(w, err)
		return
	}
	ch, err := s.cfg.Passkeys.BeginRegistration(passkey.RegistrationRequest{
		AccountID: req.AccountID, NewAccount: true, UserName: req.AccountID,
		DisplayName: req.DisplayName, Label: req.PasskeyLabel, Binding: binding,
	})
	if err != nil {
		s.writeCeremonyStartError(w, err)
		return
	}
	s.setupMu.Lock()
	if len(s.pendingSetup) >= maxPendingSetups {
		clear(s.pendingSetup)
	}
	s.pendingSetup[ch.CeremonyID] = req.AccountID
	s.setupMu.Unlock()
	writeJSON(w, http.StatusOK, ch)
}

func (s *server) handleSetupFinish(w http.ResponseWriter, r *http.Request, _ *caller) {
	var req struct {
		SetupToken string `json:"setupToken"`
		finishRequest
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if !s.setupAvailable() {
		writeError(w, http.StatusConflict, errSetupUnavailable)
		return
	}
	if !s.checkSetupToken(w, r, req.SetupToken) {
		return
	}
	binding, ok := s.ceremonyBinding(r)
	if !ok {
		s.log.Info("api: setup finish without ceremony cookie", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return
	}
	s.clearCookie(w, s.cookies.ceremony)

	// Serialize finishing so two ceremonies cannot both create an
	// administrator.
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupDone {
		writeError(w, http.StatusConflict, errSetupUnavailable)
		return
	}
	accountID, ok := s.pendingSetup[req.CeremonyID]
	delete(s.pendingSetup, req.CeremonyID)
	if !ok {
		s.log.Info("api: setup finish for unknown ceremony", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return
	}
	const change = "create first administrator"
	if !s.auditAttempt(w, accountID, authz.ActionCreate, reasonFirstRunSetup, change) {
		return
	}
	admin := authz.Binding{Subject: userSubject(accountID), Role: authz.RoleAdministrator, Scope: authz.Global}
	if err := s.cfg.Authorizer.Policy().Bind(admin); err != nil {
		s.log.Error("api: bind first administrator", "account", accountID, "err", err)
		s.auditResult(accountID, authz.ActionCreate, reasonFirstRunSetup, change, err)
		writeError(w, http.StatusInternalServerError, errInternal)
		return
	}
	if _, err := s.cfg.Passkeys.FinishRegistration(req.CeremonyID, binding, req.Credential); err != nil {
		_ = s.cfg.Authorizer.Policy().Unbind(admin)
		s.log.Info("api: setup passkey registration failed", "remote", clientIP(r))
		s.auditResult(accountID, authz.ActionCreate, reasonFirstRunSetup, change, err)
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return
	}
	s.setupDone = true
	s.cfg.SetupToken.Invalidate()

	codes, set, err := recovery.GenerateCodes(recovery.DefaultCodeCount, s.cfg.RecoveryCodeIterations, s.now())
	if err == nil {
		s.cfg.Codes.Enroll(accountID, set)
		err = s.cfg.Accounts.AddCredential(accountID, recovery.Credential{
			ID: "recovery-codes", Kind: recovery.CredentialRecoveryCodes, Label: "Recovery codes", CreatedAt: s.now().UTC(),
		})
	}
	s.auditResult(accountID, authz.ActionCreate, reasonFirstRunSetup, change, err)
	if err != nil {
		// The administrator exists with a passkey; only the recovery codes
		// are missing and can be generated later.
		s.log.Error("api: enrol recovery codes for first administrator", "account", accountID, "err", err)
		codes = nil
	}
	if !s.startSession(w, r, accountID) {
		return
	}
	writeJSON(w, http.StatusCreated, SetupResult{AccountID: accountID, RecoveryCodes: codes})
}
