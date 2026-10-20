// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/authz"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
	"github.com/MagicalWig34653/db-cluster-manager/internal/recovery"
	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

// Config wires the full API to its stores. All fields except Logger, Now,
// AdministratorExists and RecoveryCodeIterations are required.
type Config struct {
	// Passkeys runs WebAuthn ceremonies. Its relying-party origins are also
	// the only origins accepted for state-changing requests.
	Passkeys *passkey.Manager
	// Accounts must be the registry Passkeys attaches credentials to and
	// the EpochSource of both session stores.
	Accounts *recovery.Accounts
	// Sessions holds full sessions.
	Sessions *recovery.Sessions
	// RecoverySessions holds short sessions issued after a recovery; they
	// only allow passkey re-enrolment. Use a short policy.
	RecoverySessions *recovery.Sessions
	// Codes verifies recovery codes; URLTokens redeems recovery URL tokens.
	Codes     *recovery.CodeVerifier
	URLTokens *recovery.URLTokens
	// Limiter limits recovery redemptions and setup-token guesses, keyed by
	// client IP and account.
	Limiter *recovery.Limiter
	// Authorizer evaluates permissions and writes audit records through
	// its AuditSink; AuditSink must be the same sink.
	Authorizer *authz.Authorizer
	AuditSink  authz.AuditSink
	// SetupToken protects first-run setup. Nil disables setup.
	SetupToken *SetupToken
	// AdministratorExists reports persisted administrators, if any store can
	// tell. Nil means only administrators created through this handler
	// count.
	AdministratorExists func() bool
	// Inventory and Activity back the resource endpoints.
	Inventory Inventory
	Activity  Activity
	// RecoveryCodeIterations is passed to recovery.GenerateCodes; 0 selects
	// the default.
	RecoveryCodeIterations int
	Logger                 *slog.Logger
	Now                    func() time.Time
}

func (c Config) validate() error {
	missing := func(name string) error { return fmt.Errorf("api: config field %s is required", name) }
	switch {
	case c.Passkeys == nil:
		return missing("Passkeys")
	case c.Accounts == nil:
		return missing("Accounts")
	case c.Sessions == nil:
		return missing("Sessions")
	case c.RecoverySessions == nil:
		return missing("RecoverySessions")
	case c.Codes == nil:
		return missing("Codes")
	case c.URLTokens == nil:
		return missing("URLTokens")
	case c.Limiter == nil:
		return missing("Limiter")
	case c.Authorizer == nil:
		return missing("Authorizer")
	case c.AuditSink == nil:
		return missing("AuditSink")
	case c.Inventory == nil:
		return missing("Inventory")
	case c.Activity == nil:
		return missing("Activity")
	}
	return nil
}

// access declares how an endpoint may be reached. The zero value is invalid,
// so an endpoint that forgets to declare access cannot be registered.
type access int

const (
	// accessPublic needs no session (health, setup, login, recovery).
	accessPublic access = iota + 1
	// accessSession needs a full session; used for self-service on the
	// caller's own account.
	accessSession
	// accessReenrol accepts a full session or a recovery session, which
	// exists only to register a new passkey.
	accessReenrol
	// accessFiltered needs a full session; the handler returns only items
	// the caller may read (authz.Filter).
	accessFiltered
	// accessPermission needs a full session and the declared action on the
	// resource returned by resolve.
	accessPermission
)

// endpoint is one route with its declared access.
type endpoint struct {
	method, path string
	access       access
	// prototype endpoints are also served by NewHandler.
	prototype bool
	// action and resolve declare the permission of accessPermission
	// endpoints. resolve returns the target resource and, optionally, the
	// loaded object, which the handler receives as caller.target.
	action  authz.Action
	resolve func(r *http.Request) (authz.Resource, any, error)
	// hideDenied answers a denial with 404, so object routes do not reveal
	// whether an object the caller may not see exists.
	hideDenied bool
	handle     func(w http.ResponseWriter, r *http.Request, c *caller)
}

// caller is the authenticated principal of a request.
type caller struct {
	subject authz.Subject
	session recovery.Session
	token   string
	// restricted is set for recovery sessions.
	restricted bool
	target     any
}

type server struct {
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	origins map[string]bool
	cookies cookieNames
	secure  bool

	setupMu      sync.Mutex
	setupDone    bool
	pendingSetup map[string]string // ceremony ID -> account ID
}

