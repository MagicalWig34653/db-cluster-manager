// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
)

// Audit reasons for state changes performed by the authentication endpoints,
// which are not authz policy decisions.
const (
	reasonFirstRunSetup   authz.Reason = "first_run_setup"
	reasonAuthenticated   authz.Reason = "passkey_authenticated"
	reasonSelfService     authz.Reason = "self_service"
	reasonRecovery        authz.Reason = "recovery_credential"
	reasonRecoveryEnrolls authz.Reason = "recovery_reenrolment"
	reasonSignedOut       authz.Reason = "signed_out"
)

// Cookies. Every cookie is HttpOnly, SameSite=Strict and Path=/. Secure (and
// the __Host- prefix, which requires Secure and forbids Domain) is used unless
// every configured origin is http://localhost, the only plain-HTTP origin the
// passkey configuration accepts.
type cookieNames struct {
	session, recovery, ceremony string
}

func newCookieNames(secure bool) cookieNames {
	p := ""
	if secure {
		p = "__Host-"
	}
	return cookieNames{session: p + "dcm_session", recovery: p + "dcm_recovery", ceremony: p + "dcm_ceremony"}
}

// ceremonyCookieTTL matches the default passkey challenge lifetime.
const ceremonyCookieTTL = passkey.DefaultChallengeTTL

func (s *server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(maxAge / time.Second),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	})
}

func (s *server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	})
}

// newCeremonyBinding starts a pre-authentication browser binding: a random
// cookie whose digest binds a passkey ceremony to this browser, so a
// ceremony ID observed elsewhere cannot be finished from another client.
func (s *server) newCeremonyBinding(w http.ResponseWriter) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	s.setCookie(w, s.cookies.ceremony, v, ceremonyCookieTTL)
	return bindingOf(v), nil
}

func (s *server) ceremonyBinding(r *http.Request) (string, bool) {
	ck, err := r.Cookie(s.cookies.ceremony)
	if err != nil || ck.Value == "" {
		return "", false
	}
	return bindingOf(ck.Value), true
}

func bindingOf(v string) string {
	d := sha256.Sum256([]byte(v))
	return "pre:" + hex.EncodeToString(d[:])
}

func sessionBinding(sess recovery.Session) string { return "session:" + sess.ID }

func (s *server) startSession(w http.ResponseWriter, r *http.Request, accountID string) bool {
	token, _, err := s.cfg.Sessions.Issue(accountID)
	if err != nil {
		s.log.Error("api: issue session", "account", accountID, "err", err)
		writeError(w, http.StatusInternalServerError, errInternal)
		return false
	}
	// Drop any previous session presented by this browser.
	if ck, err := r.Cookie(s.cookies.session); err == nil {
		s.cfg.Sessions.Revoke(ck.Value)
	}
	if ck, err := r.Cookie(s.cookies.recovery); err == nil {
		s.cfg.RecoverySessions.Revoke(ck.Value)
		s.clearCookie(w, s.cookies.recovery)
	}
	s.setCookie(w, s.cookies.session, token, 0)
	return true
}

// decodeJSON reads a JSON body into v. An empty body is accepted only when
// allowEmpty is set. It writes the error response itself.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	if err := readJSON(r, v, allowEmpty); err != nil {
		writeError(w, err.status, err.code)
		return false
	}
	return true
}

// requestError is a client error found while reading a request. Resolvers
// return it so writeServiceError answers with its status and code.
type requestError struct {
	status int
	code   string
}

func (e *requestError) Error() string { return "api: request rejected: " + e.code }

// readJSON reads a JSON body into v (unknown fields rejected, one value).
func readJSON(r *http.Request, v any, allowEmpty bool) *requestError {
	if r.ContentLength == 0 && allowEmpty {
		return nil
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return &requestError{http.StatusUnsupportedMediaType, errUnsupportedFormat}
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			return &requestError{http.StatusRequestEntityTooLarge, errRequestTooLarge}
		case errors.Is(err, io.EOF) && allowEmpty:
			return nil
		}
		return &requestError{http.StatusBadRequest, errBadRequest}
	}
	if dec.More() {
		return &requestError{http.StatusBadRequest, errBadRequest}
	}
	return nil
}

// validAccountID bounds account IDs so they are safe in limiter keys, logs
// and audit records.
func validAccountID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == '@':
		default:
			return false
		}
	}
	return true
}

func accountResource(id string) authz.Resource {
	return authz.Resource{Type: authz.ResourceUserAccount, Scope: authz.Global, ID: id}
}

// audit writes one audit record of a state change performed by an
// authentication endpoint.
func (s *server) audit(actor string, act authz.Action, outcome authz.Outcome, reason authz.Reason, change, errText string) error {
	return s.cfg.AuditSink.Record(authz.AuditRecord{
		Time: s.now().UTC(), Actor: userSubject(actor), Action: act,
		Resource: accountResource(actor), Outcome: outcome, Reason: reason,
		Change: change, Error: errText,
	})
}

