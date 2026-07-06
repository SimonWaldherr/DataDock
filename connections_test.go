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

// TestAddedConnectionIsPrivateToSession guards against the bug where adding
// a managed connection made it visible to every concurrent user of the same
// running server (the connection store had no per-session ownership at
// all). Two different sessions each add their own "mydb" connection; each
// must see only its own, and adding the same ID from the second session
// must not close/replace the first session's live connection.
func TestAddedConnectionIsPrivateToSession(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	addConnection := func(cookie *http.Cookie) *http.Cookie {
		form := url.Values{"id": {"mydb"}, "name": {"mydb"}, "kind": {"sqlite"}, "dsn": {":memory:"}}
		req := httptest.NewRequest(http.MethodPost, "/connections", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("expected add-connection redirect, got %d: %s", rec.Code, rec.Body.String())
		}
		if cookie != nil {
			return cookie
		}
		cookies := rec.Result().Cookies()
		if len(cookies) == 0 {
			t.Fatalf("expected a session cookie to be set")
		}
		return cookies[0]
	}

	sessionA := addConnection(nil)
	sessionB := addConnection(nil)
	if sessionA.Value == sessionB.Value {
		t.Fatalf("expected two distinct sessions")
	}
	addConnection(sessionA)
	addConnection(sessionB)

	listFor := func(cookie *http.Cookie) []ConnectionInfo {
		req := httptest.NewRequest(http.MethodGet, "/connections", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		sessionID := sessionIDFromRequest(req)
		return app.conns.ListFor(sessionID)
	}

	namesFor := func(infos []ConnectionInfo) (names []string) {
		for _, c := range infos {
			names = append(names, c.Name)
		}
		return names
	}

	aList := listFor(sessionA)
	bList := listFor(sessionB)
	if len(aList) != 2 { // default tinySQL + its own "mydb"
		t.Fatalf("expected session A to see exactly its own connections, got %v", namesFor(aList))
	}
	if len(bList) != 2 {
		t.Fatalf("expected session B to see exactly its own connections, got %v", namesFor(bList))
	}

	// Neither session's private "mydb" connection was silently replaced by
	// the other's identically-named one: both underlying *sql.DB handles
	// must still be distinct, open connections.
	var aConnID, bConnID string
	for _, c := range aList {
		if c.Name == "mydb" {
			aConnID = c.ID
		}
	}
	for _, c := range bList {
		if c.Name == "mydb" {
			bConnID = c.ID
		}
	}
	if aConnID == "" || bConnID == "" {
		t.Fatalf("expected both sessions to have their own mydb connection, got a=%q b=%q", aConnID, bConnID)
	}
	if aConnID == bConnID {
		t.Fatalf("expected disambiguated connection IDs, both sessions got %q", aConnID)
	}
	if conn := app.conns.Get(aConnID); conn == nil || conn.DB == nil {
		t.Fatalf("expected session A's connection to still be open")
	}
	if conn := app.conns.Get(bConnID); conn == nil || conn.DB == nil {
		t.Fatalf("expected session B's connection to still be open")
	}

	// Session B must not be able to switch its active connection to
	// session A's private connection by forging its ID.
	form := url.Values{"id": {aConnID}}
	switchReq := httptest.NewRequest(http.MethodPost, "/connections/active", strings.NewReader(form.Encode()))
	switchReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	switchReq.AddCookie(sessionB)
	switchRec := httptest.NewRecorder()
	mux.ServeHTTP(switchRec, switchReq)
	if switchRec.Code == http.StatusSeeOther {
		t.Fatalf("expected session B to be denied switching to session A's private connection")
	}
}

// TestSetDefaultConnectionRequiresAdminAndSharedConnection guards against a
// privilege-escalation bug where any logged-in-as-nobody session could POST
// to /connections/active with set_default=1 and globally hijack the
// server's fallback connection for every other session — including
// pointing it at their own private, credential-bearing connection, without
// ever going through the admin-gated "share" step. The fix: changing the
// default now requires (a) an admin session and (b) a connection that's
// already shared (not privately owned by anyone).
func TestSetDefaultConnectionRequiresAdminAndSharedConnection(t *testing.T) {
	app := newTestApp(t)
	app.adminPasswordHash = "test-hash-not-a-real-bcrypt-value"
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	// A non-admin session adds its own private connection.
	addRec := httptest.NewRecorder()
	addReq := httptest.NewRequest(http.MethodPost, "/connections", strings.NewReader(
		url.Values{"id": {"mydb"}, "name": {"mydb"}, "kind": {"sqlite"}, "dsn": {":memory:"}}.Encode()))
	addReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(addRec, addReq)
	if addRec.Code != http.StatusSeeOther {
		t.Fatalf("expected add-connection redirect, got %d: %s", addRec.Code, addRec.Body.String())
	}
	userCookie := addRec.Result().Cookies()[0]

	// The old unprivileged form field must no longer have any effect: a
	// plain "Use" request with set_default=1 should not change the global
	// default, whether or not the field is even still accepted.
	useRec := httptest.NewRecorder()
	useReq := httptest.NewRequest(http.MethodPost, "/connections/active", strings.NewReader(
		url.Values{"id": {"mydb"}, "set_default": {"1"}}.Encode()))
	useReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	useReq.AddCookie(userCookie)
	mux.ServeHTTP(useRec, useReq)
	if got := app.conns.DefaultID(); got != defaultConnectionID {
		t.Fatalf("expected default to remain %q after unprivileged request, got %q", defaultConnectionID, got)
	}

	// A request to the admin-only route without an admin session must be
	// rejected, and must not change the default either.
	adminRouteNoAuthRec := httptest.NewRecorder()
	adminRouteNoAuthReq := httptest.NewRequest(http.MethodPost, "/admin/connections/default", strings.NewReader(
		url.Values{"id": {"mydb"}}.Encode()))
	adminRouteNoAuthReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	adminRouteNoAuthReq.AddCookie(userCookie)
	mux.ServeHTTP(adminRouteNoAuthRec, adminRouteNoAuthReq)
	if loc := adminRouteNoAuthRec.Header().Get("Location"); !strings.Contains(loc, "/admin/login") {
		t.Fatalf("expected non-admin session to be redirected to admin login, got %q (code %d)", loc, adminRouteNoAuthRec.Code)
	}
	if got := app.conns.DefaultID(); got != defaultConnectionID {
		t.Fatalf("expected default to remain %q after non-admin admin-route request, got %q", defaultConnectionID, got)
	}

	// Log the same session in as admin, then try to default the still-private
	// connection: ConnectionManager.SetDefault must refuse it.
	app.markAdminAuthenticated(sessionIDFromRequest(useReq))
	adminSessionCookie := userCookie

	stillPrivateRec := httptest.NewRecorder()
	stillPrivateReq := httptest.NewRequest(http.MethodPost, "/admin/connections/default", strings.NewReader(
		url.Values{"id": {"mydb"}}.Encode()))
	stillPrivateReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	stillPrivateReq.AddCookie(adminSessionCookie)
	mux.ServeHTTP(stillPrivateRec, stillPrivateReq)
	if got := app.conns.DefaultID(); got != defaultConnectionID {
		t.Fatalf("expected default to remain %q for a still-private connection, got %q", defaultConnectionID, got)
	}

	// Share it (admin-only "Save for everyone"), then defaulting it must
	// succeed.
	persistRec := httptest.NewRecorder()
	persistReq := httptest.NewRequest(http.MethodPost, "/admin/connections/persist", strings.NewReader(
		url.Values{"id": {"mydb"}}.Encode()))
	persistReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	persistReq.AddCookie(adminSessionCookie)
	mux.ServeHTTP(persistRec, persistReq)
	if persistRec.Code != http.StatusSeeOther {
		t.Fatalf("expected persist redirect, got %d: %s", persistRec.Code, persistRec.Body.String())
	}

	defaultRec := httptest.NewRecorder()
	defaultReq := httptest.NewRequest(http.MethodPost, "/admin/connections/default", strings.NewReader(
		url.Values{"id": {"mydb"}}.Encode()))
	defaultReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	defaultReq.AddCookie(adminSessionCookie)
	mux.ServeHTTP(defaultRec, defaultReq)
	if defaultRec.Code != http.StatusSeeOther {
		t.Fatalf("expected default-change redirect once shared, got %d: %s", defaultRec.Code, defaultRec.Body.String())
	}
	if got := app.conns.DefaultID(); got != "mydb" {
		t.Fatalf("expected default to become %q once shared, got %q", "mydb", got)
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