// endpoints is the route table. It is safe to call on a nil server: method
// values are only bound, not invoked.
func (s *server) endpoints() []endpoint {
	return []endpoint{
		{method: "GET", path: "/health", access: accessPublic, prototype: true, handle: handleHealth},
		{method: "GET", path: "/version", access: accessPublic, prototype: true, handle: handleVersion},

		{method: "GET", path: "/setup/status", access: accessPublic, handle: s.handleSetupStatus},
		{method: "POST", path: "/setup/begin", access: accessPublic, handle: s.handleSetupBegin},
		{method: "POST", path: "/setup/finish", access: accessPublic, handle: s.handleSetupFinish},

		{method: "POST", path: "/auth/login/begin", access: accessPublic, handle: s.handleLoginBegin},
		{method: "POST", path: "/auth/login/finish", access: accessPublic, handle: s.handleLoginFinish},
		{method: "POST", path: "/auth/logout", access: accessReenrol, handle: s.handleLogout},
		{method: "GET", path: "/auth/session", access: accessReenrol, handle: s.handleSession},
		{method: "POST", path: "/auth/passkeys/begin", access: accessReenrol, handle: s.handlePasskeyBegin},
		{method: "POST", path: "/auth/passkeys/finish", access: accessReenrol, handle: s.handlePasskeyFinish},
		{method: "POST", path: "/auth/recovery/code", access: accessPublic, handle: s.handleRecoveryCode},
		{method: "POST", path: "/auth/recovery/token", access: accessPublic, handle: s.handleRecoveryToken},

		{method: "GET", path: "/swarms", access: accessFiltered, handle: s.handleSwarms},
		{method: "GET", path: "/clusters", access: accessFiltered, handle: s.handleClusters},
		{
			method: "GET", path: "/clusters/{id}", access: accessPermission,
			action: authz.ActionRead, resolve: s.resolveCluster, hideDenied: true,
			handle: s.handleCluster,
		},
		{method: "GET", path: "/operations", access: accessFiltered, handle: s.handleOperations},
		{method: "GET", path: "/events", access: accessFiltered, handle: s.handleEvents},
	}
}

// New returns the full API handler, rooted at /api/v1.
func New(cfg Config) (http.Handler, error) {
	s, err := newServer(cfg)
	if err != nil {
		return nil, err
	}
	return s.mux(), nil
}

func newServer(cfg Config) (*server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s := &server{cfg: cfg, log: cfg.Logger, now: cfg.Now, pendingSetup: make(map[string]string)}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.origins = make(map[string]bool)
	s.secure = false
	for _, o := range cfg.Passkeys.Config().Origins {
		s.origins[strings.ToLower(o)] = true
		if !isLocalhostHTTP(o) {
			s.secure = true
		}
	}
	s.cookies = newCookieNames(s.secure)
	return s, nil
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, ep := range s.endpoints() {
		mux.Handle(ep.method+" "+Prefix+ep.path, s.wrap(ep))
	}
	mux.HandleFunc("/api/", notFound)
	return mux
}

// maxBodyBytes bounds request bodies; WebAuthn responses are a few KiB.
const maxBodyBytes = 64 << 10

