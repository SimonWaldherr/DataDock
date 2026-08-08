package main

import (
	"context"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func TestEnsureNativeRBACRolesIsIdempotent(t *testing.T) {
	app := newTestApp(t)
	for i := 0; i < 3; i++ {
		if err := app.ensureNativeRBACRoles(); err != nil {
			t.Fatalf("ensureNativeRBACRoles (call %d): %v", i, err)
		}
	}
	catalog := app.nativeDB.Catalog()
	if err := catalog.CreateUser("probe", "throwaway-password-1234", []string{nativeRoleWrite}); err != nil {
		t.Fatalf("create probe user: %v", err)
	}
	if !catalog.HasPermission("probe", tinysql.PermInsert, "*", "*") {
		t.Error("expected the write role to still have exactly one INSERT grant after repeated ensureNativeRBACRoles calls")
	}
}

func TestMirrorNativeRBACUserGrantsMatchDataDockRole(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	catalog := app.nativeDB.Catalog()

	if err := app.mirrorNativeRBACUser(ctx, "alice", RoleUser); err != nil {
		t.Fatalf("mirror RoleUser: %v", err)
	}
	if !catalog.HasPermission("alice", tinysql.PermInsert, "*", "*") {
		t.Error("expected a mirrored RoleUser to have INSERT permission")
	}
	if !catalog.HasPermission("alice", tinysql.PermSelect, "*", "*") {
		t.Error("expected a mirrored RoleUser to have SELECT permission")
	}

	if err := app.mirrorNativeRBACUser(ctx, "bob", RoleReadOnly); err != nil {
		t.Fatalf("mirror RoleReadOnly: %v", err)
	}
	if catalog.HasPermission("bob", tinysql.PermInsert, "*", "*") {
		t.Error("expected a mirrored RoleReadOnly to NOT have INSERT permission")
	}
	if !catalog.HasPermission("bob", tinysql.PermSelect, "*", "*") {
		t.Error("expected a mirrored RoleReadOnly to still have SELECT permission")
	}

	if err := app.mirrorNativeRBACUser(ctx, "root", RoleAdmin); err != nil {
		t.Fatalf("mirror RoleAdmin: %v", err)
	}
	if !catalog.HasPermission("root", tinysql.PermInsert, "*", "*") {
		t.Error("expected a mirrored RoleAdmin to have INSERT permission")
	}
}

func TestMirrorNativeRBACUserRoleChangeRevokesPreviousGrants(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	catalog := app.nativeDB.Catalog()

	if err := app.mirrorNativeRBACUser(ctx, "carol", RoleUser); err != nil {
		t.Fatalf("mirror as RoleUser: %v", err)
	}
	if !catalog.HasPermission("carol", tinysql.PermInsert, "*", "*") {
		t.Fatal("expected carol to have INSERT permission as RoleUser")
	}

	// Demote carol to RoleReadOnly — the mirror must revoke write access, not
	// just additively grant read.
	if err := app.mirrorNativeRBACUser(ctx, "carol", RoleReadOnly); err != nil {
		t.Fatalf("mirror as RoleReadOnly: %v", err)
	}
	if catalog.HasPermission("carol", tinysql.PermInsert, "*", "*") {
		t.Error("expected carol's INSERT permission to be revoked after demotion to RoleReadOnly")
	}
	if !catalog.HasPermission("carol", tinysql.PermSelect, "*", "*") {
		t.Error("expected carol to still have SELECT permission after demotion")
	}
}

func TestMirrorNativeRBACUserDisabledAndDeleted(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	catalog := app.nativeDB.Catalog()

	if err := app.mirrorNativeRBACUser(ctx, "dave", RoleUser); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if !catalog.HasPermission("dave", tinysql.PermInsert, "*", "*") {
		t.Fatal("expected dave to have INSERT permission")
	}

	if err := app.mirrorNativeRBACUserDisabled("dave", true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if catalog.HasPermission("dave", tinysql.PermInsert, "*", "*") {
		t.Error("expected a disabled mirrored user to fail HasPermission (storage/rbac.go fails closed for disabled users)")
	}

	if err := app.mirrorNativeRBACUserDisabled("dave", false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if !catalog.HasPermission("dave", tinysql.PermInsert, "*", "*") {
		t.Error("expected a re-enabled mirrored user to regain permission")
	}

	if err := app.mirrorNativeRBACUserDeleted("dave"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, exists := catalog.GetUser("dave"); exists {
		t.Error("expected the mirrored user to be gone after mirrorNativeRBACUserDeleted")
	}

	// Deleting/disabling a never-mirrored user must be a no-op, not an error
	// (e.g. a user created while the feature was still off).
	if err := app.mirrorNativeRBACUserDeleted("never-existed"); err != nil {
		t.Errorf("expected deleting an unmirrored user to be a no-op, got %v", err)
	}
	if err := app.mirrorNativeRBACUserDisabled("never-existed", true); err != nil {
		t.Errorf("expected disabling an unmirrored user to be a no-op, got %v", err)
	}
}

func TestBootstrapNativeRBACMirrorSyncsExistingUsers(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if err := app.createUser(ctx, "existing-admin", "hash", RoleAdmin); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	if err := app.setUserDisabled(ctx, "existing-admin", false); err != nil {
		t.Fatalf("setUserDisabled: %v", err)
	}

	if err := app.bootstrapNativeRBACMirror(ctx); err != nil {
		t.Fatalf("bootstrapNativeRBACMirror: %v", err)
	}

	if !app.nativeDB.Catalog().HasPermission("existing-admin", tinysql.PermInsert, "*", "*") {
		t.Error("expected bootstrapNativeRBACMirror to have mirrored the pre-existing user with write access")
	}
}

// TestNativeRBACSafetyNetHoldsRegardlessOfMirroring is the critical
// regression guard: no matter how many users get mirrored in (i.e. how many
// times CreateUser is called on the underlying tinySQL DB), Execute's
// automatic engine-level enforcement must stay permanently off, because
// ensureNativeRBACSafetyNet forced SetRBACEnabled(false). If this ever
// starts failing, every native execution call site in the app that doesn't
// set tinysql.WithUser (jobs, pipelines, imports, migrations, matching, ...)
// would start returning "access denied".
func TestNativeRBACSafetyNetHoldsRegardlessOfMirroring(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if app.nativeDB.IsRBACEnabled() {
		t.Fatal("expected IsRBACEnabled() to be false immediately after newTestApp (ensureNativeRBACSafetyNet)")
	}

	for i, role := range []Role{RoleAdmin, RoleUser, RoleReadOnly} {
		username := []string{"root", "alice", "bob"}[i]
		if err := app.mirrorNativeRBACUser(ctx, username, role); err != nil {
			t.Fatalf("mirror %q: %v", username, err)
		}
	}
	// HasUsers() is now true (three native users exist) — the safety net's
	// whole point is that this must not matter.
	if !app.nativeDB.Catalog().HasUsers() {
		t.Fatal("test setup bug: expected HasUsers() to be true after mirroring users")
	}
	if app.nativeDB.IsRBACEnabled() {
		t.Fatal("expected IsRBACEnabled() to remain false even with native users mirrored in — the safety net must hold")
	}

	// And prove it concretely: a native execution with no acting user set
	// via tinysql.WithUser must still succeed, exactly as it would with zero
	// RBAC involvement at all.
	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.safety_net", "CREATE TABLE safety_net_probe (id INT)"); err != nil {
		t.Fatalf("expected an unauthenticated native execution to still succeed, got: %v", err)
	}
}

func TestSessionCanWriteSQLNativeMirrorSecondCheck(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	settings := app.currentRuntimeSettings()
	settings.NativePermissionMirrorEnabled = true
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings: %v", err)
	}

	aliceCtx := sessionCtx(app, "sess-alice-0000000000000", "alice", RoleUser)
	if err := app.mirrorNativeRBACUser(ctx, "alice", RoleUser); err != nil {
		t.Fatalf("mirror alice: %v", err)
	}
	if !app.sessionCanWriteSQL(aliceCtx) {
		t.Error("expected alice (RoleUser, correctly mirrored) to be allowed to write")
	}

	// Simulate a drifted mirror: alice's app-level role says she can write,
	// but her native mirror was (for whatever reason) demoted to read-only.
	// The second check must catch this and deny, not just trust the first.
	if err := app.mirrorNativeRBACUser(ctx, "alice", RoleReadOnly); err != nil {
		t.Fatalf("demote alice's mirror: %v", err)
	}
	if app.sessionCanWriteSQL(aliceCtx) {
		t.Error("expected a drifted native mirror (denying) to override the app-level allow — defense in depth failed")
	}

	// With the toggle off, only the app-level decision matters, regardless
	// of the (still-drifted) mirror state.
	settings.NativePermissionMirrorEnabled = false
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings (disable): %v", err)
	}
	if !app.sessionCanWriteSQL(aliceCtx) {
		t.Error("expected the native mirror check to have no effect once native_permission_mirror_enabled is off")
	}
}
