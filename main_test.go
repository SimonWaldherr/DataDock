package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SimonWaldherr/datadock/internal/standards"
	"github.com/SimonWaldherr/datadock/internal/typed"
	tinysql "github.com/SimonWaldherr/tinySQL"
	tsqldriver "github.com/SimonWaldherr/tinySQL/driver"
)

var testCounter atomic.Int64

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func fakeLLMDiscoveryClient(handler func(*http.Request) (int, string)) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status, body := handler(req)
		if status == 0 {
			status = http.StatusNotFound
			body = `{"error":"not found"}`
		}
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestOpenNativeDBIgnoresStaleWALSidecar(t *testing.T) {
	path := t.TempDir() + "/datadock.db"
	db := tinysql.NewDB()
	if err := tinysql.SaveToFile(db, path); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	if err := os.WriteFile(path+".wal", []byte("stale broken wal"), 0o644); err != nil {
		t.Fatalf("write stale wal: %v", err)
	}

	loaded, err := openNativeDB(path)
	if err != nil {
		t.Fatalf("open native db should ignore stale WAL sidecar: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected loaded database")
	}
	if err := loaded.Close(); err != nil {
		t.Fatalf("close loaded db: %v", err)
	}
}

func TestTinySQLVectorCacheAPI(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	adminCookie := setupAdminSession(t, mux)
	for _, query := range []string{
		"CREATE TABLE vector_cache_test (id INT, embedding VECTOR)",
		"INSERT INTO vector_cache_test VALUES (1, '[1,0]'), (2, '[0,1]')",
		"SELECT id FROM VEC_SEARCH('vector_cache_test', 'embedding', '[1,0]', 1, 'cosine', 'flat')",
		"SELECT id FROM VEC_SEARCH('vector_cache_test', 'embedding', '[1,0]', 1, 'cosine', 'flat')",
	} {
		if result := app.executeTinySQL(context.Background(), query); result.Err != "" {
			t.Fatalf("execute %q: %s", query, result.Err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tinysql/vector-cache", nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("vector cache status = %d: %s", rec.Code, rec.Body.String())
	}
	var stats tinysql.VectorCacheStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode vector cache stats: %v", err)
	}
	if !stats.Enabled || stats.Entries != 1 || stats.Hits != 1 || stats.Misses != 1 || len(stats.RecentQueries) != 2 || !stats.RecentQueries[1].CacheHit {
		t.Fatalf("unexpected vector cache stats: %#v", stats)
	}
}

func TestPortableSnapshotRoundTrip(t *testing.T) {
	app := newTestApp(t)
	if result := app.executeTinySQL(context.Background(), "CREATE TABLE snapshot_rows (id INTEGER NOT NULL, name TEXT DEFAULT 'unknown')"); result.Err != "" {
		t.Fatalf("create snapshot table: %s", result.Err)
	}
	if result := app.executeTinySQL(context.Background(), "INSERT INTO snapshot_rows (id, name) VALUES (1, 'Ada')"); result.Err != "" {
		t.Fatalf("insert snapshot row: %s", result.Err)
	}
	var snapshot bytes.Buffer
	if err := tinysql.SaveToWriter(app.nativeDB, &snapshot); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	restored, err := tinysql.LoadFromReader(bytes.NewReader(snapshot.Bytes()))
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	rs, err := tinysql.ExecSQL(context.Background(), restored, app.tenant, "SELECT name FROM snapshot_rows WHERE id = 1")
	if err != nil || len(rs.Rows) != 1 || rs.Rows[0]["name"] != "Ada" {
		t.Fatalf("snapshot roundtrip = %#v, %v", rs, err)
	}
}

func TestNativeAuditRecordsOriginalStatement(t *testing.T) {
	app := newTestApp(t)
	audit := tinysql.NewAuditLog()
	app.nativeDB.AttachAuditLog(audit)
	const sqlText = "CREATE TABLE audit_statement_text (id INTEGER)"
	if result := app.executeTinySQL(context.Background(), sqlText); result.Err != "" {
		t.Fatalf("execute audited SQL: %s", result.Err)
	}
	entries := audit.Entries()
	if len(entries) != 1 || entries[0].Statement != sqlText {
		t.Fatalf("audit entries = %#v", entries)
	}
	if err := audit.Verify(); err != nil {
		t.Fatalf("verify audit chain: %v", err)
	}
}

func TestStorageEncryptionKeyFromEnv(t *testing.T) {
	t.Setenv("DATADOCK_ENCRYPTION_KEY", strings.Repeat("ab", tinysql.EncryptionKeySize))
	key, err := storageEncryptionKeyFromEnv()
	if err != nil || len(key) != tinysql.EncryptionKeySize {
		t.Fatalf("decode encryption key = %d bytes, %v", len(key), err)
	}
	t.Setenv("DATADOCK_ENCRYPTION_KEY", "not-a-key")
	if _, err := storageEncryptionKeyFromEnv(); err == nil {
		t.Fatal("expected malformed encryption key to fail")
	}
}

func TestEncryptedMemoryStorageIsRejected(t *testing.T) {
	if _, err := openNativeDBWithStorage(":memory:", "memory", bytes.Repeat([]byte{1}, tinysql.EncryptionKeySize)); err == nil {
		t.Fatal("expected encryption without a disk-backed mode to fail")
	}
}

func TestStorageOptionsValidateReadOnlyAndCache(t *testing.T) {
	if _, err := openNativeDBWithStorageOptions(":memory:", "memory", nil, true, 0); err == nil {
		t.Fatal("expected read-only memory storage to be rejected")
	}
	if _, err := openNativeDBWithStorageOptions(t.TempDir(), "paged_index", nil, false, -1); err == nil {
		t.Fatal("expected negative storage cache to be rejected")
	}
}

func TestPagedIndexStorageModeOpens(t *testing.T) {
	db, err := openNativeDBWithStorageOptions(t.TempDir(), "paged_index", nil, false, 4<<20)
	if err != nil {
		t.Fatalf("open paged index storage: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close paged index storage: %v", err)
	}
}

// newTestApp creates a fully isolated App for testing. Each call uses a unique
// tenant name so tests don't interfere through the global driver server.
func newTestApp(t *testing.T) *App {
	t.Helper()

	nativeDB := tinysql.NewDB()
	tenant := "default"
	_ = testCounter.Add(1)

	sqlDB, err := tsqldriver.OpenWithDB(nativeDB)
	if err != nil {
		t.Fatalf("open sql db: %v", err)
	}
	// Force a single connection to avoid pool-reuse across SetDefaultDB calls.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(0)
	t.Cleanup(func() { sqlDB.Close() })

	tpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	return newApp(nativeDB, sqlDB, tenant, tpl)
}

func setupAdminSession(t *testing.T, mux *http.ServeMux) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/setup", strings.NewReader(
		url.Values{"password": {"secret123"}, "password_confirm": {"secret123"}, "next": {"/admin"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("admin setup: expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	return lastSessionCookie(t, rec)
}

// lastSessionCookie returns the last datadock_session cookie in rec's
// response, since a request that both establishes an anonymous session
// (withSession) and then authenticates (rotateSessionOnAuth) sets the
// cookie twice — the second (rotated) value is the one that's actually
// authenticated, and is also what a real browser's cookie jar would end up
// storing for that name.
func lastSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	var last *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			last = c
		}
	}
	if last == nil {
		t.Fatal("response did not set a session cookie")
	}
	return last
}

// TestStyleCSSHasShortCacheControl guards against style.css going back to
// its previous "no-cache" header, which forced a full re-download on every
// single page navigation.
func TestStyleCSSHasShortCacheControl(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/static/style.css", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected style.css to be served, got %d", rec.Code)
	}
	cc := rec.Header().Get("Cache-Control")
	if cc == "" || cc == "no-cache" || !strings.Contains(cc, "max-age") {
		t.Fatalf("expected a positive max-age Cache-Control, got %q", cc)
	}
}

func TestIndexRedirectsToFirstTable(t *testing.T) {
	app := newTestApp(t)

	// Create a table so the index redirects to it.
	if _, err := app.sqlDB.Exec("CREATE TABLE items (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	app.indexHandler(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected 303, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "items") {
		t.Errorf("expected redirect to items table, got %q", loc)
	}
}

func TestIndexNoTables(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	app.indexHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Get Started with DataDock") {
		t.Errorf("expected empty-state message, got:\n%s", w.Body.String())
	}
}

func TestStyleCSSRoute(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/static/style.css", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Fatalf("expected CSS content type, got %q", got)
	}
	if !strings.Contains(w.Body.String(), ":root") || !strings.Contains(w.Body.String(), ".app-nav") {
		t.Fatalf("CSS route returned unexpected content")
	}
}

func TestAboutPageRoute(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/about", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"About DataDock", "Runtime", "Local Browser Data"} {
		if !strings.Contains(body, want) {
			t.Fatalf("about page missing %q: %s", want, body)
		}
	}
}

func TestDemoDataCreatesAllDemoTables(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	adminCookie := setupAdminSession(t, mux)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/demo-data", nil)
		req.AddCookie(adminCookie)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("demo import attempt %d: expected 303, got %d: %s", i+1, w.Code, w.Body.String())
		}
	}

	expectedTables := map[string]int{
		"datadock_demo_departments": 5,
		"datadock_demo_people":      10,
		"datadock_demo_projects":    8,
		"datadock_demo_events":      20,
		"datadock_demo_invoices":    11,
		"datadock_demo_tickets":     13,
		"datadock_demo_customers":   10,
		"datadock_demo_products":    10,
		"datadock_demo_orders":      20,
		"datadock_demo_order_items": 26,
		"datadock_demo_metrics":     60,
		"datadock_demo_locations":   6,
		"datadock_demo_payloads":    3,
	}
	for table, minRows := range expectedTables {
		req := httptest.NewRequest(http.MethodGet, "/t/"+table, nil)
		req.SetPathValue("table", table)
		w := httptest.NewRecorder()
		app.tableViewHandler(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected %s table page 200, got %d: %s", table, w.Code, w.Body.String())
		}
		_, rows, err := app.queryRows(context.Background(), "SELECT * FROM "+table)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if len(rows) < minRows {
			t.Fatalf("expected at least %d rows in %s, got %d", minRows, table, len(rows))
		}
	}

	_, rows, err := app.queryRows(context.Background(), `SELECT project_id, SUM(amount) AS event_total
FROM datadock_demo_events
GROUP BY project_id`)
	if err != nil {
		t.Fatalf("demo aggregate query: %v", err)
	}
	if len(rows) < 8 {
		t.Fatalf("expected aggregate rows for demo projects, got %d", len(rows))
	}
	_, rows, err = app.queryRows(context.Background(), "SELECT name, lon, lat FROM datadock_demo_locations")
	if err != nil {
		t.Fatalf("demo geo query: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("expected 6 demo locations, got %d", len(rows))
	}

	if _, err := app.nativeDB.Catalog().GetJob(demoJobName); err != nil {
		t.Fatalf("expected seeded demo job %q, got error: %v", demoJobName, err)
	}
}

func TestDemoDataRemoveDropsTablesAndJob(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	adminCookie := setupAdminSession(t, mux)

	req := httptest.NewRequest(http.MethodPost, "/demo-data", nil)
	req.AddCookie(adminCookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("demo import: expected 303, got %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/demo-data/remove", nil)
	req.AddCookie(adminCookie)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("demo remove: expected 303, got %d: %s", w.Code, w.Body.String())
	}

	for _, table := range app.tableNames(context.Background()) {
		if strings.HasPrefix(table, "datadock_demo_") {
			t.Fatalf("expected no demo tables left, found %s", table)
		}
	}
	if _, err := app.nativeDB.Catalog().GetJob(demoJobName); err == nil {
		t.Fatalf("expected demo job %q to be removed", demoJobName)
	}
}

// TestCSRFProtectionRejectsCrossSiteWrites guards the defense-in-depth CSRF
// layer added on top of the session cookie's SameSite=Lax: a browser
// request whose Sec-Fetch-Site header says it came from another site must
// be rejected, while a same-origin request (or a header-less non-browser
// request, like curl) must go through unaffected.
func TestCSRFProtectionRejectsCrossSiteWrites(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	handler := app.csrfProtectedHandler(mux)
	if err := app.applyRuntimeSettings(RuntimeSettings{Dialect: "tinysql", AuthMode: "none"}); err != nil {
		t.Fatalf("apply settings: %v", err)
	}

	form := func() *strings.Reader {
		return strings.NewReader(url.Values{"table_name": {"csrf_test"}, "col_name": {"note"}, "col_type": {"TEXT"}}.Encode())
	}

	crossSiteReq := httptest.NewRequest(http.MethodPost, "/create-table", form())
	crossSiteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crossSiteReq.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSiteRec := httptest.NewRecorder()
	handler.ServeHTTP(crossSiteRec, crossSiteReq)
	if crossSiteRec.Code != http.StatusForbidden {
		t.Fatalf("expected a cross-site write to be rejected with 403, got %d: %s", crossSiteRec.Code, crossSiteRec.Body.String())
	}
	if _, err := app.sqlDB.Exec("SELECT 1 FROM csrf_test"); err == nil {
		t.Fatal("expected the cross-site create-table attempt to not actually create the table")
	}

	sameSiteReq := httptest.NewRequest(http.MethodPost, "/create-table", form())
	sameSiteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sameSiteReq.Header.Set("Sec-Fetch-Site", "same-origin")
	sameSiteRec := httptest.NewRecorder()
	handler.ServeHTTP(sameSiteRec, sameSiteReq)
	if sameSiteRec.Code != http.StatusSeeOther {
		t.Fatalf("expected a same-origin write to succeed, got %d: %s", sameSiteRec.Code, sameSiteRec.Body.String())
	}
	if _, err := app.sqlDB.Exec("SELECT 1 FROM csrf_test"); err != nil {
		t.Fatalf("expected the same-origin create-table to have created the table: %v", err)
	}
}

