package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// sessionAuthTTL bounds how long a login lasts before requiring the
// password again, independent of the long-lived session cookie itself.
const sessionAuthTTL = 12 * time.Hour

// currentSessionUser looks up the authenticated identity behind a session
// cookie, if any. It's the general-purpose accessor requireRole,
// requireWritable, apiQueryHandler, and audit logging all use; ok is false
// for an empty, unknown, or expired session ID (which also deletes the
// expired entry, same as the old isAdminAuthenticated did).
func (a *App) currentSessionUser(sessionID string) (username string, role Role, ok bool) {
	if sessionID == "" {
		return "", "", false
	}
	a.adminAuthMu.Lock()
	defer a.adminAuthMu.Unlock()
	auth, found := a.adminAuthedSessions[sessionID]
	if !found {
		return "", "", false
	}
	if time.Now().After(auth.Expiry) {
		delete(a.adminAuthedSessions, sessionID)
		return "", "", false
	}
	return auth.Username, auth.Role, true
}

// isAdminAuthenticated reports whether the session is logged in as an
// admin. It keeps its historical name/signature since it's consumed
// directly by handlers.go and templates/connections.html's AdminAuthenticated
// field. AuthModeNone always reports true here: every request is implicitly
// an Admin request in that mode (matching requireRole's own bypass), so
// system-table visibility and admin-only UI affordances stay consistent
// with what routes actually allow.
func (a *App) isAdminAuthenticated(sessionID string) bool {
	if a.currentAuthMode() == AuthModeNone {
		return true
	}
	_, role, ok := a.currentSessionUser(sessionID)
	return ok && role == RoleAdmin
}

func (a *App) markSessionAuthenticated(sessionID, username string, role Role) {
	if sessionID == "" {
		return
	}
	a.adminAuthMu.Lock()
	defer a.adminAuthMu.Unlock()
	if a.adminAuthedSessions == nil {
		a.adminAuthedSessions = make(map[string]sessionAuth)
	}
	a.adminAuthedSessions[sessionID] = sessionAuth{
		Username: username,
		Role:     role,
		Expiry:   time.Now().Add(sessionAuthTTL),
	}
}

func (a *App) markAdminAuthenticated(sessionID string) {
	a.markSessionAuthenticated(sessionID, "admin", RoleAdmin)
}

func (a *App) clearSessionAuthenticated(sessionID string) {
	a.adminAuthMu.Lock()
	defer a.adminAuthMu.Unlock()
	delete(a.adminAuthedSessions, sessionID)
}

func (a *App) clearAdminAuthenticated(sessionID string) {
	a.clearSessionAuthenticated(sessionID)
}

// revokeSessionsForUsername logs out every currently-authenticated session
// for username, wherever it's logged in from. Disabling/deleting an account
// or changing its password only touched __datadock_users before this;
// currentSessionUser never re-checks the live Disabled flag or password
// hash against an already-authenticated session's in-memory entry, so an
// attacker with a live session on a just-disabled or just-offboarded
// account kept full access until that session's normal TTL expired
// (up to sessionAuthTTL) regardless of the account-level action.
func (a *App) revokeSessionsForUsername(username string) {
	a.revokeOtherSessionsForUsername(username, "")
}

// revokeOtherSessionsForUsername is revokeSessionsForUsername, but leaves
// exceptSessionID untouched — used when a user changes their own password,
// so the session making that very request doesn't get logged out along
// with every other (e.g. compromised) session for the account.
func (a *App) revokeOtherSessionsForUsername(username, exceptSessionID string) {
	if username == "" {
		return
	}
	a.adminAuthMu.Lock()
	defer a.adminAuthMu.Unlock()
	for sessionID, auth := range a.adminAuthedSessions {
		if auth.Username == username && sessionID != exceptSessionID {
			delete(a.adminAuthedSessions, sessionID)
		}
	}
}

func hashAdminPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func verifyAdminPassword(hash, plain string) bool {
	if strings.TrimSpace(hash) == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// dummyPasswordHash is a valid bcrypt hash of an arbitrary fixed password, at
// the same cost as hashAdminPassword. adminLoginSubmitHandler compares
// against it whenever the submitted username doesn't exist or is disabled,
// so every login attempt pays the same bcrypt cost and a response-time
// difference can't be used to enumerate usernames or disabled accounts.
var dummyPasswordHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte("datadock-constant-time-dummy"), bcrypt.DefaultCost)
	if err != nil {
		return ""
	}
	return string(h)
}()

// requireRole gates a handler behind first-run setup and a per-session login
// with one of the given roles. Browser routes redirect into the setup/login
// flow; API routes return Problem Details so automation clients can handle
// the missing-setup (428), missing-login (401), and wrong-role (403) states
// explicitly.
//
// In AuthModeNone there is no login concept at all: every request is
// implicitly an Admin request, so this passes straight through regardless
// of which roles were requested. main.go's startup bind check (and
// applyRuntimeSettings' runtime check) are what keep that safe — they
// refuse AuthModeNone on anything but a loopback address unless the
// operator explicitly opts in with -allow-insecure-remote.
func (a *App) requireRole(allowed ...Role) func(http.HandlerFunc) http.HandlerFunc {
	allowedSet := make(map[Role]bool, len(allowed))
	for _, role := range allowed {
		allowedSet[role] = true
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if a.currentAuthMode() == AuthModeNone {
				next(w, r)
				return
			}
			sessionID := sessionIDFromContext(r.Context())
			configured, err := a.authConfigured(r.Context())
			if err != nil {
				a.serverError(w, err)
				return
			}
			if !configured {
				if isAPIRequest(r) {
					a.writeProblem(w, r, http.StatusPreconditionRequired, "No admin account yet", "create the first Admin account first at /admin/setup")
					return
				}
				http.Redirect(w, r, "/admin/setup?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
				return
			}
			_, role, ok := a.currentSessionUser(sessionID)
			if !ok {
				if isAPIRequest(r) {
					a.writeProblem(w, r, http.StatusUnauthorized, "Login required", "log in at /admin/login first")
					return
				}
				http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
				return
			}
			if !allowedSet[role] {
				if isAPIRequest(r) {
					a.writeProblem(w, r, http.StatusForbidden, "Insufficient role", "your account's role does not allow this action")
					return
				}
				a.writeForbiddenPage(w, r, "Your account does not have permission to view this page.")
				return
			}
			next(w, r)
		}
	}
}

// requireLogin is requireRole with every role allowed: it exists purely to
// require *some* authenticated session (any role) rather than gating on a
// specific one. Composed with requireWritable — e.g.
// a.requireLogin(a.requireWritable(handler)) — on every mutating route that
// isn't already requireAdmin-gated: requireWritable by itself only blocks
// maintenance mode and RoleReadOnly, it was never an authentication check,
// so in AuthModeLocal (the default for anything reachable beyond
// localhost) an anonymous visitor could still reach every one of those
// routes with zero credentials. AuthModeNone is unaffected, matching its
// existing "whoever can reach the process already has full access to the
// machine" model.
func (a *App) requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return a.requireRole(RoleAdmin, RoleUser, RoleReadOnly)(next)
}

// loginRequiredForWrite is requireLogin's inline-branch equivalent, for the
// handful of routes that gate a write inline instead of at the route table
// (POST /match's save/save_config branches, and /match/tables' upload
// branch) rather than via a handler-wrapping middleware.
func (a *App) loginRequiredForWrite(ctx context.Context) bool {
	if a.currentAuthMode() == AuthModeNone {
		return false
	}
	_, _, ok := a.currentSessionUser(sessionIDFromContext(ctx))
	return !ok
}

// requireAdmin is the common case of requireRole: only RoleAdmin may pass.
// Kept as its own name because it's used at every admin-only route
// registration and reads more clearly there than requireRole(RoleAdmin).
func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return a.requireRole(RoleAdmin)(next)
}

