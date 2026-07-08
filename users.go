package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const usersTable = "__datadock_users"

// Role is the permission tier assigned to a local user account.
type Role string

const (
	// RoleAdmin can manage runtime settings, connections, jobs, demo data,
	// and other users — today's single shared Admin's full capability set.
	RoleAdmin Role = "admin"
	// RoleUser is today's implicit default: full read/write access to data
	// (record CRUD, SQL editor, migration, matching, imports/exports), but
	// no access to admin-only settings/connections/jobs/user management.
	RoleUser Role = "user"
	// RoleReadOnly blocks every write route and every non-SELECT SQL
	// statement, regardless of maintenance mode — for an account that
	// should only ever look at data, never change it.
	RoleReadOnly Role = "readonly"
)

// normalizeRole validates a raw role string. Unlike normalizeAuthMode, a
// blank value is invalid here: every stored user row must have an explicit
// role. Defaulting (the first-ever user is always RoleAdmin) is the
// responsibility of the caller that creates that user, not this function.
func normalizeRole(raw string) (Role, error) {
	role := Role(strings.ToLower(strings.TrimSpace(raw)))
	switch role {
	case RoleAdmin, RoleUser, RoleReadOnly:
		return role, nil
	default:
		return "", fmt.Errorf("unknown role %q (expected %q, %q, or %q)", raw, RoleAdmin, RoleUser, RoleReadOnly)
	}
}

// User is one row of the __datadock_users table.
type User struct {
	Username     string
	PasswordHash string
	Role         Role
	CreatedAt    time.Time
	Disabled     bool
}

// ErrUserExists is returned by createUser when the (case-insensitively
// compared) username is already taken.
var ErrUserExists = errors.New("username already exists")

// ensureUsersTable follows the exact idiom ensureRuntimeSettingsTable uses
// in settings.go: attempt CREATE TABLE unconditionally, swallow "already
// exists". No PRIMARY KEY/UNIQUE constraint (none exist anywhere in this
// codebase's tinySQL DDL) — uniqueness is enforced in Go, in createUser.
func (a *App) ensureUsersTable(ctx context.Context) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "users.ensure_table",
		"CREATE TABLE "+usersTable+" (username TEXT, password_hash TEXT, role TEXT, created_at TEXT, disabled TEXT)")
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return fmt.Errorf("ensure users table: %w", err)
}

