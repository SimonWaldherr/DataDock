package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// adminAuthTTL bounds how long an Admin login lasts before requiring the
// password again, independent of the long-lived session cookie itself.
const adminAuthTTL = 12 * time.Hour

func (a *App) isAdminAuthenticated(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	a.adminAuthMu.Lock()
	defer a.adminAuthMu.Unlock()
	expiry, ok := a.adminAuthedSessions[sessionID]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(a.adminAuthedSessions, sessionID)
		return false
	}
	return true
}

func (a *App) markAdminAuthenticated(sessionID string) {
	if sessionID == "" {
		return
	}
	a.adminAuthMu.Lock()
	defer a.adminAuthMu.Unlock()
	if a.adminAuthedSessions == nil {
		a.adminAuthedSessions = make(map[string]time.Time)
	}
	a.adminAuthedSessions[sessionID] = time.Now().Add(adminAuthTTL)
}

func (a *App) clearAdminAuthenticated(sessionID string) {
	a.adminAuthMu.Lock()
	defer a.adminAuthMu.Unlock()
	delete(a.adminAuthedSessions, sessionID)
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

// requireAdmin gates a handler behind the Admin password: with no password
// set yet, it sends the visitor to set one up; otherwise it requires the
// session to have already logged in via /admin/login. API requests get a
// JSON problem response instead of a redirect, since a browser redirect
// makes no sense for a fetch() call.
func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := sessionIDFromContext(r.Context())
		if !a.adminPasswordIsSet() {
			if isAPIRequest(r) {
				a.writeProblem(w, r, http.StatusPreconditionRequired, "Admin password not set", "set an admin password first at /admin/setup")
				return
			}
			http.Redirect(w, r, "/admin/setup?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
			return
		}
		if !a.isAdminAuthenticated(sessionID) {
			if isAPIRequest(r) {
				a.writeProblem(w, r, http.StatusUnauthorized, "Admin login required", "log in at /admin/login first")
				return
			}
			http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
			return
		}
		next(w, r)
	}
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

func (a *App) adminSetupHandler(w http.ResponseWriter, r *http.Request) {
	if a.adminPasswordIsSet() {
		http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(sanitizeAdminNextPath(r.URL.Query().Get("next"))), http.StatusSeeOther)
		return
	}
	a.renderAdminAuth(w, "setup", r.URL.Query().Get("next"), "")
}

func (a *App) adminSetupSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if a.adminPasswordIsSet() {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := sanitizeAdminNextPath(r.Form.Get("next"))
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
	a.markAdminAuthenticated(sessionIDFromContext(r.Context()))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) adminLoginHandler(w http.ResponseWriter, r *http.Request) {
	if !a.adminPasswordIsSet() {
		http.Redirect(w, r, "/admin/setup?next="+url.QueryEscape(sanitizeAdminNextPath(r.URL.Query().Get("next"))), http.StatusSeeOther)
		return
	}
	if a.isAdminAuthenticated(sessionIDFromContext(r.Context())) {
		http.Redirect(w, r, sanitizeAdminNextPath(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	a.renderAdminAuth(w, "login", r.URL.Query().Get("next"), "")
}

func (a *App) adminLoginSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	next := sanitizeAdminNextPath(r.Form.Get("next"))
	if !verifyAdminPassword(a.currentAdminPasswordHash(), r.Form.Get("password")) {
		a.renderAdminAuth(w, "login", next, "Incorrect password.")
		return
	}
	a.markAdminAuthenticated(sessionIDFromContext(r.Context()))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) adminLogoutHandler(w http.ResponseWriter, r *http.Request) {
	a.clearAdminAuthenticated(sessionIDFromContext(r.Context()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// adminToggleMaintenanceHandler flips maintenance (read-only) mode on or
// off. It's a separate, minimal endpoint — starting from
// a.currentRuntimeSettings() and only touching ReadOnlyMode — rather than
// reusing the big settings form, for two reasons: it's always reachable
// (never gated by requireWritable, so turning maintenance mode on can never
// lock an admin out of turning it back off), and it can't accidentally
// reset any other setting the way a form missing a field would.
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
	if !verifyAdminPassword(a.currentAdminPasswordHash(), r.Form.Get("current_password")) {
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
	data["Success"] = "Admin password changed."
	a.render(w, r, "admin", data)
}

// adminPersistConnectionHandler is behind requireAdmin: it's the only way a
// managed connection's credentials get written to the server's shared,
// persistent settings (visible to every session, surviving restarts).
// Adding a connection normally only keeps it in memory for the running
// process; this is the explicit, admin-only opt-in for making it permanent.
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
// connection from the persistent settings AND closes/drops it from the
// running process, so "forget" is a real delete, not just "stop saving it".
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
	http.Redirect(w, r, "/connections", http.StatusSeeOther)
}

// adminSetDefaultConnectionHandler is behind requireAdmin: it changes which
// connection every session falls back to when it hasn't picked its own
// active connection. This is deliberately separate from the unprivileged
// "Use" action (setActiveConnectionHandler), which only ever affects the
// requester's own session — making the *default* affects every concurrent
// user, so it needs the same admin gate as persisting/sharing a connection.
// ConnectionManager.SetDefault additionally refuses a still-private
// connection, so an admin must persist/share one before it can become the
// default for everyone.
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