// writeForbiddenPage is requireRole's browser-facing equivalent of
// writeMaintenanceBlocked (session.go): a plain, dependency-free page, since
// the visitor may not be authorized to see anything a.render() would try to
// build (e.g. system-table listings).
func (a *App) writeForbiddenPage(w http.ResponseWriter, r *http.Request, detail string) {
	writeErrorPage(w, http.StatusForbidden, "Forbidden", detail, `<a href="/">Go back</a>`)
}

func isAPIRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/")
}

// sanitizeAdminNextPath keeps the post-login redirect target inside the app
// (never an absolute/external URL), falling back to /admin.
func sanitizeAdminNextPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/admin"
	}
	return raw
}

// isValidUsername keeps usernames simple to type, display in badges/titles,
// and compare case-insensitively: 3-64 characters, ASCII letters/digits and
// . _ - only. Not a security boundary by itself (html/template still
// auto-escapes on render either way) — just keeps the account list tidy.
func isValidUsername(username string) bool {
	if len(username) < 3 || len(username) > 64 {
		return false
	}
	for _, r := range username {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// renderAdminAuth renders the standalone setup/login page. It intentionally
// doesn't go through a.render()/base_start: the visitor isn't authenticated
// yet, so there's no sidebar/connections context to show.
func (a *App) renderAdminAuth(w http.ResponseWriter, mode, next, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tpl.ExecuteTemplate(w, "admin_auth", map[string]interface{}{
		"Mode":  mode,
		"Next":  next,
		"Error": errMsg,
	}); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

// adminSetupHandler shows the first-run flow. When nobody has set anything
// up yet AND -auth-mode/$DATADOCK_AUTH_MODE was never explicitly given for
// this process, it shows a "Nur ich / Team" mode chooser first (Schritt 4)
// instead of jumping straight to account creation — an operator who already
// told DataDock which mode to use via flag/env skips straight to "setup"
// (today's behavior, unchanged), and ?team=1 (from the chooser's "Team"
// link, or a direct visit) also skips it for this one request.
func (a *App) adminSetupHandler(w http.ResponseWriter, r *http.Request) {
	if a.currentAuthMode() == AuthModeNone {
		http.Redirect(w, r, sanitizeAdminNextPath(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	configured, err := a.authConfigured(r.Context())
	if err != nil {
		a.serverError(w, err)
		return
	}
	if configured {
		http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(sanitizeAdminNextPath(r.URL.Query().Get("next"))), http.StatusSeeOther)
		return
	}
	if !a.authModeExplicit && r.URL.Query().Get("team") == "" {
		a.renderAdminAuth(w, "choose", r.URL.Query().Get("next"), "")
		return
	}
	a.renderAdminAuth(w, "setup", r.URL.Query().Get("next"), "")
}

// adminSetupModeHandler handles the chooser's "Nur ich (kein Login)" button.
// It deliberately reuses applyRuntimeSettings/saveRuntimeSettings unchanged
// instead of duplicating their loopback-bind safety check: if the server is
// reachable beyond localhost without -allow-insecure-remote, the same error
// that would stop a -auth-mode=none startup stops this switch too, surfaced
// back on the chooser page instead of silently doing nothing.
func (a *App) adminSetupModeHandler(w http.ResponseWriter, r *http.Request) {
	if a.currentAuthMode() == AuthModeNone {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	configured, err := a.authConfigured(r.Context())
	if err != nil {
		a.serverError(w, err)
		return
	}
	if configured {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := sanitizeAdminNextPath(r.Form.Get("next"))
	if r.Form.Get("mode") != string(AuthModeNone) {
		http.Error(w, "unsupported mode", http.StatusBadRequest)
		return
	}
	settings := a.currentRuntimeSettings()
	settings.AuthMode = string(AuthModeNone)
	if err := a.applyRuntimeSettings(settings); err != nil {
		a.renderAdminAuth(w, "choose", next, err.Error())
		return
	}
	if err := a.saveRuntimeSettings(r.Context()); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *App) adminSetupSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if a.currentAuthMode() == AuthModeNone {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	configured, err := a.authConfigured(r.Context())
	if err != nil {
		a.serverError(w, err)
		return
	}
	if configured {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// A failed submit always re-renders "setup" directly, never bounces
	// back to "choose": reaching this handler at all means the visitor
	// already committed to creating an account.
	next := sanitizeAdminNextPath(r.Form.Get("next"))
	username := strings.TrimSpace(r.Form.Get("username"))
	if username == "" {
		username = "admin"
	}
	if !isValidUsername(username) {
		a.renderAdminAuth(w, "setup", next, "Username must be 3-64 characters: letters, digits, dot, underscore, or hyphen.")
		return
	}
	password := r.Form.Get("password")
	if len(password) < 8 {
		a.renderAdminAuth(w, "setup", next, "Password must be at least 8 characters.")
		return
	}
	if password != r.Form.Get("password_confirm") {
		a.renderAdminAuth(w, "setup", next, "Passwords do not match.")
		return
	}
	hash, err := hashAdminPassword(password)
	if err != nil {
		a.serverError(w, err)
		return
	}
	settings := a.currentRuntimeSettings()
	settings.AdminPasswordHash = hash
	if err := a.applyRuntimeSettings(settings); err != nil {
		a.serverError(w, err)
		return
	}
	if err := a.saveRuntimeSettings(r.Context()); err != nil {
		a.serverError(w, err)
		return
	}
	// The first account created is always an admin — there's no one else
	// yet to have assigned it a different role.
	if err := a.createUser(r.Context(), username, hash, RoleAdmin); err != nil {
		if errors.Is(err, ErrUserExists) {
			a.renderAdminAuth(w, "setup", next, "That username is taken.")
			return
		}
		a.serverError(w, err)
		return
	}
	a.rotateSessionOnAuth(w, r, username, RoleAdmin)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) adminLoginHandler(w http.ResponseWriter, r *http.Request) {
	if a.currentAuthMode() == AuthModeNone {
		http.Redirect(w, r, sanitizeAdminNextPath(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	configured, err := a.authConfigured(r.Context())
	if err != nil {
		a.serverError(w, err)
		return
	}
	if !configured && !a.adminPasswordIsSet() {
		http.Redirect(w, r, "/admin/setup?next="+url.QueryEscape(sanitizeAdminNextPath(r.URL.Query().Get("next"))), http.StatusSeeOther)
		return
	}
	if a.isAdminAuthenticated(sessionIDFromContext(r.Context())) {
		http.Redirect(w, r, sanitizeAdminNextPath(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	a.renderAdminAuth(w, "login", r.URL.Query().Get("next"), "")
}

// loginAttemptState tracks recent failed login attempts for one username.
type loginAttemptState struct {
	failures    int
	lockedUntil time.Time
}

const (
	// maxLoginFailuresBeforeLockout is how many consecutive failed
	// attempts for a username are allowed before it's locked out.
	maxLoginFailuresBeforeLockout = 5
	// loginLockoutDuration is how long a username stays locked out once
	// maxLoginFailuresBeforeLockout is reached.
	loginLockoutDuration = 15 * time.Minute
)

// loginLockedOut reports whether username is currently locked out from
// login attempts, and clears any expired lockout it finds along the way.
func (a *App) loginLockedOut(username string) bool {
	a.loginAttemptsMu.Lock()
	defer a.loginAttemptsMu.Unlock()
	state := a.loginAttempts[username]
	if state == nil {
		return false
	}
	if state.lockedUntil.IsZero() {
		return false
	}
	if time.Now().After(state.lockedUntil) {
		delete(a.loginAttempts, username)
		return false
	}
	return true
}

// recordLoginFailure counts one more failed attempt for username, locking
// it out once maxLoginFailuresBeforeLockout is reached. Deliberately keyed
// by username, not client IP: DataDock has no reliable client IP behind an
// arbitrary reverse proxy without trusting a spoofable header, and the
// actual attack this defends is unlimited parallel guessing against one
// known account (typically "admin"), which per-username lockout already
// stops regardless of how many source addresses it's spread across.
func (a *App) recordLoginFailure(username string) {
	a.loginAttemptsMu.Lock()
	defer a.loginAttemptsMu.Unlock()
	if a.loginAttempts == nil {
		a.loginAttempts = make(map[string]*loginAttemptState)
	}
	state := a.loginAttempts[username]
	if state == nil {
		state = &loginAttemptState{}
		a.loginAttempts[username] = state
	}
	state.failures++
	if state.failures >= maxLoginFailuresBeforeLockout {
		state.lockedUntil = time.Now().Add(loginLockoutDuration)
	}
}

// clearLoginFailures resets username's failure count after a successful
// login.
func (a *App) clearLoginFailures(username string) {
	a.loginAttemptsMu.Lock()
	defer a.loginAttemptsMu.Unlock()
	delete(a.loginAttempts, username)
}

func (a *App) adminLoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if a.currentAuthMode() == AuthModeNone {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := sanitizeAdminNextPath(r.Form.Get("next"))
	username := strings.TrimSpace(r.Form.Get("username"))
	password := r.Form.Get("password")
	if username == "" {
		username = "admin"
	}
	if a.loginLockedOut(username) {
		a.renderAdminAuth(w, "login", next, "Too many failed attempts for this account. Try again later.")
		return
	}
	user, found, err := a.getUserByUsername(r.Context(), username)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if !found && username == "admin" && verifyAdminPassword(a.currentAdminPasswordHash(), password) {
		if hash := strings.TrimSpace(a.currentAdminPasswordHash()); hash != "" {
			if err := a.createUser(r.Context(), "admin", hash, RoleAdmin); err != nil && !errors.Is(err, ErrUserExists) {
				a.serverError(w, err)
				return
			}
			user = User{Username: "admin", PasswordHash: hash, Role: RoleAdmin}
			found = true
		}
	}
	// Always run a bcrypt comparison of equal cost, even when the account
	// doesn't exist or is disabled, so response time can't be used to
	// enumerate usernames or disabled accounts (see dummyPasswordHash).
	hashToCheck := user.PasswordHash
	if !found || user.Disabled {
		hashToCheck = dummyPasswordHash
	}
	passwordOK := verifyAdminPassword(hashToCheck, password)
	if !found || user.Disabled || !passwordOK {
		a.recordLoginFailure(username)
		a.renderAdminAuth(w, "login", next, "Incorrect password.")
		return
	}
	a.clearLoginFailures(username)
	a.rotateSessionOnAuth(w, r, user.Username, user.Role)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) adminLogoutHandler(w http.ResponseWriter, r *http.Request) {
	a.clearAdminAuthenticated(sessionIDFromContext(r.Context()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// adminToggleMaintenanceHandler is deliberately separate from the main settings
// form: it is reachable even while maintenance mode is on and only flips the
// ReadOnlyMode bit, so an incomplete settings form cannot change other values.
func (a *App) adminToggleMaintenanceHandler(w http.ResponseWriter, r *http.Request) {
	settings := a.currentRuntimeSettings()
	settings.ReadOnlyMode = !settings.ReadOnlyMode
	if err := a.applyRuntimeSettings(settings); err != nil {
		a.serverError(w, err)
		return
	}
	if err := a.saveRuntimeSettings(r.Context()); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// adminChangePasswordHandler is itself behind requireAdmin, and additionally
// re-checks the current password before accepting a new one.
func (a *App) adminChangePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	data := a.adminPageData(r.Context(), nil)
	fail := func(msg string) {
		data["Error"] = msg
		a.render(w, r, "admin", data)
	}
	username, _, hasSessionUser := a.currentSessionUser(sessionIDFromContext(r.Context()))
	currentHash := strings.TrimSpace(a.currentAdminPasswordHash())
	if hasSessionUser {
		user, found, err := a.getUserByUsername(r.Context(), username)
		if err != nil {
			a.serverError(w, err)
			return
		}
		if found {
			currentHash = user.PasswordHash
		}
	}
	if !verifyAdminPassword(currentHash, r.Form.Get("current_password")) {
		fail("Current password is incorrect.")
		return
	}
	newPassword := r.Form.Get("new_password")
	if len(newPassword) < 8 {
		fail("New password must be at least 8 characters.")
		return
	}
	if newPassword != r.Form.Get("new_password_confirm") {
		fail("New passwords do not match.")
		return
	}
	hash, err := hashAdminPassword(newPassword)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if hasSessionUser {
		if err := a.setUserPasswordHash(r.Context(), username, hash); err != nil {
			a.serverError(w, err)
			return
		}
	}
	settings := a.currentRuntimeSettings()
	settings.AdminPasswordHash = hash
	if err := a.applyRuntimeSettings(settings); err != nil {
		a.serverError(w, err)
		return
	}
	if err := a.saveRuntimeSettings(r.Context()); err != nil {
		a.serverError(w, err)
		return
	}
	// Revoke every OTHER already-authenticated session for this account (not
	// the one making this request) — otherwise a session an admin is trying
	// to lock out by changing the password keeps working until its normal
	// sessionAuthTTL expiry.
	if hasSessionUser {
		a.revokeOtherSessionsForUsername(username, sessionIDFromContext(r.Context()))
	}
	data["Success"] = "Password changed."
	a.render(w, r, "admin", data)
}

// adminPersistConnectionHandler is behind requireAdmin: it's the only way a
// managed connection's credentials get written to the server's shared,
// persistent settings.
func (a *App) adminPersistConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("id")
	if a.conns.Get(id) == nil {
		a.render(w, r, "connections", map[string]interface{}{"Error": fmt.Sprintf("connection %q not found", id)})
		return
	}
	a.conns.MarkPersisted(id)
	if err := a.saveManagedConnections(r.Context()); err != nil {
		a.conns.UnmarkPersisted(id)
		a.render(w, r, "connections", map[string]interface{}{"Error": "Could not save connection: " + err.Error()})
		return
	}
	http.Redirect(w, r, "/connections", http.StatusSeeOther)
}

// adminForgetConnectionHandler is behind requireAdmin: it removes a
// connection from the persistent settings and closes/drops it from the
// running process.
func (a *App) adminForgetConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("id")
	a.conns.UnmarkPersisted(id)
	if err := a.saveManagedConnections(r.Context()); err != nil {
		a.render(w, r, "connections", map[string]interface{}{"Error": "Could not update saved connections: " + err.Error()})
		return
	}
	if err := a.conns.Remove(id); err != nil {
		a.render(w, r, "connections", map[string]interface{}{"Error": err.Error()})
		return
	}
	// Best-effort: a removed connection's ID can later be reused for an
	// unrelated database, so its indexed SQL-logic vectors (see
	// logic_search.go) must not survive it — but a cleanup failure here
	// shouldn't block the connection removal that already succeeded.
	_ = a.deleteObjectEmbeddingsForConnection(r.Context(), id)
	http.Redirect(w, r, "/connections", http.StatusSeeOther)
}

// adminSetDefaultConnectionHandler is behind requireAdmin because changing
// the default connection affects every session that has not picked its own.
func (a *App) adminSetDefaultConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("id")
	if err := a.conns.SetDefault(id); err != nil {
		a.render(w, r, "connections", map[string]interface{}{"Error": err.Error()})
		return
	}
	if err := a.saveManagedConnections(r.Context()); err != nil {
		a.render(w, r, "connections", map[string]interface{}{"Error": "Default changed but could not be saved: " + err.Error()})
		return
	}
	http.Redirect(w, r, "/connections", http.StatusSeeOther)
}
