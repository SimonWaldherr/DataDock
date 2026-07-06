package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

const adminAuthRealm = "DataDock Admin"

type AdminAuthConfig struct {
	Username string
	Password string
}

func newGeneratedAdminPassword() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (a *App) setAdminAuth(cfg AdminAuthConfig) {
	cfg.Username = strings.TrimSpace(cfg.Username)
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	a.settingsMu.Lock()
	a.adminAuth = cfg
	a.settingsMu.Unlock()
}

func (a *App) adminAuthEnabled() bool {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.adminAuth.Password != ""
}

func (a *App) adminAuthUsername() string {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	if a.adminAuth.Username == "" {
		return "admin"
	}
	return a.adminAuth.Username
}

func (a *App) requireAdmin(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.settingsMu.RLock()
		cfg := a.adminAuth
		a.settingsMu.RUnlock()
		if cfg.Password == "" {
			handler(w, r)
			return
		}
		if cfg.Username == "" {
			cfg.Username = "admin"
		}
		user, pass, ok := r.BasicAuth()
		if !ok || !constantTimeStringEqual(user, cfg.Username) || !constantTimeStringEqual(pass, cfg.Password) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q, charset="UTF-8"`, adminAuthRealm))
			http.Error(w, "admin authentication required", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}
}

func constantTimeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