// auditAttempt records an allowed state change before it is applied. If the
// record cannot be stored, it answers 503 and returns false: the change must
// not happen (fail closed).
func (s *server) auditAttempt(w http.ResponseWriter, actor string, act authz.Action, reason authz.Reason, change string) bool {
	if err := s.audit(actor, act, authz.OutcomeAllowed, reason, change, ""); err != nil {
		s.log.Error("api: request denied because the audit record could not be stored", "actor", actor, "change", change, "err", err)
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
		return false
	}
	return true
}

// auditResult records the result of an applied change. The change cannot be
// undone at this point, so a failing sink is logged.
func (s *server) auditResult(actor string, act authz.Action, reason authz.Reason, change string, err error) {
	out, text := authz.OutcomeSucceeded, ""
	if err != nil {
		out, text = authz.OutcomeFailed, "change failed"
	}
	if serr := s.audit(actor, act, out, reason, change, text); serr != nil {
		s.log.Error("api: result audit record could not be stored", "actor", actor, "change", change, "err", serr)
	}
}

type finishRequest struct {
	CeremonyID string          `json:"ceremonyId"`
	Credential json.RawMessage `json:"credential"`
}

func (s *server) writeCeremonyStartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, passkey.ErrTooManyCeremonies):
		s.log.Warn("api: pending ceremony limit reached")
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
	case errors.Is(err, passkey.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, errBadRequest)
	default:
		s.log.Error("api: begin ceremony", "err", err)
		writeError(w, http.StatusInternalServerError, errInternal)
	}
}

// --- login ---

