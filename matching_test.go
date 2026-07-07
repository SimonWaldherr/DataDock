package main

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newMatchTablesRequest builds a multipart/form-data POST to /match/tables
// with the given plain fields and file fields (fieldName -> file content),
// mirroring what the "2. Tables" form in match.html submits.
func newMatchTablesRequest(t *testing.T, fields map[string]string, files map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	for fieldName, content := range files {
		fw, err := w.CreateFormFile(fieldName, "upload.csv")
		if err != nil {
			t.Fatalf("create form file %s: %v", fieldName, err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("write file content for %s: %v", fieldName, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/match/tables", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

// TestCanonicalColumnPreservesMeaningfulWhitespace guards against a real
// production bug: a SQL Server export column literally named "Name "
// (trailing space) was being rejected as "not found" because
// canonicalColumn used to trim the requested name before comparing it
// against the schema, silently turning a correct match into a mismatch.
func TestCanonicalColumnPreservesMeaningfulWhitespace(t *testing.T) {
	meta := TableMeta{
		Name: "tmp.gp_deb_2026-07-07",
		Columns: []Column{
			{Name: "Debitor"},
			{Name: "Name "},
			{Name: "Name zusatz (optional)"},
		},
	}
	got, err := canonicalColumn(meta, "Name ")
	if err != nil {
		t.Fatalf("canonicalColumn(%q) returned error: %v", "Name ", err)
	}
	if got != "Name " {
		t.Errorf("canonicalColumn(%q) = %q, want %q", "Name ", got, "Name ")
	}

	// A blank (or whitespace-only) request is still rejected as "nothing
	// selected" rather than accidentally matching "Name " or any other
	// column.
	if _, err := canonicalColumn(meta, "   "); err == nil {
		t.Error("expected an error for a blank column name, got nil")
	}
}

// TestParseMatchFieldSpecsPreservesColumnWhitespace guards the same bug at
// the form-parsing layer: field_source/field_target values are column
// identifiers, not free text, and must round-trip byte-for-byte.
func TestParseMatchFieldSpecsPreservesColumnWhitespace(t *testing.T) {
	form := url.Values{
		"field_source": {"Name "},
		"field_target": {"firma"},
		"field_method": {"token_set"},
		"field_weight": {"1"},
	}
	specs := parseMatchFieldSpecs(form)
	if len(specs) != 1 {
		t.Fatalf("expected 1 field spec, got %d", len(specs))
	}
	if specs[0].SourceColumn != "Name " {
		t.Errorf("SourceColumn = %q, want %q (trailing space preserved)", specs[0].SourceColumn, "Name ")
	}
}

// TestRunMatchRespectsConfigurableRowLimit guards a real production report:
// a 827,082-row customer table was hard-refused by a fixed 200,000-row
// constant. The limit must now come from the admin-configurable
// match_max_rows setting (see settings.go) and the error must name the
// setting so a user hitting it knows how to raise it.
func TestRunMatchRespectsConfigurableRowLimit(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	exec := func(sql string) {
		t.Helper()
		if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup", sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`CREATE TABLE big_src (id INT, name TEXT)`)
	exec(`INSERT INTO big_src (id, name) VALUES (1, 'Alpha')`)
	exec(`INSERT INTO big_src (id, name) VALUES (2, 'Beta')`)
	exec(`INSERT INTO big_src (id, name) VALUES (3, 'Gamma')`)
	exec(`CREATE TABLE tgt (id INT, name TEXT)`)
	exec(`INSERT INTO tgt (id, name) VALUES (1, 'alpha')`)

	req := MatchRequest{
		SourceConnID: "default", TargetConnID: "default",
		SourceTable: "big_src", TargetTable: "tgt",
		SourceKeyColumn: "id", TargetKeyColumn: "id",
		Fields: []MatchFieldSpec{{SourceColumn: "name", TargetColumn: "name", Method: "token_set", Weight: 1}},
	}

	// Default limit (2,000,000) comfortably covers 3 rows.
	if _, err := app.runMatch(ctx, "", req); err != nil {
		t.Fatalf("expected the default limit to allow 3 rows, got error: %v", err)
	}

	// A tiny configured limit must reject the same table, and the error
	// must name the setting so the fix is discoverable without reading the
	// source code.
	app.settingsMu.Lock()
	app.matchMaxRows = 2
	app.settingsMu.Unlock()
	_, err := app.runMatch(ctx, "", req)
	if err == nil {
		t.Fatal("expected an error once the row count exceeds the configured limit")
	}
	if !strings.Contains(err.Error(), "Matching Max Rows") {
		t.Errorf("error %q should mention the Admin Settings field name so the fix is discoverable", err.Error())
	}

	// Raising it back through applyRuntimeSettings (the Admin Settings path)
	// must lift the restriction again.
	settings := app.currentRuntimeSettings()
	settings.MatchMaxRows = 10
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings: %v", err)
	}
	if _, err := app.runMatch(ctx, "", req); err != nil {
		t.Fatalf("expected raising match_max_rows via Admin Settings to fix it, got: %v", err)
	}
}

// TestMatchPageDataSurfacesTableListingError guards a real report: the
// Matching wizard's "Source/Target Table" dropdown went silently empty
// against a real (non-tinySQL) connection, with zero indication whether
// that meant "this connection truly has no tables" or "listing them
// failed". matchPageData must now surface the underlying error instead of
// swallowing it, unlike a.tableNames (used elsewhere, e.g. the sidebar,
// which intentionally stays silent so a flaky connection doesn't break
// unrelated pages).
func TestMatchPageDataSurfacesTableListingError(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	conn, err := OpenManagedConnectionVerbose(ctx, "broken", "Broken SQLite", "sqlite", t.TempDir()+"/broken.db", nil)
	if err != nil {
		t.Fatalf("open managed connection: %v", err)
	}
	if err := app.conns.Add(conn); err != nil {
		t.Fatalf("add connection: %v", err)
	}
	// Simulate a connection that was reachable at add-time (so it passed
	// the initial ping) but has since gone bad — the exact class of
	// failure a "genuinely empty dropdown" can't be told apart from.
	if err := conn.DB.Close(); err != nil {
		t.Fatalf("close underlying db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/match?source=broken&target=default", nil)
	data := app.matchPageData(req, "broken", "default", "", "")

	errMsg, ok := data["SourceTablesError"].(string)
	if !ok || errMsg == "" {
		t.Fatalf("expected SourceTablesError to be set for a connection whose DB is closed, got data[\"SourceTablesError\"] = %#v", data["SourceTablesError"])
	}
	if _, ok := data["SourceTables"]; ok {
		t.Error("SourceTables should not be set when listing them failed")
	}
}

// TestMatchTablesHandlerUploadsSourceKeepsTargetSelection covers the exact
// UX this route was built for: picking "File Upload" for one side must
// import that side's file into a tinySQL table while leaving whatever
// table was already selected on the other side untouched.
func TestMatchTablesHandlerUploadsSourceKeepsTargetSelection(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup", `CREATE TABLE erp2_kunden (kdnr INT, firma TEXT)`); err != nil {
		t.Fatalf("create target table: %v", err)
	}

	req := newMatchTablesRequest(t,
		map[string]string{"source_id": matchUploadSentinel, "target_id": "default", "target_table": "erp2_kunden"},
		map[string]string{"source_file": "id,name\n1,Alpha\n2,Beta\n"},
	)
	rec := httptest.NewRecorder()
	app.matchTablesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "alert-danger") {
		t.Fatalf("expected no error banner, got body containing one: %s", body)
	}
	if !strings.Contains(body, `value="erp2_kunden" selected`) {
		t.Errorf("expected the target table selection (erp2_kunden) to survive the source upload; body:\n%s", body)
	}
	// The uploaded file must have produced a real, queryable tinySQL table.
	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.verify", `SELECT * FROM upload LIMIT 0`); err != nil {
		t.Errorf("expected the uploaded file to have created a table named after it: %v", err)
	}
}

// TestMatchTablesHandlerMissingFileErrorsCleanly guards against a
// half-submitted "File Upload" side (selected but no file chosen yet).
func TestMatchTablesHandlerMissingFileErrorsCleanly(t *testing.T) {
	app := newTestApp(t)
	req := newMatchTablesRequest(t,
		map[string]string{"source_id": matchUploadSentinel, "target_id": "default"},
		nil,
	)
	rec := httptest.NewRecorder()
	app.matchTablesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (the page re-renders with an error), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "choose a file to upload") {
		t.Errorf("expected a clear \"choose a file\" error in the response body")
	}
}

// TestMatchTablesHandlerBlocksUploadInMaintenanceMode guards that only the
// upload branch (a write) is gated by maintenance mode, not a plain
// dropdown resubmit — mirroring how mode=save is gated on POST /match.
func TestMatchTablesHandlerBlocksUploadInMaintenanceMode(t *testing.T) {
	app := newTestApp(t)
	settings := app.currentRuntimeSettings()
	settings.ReadOnlyMode = true
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings: %v", err)
	}

	uploadReq := newMatchTablesRequest(t,
		map[string]string{"source_id": matchUploadSentinel, "target_id": "default"},
		map[string]string{"source_file": "id,name\n1,Alpha\n"},
	)
	rec := httptest.NewRecorder()
	app.matchTablesHandler(rec, uploadReq)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected the upload branch to be blocked in maintenance mode with 503, got %d", rec.Code)
	}

	plainReq := newMatchTablesRequest(t,
		map[string]string{"source_id": "default", "target_id": "default"},
		nil,
	)
	rec = httptest.NewRecorder()
	app.matchTablesHandler(rec, plainReq)
	if rec.Code != http.StatusOK {
		t.Errorf("expected a plain dropdown resubmit to stay available in maintenance mode, got %d", rec.Code)
	}
}