func TestMaintenanceModeBlocksWrites(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	if _, err := app.sqlDB.Exec("CREATE TABLE people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	if err := app.applyRuntimeSettings(RuntimeSettings{Dialect: "tinysql", ReadOnlyMode: true}); err != nil {
		t.Fatalf("enable maintenance mode: %v", err)
	}

	form := url.Values{}
	form.Set("name", "Blocked")
	req := httptest.NewRequest(http.MethodPost, "/t/people/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected record creation to be blocked with 503, got %d: %s", w.Code, w.Body.String())
	}

	insertBody := `{"sql":"INSERT INTO people (id, name) VALUES (1, 'Blocked')"}`
	req = httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(insertBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected /api/query 200 with an error payload, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "maintenance mode") {
		t.Fatalf("expected maintenance-mode error in query result, got: %s", w.Body.String())
	}

	selectBody := `{"sql":"SELECT * FROM people"}`
	req = httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(selectBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("expected read-only SELECT to succeed during maintenance mode, got %d: %s", w.Code, w.Body.String())
	}

	connForm := url.Values{}
	connForm.Set("name", "blocked-conn")
	connForm.Set("kind", "sqlite")
	connForm.Set("dsn", ":memory:")
	req = httptest.NewRequest(http.MethodPost, "/connections", strings.NewReader(connForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected adding a connection to be blocked with 503 during maintenance mode, got %d: %s", w.Code, w.Body.String())
	}

	if err := app.applyRuntimeSettings(RuntimeSettings{Dialect: "tinysql", ReadOnlyMode: false}); err != nil {
		t.Fatalf("disable maintenance mode: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/t/people/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected record creation to succeed after disabling maintenance mode, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMissingTableShowsFriendlyPage(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/t/datadock_demo_events", nil)
	req.SetPathValue("table", "datadock_demo_events")
	w := httptest.NewRecorder()
	app.tableViewHandler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Table or view not found") {
		t.Fatalf("expected friendly missing-object page, got: %s", w.Body.String())
	}
}

func TestTableViewHandler(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO people (id, name) VALUES (1, 'Alice')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/t/people", nil)
	req.SetPathValue("table", "people")
	w := httptest.NewRecorder()
	app.tableViewHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Alice") {
		t.Errorf("expected Alice in table view, got:\n%s", body)
	}
}

func TestCreateTableHandler(t *testing.T) {
	app := newTestApp(t)

	form := url.Values{}
	form.Set("table_name", "products")
	form.Add("col_name", "title")
	form.Add("col_type", "TEXT")
	form.Add("col_name", "price")
	form.Add("col_type", "REAL")

	req := httptest.NewRequest(http.MethodPost, "/create-table",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.createTableHandler(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("expected 303, got %d; body: %s", w.Code, w.Body.String())
	}

	// Verify table was created.
	tables := app.tableNames(context.Background())
	found := false
	for _, n := range tables {
		if n == "products" {
			found = true
		}
	}
	if !found {
		t.Errorf("table 'products' not found in %v", tables)
	}
}

func TestRecordCRUD(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE notes (id INT, body TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	ctx := context.Background()

	// Insert
	meta, err := app.tableMeta(ctx, "notes")
	if err != nil {
		t.Fatalf("tableMeta: %v", err)
	}
	if err := app.insertRecord(ctx, "notes", map[string]string{"body": "hello"}, meta.Columns); err != nil {
		t.Fatalf("insertRecord: %v", err)
	}

	// Read back
	cols, row, err := app.getRecord(ctx, "notes", "1")
	if err != nil {
		t.Fatalf("getRecord: %v", err)
	}
	vals := make(map[string]string, len(cols))
	for i, c := range cols {
		vals[c.Name] = row[i]
	}
	if vals["body"] != "hello" {
		t.Errorf("expected body=hello, got %q", vals["body"])
	}

	// Update
	if err := app.updateRecord(ctx, "notes", "1", map[string]string{"body": "world"}, meta.Columns); err != nil {
		t.Fatalf("updateRecord: %v", err)
	}
	cols2, row2, err := app.getRecord(ctx, "notes", "1")
	if err != nil {
		t.Fatalf("getRecord after update: %v", err)
	}
	vals2 := make(map[string]string, len(cols2))
	for i, c := range cols2 {
		vals2[c.Name] = row2[i]
	}
	if vals2["body"] != "world" {
		t.Errorf("expected body=world after update, got %q", vals2["body"])
	}

	// Delete
	if err := app.deleteRecord(ctx, "notes", "1"); err != nil {
		t.Fatalf("deleteRecord: %v", err)
	}
	meta2, _ := app.tableMeta(ctx, "notes")
	if meta2.RowCount != 0 {
		t.Errorf("expected 0 rows after delete, got %d", meta2.RowCount)
	}
}

func TestQueryEditor(t *testing.T) {
	app := newTestApp(t)

	// GET query editor
	req := httptest.NewRequest(http.MethodGet, "/query", nil)
	w := httptest.NewRecorder()
	app.queryEditorHandler(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "SQL Editor") {
		t.Errorf("expected SQL Editor heading")
	}
	body := w.Body.String()
	// The page's own markup: the JS behind it (Quick Chart rendering, the
	// sample query/prompt lists, Monaco loading, ...) lives in app.js and is
	// covered by TestAppJSContainsPageLogic instead.
	for _, want := range []string{
		"/history",
		"Share",
		"toggleSchemaPreview",
		"Test connection",
		"Excel CSV",
		"GeoJSON",
		"Optimize SQL",
		"Fix query",
		"Review SQL",
		"Analyze quality",
		"Suggest next questions",
		`option value="map"`,
		"initQueryPage",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected query editor to contain %q", want)
		}
	}
}

func TestHistoryPage(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/history", nil)
	w := httptest.NewRecorder()
	app.historyHandler(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Local History", "renderLocalQueryHistory"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected history page to contain %q", want)
		}
	}
}

// TestAppJSContainsPageLogic guards static/app.js itself: every template's
// inline <script> was consolidated into this one file (see README's
// "Front-end JavaScript" section), so this is the test that would catch a
// function accidentally dropped during that move — the per-page HTML tests
// above only check for the thin bootstrap left inline (a data variable or an
// onclick reference), not the function bodies themselves anymore.
func TestAppJSContainsPageLogic(t *testing.T) {
	data, err := webFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read static/app.js: %v", err)
	}
	js := string(data)
	for _, want := range []string{
		// SQL editor (/query)
		"function renderQuickChartControls",
		"function initMonacoEditor",
		"function currentSQL",
		"function renderLLMSQLMetadata",
		"function renderLLMSuggestions",
		"analyze_quality",
		"Locations for Map view",
		"JSON/XML payload tree",
		"Excel CSV edge cases",
		// History (/history)
		"function restoreLocalQueryHistory",
		"function openSQLInEditor",
		"function clearLocalQueryHistory",
		// Connections (/connections)
		"function initConnectionForm",
		"function initLogicSearchBox",
		// Table view (/t/{name})
		"function confirmDrop",
		"function toggleTableDependencies",
		// Admin (/admin)
		"function detectLLMServers",
		// Matching wizard (/match)
		"function addFieldRow",
		// Jobs (/jobs)
		"function createJob",
		// Manage Tables (/create-table, /import, /export)
		"function switchManageTab",
		// Routine view (/r/{name})
		"function copyRoutineDefinition",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("expected static/app.js to contain %q", want)
		}
	}
}

func TestAPISchemaHandler(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/schema", nil)
	w := httptest.NewRecorder()
	app.apiSchemaHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("expected application/json content type, got %q", got)
	}
	body := w.Body.String()
	for _, want := range []string{`"dialect"`, `"tables"`, `"people"`, `"columns"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected schema response to contain %q, got: %s", want, body)
		}
	}
}

func TestAPIAdminStatusHandler(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/status", nil)
	w := httptest.NewRecorder()
	app.apiAdminStatusHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	for _, want := range []string{`"ok":true`, `"storage_mode"`, `"scheduler_running"`, `"tables"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("expected admin status to contain %q, got: %s", want, w.Body.String())
		}
	}
}

func TestAPIAdminSettingsHandler(t *testing.T) {
	app := newTestApp(t)

	getReq := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	getRec := httptest.NewRecorder()
	app.apiAdminSettingsHandler(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected settings GET 200, got %d; body: %s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), `"llm_configured":false`) {
		t.Fatalf("expected LLM disabled by default, got: %s", getRec.Body.String())
	}

	postBody := strings.NewReader(`{"dialect":"postgres","connect_timeout":"3s","query_timeout":"7s","llm_base_url":"http://lmstudio.example:1234/v1","llm_model":"local-model","llm_timeout":"9s"}`)
	postReq := httptest.NewRequest(http.MethodPost, "/api/admin/settings", postBody)
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	app.apiAdminSettingsHandler(postRec, postReq)
	if postRec.Code != http.StatusOK {
		t.Fatalf("expected settings POST 200, got %d; body: %s", postRec.Code, postRec.Body.String())
	}
	if app.currentDialect().Name != "PostgreSQL" || app.currentConnectTimeout() != 3*time.Second || app.currentQueryTimeout() != 7*time.Second {
		t.Fatalf("settings not applied: dialect=%s connect=%s query=%s", app.currentDialect().Name, app.currentConnectTimeout(), app.currentQueryTimeout())
	}
	if app.llmClient() == nil {
		t.Fatalf("expected LLM client after base URL and model were configured")
	}
}

func TestLLMDiscoveryFindsOpenAICompatibleModels(t *testing.T) {
	client := fakeLLMDiscoveryClient(func(r *http.Request) (int, string) {
		if r.URL.Path != "/v1/models" {
			return http.StatusNotFound, `{"error":"not found"}`
		}
		return http.StatusOK, `{"data":[{"id":"mistral-small"},{"id":"qwen2.5-coder"}]}`
	})

	result := discoverLLMServers(context.Background(), client, "http://llm.local", "")
	if len(result.Servers) == 0 {
		t.Fatalf("expected discovered server, got %#v", result)
	}
	if result.Recommended == nil {
		t.Fatalf("expected recommended server, got %#v", result)
	}
	if result.Recommended.BaseURL != "http://llm.local/v1" {
		t.Fatalf("unexpected base URL: %s", result.Recommended.BaseURL)
	}
	if !containsString(result.Recommended.Models, "qwen2.5-coder") {
		t.Fatalf("expected discovered model, got %#v", result.Recommended.Models)
	}
}

func TestLLMDiscoveryFindsOllamaModels(t *testing.T) {
	client := fakeLLMDiscoveryClient(func(r *http.Request) (int, string) {
		if r.URL.Path != "/api/tags" {
			return http.StatusNotFound, `{"error":"not found"}`
		}
		return http.StatusOK, `{"models":[{"name":"llama3.2:latest"},{"model":"codellama:7b"}]}`
	})

	result := discoverLLMServers(context.Background(), client, "http://ollama.local", "")
	if len(result.Servers) == 0 {
		t.Fatalf("expected discovered Ollama server, got %#v", result)
	}
	if !containsString(result.Servers[0].Models, "llama3.2:latest") {
		t.Fatalf("expected Ollama model, got %#v", result.Servers[0].Models)
	}
}