func (s *server) handleLoginBegin(w http.ResponseWriter, r *http.Request, _ *caller) {
	var req struct{}
	if !decodeJSON(w, r, &req, true) {
		return
	}
	binding, err := s.newCeremonyBinding(w)
	if err != nil {
		s.writeCeremonyStartError(w, err)
		return
	}
	// Discoverable login only: the client never names an account, so the
	// response cannot reveal whether one exists.
	ch, err := s.cfg.Passkeys.BeginLogin(passkey.LoginRequest{Binding: binding})
	if err != nil {
		s.writeCeremonyStartError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// SessionInfo describes the caller's session.
type SessionInfo struct {
	AccountID string `json:"accountId"`
	// Restricted is set for a recovery session, which only allows passkey
	// re-enrolment.
	Restricted               bool      `json:"restricted"`
	PendingPasskeyEnrollment bool      `json:"pendingPasskeyEnrollment"`
	CreatedAt                time.Time `json:"createdAt"`
}

func (s *server) handleLoginFinish(w http.ResponseWriter, r *http.Request, _ *caller) {
	binding, ok := s.ceremonyBinding(r)
	if !ok {
		s.log.Info("api: login finish without ceremony cookie", "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return
	}
	var req finishRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	s.clearCookie(w, s.cookies.ceremony)
	res, err := s.cfg.Passkeys.FinishLogin(req.CeremonyID, binding, req.Credential)
	if err != nil {
		s.log.Info("api: login failed", "remote", clientIP(r), "err", err)
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return
	}
	if !s.auditAttempt(w, res.AccountID, authz.ActionCreate, reasonAuthenticated, "sign in with passkey") {
		return
	}
	if !s.startSession(w, r, res.AccountID) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"accountId": res.AccountID})
}

// handleLogout ends the presented session. Deliberate exception to "a
// failing audit sink denies the request": refusing to end a session would
// keep a credential alive, which is the less safe outcome. The failure is
// logged instead.
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request, c *caller) {
	if c.restricted {
		s.cfg.RecoverySessions.Revoke(c.token)
	} else {
		s.cfg.Sessions.Revoke(c.token)
	}
	s.clearCookie(w, s.cookies.session)
	s.clearCookie(w, s.cookies.recovery)
	if err := s.audit(c.subject.ID, authz.ActionDelete, authz.OutcomeSucceeded, reasonSignedOut, "sign out", ""); err != nil {
		s.log.Error("api: sign-out audit record could not be stored", "actor", c.subject.ID, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleSession(w http.ResponseWriter, _ *http.Request, c *caller) {
	acc, err := s.cfg.Accounts.Get(c.subject.ID)
	if err != nil {
		s.log.Error("api: session account lookup", "account", c.subject.ID, "err", err)
		writeError(w, http.StatusUnauthorized, errUnauthenticated)
		return
	}
	writeJSON(w, http.StatusOK, SessionInfo{
		AccountID: acc.ID, Restricted: c.restricted,
		PendingPasskeyEnrollment: acc.PendingPasskeyEnrollment, CreatedAt: c.session.CreatedAt,
	})
}

// --- passkey registration on the caller's own account ---

func (s *server) handlePasskeyBegin(w http.ResponseWriter, r *http.Request, c *caller) {
	var req struct {
		Label string `json:"label"`
	}
	if !decodeJSON(w, r, &req, true) {
		return
	}
	if len(req.Label) > 64 {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	ch, err := s.cfg.Passkeys.BeginRegistration(passkey.RegistrationRequest{
		AccountID: c.subject.ID, UserName: c.subject.ID, Label: req.Label, Binding: sessionBinding(c.session),
	})
	if err != nil {
		s.writeCeremonyStartError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *server) handlePasskeyFinish(w http.ResponseWriter, r *http.Request, c *caller) {
	var req finishRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	reason := reasonSelfService
	if c.restricted {
		reason = reasonRecoveryEnrolls
	}
	const change = "register passkey"
	if !s.auditAttempt(w, c.subject.ID, authz.ActionUpdate, reason, change) {
		return
	}
	info, err := s.cfg.Passkeys.FinishRegistration(req.CeremonyID, sessionBinding(c.session), req.Credential)
	if err != nil {
		s.log.Info("api: passkey registration failed", "account", c.subject.ID, "err", err)
		s.auditResult(c.subject.ID, authz.ActionUpdate, reason, change, err)
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	s.auditResult(c.subject.ID, authz.ActionUpdate, reason, change, nil)
	// The credential change advanced the account epoch, which ended every
	// session of the account, including this one. Issue a fresh full
	// session; for a recovery session this completes the recovery.
	if !s.startSession(w, r, c.subject.ID) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"passkey": info})
}

// --- recovery ---

func (s *server) handleRecoveryCode(w http.ResponseWriter, r *http.Request, _ *caller) {
	var req struct {
		AccountID string `json:"accountId"`
		Code      string `json:"code"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	s.redeem(w, r, req.AccountID, "redeem recovery code", func() error {
		return s.cfg.Codes.Verify(req.AccountID, req.Code)
	})
}

func (s *server) handleRecoveryToken(w http.ResponseWriter, r *http.Request, _ *caller) {
	var req struct {
		AccountID string `json:"accountId"`
		Token     string `json:"token"`
	}
	if !decodeJSON(w, r, &req, false) {
		return
	}
	s.redeem(w, r, req.AccountID, "redeem recovery URL token", func() error {
		return s.cfg.URLTokens.Redeem(req.AccountID, req.Token)
	})
}

// redeem verifies a recovery credential under the IP+account limiter and,
// on success, removes the account's passkeys (the user lost them), which
// ends every session, and issues a recovery session that only allows
// registering a new passkey. Every failure looks the same to the client.
func (s *server) redeem(w http.ResponseWriter, r *http.Request, accountID, change string, verify func() error) {
	ip := clientIP(r)
	if !validAccountID(accountID) {
		s.log.Info("api: recovery with malformed account ID", "remote", ip)
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return
	}
	key := "recovery|" + ip + "|" + accountID
	if err := s.cfg.Limiter.Begin(key); err != nil {
		s.writeLocked(w, r, err)
		return
	}
	if err := verify(); err != nil {
		s.log.Info("api: recovery failed", "account", accountID, "remote", ip, "err", err)
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return
	}
	s.cfg.Limiter.Succeed(key)
	// The credential is consumed at this point; if the audit record cannot
	// be stored the recovery is still refused.
	if !s.auditAttempt(w, accountID, authz.ActionUpdate, reasonRecovery, change) {
		return
	}
	err := s.cfg.Accounts.BeginPasskeyReenrollment(accountID)
	s.auditResult(accountID, authz.ActionUpdate, reasonRecovery, change, err)
	if err != nil {
		s.log.Error("api: begin passkey re-enrolment", "account", accountID, "err", err)
		writeError(w, http.StatusUnauthorized, errAuthFailed)
		return
	}
	s.cfg.Sessions.RevokeUser(accountID)
	token, _, err := s.cfg.RecoverySessions.Issue(accountID)
	if err != nil {
		s.log.Error("api: issue recovery session", "account", accountID, "err", err)
		writeError(w, http.StatusInternalServerError, errInternal)
		return
	}
	s.clearCookie(w, s.cookies.session)
	s.setCookie(w, s.cookies.recovery, token, 0)
	writeJSON(w, http.StatusOK, map[string]bool{"pendingPasskeyEnrollment": true})
}

func (s *server) writeLocked(w http.ResponseWriter, r *http.Request, err error) {
	var le *recovery.LockedError
	if errors.As(err, &le) {
		if d := le.Until.Sub(s.now()); d > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((d+time.Second-1)/time.Second)))
		}
	}
	s.log.Warn("api: attempt limit reached", "path", r.URL.Path, "remote", clientIP(r))
	writeError(w, http.StatusTooManyRequests, errRateLimited)
}
