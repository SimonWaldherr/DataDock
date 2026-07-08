package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRoutineViewHandlerFallsBackCleanlyForUnsupportedDialect covers the one
// slice of the routine-definition feature that's testable without a live
// MSSQL/PostgreSQL/MySQL server: dialects with no stored routine concept
// (tinySQL, and by the same code path SQLite) must render the existing
// object_missing page with a clear reason, not a raw error or a panic.
func TestRoutineViewHandlerFallsBackCleanlyForUnsupportedDialect(t *testing.T) {
	app := newTestApp(t)
	// Routed through the real mux, not called directly: routineViewHandler
	// reads r.PathValue("routine"), which net/http only populates when the
	// request was matched against the "GET /r/{routine}" pattern.
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/r/some_proc", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a dialect with no routine support, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "some_proc") {
		t.Errorf("expected the response to name the requested routine; body:\n%s", body)
	}
	if !strings.Contains(body, "aren&#39;t supported for tinySQL") {
		t.Errorf("expected a clear \"aren't supported\" reason in the response; body:\n%s", body)
	}
}

// TestFetchRoutineDefinitionMySQLKindSelectsShowCreateVariant guards the one
// piece of fetchRoutineDefinition's dialect branching that's testable
// without a live MySQL server: it must actually attempt a MySQL-flavored
// query for both "procedure" and "function" kinds rather than falling
// through to the "dialect not supported" default case. A real (but closed)
// *sql.DB stands in for MySQL here — cheap and panic-free, unlike a nil
// *sql.DB — since the point is only to prove a query was attempted, not to
// exercise real MySQL semantics.
func TestFetchRoutineDefinitionMySQLKindSelectsShowCreateVariant(t *testing.T) {
	conn, err := OpenManagedConnectionVerbose(context.Background(), "mysql-shape-test", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open backing connection: %v", err)
	}
	defer conn.DB.Close()
	conn.Dialect = DialectProfileForName("mysql") // force the MySQL branch; the backing DB is only there to avoid a nil *sql.DB panic
	if conn.Dialect.Name != "MariaDB/MySQL" {
		t.Fatalf("test assumption broken: DialectProfileForName(\"mysql\").Name = %q", conn.Dialect.Name)
	}

	for _, kind := range []string{"procedure", "function"} {
		_, err := conn.fetchRoutineDefinition(context.Background(), "myproc", kind)
		if err == nil {
			t.Errorf("kind=%s: expected an error since the backing DB doesn't understand SHOW CREATE %s", kind, strings.ToUpper(kind))
		}
		if strings.Contains(err.Error(), "aren't supported for") {
			t.Errorf("kind=%s: expected a MySQL query attempt, got the unsupported-dialect fallback: %v", kind, err)
		}
	}
}

// TestFetchDependenciesUnsupportedDialect guards the fallback path for
// dialects with no dependency-tracking mechanism DataDock understands
// (tinySQL and, by the same default case, SQLite).
func TestFetchDependenciesUnsupportedDialect(t *testing.T) {
	conn, err := OpenManagedConnectionVerbose(context.Background(), "tinysql-shape-test", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open backing connection: %v", err)
	}
	defer conn.DB.Close()
	conn.Dialect = DialectProfileForName("tinysql")

	_, _, err = conn.fetchDependencies(context.Background(), "sometable", "table")
	if err == nil {
		t.Fatal("expected an error for a dialect with no dependency tracking")
	}
	if !strings.Contains(err.Error(), "isn't supported for") {
		t.Errorf("error = %q, want it to mention dependency analysis isn't supported", err.Error())
	}
}

// TestFetchDependenciesPostgresRejectsRoutineKind guards the explicit
// table/view-only restriction on PostgreSQL and MySQL/MariaDB: only SQL
// Server resolves routine dependencies, so asking for a procedure/function
// on the other two dialects must fail clearly rather than silently return
// nothing (which would look like "this routine truly has no dependencies").
func TestFetchDependenciesPostgresRejectsRoutineKind(t *testing.T) {
	conn, err := OpenManagedConnectionVerbose(context.Background(), "pg-shape-test", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open backing connection: %v", err)
	}
	defer conn.DB.Close()
	conn.Dialect = DialectProfileForName("postgres")

	_, _, err = conn.fetchDependencies(context.Background(), "myproc", "procedure")
	if err == nil {
		t.Fatal("expected an error for a routine kind on PostgreSQL")
	}
	if !strings.Contains(err.Error(), "only available for tables and views") {
		t.Errorf("error = %q, want it to explain the table/view-only restriction", err.Error())
	}
}

// TestFetchDependenciesMySQLAttemptsQueryForTablesAndViews mirrors
// TestFetchRoutineDefinitionMySQLKindSelectsShowCreateVariant: it proves
// fetchDependencies actually dispatches to the MySQL-specific
// information_schema.view_table_usage query for a table/view kind, rather
// than falling through to a dialect-unsupported or kind-rejected error.
func TestFetchDependenciesMySQLAttemptsQueryForTablesAndViews(t *testing.T) {
	conn, err := OpenManagedConnectionVerbose(context.Background(), "mysql-deps-shape-test", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open backing connection: %v", err)
	}
	defer conn.DB.Close()
	conn.Dialect = DialectProfileForName("mysql")

	// SQLite has no information_schema.view_table_usage, so this must fail
	// — but with a query error, not one of our own guard-clause messages.
	_, _, err = conn.fetchDependencies(context.Background(), "myview", "view")
	if err == nil {
		t.Fatal("expected a query error since the backing DB has no information_schema.view_table_usage")
	}
	for _, guard := range []string{"isn't supported for", "only available for tables and views"} {
		if strings.Contains(err.Error(), guard) {
			t.Errorf("expected an actual MySQL query attempt, got the guard-clause error: %v", err)
		}
	}
}

// TestBuildTableScriptIncludesDependencies is the end-to-end check: a real
// tinySQL table's /api/tables/{table}/script response must surface
// dependenciesError (tinySQL has no dependency tracking) rather than
// silently omitting the field or panicking.
func TestBuildTableScriptIncludesDependencies(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup", `CREATE TABLE deps_demo (id INT, name TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	mux := http.NewServeMux()
	app.registerRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/tables/deps_demo/script", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var script TableScript
	if err := json.Unmarshal(rec.Body.Bytes(), &script); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if script.DependenciesError == "" {
		t.Error("expected DependenciesError to be set for the tinySQL connection")
	}
	if len(script.DependsOn) != 0 || len(script.Dependents) != 0 {
		t.Errorf("expected no dependency edges alongside an error, got DependsOn=%v Dependents=%v", script.DependsOn, script.Dependents)
	}
}
