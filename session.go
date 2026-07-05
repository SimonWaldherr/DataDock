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
