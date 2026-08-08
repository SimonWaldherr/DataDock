package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sessionCtx builds a context carrying sessionID, and registers sessionID in
// app.adminAuthedSessions as username/role — the same low-level technique
// main_test.go already uses (see its expired-session test) to simulate a
// logged-in session without going through the HTTP login flow.
func sessionCtx(app *App, sessionID, username string, role Role) context.Context {
	app.adminAuthMu.Lock()
	if app.adminAuthedSessions == nil {
		app.adminAuthedSessions = make(map[string]sessionAuth)
	}
	app.adminAuthedSessions[sessionID] = sessionAuth{Username: username, Role: role, Expiry: time.Now().Add(time.Hour)}
	app.adminAuthMu.Unlock()
	return context.WithValue(context.Background(), sessionIDContextKey{}, sessionID)
}

func TestRowPolicySaveLoadListDelete(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if _, ok, err := app.loadRowPolicy(ctx, "does-not-exist"); err != nil || ok {
		t.Fatalf("loadRowPolicy for a missing table: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	if err := app.saveRowPolicy(ctx, RowPolicy{TableName: "notes", OwnerColumn: "owner"}); err != nil {
		t.Fatalf("saveRowPolicy: %v", err)
	}
	policy, ok, err := app.loadRowPolicy(ctx, "notes")
	if err != nil || !ok {
		t.Fatalf("loadRowPolicy: ok=%v err=%v", ok, err)
	}
	if policy.OwnerColumn != "owner" || policy.UpdatedAt.IsZero() {
		t.Errorf("loaded policy = %+v, want OwnerColumn=owner and a stamped UpdatedAt", policy)
	}

	// Saving again under the same table name overwrites, not duplicates.
	if err := app.saveRowPolicy(ctx, RowPolicy{TableName: "notes", OwnerColumn: "created_by"}); err != nil {
		t.Fatalf("saveRowPolicy (overwrite): %v", err)
	}
	policies, err := app.listRowPolicies(ctx)
	if err != nil {
		t.Fatalf("listRowPolicies: %v", err)
	}
	if len(policies) != 1 || policies[0].OwnerColumn != "created_by" {
		t.Fatalf("expected exactly 1 policy with the overwritten column, got %+v", policies)
	}

	if err := app.deleteRowPolicy(ctx, "notes"); err != nil {
		t.Fatalf("deleteRowPolicy: %v", err)
	}
	if _, ok, err := app.loadRowPolicy(ctx, "notes"); err != nil || ok {
		t.Errorf("expected the policy to be gone after delete, got ok=%v err=%v", ok, err)
	}
}

// setupRowPolicyTestTable creates a "notes" table with an "owner" column and
// a policy on it, ready for enforcement tests.
func setupRowPolicyTestTable(t *testing.T, app *App) []Column {
	t.Helper()
	ctx := context.Background()
	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup", `CREATE TABLE notes (id INT, owner TEXT, body TEXT)`); err != nil {
		t.Fatalf("create notes table: %v", err)
	}
	if err := app.saveRowPolicy(ctx, RowPolicy{TableName: "notes", OwnerColumn: "owner"}); err != nil {
		t.Fatalf("saveRowPolicy: %v", err)
	}
	return []Column{{Name: "id"}, {Name: "owner"}, {Name: "body"}}
}

func TestRowPolicyInsertStampsOwnerRegardlessOfSubmittedValue(t *testing.T) {
	app := newTestApp(t)
	cols := setupRowPolicyTestTable(t, app)
	aliceCtx := sessionCtx(app, "sess-alice-0000000000000", "alice", RoleUser)

	// Alice submits "owner=bob" for her own new row — insertRecord must
	// override it to her own username regardless.
	if err := app.insertRecord(aliceCtx, "notes", map[string]string{"owner": "bob", "body": "hello"}, cols); err != nil {
		t.Fatalf("insertRecord: %v", err)
	}

	rows, err := app.queryConn(aliceCtx, app.localTinySQLConn(), "test.check", "SELECT owner, body FROM notes")
	if err != nil {
		t.Fatalf("query notes: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected one row")
	}
	var owner, body string
	if err := rows.Scan(&owner, &body); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if owner != "alice" {
		t.Errorf("expected the row to be stamped owner=alice despite the submitted owner=bob, got owner=%q", owner)
	}
	if body != "hello" {
		t.Errorf("expected body=hello, got %q", body)
	}
}

