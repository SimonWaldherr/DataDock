package main

import (
	"errors"
	"net/http"
	"strings"
)

// adminUsersHandler lists local accounts and the add-user form. .Users and
// .CurrentUsername are injected globally by a.render, so every handler in
// this file that re-renders "admin_users" on a validation error gets them
// for free without a manual re-fetch.
func (a *App) adminUsersHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "admin_users", map[string]interface{}{"Form": map[string]string{}})
}

func (a *App) adminUsersCreateHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	role, roleErr := normalizeRole(r.Form.Get("role"))
	password := r.Form.Get("password")

	fail := func(message string) {
		a.render(w, r, "admin_users", map[string]interface{}{
			"Error": message,
			"Form":  map[string]string{"username": username, "role": r.Form.Get("role")},
		})
	}

	if !isValidUsername(username) {
		fail("Username must be 3-64 characters: letters, digits, dot, underscore, or hyphen.")
		return
	}
	if roleErr != nil {
		fail("Choose a role.")
		return
	}
	if len(password) < 8 {
		fail("Password must be at least 8 characters.")
		return
	}
	hash, err := hashAdminPassword(password)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if err := a.createUser(r.Context(), username, hash, role); err != nil {
		if errors.Is(err, ErrUserExists) {
			fail("That username is taken.")
			return
		}
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// adminUsersRoleHandler changes a user's role. assertNotLastEnabledAdmin is
// safe to call unconditionally here: it only errors when the target is
// currently an enabled admin and would stop being one, so promotions and
// no-op "changes" to the same role never trip it.
func (a *App) adminUsersRoleHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	role, err := normalizeRole(r.Form.Get("role"))
	if err != nil {
		a.render(w, r, "admin_users", map[string]interface{}{"Error": "Choose a valid role.", "Form": map[string]string{}})
		return
	}
	target, found, err := a.getUserByUsername(r.Context(), username)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if !found {
		a.render(w, r, "admin_users", map[string]interface{}{"Error": "User not found.", "Form": map[string]string{}})
		return
	}
	if err := a.assertNotLastEnabledAdmin(r.Context(), target); err != nil {
		a.render(w, r, "admin_users", map[string]interface{}{"Error": err.Error(), "Form": map[string]string{}})
		return
	}
	if err := a.updateUserRole(r.Context(), username, role); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// adminUsersDisableHandler toggles a user's enabled/disabled state, and —
// when disabling — revokes any already-authenticated session for that
// account immediately rather than letting it keep working until its normal
// sessionAuthTTL expiry.
func (a *App) adminUsersDisableHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	disabled := r.Form.Get("disabled") == "true"
	if disabled {
		target, found, err := a.getUserByUsername(r.Context(), username)
		if err != nil {
			a.serverError(w, err)
			return
		}
		if !found {
			a.render(w, r, "admin_users", map[string]interface{}{"Error": "User not found.", "Form": map[string]string{}})
			return
		}
		if err := a.assertNotLastEnabledAdmin(r.Context(), target); err != nil {
			a.render(w, r, "admin_users", map[string]interface{}{"Error": err.Error(), "Form": map[string]string{}})
			return
		}
	}
	if err := a.setUserDisabled(r.Context(), username, disabled); err != nil {
		a.serverError(w, err)
		return
	}
	if disabled {
		a.revokeSessionsForUsername(username)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// adminUsersResetPasswordHandler lets an admin set a new password for
// another account directly — no current-password check, since this route
// is already requireAdmin-gated. Useful for a locked-out non-admin user;
// for a locked-out sole admin, restart with -auth-mode none temporarily or
// use the legacy single-password recovery path.
func (a *App) adminUsersResetPasswordHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	newPassword := r.Form.Get("new_password")
	if len(newPassword) < 8 {
		a.render(w, r, "admin_users", map[string]interface{}{"Error": "New password must be at least 8 characters.", "Form": map[string]string{}})
		return
	}
	hash, err := hashAdminPassword(newPassword)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if err := a.setUserPasswordHash(r.Context(), username, hash); err != nil {
		a.serverError(w, err)
		return
	}
	// Revoke any already-authenticated session for this account: an admin
	// resetting a password is usually responding to a compromise or
	// offboarding, and the point is moot if the old session just keeps
	// working until its normal TTL expiry.
	a.revokeSessionsForUsername(username)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// adminUsersDeleteHandler removes a user account. Refuses to delete the
// last enabled admin (assertNotLastEnabledAdmin) and refuses to delete the
// currently logged-in session's own account, to avoid reasoning about a
// dangling authenticated-but-deleted session.
func (a *App) adminUsersDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	currentUsername, _, _ := a.currentSessionUser(sessionIDFromContext(r.Context()))
	if strings.EqualFold(username, currentUsername) {
		a.render(w, r, "admin_users", map[string]interface{}{"Error": "You cannot delete your own account while logged in.", "Form": map[string]string{}})
		return
	}
	target, found, err := a.getUserByUsername(r.Context(), username)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if !found {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if err := a.assertNotLastEnabledAdmin(r.Context(), target); err != nil {
		a.render(w, r, "admin_users", map[string]interface{}{"Error": err.Error(), "Form": map[string]string{}})
		return
	}
	if err := a.deleteUser(r.Context(), username); err != nil {
		a.serverError(w, err)
		return
	}
	a.revokeSessionsForUsername(username)
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
