package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestConnectionManagerSQLiteActiveConnection(t *testing.T) {
	app := newTestApp(t)
	conn, err := OpenManagedConnection(context.Background(), "sqlite-test", "SQLite Test", "sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.DB.Close() })
	if _, err := conn.DB.Exec("CREATE TABLE ext_people (id INTEGER, name TEXT)"); err != nil {
		t.Fatalf("create sqlite table: %v", err)
	}
	if _, err := conn.DB.Exec("INSERT INTO ext_people (id, name) VALUES (1, 'Grace')"); err != nil {
		t.Fatalf("insert sqlite row: %v", err)
	}
	if err := app.conns.Add(conn); err != nil {
		t.Fatalf("add connection: %v", err)
	}
	if err := app.conns.SetActive(conn.ID); err != nil {
		t.Fatalf("set active: %v", err)
	}
	app.dialect = conn.Dialect

	names := app.tableNames(context.Background())
	if len(names) != 1 || names[0] != "ext_people" {
		t.Fatalf("expected ext_people table from active sqlite connection, got %#v", names)
	}
	meta, err := app.tableMeta(context.Background(), "ext_people")
	if err != nil {
		t.Fatalf("table meta: %v", err)
	}
	if meta.RowCount != 1 || !meta.HasID {
		t.Fatalf("unexpected meta: %#v", meta)
	}
	result := app.executeSQL(context.Background(), "SELECT name FROM ext_people")
	if result.Err != "" {
		t.Fatalf("executeSQL error: %s", result.Err)
	}
	if len(result.Rows) != 1 || result.Rows[0][0] != "Grace" {
		t.Fatalf("unexpected rows: %#v", result.Rows)
	}
}

func TestActiveConnectionIsSessionScoped(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE tiny_only (id INT, name TEXT)"); err != nil {
		t.Fatalf("create default table: %v", err)
	}

	conn, err := OpenManagedConnection(context.Background(), "sqlite-session", "SQLite Session", "sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.DB.Close() })
	if _, err := conn.DB.Exec("CREATE TABLE ext_only (id INTEGER, name TEXT)"); err != nil {
		t.Fatalf("create sqlite table: %v", err)
	}
	if err := app.conns.Add(conn); err != nil {
		t.Fatalf("add connection: %v", err)
	}

	mux := http.NewServeMux()
	app.registerRoutes(mux)

	firstGet := httptest.NewRecorder()
	mux.ServeHTTP(firstGet, httptest.NewRequest(http.MethodGet, "/connections", nil))
	sessionCookie := firstGet.Result().Cookies()[0]

	form := url.Values{"id": {conn.ID}}
	switchReq := httptest.NewRequest(http.MethodPost, "/connections/active", strings.NewReader(form.Encode()))
	switchReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	switchReq.AddCookie(sessionCookie)
	switchRec := httptest.NewRecorder()
	mux.ServeHTTP(switchRec, switchReq)
	if switchRec.Code != http.StatusSeeOther {
		t.Fatalf("expected active switch redirect, got %d", switchRec.Code)
	}

	sessionReq := httptest.NewRequest(http.MethodGet, "/", nil)
	sessionReq.AddCookie(sessionCookie)
	sessionRec := httptest.NewRecorder()
	mux.ServeHTTP(sessionRec, sessionReq)
	if loc := sessionRec.Header().Get("Location"); !strings.Contains(loc, "ext_only") {
		t.Fatalf("expected session to see sqlite table, got redirect %q", loc)
	}

	otherRec := httptest.NewRecorder()
	mux.ServeHTTP(otherRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if loc := otherRec.Header().Get("Location"); !strings.Contains(loc, "tiny_only") {
		t.Fatalf("expected new session to stay on default tinySQL table, got redirect %q", loc)
	}
}

func TestProductionConnectionDialectsAndQualifiedIdentifiers(t *testing.T) {
	kind, driver, dsn, dialect := parseConnectionDSN("mariadb", "user:pass@tcp(localhost:3306)/erp")
	if kind != "mysql" || driver != "mysql" || dsn != "user:pass@tcp(localhost:3306)/erp" || dialect.Name != "MariaDB/MySQL" {
		t.Fatalf("unexpected MariaDB parse: kind=%q driver=%q dsn=%q dialect=%q", kind, driver, dsn, dialect.Name)
	}

	kind, driver, dsn, dialect = parseConnectionDSN("mssql", "mssql://user:pass@localhost:1433?database=dwh")
	if kind != "mssql" || driver != "sqlserver" || !strings.HasPrefix(dsn, "sqlserver://") || dialect.Name != "Microsoft SQL Server" {
		t.Fatalf("unexpected MSSQL parse: kind=%q driver=%q dsn=%q dialect=%q", kind, driver, dsn, dialect.Name)
	}

	mysqlConn := &DBConnection{Dialect: DialectProfileForName("mariadb")}
	if got := mysqlConn.QuoteIdent("reporting.sales_view"); got != "`reporting`.`sales_view`" {
		t.Fatalf("MariaDB qualified identifier = %q", got)
	}

	mssqlConn := &DBConnection{Dialect: DialectProfileForName("mssql")}
	if got := mssqlConn.QuoteIdent("dbo.SalesView"); got != "[dbo].[SalesView]" {
		t.Fatalf("MSSQL qualified identifier = %q", got)
	}
}