func TestAPILLMAutoConfigAppliesSelectedServer(t *testing.T) {
	app := newTestApp(t)

	body := `{"base_url":"http://llm.local/v1","model":"local-model"}`
	req := httptest.NewRequest(http.MethodPost, "/api/llm/autoconfig", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.apiLLMAutoConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected autoconfig 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if app.llmClient() == nil {
		t.Fatal("expected LLM client after autoconfig")
	}
	settings := app.runtimeSettingsView()
	if settings.LLMBaseURL != "http://llm.local/v1" || settings.LLMModel != "local-model" {
		t.Fatalf("unexpected LLM settings: %#v", settings)
	}
}

func TestRuntimeSettingsPersistInTinySQL(t *testing.T) {
	app := newTestApp(t)
	if err := app.applyRuntimeSettings(RuntimeSettings{
		Dialect:        "mssql",
		ConnectTimeout: 4 * time.Second,
		QueryTimeout:   8 * time.Second,
		LLMBaseURL:     "http://lmstudio.example:1234/v1",
		LLMAPIKey:      "secret-key",
		LLMModel:       "local-model",
		LLMTimeout:     12 * time.Second,
	}); err != nil {
		t.Fatalf("apply settings: %v", err)
	}
	if err := app.saveRuntimeSettings(context.Background()); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	loaded, ok, err := app.loadRuntimeSettings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !ok {
		t.Fatal("expected persisted settings")
	}
	if loaded.Dialect != "Microsoft SQL Server" || loaded.LLMAPIKey != "secret-key" || loaded.LLMTimeout != 12*time.Second {
		t.Fatalf("unexpected loaded settings: %#v", loaded)
	}

	rows, err := app.sqlDB.Query("SELECT setting_key, setting_value FROM " + runtimeSettingsTable)
	if err != nil {
		t.Fatalf("query settings table: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate settings table: %v", err)
	}
	if count == 0 {
		t.Fatal("expected settings rows in tinySQL table")
	}
	for _, name := range app.tableNames(context.Background()) {
		if strings.HasPrefix(name, "__datadock_") {
			t.Fatalf("system table leaked into table browser: %s", name)
		}
	}
}

// TestSnapshotExportWarnsAndAudits guards against treating the full-database
// snapshot export (which includes every stored secret in plaintext) like an
// ordinary, harmless backup: the filename must say so plainly, and the
// export must be audit-logged like a write would be.
func TestSnapshotExportWarnsAndAudits(t *testing.T) {
	app := newTestApp(t)
	auditPath := t.TempDir() + "/audit.log"
	audit, err := NewAuditLogger(auditPath)
	if err != nil {
		t.Fatalf("new audit logger: %v", err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	app.setAuditLogger(audit)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	adminCookie := setupAdminSession(t, mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/snapshot", nil)
	req.AddCookie(adminCookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected snapshot export to succeed, got %d: %s", rec.Code, rec.Body.String())
	}
	if disp := rec.Header().Get("Content-Disposition"); !strings.Contains(disp, "SECRETS") {
		t.Fatalf("expected the snapshot filename to warn about secrets, got %q", disp)
	}

	logged, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if !strings.Contains(string(logged), "snapshot_export") {
		t.Fatalf("expected the snapshot export to be audit-logged, got: %s", logged)
	}
}

func TestAdminSessionCanSeeDataDockSystemTables(t *testing.T) {
	app := newTestApp(t)
	if err := app.saveRuntimeSettings(context.Background()); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	anon := httptest.NewRecorder()
	mux.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "/query", nil))
	if strings.Contains(anon.Body.String(), "__datadock_settings") {
		t.Fatalf("anonymous sidebar leaked DataDock system table: %s", anon.Body.String())
	}

	adminCookie := setupAdminSession(t, mux)
	adminReq := httptest.NewRequest(http.MethodGet, "/query", nil)
	adminReq.AddCookie(adminCookie)
	admin := httptest.NewRecorder()
	mux.ServeHTTP(admin, adminReq)
	if !strings.Contains(admin.Body.String(), "__datadock_settings") {
		t.Fatalf("admin sidebar did not include DataDock system table: %s", admin.Body.String())
	}
}

// TestSystemTableHiddenFromSidebarButQueryableWasABug documents and locks in
// the fix for a real gap: hiding __datadock_-prefixed tables from the
// sidebar/catalog was purely cosmetic and did nothing to stop an ad-hoc
// SELECT against them via /api/query or /api/export, exposing LLM/embedding
// API keys, connection DSNs, and password hashes to any non-admin session.
func TestSystemTableHiddenFromSidebarButQueryableWasABug(t *testing.T) {
	app := newTestApp(t)
	if err := app.applyRuntimeSettings(RuntimeSettings{Dialect: "tinysql", LLMAPIKey: "super-secret-key"}); err != nil {
		t.Fatalf("apply settings: %v", err)
	}
	if err := app.saveRuntimeSettings(context.Background()); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	queryBody := func(sql string) *http.Request {
		body, _ := json.Marshal(map[string]string{"sql": sql})
		req := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	// A non-admin session must not be able to read the settings table
	// (which holds the LLM API key) via an ad-hoc SELECT.
	blockedRec := httptest.NewRecorder()
	mux.ServeHTTP(blockedRec, queryBody("SELECT setting_value FROM __datadock_settings"))
	if strings.Contains(blockedRec.Body.String(), "super-secret-key") {
		t.Fatalf("non-admin session could read a DataDock system table's secret via ad-hoc SQL: %s", blockedRec.Body.String())
	}

	// An ordinary query unrelated to system tables must still work.
	okRec := httptest.NewRecorder()
	mux.ServeHTTP(okRec, queryBody("SELECT 1 AS n"))
	if strings.Contains(okRec.Body.String(), `"error"`) {
		t.Fatalf("expected an ordinary query to still succeed for a non-admin session, got: %s", okRec.Body.String())
	}

	// An authenticated Admin session must still be able to read it.
	adminCookie := setupAdminSession(t, mux)
	adminReq := queryBody("SELECT setting_value FROM __datadock_settings WHERE setting_key = 'llm_api_key'")
	adminReq.AddCookie(adminCookie)
	adminRec := httptest.NewRecorder()
	mux.ServeHTTP(adminRec, adminReq)
	if !strings.Contains(adminRec.Body.String(), "super-secret-key") {
		t.Fatalf("expected an authenticated Admin session to still read the system table, got: %s", adminRec.Body.String())
	}
}

func TestAdminSettingsFormPreservesAPIKey(t *testing.T) {
	app := newTestApp(t)
	if err := app.applyRuntimeSettings(RuntimeSettings{
		Dialect:        "tinysql",
		ConnectTimeout: time.Second,
		QueryTimeout:   2 * time.Second,
		LLMBaseURL:     "http://lmstudio.example:1234/v1",
		LLMAPIKey:      "secret-key",
		LLMModel:       "local-model",
		LLMTimeout:     3 * time.Second,
	}); err != nil {
		t.Fatalf("apply settings: %v", err)
	}
	form := url.Values{}
	form.Set("dialect", "sqlite")
	form.Set("connect_timeout", "4s")
	form.Set("query_timeout", "5s")
	form.Set("llm_base_url", "http://lmstudio.example:1234/v1")
	form.Set("llm_model", "local-model")
	form.Set("llm_timeout", "6s")
	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.adminSettingsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected form 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := app.currentLLMAPIKey(); got != "secret-key" {
		t.Fatalf("expected preserved API key, got %q", got)
	}
}

func TestAdminAndJobsPagesRender(t *testing.T) {
	app := newTestApp(t)

	adminReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	adminRec := httptest.NewRecorder()
	app.adminHandler(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		t.Fatalf("expected admin page 200, got %d; body: %s", adminRec.Code, adminRec.Body.String())
	}
	if !strings.Contains(adminRec.Body.String(), "Status JSON") {
		t.Fatalf("expected admin status content, got: %s", adminRec.Body.String())
	}
	if !strings.Contains(adminRec.Body.String(), "OpenAI-compatible Base URL") {
		t.Fatalf("expected admin settings form, got: %s", adminRec.Body.String())
	}
	for _, want := range []string{"LLM Auto Config", "detectLLMServers", "applyLLMAutoConfig"} {
		if !strings.Contains(adminRec.Body.String(), want) {
			t.Fatalf("expected admin page to contain %q, got: %s", want, adminRec.Body.String())
		}
	}

	jobsReq := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	jobsRec := httptest.NewRecorder()
	app.jobsHandler(jobsRec, jobsReq)
	if jobsRec.Code != http.StatusOK {
		t.Fatalf("expected jobs page 200, got %d; body: %s", jobsRec.Code, jobsRec.Body.String())
	}
	if !strings.Contains(jobsRec.Body.String(), "Registered Jobs") {
		t.Fatalf("expected jobs content, got: %s", jobsRec.Body.String())
	}
}

func TestAdminRoutesRequireAdminSession(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected first admin visit to redirect to setup, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/admin/setup") {
		t.Fatalf("expected setup redirect, got %q", loc)
	}

	apiReq := httptest.NewRequest(http.MethodGet, "/api/admin/status", nil)
	apiRec := httptest.NewRecorder()
	mux.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusPreconditionRequired {
		t.Fatalf("expected API request before setup to return 428, got %d", apiRec.Code)
	}
	if got := apiRec.Header().Get("Content-Type"); !strings.Contains(got, "application/problem+json") {
		t.Fatalf("expected API setup failure to use Problem Details, got %q", got)
	}

	setupReq := httptest.NewRequest(http.MethodPost, "/admin/setup", strings.NewReader(
		url.Values{"password": {"secret123"}, "password_confirm": {"secret123"}, "next": {"/admin"}}.Encode()))
	setupReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setupRec := httptest.NewRecorder()
	mux.ServeHTTP(setupRec, setupReq)
	if setupRec.Code != http.StatusSeeOther {
		t.Fatalf("expected setup submit redirect, got %d; body: %s", setupRec.Code, setupRec.Body.String())
	}
	if app.currentAdminPasswordHash() == "" || strings.Contains(app.currentAdminPasswordHash(), "secret123") {
		t.Fatal("admin password should be stored as a hash")
	}
	sessionCookie := lastSessionCookie(t, setupRec)

	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(sessionCookie)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected admin session request to return 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	noCookieReq := httptest.NewRequest(http.MethodGet, "/api/admin/status", nil)
	noCookieRec := httptest.NewRecorder()
	mux.ServeHTTP(noCookieRec, noCookieReq)
	if noCookieRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected API request without admin session to return 401, got %d", noCookieRec.Code)
	}
}

// TestAuthModeNoneBypassesAdminLogin covers the single-user/local case: with
// auth-mode=none, every request is implicitly an Admin request, and the
// setup/login screens redirect straight into the app instead of asking for
// a password nobody needs.
func TestAuthModeNoneBypassesAdminLogin(t *testing.T) {
	app := newTestApp(t)
	settings := app.currentRuntimeSettings()
	settings.AuthMode = "none"
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings(auth-mode=none): %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	// No cookie, no prior setup — /admin must be reachable directly.
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected /admin to be reachable with no login in auth-mode=none, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// The admin status API must not demand a session either.
	apiReq := httptest.NewRequest(http.MethodGet, "/api/admin/status", nil)
	apiRec := httptest.NewRecorder()
	mux.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("expected /api/admin/status to be reachable with no login in auth-mode=none, got %d", apiRec.Code)
	}

	// Visiting /admin/setup or /admin/login directly redirects away instead
	// of offering a password form that would be misleading in this mode.
	for _, path := range []string{"/admin/setup", "/admin/login"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s: expected a redirect in auth-mode=none, got %d", path, rec.Code)
		}
	}
}

