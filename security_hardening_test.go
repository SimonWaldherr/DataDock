package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestAnonymousSQLEditorWriteBlockedInAuthModeLocal locks in the fix for the
// anonymous-write hole in the SQL editor. The editor routes (POST /query,
// /api/query) are deliberately NOT requireLogin-wrapped so that reads stay
// open in AuthModeLocal — but the write guard (previously readOnlyRoleBlocksQuery)
// only tripped for an already-authenticated RoleReadOnly session, so an
// anonymous visitor (ok == false) could run arbitrary INSERT/UPDATE/DELETE/DDL
// with zero credentials. writeSQLBlocked now fails closed: no write-capable
// session, no write. Reads (SELECT) must still work anonymously.
func TestAnonymousSQLEditorWriteBlockedInAuthModeLocal(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE anon_sql_test (id INT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	apiQuery := func(sql string, cookie *http.Cookie) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"sql": sql})
		req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Anonymous DDL/DML must be rejected (403) and must not take effect.
	for _, write := range []string{
		"DROP TABLE anon_sql_test",
		"INSERT INTO anon_sql_test (id) VALUES (1)",
		"DELETE FROM anon_sql_test",
	} {
		rec := apiQuery(write, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("anonymous %q via /api/query: expected 403, got %d; body: %s", write, rec.Code, rec.Body.String())
		}
	}
	if _, err := app.sqlDB.Exec("SELECT 1 FROM anon_sql_test"); err != nil {
		t.Fatalf("anonymous DROP must not have dropped the table: %v", err)
	}
	if n := countRows(t, app, "anon_sql_test"); n != 0 {
		t.Fatalf("anonymous INSERT must not have persisted a row, found %d", n)
	}

	// Anonymous SELECT must still succeed — reads stay open in AuthModeLocal
	// unless require_login_for_reads is turned on (covered separately).
	if rec := apiQuery("SELECT * FROM anon_sql_test", nil); rec.Code != http.StatusOK {
		t.Fatalf("anonymous SELECT via /api/query: expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// The plain HTML form path (POST /query) must enforce the identical gate.
	formReq := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(
		url.Values{"sql": {"INSERT INTO anon_sql_test (id) VALUES (2)"}}.Encode()))
	formReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	formRec := httptest.NewRecorder()
	mux.ServeHTTP(formRec, formReq)
	if !strings.Contains(formRec.Body.String(), readOnlyRoleQueryBlockedMessage) {
		t.Fatalf("anonymous POST /query write should be blocked with the write-blocked message, body: %s", formRec.Body.String())
	}
	if n := countRows(t, app, "anon_sql_test"); n != 0 {
		t.Fatalf("blocked POST /query INSERT must not persist a row, found %d", n)
	}

	// A logged-in write-capable role (User) can write.
	userCookie := sessionCookieForRole(app, "writer", RoleUser)
	if rec := apiQuery("INSERT INTO anon_sql_test (id) VALUES (7)", userCookie); rec.Code != http.StatusOK {
		t.Fatalf("RoleUser INSERT via /api/query: expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if n := countRows(t, app, "anon_sql_test"); n != 1 {
		t.Fatalf("expected the RoleUser INSERT to persist exactly one row, found %d", n)
	}
}

// TestAnonymousSQLEditorWriteAllowedInAuthModeNone confirms the write gate does
// not apply in AuthModeNone — the deliberate loopback-only, single-user mode
// where whoever can reach the process already owns the machine.
func TestAnonymousSQLEditorWriteAllowedInAuthModeNone(t *testing.T) {
	app := newTestApp(t)
	if err := app.applyRuntimeSettings(RuntimeSettings{Dialect: "tinysql", AuthMode: "none"}); err != nil {
		t.Fatalf("apply settings: %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	body, _ := json.Marshal(map[string]string{"sql": "CREATE TABLE anon_none_sql (id INT)"})
	req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous DDL in AuthModeNone: expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if _, err := app.sqlDB.Exec("SELECT 1 FROM anon_none_sql"); err != nil {
		t.Fatalf("expected the table to exist after an anonymous CREATE in AuthModeNone: %v", err)
	}
}

// TestRequireLoginForReadsToggle covers the admin read-privacy toggle: off (the
// default) leaves reads open; on requires an authenticated session for data,
// schema, and catalog routes while any authenticated role may still read.
func TestRequireLoginForReadsToggle(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE read_toggle_test (id INT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	adminCookie := setupAdminSession(t, mux)

	get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	apiSelect := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"sql": "SELECT * FROM read_toggle_test"})
		req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Toggle OFF (default): anonymous reads are open.
	if rec := get("/t/read_toggle_test", nil); rec.Code != http.StatusOK {
		t.Fatalf("toggle off: anonymous table view expected 200, got %d", rec.Code)
	}
	if rec := apiSelect(nil); rec.Code != http.StatusOK {
		t.Fatalf("toggle off: anonymous SELECT expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Turn the toggle on directly on the app (single-goroutine test; the
	// settings mutex is only needed under concurrency).
	app.requireLoginForReads = true

	// Anonymous browser read now bounces to login; the API read returns 401.
	if rec := get("/t/read_toggle_test", nil); rec.Code != http.StatusSeeOther ||
		!strings.Contains(rec.Header().Get("Location"), "/admin/login") {
		t.Fatalf("toggle on: anonymous table view expected redirect to /admin/login, got %d location=%q",
			rec.Code, rec.Header().Get("Location"))
	}
	if rec := apiSelect(nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("toggle on: anonymous SELECT expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if rec := get("/api/schema", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("toggle on: anonymous /api/schema expected 401, got %d", rec.Code)
	}

	// An authenticated session (here Admin) can still read with the toggle on.
	if rec := get("/t/read_toggle_test", adminCookie); rec.Code != http.StatusOK {
		t.Fatalf("toggle on: authenticated table view expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if rec := apiSelect(adminCookie); rec.Code != http.StatusOK {
		t.Fatalf("toggle on: authenticated SELECT expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestAuditSessionRefIsNotTheRawToken guards that the audit log records a
// non-reversible reference to a session rather than the live cookie value,
// which would otherwise let anyone who could read the log replay a session.
func TestAuditSessionRefIsNotTheRawToken(t *testing.T) {
	raw := newSessionID()
	ref := auditSessionRef(raw)
	if ref == "" {
		t.Fatal("expected a non-empty reference for a non-empty session")
	}
	if ref == raw {
		t.Fatal("audit reference must not equal the raw session token")
	}
	if strings.Contains(raw, ref) || strings.Contains(ref, raw) {
		t.Fatalf("audit reference %q must not embed the raw token %q", ref, raw)
	}
	if !strings.HasPrefix(ref, "sha256:") {
		t.Fatalf("expected a sha256: prefixed reference, got %q", ref)
	}
	if got := auditSessionRef(raw); got != ref {
		t.Fatalf("audit reference must be stable for the same input: %q vs %q", got, ref)
	}
	if auditSessionRef("") != "" {
		t.Fatal("a blank session must map to a blank reference so the field stays omitted")
	}
}