func TestRowPolicyIsolatesUsersOnGetUpdateDelete(t *testing.T) {
	app := newTestApp(t)
	cols := setupRowPolicyTestTable(t, app)
	aliceCtx := sessionCtx(app, "sess-alice-0000000000000", "alice", RoleUser)
	bobCtx := sessionCtx(app, "sess-bob-00000000000000", "bob", RoleUser)

	if err := app.insertRecord(aliceCtx, "notes", map[string]string{"body": "alice's note"}, cols); err != nil {
		t.Fatalf("insert as alice: %v", err)
	}

	// Bob cannot read alice's row: getRecord returns sql.ErrNoRows, not her data.
	if _, _, err := app.getRecord(bobCtx, "notes", "1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected bob's getRecord on alice's row to return sql.ErrNoRows, got %v", err)
	}
	// Alice can read her own row.
	if _, row, err := app.getRecord(aliceCtx, "notes", "1"); err != nil {
		t.Fatalf("alice getRecord own row: %v", err)
	} else if row[2] != "alice's note" {
		t.Errorf("expected alice's own row body, got %+v", row)
	}

	// Bob's update against alice's id affects zero rows — content unchanged.
	if err := app.updateRecord(bobCtx, "notes", "1", map[string]string{"body": "hacked"}, cols); err != nil {
		t.Fatalf("bob updateRecord (should be a no-op, not an error): %v", err)
	}
	if _, row, err := app.getRecord(aliceCtx, "notes", "1"); err != nil {
		t.Fatalf("alice getRecord after bob's update attempt: %v", err)
	} else if row[2] != "alice's note" {
		t.Errorf("expected alice's row to be unchanged by bob's update attempt, got body=%q", row[2])
	}

	// Bob's delete against alice's id affects zero rows.
	if err := app.deleteRecord(bobCtx, "notes", "1"); err != nil {
		t.Fatalf("bob deleteRecord (should be a no-op, not an error): %v", err)
	}
	if _, _, err := app.getRecord(aliceCtx, "notes", "1"); err != nil {
		t.Fatalf("expected alice's row to still exist after bob's delete attempt: %v", err)
	}

	// Alice can update and delete her own row.
	if err := app.updateRecord(aliceCtx, "notes", "1", map[string]string{"body": "updated by alice"}, cols); err != nil {
		t.Fatalf("alice updateRecord own row: %v", err)
	}
	if _, row, err := app.getRecord(aliceCtx, "notes", "1"); err != nil || row[2] != "updated by alice" {
		t.Fatalf("expected alice's update to take effect, got row=%+v err=%v", row, err)
	}
	if err := app.deleteRecord(aliceCtx, "notes", "1"); err != nil {
		t.Fatalf("alice deleteRecord own row: %v", err)
	}
	if _, _, err := app.getRecord(aliceCtx, "notes", "1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected the row to be gone after alice's own delete, got %v", err)
	}
}

func TestRowPolicyAdminBypasses(t *testing.T) {
	app := newTestApp(t)
	cols := setupRowPolicyTestTable(t, app)
	aliceCtx := sessionCtx(app, "sess-alice-0000000000000", "alice", RoleUser)
	adminCtx := sessionCtx(app, "sess-admin-0000000000000", "root", RoleAdmin)

	if err := app.insertRecord(aliceCtx, "notes", map[string]string{"body": "alice's note"}, cols); err != nil {
		t.Fatalf("insert as alice: %v", err)
	}

	// Admin can read and modify alice's row despite not being its owner.
	if _, row, err := app.getRecord(adminCtx, "notes", "1"); err != nil {
		t.Fatalf("admin getRecord: %v", err)
	} else if row[2] != "alice's note" {
		t.Errorf("expected admin to see alice's row, got %+v", row)
	}
	if err := app.updateRecord(adminCtx, "notes", "1", map[string]string{"body": "moderated"}, cols); err != nil {
		t.Fatalf("admin updateRecord: %v", err)
	}
	if _, row, err := app.getRecord(adminCtx, "notes", "1"); err != nil || row[2] != "moderated" {
		t.Fatalf("expected admin's update to take effect, got row=%+v err=%v", row, err)
	}
}

func TestRowPolicyEnforcesOnTableBrowse(t *testing.T) {
	app := newTestApp(t)
	cols := setupRowPolicyTestTable(t, app)
	aliceCtx := sessionCtx(app, "sess-alice-0000000000000", "alice", RoleUser)
	bobCtx := sessionCtx(app, "sess-bob-00000000000000", "bob", RoleUser)

	if err := app.insertRecord(aliceCtx, "notes", map[string]string{"body": "alice's note"}, cols); err != nil {
		t.Fatalf("insert as alice: %v", err)
	}
	if err := app.insertRecord(bobCtx, "notes", map[string]string{"body": "bob's note"}, cols); err != nil {
		t.Fatalf("insert as bob: %v", err)
	}

	meta, err := app.tableMeta(aliceCtx, "notes")
	if err != nil {
		t.Fatalf("tableMeta: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/t/notes", nil).WithContext(aliceCtx)
	_, rows, err := app.tableRowsSorted(req, 1, "", "asc", 50, meta)
	if err != nil {
		t.Fatalf("tableRowsSorted as alice: %v", err)
	}
	if len(rows) != 1 || rows[0][2] != "alice's note" {
		t.Fatalf("expected alice to see exactly her own 1 row, got %+v", rows)
	}
}
