// Package main: native_rbac.go mirrors DataDock's own user accounts
// (users.go) into tinySQL's built-in RBAC system (CREATE USER/CREATE ROLE/
// GRANT, exposed via (*tinysql.DB).Catalog()) as a defense-in-depth second
// opinion — never as DataDock's primary access control, which stays exactly
// as it was (session.go/admin_auth.go).
//
// The critical safety property, established by reading tinySQL's source
// directly: CREATE USER opts a *whole process* into RBAC enforcement inside
// tinysql.Execute — every native execution without an acting user set via
// tinysql.WithUser would then fail closed with "access denied", and
// DataDock's many native execution call sites (jobs, pipelines, imports,
// migrations, matching, scheduled tasks, ...) don't set one. Wiring all of
// those correctly is out of scope here (and would be better done as part of
// the Phase-1 keystone refactor, which has a natural seam for it).
//
// Instead, ensureNativeRBACSafetyNet forces (*tinysql.DB).SetRBACEnabled(false)
// unconditionally at startup — this permanently disables Execute's automatic
// enforcement hook regardless of how many users exist, so nothing anywhere
// in the app can ever break from this feature. CreateUser/GrantPermission/
// HasPermission are still fully usable as an explicit, opt-in authorization
// *data model and decision function* that DataDock calls directly wherever
// it chooses (see sessionCanWriteSQL in handlers.go) — never through
// Execute's automatic path.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

const (
	// nativeRoleWrite mirrors RoleAdmin/RoleUser: sessionCanWriteSQL treats
	// both as write-capable today (the only real distinction between them is
	// admin-only pages/settings, not SQL permissions), so the native mirror
	// matches that exact binary split rather than inventing a finer-grained
	// policy tinySQL's own grant model doesn't need.
	nativeRoleWrite = "datadock_write"
	// nativeRoleReadOnly mirrors RoleReadOnly.
	nativeRoleReadOnly = "datadock_readonly"
)

// ensureNativeRBACSafetyNet is called once at startup, right after opening
// nativeDB. See the package doc comment above for why this call must always
// happen, unconditionally, before anything else in this file runs.
func ensureNativeRBACSafetyNet(nativeDB *tinysql.DB) {
	nativeDB.SetRBACEnabled(false)
}

// ensureNativeRBACRoles idempotently creates the two mirrored roles and
// their wildcard grants. Only grants right after a role is newly created —
// not on every call — since GrantPermission itself is append-only (calling
// it again would accumulate a duplicate, harmless but wasteful, grant every
// time this runs, e.g. on every process restart).
func (a *App) ensureNativeRBACRoles() error {
	catalog := a.nativeDB.Catalog()
	if err := catalog.CreateRole(nativeRoleWrite); err == nil {
		if err := catalog.GrantPermission(nativeRoleWrite, tinysql.PermAll, "*", "*"); err != nil {
			return fmt.Errorf("grant permission to %s: %w", nativeRoleWrite, err)
		}
	}
	if err := catalog.CreateRole(nativeRoleReadOnly); err == nil {
		if err := catalog.GrantPermission(nativeRoleReadOnly, tinysql.PermSelect, "*", "*"); err != nil {
			return fmt.Errorf("grant permission to %s: %w", nativeRoleReadOnly, err)
		}
	}
	return nil
}

// nativeRoleForDataDockRole maps a DataDock Role to the mirrored tinySQL
// role name.
func nativeRoleForDataDockRole(role Role) string {
	if role == RoleReadOnly {
		return nativeRoleReadOnly
	}
	return nativeRoleWrite
}

// nativeRBACPassword generates a random, unguessable, immediately-discarded
// password: tinySQL's Authenticate is never called by DataDock (which owns
// its own bcrypt-based login), so this credential exists only to satisfy
// CreateUser's "password must not be empty" requirement.
func nativeRBACPassword() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate native RBAC password: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// mirrorNativeRBACUser creates or updates username's mirrored tinySQL
// identity to match role. Called from users.go's createUser/updateUserRole
// lifecycle hooks; errors are logged by the caller, not propagated — a
// mirroring failure must never block DataDock's own (authoritative) user
// management.
func (a *App) mirrorNativeRBACUser(ctx context.Context, username string, role Role) error {
	if err := a.ensureNativeRBACRoles(); err != nil {
		return err
	}
	catalog := a.nativeDB.Catalog()
	target := nativeRoleForDataDockRole(role)
	if _, exists := catalog.GetUser(username); !exists {
		password, err := nativeRBACPassword()
		if err != nil {
			return err
		}
		return catalog.CreateUser(username, password, []string{target})
	}
	other := nativeRoleReadOnly
	if target == nativeRoleReadOnly {
		other = nativeRoleWrite
	}
	if err := catalog.RevokeRoleFromUser(username, other); err != nil {
		return err
	}
	if err := catalog.GrantRoleToUser(username, target); err != nil {
		return err
	}
	return catalog.SetUserDisabled(username, false)
}

// mirrorNativeRBACUserDisabled reflects users.go's setUserDisabled into the
// mirrored identity — HasPermission fails closed for a disabled user (see
// storage/rbac.go), matching the app-level effect exactly.
func (a *App) mirrorNativeRBACUserDisabled(username string, disabled bool) error {
	catalog := a.nativeDB.Catalog()
	if _, exists := catalog.GetUser(username); !exists {
		return nil
	}
	return catalog.SetUserDisabled(username, disabled)
}

// mirrorNativeRBACUserDeleted reflects users.go's deleteUser into the
// mirrored identity.
func (a *App) mirrorNativeRBACUserDeleted(username string) error {
	catalog := a.nativeDB.Catalog()
	if _, exists := catalog.GetUser(username); !exists {
		return nil
	}
	return catalog.DropUser(username)
}

// bootstrapNativeRBACMirror is called once at startup (after
// ensureNativeRBACSafetyNet) to sync every existing __datadock_users row
// into the mirror — covering both a fresh enable of this feature on an
// installation that already has users, and a restart after a restore.
func (a *App) bootstrapNativeRBACMirror(ctx context.Context) error {
	if err := a.ensureNativeRBACRoles(); err != nil {
		return err
	}
	users, err := a.listUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := a.mirrorNativeRBACUser(ctx, u.Username, u.Role); err != nil {
			return fmt.Errorf("mirror user %q: %w", u.Username, err)
		}
		if err := a.mirrorNativeRBACUserDisabled(u.Username, u.Disabled); err != nil {
			return fmt.Errorf("mirror disabled state for %q: %w", u.Username, err)
		}
	}
	return nil
}
