package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

const sessionCookieName = "datadock_session"

type sessionIDContextKey struct{}
type activeConnectionContextKey struct{}

func (a *App) withSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := sessionIDFromRequest(r)
		if sessionID == "" {
			sessionID = newSessionID()
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookieName,
				Value:    sessionID,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Expires:  time.Now().Add(30 * 24 * time.Hour),
			})
		}

		ctx := context.WithValue(r.Context(), sessionIDContextKey{}, sessionID)
		if a.conns != nil {
			ctx = context.WithValue(ctx, activeConnectionContextKey{}, a.conns.ActiveFor(sessionID))
		}
		next(w, r.WithContext(ctx))
	}
}

// requireWritable blocks a mutating handler while the admin has enabled
// maintenance (read-only) mode, or while the session is logged in with
// RoleReadOnly, returning a clear error instead of silently letting writes
// through or silently discarding them. Since it already wraps every record/
// table/import/migration/admin-connection mutating route, it doubles as the
// single choke point for a minimal write audit trail: no per-handler
// instrumentation needed to know who hit a write route and what status it
// ended with.
func (a *App) requireWritable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.currentReadOnlyMode() {
			a.writeMaintenanceBlocked(w, r)
			return
		}
		sessionID := sessionIDFromContext(r.Context())
		username, role, ok := a.currentSessionUser(sessionID)
		if a.currentAuthMode() != AuthModeNone && ok && role == RoleReadOnly {
			a.writeReadOnlyRoleBlocked(w, r)
			return
		}
		audit := a.auditLogger()
		if !audit.Enabled() {
			next(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		audit.Log(AuditEvent{
			Session:  sessionID,
			Username: username,
			Method:   r.Method,
			Path:     r.URL.Path,
			Target:   r.PathValue("table"),
			Status:   rec.status,
		})
	}
}

func (a *App) writeMaintenanceBlocked(w http.ResponseWriter, r *http.Request) {
	const detail = "DataDock is in read-only maintenance mode. Write operations are disabled by an administrator until maintenance mode is turned off in Admin settings."
	if strings.HasPrefix(r.URL.Path, "/api/") {
		a.writeProblem(w, r, http.StatusServiceUnavailable, "Maintenance mode", detail)
		return
	}
	writeErrorPage(w, http.StatusServiceUnavailable, "Maintenance mode", detail,
		`<a href="/admin">Go to Admin settings</a> &middot; <a href="javascript:history.back()">Go back</a>`)
}

func (a *App) writeReadOnlyRoleBlocked(w http.ResponseWriter, r *http.Request) {
	const detail = "Your account has read-only access. Ask an administrator to change your role to make changes."
	if strings.HasPrefix(r.URL.Path, "/api/") {
		a.writeProblem(w, r, http.StatusForbidden, "Read-only role", detail)
		return
	}
	writeErrorPage(w, http.StatusForbidden, "Read-only account", detail, `<a href="javascript:history.back()">Go back</a>`)
}

// writeErrorPage renders DataDock's minimal, dependency-free HTML shell for
// a blocked/forbidden request (shared by writeMaintenanceBlocked,
// writeReadOnlyRoleBlocked, and writeForbiddenPage): the visitor being
// blocked may not be authorized to see anything a.render() would try to
// build (e.g. system-table listings), so this intentionally has no other
// dependencies. links is raw HTML for the page's own anchor(s).
func writeErrorPage(w http.ResponseWriter, status int, title, detail, links string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>` + title + `</title>` +
		`<body style="font-family:system-ui,sans-serif;padding:3rem 1.5rem;max-width:640px;margin:0 auto;color:#111827">` +
		`<h1 style="font-size:1.3rem">` + title + `</h1><p>` + detail + `</p>` +
		`<p>` + links + `</p></body>`))
}

func sessionIDFromRequest(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return sanitizeSessionID(cookie.Value)
}

func sessionIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(sessionIDContextKey{}).(string); ok {
		return sanitizeSessionID(id)
	}
	return ""
}

func activeConnectionFromContext(ctx context.Context) *DBConnection {
	if conn, ok := ctx.Value(activeConnectionContextKey{}).(*DBConnection); ok {
		return conn
	}
	return nil
}

func contextWithActiveConnection(ctx context.Context, conn *DBConnection) context.Context {
	return context.WithValue(ctx, activeConnectionContextKey{}, conn)
}

func sanitizeSessionID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 16 || len(s) > 128 {
		return ""
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return ""
		}
	}
	return s
}

func newSessionID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}