// wrap applies the cross-cutting checks in a fixed order: origin check for
// state-changing methods, body limit, then the declared access.
func (s *server) wrap(ep endpoint) http.Handler {
	if ep.handle == nil {
		panic(fmt.Sprintf("api: endpoint %s %s has no handler", ep.method, ep.path))
	}
	switch ep.access {
	case accessPublic, accessSession, accessReenrol, accessFiltered:
	case accessPermission:
		if !ep.action.Valid() || ep.resolve == nil {
			panic(fmt.Sprintf("api: endpoint %s %s declares a permission without action or resource", ep.method, ep.path))
		}
	default:
		panic(fmt.Sprintf("api: endpoint %s %s declares no access", ep.method, ep.path))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safeMethod(r.Method) {
			if !s.originAllowed(r) {
				s.log.Warn("api: rejected cross-origin request", "method", r.Method, "path", r.URL.Path,
					"origin", r.Header.Get("Origin"), "secFetchSite", r.Header.Get("Sec-Fetch-Site"), "remote", clientIP(r))
				writeError(w, http.StatusForbidden, errOriginRejected)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		if ep.access == accessPublic {
			ep.handle(w, r, nil)
			return
		}
		c := s.authenticate(r)
		if c == nil {
			writeError(w, http.StatusUnauthorized, errUnauthenticated)
			return
		}
		if c.restricted && ep.access != accessReenrol {
			writeError(w, http.StatusForbidden, errEnrollmentNeeded)
			return
		}
		if ep.access == accessPermission {
			res, target, err := ep.resolve(r)
			if err != nil {
				s.writeServiceError(w, r, err)
				return
			}
			var d authz.Decision
			if ep.action.ChangesState() {
				// Authorize writes the audit record and fails closed.
				d = s.cfg.Authorizer.Authorize(c.subject, ep.action, res)
			} else {
				d = s.cfg.Authorizer.Check(c.subject, ep.action, res)
			}
			if !d.Allowed {
				switch {
				case d.Reason == authz.ReasonAuditUnavailable:
					s.log.Error("api: request denied because the audit record could not be stored",
						"actor", c.subject.ID, "action", ep.action, "resource", res.String())
					writeError(w, http.StatusServiceUnavailable, errUnavailable)
				case ep.hideDenied:
					writeError(w, http.StatusNotFound, errNotFound)
				default:
					writeError(w, http.StatusForbidden, errForbidden)
				}
				return
			}
			c.target = target
		}
		ep.handle(w, r, c)
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// originAllowed is the CSRF defence for state-changing requests.
//
// Choice: Origin / Sec-Fetch-Site verification instead of a double-submit
// token. The UI is a same-origin single-page app that calls the API with
// fetch; every browser that supports passkeys sends Origin on non-GET
// fetches, and the passkey layer already requires the exact UI origins in
// its configuration, so they are the natural allow-list. Unlike a token, this
// needs no server-side or cookie state, cannot leak through page content, and
// also covers login CSRF on the pre-authentication endpoints, where no
// session exists to bind a token to. SameSite=Strict cookies are a second,
// independent layer. The check is strict: a missing or "null" Origin is
// rejected, so non-browser clients must send the configured origin
// explicitly. Go's http.CrossOriginProtection was not used because it
// accepts requests without Origin and Sec-Fetch-Site and compares against
// Host rather than the passkey origins.
func (s *server) originAllowed(r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return false
	}
	return s.origins[strings.ToLower(o)]
}

func isLocalhostHTTP(origin string) bool {
	o := strings.ToLower(origin)
	return o == "http://localhost" || strings.HasPrefix(o, "http://localhost:")
}

// clientIP is the TCP peer address. Forwarded headers are not trusted; a
// reverse proxy deployment needs an explicit trusted-proxy setting first.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authenticate resolves the session cookie. A recovery session is returned
// (restricted) when no valid full session is presented; the caller decides
// whether the endpoint accepts it.
func (s *server) authenticate(r *http.Request) *caller {
	if ck, err := r.Cookie(s.cookies.session); err == nil && ck.Value != "" {
		if sess, err := s.cfg.Sessions.Validate(ck.Value); err == nil {
			return &caller{subject: userSubject(sess.UserID), session: sess, token: ck.Value}
		} else {
			s.log.Debug("api: session rejected", "err", err, "remote", clientIP(r))
		}
	}
	if ck, err := r.Cookie(s.cookies.recovery); err == nil && ck.Value != "" {
		if sess, err := s.cfg.RecoverySessions.Validate(ck.Value); err == nil {
			return &caller{subject: userSubject(sess.UserID), session: sess, token: ck.Value, restricted: true}
		} else {
			s.log.Debug("api: recovery session rejected", "err", err, "remote", clientIP(r))
		}
	}
	return nil
}

func userSubject(id string) authz.Subject {
	return authz.Subject{ID: id, Kind: authz.SubjectUser}
}

func handleHealth(w http.ResponseWriter, _ *http.Request, _ *caller) {
	writeJSON(w, http.StatusOK, Health{Status: "ok"})
}

func handleVersion(w http.ResponseWriter, _ *http.Request, _ *caller) {
	writeJSON(w, http.StatusOK, version.Get("manager"))
}

// writeServiceError maps resource-service errors without exposing details.
func (s *server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, errNotFound)
	default:
		s.log.Error("api: resource service failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, errUnavailable)
	}
}
