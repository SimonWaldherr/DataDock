package main

import (
	"fmt"
	"net"
	"strings"
)

// AuthMode selects how DataDock gates access to itself. It is independent of
// per-connection database credentials: even in AuthModeNone, individual
// managed database connections still have their own DSNs/passwords, and
// maintenance mode (requireWritable) still applies.
type AuthMode string

const (
	// AuthModeLocal is the default: a single bcrypt-hashed Admin password
	// gates /admin and every write-capable route wrapped in requireAdmin.
	// This is the right default for anything reachable beyond localhost.
	AuthModeLocal AuthMode = "local"

	// AuthModeNone disables the Admin login entirely: every session is
	// implicitly an Admin. It exists for the single-user/local case (a
	// developer running DataDock on their own machine, or a future desktop
	// packaging shell) where a login screen protects nothing, because
	// whoever can reach the process already has full access to the
	// machine. It only binds to a loopback address by default; reaching it
	// from another machine requires -allow-insecure-remote (see
	// resolveListenAddr and the auth-mode bind check in main.go).
	AuthModeNone AuthMode = "none"

	// AuthModeTrustedHeader and AuthModeOIDC are reserved identifiers for
	// the next two auth tiers (identity from a trusted reverse-proxy/SSO
	// header, and a full OIDC/SAML flow). Neither is implemented yet;
	// normalizeAuthMode rejects them today with a clear "not implemented
	// yet" error instead of silently accepting a mode that does nothing,
	// so configs written now won't quietly misbehave once support lands.
	AuthModeTrustedHeader AuthMode = "trusted-header"
	AuthModeOIDC          AuthMode = "oidc"
)

// normalizeAuthMode validates a raw auth-mode string (from a flag, env var,
// or a stored setting), defaulting a blank value to AuthModeLocal so
// upgrading DataDock without ever touching -auth-mode keeps today's
// password-gated behavior unchanged.
func normalizeAuthMode(raw string) (AuthMode, error) {
	mode := AuthMode(strings.ToLower(strings.TrimSpace(raw)))
	switch mode {
	case "":
		return AuthModeLocal, nil
	case AuthModeLocal, AuthModeNone:
		return mode, nil
	case AuthModeTrustedHeader, AuthModeOIDC:
		return "", fmt.Errorf("auth-mode %q is not implemented yet", mode)
	default:
		return "", fmt.Errorf("unknown auth-mode %q (expected %q or %q)", raw, AuthModeLocal, AuthModeNone)
	}
}

// isLoopbackAddr reports whether addr — a "host:port" listen address, or
// ":port" for "all interfaces" — resolves to a loopback-only bind target.
// An empty host and "0.0.0.0"/"::" are treated as non-loopback: those are
// exactly the "reachable from other machines" cases AuthModeNone must not
// default into, or silently accept, without an explicit override.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
