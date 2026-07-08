package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

// newTestApp creates a fully isolated App for testing. Each call uses a unique
// tenant name so tests don't interfere through the global driver server.
func newTestApp(t *testing.T) *App {
	t.Helper()

	nativeDB := tinysql.NewDB()
	tsqldriver.SetDefaultDB(nativeDB)
	tenant := fmt.Sprintf("test_%d", testCounter.Add(1))

	sqlDB, err := sql.Open("tinysql", "mem://?tenant="+tenant)
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
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("admin setup did not set a session cookie")
	}
	return cookies[0]
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
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("expected read-only SELECT to succeed during maintenance mode, got %d: %s", w.Code, w.Body.String())
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
	cookies := setupRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected setup response to set a session cookie")
	}

	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(cookies[0])
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
		"route.kml":             "kml",
		"berlin.osm":            "osm",
		"berlin.osm.xml":        "osm",
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
	for _, format := range []string{"geojson", "kml", "osm", "shp", "mbtiles", "rg"} {
		if got := importUploadLimit(format); got != maxMapImportUploadBytes {
			t.Fatalf("%s upload limit = %d, want %d", format, got, maxMapImportUploadBytes)
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