// listUsers returns every user, sorted by username for a stable UI order.
func (a *App) listUsers(ctx context.Context) ([]User, error) {
	if err := a.ensureUsersTable(ctx); err != nil {
		return nil, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "users.list",
		"SELECT username, password_hash, role, created_at, disabled FROM "+usersTable)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var username, hash, roleRaw, createdAtRaw, disabledRaw string
		if err := rows.Scan(&username, &hash, &roleRaw, &createdAtRaw, &disabledRaw); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		role, err := normalizeRole(roleRaw)
		if err != nil {
			return nil, fmt.Errorf("user %q: %w", username, err)
		}
		createdAt, _ := time.Parse(time.RFC3339, createdAtRaw)
		disabled, _ := strconv.ParseBool(disabledRaw)
		users = append(users, User{
			Username:     username,
			PasswordHash: hash,
			Role:         role,
			CreatedAt:    createdAt,
			Disabled:     disabled,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	sort.Slice(users, func(i, j int) bool {
		return strings.ToLower(users[i].Username) < strings.ToLower(users[j].Username)
	})
	return users, nil
}

// getUserByUsername looks up a user case-insensitively (there's no unique
// index to rely on, so this scans listUsers and compares with EqualFold —
// fine at the scale a local-accounts table is expected to have).
func (a *App) getUserByUsername(ctx context.Context, username string) (User, bool, error) {
	users, err := a.listUsers(ctx)
	if err != nil {
		return User{}, false, err
	}
	username = strings.TrimSpace(username)
	for _, u := range users {
		if strings.EqualFold(u.Username, username) {
			return u, true, nil
		}
	}
	return User{}, false, nil
}

// usersConfigured reports whether at least one local account exists. It
// replaces adminPasswordIsSet() as the "has setup happened yet" check.
func (a *App) usersConfigured(ctx context.Context) (bool, error) {
	users, err := a.listUsers(ctx)
	if err != nil {
		return false, err
	}
	return len(users) > 0, nil
}

func (a *App) authConfigured(ctx context.Context) (bool, error) {
	configured, err := a.usersConfigured(ctx)
	if err != nil || configured {
		return configured, err
	}
	return a.adminPasswordIsSet(), nil
}

// countEnabledAdmins is used by the last-admin guard so the UI can never be
// used to lock every admin out of DataDock.
func (a *App) countEnabledAdmins(ctx context.Context) (int, error) {
	users, err := a.listUsers(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		if u.Role == RoleAdmin && !u.Disabled {
			n++
		}
	}
	return n, nil
}

// assertNotLastEnabledAdmin errors if applying a role-change/disable/delete
// to targetUser would leave zero enabled admins. It's a no-op if the target
// isn't currently a counted admin in the first place (already disabled or
// not RoleAdmin), so it's safe to call unconditionally before any of those
// three mutations.
func (a *App) assertNotLastEnabledAdmin(ctx context.Context, targetUser User) error {
	if targetUser.Role != RoleAdmin || targetUser.Disabled {
		return nil
	}
	n, err := a.countEnabledAdmins(ctx)
	if err != nil {
		return err
	}
	if n <= 1 {
		return fmt.Errorf("cannot remove the last remaining admin (%s)", targetUser.Username)
	}
	return nil
}

// createUser inserts a new user, rejecting a case-insensitive duplicate
// username with ErrUserExists. The theoretical TOCTOU race between the
// existence check and the INSERT (two concurrent submissions could both
// pass the check) is accepted here, matching this codebase's existing risk
// tolerance: no table in it uses a UNIQUE/PRIMARY KEY constraint either.
func (a *App) createUser(ctx context.Context, username, passwordHash string, role Role) error {
	if err := a.ensureUsersTable(ctx); err != nil {
		return err
	}
	username = strings.TrimSpace(username)
	if _, found, err := a.getUserByUsername(ctx, username); err != nil {
		return err
	} else if found {
		return ErrUserExists
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "users.insert",
		"INSERT INTO "+usersTable+" (username, password_hash, role, created_at, disabled) VALUES (?, ?, ?, ?, ?)",
		username, passwordHash, string(role), time.Now().UTC().Format(time.RFC3339), strconv.FormatBool(false),
	)
	return err
}

func (a *App) updateUserRole(ctx context.Context, username string, role Role) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "users.update_role",
		"UPDATE "+usersTable+" SET role = ? WHERE username = ?", string(role), username)
	return err
}

func (a *App) setUserDisabled(ctx context.Context, username string, disabled bool) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "users.update_disabled",
		"UPDATE "+usersTable+" SET disabled = ? WHERE username = ?", strconv.FormatBool(disabled), username)
	return err
}

func (a *App) setUserPasswordHash(ctx context.Context, username, hash string) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "users.update_password",
		"UPDATE "+usersTable+" SET password_hash = ? WHERE username = ?", hash, username)
	return err
}

func (a *App) deleteUser(ctx context.Context, username string) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "users.delete",
		"DELETE FROM "+usersTable+" WHERE username = ?", username)
	return err
}

// migrateLegacyAdminPassword converts a pre-multi-user deployment's single
// shared Admin password into a real "admin" user account, so upgrading
// never locks anyone out or forces a re-setup. It's a no-op on a fresh
// install (no legacy hash) and a no-op once any user already exists (the
// migration already ran, or a fresh multi-user setup was completed).
// Bcrypt hashes are portable, so the existing hash is reused verbatim
// instead of forcing a re-hash of a password DataDock never sees in plain
// text again after this call.
func (a *App) migrateLegacyAdminPassword(ctx context.Context) error {
	if err := a.ensureUsersTable(ctx); err != nil {
		return err
	}
	configured, err := a.usersConfigured(ctx)
	if err != nil || configured {
		return err
	}
	hash := strings.TrimSpace(a.currentAdminPasswordHash())
	if hash == "" {
		return nil
	}
	if err := a.createUser(ctx, "admin", hash, RoleAdmin); err != nil && !errors.Is(err, ErrUserExists) {
		return fmt.Errorf("migrate legacy admin password: %w", err)
	}
	return nil
}
