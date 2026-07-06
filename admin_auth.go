package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

const adminAuthRealm = "DataDock Admin"

type AdminAuthConfig struct {
	Username     string
	Password     string
	usernameHash [32]byte
	passwordHash [32]byte
	enabled      bool
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
	cfg.enabled = cfg.Password != ""
	if cfg.enabled {
		cfg.usernameHash = sha256.Sum256([]byte(cfg.Username))
		cfg.passwordHash = sha256.Sum256([]byte(cfg.Password))
		cfg.Password = ""
	}
	a.settingsMu.Lock()
	a.adminAuth = cfg
	a.settingsMu.Unlock()
}

func (a *App) adminAuthEnabled() bool {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.adminAuth.enabled
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
		if !cfg.enabled {
			handler(w, r)
			return
		}
		if cfg.Username == "" {
			cfg.Username = "admin"
		}
		user, pass, ok := r.BasicAuth()
		if !ok || !constantTimeCredentialEqual(user, cfg.usernameHash) || !constantTimeCredentialEqual(pass, cfg.passwordHash) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q, charset="UTF-8"`, adminAuthRealm))
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "admin authentication required", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}
}

func constantTimeCredentialEqual(value string, expected [32]byte) bool {
	sum := sha256.Sum256([]byte(value))
	return subtle.ConstantTimeCompare(sum[:], expected[:]) == 1
}