func TestSanitizeAdminNextPath(t *testing.T) {
	cases := map[string]string{
		"":                          "/admin",
		"   ":                       "/admin",
		"https://example.com/admin": "/admin",
		"//example.com/admin":       "/admin",
		"admin":                     "/admin",
		"/admin":                    "/admin",
		"/connections?id=local":     "/connections?id=local",
	}
	for raw, want := range cases {
		if got := sanitizeAdminNextPath(raw); got != want {
			t.Fatalf("sanitizeAdminNextPath(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestLoginRotatesSessionIDPreventingFixation guards against session
// fixation: an attacker who plants a chosen session cookie in a victim's
// browser before the victim logs in must not inherit a fully authenticated
// session once the victim does log in. It also checks that the pre-login
// session's active-connection choice migrates to the new, rotated ID.
func TestLoginRotatesSessionIDPreventingFixation(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	planted := "attacker-planted-session-0000001"
	extConn, err := OpenManagedConnection(context.Background(), "ext-db", "ext", "sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open extra connection: %v", err)
	}
	t.Cleanup(func() { _ = extConn.DB.Close() })
	if err := app.conns.Add(extConn); err != nil {
		t.Fatalf("add extra connection: %v", err)
	}
	if err := app.conns.SetActiveFor(planted, extConn.ID); err != nil {
		t.Fatalf("set active for planted session: %v", err)
	}

	// The victim opens a link with the attacker's planted session cookie
	// already set, then completes first-run Admin setup (equally applicable
	// to a normal /admin/login on an already-configured instance).
	setupReq := httptest.NewRequest(http.MethodPost, "/admin/setup", strings.NewReader(
		url.Values{"password": {"secret123"}, "password_confirm": {"secret123"}, "next": {"/admin"}}.Encode()))
	setupReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setupReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: planted})
	setupRec := httptest.NewRecorder()
	mux.ServeHTTP(setupRec, setupReq)
	if setupRec.Code != http.StatusSeeOther {
		t.Fatalf("expected setup redirect, got %d: %s", setupRec.Code, setupRec.Body.String())
	}

	rotated := lastSessionCookie(t, setupRec)
	if rotated.Value == planted {
		t.Fatal("expected the session ID to be rotated on successful setup, but it was reused")
	}

	// The attacker's planted session ID must NOT have become authenticated.
	if _, _, ok := app.currentSessionUser(planted); ok {
		t.Fatal("expected the planted pre-login session ID to remain unauthenticated after rotation")
	}
	// The new, rotated session ID must be authenticated as the new admin.
	username, role, ok := app.currentSessionUser(rotated.Value)
	if !ok || role != RoleAdmin || username != "admin" {
		t.Fatalf("expected the rotated session to be authenticated as admin, got username=%q role=%q ok=%v", username, role, ok)
	}
	// The pre-login active-connection choice must have migrated to the new ID.
	if got := app.conns.ActiveFor(rotated.Value); got == nil || got.ID != extConn.ID {
		t.Fatalf("expected the rotated session to keep the pre-login active connection %q, got %+v", extConn.ID, got)
	}
}

// TestLoginLockoutAfterRepeatedFailures guards against unlimited online
// password guessing: previously nothing rate-limited /admin/login attempts
// at all, so an attacker could brute-force the well-known default "admin"
// username as fast as bcrypt cost-10 allows, in parallel, with no lockout.
func TestLoginLockoutAfterRepeatedFailures(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	_ = setupAdminSession(t, mux) // creates the "admin" account with password "secret123"

	attempt := func(password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(
			url.Values{"username": {"admin"}, "password": {password}, "next": {"/admin"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < maxLoginFailuresBeforeLockout; i++ {
		rec := attempt("wrong-password")
		if !strings.Contains(rec.Body.String(), "Incorrect password") {
			t.Fatalf("attempt %d: expected an incorrect-password response, got: %s", i, rec.Body.String())
		}
	}

	// The account is now locked out, even with the CORRECT password.
	lockedRec := attempt("secret123")
	if !strings.Contains(lockedRec.Body.String(), "Too many failed attempts") {
		t.Fatalf("expected the account to be locked out after %d failures, got: %s", maxLoginFailuresBeforeLockout, lockedRec.Body.String())
	}

	// Lifting the lockout (simulating loginLockoutDuration elapsing) lets a
	// correct login through again.
	app.loginAttemptsMu.Lock()
	delete(app.loginAttempts, "admin")
	app.loginAttemptsMu.Unlock()
	recoveredRec := attempt("secret123")
	if recoveredRec.Code != http.StatusSeeOther {
		t.Fatalf("expected login to succeed once the lockout is lifted, got %d: %s", recoveredRec.Code, recoveredRec.Body.String())
	}
}

func TestAdminLoginLogoutAndExpiry(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	cookie := setupAdminSession(t, mux)

	logoutReq := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
	logoutReq.AddCookie(cookie)
	logoutRec := httptest.NewRecorder()
	mux.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusSeeOther {
		t.Fatalf("expected logout redirect, got %d", logoutRec.Code)
	}

	adminReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	adminReq.AddCookie(cookie)
	adminRec := httptest.NewRecorder()
	mux.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusSeeOther || !strings.Contains(adminRec.Header().Get("Location"), "/admin/login") {
		t.Fatalf("expected logged-out session to redirect to login, got %d location %q", adminRec.Code, adminRec.Header().Get("Location"))
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(
		url.Values{"password": {"secret123"}, "next": {"https://evil.example/admin"}}.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginReq.AddCookie(cookie)
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusSeeOther {
		t.Fatalf("expected login redirect, got %d; body: %s", loginRec.Code, loginRec.Body.String())
	}
	if got := loginRec.Header().Get("Location"); got != "/admin" {
		t.Fatalf("expected external next target to be sanitized to /admin, got %q", got)
	}

	sessionID := sessionIDFromRequest(loginReq)
	app.adminAuthMu.Lock()
	app.adminAuthedSessions[sessionID] = sessionAuth{Username: "admin", Role: RoleAdmin, Expiry: time.Now().Add(-time.Second)}
	app.adminAuthMu.Unlock()

	expiredReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	expiredReq.AddCookie(cookie)
	expiredRec := httptest.NewRecorder()
	mux.ServeHTTP(expiredRec, expiredReq)
	if expiredRec.Code != http.StatusSeeOther || !strings.Contains(expiredRec.Header().Get("Location"), "/admin/login") {
		t.Fatalf("expected expired admin session to redirect to login, got %d location %q", expiredRec.Code, expiredRec.Header().Get("Location"))
	}
	app.adminAuthMu.Lock()
	_, stillTracked := app.adminAuthedSessions[sessionID]
	app.adminAuthMu.Unlock()
	if stillTracked {
		t.Fatal("expected expired admin session to be removed")
	}
}

func TestAdminChangePasswordUpdatesCurrentUser(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	cookie := setupAdminSession(t, mux)

	req := httptest.NewRequest(http.MethodPost, "/admin/change-password", strings.NewReader(
		url.Values{
			"current_password":     {"secret123"},
			"new_password":         {"secret456"},
			"new_password_confirm": {"secret456"},
		}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected password-change page 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	user, found, err := app.getUserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if !found {
		t.Fatal("expected setup-created admin user to exist")
	}
	if !verifyAdminPassword(user.PasswordHash, "secret456") {
		t.Fatal("expected user password hash to accept the new password")
	}
	if verifyAdminPassword(user.PasswordHash, "secret123") {
		t.Fatal("old password must not keep working for the user account")
	}
}

// sessionCookieForRole creates an authenticated session directly (bypassing
// the HTTP login flow) for a given username/role, mirroring how
// TestAdminLoginLogoutAndExpiry pokes app.adminAuthedSessions directly.
func sessionCookieForRole(app *App, username string, role Role) *http.Cookie {
	sessionID := newSessionID()
	app.markSessionAuthenticated(sessionID, username, role)
	return &http.Cookie{Name: sessionCookieName, Value: sessionID}
}

// TestAnonymousWriteRequiresLoginInAuthModeLocal locks in the fix for a real
// gap: requireWritable alone only ever blocked maintenance mode and an
// already-logged-in RoleReadOnly account — it was never an authentication
// check, so in AuthModeLocal (the default for anything reachable beyond
// localhost) a completely anonymous visitor could still create tables,
// import files, add connections, and edit/delete records with zero
// credentials. requireLogin (composed as
// requireWritable(requireLogin(handler))) now requires some authenticated
// session first.
func TestAnonymousWriteRequiresLoginInAuthModeLocal(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	postCreateTable := func() *httptest.ResponseRecorder {
		form := url.Values{"table_name": {"anon_test"}, "col_name": {"note"}, "col_type": {"TEXT"}}
		req := httptest.NewRequest(http.MethodPost, "/create-table", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// No admin account configured yet: an anonymous write bounces to setup.
	rec := postCreateTable()
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/admin/setup") {
		t.Fatalf("expected anonymous write to redirect to /admin/setup before any account exists, got %d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	adminCookie := setupAdminSession(t, mux)

	// Now that an account exists, an anonymous (no cookie) write bounces to
	// login instead, and must not have created the table.
	rec = postCreateTable()
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/admin/login") {
		t.Fatalf("expected anonymous write to redirect to /admin/login once an account exists, got %d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := app.sqlDB.Exec("SELECT 1 FROM anon_test"); err == nil {
		t.Fatal("expected the anonymous create-table attempt to not actually create the table")
	}

	// A logged-in session (any role) succeeds.
	form := url.Values{"table_name": {"anon_test"}, "col_name": {"note"}, "col_type": {"TEXT"}}
	req := httptest.NewRequest(http.MethodPost, "/create-table", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(adminCookie)
	loggedInRec := httptest.NewRecorder()
	mux.ServeHTTP(loggedInRec, req)
	if loggedInRec.Code != http.StatusSeeOther {
		t.Fatalf("expected a logged-in session to be able to create the table, got %d: %s", loggedInRec.Code, loggedInRec.Body.String())
	}
	if _, err := app.sqlDB.Exec("SELECT 1 FROM anon_test"); err != nil {
		t.Fatalf("expected the table to exist after a logged-in create, got: %v", err)
	}
}

// TestAnonymousWriteAllowedInAuthModeNone confirms AuthModeNone is
// unaffected by the requireLogin fix: it's explicitly the loopback-only,
// single-user mode where whoever can reach the process already has full
// access to the machine, so no login concept should apply there at all.
func TestAnonymousWriteAllowedInAuthModeNone(t *testing.T) {
	app := newTestApp(t)
	if err := app.applyRuntimeSettings(RuntimeSettings{Dialect: "tinysql", AuthMode: "none"}); err != nil {
		t.Fatalf("apply settings: %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	form := url.Values{"table_name": {"anon_none_test"}, "col_name": {"note"}, "col_type": {"TEXT"}}
	req := httptest.NewRequest(http.MethodPost, "/create-table", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected an anonymous write to succeed in AuthModeNone, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := app.sqlDB.Exec("SELECT 1 FROM anon_none_test"); err != nil {
		t.Fatalf("expected the table to exist after an anonymous create in AuthModeNone, got: %v", err)
	}
}

// TestJSONAPIRejectsWrongContentType guards a Content-Type confusion gap:
// JSON API endpoints previously decoded the request body as JSON
// regardless of the actual Content-Type header, so a request sent with a
// "simple" content type a plain cross-site <form> can submit without
// triggering a CORS preflight (e.g. text/plain) would still be parsed and
// acted on.
func TestJSONAPIRejectsWrongContentType(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	body := `{"sql":"SELECT 1"}`

	wrongTypeReq := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	wrongTypeReq.Header.Set("Content-Type", "text/plain")
	wrongTypeRec := httptest.NewRecorder()
	mux.ServeHTTP(wrongTypeRec, wrongTypeReq)
	if wrongTypeRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected a non-JSON Content-Type to be rejected with 415, got %d: %s", wrongTypeRec.Code, wrongTypeRec.Body.String())
	}

	missingTypeReq := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	missingTypeRec := httptest.NewRecorder()
	mux.ServeHTTP(missingTypeRec, missingTypeReq)
	if missingTypeRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected a missing Content-Type to be rejected with 415, got %d: %s", missingTypeRec.Code, missingTypeRec.Body.String())
	}

	correctTypeReq := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	correctTypeReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	correctTypeRec := httptest.NewRecorder()
	mux.ServeHTTP(correctTypeRec, correctTypeReq)
	if correctTypeRec.Code != http.StatusOK {
		t.Fatalf("expected application/json (with charset) to succeed, got %d: %s", correctTypeRec.Code, correctTypeRec.Body.String())
	}
}

func TestReadOnlyRoleBlocksWritesAndNonSelectSQL(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE ro_test (id INT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	roCookie := sessionCookieForRole(app, "readonly-user", RoleReadOnly)

	dropReq := httptest.NewRequest(http.MethodPost, "/drop-table/ro_test", nil)
	dropReq.AddCookie(roCookie)
	dropRec := httptest.NewRecorder()
	mux.ServeHTTP(dropRec, dropReq)
	if dropRec.Code != http.StatusForbidden {
		t.Fatalf("expected readonly role to be blocked from dropping a table with 403, got %d", dropRec.Code)
	}

	insertBody, _ := json.Marshal(map[string]string{"sql": "INSERT INTO ro_test (id) VALUES (2)"})
	insertReq := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(insertBody))
	insertReq.Header.Set("Content-Type", "application/json")
	insertReq.AddCookie(roCookie)
	insertRec := httptest.NewRecorder()
	mux.ServeHTTP(insertRec, insertReq)
	if insertRec.Code != http.StatusForbidden {
		t.Fatalf("expected readonly role to be blocked from INSERT via /api/query with 403, got %d; body: %s", insertRec.Code, insertRec.Body.String())
	}

	selectBody, _ := json.Marshal(map[string]string{"sql": "SELECT * FROM ro_test"})
	selectReq := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewReader(selectBody))
	selectReq.Header.Set("Content-Type", "application/json")
	selectReq.AddCookie(roCookie)
	selectRec := httptest.NewRecorder()
	mux.ServeHTTP(selectRec, selectReq)
	if selectRec.Code != http.StatusOK {
		t.Fatalf("expected readonly role to run SELECT via /api/query, got %d; body: %s", selectRec.Code, selectRec.Body.String())
	}

	// The plain HTML form endpoint (/query) must enforce the identical
	// read-only-role gate as the JSON API (/api/query) — it historically
	// didn't, letting a read-only account run arbitrary writes just by
	// using the form instead of the JS editor.
	formInsertReq := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(
		url.Values{"sql": {"INSERT INTO ro_test (id) VALUES (3)"}}.Encode()))
	formInsertReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	formInsertReq.AddCookie(roCookie)
	formInsertRec := httptest.NewRecorder()
	mux.ServeHTTP(formInsertRec, formInsertReq)
	if strings.Contains(formInsertRec.Body.String(), readOnlyRoleQueryBlockedMessage) == false {
		t.Fatalf("expected POST /query to block a readonly-role write with the read-only message, body: %s", formInsertRec.Body.String())
	}
	if count := countRows(t, app, "ro_test"); count != 0 {
		t.Fatalf("expected the blocked INSERT via POST /query to not persist any row, found %d", count)
	}
}

// countRows returns the row count of table via a direct SELECT COUNT(*),
// bypassing any HTTP-level role gating so tests can assert a write was
// truly rejected rather than just checking the response body.
func countRows(t *testing.T, app *App, table string) int {
	t.Helper()
	result := app.executeSQL(context.Background(), "SELECT COUNT(*) FROM "+table)
	if result.Err != "" {
		t.Fatalf("count rows in %s: %s", table, result.Err)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
		t.Fatalf("unexpected COUNT(*) result shape for %s: %+v", table, result.Rows)
	}
	n, err := strconv.Atoi(result.Rows[0][0])
	if err != nil {
		t.Fatalf("unexpected COUNT(*) value for %s: %q (%v)", table, result.Rows[0][0], err)
	}
	return n
}

// TestReadOnlyRoleBlocksMatchSaveModes covers POST /match's save_config and
// /match/tables' upload branch, both of which are deliberately NOT wrapped
// in requireWritable at the route table level (only mode=save/save_config
// and the upload branch itself need write protection, not a plain dropdown
// resubmit or preview). Historically both replicated only the
// maintenance-mode half of requireWritable's check, letting a RoleReadOnly
// session persist a match configuration despite having no write access.
func TestReadOnlyRoleBlocksMatchSaveModes(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	roCookie := sessionCookieForRole(app, "readonly-user", RoleReadOnly)

	saveConfigReq := httptest.NewRequest(http.MethodPost, "/match", strings.NewReader(
		url.Values{
			"mode":        {"save_config"},
			"config_name": {"ro-should-not-save"},
		}.Encode()))
	saveConfigReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveConfigReq.AddCookie(roCookie)
	saveConfigRec := httptest.NewRecorder()
	mux.ServeHTTP(saveConfigRec, saveConfigReq)
	if saveConfigRec.Code != http.StatusForbidden {
		t.Fatalf("expected readonly role to be blocked from mode=save_config with 403, got %d; body: %s", saveConfigRec.Code, saveConfigRec.Body.String())
	}
	configs, err := app.listMatchConfigs(context.Background())
	if err != nil {
		t.Fatalf("list match configs: %v", err)
	}
	for _, cfg := range configs {
		if cfg.Name == "ro-should-not-save" {
			t.Fatal("expected the blocked save_config to not persist a match configuration")
		}
	}
}

func TestUserRoleCanWriteButNotAdminRoutes(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE user_role_test (id INT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	// requireRole's "no admin account yet" branch would otherwise redirect
	// /admin to /admin/setup before ever reaching the role check below.
	setupAdminSession(t, mux)

	userCookie := sessionCookieForRole(app, "plain-user", RoleUser)

	dropReq := httptest.NewRequest(http.MethodPost, "/drop-table/user_role_test", nil)
	dropReq.AddCookie(userCookie)
	dropRec := httptest.NewRecorder()
	mux.ServeHTTP(dropRec, dropReq)
	if dropRec.Code != http.StatusSeeOther {
		t.Fatalf("expected RoleUser to be able to drop a table, got %d; body: %s", dropRec.Code, dropRec.Body.String())
	}

	adminReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	adminReq.AddCookie(userCookie)
	adminRec := httptest.NewRecorder()
	mux.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusForbidden {
		t.Fatalf("expected RoleUser to be forbidden from /admin, got %d", adminRec.Code)
	}
}

// TestDisablingUserRevokesItsLiveSession guards against a real gap:
// disabling a user only ever updated __datadock_users, never touching an
// already-authenticated session for that account, so a live session kept
// working until its normal 12h sessionAuthTTL expiry regardless of being
// disabled.
func TestDisablingUserRevokesItsLiveSession(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	adminCookie := setupAdminSession(t, mux)

	if err := app.createUser(context.Background(), "compromised", "irrelevant-hash", RoleUser); err != nil {
		t.Fatalf("create user: %v", err)
	}
	liveCookie := sessionCookieForRole(app, "compromised", RoleUser)

	// The live session works before being disabled.
	beforeReq := httptest.NewRequest(http.MethodGet, "/", nil)
	beforeReq.AddCookie(liveCookie)
	beforeRec := httptest.NewRecorder()
	mux.ServeHTTP(beforeRec, beforeReq)
	if beforeRec.Code != http.StatusSeeOther && beforeRec.Code != http.StatusOK {
		t.Fatalf("expected the live session to work before being disabled, got %d", beforeRec.Code)
	}

	disableReq := httptest.NewRequest(http.MethodPost, "/admin/users/disable", strings.NewReader(
		url.Values{"username": {"compromised"}, "disabled": {"true"}}.Encode()))
	disableReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	disableReq.AddCookie(adminCookie)
	disableRec := httptest.NewRecorder()
	mux.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusSeeOther {
		t.Fatalf("expected disable redirect, got %d: %s", disableRec.Code, disableRec.Body.String())
	}

	if _, _, ok := app.currentSessionUser(liveCookie.Value); ok {
		t.Fatal("expected the disabled user's live session to be revoked immediately, not just left to expire")
	}
}

func TestAdminUsersPageCRUDAndGuards(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	adminCookie := setupAdminSession(t, mux)

	// Non-admin roles are forbidden from the Users page.
	roCookie := sessionCookieForRole(app, "someone", RoleReadOnly)
	forbiddenReq := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	forbiddenReq.AddCookie(roCookie)
	forbiddenRec := httptest.NewRecorder()
	mux.ServeHTTP(forbiddenRec, forbiddenReq)
	if forbiddenRec.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin to be forbidden from /admin/users, got %d", forbiddenRec.Code)
	}

	// Create a second user.
	createReq := httptest.NewRequest(http.MethodPost, "/admin/users", strings.NewReader(
		url.Values{"username": {"second"}, "password": {"password123"}, "role": {"user"}}.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.AddCookie(adminCookie)
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusSeeOther {
		t.Fatalf("expected user creation redirect, got %d; body: %s", createRec.Code, createRec.Body.String())
	}
	if _, found, err := app.getUserByUsername(context.Background(), "second"); err != nil || !found {
		t.Fatalf("expected user 'second' to exist, found=%v err=%v", found, err)
	}

	// Change its role to readonly.
	roleReq := httptest.NewRequest(http.MethodPost, "/admin/users/role", strings.NewReader(
		url.Values{"username": {"second"}, "role": {"readonly"}}.Encode()))
	roleReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	roleReq.AddCookie(adminCookie)
	roleRec := httptest.NewRecorder()
	mux.ServeHTTP(roleRec, roleReq)
	if roleRec.Code != http.StatusSeeOther {
		t.Fatalf("expected role-change redirect, got %d; body: %s", roleRec.Code, roleRec.Body.String())
	}
	if u, _, _ := app.getUserByUsername(context.Background(), "second"); u.Role != RoleReadOnly {
		t.Fatalf("expected role readonly, got %q", u.Role)
	}

	// Disable it.
	disableReq := httptest.NewRequest(http.MethodPost, "/admin/users/disable", strings.NewReader(
		url.Values{"username": {"second"}, "disabled": {"true"}}.Encode()))
	disableReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	disableReq.AddCookie(adminCookie)
	disableRec := httptest.NewRecorder()
	mux.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusSeeOther {
		t.Fatalf("expected disable redirect, got %d; body: %s", disableRec.Code, disableRec.Body.String())
	}
	if u, _, _ := app.getUserByUsername(context.Background(), "second"); !u.Disabled {
		t.Fatal("expected user 'second' to be disabled")
	}

	// Delete it.
	deleteReq := httptest.NewRequest(http.MethodPost, "/admin/users/delete", strings.NewReader(
		url.Values{"username": {"second"}}.Encode()))
	deleteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	deleteReq.AddCookie(adminCookie)
	deleteRec := httptest.NewRecorder()
	mux.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusSeeOther {
		t.Fatalf("expected delete redirect, got %d; body: %s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, found, _ := app.getUserByUsername(context.Background(), "second"); found {
		t.Fatal("expected user 'second' to be deleted")
	}

	// The sole remaining admin cannot delete themselves...
	adminUsername, _, _ := app.currentSessionUser(sessionIDFromRequest(adminCookieRequest(adminCookie)))
	selfDeleteReq := httptest.NewRequest(http.MethodPost, "/admin/users/delete", strings.NewReader(
		url.Values{"username": {adminUsername}}.Encode()))
	selfDeleteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	selfDeleteReq.AddCookie(adminCookie)
	selfDeleteRec := httptest.NewRecorder()
	mux.ServeHTTP(selfDeleteRec, selfDeleteReq)
	if selfDeleteRec.Code != http.StatusOK || !strings.Contains(selfDeleteRec.Body.String(), "cannot delete your own account") {
		t.Fatalf("expected a self-delete refusal, got %d; body: %s", selfDeleteRec.Code, selfDeleteRec.Body.String())
	}

	// ...nor demote themselves away from admin, since they're the last one.
	selfDemoteReq := httptest.NewRequest(http.MethodPost, "/admin/users/role", strings.NewReader(
		url.Values{"username": {adminUsername}, "role": {"user"}}.Encode()))
	selfDemoteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	selfDemoteReq.AddCookie(adminCookie)
	selfDemoteRec := httptest.NewRecorder()
	mux.ServeHTTP(selfDemoteRec, selfDemoteReq)
	if selfDemoteRec.Code != http.StatusOK || !strings.Contains(selfDemoteRec.Body.String(), "last remaining admin") {
		t.Fatalf("expected a last-admin refusal, got %d; body: %s", selfDemoteRec.Code, selfDemoteRec.Body.String())
	}
}

// adminCookieRequest wraps a cookie in a bare *http.Request so
// sessionIDFromRequest (which reads from an *http.Request) can extract the
// session ID back out of it in a test.
func adminCookieRequest(cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	return req
}

// TestAdminSetupModeChooser covers Schritt 4: a fresh instance with no
// -auth-mode given shows a "Nur ich / Team" chooser instead of jumping
// straight to account creation; an operator who did pass -auth-mode skips
// it; and the chooser's "Nur ich" action reuses applyRuntimeSettings' own
// loopback-bind safety net rather than duplicating it.
func TestAdminSetupModeChooser(t *testing.T) {
	t.Run("shown when auth-mode was not explicit", func(t *testing.T) {
		app := newTestApp(t)
		mux := http.NewServeMux()
		app.registerRoutes(mux)

		req := httptest.NewRequest(http.MethodGet, "/admin/setup", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="mode" value="none"`) {
			t.Fatalf("expected the mode chooser to render, got %d; body snippet missing chooser form", rec.Code)
		}
	})

	t.Run("skipped when auth-mode was explicit", func(t *testing.T) {
		app := newTestApp(t)
		app.authModeExplicit = true
		mux := http.NewServeMux()
		app.registerRoutes(mux)

		req := httptest.NewRequest(http.MethodGet, "/admin/setup", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `name="mode" value="none"`) {
			t.Fatalf("expected the account-creation form directly (no chooser), got %d", rec.Code)
		}
	})

	t.Run("team=1 bypasses the chooser", func(t *testing.T) {
		app := newTestApp(t)
		mux := http.NewServeMux()
		app.registerRoutes(mux)

		req := httptest.NewRequest(http.MethodGet, "/admin/setup?team=1", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `name="mode" value="none"`) {
			t.Fatalf("expected ?team=1 to bypass the chooser, got %d", rec.Code)
		}
	})

	t.Run("mode=none succeeds on a loopback bind", func(t *testing.T) {
		app := newTestApp(t)
		app.listenAddr = "127.0.0.1:8080"
		mux := http.NewServeMux()
		app.registerRoutes(mux)

		req := httptest.NewRequest(http.MethodPost, "/admin/setup/mode", strings.NewReader(url.Values{"mode": {"none"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("expected mode=none to succeed on a loopback bind, got %d; body: %s", rec.Code, rec.Body.String())
		}
		if app.currentAuthMode() != AuthModeNone {
			t.Fatalf("expected auth mode to become none, got %q", app.currentAuthMode())
		}
	})

	t.Run("mode=none fails on a non-loopback bind without opt-in", func(t *testing.T) {
		app := newTestApp(t)
		app.listenAddr = "0.0.0.0:8080"
		mux := http.NewServeMux()
		app.registerRoutes(mux)

		req := httptest.NewRequest(http.MethodPost, "/admin/setup/mode", strings.NewReader(url.Values{"mode": {"none"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "non-loopback") {
			t.Fatalf("expected the chooser to re-render with the loopback safety error, got %d; body: %s", rec.Code, rec.Body.String())
		}
		if app.currentAuthMode() == AuthModeNone {
			t.Fatal("auth mode must not have switched to none on a refused non-loopback bind")
		}
	})
}

func TestAPIImportHandlerCSV(t *testing.T) {
	app := newTestApp(t)
	body := strings.NewReader(`{"table":"imported_people","format":"csv","content":"id,name\n1,Ada\n2,Grace\n","header_mode":"present","create_table":true,"type_inference":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	app.apiImportHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"rows_inserted":2`) {
		t.Fatalf("expected import result rows, got: %s", w.Body.String())
	}
	cols, rows, err := app.queryRows(context.Background(), "SELECT name FROM imported_people ORDER BY id")
	if err != nil {
		t.Fatalf("query imported table: %v", err)
	}
	if len(cols) != 1 || len(rows) != 2 || rows[0][0] != "Ada" || rows[1][0] != "Grace" {
		t.Fatalf("unexpected imported rows cols=%v rows=%v", cols, rows)
	}
}

func TestMapTileEndpointsServeTileJSONAndPayload(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE city_tiles (record_type TEXT, name TEXT, value TEXT, zoom_level INT, tile_column INT, tile_row INT, tile_size INT, tile_sha256 TEXT, tile_data_base64 TEXT)"); err != nil {
		t.Fatalf("create tile table: %v", err)
	}
	for _, pair := range [][2]string{
		{"datadock:source_format", "mbtiles"},
		{"datadock:tile_scheme", "tms"},
		{"format", "pbf"},
		{"minzoom", "0"},
		{"maxzoom", "0"},
		{"bounds", "13,52,13.1,52.1"},
	} {
		if _, err := app.sqlDB.Exec("INSERT INTO city_tiles (record_type, name, value) VALUES (?, ?, ?)", "metadata", pair[0], pair[1]); err != nil {
			t.Fatalf("insert metadata %q: %v", pair[0], err)
		}
	}
	payload := []byte{0x1a, 0x07, 0x0a, 0x05, 'r', 'o', 'a', 'd', 's'}
	if _, err := app.sqlDB.Exec("INSERT INTO city_tiles (record_type, zoom_level, tile_column, tile_row, tile_size, tile_data_base64) VALUES (?, ?, ?, ?, ?, ?)", "tile", 0, 0, 0, len(payload), base64.StdEncoding.EncodeToString(payload)); err != nil {
		t.Fatalf("insert tile: %v", err)
	}
	tmsPayload := []byte{5, 6, 7}
	if _, err := app.sqlDB.Exec("INSERT INTO city_tiles (record_type, zoom_level, tile_column, tile_row, tile_size, tile_data_base64) VALUES (?, ?, ?, ?, ?, ?)", "tile", 1, 0, 1, len(tmsPayload), base64.StdEncoding.EncodeToString(tmsPayload)); err != nil {
		t.Fatalf("insert TMS tile: %v", err)
	}

	tileJSONReq := httptest.NewRequest(http.MethodGet, "/api/map/tiles/city_tiles/tilejson", nil)
	tileJSONReq.SetPathValue("table", "city_tiles")
	tileJSONRec := httptest.NewRecorder()
	app.apiMapTileJSONHandler(tileJSONRec, tileJSONReq)
	if tileJSONRec.Code != http.StatusOK {
		t.Fatalf("tilejson code = %d; body: %s", tileJSONRec.Code, tileJSONRec.Body.String())
	}
	var tileJSON map[string]any
	if err := json.Unmarshal(tileJSONRec.Body.Bytes(), &tileJSON); err != nil {
		t.Fatalf("decode tilejson: %v", err)
	}
	if tileJSON["scheme"] != "xyz" || tileJSON["format"] != "pbf" {
		t.Fatalf("unexpected TileJSON: %#v", tileJSON)
	}
	if layers, ok := tileJSON["vector_layers"].([]any); !ok || len(layers) != 1 || layers[0].(map[string]any)["id"] != "roads" {
		t.Fatalf("expected vector layers in TileJSON: %#v", tileJSON)
	}

	tileReq := httptest.NewRequest(http.MethodGet, "/api/map/tiles/city_tiles/0/0/0", nil)
	tileReq.SetPathValue("table", "city_tiles")
	tileReq.SetPathValue("z", "0")
	tileReq.SetPathValue("x", "0")
	tileReq.SetPathValue("y", "0")
	tileRec := httptest.NewRecorder()
	app.apiMapTileHandler(tileRec, tileReq)
	if tileRec.Code != http.StatusOK || !bytes.Equal(tileRec.Body.Bytes(), payload) {
		t.Fatalf("tile response = %d %x", tileRec.Code, tileRec.Body.Bytes())
	}
	if got := tileRec.Header().Get("Content-Type"); got != "application/vnd.mapbox-vector-tile" {
		t.Fatalf("tile Content-Type = %q", got)
	}

	tmsReq := httptest.NewRequest(http.MethodGet, "/api/map/tiles/city_tiles/1/0/0", nil)
	tmsReq.SetPathValue("table", "city_tiles")
	tmsReq.SetPathValue("z", "1")
	tmsReq.SetPathValue("x", "0")
	tmsReq.SetPathValue("y", "0")
	tmsRec := httptest.NewRecorder()
	app.apiMapTileHandler(tmsRec, tmsReq)
	if tmsRec.Code != http.StatusOK || !bytes.Equal(tmsRec.Body.Bytes(), tmsPayload) {
		t.Fatalf("TMS-to-XYZ response = %d %x", tmsRec.Code, tmsRec.Body.Bytes())
	}
}

func TestRoutingAPISelectsShortestPathAndReachableNodes(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE route_graph (record_type TEXT, id TEXT, from_id TEXT, to_id TEXT, lat TEXT, lon TEXT, cost TEXT, distance TEXT, geometry TEXT, geometry_type TEXT, properties TEXT)"); err != nil {
		t.Fatalf("create route graph: %v", err)
	}
	statements := []string{
		`INSERT INTO route_graph (record_type, id, lat, lon, properties) VALUES ('node', 'a', '52', '13', '{}')`,
		`INSERT INTO route_graph (record_type, id, lat, lon, properties) VALUES ('node', 'b', '52', '13.01', '{}')`,
		`INSERT INTO route_graph (record_type, id, lat, lon, properties) VALUES ('node', 'c', '52.01', '13', '{}')`,
		`INSERT INTO route_graph (record_type, id, from_id, to_id, cost, distance, geometry, properties) VALUES ('edge', 'ab', 'a', 'b', '1', '100', '{"type":"LineString","coordinates":[[13,52],[13.01,52]]}', '{}')`,
		`INSERT INTO route_graph (record_type, id, from_id, to_id, cost, distance, geometry, properties) VALUES ('edge', 'bc', 'b', 'c', '1', '100', '{"type":"LineString","coordinates":[[13.01,52],[13,52.01]]}', '{}')`,
		`INSERT INTO route_graph (record_type, id, from_id, to_id, cost, distance, geometry, properties) VALUES ('edge', 'ac', 'a', 'c', '5', '300', '{"type":"LineString","coordinates":[[13,52],[13,52.01]]}', '{}')`,
	}
	for _, statement := range statements {
		if _, err := app.sqlDB.Exec(statement); err != nil {
			t.Fatalf("seed graph: %v", err)
		}
	}

	routeReq := httptest.NewRequest(http.MethodPost, "/api/routing/route_graph/route", strings.NewReader(`{"from_id":"a","to_id":"c","cost_field":"cost"}`))
	routeReq.SetPathValue("table", "route_graph")
	routeRec := httptest.NewRecorder()
	app.apiRouteHandler(routeRec, routeReq)
	if routeRec.Code != http.StatusOK {
		t.Fatalf("route code = %d; body: %s", routeRec.Code, routeRec.Body.String())
	}
	var route map[string]any
	if err := json.Unmarshal(routeRec.Body.Bytes(), &route); err != nil {
		t.Fatalf("decode route: %v", err)
	}
	if route["cost"] != float64(2) || route["edge_count"] != float64(2) {
		t.Fatalf("unexpected route: %#v", route)
	}
	if nodes, ok := route["node_ids"].([]any); !ok || len(nodes) != 3 || nodes[1] != "b" {
		t.Fatalf("unexpected route nodes: %#v", route["node_ids"])
	}

	reachableReq := httptest.NewRequest(http.MethodPost, "/api/routing/route_graph/reachable", strings.NewReader(`{"from_id":"a","cost_field":"cost","max_cost":2}`))
	reachableReq.SetPathValue("table", "route_graph")
	reachableRec := httptest.NewRecorder()
	app.apiReachableHandler(reachableRec, reachableReq)
	if reachableRec.Code != http.StatusOK {
		t.Fatalf("reachable code = %d; body: %s", reachableRec.Code, reachableRec.Body.String())
	}
	var reachable map[string]any
	if err := json.Unmarshal(reachableRec.Body.Bytes(), &reachable); err != nil {
		t.Fatalf("decode reachable: %v", err)
	}
	if reachable["reachable_nodes"] != float64(3) {
		t.Fatalf("unexpected reachable response: %#v", reachable)
	}
}

func TestRoutingCostFieldPreservesCustomPropertyCase(t *testing.T) {
	if got := routingCostFieldName("properties.TravelTime"); got != "properties.TravelTime" {
		t.Fatalf("cost field = %q, want properties.TravelTime", got)
	}
	if got := routingCostFieldName("TravelTime"); got != "properties.TravelTime" {
		t.Fatalf("implicit property field = %q, want properties.TravelTime", got)
	}
}

func TestSpatialImportReportCapturesQualityAndProvenance(t *testing.T) {
	app := newTestApp(t)
	content := `{"type":"FeatureCollection","features":[{"type":"Feature","geometry":{"type":"Point","coordinates":[13,52]},"properties":{"name":"valid"}},{"type":"Feature","geometry":{"type":"Point","coordinates":[230,95]},"properties":{"name":"invalid"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(`{"table":"quality_places","format":"geojson","content":`+strconv.Quote(content)+`}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.apiImportHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import code = %d; body: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Report SpatialImportReport `json:"spatial_report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	if response.Report.Status != "warning" || response.Report.ValidGeometryRows != 1 || response.Report.InvalidGeometryRows != 1 || response.Report.SourceFormat != "geojson" || response.Report.SourceSHA256 == "" {
		t.Fatalf("unexpected report: %#v", response.Report)
	}

	reportReq := httptest.NewRequest(http.MethodGet, "/api/spatial-reports/quality_places", nil)
	reportReq.SetPathValue("table", "quality_places")
	reportRec := httptest.NewRecorder()
	app.apiSpatialReportHandler(reportRec, reportReq)
	if reportRec.Code != http.StatusOK || !strings.Contains(reportRec.Body.String(), `"invalid_geometry_rows":1`) {
		t.Fatalf("stored report = %d %s", reportRec.Code, reportRec.Body.String())
	}
}

func TestAPIJobsCreateAndRun(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE vals (id INT, v INT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO vals (id, v) VALUES (1, 42)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	createBody := strings.NewReader(`{"name":"sum_vals","sql":"SELECT SUM(v) AS total FROM vals","schedule_type":"INTERVAL","interval_ms":3600000,"enabled":false}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/jobs", createBody)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	app.apiJobsHandler(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("expected create job 200, got %d; body: %s", createRec.Code, createRec.Body.String())
	}

	runReq := httptest.NewRequest(http.MethodPost, "/api/jobs/run", strings.NewReader(`{"name":"sum_vals"}`))
	runReq.Header.Set("Content-Type", "application/json")
	runRec := httptest.NewRecorder()
	app.apiRunJobHandler(runRec, runReq)
	if runRec.Code != http.StatusOK {
		t.Fatalf("expected run job 200, got %d; body: %s", runRec.Code, runRec.Body.String())
	}
	if !strings.Contains(runRec.Body.String(), `"status":"SUCCEEDED"`) || !strings.Contains(runRec.Body.String(), "42") {
		t.Fatalf("unexpected run response: %s", runRec.Body.String())
	}
	if len(app.nativeDB.Catalog().ListJobHistory()) != 1 {
		t.Fatalf("expected one job history row")
	}
}

func TestAPIQueryHandler(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE vals (id INT, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO vals (id, v) VALUES (1, 42)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	body := strings.NewReader(`{"sql":"SELECT * FROM vals"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/query", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.apiQueryHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "42") {
		t.Errorf("expected 42 in response, got: %s", w.Body.String())
	}
}

func TestAPIQueryHandlerWindow(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE vals (id INT, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := app.sqlDB.Exec("INSERT INTO vals (id, v) VALUES (?, ?)", i, i*10); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	body := strings.NewReader(`{"sql":"SELECT id, v FROM vals ORDER BY id","limit":2}`)
	req := httptest.NewRequest(http.MethodPost, "/api/query", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.apiQueryHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	var got struct {
		Rows           [][]string `json:"rows"`
		Limit          int        `json:"limit"`
		HasMore        bool       `json:"has_more"`
		StatementClass string     `json:"statement_class"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v\n%s", err, w.Body.String())
	}
	if len(got.Rows) != 2 || !got.HasMore || got.Limit != 2 {
		t.Fatalf("unexpected window response: %#v", got)
	}
	if got.StatementClass != "read_query" {
		t.Fatalf("statement class = %q, want read_query", got.StatementClass)
	}
}

func TestTinySQLPragmaAndAgentContext(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE vals (id INT, name TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	pragmaReq := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"sql":"PRAGMA table_info(vals)"}`))
	pragmaReq.Header.Set("Content-Type", "application/json")
	pragmaRec := httptest.NewRecorder()
	app.apiQueryHandler(pragmaRec, pragmaReq)
	if pragmaRec.Code != http.StatusOK {
		t.Fatalf("expected PRAGMA 200, got %d; body: %s", pragmaRec.Code, pragmaRec.Body.String())
	}
	if !strings.Contains(pragmaRec.Body.String(), "read_query") || !strings.Contains(pragmaRec.Body.String(), "name") {
		t.Fatalf("unexpected PRAGMA response: %s", pragmaRec.Body.String())
	}

	callReq := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"sql":"CALL datadock_agent_context(4, 2000)"}`))
	callReq.Header.Set("Content-Type", "application/json")
	callRec := httptest.NewRecorder()
	app.apiQueryHandler(callRec, callReq)
	if callRec.Code != http.StatusOK {
		t.Fatalf("expected CALL 200, got %d; body: %s", callRec.Code, callRec.Body.String())
	}
	if !strings.Contains(callRec.Body.String(), "procedure_call") || !strings.Contains(callRec.Body.String(), "vals") {
		t.Fatalf("unexpected CALL response: %s", callRec.Body.String())
	}

	ctxReq := httptest.NewRequest(http.MethodGet, "/api/tinysql/agent-context?max_tables=4&max_chars=2000", nil)
	ctxRec := httptest.NewRecorder()
	app.apiTinySQLAgentContextHandler(ctxRec, ctxReq)
	if ctxRec.Code != http.StatusOK {
		t.Fatalf("expected agent context 200, got %d; body: %s", ctxRec.Code, ctxRec.Body.String())
	}
	if !strings.Contains(ctxRec.Body.String(), "vals") || !strings.Contains(ctxRec.Body.String(), `"max_tables":4`) {
		t.Fatalf("unexpected agent context response: %s", ctxRec.Body.String())
	}
}

func TestTinySQLV018IndexCatalog(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE indexed_people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := app.sqlDB.Exec("CREATE INDEX idx_indexed_people_name ON indexed_people(name)"); err != nil {
		t.Fatalf("create index: %v", err)
	}

	body := strings.NewReader(`{"sql":"SELECT name, table_name, columns, is_unique FROM sys.indexes WHERE name = 'idx_indexed_people_name'"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/query", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.apiQueryHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected index catalog query 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Rows [][]string `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode index catalog response: %v\n%s", err, rec.Body.String())
	}
	if len(got.Rows) != 1 {
		t.Fatalf("expected one index catalog row, got %#v", got.Rows)
	}
	if got.Rows[0][0] != "idx_indexed_people_name" || got.Rows[0][1] != "indexed_people" || got.Rows[0][2] != "name" || got.Rows[0][3] != "false" {
		t.Fatalf("unexpected index catalog row: %#v", got.Rows[0])
	}

	if _, err := app.sqlDB.Exec("DROP INDEX idx_indexed_people_name"); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	_, rows, err := app.queryRows(context.Background(), "SELECT name FROM sys.indexes WHERE name = 'idx_indexed_people_name'")
	if err != nil {
		t.Fatalf("query dropped index: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected dropped index to disappear from sys.indexes, got %#v", rows)
	}
}

func TestTinySQLV015GeoFunctions(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE places (name TEXT, geometry JSON)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO places VALUES ('Berlin', GEO_POINT(13.4050, 52.5200))`,
		`INSERT INTO places VALUES ('Munich', GEO_POINT(11.5755, 48.1372))`,
	} {
		if _, err := app.sqlDB.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}

	body := strings.NewReader(`{"sql":"SELECT name, ST_X(geometry) AS lon, ST_Y(geometry) AS lat, GEO_DWITHIN(geometry, ST_MakePoint(13.4050, 52.5200), 1000) AS near_berlin, ST_DISTANCE(geometry, ST_MakePoint(11.5755, 48.1372)) AS meters FROM places WHERE GEO_WITHIN_BBOX(geometry, 13.0, 52.0, 14.0, 53.0)"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/query", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.apiQueryHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected geo query 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode geo query response: %v\n%s", err, rec.Body.String())
	}
	if len(got.Rows) != 1 || len(got.Rows[0]) != 5 {
		t.Fatalf("unexpected geo query rows: %#v", got.Rows)
	}
	row := got.Rows[0]
	if row[0] != "Berlin" || row[1] != "13.405" || row[2] != "52.52" || row[3] != "true" {
		t.Fatalf("unexpected geo function values: columns=%#v rows=%#v", got.Columns, got.Rows)
	}
}

func TestTableExportCSVAndJSON(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO people (id, name) VALUES (1, 'Alice')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	csvReq := httptest.NewRequest(http.MethodGet, "/t/people/export?format=csv", nil)
	csvReq.SetPathValue("table", "people")
	csvRec := httptest.NewRecorder()
	app.exportTableHandler(csvRec, csvReq)
	if csvRec.Code != http.StatusOK {
		t.Fatalf("expected CSV export 200, got %d; body: %s", csvRec.Code, csvRec.Body.String())
	}
	if got := csvRec.Header().Get("Content-Type"); !strings.Contains(got, "text/csv") {
		t.Errorf("expected text/csv content type, got %q", got)
	}
	if body := csvRec.Body.String(); !strings.Contains(body, "id") ||
		!strings.Contains(body, "name") ||
		!strings.Contains(body, "1") ||
		!strings.Contains(body, "Alice") {
		t.Errorf("unexpected CSV body: %s", body)
	}

	jsonReq := httptest.NewRequest(http.MethodGet, "/t/people/export?format=json", nil)
	jsonReq.SetPathValue("table", "people")
	jsonRec := httptest.NewRecorder()
	app.exportTableHandler(jsonRec, jsonReq)
	if jsonRec.Code != http.StatusOK {
		t.Fatalf("expected JSON export 200, got %d; body: %s", jsonRec.Code, jsonRec.Body.String())
	}
	if got := jsonRec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("expected application/json content type, got %q", got)
	}
	if body := jsonRec.Body.String(); !strings.Contains(body, `"name":"Alice"`) {
		t.Errorf("unexpected JSON body: %s", body)
	}

	tsvReq := httptest.NewRequest(http.MethodGet, "/t/people/export?format=tsv", nil)
	tsvReq.SetPathValue("table", "people")
	tsvRec := httptest.NewRecorder()
	app.exportTableHandler(tsvRec, tsvReq)
	if tsvRec.Code != http.StatusOK {
		t.Fatalf("expected TSV export 200, got %d; body: %s", tsvRec.Code, tsvRec.Body.String())
	}
	if got := tsvRec.Header().Get("Content-Type"); !strings.Contains(got, "text/tab-separated-values") {
		t.Errorf("expected TSV content type, got %q", got)
	}
	if body := tsvRec.Body.String(); !strings.Contains(body, "id\tname") || !strings.Contains(body, "1\tAlice") {
		t.Errorf("unexpected TSV body: %s", body)
	}

	xmlReq := httptest.NewRequest(http.MethodGet, "/t/people/export?format=xml", nil)
	xmlReq.SetPathValue("table", "people")
	xmlRec := httptest.NewRecorder()
	app.exportTableHandler(xmlRec, xmlReq)
	if xmlRec.Code != http.StatusOK {
		t.Fatalf("expected XML export 200, got %d; body: %s", xmlRec.Code, xmlRec.Body.String())
	}
	if got := xmlRec.Header().Get("Content-Type"); !strings.Contains(got, "application/xml") {
		t.Errorf("expected XML content type, got %q", got)
	}
	if body := xmlRec.Body.String(); !strings.Contains(body, `<cell name="name" type="text">Alice</cell>`) {
		t.Errorf("unexpected XML body: %s", body)
	}

	xlsxReq := httptest.NewRequest(http.MethodGet, "/t/people/export?format=xlsx", nil)
	xlsxReq.SetPathValue("table", "people")
	xlsxRec := httptest.NewRecorder()
	app.exportTableHandler(xlsxRec, xlsxReq)
	if xlsxRec.Code != http.StatusOK {
		t.Fatalf("expected XLSX export 200, got %d; body: %s", xlsxRec.Code, xlsxRec.Body.String())
	}
	if got := xlsxRec.Header().Get("Content-Type"); !strings.Contains(got, "spreadsheetml.sheet") {
		t.Errorf("expected XLSX content type, got %q", got)
	}
	if !xlsxZipContains(t, xlsxRec.Body.Bytes(), "Alice") {
		t.Errorf("expected XLSX export to contain Alice")
	}
}

func TestAPIExportHandler(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE vals (id INT, v INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO vals (id, v) VALUES (1, 42)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	body := strings.NewReader(`{"sql":"SELECT * FROM vals","format":"json"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/export", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.apiExportHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "query.json") {
		t.Errorf("expected query.json content disposition, got %q", got)
	}
	if !strings.Contains(w.Body.String(), `"v":42`) {
		t.Errorf("expected exported value, got: %s", w.Body.String())
	}

	xmlBody := strings.NewReader(`{"sql":"SELECT * FROM vals","format":"xml"}`)
	xmlReq := httptest.NewRequest(http.MethodPost, "/api/export", xmlBody)
	xmlReq.Header.Set("Content-Type", "application/json")
	xmlRec := httptest.NewRecorder()
	app.apiExportHandler(xmlRec, xmlReq)

	if xmlRec.Code != http.StatusOK {
		t.Fatalf("expected XML export 200, got %d; body: %s", xmlRec.Code, xmlRec.Body.String())
	}
	if got := xmlRec.Header().Get("Content-Disposition"); !strings.Contains(got, "query.xml") {
		t.Errorf("expected query.xml content disposition, got %q", got)
	}
	if !strings.Contains(xmlRec.Body.String(), `<cell name="v" type="int">42</cell>`) {
		t.Errorf("expected exported XML value, got: %s", xmlRec.Body.String())
	}

	xlsxBody := strings.NewReader(`{"sql":"SELECT * FROM vals","format":"xlsx"}`)
	xlsxReq := httptest.NewRequest(http.MethodPost, "/api/export", xlsxBody)
	xlsxReq.Header.Set("Content-Type", "application/json")
	xlsxRec := httptest.NewRecorder()
	app.apiExportHandler(xlsxRec, xlsxReq)

	if xlsxRec.Code != http.StatusOK {
		t.Fatalf("expected XLSX export 200, got %d; body: %s", xlsxRec.Code, xlsxRec.Body.String())
	}
	if got := xlsxRec.Header().Get("Content-Disposition"); !strings.Contains(got, "query.xlsx") {
		t.Errorf("expected query.xlsx content disposition, got %q", got)
	}
	if !xlsxZipContains(t, xlsxRec.Body.Bytes(), "42") {
		t.Errorf("expected exported XLSX value")
	}
}

func TestExcelCSVCellKeepsExcelFromGuessingText(t *testing.T) {
	tests := map[string]string{
		"00123":                `="00123"`,
		"2026-07-05T21:25:49Z": `2026-07-05 21:25:49`,
		"=SUM(1,2)":            `="=SUM(1,2)"`,
		"12.5":                 "12.5",
	}

	if got := excelCSVCell("2026-07-05T21:25:49Z", typed.KindDateTime); got != tests["2026-07-05T21:25:49Z"] {
		t.Fatalf("datetime excel cell = %q, want %q", got, tests["2026-07-05T21:25:49Z"])
	}
	if got := excelCSVCell("12.5", typed.KindFloat); got != tests["12.5"] {
		t.Fatalf("float excel cell = %q, want %q", got, tests["12.5"])
	}
	for _, input := range []string{"00123", "=SUM(1,2)"} {
		if got := excelCSVCell(input, typed.KindText); got != tests[input] {
			t.Fatalf("text excel cell %q = %q, want %q", input, got, tests[input])
		}
	}
}

func TestGeoJSONExportFromLonLat(t *testing.T) {
	columns := []string{"name", "lon", "lat"}
	rows := [][]string{{"Munich", "11.5761", "48.1372"}}
	kinds := typed.InferColumns(rows, len(columns))
	fc := buildGeoJSONFeatureCollection(columns, rows, kinds)
	if fc.Type != "FeatureCollection" || len(fc.Features) != 1 {
		t.Fatalf("unexpected feature collection: %#v", fc)
	}
	coords, ok := fc.Features[0].Geometry["coordinates"].([]float64)
	if !ok || len(coords) != 2 || coords[0] != 11.5761 || coords[1] != 48.1372 {
		t.Fatalf("unexpected coordinates: %#v", fc.Features[0].Geometry["coordinates"])
	}
	if got := fc.Features[0].Properties["name"]; got != "Munich" {
		t.Fatalf("unexpected properties: %#v", fc.Features[0].Properties)
	}
}

func TestGeoJSONExportOptionsMapshaperLike(t *testing.T) {
	columns := []string{"geometry", "name", "kind", "drop_me"}
	rows := [][]string{{
		`{"type":"MultiLineString","coordinates":[[[0,0],[0.5,0.001],[1,0]],[[10,10],[11,11]]]}`,
		"Main",
		"road",
		"hidden",
	}}
	kinds := typed.InferColumns(rows, len(columns))

	fc := buildGeoJSONFeatureCollection(columns, rows, kinds)
	transformed := applyGeoJSONExportOptions(fc, exportOptions{
		Explode:           true,
		SimplifyTolerance: 0.01,
		BBox:              &geoBBox{MinX: -1, MinY: -1, MaxX: 2, MaxY: 2},
		Fields:            []string{"name", "kind", "drop_me"},
		DropFields:        []string{"drop_me"},
	})

	if len(transformed.Features) != 1 {
		t.Fatalf("features after explode/bbox = %d, want 1: %#v", len(transformed.Features), transformed.Features)
	}
	if got := transformed.Features[0].Geometry["type"]; got != "LineString" {
		t.Fatalf("geometry type = %v, want LineString", got)
	}
	points := toPointList(transformed.Features[0].Geometry["coordinates"])
	if len(points) != 2 {
		t.Fatalf("simplified points = %d, want 2: %#v", len(points), points)
	}
	props := transformed.Features[0].Properties
	if props["name"] != "Main" || props["kind"] != "road" {
		t.Fatalf("unexpected kept properties: %#v", props)
	}
	if _, ok := props["drop_me"]; ok {
		t.Fatalf("drop_me should have been removed: %#v", props)
	}
}

func TestTableExportMapFormats(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE places (name TEXT, lon TEXT, lat TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO places (name, lon, lat) VALUES ('Munich', 11.5761, 48.1372)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	for _, tc := range []struct {
		format      string
		contentType string
		body        string
	}{
		{"kml", "kml", "<coordinates>11.5761,48.1372,0</coordinates>"},
		{"gpx", "gpx", `<wpt lat="48.1372" lon="11.5761">`},
		{"html", "text/html", "<td>Munich</td>"},
		{"geojson-summary", "application/json", `"Point":1`},
	} {
		req := httptest.NewRequest(http.MethodGet, "/t/places/export?format="+tc.format, nil)
		req.SetPathValue("table", "places")
		rec := httptest.NewRecorder()
		app.exportTableHandler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s export code = %d; body: %s", tc.format, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tc.contentType) {
			t.Fatalf("%s content type = %q, want %q", tc.format, got, tc.contentType)
		}
		if !strings.Contains(rec.Body.String(), tc.body) {
			t.Fatalf("%s body missing %q: %s", tc.format, tc.body, rec.Body.String())
		}
	}
}

func TestTableExportSQLiteAndShapefile(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE places_export (name TEXT, lon TEXT, lat TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO places_export (name, lon, lat) VALUES ('Munich', 11.5761, 48.1372)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sqliteReq := httptest.NewRequest(http.MethodGet, "/t/places_export/export?format=sqlite", nil)
	sqliteReq.SetPathValue("table", "places_export")
	sqliteRec := httptest.NewRecorder()
	app.exportTableHandler(sqliteRec, sqliteReq)
	if sqliteRec.Code != http.StatusOK {
		t.Fatalf("sqlite export code = %d; body: %s", sqliteRec.Code, sqliteRec.Body.String())
	}
	sqlitePath := writeTempTestFile(t, "datadock-export-*.sqlite", sqliteRec.Body.Bytes())
	exportedDB, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		t.Fatalf("open exported sqlite: %v", err)
	}
	defer exportedDB.Close()
	var name string
	if err := exportedDB.QueryRow(`SELECT name FROM places_export`).Scan(&name); err != nil {
		t.Fatalf("query exported sqlite: %v", err)
	}
	if name != "Munich" {
		t.Fatalf("sqlite name = %q, want Munich", name)
	}

	shpReq := httptest.NewRequest(http.MethodGet, "/t/places_export/export?format=shp", nil)
	shpReq.SetPathValue("table", "places_export")
	shpRec := httptest.NewRecorder()
	app.exportTableHandler(shpRec, shpReq)
	if shpRec.Code != http.StatusOK {
		t.Fatalf("shp export code = %d; body: %s", shpRec.Code, shpRec.Body.String())
	}
	if got := shpRec.Header().Get("Content-Type"); !strings.Contains(got, "application/zip") {
		t.Fatalf("shp content type = %q", got)
	}
	for _, name := range []string{"places_export.shp", "places_export.shx", "places_export.dbf"} {
		if !zipContainsFile(t, shpRec.Body.Bytes(), name) {
			t.Fatalf("shapefile zip missing %s", name)
		}
	}
}

func writeTempTestFile(t *testing.T, pattern string, data []byte) string {
	t.Helper()
	file, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	path := file.Name()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		t.Fatalf("write temp file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func zipContainsFile(t *testing.T, data []byte, want string) bool {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	for _, file := range zr.File {
		if file.Name == want {
			return true
		}
	}
	return false
}

func TestViewsAppearAsBrowsableObjects(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE base_people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO base_people (id, name) VALUES (1, 'Ada')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := app.sqlDB.Exec("CREATE VIEW people_view AS SELECT id, name FROM base_people"); err != nil {
		t.Fatalf("create view: %v", err)
	}

	objects := app.tableObjects(context.Background())
	kinds := make(map[string]string, len(objects))
	for _, obj := range objects {
		kinds[obj.Name] = obj.Kind
	}
	if kinds["base_people"] != "table" {
		t.Fatalf("expected base_people table, got objects %#v", objects)
	}
	if kinds["people_view"] != "view" {
		t.Fatalf("expected people_view view, got objects %#v", objects)
	}

	req := httptest.NewRequest(http.MethodGet, "/t/people_view", nil)
	req.SetPathValue("table", "people_view")
	w := httptest.NewRecorder()
	app.tableViewHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected view page 200, got %d; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "people_view") || !strings.Contains(body, "view") || !strings.Contains(body, "Ada") {
		t.Fatalf("expected view page to show view metadata and row, got: %s", body)
	}
}

func TestAPIExportRejectsMutation(t *testing.T) {
	app := newTestApp(t)

	body := strings.NewReader(`{"sql":"CREATE TABLE nope (id INT)","format":"csv"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/export", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.apiExportHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "export requires") {
		t.Errorf("expected export requires error, got: %s", w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != standards.MediaTypeProblemJSON {
		t.Fatalf("expected problem+json, got %q", got)
	}
	if !strings.Contains(w.Body.String(), `"type":"about:blank"`) || !strings.Contains(w.Body.String(), `"status":400`) {
		t.Fatalf("expected RFC 9457 problem response, got: %s", w.Body.String())
	}
}

func TestSafeExportFilenameBase(t *testing.T) {
	tests := map[string]string{
		`people`:          "people",
		`bad"name`:        "bad_name",
		" spaced table ":  "spaced_table",
		"reports-2026.v1": "reports-2026.v1",
		"":                "export",
	}

	for input, want := range tests {
		if got := safeExportFilenameBase(input); got != want {
			t.Errorf("safeExportFilenameBase(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestExportContentDisposition(t *testing.T) {
	tests := []struct {
		name string
		base string
		ext  string
		want string
	}{
		{
			name: "ascii",
			base: "people",
			ext:  "csv",
			want: `attachment; filename="people.csv"`,
		},
		{
			name: "unicode",
			base: "Kunden März",
			ext:  "xlsx",
			want: `attachment; filename="Kunden_M_rz.xlsx"; filename*=UTF-8''Kunden%20M%C3%A4rz.xlsx`,
		},
		{
			name: "unsafe unicode name",
			base: ` ../Kunden: "März" `,
			ext:  ".excel.csv",
			want: `attachment; filename=".._Kunden___M_rz_.excel.csv"; filename*=UTF-8''Kunden_%20_M%C3%A4rz.excel.csv`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exportContentDisposition(tt.base, tt.ext); got != tt.want {
				t.Fatalf("exportContentDisposition(%q, %q) = %q, want %q", tt.base, tt.ext, got, tt.want)
			}
		})
	}
}

func TestImportFormatFromNameMapFormats(t *testing.T) {
	tests := map[string]string{
		"places.geojson":        "geojson",
		"places.gpkg":           "gpkg",
		"track.gpx":             "gpx",
		"warehouse.sqlite":      "sqlite",
		"warehouse.sqlite3":     "sqlite3",
		"warehouse.db":          "db",
		"warehouse.duckdb":      "duckdb",
		"dataset.parquet":       "parquet",
		"dataset.arrow":         "arrow",
		"dataset.feather":       "feather",
		"table.html":            "html",
		"records.msgpack":       "msgpack",
		"records.cbor":          "cbor",
		"records.bson":          "bson",
		"calendar.ics":          "ics",
		"contacts.vcf":          "vcf",
		"route.kml":             "kml",
		"berlin.osm":            "osm",
		"berlin.osm.xml":        "osm",
		"berlin.osm.pbf":        "pbf",
		"extract.pbf":           "pbf",
		"roads.zip":             "shp",
		"tiles.mbtiles":         "mbtiles",
		"network.rg":            "rg",
		"network.graph.json":    "rg",
		"network.routing-graph": "rg",
	}
	for filename, want := range tests {
		if got := importFormatFromName(filename, ""); got != want {
			t.Fatalf("importFormatFromName(%q) = %q, want %q", filename, got, want)
		}
	}
}

func TestMapImportsUseLargerUploadLimit(t *testing.T) {
	if got := importUploadLimit("csv"); got != maxImportUploadBytes {
		t.Fatalf("csv upload limit = %d, want %d", got, maxImportUploadBytes)
	}
	for _, format := range []string{"geojson", "gpkg", "gpx", "kml", "osm", "pbf", "shp", "mbtiles", "rg", "sqlite", "duckdb", "parquet", "arrow", "feather"} {
		if got := importUploadLimit(format); got != maxMapImportUploadBytes {
			t.Fatalf("%s upload limit = %d, want %d", format, got, maxMapImportUploadBytes)
		}
	}
}

func TestXLSXToCSVMergesMultipleSheets(t *testing.T) {
	data := testMultiSheetXLSX(t)
	csvData, err := xlsxToCSV(data)
	if err != nil {
		t.Fatalf("xlsxToCSV: %v", err)
	}
	got := strings.TrimSpace(string(csvData))
	for _, want := range []string{"sheet,name", "sheet1,Ada", "sheet2,Grace"} {
		if !strings.Contains(got, want) {
			t.Fatalf("converted CSV %q does not contain %q", got, want)
		}
	}
}

func TestRecoverMiddlewareReturnsCleanServerError(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panics", nil)

	// The whole point of the middleware: a handler panic must not propagate
	// out and crash the test/server process — recoverMiddleware must catch
	// it and turn it into a normal 500 response.
	recoverMiddleware(panicking).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 after recovered panic, got %d", rec.Code)
	}
}

func TestRecoverMiddlewarePassesThroughNormalResponses(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("teapot"))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	recoverMiddleware(ok).ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot || rec.Body.String() != "teapot" {
		t.Fatalf("expected untouched response to pass through, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestLoggingMiddlewareCapturesActualStatusCode(t *testing.T) {
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)

	// loggingMiddleware must not alter the response itself — only observe
	// it — regardless of what status the wrapped handler sends.
	loggingMiddleware(notFound).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected logging middleware to pass through status 404, got %d", rec.Code)
	}
}

func TestLoggingMiddlewareSkipsHealthzPath(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	loggingMiddleware(next).ServeHTTP(rec, req)

	if !called {
		t.Fatal("expected /healthz request to still reach the wrapped handler")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHealthzHandlerReturnsOK(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	healthzHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("expected body %q, got %q", "ok", rec.Body.String())
	}
}

func xlsxZipContains(t *testing.T, data []byte, want string) bool {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read xlsx zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != "xl/worksheets/sheet1.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open xlsx sheet: %v", err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read xlsx sheet: %v", err)
		}
		return strings.Contains(string(body), want)
	}
	t.Fatalf("xlsx sheet not found")
	return false
}

func testMultiSheetXLSX(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	sheets := map[string]string{
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c r="A1" t="inlineStr"><is><t>name</t></is></c></row><row><c r="A2" t="inlineStr"><is><t>Ada</t></is></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml": `<worksheet><sheetData><row><c r="A1" t="inlineStr"><is><t>name</t></is></c></row><row><c r="A2" t="inlineStr"><is><t>Grace</t></is></c></row></sheetData></worksheet>`,
	}
	for name, body := range sheets {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close xlsx zip: %v", err)
	}
	return buf.Bytes()
}
