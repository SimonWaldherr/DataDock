package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	dbimporter "github.com/SimonWaldherr/datadock/internal/importer"
	dbjobs "github.com/SimonWaldherr/datadock/internal/jobs"
	"github.com/SimonWaldherr/datadock/internal/match"
	"github.com/SimonWaldherr/datadock/internal/resultutil"
	"github.com/SimonWaldherr/datadock/internal/sqlutil"
	"github.com/SimonWaldherr/datadock/internal/standards"
	"github.com/SimonWaldherr/datadock/internal/typed"
	tinysql "github.com/SimonWaldherr/tinySQL"
)

// newApp constructs an App value.
func newApp(nativeDB *tinysql.DB, sqlDB *sql.DB, tenant string, tpl *template.Template) *App {
	configureTinySQLVectorCache()
	defaultConn := &DBConnection{
		ID:      defaultConnectionID,
		Name:    "tinySQL",
		Kind:    "tinysql",
		Dialect: DialectProfileForName("tinysql"),
		DB:      sqlDB,
		Native:  nativeDB,
	}
	app := &App{
		nativeDB:       nativeDB,
		sqlDB:          sqlDB,
		tenant:         tenant,
		tpl:            tpl,
		dialect:        DialectProfileForName("tinysql"),
		conns:          NewConnectionManager(defaultConn),
		connectTimeout: 10 * time.Second,
		queryTimeout:   60 * time.Second,
		llmConfig:      LLMConfig{Timeout: 45 * time.Second},
		pageSize:       defaultPageSize,
		matchMaxRows:   defaultMatchMaxRows,
		defaultTheme:   defaultUITheme,
		defaultDensity: defaultUIDensity,
	}
	app.registerTinySQLProcedures()
	return app
}

// registerRoutes wires up all HTTP routes.
func (a *App) registerRoutes(mux *http.ServeMux) {
	handle := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, a.withSession(handler))
	}

	mux.HandleFunc("GET /static/style.css", styleCSSHandler)

	handle("GET /", a.indexHandler)

	// Table datasheet view.
	handle("GET /t/{table}", a.tableViewHandler)
	handle("GET /t/{table}/map", a.mapLayerViewHandler)
	handle("GET /t/{table}/route", a.routingViewHandler)
	handle("GET /t/{table}/quality", a.spatialQualityViewHandler)
	handle("GET /t/{table}/export", a.exportTableHandler)
	handle("GET /api/tables/{table}/script", a.apiTableScriptHandler)

	// Read-only stored procedure/function definition view (catalog tree
	// "Procedures"/"Functions" folders link here instead of /t/{table},
	// since a routine has no rows to browse).
	handle("GET /r/{routine}", a.routineViewHandler)

	// Record CRUD.
	handle("GET /t/{table}/new", a.newRecordFormHandler)
	handle("POST /t/{table}/new", a.requireWritable(a.createRecordHandler))
	handle("GET /t/{table}/{id}/edit", a.editRecordFormHandler)
	handle("POST /t/{table}/{id}/edit", a.requireWritable(a.updateRecordHandler))
	handle("POST /t/{table}/{id}/delete", a.requireWritable(a.deleteRecordHandler))

	// Connections are session-scoped by default; admin login is only needed
	// to persist/share credentials or change the server-wide default.
	handle("GET /connections", a.connectionsHandler)
	handle("POST /connections", a.requireWritable(a.addConnectionHandler))
	handle("POST /connections/active", a.setActiveConnectionHandler)

	// Admin area: gated by an admin password. With none set yet, every
	// requireAdmin-wrapped route bounces to /admin/setup; afterwards it
	// bounces to /admin/login until the session logs in.
	handle("GET /admin", a.requireAdmin(a.adminHandler))
	// Deliberately NOT wrapped in requireWritable: that would let turning on
	// maintenance mode lock the admin out of ever turning it back off again
	// through this same form (every subsequent POST would itself get
	// blocked as a write while maintenance mode is on). requireWritable is
	// for gating everyone else's data edits, not the admin's own settings.
	handle("POST /admin/settings", a.requireAdmin(a.adminSettingsHandler))
	handle("POST /admin/maintenance/toggle", a.requireAdmin(a.adminToggleMaintenanceHandler))
	handle("POST /admin/change-password", a.requireAdmin(a.adminChangePasswordHandler))
	handle("POST /admin/connections/persist", a.requireAdmin(a.requireWritable(a.adminPersistConnectionHandler)))
	handle("POST /admin/connections/forget", a.requireAdmin(a.requireWritable(a.adminForgetConnectionHandler)))
	handle("POST /admin/connections/default", a.requireAdmin(a.requireWritable(a.adminSetDefaultConnectionHandler)))
	// Not requireAdmin: a private connection's owner may reindex it too —
	// see reindexConnectionLogicHandler's own doc comment for the
	// shared-vs-private authorization split it enforces internally.
	handle("POST /connections/reindex-logic", a.requireWritable(a.reindexConnectionLogicHandler))
	handle("POST /admin/logic-search/reindex-all", a.requireAdmin(a.requireWritable(a.adminReindexAllSharedLogicHandler)))
	handle("GET /admin/setup", a.adminSetupHandler)
	handle("POST /admin/setup/mode", a.adminSetupModeHandler)
	handle("POST /admin/setup", a.adminSetupSubmitHandler)
	handle("GET /admin/login", a.adminLoginHandler)
	handle("POST /admin/login", a.adminLoginSubmitHandler)
	handle("POST /admin/logout", a.adminLogoutHandler)
	handle("GET /admin/users", a.requireAdmin(a.adminUsersHandler))
	handle("POST /admin/users", a.requireAdmin(a.requireWritable(a.adminUsersCreateHandler)))
	handle("POST /admin/users/role", a.requireAdmin(a.requireWritable(a.adminUsersRoleHandler)))
	handle("POST /admin/users/disable", a.requireAdmin(a.requireWritable(a.adminUsersDisableHandler)))
	handle("POST /admin/users/reset-password", a.requireAdmin(a.requireWritable(a.adminUsersResetPasswordHandler)))
	handle("POST /admin/users/delete", a.requireAdmin(a.requireWritable(a.adminUsersDeleteHandler)))

	handle("GET /jobs", a.requireAdmin(a.jobsHandler))
	handle("GET /pipelines", a.requireAdmin(a.pipelinesHandler))
	handle("GET /migrate", a.migrationHandler)
	handle("POST /migrate", a.requireWritable(a.runMigrationHandler))
	handle("GET /match", a.matchHandler)
	// Not wrapped in requireWritable: previewing candidates and exporting
	// them as CSV are read-only and must stay available in maintenance
	// mode, like read-only SQL. Only mode=save (which creates/inserts into
	// a real table) is gated, inline, inside runMatchHandler.
	handle("POST /match", a.runMatchHandler)
	// Not wrapped in requireWritable at the route level: a plain table
	// dropdown resubmit is read-only. Only the upload branch (creates a
	// table) is gated, inline, inside matchTablesHandler.
	handle("POST /match/tables", a.matchTablesHandler)
	handle("POST /match/configs/delete", a.requireWritable(a.deleteMatchConfigHandler))
	handle("POST /match/schedules", a.requireWritable(a.saveMatchScheduleHandler))
	handle("GET /create-table", a.createTableFormHandler)
	handle("POST /create-table", a.requireWritable(a.createTableHandler))
	handle("GET /import", a.importFormHandler)
	handle("POST /import", a.requireWritable(a.importFileHandler))
	handle("POST /demo-data", a.requireAdmin(a.requireWritable(a.demoDataHandler)))
	handle("POST /demo-data/remove", a.requireAdmin(a.requireWritable(a.demoDataRemoveHandler)))
	handle("GET /export", a.exportFormHandler)
	handle("GET /history", a.historyHandler)
	handle("GET /guide", a.guideHandler)
	handle("GET /about", a.aboutHandler)
	handle("POST /drop-table/{table}", a.requireWritable(a.dropTableHandler))

	// SQL query editor.
	handle("GET /query", a.queryEditorHandler)
	handle("POST /query", a.queryExecHandler)

	// JSON API used by the query editor for async execution.
	handle("POST /api/query", a.apiQueryHandler)
	handle("POST /api/export", a.apiExportHandler)
	handle("GET /api/schema", a.apiSchemaHandler)
	handle("GET /api/tinysql/agent-context", a.apiTinySQLAgentContextHandler)
	handle("GET /api/tinysql/vector-cache", a.requireAdmin(a.apiTinySQLVectorCacheHandler))
	handle("GET /api/admin/snapshot", a.requireAdmin(a.apiSnapshotExportHandler))
	handle("GET /api/catalog", a.apiCatalogHandler)
	handle("GET /api/catalog/expand", a.apiCatalogExpandHandler)
	handle("POST /api/logic-search", a.apiLogicSearchHandler)
	handle("GET /api/admin/status", a.requireAdmin(a.apiAdminStatusHandler))
	handle("GET /api/admin/settings", a.requireAdmin(a.apiAdminSettingsHandler))
	handle("POST /api/admin/settings", a.requireAdmin(a.apiAdminSettingsHandler))
	handle("GET /api/jobs", a.requireAdmin(a.apiJobsHandler))
	handle("POST /api/jobs", a.requireAdmin(a.requireWritable(a.apiJobsHandler)))
	handle("POST /api/jobs/run", a.requireAdmin(a.requireWritable(a.apiRunJobHandler)))
	handle("GET /api/pipelines", a.requireAdmin(a.apiPipelinesHandler))
	handle("POST /api/pipelines", a.requireAdmin(a.requireWritable(a.apiPipelinesHandler)))
	handle("POST /api/pipelines/run", a.requireAdmin(a.requireWritable(a.apiRunPipelineHandler)))
	handle("POST /api/pipelines/delete", a.requireAdmin(a.requireWritable(a.apiDeletePipelineHandler)))
	handle("GET /api/pipelines/export", a.requireAdmin(a.apiExportPipelineBundleHandler))
	handle("POST /api/pipelines/import", a.requireAdmin(a.requireWritable(a.apiImportPipelineBundleHandler)))
	handle("POST /api/import", a.requireWritable(a.apiImportHandler))
	handle("GET /api/map/tiles/{table}/tilejson", a.apiMapTileJSONHandler)
	handle("GET /api/map/tiles/{table}/{z}/{x}/{y}", a.apiMapTileHandler)
	handle("POST /api/routing/{table}/route", a.apiRouteHandler)
	handle("POST /api/routing/{table}/reachable", a.apiReachableHandler)
	handle("GET /api/spatial-reports/{table}", a.apiSpatialReportHandler)
	handle("POST /api/llm", a.apiLLMHandler)
	handle("POST /api/llm/preview", a.apiLLMPreviewHandler)
	handle("GET /api/llm/discover", a.requireAdmin(a.apiLLMDiscoverHandler))
	handle("POST /api/llm/autoconfig", a.requireAdmin(a.apiLLMAutoConfigHandler))
	handle("GET /api/llm/health", a.apiLLMHealthHandler)
	handle("POST /api/llm/run", a.apiLLMRunHandler)
}

func styleCSSHandler(w http.ResponseWriter, r *http.Request) {
	content, err := webFS.ReadFile("static/style.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(content)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, data map[string]interface{}) {
	adminAuthenticated := a.isAdminAuthenticated(sessionIDFromContext(r.Context()))
	objects := a.tableObjectsWithSystem(r.Context(), adminAuthenticated)
	data["Tables"] = tableObjectNames(objects)
	data["TableObjects"] = objects
	data["DefaultTheme"] = a.currentDefaultTheme()
	data["DefaultDensity"] = a.currentDefaultDensity()
	data["MaintenanceMode"] = a.currentReadOnlyMode()
	if _, ok := data["PageSize"]; !ok {
		data["PageSize"] = a.currentPageSize()
	}
	data["PageSizeOptions"] = pageSizeOptions
	data["AdminAuthenticated"] = adminAuthenticated
	// Unconditional (not just on the "connections"/"admin_users" pages): any
	// handler can re-render those from an error path, and the templates
	// unconditionally index these, so a handler that forgot to set them
	// would otherwise panic instead of showing the error.
	data["LogicSearchConfigured"] = a.embeddingClientFn() != nil
	data["LogicIndexStatus"], data["LogicIndexRunning"] = a.logicIndexStatuses()
	data["Users"], _ = a.listUsers(r.Context())
	data["CurrentUsername"], _, _ = a.currentSessionUser(sessionIDFromContext(r.Context()))
	if a.conns != nil {
		sessionID := sessionIDFromContext(r.Context())
		data["Connections"] = a.conns.ListFor(sessionID)
		active := a.activeConn(r.Context())
		data["ActiveConnection"] = active
		// Managed SQL connections can span multiple databases/schemas/
		// procedures on the server, which is too expensive to walk on every
		// page render; the sidebar fetches /api/catalog asynchronously for
		// those instead of the synchronous flat Tables/Views list used for
		// the embedded tinySQL engine.
		data["CatalogAsync"] = active != nil && !active.IsTinySQL()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) canBrowseTableName(r *http.Request, name string) bool {
	if !isDataDockSystemObject(name) {
		return true
	}
	return a.isAdminAuthenticated(sessionIDFromContext(r.Context()))
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
}

func (a *App) renderObjectMissing(w http.ResponseWriter, r *http.Request, name string, err error) {
	w.WriteHeader(http.StatusNotFound)
	detail := "The table or view may have been dropped, renamed, or is not available on the active connection."
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		detail = err.Error()
	}
	a.render(w, r, "object_missing", map[string]interface{}{
		"ObjectName": name,
		"Detail":     detail,
	})
}

func isMissingDBObjectError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "doesn't exist") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "unknown table") ||
		strings.Contains(msg, "invalid object name")
}

func (a *App) writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", standards.MediaTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) writeProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	standards.WriteProblem(w, standards.NewProblem(status, title, detail, r.URL.Path))
}

func (a *App) writeExport(w http.ResponseWriter, columns []string, rows [][]string, format, filenameBase string, opts ...exportOptions) bool {
	kinds := typed.InferColumns(rows, len(columns))
	exportOpts := exportOptions{}
	if len(opts) > 0 {
		exportOpts = opts[0]
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "csv":
		w.Header().Set("Content-Type", standards.MediaTypeCSV)
		w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "csv"))
		cw := csv.NewWriter(w)
		_ = cw.Write(columns)
		for _, row := range rows {
			_ = cw.Write(row)
		}
		cw.Flush()
		return true
	case "tsv":
		w.Header().Set("Content-Type", standards.MediaTypeTSV)
		w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "tsv"))
		cw := csv.NewWriter(w)
		cw.Comma = '\t'
		_ = cw.Write(columns)
		for _, row := range rows {
			_ = cw.Write(row)
		}
		cw.Flush()
		return true
	case "csv-excel", "excel-csv":
		return writeExcelCSV(w, columns, rows, kinds, filenameBase) == nil
	case "json":
		w.Header().Set("Content-Type", standards.MediaTypeJSON)
		w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "json"))
		records := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			record := make(map[string]any, len(columns))
			for i, col := range columns {
				if i < len(row) {
					record[col] = typed.JSONValue(row[i], kinds[i])
				} else {
					record[col] = nil
				}
			}
			records = append(records, record)
		}
		_ = json.NewEncoder(w).Encode(records)
		return true
	case "ndjson":
		w.Header().Set("Content-Type", standards.MediaTypeNDJSON)
		w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "ndjson"))
		enc := json.NewEncoder(w)
		for _, row := range rows {
			record := make(map[string]any, len(columns))
			for i, col := range columns {
				if i < len(row) {
					record[col] = typed.JSONValue(row[i], kinds[i])
				} else {
					record[col] = nil
				}
			}
			_ = enc.Encode(record)
		}
		return true
	case "xml":
		w.Header().Set("Content-Type", standards.MediaTypeXML)
		w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "xml"))
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(exportXMLDocument(columns, rows, kinds))
		return true
	case "geojson":
		return writeGeoJSONExportWithOptions(w, columns, rows, kinds, filenameBase, exportOpts) == nil
	case "geojson-summary", "geojson-stats":
		return writeGeoJSONSummaryExport(w, columns, rows, kinds, filenameBase, exportOpts) == nil
	case "kml":
		return writeKMLExport(w, columns, rows, kinds, filenameBase, exportOpts) == nil
	case "gpx":
		return writeGPXExport(w, columns, rows, kinds, filenameBase, exportOpts) == nil
	case "shp", "shapefile", "shpzip", "shp.zip":
		return writeShapefileZipExport(w, columns, rows, kinds, filenameBase, exportOpts) == nil
	case "sqlite", "sqlite3", "db":
		return writeSQLiteExport(w, columns, rows, filenameBase) == nil
	case "html", "htm":
		return writeHTMLExport(w, columns, rows, filenameBase) == nil
	case "xlsx":
		w.Header().Set("Content-Type", standards.MediaTypeXLSX)
		w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "xlsx"))
		return writeXLSX(w, columns, rows) == nil
	default:
		return false
	}
}

type exportXMLDoc struct {
	XMLName xml.Name       `xml:"result"`
	Columns []string       `xml:"columns>column"`
	Rows    []exportXMLRow `xml:"rows>row"`
}

type exportXMLRow struct {
	Cells []exportXMLCell `xml:"cell"`
}

type exportXMLCell struct {
	Name  string `xml:"name,attr"`
	Type  string `xml:"type,attr,omitempty"`
	Value string `xml:",chardata"`
}

func exportXMLDocument(columns []string, rows [][]string, kinds []typed.Kind) exportXMLDoc {
	doc := exportXMLDoc{Columns: columns, Rows: make([]exportXMLRow, 0, len(rows))}
	for _, row := range rows {
		out := exportXMLRow{Cells: make([]exportXMLCell, 0, len(columns))}
		for i, col := range columns {
			cell := exportXMLCell{Name: col}
			if i < len(kinds) {
				cell.Type = string(kinds[i])
			}
			if i < len(row) {
				cell.Value = row[i]
			}
			out.Cells = append(out.Cells, cell)
		}
		doc.Rows = append(doc.Rows, out)
	}
	return doc
}

func writeXLSX(w http.ResponseWriter, columns []string, rows [][]string) error {
	zw := zip.NewWriter(w)
	kinds := typed.InferColumns(rows, len(columns))
	files := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
			`</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
			`</Relationships>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
			`</Relationships>`,
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="Result" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/styles.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><numFmts count="3"><numFmt numFmtId="164" formatCode="yyyy-mm-dd"/><numFmt numFmtId="165" formatCode="yyyy-mm-dd hh:mm:ss"/><numFmt numFmtId="166" formatCode="hh:mm:ss"/></numFmts><fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts><fills count="1"><fill><patternFill patternType="none"/></fill></fills><borders count="1"><border/></borders><cellXfs count="4"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/><xf numFmtId="165" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/><xf numFmtId="166" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs></styleSheet>`,
		"xl/worksheets/sheet1.xml": xlsxSheetXML(columns, rows, kinds),
	}
	order := []string{
		"[Content_Types].xml",
		"_rels/.rels",
		"xl/workbook.xml",
		"xl/_rels/workbook.xml.rels",
		"xl/styles.xml",
		"xl/worksheets/sheet1.xml",
	}
	for _, name := range order {
		fw, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := fw.Write([]byte(files[name])); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

func xlsxSheetXML(columns []string, rows [][]string, kinds []typed.Kind) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	writeXLSXRow(&b, 1, columns)
	for i, row := range rows {
		writeXLSXTypedRow(&b, i+2, row, kinds, len(columns))
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

func writeXLSXRow(b *strings.Builder, rowNum int, values []string) {
	b.WriteString(`<row r="`)
	b.WriteString(strconv.Itoa(rowNum))
	b.WriteString(`">`)
	for i, value := range values {
		b.WriteString(`<c r="`)
		b.WriteString(xlsxCellRef(i+1, rowNum))
		b.WriteString(`" t="inlineStr"><is><t>`)
		b.WriteString(xmlText(value))
		b.WriteString(`</t></is></c>`)
	}
	b.WriteString(`</row>`)
}

func writeXLSXTypedRow(b *strings.Builder, rowNum int, values []string, kinds []typed.Kind, columnCount int) {
	b.WriteString(`<row r="`)
	b.WriteString(strconv.Itoa(rowNum))
	b.WriteString(`">`)
	for i := 0; i < columnCount; i++ {
		value := ""
		if i < len(values) {
			value = values[i]
		}
		kind := typed.KindText
		if i < len(kinds) {
			kind = kinds[i]
		}
		writeXLSXTypedCell(b, i+1, rowNum, value, kind)
	}
	b.WriteString(`</row>`)
}

func writeXLSXTypedCell(b *strings.Builder, col, rowNum int, value string, kind typed.Kind) {
	ref := xlsxCellRef(col, rowNum)
	classified := typed.Classify(value)
	if classified.Kind == typed.KindBlank {
		b.WriteString(`<c r="`)
		b.WriteString(ref)
		b.WriteString(`"/>`)
		return
	}
	switch kind {
	case typed.KindInt:
		if classified.Kind == typed.KindInt {
			writeXLSXNumericCell(b, ref, strconv.FormatInt(classified.Int, 10), "")
			return
		}
	case typed.KindFloat:
		if classified.Kind == typed.KindFloat || classified.Kind == typed.KindInt {
			writeXLSXNumericCell(b, ref, strconv.FormatFloat(classified.Float, 'g', -1, 64), "")
			return
		}
	case typed.KindBool:
		if classified.Kind == typed.KindBool {
			b.WriteString(`<c r="`)
			b.WriteString(ref)
			b.WriteString(`" t="b"><v>`)
			if classified.Bool {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
			b.WriteString(`</v></c>`)
			return
		}
	case typed.KindDate, typed.KindDateTime, typed.KindTime:
		if classified.Kind == kind {
			if serial, ok := typed.ExcelSerial(classified, kind); ok {
				style := "1"
				if kind == typed.KindDateTime {
					style = "2"
				} else if kind == typed.KindTime {
					style = "3"
				}
				writeXLSXNumericCell(b, ref, strconv.FormatFloat(serial, 'f', -1, 64), style)
				return
			}
		}
	}
	writeXLSXTextCell(b, ref, value)
}

func writeXLSXNumericCell(b *strings.Builder, ref, value, style string) {
	b.WriteString(`<c r="`)
	b.WriteString(ref)
	if style != "" {
		b.WriteString(`" s="`)
		b.WriteString(style)
	}
	b.WriteString(`"><v>`)
	b.WriteString(value)
	b.WriteString(`</v></c>`)
}

func writeXLSXTextCell(b *strings.Builder, ref, value string) {
	b.WriteString(`<c r="`)
	b.WriteString(ref)
	b.WriteString(`" t="inlineStr"><is><t>`)
	b.WriteString(xmlText(value))
	b.WriteString(`</t></is></c>`)
}

func xlsxCellRef(col, row int) string {
	var letters []byte
	for col > 0 {
		col--
		letters = append([]byte{byte('A' + col%26)}, letters...)
		col /= 26
	}
	return string(letters) + strconv.Itoa(row)
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func safeExportFilenameBase(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '_', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, strings.TrimSpace(name))
	if name == "" {
		return "export"
	}
	return name
}

func exportContentDisposition(filenameBase, extension string) string {
	extension = strings.TrimPrefix(strings.TrimSpace(extension), ".")
	if extension == "" {
		extension = "dat"
	}
	fallback := safeExportFilenameBase(filenameBase) + "." + extension
	utf8Name := unicodeExportFilenameBase(filenameBase) + "." + extension
	if utf8Name == fallback {
		return fmt.Sprintf(`attachment; filename="%s"`, fallback)
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback, url.PathEscape(utf8Name))
}

func unicodeExportFilenameBase(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		default:
			return r
		}
	}, strings.TrimSpace(name))
	name = strings.Trim(name, " ._")
	if name == "" {
		return "export"
	}
	return name
}

func connectionErrorMessage(err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Sprintf("Could not connect within %s. Check host, port, firewall/VPN, DNS, TLS settings, and database credentials.", timeout)
	}
	return "Could not connect: " + err.Error()
}

// ─── route handlers ───────────────────────────────────────────────────────────

// indexHandler redirects to the first table or the query editor.
func (a *App) indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	names := a.tableNames(r.Context())
	if len(names) > 0 {
		http.Redirect(w, r, "/t/"+url.PathEscape(names[0]), http.StatusSeeOther)
		return
	}
	a.render(w, r, "index", map[string]interface{}{})
}

// tableViewHandler renders the datasheet for a table.
func (a *App) tableViewHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		a.renderObjectMissing(w, r, tableName, fmt.Errorf("table %q not found", tableName))
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	sort := r.URL.Query().Get("sort")
	sortDir := r.URL.Query().Get("dir")
	if sortDir != "asc" && sortDir != "desc" {
		sortDir = "asc"
	}
	pageSize := resolvePageSize(r.URL.Query().Get("pagesize"), a.currentPageSize())

	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}

	// Build a sorted query if requested. Pass meta so the function can use the
	// DB-sourced table name and validated column list.
	cols, rows, err := a.tableRowsSorted(r, page, sort, sortDir, pageSize, meta)
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}

	totalPages := 1
	if meta.RowCount > pageSize {
		totalPages = (meta.RowCount + pageSize - 1) / pageSize
	}

	a.render(w, r, "table_view", map[string]interface{}{
		"Table":         meta.Name, // DB-sourced canonical name
		"Meta":          meta,
		"MapLayer":      a.isMapTileTable(r.Context(), meta.Name),
		"RoutingGraph":  a.isRoutingGraphTable(r.Context(), meta.Name),
		"SpatialReport": a.hasSpatialImportReport(r.Context(), meta.Name),
		"Cols":          cols,
		"Rows":          rows,
		"Page":          page,
		"TotalPages":    totalPages,
		"Sort":          sort,
		"SortDir":       sortDir,
		"PageSize":      pageSize,
	})
}

// routineViewHandler shows the read-only source of a stored procedure or
// function on the active connection. Unlike tables/views, a routine has no
// rows to page through, so it gets its own minimal template instead of
// table_view.html. ?kind=function selects function lookup semantics
// (matters for MySQL/MariaDB's SHOW CREATE syntax); anything else is
// treated as a procedure.
func (a *App) routineViewHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("routine")
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "function" {
		kind = "procedure"
	}
	conn := a.activeConn(r.Context())
	if conn == nil {
		a.renderObjectMissing(w, r, name, fmt.Errorf("no active connection"))
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	definition, err := conn.fetchRoutineDefinition(ctx, name, kind)
	if err != nil {
		a.renderObjectMissing(w, r, name, err)
		return
	}
	data := map[string]interface{}{
		"RoutineName": name,
		"RoutineKind": kind,
		"Definition":  definition,
	}
	if dependsOn, dependents, err := conn.fetchDependencies(ctx, name, kind); err != nil {
		data["DependenciesError"] = err.Error()
	} else {
		data["DependsOn"] = dependsOn
		data["Dependents"] = dependents
	}
	a.render(w, r, "routine_view", data)
}

// pageSizeOptions is the shared list of selectable rows-per-page values,
// offered identically by the table view and the SQL editor's result pager so
// pagination feels the same throughout the app.
var pageSizeOptions = []int{25, 50, 100, 250, 500, 1000}

// resolvePageSize parses an optional "pagesize" query/JSON value, clamping it
// to a sane range and falling back to fallback when raw is empty or invalid.
func resolvePageSize(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	if n > 1000 {
		return 1000
	}
	return n
}

// apiTableScriptHandler returns ready-to-run SQL snippets for a table/view
// (Select Top 1000 Rows / Script as INSERT / Script as UPDATE), used by the
// sidebar's and table view's quick-action buttons.
func (a *App) apiTableScriptHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		a.writeProblem(w, r, http.StatusNotFound, "Not found", fmt.Sprintf("table %q not found", tableName))
		return
	}
	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		if isMissingDBObjectError(err) {
			a.writeProblem(w, r, http.StatusNotFound, "Not found", err.Error())
			return
		}
		a.serverError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, a.buildTableScript(r.Context(), meta))
}

// exportTableHandler streams a full table export as CSV or JSON.
func (a *App) exportTableHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		a.renderObjectMissing(w, r, tableName, fmt.Errorf("table %q not found", tableName))
		return
	}
	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		a.renderObjectMissing(w, r, tableName, err)
		return
	}

	conn := a.activeConn(r.Context())
	cols, rows, err := a.queryRows(r.Context(), selectAllQuery(conn, meta.Name))
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}
	opts, err := exportOptionsFromValues(r.URL.Query())
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid export option", err.Error())
		return
	}
	if ok := a.writeExport(w, cols, rows, r.URL.Query().Get("format"), meta.Name, opts); !ok {
		http.Error(w, "unsupported export format", http.StatusBadRequest)
	}
}

func (a *App) connectionsHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "connections", map[string]interface{}{"Form": defaultConnectionForm()})
}

// logicIndexStatuses snapshots every connection's last reindex report and
// whether one is currently running, for the connections page to render
// without exposing a.logicIndexMu to templates.
func (a *App) logicIndexStatuses() (map[string]LogicIndexReport, map[string]bool) {
	a.logicIndexMu.Lock()
	defer a.logicIndexMu.Unlock()
	report := make(map[string]LogicIndexReport, len(a.logicIndexStatus))
	for k, v := range a.logicIndexStatus {
		report[k] = v
	}
	running := make(map[string]bool, len(a.logicIndexing))
	for k, v := range a.logicIndexing {
		running[k] = v
	}
	return report, running
}

// reindexConnectionLogicHandler triggers a background SQL-logic reindex
// (see logic_search.go) for one connection. Deliberately NOT admin-only:
// each user may add their own private connection (see ConnectionManager's
// Owner field), and only that user can ever see or use it, so only that
// user can meaningfully benefit from — or reasonably be trusted to trigger —
// reindexing it. A SHARED connection (visible to everyone) is different:
// reindexing it affects the embedding index every user's prompts draw on
// and can make many billed embedding-API calls, so that still requires an
// admin. searchObjectLogic/GetFor already guarantee a non-owning, non-admin
// session can't even resolve a connection it doesn't own, but the ownership
// check below is what stops it from being reindexed by someone who merely
// happens to share the server.
func (a *App) reindexConnectionLogicHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("id")
	sessionID := sessionIDFromContext(r.Context())
	conn := a.conns.GetFor(sessionID, id)
	if conn == nil {
		a.render(w, r, "connections", map[string]interface{}{"Error": fmt.Sprintf("connection %q not found", id)})
		return
	}
	if conn.Owner == "" && !a.isAdminAuthenticated(sessionID) {
		a.render(w, r, "connections", map[string]interface{}{"Error": "Only an admin can reindex a shared connection."})
		return
	}
	if err := a.startReindexConnectionLogic(sessionID, id); err != nil {
		a.render(w, r, "connections", map[string]interface{}{"Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/connections", http.StatusSeeOther)
}

// adminReindexAllSharedLogicHandler is the Admin Settings "reindex
// everything" counterpart to the per-connection button above: it starts a
// background reindex for every SHARED, dialect-supported connection in one
// click, for an admin who'd rather refresh the whole server-wide index than
// click through each connection individually. Private, per-user
// connections aren't included — each owner reindexes their own from the
// Connections page.
func (a *App) adminReindexAllSharedLogicHandler(w http.ResponseWriter, r *http.Request) {
	var started, errs []string
	for _, info := range a.conns.List() {
		conn := a.conns.Get(info.ID)
		if conn == nil || !logicSearchSupported(conn) {
			continue
		}
		if err := a.startReindexConnectionLogic("", info.ID); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", info.ID, err))
			continue
		}
		started = append(started, info.ID)
	}
	data := a.adminPageData(r.Context(), nil)
	switch {
	case len(started) == 0 && len(errs) == 0:
		data["Error"] = "No shared connections are eligible for SQL-logic reindexing (PostgreSQL, MySQL/MariaDB, or Microsoft SQL Server only)."
	case len(started) == 0:
		data["Error"] = "Could not start reindexing: " + strings.Join(errs, "; ")
	default:
		msg := fmt.Sprintf("Started reindexing %d connection(s): %s.", len(started), strings.Join(started, ", "))
		if len(errs) > 0 {
			msg += " Skipped: " + strings.Join(errs, "; ")
		}
		data["Success"] = msg
	}
	a.render(w, r, "admin", data)
}

// apiLogicSearchHandler answers a semantic search over one connection's
// indexed view/procedure/function definitions (see logic_search.go). Not
// admin-gated (it's read-only, mirroring /api/catalog), but
// searchObjectLogic resolves connection_id via this session's ID, so a
// session can never search a connection it can't already see.
func (a *App) apiLogicSearchHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ConnectionID string `json:"connection_id"`
		Query        string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	sessionID := sessionIDFromContext(r.Context())
	hits, err := a.searchObjectLogic(ctx, sessionID, strings.TrimSpace(body.ConnectionID), strings.TrimSpace(body.Query), maxLLMLogicSearchHits)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Search failed", err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

// defaultConnectionForm seeds the "Add Managed Connection" form with sane
// defaults on first load (before any submission/failure has populated it).
func defaultConnectionForm() map[string]interface{} {
	return map[string]interface{}{
		"kind":      "mssql",
		"authmode":  "sql",
		"encrypt":   "disable",
		"sslmode":   "disable",
		"trustCert": true,
	}
}

func (a *App) importFormHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "import"})
}

// historyHandler renders the dedicated local query history page. The
// history itself lives in the browser's localStorage (never sent to the
// server), so this just serves the shell page that reads it.
func (a *App) historyHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "history", map[string]interface{}{})
}

func (a *App) guideHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "guide", map[string]interface{}{})
}

func (a *App) aboutHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "about", map[string]interface{}{
		"Tenant":       a.tenant,
		"PageSize":     a.currentPageSize(),
		"QueryTimeout": a.currentQueryTimeout().String(),
		"ConnTimeout":  a.currentConnectTimeout().String(),
	})
}

// adminPageData assembles the base data every render of the "admin" template
// needs (Status/StatusJSON/Settings), merging in any page-specific extras
// such as an Error or Success message.
func (a *App) adminPageData(ctx context.Context, extra map[string]interface{}) map[string]interface{} {
	status := a.adminStatus(ctx)
	data := map[string]interface{}{
		"Status":     status,
		"StatusJSON": formatStatusJSON(status),
		"Settings":   a.runtimeSettingsView(),
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func (a *App) adminHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "admin", a.adminPageData(r.Context(), nil))
}

func (a *App) adminSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	settings, err := runtimeSettingsFromForm(r)
	if err == nil && strings.TrimSpace(r.Form.Get("llm_api_key")) == "" {
		settings.LLMAPIKey = a.currentLLMAPIKey()
	}
	if err == nil && strings.TrimSpace(r.Form.Get("embedding_api_key")) == "" {
		settings.EmbeddingAPIKey = a.currentRuntimeSettings().EmbeddingAPIKey
	}
	if err == nil {
		settings.Port = a.currentPort()
		// This form never asks for the Admin password (that's its own form,
		// handled by adminChangePasswordHandler) — without carrying it over,
		// saving any other setting here would silently wipe it back to
		// empty and disable Admin password protection.
		settings.AdminPasswordHash = a.currentAdminPasswordHash()
		// Maintenance mode has its own always-available toggle
		// (adminToggleMaintenanceHandler); this form doesn't include that
		// control, so keep whatever it's currently set to.
		settings.ReadOnlyMode = a.currentReadOnlyMode()
		// Auth mode is bootstrap-only for now (set via -auth-mode/env at
		// startup, see main.go); this form has no control for it yet, so
		// keep whatever it's currently set to instead of silently resetting
		// it to the "" -> local default.
		settings.AuthMode = string(a.currentAuthMode())
		err = a.applyRuntimeSettings(settings)
	}
	if err == nil {
		err = a.saveRuntimeSettings(r.Context())
	}
	data := a.adminPageData(r.Context(), nil)
	if err != nil {
		data["Error"] = err.Error()
		a.render(w, r, "admin", data)
		return
	}
	data["Success"] = "Settings updated."
	a.render(w, r, "admin", data)
}

func (a *App) jobsHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "jobs", map[string]interface{}{
		"Jobs":    a.nativeDB.Catalog().ListJobs(),
		"History": a.nativeDB.Catalog().ListJobHistory(),
	})
}

// pipelinesHandler renders the administrator-only view for reusable, versioned
// read-only SQL pipelines and their most recent lineage records.
func (a *App) pipelinesHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	pipelines, err := a.listPipelines(ctx)
	if err != nil {
		a.render(w, r, "pipelines", map[string]interface{}{"Error": "Load pipelines: " + err.Error()})
		return
	}
	runs, err := a.listPipelineRuns(ctx, "")
	if err != nil {
		a.render(w, r, "pipelines", map[string]interface{}{"Pipelines": pipelines, "Error": "Load pipeline runs: " + err.Error()})
		return
	}
	const recentPipelineRuns = 25
	if len(runs) > recentPipelineRuns {
		runs = runs[:recentPipelineRuns]
	}
	a.render(w, r, "pipelines", map[string]interface{}{
		"Pipelines": pipelines,
		"Runs":      runs,
	})
}

func (a *App) addConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("id")
	name := r.Form.Get("name")
	kind := r.Form.Get("kind")
	dsn := strings.TrimSpace(r.Form.Get("dsn"))

	// Echo back everything the user typed so a failed attempt (e.g. a wrong
	// password) doesn't force them to fill in the whole form again.
	form := connectionFormEcho(r)
	fail := func(message string) {
		a.render(w, r, "connections", map[string]interface{}{"Error": message, "Form": form})
	}

	if dsn == "" && strings.TrimSpace(r.Form.Get("host")) != "" {
		built, err := buildDSNFromFields(QuickConnectFields{
			Kind:      kind,
			Host:      r.Form.Get("host"),
			Port:      r.Form.Get("port"),
			Database:  r.Form.Get("database"),
			User:      r.Form.Get("user"),
			Password:  r.Form.Get("password"),
			Instance:  r.Form.Get("instance"),
			SSLMode:   r.Form.Get("sslmode"),
			Encrypt:   r.Form.Get("encrypt"),
			TrustCert: r.Form.Get("trust_cert") != "",
			Params:    r.Form.Get("params"),
			AuthMode:  r.Form.Get("authmode"),
		})
		if err != nil {
			fail(err.Error())
			return
		}
		dsn = built
	}
	ctx, cancel := a.withConnectTimeout(r.Context())
	defer cancel()
	conn, err := OpenManagedConnectionVerbose(ctx, id, name, kind, dsn, a.verboseLogger())
	if err != nil {
		fail(connectionErrorMessage(err, a.currentConnectTimeout()))
		return
	}
	// Owned by this session only: other concurrent users on the same running
	// server neither see nor can switch to it until an admin explicitly
	// shares it via "Save for everyone" (adminPersistConnectionHandler). Add
	// itself disambiguates the ID against another session's same-named
	// private connection, so this can't silently close/replace someone
	// else's live DB.
	conn.Owner = sessionIDFromContext(r.Context())
	if err := a.conns.Add(conn); err != nil {
		_ = conn.DB.Close()
		fail(err.Error())
		return
	}
	_ = a.conns.SetActiveFor(sessionIDFromContext(r.Context()), conn.ID)
	// Deliberately does not persist to the server's shared settings here —
	// this connection (and any credentials in its DSN) only lives in memory
	// for this running process until an admin explicitly saves it via
	// adminPersistConnectionHandler (POST /admin/connections/persist).
	http.Redirect(w, r, "/connections", http.StatusSeeOther)
}

// connectionFormEcho captures the "Add Managed Connection" form fields so
// they can be re-rendered into the form after a failed attempt (e.g. a bad
// password or unreachable host), instead of forcing the user to retype
// everything. password and dsn are deliberately left blank on echo: both can
// carry a plaintext credential, and re-rendering them into a value="..."
// attribute would leak that secret into view-source, browser history, and
// any HAR/proxy capture of the response.
func connectionFormEcho(r *http.Request) map[string]interface{} {
	get := r.Form.Get
	return map[string]interface{}{
		"id":        get("id"),
		"name":      get("name"),
		"kind":      get("kind"),
		"host":      get("host"),
		"port":      get("port"),
		"database":  get("database"),
		"user":      get("user"),
		"password":  "",
		"instance":  get("instance"),
		"sslmode":   get("sslmode"),
		"encrypt":   get("encrypt"),
		"authmode":  get("authmode"),
		"params":    get("params"),
		"dsn":       "",
		"trustCert": get("trust_cert") != "",
	}
}

// setActiveConnectionHandler is the unprivileged "Use" action: it only ever
// changes this session's own active connection (SetActiveFor is
// session-scoped and already rejects switching to a connection privately
// owned by someone else). Changing the server-wide default for every
// session is a separate, admin-gated action — see
// adminSetDefaultConnectionHandler — precisely because it affects everyone,
// not just the requester.
func (a *App) setActiveConnectionHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := a.conns.SetActiveFor(sessionIDFromContext(r.Context()), r.Form.Get("id")); err != nil {
		a.render(w, r, "connections", map[string]interface{}{"Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/connections", http.StatusSeeOther)
}

func (a *App) migrationHandler(w http.ResponseWriter, r *http.Request) {
	data := a.migrationPageData(r)
	a.render(w, r, "migrate", data)
}

func (a *App) runMigrationHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sourceID := r.Form.Get("source_id")
	targetID := r.Form.Get("target_id")
	sourceTable := r.Form.Get("source_table")
	targetTable := r.Form.Get("target_table")
	createTarget := r.Form.Get("create_target") == "on"

	sessionID := sessionIDFromContext(r.Context())
	summary, err := a.migrateTable(r.Context(), sessionID, sourceID, targetID, sourceTable, targetTable, createTarget)
	data := a.migrationPageData(r)
	data["SelectedSourceID"] = sourceID
	data["SelectedTargetID"] = targetID
	data["SourceTable"] = sourceTable
	data["TargetTable"] = targetTable
	data["CreateTarget"] = createTarget
	if source := a.conns.GetFor(sessionID, sourceID); source != nil {
		data["SourceTables"] = a.tableNames(contextWithActiveConnection(r.Context(), source))
	}
	if err != nil {
		data["Error"] = err.Error()
		a.render(w, r, "migrate", data)
		return
	}
	data["Summary"] = summary
	data["Success"] = fmt.Sprintf("Migrated %d rows from %s to %s.", summary.Rows, summary.SourceTable, summary.TargetTable)
	a.render(w, r, "migrate", data)
}

func (a *App) migrationPageData(r *http.Request) map[string]interface{} {
	sessionID := sessionIDFromContext(r.Context())
	active := a.activeConn(r.Context())
	sourceID := strings.TrimSpace(r.URL.Query().Get("source"))
	if sourceID == "" && active != nil {
		sourceID = active.ID
	}
	targetID := strings.TrimSpace(r.URL.Query().Get("target"))
	if targetID == "" {
		targetID = defaultConnectionID
	}

	data := map[string]interface{}{
		"Connections":        a.conns.ListFor(sessionID),
		"SelectedSourceID":   sourceID,
		"SelectedTargetID":   targetID,
		"CreateTarget":       true,
		"TargetTable":        strings.TrimSpace(r.URL.Query().Get("target_table")),
		"MigrationMaxTables": maxRAGTables,
	}
	if source := a.conns.GetFor(sessionID, sourceID); source != nil {
		ctx := contextWithActiveConnection(r.Context(), source)
		data["SourceTables"] = a.tableNames(ctx)
	}
	return data
}

// matchMethodOption describes one selectable comparison method for the
// Matching wizard's per-field dropdown.
type matchMethodOption struct{ Value, Label, Hint string }

var matchMethodOptions = []matchMethodOption{
	{"token_set", "Word match", "Compares the set of words, ignoring order and legal-form suffixes (GmbH, AG, Inc., ...)"},
	{"similarity", "Similarity", "Typo-tolerant character-level similarity (Jaro-Winkler) on the normalized value"},
	{"normalized", "Normalized exact", "Case, diacritics, punctuation, and legal-form suffixes folded away, then compared exactly"},
	{"address", "Address (street + number)", "Splits the trailing house number and normalizes Str./Straße before comparing"},
	{"exact_ci", "Exact (ignore case)", "Case-insensitive exact match, no other normalization"},
	{"exact", "Exact", "Byte-for-byte identical values only"},
	{"numeric", "Numeric (tolerance %)", "Compares two numbers within a relative tolerance"},
	{"ean", "EAN/GTIN (validated)", "Only compares EAN-8/EAN-13 codes that pass checksum validation; garbled or placeholder codes are ignored rather than falsely matched"},
}

const matchDisplayLimit = 500

// matchHandler renders the Matching wizard. With ?config=<name>, it loads a
// saved MatchConfig instead of reading source/target/table selections from
// the query string, populating every step of the wizard (key columns,
// field rules, thresholds, save target) from that configuration in one
// shot — the "make it simple" path once a configuration has been saved.
func (a *App) matchHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	configName := strings.TrimSpace(q.Get("config"))
	if configName == "" {
		data := a.matchPageData(r, q.Get("source"), q.Get("target"), q.Get("source_table"), q.Get("target_table"))
		a.render(w, r, "match", data)
		return
	}

	cfg, ok, err := a.loadMatchConfig(r.Context(), configName)
	if err != nil || !ok {
		data := a.matchPageData(r, q.Get("source"), q.Get("target"), q.Get("source_table"), q.Get("target_table"))
		if err != nil {
			data["Error"] = "Could not load configuration: " + err.Error()
		} else {
			data["Error"] = fmt.Sprintf("Configuration %q not found.", configName)
		}
		a.render(w, r, "match", data)
		return
	}

	data := a.matchPageData(r, cfg.SourceConnID, cfg.TargetConnID, cfg.SourceTable, cfg.TargetTable)
	data["SourceKeyColumn"] = cfg.SourceKeyColumn
	data["TargetKeyColumn"] = cfg.TargetKeyColumn
	data["AutoThresholdPct"] = int(cfg.AutoThreshold*100 + 0.5)
	data["ReviewThresholdPct"] = int(cfg.ReviewThreshold*100 + 0.5)
	data["NoBlocking"] = cfg.NoBlocking
	data["FieldRows"] = cfg.Fields
	if cfg.SaveConnID != "" {
		data["SaveConnID"] = cfg.SaveConnID
	}
	if cfg.SaveTable != "" {
		data["SaveTable"] = cfg.SaveTable
	}
	if cfg.SaveScope != "" {
		data["SaveScope"] = cfg.SaveScope
	}
	data["ConfigName"] = cfg.Name
	// data["Schedule"] is a *MatchSchedule (not a value) specifically so a
	// missing schedule leaves the key unset/nil: Go templates treat every
	// non-pointer struct as truthy in {{if}}, so a zero-value MatchSchedule
	// would otherwise make the "Recurring execution" status line render
	// unconditionally.
	if sched, ok, err := a.loadMatchSchedule(r.Context(), cfg.Name); err == nil && ok {
		data["Schedule"] = &sched
	}
	data["Success"] = fmt.Sprintf("Loaded configuration %q.", cfg.Name)
	a.render(w, r, "match", data)
}

// matchPageData builds the Matching wizard's render data from explicit
// connection/table selections rather than always reading r.URL.Query():
// matchHandler (GET) passes the query string, but runMatchHandler (POST) has
// to pass the just-submitted form values instead, since a POST to a bare
// "/match" URL carries no query string of its own — reading query params
// unconditionally there would silently drop the source/target selection and
// make the field-rule editor disappear after every run.
func (a *App) matchPageData(r *http.Request, sourceID, targetID, sourceTable, targetTable string) map[string]interface{} {
	sessionID := sessionIDFromContext(r.Context())
	active := a.activeConn(r.Context())

	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" && active != nil {
		sourceID = active.ID
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		targetID = defaultConnectionID
	}
	sourceTable = strings.TrimSpace(sourceTable)
	targetTable = strings.TrimSpace(targetTable)

	data := map[string]interface{}{
		"Connections":        a.conns.ListFor(sessionID),
		"SelectedSourceID":   sourceID,
		"SelectedTargetID":   targetID,
		"SourceTable":        sourceTable,
		"TargetTable":        targetTable,
		"MatchMethods":       matchMethodOptions,
		"AutoThresholdPct":   90,
		"ReviewThresholdPct": 60,
		"SaveScope":          "auto_and_review",
		"SaveConnID":         defaultConnectionID,
		"SaveTable":          suggestResultTableName(sourceTable, targetTable),
	}
	if configs, err := a.listMatchConfigs(r.Context()); err == nil {
		data["MatchConfigs"] = configs
	}

	var sourceCols, targetCols []Column
	if source := a.conns.GetFor(sessionID, sourceID); source != nil {
		ctx := contextWithActiveConnection(r.Context(), source)
		if names, err := a.matchTableNames(ctx, source); err != nil {
			data["SourceTablesError"] = err.Error()
		} else {
			data["SourceTables"] = names
		}
		if sourceTable != "" {
			if meta, err := a.tableMeta(ctx, sourceTable); err != nil {
				data["SourceColumnsError"] = err.Error()
			} else {
				sourceCols = meta.Columns
				data["SourceColumns"] = meta.Columns
				data["SourceKeyColumn"] = defaultKeyColumn(meta)
			}
		}
	}
	if target := a.conns.GetFor(sessionID, targetID); target != nil {
		ctx := contextWithActiveConnection(r.Context(), target)
		if names, err := a.matchTableNames(ctx, target); err != nil {
			data["TargetTablesError"] = err.Error()
		} else {
			data["TargetTables"] = names
		}
		if targetTable != "" {
			if meta, err := a.tableMeta(ctx, targetTable); err != nil {
				data["TargetColumnsError"] = err.Error()
			} else {
				targetCols = meta.Columns
				data["TargetColumns"] = meta.Columns
				data["TargetKeyColumn"] = defaultKeyColumn(meta)
			}
		}
	}
	if len(sourceCols) > 0 && len(targetCols) > 0 {
		data["FieldRows"] = suggestFieldPairs(sourceCols, targetCols)
	}
	return data
}

// matchTableNames lists browsable tables/views for conn. Unlike
// a.tableNames (used by the sidebar and other pages, which deliberately
// swallows errors so a flaky connection doesn't break page rendering
// everywhere), this surfaces the underlying error for non-tinySQL
// connections instead of silently returning an empty list — on the
// Matching wizard specifically, an empty "Source/Target Table" dropdown
// with no explanation is otherwise indistinguishable from "this connection
// genuinely has zero tables", which makes a real failure (network,
// permissions, timeout) impossible to diagnose from the UI.
func (a *App) matchTableNames(ctx context.Context, conn *DBConnection) ([]string, error) {
	if conn.IsTinySQL() {
		return a.tableNames(ctx), nil
	}
	objects, err := conn.tableObjects(ctx)
	if err != nil {
		return nil, err
	}
	return tableObjectNames(objects), nil
}

// defaultKeyColumn guesses a sensible identifier column to key match results
// on: "id" if present, otherwise the table's first column.
func defaultKeyColumn(meta TableMeta) string {
	for _, c := range meta.Columns {
		if strings.EqualFold(c.Name, "id") {
			return c.Name
		}
	}
	if len(meta.Columns) > 0 {
		return meta.Columns[0].Name
	}
	return ""
}

// suggestFieldPairs proposes a starting set of field rules by pairing each
// source column with its best-matching (by column-name similarity) unused
// target column, so the wizard isn't a blank form even before the user has
// mapped anything by hand. "id"-named columns are skipped since they're
// virtually never meaningful to fuzzy-match on across two systems.
func suggestFieldPairs(sourceCols, targetCols []Column) []MatchFieldSpec {
	usedTarget := make(map[string]bool, len(targetCols))
	var out []MatchFieldSpec
	for _, sc := range sourceCols {
		if strings.EqualFold(sc.Name, "id") || len(out) >= 8 {
			continue
		}
		normSrc := match.CanonicalizeName(sc.Name)
		bestScore, bestName := 0.0, ""
		for _, tc := range targetCols {
			if usedTarget[tc.Name] || strings.EqualFold(tc.Name, "id") {
				continue
			}
			if score := match.JaroWinkler(normSrc, match.CanonicalizeName(tc.Name)); score > bestScore {
				bestScore, bestName = score, tc.Name
			}
		}
		if bestName != "" && bestScore >= 0.6 {
			usedTarget[bestName] = true
			out = append(out, MatchFieldSpec{SourceColumn: sc.Name, TargetColumn: bestName, Method: "token_set", Weight: 1})
		}
	}
	return out
}

// suggestResultTableName proposes a default crosswalk table name derived
// from the two compared tables.
func suggestResultTableName(sourceTable, targetTable string) string {
	if sourceTable == "" || targetTable == "" {
		return ""
	}
	base := strings.ToLower(sourceTable + "_" + targetTable + "_matches")
	return sanitizeIdentifier(strings.ReplaceAll(base, " ", "_"))
}

// runMatchHandler drives all three Matching wizard actions from one POST:
// mode=preview scores and displays candidates, mode=export streams them as a
// CSV download, and mode=save persists the (optionally filtered) results
// into a crosswalk table. Preview/export deliberately stay available during
// maintenance mode (they're read-only); only the save branch is gated.
func (a *App) runMatchHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sessionID := sessionIDFromContext(r.Context())
	mode := strings.TrimSpace(r.Form.Get("mode"))
	if mode == "" {
		mode = "preview"
	}

	req := MatchRequest{
		SourceConnID:    r.Form.Get("source_id"),
		TargetConnID:    r.Form.Get("target_id"),
		SourceTable:     r.Form.Get("source_table"),
		TargetTable:     r.Form.Get("target_table"),
		SourceKeyColumn: r.Form.Get("source_key_column"),
		TargetKeyColumn: r.Form.Get("target_key_column"),
		AutoThreshold:   parseMatchPercent(r.Form.Get("auto_threshold"), 90),
		ReviewThreshold: parseMatchPercent(r.Form.Get("review_threshold"), 60),
		NoBlocking:      r.Form.Get("no_blocking") == "on",
		Fields:          parseMatchFieldSpecs(r.Form),
	}

	saveConnID := strings.TrimSpace(r.Form.Get("save_conn_id"))
	saveTable := strings.TrimSpace(r.Form.Get("save_table"))
	saveScope := strings.TrimSpace(r.Form.Get("save_scope"))
	if saveScope == "" {
		saveScope = "auto_and_review"
	}
	configName := strings.TrimSpace(r.Form.Get("config_name"))

	data := a.matchPageData(r, req.SourceConnID, req.TargetConnID, req.SourceTable, req.TargetTable)
	data["SourceKeyColumn"] = req.SourceKeyColumn
	data["TargetKeyColumn"] = req.TargetKeyColumn
	data["AutoThresholdPct"] = int(req.AutoThreshold * 100)
	data["ReviewThresholdPct"] = int(req.ReviewThreshold * 100)
	data["NoBlocking"] = req.NoBlocking
	data["FieldRows"] = req.Fields
	data["SaveConnID"] = saveConnID
	data["SaveTable"] = saveTable
	data["SaveScope"] = saveScope
	data["ConfigName"] = configName

	// mode=save_config persists the setup itself rather than running it —
	// the "make this simple to build again" action the wizard's field
	// mapping/thresholds/etc. steps exist to support.
	if mode == "save_config" {
		if a.currentReadOnlyMode() {
			a.writeMaintenanceBlocked(w, r)
			return
		}
		if a.roleBlocksWrite(r.Context()) {
			a.writeReadOnlyRoleBlocked(w, r)
			return
		}
		cfg := MatchConfig{
			Name:            configName,
			SourceConnID:    req.SourceConnID,
			TargetConnID:    req.TargetConnID,
			SourceTable:     req.SourceTable,
			TargetTable:     req.TargetTable,
			SourceKeyColumn: req.SourceKeyColumn,
			TargetKeyColumn: req.TargetKeyColumn,
			Fields:          req.Fields,
			AutoThreshold:   req.AutoThreshold,
			ReviewThreshold: req.ReviewThreshold,
			NoBlocking:      req.NoBlocking,
			SaveConnID:      saveConnID,
			SaveTable:       saveTable,
			SaveScope:       saveScope,
		}
		if err := a.saveMatchConfig(r.Context(), cfg); err != nil {
			data["Error"] = err.Error()
			a.render(w, r, "match", data)
			return
		}
		if configs, err := a.listMatchConfigs(r.Context()); err == nil {
			data["MatchConfigs"] = configs
		}
		data["Success"] = fmt.Sprintf("Saved configuration %q.", cfg.Name)
		a.render(w, r, "match", data)
		return
	}

	summary, err := a.runMatch(r.Context(), sessionID, req)
	if err != nil {
		data["Error"] = err.Error()
		a.render(w, r, "match", data)
		return
	}
	data["Summary"] = summary
	displayResults := summary.Results
	if len(displayResults) > matchDisplayLimit {
		data["Truncated"] = true
		displayResults = displayResults[:matchDisplayLimit]
	}
	data["DisplayResults"] = displayResults

	switch mode {
	case "export":
		columns := []string{"source_key", "source_label", "target_key", "target_label", "score", "status"}
		rows := make([][]string, 0, len(summary.Results))
		for _, res := range summary.Results {
			rows = append(rows, []string{
				res.SourceKey, res.SourceLabel, res.TargetKey, res.TargetLabel,
				strconv.FormatFloat(res.Score, 'f', 4, 64), res.Status,
			})
		}
		if a.writeExport(w, columns, rows, "csv", suggestResultTableName(summary.SourceTable, summary.TargetTable)) {
			return
		}
		data["Error"] = "export failed"
		a.render(w, r, "match", data)
	case "save":
		if a.currentReadOnlyMode() {
			a.writeMaintenanceBlocked(w, r)
			return
		}
		if a.roleBlocksWrite(r.Context()) {
			a.writeReadOnlyRoleBlocked(w, r)
			return
		}
		saveConn := a.conns.GetFor(sessionID, saveConnID)
		if saveConn == nil {
			data["Error"] = fmt.Sprintf("save connection %q not found", saveConnID)
			a.render(w, r, "match", data)
			return
		}
		filtered := filterMatchResultsByScope(summary.Results, saveScope)
		written, err := a.saveMatchResults(r.Context(), saveConn, saveTable, filtered, time.Now())
		if err != nil {
			data["Error"] = err.Error()
			a.render(w, r, "match", data)
			return
		}
		data["Success"] = fmt.Sprintf("Saved %d match(es) to %q on %q.", written, saveTable, saveConn.Name)
		a.render(w, r, "match", data)
	default:
		a.render(w, r, "match", data)
	}
}

// parseMatchFieldSpecs reconstructs the dynamic per-row field rules from the
// Matching form's parallel arrays (field_source[i] pairs with
// field_target[i], field_method[i], ... at the same index i) — the standard
// technique for a JS-managed variable-length row list without a client-side
// framework. Rows missing either column name are dropped as blank leftovers.
func parseMatchFieldSpecs(form url.Values) []MatchFieldSpec {
	sources := form["field_source"]
	targets := form["field_target"]
	methods := form["field_method"]
	weights := form["field_weight"]
	tolerances := form["field_tolerance"]
	groups := form["field_group"]

	n := len(sources)
	if len(targets) < n {
		n = len(targets)
	}
	specs := make([]MatchFieldSpec, 0, n)
	for i := 0; i < n; i++ {
		// src/tgt are column identifiers, not free text: deliberately NOT
		// trimmed (see canonicalColumn's doc comment) so a real column named
		// e.g. "Name " with a trailing space still resolves correctly. Only
		// the blank check below uses a trimmed copy.
		src := sources[i]
		tgt := targets[i]
		if strings.TrimSpace(src) == "" || strings.TrimSpace(tgt) == "" {
			continue
		}
		method := "token_set"
		if i < len(methods) && strings.TrimSpace(methods[i]) != "" {
			method = strings.TrimSpace(methods[i])
		}
		weight := 1.0
		if i < len(weights) {
			if w, err := strconv.ParseFloat(strings.TrimSpace(weights[i]), 64); err == nil && w > 0 {
				weight = w
			}
		}
		tolerance := 0.0
		if i < len(tolerances) {
			if t, err := strconv.ParseFloat(strings.TrimSpace(tolerances[i]), 64); err == nil {
				tolerance = t / 100
			}
		}
		group := ""
		if i < len(groups) {
			group = strings.TrimSpace(groups[i])
		}
		specs = append(specs, MatchFieldSpec{SourceColumn: src, TargetColumn: tgt, Method: method, Weight: weight, Tolerance: tolerance, Group: group})
	}
	return specs
}

// parseMatchPercent parses a 0-100 percentage form field into a 0..1
// fraction, tolerating an already-fractional value and falling back to
// defaultPct when blank or unparseable.
func parseMatchPercent(s string, defaultPct float64) float64 {
	s = strings.TrimSpace(s)
	v := defaultPct
	if s != "" {
		if parsed, err := strconv.ParseFloat(s, 64); err == nil {
			v = parsed
		}
	}
	if v > 1 {
		v /= 100
	}
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func filterMatchResultsByScope(results []MatchResultRow, scope string) []MatchResultRow {
	if scope != "auto_only" {
		return results
	}
	out := make([]MatchResultRow, 0, len(results))
	for _, res := range results {
		if res.Status == match.StatusAuto {
			out = append(out, res)
		}
	}
	return out
}

// matchUploadSentinel is the special "connection" a Matching wizard side
// can be set to instead of a real registered connection: it tells
// matchTablesHandler to import an uploaded file into a new tinySQL table
// and use that as the side's effective connection/table, rather than
// reading one from a pre-existing table. Modeled after the
// "__datadock_"-prefixed reserved system-object names elsewhere in the
// app, so it's extremely unlikely to collide with a real connection ID a
// user picked.
const matchUploadSentinel = "__upload__"

// matchTablesHandler is the single submit target for the Matching wizard's
// "2. Tables" step, covering all four combinations of "pick an existing
// table" vs. "upload a file" for the source and target sides. A plain
// dropdown change re-submits with no file attached (cheap, read-only); a
// side set to matchUploadSentinel imports whatever file was attached for
// that side (via the same importUploadedFile path as the Manage Tables
// import tab) and resolves to (default connection, that table), exactly as
// if the user had picked it from the dropdown to begin with.
func (a *App) matchTablesHandler(w http.ResponseWriter, r *http.Request) {
	// See importFileHandler's identical comment: ParseMultipartForm's
	// maxMemory argument alone does not cap the request body size.
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20+1)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		data := a.matchPageData(r, r.Form.Get("source_id"), r.Form.Get("target_id"), "", "")
		data["Error"] = "Could not read upload: " + err.Error()
		a.render(w, r, "match", data)
		return
	}
	sourceID := r.Form.Get("source_id")
	targetID := r.Form.Get("target_id")

	// Uploading a file always creates a table, so — unlike a plain
	// dropdown resubmit, which only re-reads existing metadata — that
	// specific case must respect maintenance mode.
	if sourceID == matchUploadSentinel || targetID == matchUploadSentinel {
		if a.currentReadOnlyMode() {
			a.writeMaintenanceBlocked(w, r)
			return
		}
		if a.roleBlocksWrite(r.Context()) {
			a.writeReadOnlyRoleBlocked(w, r)
			return
		}
	}

	resolvedSourceID, sourceTable, sourceErr := a.resolveMatchTableSide(r, sourceID, "source")
	resolvedTargetID, targetTable, targetErr := a.resolveMatchTableSide(r, targetID, "target")

	data := a.matchPageData(r, resolvedSourceID, resolvedTargetID, sourceTable, targetTable)
	switch {
	case sourceErr != nil:
		data["Error"] = sourceErr.Error()
	case targetErr != nil:
		data["Error"] = targetErr.Error()
	}
	a.render(w, r, "match", data)
}

// deleteMatchConfigHandler removes a saved Match Configuration, and — since
// a Match Schedule only makes sense attached to a configuration that still
// exists — its Match Schedule, if any.
func (a *App) deleteMatchConfigHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.Form.Get("config_name"))
	deleteErr := a.deleteMatchConfig(r.Context(), name)
	var scheduleErr error
	if deleteErr == nil {
		scheduleErr = a.deleteMatchSchedule(r.Context(), name)
	}

	data := a.matchPageData(r, r.Form.Get("source_id"), r.Form.Get("target_id"), r.Form.Get("source_table"), r.Form.Get("target_table"))
	switch {
	case deleteErr != nil:
		data["Error"] = deleteErr.Error()
	case scheduleErr != nil:
		data["Error"] = "Configuration deleted, but its schedule could not be removed: " + scheduleErr.Error()
	default:
		data["Success"] = fmt.Sprintf("Deleted configuration %q.", name)
	}
	a.render(w, r, "match", data)
}

// saveMatchScheduleHandler attaches (or updates) a recurring cron schedule
// to an already-saved MatchConfig, so it re-runs and appends to its
// crosswalk table on its own.
func (a *App) saveMatchScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	configName := strings.TrimSpace(r.Form.Get("config_name"))
	sched := MatchSchedule{
		ConfigName: configName,
		CronExpr:   strings.TrimSpace(r.Form.Get("cron_expr")),
		Enabled:    r.Form.Get("enabled") == "on",
	}

	data := a.matchPageData(r, r.Form.Get("source_id"), r.Form.Get("target_id"), r.Form.Get("source_table"), r.Form.Get("target_table"))
	data["ConfigName"] = configName
	if err := a.saveMatchSchedule(r.Context(), sched); err != nil {
		data["Error"] = err.Error()
		a.render(w, r, "match", data)
		return
	}
	if saved, ok, err := a.loadMatchSchedule(r.Context(), configName); err == nil && ok {
		data["Schedule"] = &saved
	}
	data["Success"] = fmt.Sprintf("Saved schedule for %q.", configName)
	a.render(w, r, "match", data)
}

// resolveMatchTableSide figures out the actual connection ID and table for
// one side ("source" or "target") of the Tables step. When connID is the
// matchUploadSentinel, it imports the "<side>_file" upload into a new
// tinySQL table; otherwise it passes connID through unchanged and reads the
// table from the ordinary "<side>_table" field.
func (a *App) resolveMatchTableSide(r *http.Request, connID, side string) (resolvedConnID, table string, err error) {
	if connID != matchUploadSentinel {
		return connID, strings.TrimSpace(r.Form.Get(side + "_table")), nil
	}
	file, header, ferr := r.FormFile(side + "_file")
	if ferr != nil {
		return connID, "", fmt.Errorf("choose a file to upload for the %s table", side)
	}
	defer file.Close()

	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	importedTable, _, ierr := a.importUploadedFile(ctx, file, header.Filename, "", "", "auto")
	if ierr != nil {
		return connID, "", ierr
	}
	return defaultConnectionID, importedTable, nil
}

// tableRowsSorted fetches a page of rows, optionally sorted by a known column.
// meta must already be validated (obtained from a.tableMeta). The SQL query is
// built entirely from DB-sourced values (meta.Name, validated column names).
func (a *App) tableRowsSorted(r *http.Request, page int, sortCol, dir string, pageSize int, meta TableMeta) ([]Column, [][]string, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = a.currentPageSize()
	}
	offset := (page - 1) * pageSize

	conn := a.activeConn(r.Context())
	// Resolve the sort column to its DB-sourced name to avoid using raw user
	// input in the SQL query. An unrecognised sort parameter is silently ignored.
	orderClause := ""
	if sortCol != "" {
		var canonical string
		for _, col := range meta.Columns {
			if col.Name == sortCol {
				canonical = col.Name // value from the DB schema, not from user
				break
			}
		}
		if canonical != "" {
			orderClause = canonical
		}
	}

	query := conn.selectPageSQL(meta.Name, orderClause, dir, pageSize, offset)

	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	rows, err := a.queryConn(ctx, conn, "table.sorted_rows", query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, err
	}
	cols := make([]Column, len(colTypes))
	for i, ct := range colTypes {
		cols[i] = Column{Name: ct.Name(), TypeName: ct.DatabaseTypeName()}
	}

	var result [][]string
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = anyToString(v)
		}
		result = append(result, row)
	}
	return cols, result, rows.Err()
}

// newRecordFormHandler renders a blank form for creating a record.
func (a *App) newRecordFormHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		a.renderObjectMissing(w, r, tableName, fmt.Errorf("table %q not found", tableName))
		return
	}
	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}
	a.render(w, r, "record_form", map[string]interface{}{
		"Table":  meta.Name, // canonical name from DB
		"Meta":   meta,
		"Values": map[string]string{},
		"IsNew":  true,
	})
}

// createRecordHandler stores a new record.
func (a *App) createRecordHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		a.renderObjectMissing(w, r, tableName, fmt.Errorf("table %q not found", tableName))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}

	values := make(map[string]string, len(meta.Columns))
	for _, col := range meta.Columns {
		if !strings.EqualFold(col.Name, "id") {
			values[col.Name] = r.Form.Get(col.Name)
		}
	}

	if err := a.insertRecord(r.Context(), meta.Name, values, meta.Columns); err != nil {
		a.render(w, r, "record_form", map[string]interface{}{
			"Table":  meta.Name,
			"Meta":   meta,
			"Values": values,
			"IsNew":  true,
			"Error":  err.Error(),
		})
		return
	}
	http.Redirect(w, r, "/t/"+url.PathEscape(meta.Name), http.StatusSeeOther)
}

// editRecordFormHandler renders a pre-populated edit form.
func (a *App) editRecordFormHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		a.renderObjectMissing(w, r, tableName, fmt.Errorf("table %q not found", tableName))
		return
	}
	id := r.PathValue("id")

	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}
	if !meta.HasID {
		http.Error(w, "table has no id column", http.StatusBadRequest)
		return
	}

	cols, row, err := a.getRecord(r.Context(), meta.Name, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}

	values := make(map[string]string, len(cols))
	for i, col := range cols {
		values[col.Name] = row[i]
	}

	a.render(w, r, "record_form", map[string]interface{}{
		"Table":  meta.Name,
		"Meta":   meta,
		"Values": values,
		"ID":     id,
		"IsNew":  false,
	})
}

// updateRecordHandler saves changes to an existing record.
func (a *App) updateRecordHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		a.renderObjectMissing(w, r, tableName, fmt.Errorf("table %q not found", tableName))
		return
	}
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}

	values := make(map[string]string, len(meta.Columns))
	for _, col := range meta.Columns {
		if !strings.EqualFold(col.Name, "id") {
			values[col.Name] = r.Form.Get(col.Name)
		}
	}

	if err := a.updateRecord(r.Context(), meta.Name, id, values, meta.Columns); err != nil {
		a.render(w, r, "record_form", map[string]interface{}{
			"Table":  meta.Name,
			"Meta":   meta,
			"Values": values,
			"ID":     id,
			"IsNew":  false,
			"Error":  err.Error(),
		})
		return
	}
	http.Redirect(w, r, "/t/"+url.PathEscape(meta.Name), http.StatusSeeOther)
}

// deleteRecordHandler deletes a record by id.
func (a *App) deleteRecordHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		a.renderObjectMissing(w, r, tableName, fmt.Errorf("table %q not found", tableName))
		return
	}
	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		if isMissingDBObjectError(err) {
			a.renderObjectMissing(w, r, tableName, err)
			return
		}
		a.serverError(w, err)
		return
	}
	if err := a.deleteRecord(r.Context(), meta.Name, r.PathValue("id")); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/t/"+url.PathEscape(meta.Name), http.StatusSeeOther)
}

// createTableFormHandler renders the New Table tab of the combined table
// management page.
func (a *App) createTableFormHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "create"})
}

// createTableHandler executes the CREATE TABLE DDL.
func (a *App) createTableHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	tableName := strings.TrimSpace(r.Form.Get("table_name"))
	if tableName == "" {
		a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "create", "Error": "Table name is required."})
		return
	}
	if !isValidIdentifier(tableName) {
		a.render(w, r, "manage_table", map[string]interface{}{
			"ActiveTab": "create",
			"Error":     "Table name may only contain letters, digits, and underscores.",
		})
		return
	}
	// Re-derive table name from validated characters only to break the taint
	// path: at this point every character in tableName is [a-zA-Z0-9_], so
	// the result is identical to tableName, but the data no longer carries
	// a taint from the raw user input as far as static analysis is concerned.
	safeTableName := sanitizeIdentifier(tableName)

	colNames := r.Form["col_name"]
	colTypes := r.Form["col_type"]
	if len(colNames) == 0 {
		a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "create", "Error": "At least one column is required."})
		return
	}

	// Build the column list; always prepend `id INT` as the primary key.
	defs := []string{"id INT"}
	for i, name := range colNames {
		name = strings.TrimSpace(name)
		if name == "" || strings.EqualFold(name, "id") {
			continue
		}
		if !isValidIdentifier(name) {
			a.render(w, r, "manage_table", map[string]interface{}{
				"ActiveTab": "create",
				"Error":     fmt.Sprintf("Column name %q may only contain letters, digits, and underscores.", name),
			})
			return
		}
		// Re-derive column name via the same sanitization path.
		safeName := sanitizeIdentifier(name)
		t := "TEXT"
		if i < len(colTypes) {
			switch strings.ToUpper(colTypes[i]) {
			case "INT", "INTEGER":
				t = "INT"
			case "REAL", "FLOAT", "DOUBLE":
				t = "FLOAT"
			case "BOOL", "BOOLEAN":
				t = "BOOL"
			default:
				t = "TEXT"
			}
		}
		defs = append(defs, a.activeConn(r.Context()).QuoteIdent(safeName)+" "+t)
	}

	conn := a.activeConn(r.Context())
	ddl := createTableQuery(conn, safeTableName, defs)
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	if _, err := a.execConn(ctx, conn, "table.create", ddl); err != nil {
		a.render(w, r, "manage_table", map[string]interface{}{
			"ActiveTab": "create",
			"Error":     "Could not create table: " + err.Error(),
			"TableName": safeTableName,
		})
		return
	}
	http.Redirect(w, r, "/t/"+url.PathEscape(safeTableName), http.StatusSeeOther)
}

func (a *App) importFileHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn != nil && !conn.IsTinySQL() {
		a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "import", "Error": "File import currently targets the local tinySQL DB only."})
		return
	}
	// ParseMultipartForm's own maxMemory argument only bounds how much file
	// data it holds in memory before spilling to a temp file — it does not
	// cap the request body itself, so without MaxBytesReader a client can
	// still make the server spool an arbitrarily large body to disk before
	// any size check below ever runs. maxMapImportUploadBytes is the
	// largest limit any format accepted here can have; importUploadedFile
	// still enforces the tighter per-format limit afterward.
	r.Body = http.MaxBytesReader(w, r.Body, maxMapImportUploadBytes+1)
	if err := r.ParseMultipartForm(maxImportUploadBytes); err != nil {
		a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "import", "Error": "Could not read upload: " + err.Error()})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "import", "Error": "Choose a file to import."})
		return
	}
	defer file.Close()

	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	table, res, err := a.importUploadedFile(ctx, file, header.Filename, r.Form.Get("table"), r.Form.Get("format"), r.Form.Get("header_mode"))
	if err != nil {
		a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "import", "Error": err.Error(), "TableName": table})
		return
	}
	a.render(w, r, "manage_table", map[string]interface{}{
		"ActiveTab": "import",
		"Success":   fmt.Sprintf("Imported %d rows into %s.", res.RowsInserted, table),
		"Table":     table,
		"Result":    res,
		"SpatialReport": func() *SpatialImportReport {
			report, _ := a.loadSpatialImportReport(ctx, table)
			return report
		}(),
	})
}

// importUploadedFile parses a multipart file upload (CSV/TSV/JSON/NDJSON/
// XLSX/XML/YAML plus supported geodata, database, calendar, contact, HTML,
// and compact binary data formats) and imports it into a new table on the local
// tinySQL database — the only backend file import currently supports.
// Shared by the Manage Tables import tab (importFileHandler) and the
// Matching wizard's "File Upload" connection option (resolveMatchTableSide,
// called from matchTablesHandler), so both go through the exact same,
// already-tested parsing/type-inference path.
func (a *App) importUploadedFile(ctx context.Context, file multipart.File, filename, tableOverride, formatOverride, headerMode string) (string, *dbimporter.ImportResult, error) {
	table := strings.TrimSpace(tableOverride)
	if table == "" {
		table = tableNameFromFilename(filename)
	}
	if !isValidIdentifier(table) {
		return "", nil, fmt.Errorf("table name may only contain letters, digits, and underscores")
	}
	table = sanitizeIdentifier(table)

	sourceFormat := importFormatFromName(filename, formatOverride)
	format := sourceFormat
	limit := importUploadLimit(format)
	content, err := readLimitedImport(file, limit)
	if err != nil {
		return table, nil, fmt.Errorf("could not read upload: %w", err)
	}
	originalContent := content
	if format == "xlsx" {
		content, err = xlsxToCSV(content)
		if err != nil {
			return table, nil, fmt.Errorf("could not read XLSX file: %w", err)
		}
		format = "csv"
	}
	opts := &dbimporter.ImportOptions{
		CreateTable:   true,
		HeaderMode:    strings.TrimSpace(headerMode),
		TypeInference: true,
	}
	if opts.HeaderMode == "" {
		opts.HeaderMode = "auto"
	}
	if format == "tsv" {
		opts.DelimiterCandidates = []rune{'\t'}
		format = "csv"
	}
	res, err := importContent(ctx, a.nativeDB, a.tenant, table, format, bytes.NewReader(content), opts)
	if err != nil {
		return table, nil, fmt.Errorf("import failed: %w", err)
	}
	if _, err := a.recordSpatialImportReport(ctx, table, filename, sourceFormat, originalContent); err != nil {
		res.Errors = append(res.Errors, "spatial report: "+err.Error())
	}
	return table, res, nil
}

const (
	maxImportUploadBytes    int64 = 16 << 20
	maxMapImportUploadBytes int64 = 256 << 20
)

func importUploadLimit(format string) int64 {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "geojson", "gpkg", "geopackage", "gpx", "kml", "osm", "pbf", "osmpbf", "osm-pbf", "shp", "shapefile", "shpzip", "mbtiles", "pmtiles", "rg", "routing-graph", "routing_graph", "sqlite", "sqlite3", "db", "duckdb", "parquet", "arrow", "feather":
		return maxMapImportUploadBytes
	default:
		return maxImportUploadBytes
	}
}

func readLimitedImport(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file is larger than the %s import limit", formatByteSize(limit))
	}
	return data, nil
}

// exportFormHandler renders the Export tab of the combined table management
// page. The actual file streaming reuses the existing per-table
// exportTableHandler at /t/{table}/export?format=X.
func (a *App) exportFormHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "manage_table", map[string]interface{}{"ActiveTab": "export"})
}

// demoJobName identifies the sample scheduled job seeded alongside demo data
// so the Jobs page has a working example out of the box.
const demoJobName = "demo_orders_by_status"

func (a *App) demoDataHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn != nil && !conn.IsTinySQL() {
		a.render(w, r, "admin", a.adminPageData(r.Context(), map[string]interface{}{"Error": "Demo data can only be imported into the local tinySQL database."}))
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	for _, stmt := range demoDataStatements() {
		result := a.executeTinySQL(ctx, stmt)
		if result.Err != "" && !strings.HasPrefix(stmt, "DROP TABLE ") {
			a.render(w, r, "admin", a.adminPageData(r.Context(), map[string]interface{}{"Error": "Could not import demo data: " + result.Err}))
			return
		}
	}
	a.seedDemoJob()
	http.Redirect(w, r, "/t/datadock_demo_people", http.StatusSeeOther)
}

// demoDataRemoveHandler drops every demo table (and the seeded demo job) so a
// server can be reset to a clean slate without touching real data.
func (a *App) demoDataRemoveHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn != nil && !conn.IsTinySQL() {
		a.render(w, r, "admin", a.adminPageData(r.Context(), map[string]interface{}{"Error": "Demo data can only be removed from the local tinySQL database."}))
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	for _, stmt := range demoDataDropStatements() {
		a.executeTinySQL(ctx, stmt) // best-effort: tables may not exist yet
	}
	_ = a.nativeDB.Catalog().DeleteJob(demoJobName)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// seedDemoJob registers a small scheduled job over the demo orders table so
// the Jobs page shows a realistic, already-working example.
func (a *App) seedDemoJob() {
	job, err := dbjobs.Build(dbjobs.Config{
		Name:         demoJobName,
		SQL:          "SELECT status, COUNT(*) AS order_count, SUM(total_amount) AS total_revenue FROM datadock_demo_orders GROUP BY status",
		ScheduleType: "INTERVAL",
		IntervalMs:   24 * 60 * 60 * 1000,
		NoOverlap:    true,
	})
	if err != nil {
		return
	}
	_ = a.nativeDB.RegisterJob(job)
}

// demoDataDropStatements lists DROP TABLE statements for every demo table,
// ordered so tables referencing other demo tables are dropped first.
func demoDataDropStatements() []string {
	return []string{
		"DROP TABLE datadock_demo_order_items",
		"DROP TABLE datadock_demo_orders",
		"DROP TABLE datadock_demo_products",
		"DROP TABLE datadock_demo_customers",
		"DROP TABLE datadock_demo_payloads",
		"DROP TABLE datadock_demo_locations",
		"DROP TABLE datadock_demo_metrics",
		"DROP TABLE datadock_demo_tickets",
		"DROP TABLE datadock_demo_invoices",
		"DROP TABLE datadock_demo_events",
		"DROP TABLE datadock_demo_projects",
		"DROP TABLE datadock_demo_people",
		"DROP TABLE datadock_demo_departments",
	}
}

func demoDataStatements() []string {
	type department struct {
		id     int
		name   string
		region string
		budget int
	}
	type person struct {
		id           int
		name         string
		role         string
		departmentID int
		city         string
		hiredOn      string
	}
	type project struct {
		id           int
		name         string
		status       string
		ownerID      int
		departmentID int
		startDate    string
		dueDate      string
		budget       int
	}
	type event struct {
		id        int
		projectID int
		kind      string
		date      string
		amount    int
		severity  string
	}
	type invoice struct {
		id        int
		projectID int
		customer  string
		date      string
		status    string
		amount    int
	}
	type ticket struct {
		id        int
		projectID int
		openedBy  int
		priority  string
		status    string
		openedOn  string
		closedOn  string
	}

	departments := []department{
		{1, "Platform", "EMEA", 640000},
		{2, "Analytics", "Americas", 520000},
		{3, "Operations", "EMEA", 410000},
		{4, "Customer Success", "APAC", 360000},
		{5, "Finance", "Americas", 300000},
	}
	people := []person{
		{1, "Ada Lovelace", "Data Analyst", 2, "London", "2021-03-15"},
		{2, "Grace Hopper", "Principal Engineer", 1, "New York", "2020-07-01"},
		{3, "Katherine Johnson", "Operations Scientist", 3, "Hampton", "2022-01-10"},
		{4, "Hedy Lamarr", "Product Strategist", 4, "Vienna", "2021-09-20"},
		{5, "Dorothy Vaughan", "Team Lead", 2, "Washington", "2019-11-05"},
		{6, "Mary Jackson", "Systems Engineer", 1, "Hampton", "2022-06-13"},
		{7, "Margaret Hamilton", "Reliability Engineer", 1, "Boston", "2020-02-24"},
		{8, "Radia Perlman", "Network Architect", 3, "Seattle", "2023-04-03"},
		{9, "Evelyn Boyd Granville", "Finance Analyst", 5, "Los Angeles", "2022-10-17"},
		{10, "Annie Easley", "Support Engineer", 4, "Cleveland", "2023-08-28"},
	}
	projects := []project{
		{1, "Migration Pilot", "active", 2, 1, "2026-01-08", "2026-08-30", 180000},
		{2, "Revenue Model", "planned", 1, 5, "2026-04-01", "2026-10-15", 95000},
		{3, "Ops Dashboard", "active", 3, 3, "2026-02-14", "2026-07-31", 125000},
		{4, "Retention Signals", "active", 4, 4, "2026-03-05", "2026-09-10", 110000},
		{5, "Data Quality Sweep", "blocked", 5, 2, "2026-01-22", "2026-06-28", 70000},
		{6, "Edge Sync", "active", 8, 1, "2026-05-12", "2026-12-18", 210000},
		{7, "Forecast Review", "done", 9, 5, "2025-11-01", "2026-03-15", 80000},
		{8, "Support Triage AI", "planned", 10, 4, "2026-07-01", "2026-11-30", 99000},
	}
	events := []event{
		{1, 1, "imported_rows", "2026-01-12", 1280, "info"},
		{2, 1, "warnings", "2026-01-12", 4, "low"},
		{3, 1, "transformed_rows", "2026-02-03", 1175, "info"},
		{4, 2, "forecast", "2026-04-12", 42000, "info"},
		{5, 2, "variance", "2026-04-18", 6100, "medium"},
		{6, 3, "queries", "2026-02-20", 87, "info"},
		{7, 3, "alerts", "2026-03-01", 6, "medium"},
		{8, 3, "resolved_alerts", "2026-03-09", 5, "low"},
		{9, 4, "survey_responses", "2026-03-20", 340, "info"},
		{10, 4, "churn_risk_accounts", "2026-04-02", 18, "high"},
		{11, 5, "duplicate_records", "2026-02-14", 211, "high"},
		{12, 5, "fixed_records", "2026-03-18", 164, "medium"},
		{13, 6, "sync_jobs", "2026-05-20", 48, "info"},
		{14, 6, "failed_jobs", "2026-05-21", 3, "high"},
		{15, 6, "retries", "2026-05-22", 9, "medium"},
		{16, 7, "forecast", "2025-12-11", 78000, "info"},
		{17, 7, "approved_amount", "2026-03-12", 74200, "info"},
		{18, 8, "classified_tickets", "2026-07-02", 120, "info"},
		{19, 8, "escalations", "2026-07-03", 7, "medium"},
		{20, 8, "false_positives", "2026-07-04", 11, "low"},
	}
	invoices := []invoice{
		{1, 1, "Northwind Labs", "2026-02-01", "paid", 44000},
		{2, 1, "Northwind Labs", "2026-04-01", "sent", 38000},
		{3, 2, "Contoso Finance", "2026-05-05", "draft", 25000},
		{4, 3, "Fabrikam Ops", "2026-03-10", "paid", 31500},
		{5, 3, "Fabrikam Ops", "2026-06-10", "overdue", 22000},
		{6, 4, "Tailspin Support", "2026-04-15", "sent", 27000},
		{7, 5, "Adventure Works", "2026-03-20", "disputed", 14500},
		{8, 6, "Wide World Importers", "2026-06-01", "sent", 56000},
		{9, 6, "Wide World Importers", "2026-07-01", "draft", 47000},
		{10, 7, "Contoso Finance", "2026-02-28", "paid", 74200},
		{11, 8, "Tailspin Support", "2026-07-05", "draft", 18000},
	}
	tickets := []ticket{
		{1, 1, 6, "high", "closed", "2026-01-15", "2026-01-17"},
		{2, 1, 7, "medium", "closed", "2026-02-07", "2026-02-09"},
		{3, 2, 9, "low", "open", "2026-04-20", ""},
		{4, 3, 3, "medium", "closed", "2026-03-02", "2026-03-05"},
		{5, 3, 8, "high", "open", "2026-06-18", ""},
		{6, 4, 4, "medium", "in_progress", "2026-04-03", ""},
		{7, 5, 5, "critical", "blocked", "2026-02-18", ""},
		{8, 5, 1, "high", "closed", "2026-03-19", "2026-03-25"},
		{9, 6, 8, "critical", "in_progress", "2026-05-21", ""},
		{10, 6, 2, "medium", "open", "2026-06-02", ""},
		{11, 7, 9, "low", "closed", "2026-01-08", "2026-01-12"},
		{12, 8, 10, "high", "open", "2026-07-03", ""},
		{13, 8, 4, "medium", "open", "2026-07-04", ""},
	}

	type customer struct {
		id         int
		name       string
		segment    string
		country    string
		signupDate string
	}
	type product struct {
		id        int
		name      string
		category  string
		unitPrice int
	}
	type order struct {
		id          int
		customerID  int
		orderDate   string
		channel     string
		status      string
		totalAmount int
	}
	type orderItem struct {
		id        int
		orderID   int
		productID int
		quantity  int
		unitPrice int
	}
	type metricPoint struct {
		id     int
		date   string
		metric string
		value  int
	}
	type location struct {
		id       int
		name     string
		category string
		lat      float64
		lon      float64
		geometry string
	}
	type payloadRow struct {
		id          int
		source      string
		externalID  string
		importedAt  string
		payloadJSON string
		payloadXML  string
	}

	customers := []customer{
		{1, "Northwind Labs", "Enterprise", "DE", "2024-01-12"},
		{2, "Contoso Finance", "Enterprise", "US", "2023-11-03"},
		{3, "Fabrikam Ops", "Mid-Market", "US", "2024-03-22"},
		{4, "Tailspin Support", "SMB", "GB", "2024-06-15"},
		{5, "Adventure Works", "Mid-Market", "CA", "2023-09-08"},
		{6, "Wide World Importers", "Enterprise", "AU", "2024-02-19"},
		{7, "Globex Analytics", "SMB", "FR", "2024-08-01"},
		{8, "Initech Data", "Mid-Market", "US", "2024-05-27"},
		{9, "Umbrella Retail", "Enterprise", "JP", "2023-12-14"},
		{10, "Hooli Cloud", "SMB", "US", "2025-01-09"},
	}
	products := []product{
		{1, "Starter Plan", "Subscription", 29},
		{2, "Growth Plan", "Subscription", 99},
		{3, "Scale Plan", "Subscription", 249},
		{4, "Enterprise Plan", "Subscription", 899},
		{5, "Onboarding Package", "Services", 1200},
		{6, "Data Migration", "Services", 1800},
		{7, "Priority Support", "Add-on", 150},
		{8, "Extra Storage 1TB", "Add-on", 45},
		{9, "Custom Report Pack", "Services", 600},
		{10, "Training Workshop", "Services", 950},
	}
	orders := []order{
		{1, 1, "2026-01-10", "web", "paid", 899},
		{2, 1, "2026-03-10", "web", "paid", 435},
		{3, 2, "2026-01-18", "sales", "paid", 249},
		{4, 2, "2026-04-02", "sales", "paid", 1800},
		{5, 3, "2026-02-05", "web", "paid", 99},
		{6, 3, "2026-05-14", "partner", "pending", 600},
		{7, 4, "2026-02-20", "web", "paid", 29},
		{8, 4, "2026-06-01", "web", "refunded", 179},
		{9, 5, "2026-01-25", "sales", "paid", 1200},
		{10, 5, "2026-04-18", "sales", "paid", 950},
		{11, 6, "2026-02-11", "partner", "paid", 1798},
		{12, 6, "2026-05-30", "partner", "paid", 2400},
		{13, 7, "2026-03-03", "web", "paid", 99},
		{14, 7, "2026-06-15", "web", "cancelled", 249},
		{15, 8, "2026-01-29", "sales", "paid", 1049},
		{16, 8, "2026-05-06", "sales", "pending", 600},
		{17, 9, "2026-02-27", "partner", "paid", 3600},
		{18, 9, "2026-06-22", "partner", "paid", 1040},
		{19, 10, "2026-03-19", "web", "paid", 58},
		{20, 10, "2026-07-01", "web", "paid", 249},
	}
	orderItems := []orderItem{
		{1, 1, 4, 1, 899},
		{2, 2, 7, 2, 150},
		{3, 2, 8, 3, 45},
		{4, 3, 3, 1, 249},
		{5, 4, 6, 1, 1800},
		{6, 5, 2, 1, 99},
		{7, 6, 9, 1, 600},
		{8, 7, 1, 1, 29},
		{9, 8, 1, 1, 29},
		{10, 8, 7, 1, 150},
		{11, 9, 5, 1, 1200},
		{12, 10, 10, 1, 950},
		{13, 11, 4, 2, 899},
		{14, 12, 6, 1, 1800},
		{15, 12, 9, 1, 600},
		{16, 13, 2, 1, 99},
		{17, 14, 3, 1, 249},
		{18, 15, 4, 1, 899},
		{19, 15, 7, 1, 150},
		{20, 16, 9, 1, 600},
		{21, 17, 6, 2, 1800},
		{22, 18, 10, 1, 950},
		{23, 18, 8, 2, 45},
		{24, 19, 1, 2, 29},
		{25, 20, 2, 1, 99},
		{26, 20, 7, 1, 150},
	}

	// metrics holds a daily time series so the SQL editor's quick-chart
	// date/time line detection has something realistic to plot.
	var metrics []metricPoint
	metricStart := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	metricID := 1
	for i := 0; i < 30; i++ {
		day := metricStart.AddDate(0, 0, i)
		dateStr := day.Format("2006-01-02")
		weekend := day.Weekday() == time.Saturday || day.Weekday() == time.Sunday

		dau := 220 + i*2 + (i*17)%23 - 11
		revenue := 1500 + i*12 + (i*29)%97 - 48
		if weekend {
			dau -= 55
			revenue -= 320
		}
		if dau < 40 {
			dau = 40
		}
		if revenue < 200 {
			revenue = 200
		}

		metrics = append(metrics, metricPoint{metricID, dateStr, "daily_active_users", dau})
		metricID++
		metrics = append(metrics, metricPoint{metricID, dateStr, "daily_revenue", revenue})
		metricID++
	}
	locations := []location{
		{1, "Munich HQ", "office", 48.1372, 11.5761, `{"type":"Point","coordinates":[11.5761,48.1372]}`},
		{2, "Berlin Lab", "office", 52.52, 13.405, `{"type":"Point","coordinates":[13.405,52.52]}`},
		{3, "Hamburg Port Sensor", "sensor", 53.5511, 9.9937, `{"type":"Point","coordinates":[9.9937,53.5511]}`},
		{4, "Cologne Edge Node", "edge", 50.9375, 6.9603, `{"type":"Point","coordinates":[6.9603,50.9375]}`},
		{5, "Vienna Partner", "partner", 48.2082, 16.3738, `{"type":"Point","coordinates":[16.3738,48.2082]}`},
		{6, "Zurich Backup", "backup", 47.3769, 8.5417, `{"type":"Point","coordinates":[8.5417,47.3769]}`},
	}
	payloads := []payloadRow{
		{1, "webhook", "000123", "2026-07-05T08:15:30Z", `{"event":"order.created","amount":1299.5,"tags":["new","priority"],"customer":{"segment":"Enterprise","country":"DE"}}`, `<event type="order.created"><amount currency="EUR">1299.50</amount><customer segment="Enterprise" country="DE"/></event>`},
		{2, "sensor", "000124", "2026-07-05T09:22:10Z", `{"event":"temperature.reading","value":21.7,"unit":"celsius","device":{"id":"sensor-17","site":"Munich HQ"}}`, `<reading unit="celsius"><value>21.7</value><device id="sensor-17" site="Munich HQ"/></reading>`},
		{3, "import", "000125", "2026-07-05T10:05:00Z", `{"event":"file.imported","rows":420,"format":"geojson","warnings":[]}`, `<import format="geojson"><rows>420</rows><warnings>0</warnings></import>`},
	}

	statements := append([]string{}, demoDataDropStatements()...)
	statements = append(statements,
		"CREATE TABLE datadock_demo_departments (id INT, name TEXT, region TEXT, annual_budget INT)",
		"CREATE TABLE datadock_demo_people (id INT, name TEXT, role TEXT, department_id INT, city TEXT, hired_on TEXT)",
		"CREATE TABLE datadock_demo_projects (id INT, name TEXT, status TEXT, owner_id INT, department_id INT, start_date TEXT, due_date TEXT, budget INT)",
		"CREATE TABLE datadock_demo_events (id INT, project_id INT, event_type TEXT, event_date TEXT, amount INT, severity TEXT)",
		"CREATE TABLE datadock_demo_invoices (id INT, project_id INT, customer TEXT, invoice_date TEXT, status TEXT, amount INT)",
		"CREATE TABLE datadock_demo_tickets (id INT, project_id INT, opened_by INT, priority TEXT, status TEXT, opened_on TEXT, closed_on TEXT)",
		"CREATE TABLE datadock_demo_customers (id INT, name TEXT, segment TEXT, country TEXT, signup_date TEXT)",
		"CREATE TABLE datadock_demo_products (id INT, name TEXT, category TEXT, unit_price INT)",
		"CREATE TABLE datadock_demo_orders (id INT, customer_id INT, order_date TEXT, channel TEXT, status TEXT, total_amount INT)",
		"CREATE TABLE datadock_demo_order_items (id INT, order_id INT, product_id INT, quantity INT, unit_price INT)",
		"CREATE TABLE datadock_demo_metrics (id INT, metric_date TEXT, metric TEXT, value INT)",
		"CREATE TABLE datadock_demo_locations (id INT, name TEXT, category TEXT, lat FLOAT, lon FLOAT, geometry TEXT)",
		"CREATE TABLE datadock_demo_payloads (id INT, source TEXT, external_id TEXT, imported_at TEXT, payload_json TEXT, payload_xml TEXT)",
	)
	for _, row := range departments {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_departments (id, name, region, annual_budget) VALUES (%d, %s, %s, %d)", row.id, demoSQLString(row.name), demoSQLString(row.region), row.budget))
	}
	for _, row := range people {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_people (id, name, role, department_id, city, hired_on) VALUES (%d, %s, %s, %d, %s, %s)", row.id, demoSQLString(row.name), demoSQLString(row.role), row.departmentID, demoSQLString(row.city), demoSQLString(row.hiredOn)))
	}
	for _, row := range projects {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_projects (id, name, status, owner_id, department_id, start_date, due_date, budget) VALUES (%d, %s, %s, %d, %d, %s, %s, %d)", row.id, demoSQLString(row.name), demoSQLString(row.status), row.ownerID, row.departmentID, demoSQLString(row.startDate), demoSQLString(row.dueDate), row.budget))
	}
	for _, row := range events {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_events (id, project_id, event_type, event_date, amount, severity) VALUES (%d, %d, %s, %s, %d, %s)", row.id, row.projectID, demoSQLString(row.kind), demoSQLString(row.date), row.amount, demoSQLString(row.severity)))
	}
	for _, row := range invoices {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_invoices (id, project_id, customer, invoice_date, status, amount) VALUES (%d, %d, %s, %s, %s, %d)", row.id, row.projectID, demoSQLString(row.customer), demoSQLString(row.date), demoSQLString(row.status), row.amount))
	}
	for _, row := range tickets {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_tickets (id, project_id, opened_by, priority, status, opened_on, closed_on) VALUES (%d, %d, %d, %s, %s, %s, %s)", row.id, row.projectID, row.openedBy, demoSQLString(row.priority), demoSQLString(row.status), demoSQLString(row.openedOn), demoSQLString(row.closedOn)))
	}
	for _, row := range customers {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_customers (id, name, segment, country, signup_date) VALUES (%d, %s, %s, %s, %s)", row.id, demoSQLString(row.name), demoSQLString(row.segment), demoSQLString(row.country), demoSQLString(row.signupDate)))
	}
	for _, row := range products {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_products (id, name, category, unit_price) VALUES (%d, %s, %s, %d)", row.id, demoSQLString(row.name), demoSQLString(row.category), row.unitPrice))
	}
	for _, row := range orders {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_orders (id, customer_id, order_date, channel, status, total_amount) VALUES (%d, %d, %s, %s, %s, %d)", row.id, row.customerID, demoSQLString(row.orderDate), demoSQLString(row.channel), demoSQLString(row.status), row.totalAmount))
	}
	for _, row := range orderItems {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_order_items (id, order_id, product_id, quantity, unit_price) VALUES (%d, %d, %d, %d, %d)", row.id, row.orderID, row.productID, row.quantity, row.unitPrice))
	}
	for _, row := range metrics {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_metrics (id, metric_date, metric, value) VALUES (%d, %s, %s, %d)", row.id, demoSQLString(row.date), demoSQLString(row.metric), row.value))
	}
	for _, row := range locations {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_locations (id, name, category, lat, lon, geometry) VALUES (%d, %s, %s, %g, %g, %s)", row.id, demoSQLString(row.name), demoSQLString(row.category), row.lat, row.lon, demoSQLString(row.geometry)))
	}
	for _, row := range payloads {
		statements = append(statements, fmt.Sprintf("INSERT INTO datadock_demo_payloads (id, source, external_id, imported_at, payload_json, payload_xml) VALUES (%d, %s, %s, %s, %s, %s)", row.id, demoSQLString(row.source), demoSQLString(row.externalID), demoSQLString(row.importedAt), demoSQLString(row.payloadJSON), demoSQLString(row.payloadXML)))
	}
	return statements
}

func demoSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// dropTableHandler drops a table after confirmation.
func (a *App) dropTableHandler(w http.ResponseWriter, r *http.Request) {
	tableName := r.PathValue("table")
	if !a.canBrowseTableName(r, tableName) {
		http.NotFound(w, r)
		return
	}
	// Resolve to the canonical DB name before building any SQL.
	meta, err := a.tableMeta(r.Context(), tableName)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	conn := a.activeConn(r.Context())
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	if _, err := a.execConn(ctx, conn, "table.drop", dropTableQuery(conn, meta.Name)); err != nil {
		a.serverError(w, err)
		return
	}
	if conn.IsTinySQL() {
		_ = a.deleteSpatialImportReport(ctx, meta.Name)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// queryEditorHandler renders the SQL query editor page.
func (a *App) queryEditorHandler(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "query", map[string]interface{}{})
}

// queryExecHandler handles form-POST execution of SQL (fallback without JS).
// roleBlocksWrite reports whether the current session's role should block a
// write a handler is about to perform inline — i.e. one of the routes that,
// unlike most mutating routes, isn't wrapped in requireWritable at the route
// table level (POST /match's save/save_config branches, and the upload
// branch of POST /match/tables) and must therefore replicate
// requireWritable's RoleReadOnly check itself. Historically these three
// call sites only replicated the maintenance-mode half of that check, so a
// RoleReadOnly session could still create tables and persist match results.
func (a *App) roleBlocksWrite(ctx context.Context) bool {
	_, role, ok := a.currentSessionUser(sessionIDFromContext(ctx))
	return a.currentAuthMode() != AuthModeNone && ok && role == RoleReadOnly
}

// readOnlyRoleBlocksQuery reports whether sqlText should be rejected because
// the session is authenticated with RoleReadOnly and the statement isn't
// provably read-only. Shared by queryExecHandler and apiQueryHandler so the
// plain HTML form and the JSON API editor enforce the identical gate — only
// the JSON path checked this historically, letting a read-only account run
// arbitrary writes by using the plain form instead.
func (a *App) readOnlyRoleBlocksQuery(ctx context.Context, sqlText string) bool {
	_, role, ok := a.currentSessionUser(sessionIDFromContext(ctx))
	return a.currentAuthMode() != AuthModeNone && ok && role == RoleReadOnly && !sqlutil.IsResultProducing(sqlText)
}

const readOnlyRoleQueryBlockedMessage = "your account has read-only access; only SELECT/WITH/SHOW/EXPLAIN queries are allowed"

func (a *App) queryExecHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(r.Form.Get("sql"))
	if a.readOnlyRoleBlocksQuery(r.Context(), query) {
		a.render(w, r, "query", map[string]interface{}{
			"SQL":    query,
			"Result": QueryResult{Err: readOnlyRoleQueryBlockedMessage},
		})
		return
	}
	result := a.executeSQL(r.Context(), query)
	if audit := a.auditLogger(); audit.Enabled() && result.StatementClass != sqlutil.StatementReadQuery {
		status := http.StatusOK
		if result.Err != "" {
			status = http.StatusBadRequest
		}
		sessionID := sessionIDFromContext(r.Context())
		username, _, _ := a.currentSessionUser(sessionID)
		audit.Log(AuditEvent{
			Session:   sessionID,
			Username:  username,
			Method:    r.Method,
			Path:      r.URL.Path,
			Operation: string(result.StatementClass),
			Detail:    query,
			Status:    status,
		})
	}
	a.render(w, r, "query", map[string]interface{}{
		"SQL":    query,
		"Result": result,
	})
}

// apiQueryHandler handles JSON-based SQL execution from the editor's JS.
func (a *App) apiQueryHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SQL    string `json:"sql"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}
	limit := clampQueryWindowLimit(body.Limit)
	offset := body.Offset
	if offset < 0 {
		offset = 0
	}
	sessionID := sessionIDFromContext(r.Context())
	username, _, _ := a.currentSessionUser(sessionID)
	sqlText := strings.TrimSpace(body.SQL)
	if a.readOnlyRoleBlocksQuery(r.Context(), sqlText) {
		a.writeProblem(w, r, http.StatusForbidden, "Read-only role", readOnlyRoleQueryBlockedMessage)
		return
	}
	result := a.executeSQLWindow(r.Context(), sqlText, offset, limit)
	if audit := a.auditLogger(); audit.Enabled() && result.StatementClass != sqlutil.StatementReadQuery {
		status := http.StatusOK
		if result.Err != "" {
			status = http.StatusBadRequest
		}
		audit.Log(AuditEvent{
			Session:   sessionID,
			Username:  username,
			Method:    r.Method,
			Path:      r.URL.Path,
			Operation: string(result.StatementClass),
			Detail:    sqlText,
			Status:    status,
		})
	}

	type apiResult struct {
		Columns        []string   `json:"columns,omitempty"`
		Rows           [][]string `json:"rows,omitempty"`
		Affected       int64      `json:"affected,omitempty"`
		ElapsedMs      int64      `json:"elapsed_ms"`
		StatementClass string     `json:"statement_class,omitempty"`
		Offset         int        `json:"offset,omitempty"`
		Limit          int        `json:"limit,omitempty"`
		HasMore        bool       `json:"has_more,omitempty"`
		Error          string     `json:"error,omitempty"`
	}
	out := apiResult{
		Columns:        result.Columns,
		Rows:           result.Rows,
		Affected:       result.Affected,
		ElapsedMs:      result.Elapsed.Milliseconds(),
		StatementClass: string(result.StatementClass),
		Offset:         result.Offset,
		Limit:          result.Limit,
		HasMore:        result.HasMore,
		Error:          result.Err,
	}
	a.writeJSON(w, http.StatusOK, out)
}

func clampQueryWindowLimit(limit int) int {
	const (
		defaultLimit = 500
		maxLimit     = 5000
	)
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

// apiExportHandler streams the result of a read-only SQL query as CSV or JSON.
func (a *App) apiExportHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SQL               string    `json:"sql"`
		Format            string    `json:"format"`
		Explode           bool      `json:"explode"`
		Simplify          float64   `json:"simplify"`
		SimplifyTolerance float64   `json:"simplify_tolerance"`
		BBox              []float64 `json:"bbox"`
		Fields            []string  `json:"fields"`
		DropFields        []string  `json:"drop_fields"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}

	query := strings.TrimSpace(body.SQL)
	if query == "" {
		a.writeProblem(w, r, http.StatusBadRequest, "Empty SQL", "sql is required.")
		return
	}
	if !isResultQuerySQL(query) {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported SQL", "export requires SELECT, WITH, SHOW, EXPLAIN, DESCRIBE, or PRAGMA")
		return
	}
	if a.blockedForSystemTableAccess(r.Context(), query) {
		a.writeProblem(w, r, http.StatusForbidden, "Forbidden", systemTableAccessDeniedMessage)
		return
	}

	cols, rows, err := a.queryRows(r.Context(), query)
	if err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Query failed", err.Error())
		return
	}
	opts := exportOptions{
		Explode:           body.Explode,
		SimplifyTolerance: body.SimplifyTolerance,
		Fields:            body.Fields,
		DropFields:        body.DropFields,
	}
	if opts.SimplifyTolerance == 0 && body.Simplify > 0 {
		opts.SimplifyTolerance = body.Simplify
	}
	if opts.SimplifyTolerance < 0 {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid export option", "simplify_tolerance must be non-negative")
		return
	}
	if len(body.BBox) > 0 {
		if len(body.BBox) != 4 || body.BBox[0] > body.BBox[2] || body.BBox[1] > body.BBox[3] {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid export option", "bbox must be [minx,miny,maxx,maxy]")
			return
		}
		opts.BBox = &geoBBox{MinX: body.BBox[0], MinY: body.BBox[1], MaxX: body.BBox[2], MaxY: body.BBox[3]}
	}
	if ok := a.writeExport(w, cols, rows, body.Format, "query", opts); !ok {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported export format", "unsupported export format")
	}
}

func (a *App) apiSchemaHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", standards.MediaTypeJSON)
	_, _ = w.Write([]byte(a.schemaSnapshot(r.Context())))
}

func (a *App) apiTinySQLAgentContextHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn != nil && !conn.IsTinySQL() {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported connection", "tinySQL agent context is available for the local tinySQL connection only")
		return
	}
	maxTables, err := optionalPositiveIntQuery(r, "max_tables", 12)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid query parameter", err.Error())
		return
	}
	maxChars, err := optionalPositiveIntQuery(r, "max_chars", 6000)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid query parameter", err.Error())
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	profile, err := a.buildTinySQLAgentContext(ctx, maxTables, maxChars)
	if err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Agent context failed", err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"context":    profile,
		"max_tables": maxTables,
		"max_chars":  maxChars,
	})
}

func (a *App) apiTinySQLVectorCacheHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn != nil && !conn.IsTinySQL() {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported connection", "tinySQL vector cache statistics are available for the local tinySQL connection only")
		return
	}
	a.writeJSON(w, http.StatusOK, tinysql.VectorCacheAnalytics())
}

// apiSnapshotExportHandler emits a portable native snapshot only. Restore is
// intentionally not exposed over HTTP: it needs size limits, validation, an
// atomic swap, and explicit operator approval outside a request handler.
func (a *App) apiSnapshotExportHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="datadock.snapshot"`)
	if err := tinysql.SaveToWriter(a.nativeDB, w); err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Snapshot export failed", err.Error())
	}
}

func optionalPositiveIntQuery(r *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

// apiCatalogHandler returns the full server-wide catalog tree (every
// database/schema/table/view/procedure the active connection's credentials
// can see) for the sidebar. Other databases on the same PostgreSQL server
// come back with needsFetch=true and are populated on demand via
// apiCatalogExpandHandler.
func (a *App) apiCatalogHandler(w http.ResponseWriter, r *http.Request) {
	includeSystem := a.isAdminAuthenticated(sessionIDFromContext(r.Context()))
	tree, err := a.catalogTreeWithSystem(r.Context(), includeSystem)
	if err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Could not load catalog", err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, tree)
}

// apiCatalogExpandHandler lazily loads one database's schemas/tables/views
// that apiCatalogHandler reported as needsFetch=true.
func (a *App) apiCatalogExpandHandler(w http.ResponseWriter, r *http.Request) {
	database := strings.TrimSpace(r.URL.Query().Get("database"))
	if database == "" {
		a.writeProblem(w, r, http.StatusBadRequest, "Missing database", "the database query parameter is required")
		return
	}
	conn := a.activeConn(r.Context())
	if conn == nil || conn.IsTinySQL() {
		a.writeProblem(w, r, http.StatusBadRequest, "Not supported", "the active connection has a single database")
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	db, err := conn.ExpandCatalogDatabase(ctx, database)
	if err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Could not load database", err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, db)
}

func (a *App) apiAdminStatusHandler(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, http.StatusOK, a.adminStatus(r.Context()))
}

func (a *App) apiAdminSettingsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.writeJSON(w, http.StatusOK, a.runtimeSettingsView())
	case http.MethodPost:
		var body struct {
			Dialect        string `json:"dialect"`
			ConnectTimeout string `json:"connect_timeout"`
			QueryTimeout   string `json:"query_timeout"`
			LLMBaseURL     string `json:"llm_base_url"`
			LLMAPIKey      string `json:"llm_api_key"`
			LLMModel       string `json:"llm_model"`
			LLMTimeout     string `json:"llm_timeout"`
			ReadOnlyMode   bool   `json:"read_only_mode"`
			PageSize       int    `json:"page_size"`
			MatchMaxRows   int    `json:"match_max_rows"`
			DefaultTheme   string `json:"default_theme"`
			DefaultDensity string `json:"default_density"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
			return
		}
		pageSize := ""
		if body.PageSize > 0 {
			pageSize = strconv.Itoa(body.PageSize)
		}
		matchMaxRows := ""
		if body.MatchMaxRows > 0 {
			matchMaxRows = strconv.Itoa(body.MatchMaxRows)
		}
		settings, err := runtimeSettingsFromInput(runtimeSettingsInput{
			Dialect:        body.Dialect,
			ConnectTimeout: body.ConnectTimeout,
			QueryTimeout:   body.QueryTimeout,
			LLMBaseURL:     body.LLMBaseURL,
			LLMAPIKey:      body.LLMAPIKey,
			LLMModel:       body.LLMModel,
			LLMTimeout:     body.LLMTimeout,
			ReadOnlyMode:   body.ReadOnlyMode,
			PageSize:       pageSize,
			MatchMaxRows:   matchMaxRows,
			DefaultTheme:   body.DefaultTheme,
			DefaultDensity: body.DefaultDensity,
		})
		if err == nil && strings.TrimSpace(body.LLMAPIKey) == "" {
			// An empty key means "leave it alone", not "clear it" — matches
			// the masked placeholder shown in the Admin UI, which never
			// re-sends the real key.
			settings.LLMAPIKey = a.currentLLMAPIKey()
		}
		if err == nil {
			settings.Port = a.currentPort()
			settings.AdminPasswordHash = a.currentAdminPasswordHash()
			// Maintenance mode has its own always-available toggle
			// (POST /admin/maintenance/toggle); this endpoint no longer
			// changes it, so keep whatever it's currently set to.
			settings.ReadOnlyMode = a.currentReadOnlyMode()
			// Auth mode is bootstrap-only for now (set via -auth-mode/env at
			// startup); this endpoint has no field for it yet, so keep
			// whatever it's currently set to.
			settings.AuthMode = string(a.currentAuthMode())
			err = a.applyRuntimeSettings(settings)
		}
		if err == nil {
			err = a.saveRuntimeSettings(r.Context())
		}
		if err != nil {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid settings", err.Error())
			return
		}
		a.writeJSON(w, http.StatusOK, a.runtimeSettingsView())
	default:
		a.writeProblem(w, r, http.StatusMethodNotAllowed, "Method not allowed", "method not allowed")
	}
}

func (a *App) apiLLMDiscoverHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3500*time.Millisecond)
	defer cancel()
	result := discoverLLMServersVerbose(ctx, nil, r.URL.Query().Get("host"), r.URL.Query().Get("port"), a.verboseLogger())
	a.writeJSON(w, http.StatusOK, result)
}

func (a *App) apiLLMAutoConfigHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Host    string `json:"host"`
		Port    string `json:"port"`
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}

	baseURL := strings.TrimSpace(body.BaseURL)
	model := strings.TrimSpace(body.Model)
	if baseURL == "" || model == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 3500*time.Millisecond)
		defer cancel()
		result := discoverLLMServersVerbose(ctx, nil, body.Host, body.Port, a.verboseLogger())
		if result.Recommended == nil || len(result.Recommended.Models) == 0 {
			a.writeProblem(w, r, http.StatusNotFound, "No LLM server found", "No Ollama, LM Studio, llmster, or OpenAI-compatible local server responded with models.")
			return
		}
		if baseURL == "" {
			baseURL = result.Recommended.BaseURL
		}
		if model == "" {
			model = result.Recommended.Models[0]
		}
	}

	settings := a.currentRuntimeSettings()
	settings.LLMBaseURL = baseURL
	settings.LLMModel = model
	if err := a.applyRuntimeSettings(settings); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid settings", err.Error())
		return
	}
	if err := a.saveRuntimeSettings(r.Context()); err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Save failed", err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, a.runtimeSettingsView())
}

func (a *App) adminStatus(ctx context.Context) map[string]any {
	health := tinysql.HealthCheck(a.nativeDB)
	stats := health.BackendStats
	settings := a.runtimeSettingsView()
	usersConfigured, _ := a.usersConfigured(ctx)
	status := map[string]any{
		"ok":                    health.OK,
		"tenant":                a.tenant,
		"storage_mode":          health.ModeName,
		"path":                  health.Path,
		"read_only":             health.ReadOnly,
		"closed":                health.Closed,
		"closing":               health.Closing,
		"scheduler_running":     health.SchedulerRunning,
		"wal_active":            health.WALActive,
		"advanced_wal_active":   health.AdvancedWALActive,
		"tenants":               health.Tenants,
		"tables":                health.Tables,
		"audit_log_enabled":     a.auditLog,
		"auth_mode":             settings.AuthMode,
		"admin_password_set":    settings.AdminPasswordSet,
		"users_configured":      usersConfigured,
		"managed_connections":   0,
		"active_connection":     nil,
		"last_sync_at":          formatTimePtr(health.LastSyncAt),
		"last_close_at":         formatTimePtr(health.LastCloseAt),
		"error":                 health.Error,
		"backend_tables_memory": stats.TablesInMemory,
		"backend_tables_disk":   stats.TablesOnDisk,
		"memory_used_bytes":     stats.MemoryUsedBytes,
		"memory_used_human":     formatByteSize(stats.MemoryUsedBytes),
		"memory_limit_bytes":    stats.MemoryLimitBytes,
		"memory_limit_human":    formatByteSize(stats.MemoryLimitBytes),
		"memory_used_percent":   percentOf(stats.MemoryUsedBytes, stats.MemoryLimitBytes),
		"disk_used_bytes":       stats.DiskUsedBytes,
		"disk_used_human":       formatByteSize(stats.DiskUsedBytes),
		"cache_hit_rate":        stats.CacheHitRate,
		"cache_hit_percent":     int(stats.CacheHitRate * 100),
		"sync_count":            stats.SyncCount,
		"load_count":            stats.LoadCount,
		"eviction_count":        stats.EvictionCount,
		"settings":              settings,
	}
	if a.conns != nil {
		conns := a.conns.ListFor(sessionIDFromContext(ctx))
		status["managed_connections"] = len(conns)
		if conn := a.activeConn(ctx); conn != nil {
			status["active_connection"] = map[string]string{
				"id":      conn.ID,
				"name":    conn.Name,
				"kind":    conn.Kind,
				"dialect": conn.Dialect.Name,
			}
		}
	}
	if !health.Recovery.RecoveredAt.IsZero() || health.Recovery.Path != "" {
		status["recovery"] = map[string]any{
			"mode":                   health.Recovery.Mode.String(),
			"path":                   health.Recovery.Path,
			"checkpoint_loaded":      health.Recovery.CheckpointLoaded,
			"recovered_transactions": health.Recovery.RecoveredTransactions,
			"recovered_operations":   health.Recovery.RecoveredOperations,
			"truncated":              health.Recovery.Truncated,
			"recovered_at":           formatTimePtr(health.Recovery.RecoveredAt),
		}
	}
	return status
}

// formatByteSize renders a byte count as a short human-readable string.
func formatByteSize(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// percentOf returns used/limit as an integer percentage in [0, 100], or 0
// when limit is not set (unlimited/unknown).
func percentOf(used, limit int64) int {
	if limit <= 0 {
		return 0
	}
	pct := int(used * 100 / limit)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// formatStatusJSON renders the admin status map as real, human-readable JSON.
// (Previously the Admin page printed the map with Go's "%#v" verb, which
// emits Go composite-literal syntax rather than JSON.)
func formatStatusJSON(status map[string]any) string {
	b, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", status)
	}
	return string(b)
}

func (a *App) apiJobsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.writeJSON(w, http.StatusOK, map[string]any{
			"jobs":    a.nativeDB.Catalog().ListJobs(),
			"history": a.nativeDB.Catalog().ListJobHistory(),
		})
	case http.MethodPost:
		var body struct {
			Name         string `json:"name"`
			SQL          string `json:"sql"`
			ScheduleType string `json:"schedule_type"`
			CronExpr     string `json:"cron_expr"`
			IntervalMs   int64  `json:"interval_ms"`
			RunAt        string `json:"run_at"`
			Timezone     string `json:"timezone"`
			Enabled      *bool  `json:"enabled"`
			CatchUp      bool   `json:"catch_up"`
			NoOverlap    bool   `json:"no_overlap"`
			MaxRuntimeMs int64  `json:"max_runtime_ms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
			return
		}
		var runAt *time.Time
		if strings.TrimSpace(body.RunAt) != "" {
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(body.RunAt))
			if err != nil {
				a.writeProblem(w, r, http.StatusBadRequest, "Invalid timestamp", "run_at must be RFC3339 for ONCE jobs")
				return
			}
			runAt = &parsed
		}
		job, err := dbjobs.Build(dbjobs.Config{
			Name:         body.Name,
			SQL:          body.SQL,
			ScheduleType: body.ScheduleType,
			CronExpr:     body.CronExpr,
			IntervalMs:   body.IntervalMs,
			RunAt:        runAt,
			Timezone:     body.Timezone,
			Enabled:      body.Enabled,
			CatchUp:      body.CatchUp,
			NoOverlap:    body.NoOverlap,
			MaxRuntimeMs: body.MaxRuntimeMs,
		})
		if err != nil {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid job", err.Error())
			return
		}
		if err := a.nativeDB.RegisterJob(job); err != nil {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid job", err.Error())
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"job": job})
	default:
		a.writeProblem(w, r, http.StatusMethodNotAllowed, "Method not allowed", "method not allowed")
	}
}

func (a *App) apiRunJobHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		TimeoutMS int64  `json:"timeout_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}
	job, err := a.nativeDB.Catalog().GetJob(strings.TrimSpace(body.Name))
	if err != nil {
		a.writeProblem(w, r, http.StatusNotFound, "Job not found", err.Error())
		return
	}
	timeout := a.currentQueryTimeout()
	if body.TimeoutMS > 0 {
		timeout = time.Duration(body.TimeoutMS) * time.Millisecond
	}
	ctx := r.Context()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	started := time.Now()
	result := a.executeTinySQL(ctx, job.SQLText)
	finished := time.Now()
	status := "SUCCEEDED"
	errMsg := ""
	if result.Err != "" {
		status = "FAILED"
		errMsg = result.Err
	}
	_ = a.nativeDB.Catalog().AddJobHistory(&tinysql.CatalogJobHistory{
		JobName:      job.Name,
		StartedAt:    started,
		FinishedAt:   finished,
		DurationMs:   finished.Sub(started).Milliseconds(),
		Status:       status,
		ErrorMessage: errMsg,
	})
	a.writeJSON(w, http.StatusOK, map[string]any{
		"job":        job.Name,
		"status":     status,
		"columns":    result.Columns,
		"rows":       result.Rows,
		"affected":   result.Affected,
		"elapsed_ms": result.Elapsed.Milliseconds(),
		"error":      result.Err,
	})
}

// apiPipelinesHandler lists pipeline metadata or appends a new immutable
// definition version. The write route is wrapped with requireWritable so a
// maintenance window freezes both data and its reproducible workflow config.
func (a *App) apiPipelinesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := a.withQueryTimeout(r.Context())
		defer cancel()
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name != "" {
			versions, err := a.listPipelineVersions(ctx, name)
			if err != nil {
				a.writeProblem(w, r, http.StatusInternalServerError, "Load pipelines", err.Error())
				return
			}
			runs, err := a.listPipelineRuns(ctx, name)
			if err != nil {
				a.writeProblem(w, r, http.StatusInternalServerError, "Load pipeline runs", err.Error())
				return
			}
			a.writeJSON(w, http.StatusOK, map[string]any{"pipelines": versions, "runs": runs})
			return
		}
		pipelines, err := a.listPipelines(ctx)
		if err != nil {
			a.writeProblem(w, r, http.StatusInternalServerError, "Load pipelines", err.Error())
			return
		}
		runs, err := a.listPipelineRuns(ctx, "")
		if err != nil {
			a.writeProblem(w, r, http.StatusInternalServerError, "Load pipeline runs", err.Error())
			return
		}
		a.writeJSON(w, http.StatusOK, map[string]any{"pipelines": pipelines, "runs": runs})
	case http.MethodPost:
		var pipeline Pipeline
		if err := json.NewDecoder(r.Body).Decode(&pipeline); err != nil {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be a pipeline definition.")
			return
		}
		ctx, cancel := a.withQueryTimeout(r.Context())
		defer cancel()
		saved, err := a.savePipeline(ctx, pipeline)
		if err != nil {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid pipeline", err.Error())
			return
		}
		a.auditPipelineEvent(r, "pipeline.save", saved.Name, fmt.Sprintf("version=%d steps=%d", saved.Version, len(saved.Steps)), http.StatusOK)
		a.writeJSON(w, http.StatusCreated, map[string]any{"pipeline": saved})
	default:
		a.writeProblem(w, r, http.StatusMethodNotAllowed, "Method not allowed", "method not allowed")
	}
}

// apiRunPipelineHandler executes one explicit immutable pipeline version. A
// failed SQL step still returns a recorded run in the response so clients can
// inspect its lineage and failure without needing a second request.
func (a *App) apiRunPipelineHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must include a pipeline name.")
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	run, err := a.runPipeline(ctx, body.Name, body.Version)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Run pipeline", err.Error())
		return
	}
	status := http.StatusOK
	if run.Status != "succeeded" {
		status = http.StatusUnprocessableEntity
	}
	a.auditPipelineEvent(r, "pipeline.run", run.PipelineName, fmt.Sprintf("version=%d run_id=%s status=%s", run.PipelineVersion, run.RunID, run.Status), status)
	a.writeJSON(w, status, map[string]any{"run": run})
}

func (a *App) apiDeletePipelineHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must include a pipeline name.")
		return
	}
	name := strings.TrimSpace(body.Name)
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	if err := a.deletePipeline(ctx, name); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Delete pipeline", err.Error())
		return
	}
	a.auditPipelineEvent(r, "pipeline.delete", name, "all definition versions removed; run lineage retained", http.StatusOK)
	a.writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

// apiExportPipelineBundleHandler emits an immutable-definition-only JSON
// bundle. Building it into a bytes.Buffer first prevents a partial download if
// a database read or JSON encode fails before HTTP headers are committed.
func (a *App) apiExportPipelineBundleHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	var bundle bytes.Buffer
	if err := a.writePipelineBundle(ctx, &bundle); err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Export pipelines", err.Error())
		return
	}
	w.Header().Set("Content-Type", standards.MediaTypeJSON)
	w.Header().Set("Content-Disposition", `attachment; filename="datadock-pipelines.json"`)
	_, _ = io.Copy(w, &bundle)
}

// apiImportPipelineBundleHandler reads an append-only definition bundle. Run
// history is intentionally not importable, so a transferred workflow cannot
// create misleading execution provenance on another DataDock instance.
func (a *App) apiImportPipelineBundleHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPipelineBundleBytes+1)
	defer r.Body.Close()
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	result, err := a.readPipelineBundle(ctx, r.Body)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Import pipelines", err.Error())
		return
	}
	a.auditPipelineEvent(r, "pipeline.import", "pipeline bundle", fmt.Sprintf("imported=%d skipped=%d", len(result.Imported), result.Skipped), http.StatusOK)
	a.writeJSON(w, http.StatusOK, result)
}

func (a *App) auditPipelineEvent(r *http.Request, operation, target, detail string, status int) {
	audit := a.auditLogger()
	if !audit.Enabled() {
		return
	}
	sessionID := sessionIDFromContext(r.Context())
	username, _, _ := a.currentSessionUser(sessionID)
	audit.Log(AuditEvent{
		Session:   sessionID,
		Username:  username,
		Method:    r.Method,
		Path:      r.URL.Path,
		Operation: operation,
		Target:    target,
		Detail:    detail,
		Status:    status,
	})
}

func (a *App) apiImportHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn != nil && !conn.IsTinySQL() {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported connection", "imports are currently supported for the local tinySQL/datadock metadata database only")
		return
	}
	var body struct {
		Table         string `json:"table"`
		Format        string `json:"format"`
		Content       string `json:"content"`
		HeaderMode    string `json:"header_mode"`
		CreateTable   *bool  `json:"create_table"`
		Truncate      bool   `json:"truncate"`
		TypeInference *bool  `json:"type_inference"`
		Fuzzy         bool   `json:"fuzzy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}
	table := strings.TrimSpace(body.Table)
	if table == "" {
		a.writeProblem(w, r, http.StatusBadRequest, "Missing table", "table is required")
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		a.writeProblem(w, r, http.StatusBadRequest, "Missing content", "content is required")
		return
	}
	opts := &dbimporter.ImportOptions{
		CreateTable:   boolDefault(body.CreateTable, true),
		Truncate:      body.Truncate,
		HeaderMode:    strings.TrimSpace(body.HeaderMode),
		TypeInference: boolDefault(body.TypeInference, true),
	}
	if opts.HeaderMode == "" {
		opts.HeaderMode = "auto"
	}
	format := strings.ToLower(strings.TrimSpace(body.Format))
	if format == "" {
		format = "csv"
	}
	sourceFormat := format
	if format == "tsv" {
		opts.DelimiterCandidates = []rune{'\t'}
		format = "csv"
	}
	src := strings.NewReader(body.Content)
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()

	var res *dbimporter.ImportResult
	var err error
	if body.Fuzzy {
		fuzzyOpts := &dbimporter.FuzzyImportOptions{ImportOptions: opts}
		switch format {
		case "csv":
			res, err = dbimporter.FuzzyImportCSV(ctx, a.nativeDB, a.tenant, table, src, fuzzyOpts)
		case "json":
			res, err = dbimporter.FuzzyImportJSON(ctx, a.nativeDB, a.tenant, table, src, fuzzyOpts)
		case "ndjson":
			res, err = dbimporter.FuzzyImportNDJSON(ctx, a.nativeDB, a.tenant, table, src, fuzzyOpts)
		case "yaml", "yml":
			res, err = dbimporter.FuzzyImportYAML(ctx, a.nativeDB, a.tenant, table, src, fuzzyOpts)
		case "xml":
			res, err = dbimporter.FuzzyImportXML(ctx, a.nativeDB, a.tenant, table, src, fuzzyOpts)
		default:
			err = fmt.Errorf("fuzzy import supports csv/tsv, json, ndjson, yaml, and xml only")
		}
	} else {
		res, err = importContent(ctx, a.nativeDB, a.tenant, table, format, src, opts)
	}
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Import failed", err.Error())
		return
	}
	report, reportErr := a.recordSpatialImportReport(ctx, table, "api://inline", sourceFormat, []byte(body.Content))
	if reportErr != nil {
		res.Errors = append(res.Errors, "spatial report: "+reportErr.Error())
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"table":          table,
		"rows_inserted":  res.RowsInserted,
		"rows_skipped":   res.RowsSkipped,
		"delimiter":      string(res.Delimiter),
		"had_header":     res.HadHeader,
		"encoding":       res.Encoding,
		"line_ending":    res.LineEnding,
		"columns":        res.ColumnNames,
		"errors":         res.Errors,
		"spatial_report": report,
	})
}

func (a *App) executeTinySQL(ctx context.Context, sqlText string) QueryResult {
	start := time.Now()
	result := QueryResult{}
	if a.verboseLogger().Enabled() {
		a.verboseLogger().Log(VerboseEvent{
			System:    "database",
			Direction: "outbound",
			Operation: "tinysql.execute",
			Target:    "tinysql://" + a.tenant,
			SQL:       sqlText,
		})
	}
	rs, err := tinysql.ExecSQL(tinysql.WithAuditText(ctx, sqlText), a.nativeDB, a.tenant, sqlText)
	if err != nil {
		result.Err = err.Error()
		result.Elapsed = time.Since(start)
		if a.verboseLogger().Enabled() {
			a.verboseLogger().Log(VerboseEvent{
				System:    "database",
				Direction: "inbound",
				Operation: "tinysql.execute",
				Target:    "tinysql://" + a.tenant,
				Duration:  result.Elapsed,
				Status:    "error",
				Error:     err.Error(),
			})
		}
		return result
	}
	if rs != nil {
		result.Columns, result.Rows = resultutil.ResultSetToStringMatrix(rs)
	}
	result.Elapsed = time.Since(start)
	if a.verboseLogger().Enabled() {
		a.verboseLogger().Log(VerboseEvent{
			System:    "database",
			Direction: "inbound",
			Operation: "tinysql.execute",
			Target:    "tinysql://" + a.tenant,
			Duration:  result.Elapsed,
			Status:    "ok",
			Preview:   fmt.Sprintf("columns=%d rows=%d", len(result.Columns), len(result.Rows)),
		})
	}
	return result
}

func importContent(ctx context.Context, db *tinysql.DB, tenant, table, format string, src io.Reader, opts *dbimporter.ImportOptions) (*dbimporter.ImportResult, error) {
	switch format {
	case "csv":
		return dbimporter.ImportCSV(ctx, db, tenant, table, src, opts)
	case "json":
		return dbimporter.ImportJSON(ctx, db, tenant, table, src, opts)
	case "ndjson":
		return dbimporter.ImportNDJSON(ctx, db, tenant, table, src, opts)
	case "yaml", "yml":
		return dbimporter.ImportYAML(ctx, db, tenant, table, src, opts)
	case "xml":
		return dbimporter.ImportXML(ctx, db, tenant, table, src, opts)
	case "html", "htm":
		return dbimporter.ImportHTMLTables(ctx, db, tenant, table, src, opts)
	case "msgpack", "mpack", "msg":
		return dbimporter.ImportMessagePack(ctx, db, tenant, table, src, opts)
	case "cbor":
		return dbimporter.ImportCBOR(ctx, db, tenant, table, src, opts)
	case "bson":
		return dbimporter.ImportBSON(ctx, db, tenant, table, src, opts)
	case "ics", "ical", "icalendar":
		return dbimporter.ImportICalendar(ctx, db, tenant, table, src, opts)
	case "vcf", "vcard":
		return dbimporter.ImportVCard(ctx, db, tenant, table, src, opts)
	case "sqlite", "sqlite3", "db":
		return dbimporter.ImportSQLite(ctx, db, tenant, table, src, opts)
	case "duckdb":
		return dbimporter.ImportDuckDBManifest(ctx, db, tenant, table, src, opts)
	case "parquet", "arrow", "feather":
		return dbimporter.ImportColumnarManifest(ctx, db, tenant, table, format, src, opts)
	case "kml":
		return dbimporter.ImportKML(ctx, db, tenant, table, src, opts)
	case "gpx":
		return dbimporter.ImportGPX(ctx, db, tenant, table, src, opts)
	case "osm":
		return dbimporter.ImportOSM(ctx, db, tenant, table, src, opts)
	case "pbf", "osmpbf", "osm-pbf":
		return dbimporter.ImportOSMPBF(ctx, db, tenant, table, src, opts)
	case "gpkg", "geopackage":
		return dbimporter.ImportGeoPackage(ctx, db, tenant, table, src, opts)
	case "shp", "shapefile", "shpzip":
		return dbimporter.ImportShapefileZip(ctx, db, tenant, table, src, opts)
	case "mbtiles":
		return dbimporter.ImportMBTiles(ctx, db, tenant, table, src, opts)
	case "pmtiles":
		return dbimporter.ImportPMTiles(ctx, db, tenant, table, src, opts)
	case "rg", "routing-graph", "routing_graph":
		return dbimporter.ImportRoutingGraph(ctx, db, tenant, table, src, opts)
	case "geojson":
		return dbimporter.ImportGeoJSON(ctx, db, tenant, table, src, opts)
	default:
		return nil, fmt.Errorf("unsupported import format %q", format)
	}
}

func tableNameFromFilename(name string) string {
	base := strings.TrimSpace(name)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.ToLower(base)
	var b strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" || out[0] >= '0' && out[0] <= '9' {
		return "imported_table"
	}
	return out
}

func importFormatFromName(filename, selected string) string {
	selected = strings.ToLower(strings.TrimSpace(selected))
	if selected != "" && selected != "auto" {
		return selected
	}
	name := strings.ToLower(strings.TrimSpace(filename))
	if strings.HasSuffix(name, ".osm.xml") {
		return "osm"
	}
	if strings.HasSuffix(name, ".osm.pbf") {
		return "pbf"
	}
	if strings.HasSuffix(name, ".routing-graph") || strings.HasSuffix(name, ".routing_graph") || strings.HasSuffix(name, ".graph.json") {
		return "rg"
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		switch name[i+1:] {
		case "csv", "tsv", "json", "ndjson", "xlsx", "xml", "yaml", "yml", "geojson", "gpkg", "gpx", "kml", "osm", "pbf", "mbtiles", "pmtiles", "rg", "sqlite", "sqlite3", "db", "duckdb", "parquet", "arrow", "feather", "html", "htm", "msgpack", "mpack", "msg", "cbor", "bson", "ics", "ical", "vcf", "vcard":
			return name[i+1:]
		case "zip", "shp":
			return "shp"
		}
	}
	return "csv"
}

type xlsxSharedStrings struct {
	Items []xlsxSharedString `xml:"si"`
}

type xlsxSharedString struct {
	TextParts []string `xml:"t"`
}

type xlsxWorksheet struct {
	Rows []xlsxRow `xml:"sheetData>row"`
}

type xlsxRow struct {
	Cells []xlsxCell `xml:"c"`
}

type xlsxCell struct {
	Ref         string `xml:"r,attr"`
	Type        string `xml:"t,attr"`
	Value       string `xml:"v"`
	InlineValue string `xml:"is>t"`
}

func xlsxToCSV(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}
	shared, err := readXLSXSharedStrings(files["xl/sharedStrings.xml"])
	if err != nil {
		return nil, err
	}
	var sheetNames []string
	for name := range files {
		if strings.HasPrefix(name, "xl/worksheets/sheet") && strings.HasSuffix(name, ".xml") {
			sheetNames = append(sheetNames, name)
		}
	}
	sort.Slice(sheetNames, func(i, j int) bool {
		return xlsxSheetNumber(sheetNames[i]) < xlsxSheetNumber(sheetNames[j])
	})
	if len(sheetNames) == 0 {
		return nil, fmt.Errorf("no xl/worksheets/sheet*.xml files found")
	}
	if len(sheetNames) == 1 {
		return xlsxRowsToCSV(readXLSXSheetRows(files[sheetNames[0]], shared))
	}

	var allRows [][]string
	var header []string
	maxCols := 0
	for _, name := range sheetNames {
		rows, err := readXLSXSheetRows(files[name], shared)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			continue
		}
		if len(header) == 0 {
			header = append([]string{"sheet"}, rows[0]...)
			maxCols = len(header)
		}
		sheetLabel := strings.TrimSuffix(strings.TrimPrefix(name, "xl/worksheets/"), ".xml")
		for _, row := range rows[1:] {
			record := append([]string{sheetLabel}, row...)
			if len(record) > maxCols {
				maxCols = len(record)
			}
			allRows = append(allRows, record)
		}
	}
	if len(header) == 0 {
		return nil, fmt.Errorf("xlsx contains no worksheet rows")
	}
	for len(header) < maxCols {
		header = append(header, fmt.Sprintf("col_%d", len(header)))
	}
	return xlsxRowsToCSV(append([][]string{header}, allRows...), nil)
}

func readXLSXSheetRows(sheet *zip.File, shared []string) ([][]string, error) {
	if sheet == nil {
		return nil, fmt.Errorf("worksheet not found")
	}
	rc, err := sheet.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var ws xlsxWorksheet
	if err := xml.NewDecoder(rc).Decode(&ws); err != nil {
		return nil, err
	}
	var rows [][]string
	maxCols := 0
	for _, row := range ws.Rows {
		out := make([]string, 0, len(row.Cells))
		for _, cell := range row.Cells {
			colIdx := xlsxColumnIndex(cell.Ref)
			for len(out) < colIdx {
				out = append(out, "")
			}
			out = append(out, xlsxCellValue(cell, shared))
		}
		if len(out) > maxCols {
			maxCols = len(out)
		}
		rows = append(rows, out)
	}
	return rows, nil
}

func xlsxRowsToCSV(rows [][]string, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}
	for _, row := range rows {
		for len(row) < maxCols {
			row = append(row, "")
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func xlsxSheetNumber(name string) int {
	base := strings.TrimSuffix(strings.TrimPrefix(name, "xl/worksheets/sheet"), ".xml")
	n, err := strconv.Atoi(base)
	if err != nil {
		return 0
	}
	return n
}

func readXLSXSharedStrings(f *zip.File) ([]string, error) {
	if f == nil {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var ss xlsxSharedStrings
	if err := xml.NewDecoder(rc).Decode(&ss); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ss.Items))
	for _, item := range ss.Items {
		out = append(out, strings.Join(item.TextParts, ""))
	}
	return out, nil
}

func xlsxCellValue(cell xlsxCell, shared []string) string {
	switch cell.Type {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(cell.Value))
		if err == nil && i >= 0 && i < len(shared) {
			return shared[i]
		}
		return ""
	case "inlineStr":
		return cell.InlineValue
	default:
		return cell.Value
	}
}

func xlsxColumnIndex(ref string) int {
	if ref == "" {
		return 0
	}
	idx := 0
	for _, r := range ref {
		if r >= 'A' && r <= 'Z' {
			idx = idx*26 + int(r-'A'+1)
		} else if r >= 'a' && r <= 'z' {
			idx = idx*26 + int(r-'a'+1)
		} else {
			break
		}
	}
	if idx <= 0 {
		return 0
	}
	return idx - 1
}

func boolDefault(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

// runtimeSettingsInput carries the raw string/bool values collected from
// either the Admin HTML form or the JSON admin settings API, so both paths
// share one parsing/validation implementation.
type runtimeSettingsInput struct {
	Dialect          string
	ConnectTimeout   string
	QueryTimeout     string
	LLMBaseURL       string
	LLMAPIKey        string
	LLMModel         string
	LLMTimeout       string
	EmbeddingBaseURL string
	EmbeddingAPIKey  string
	EmbeddingModel   string
	VectorIndex      string
	VectorWarm       bool
	ReadOnlyMode     bool
	PageSize         string
	MatchMaxRows     string
	DefaultTheme     string
	DefaultDensity   string
}

func runtimeSettingsFromForm(r *http.Request) (RuntimeSettings, error) {
	return runtimeSettingsFromInput(runtimeSettingsInput{
		Dialect:          r.Form.Get("dialect"),
		ConnectTimeout:   r.Form.Get("connect_timeout"),
		QueryTimeout:     r.Form.Get("query_timeout"),
		LLMBaseURL:       r.Form.Get("llm_base_url"),
		LLMAPIKey:        r.Form.Get("llm_api_key"),
		LLMModel:         r.Form.Get("llm_model"),
		LLMTimeout:       r.Form.Get("llm_timeout"),
		EmbeddingBaseURL: r.Form.Get("embedding_base_url"),
		EmbeddingAPIKey:  r.Form.Get("embedding_api_key"),
		EmbeddingModel:   r.Form.Get("embedding_model"),
		VectorIndex:      r.Form.Get("vector_index"),
		VectorWarm:       r.Form.Get("vector_warm") != "",
		ReadOnlyMode:     r.Form.Get("read_only_mode") != "",
		PageSize:         r.Form.Get("page_size"),
		MatchMaxRows:     r.Form.Get("match_max_rows"),
		DefaultTheme:     r.Form.Get("default_theme"),
		DefaultDensity:   r.Form.Get("default_density"),
	})
}

func runtimeSettingsFromInput(in runtimeSettingsInput) (RuntimeSettings, error) {
	connect, err := parseOptionalDuration(in.ConnectTimeout, 10*time.Second, "connect_timeout")
	if err != nil {
		return RuntimeSettings{}, err
	}
	query, err := parseOptionalDuration(in.QueryTimeout, 60*time.Second, "query_timeout")
	if err != nil {
		return RuntimeSettings{}, err
	}
	llm, err := parseOptionalDuration(in.LLMTimeout, 45*time.Second, "llm_timeout")
	if err != nil {
		return RuntimeSettings{}, err
	}
	pageSize := 0
	if raw := strings.TrimSpace(in.PageSize); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return RuntimeSettings{}, fmt.Errorf("page_size must be a positive integer")
		}
		pageSize = n
	}
	matchMaxRows := 0
	if raw := strings.TrimSpace(in.MatchMaxRows); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return RuntimeSettings{}, fmt.Errorf("match_max_rows must be a positive integer")
		}
		matchMaxRows = n
	}
	return RuntimeSettings{
		Dialect:          in.Dialect,
		ConnectTimeout:   connect,
		QueryTimeout:     query,
		LLMBaseURL:       in.LLMBaseURL,
		LLMAPIKey:        in.LLMAPIKey,
		LLMModel:         in.LLMModel,
		LLMTimeout:       llm,
		EmbeddingBaseURL: in.EmbeddingBaseURL,
		EmbeddingAPIKey:  in.EmbeddingAPIKey,
		EmbeddingModel:   in.EmbeddingModel,
		VectorIndex:      in.VectorIndex,
		VectorWarm:       in.VectorWarm,
		ReadOnlyMode:     in.ReadOnlyMode,
		PageSize:         pageSize,
		MatchMaxRows:     matchMaxRows,
		DefaultTheme:     in.DefaultTheme,
		DefaultDensity:   in.DefaultDensity,
	}, nil
}

func parseOptionalDuration(value string, fallback time.Duration, name string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration such as 5s, 1m, or 250ms", name)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s must not be negative", name)
	}
	return d, nil
}

func formatTimePtr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// apiLLMHandler handles OpenAI-compatible natural-language SQL assistance.
func (a *App) apiLLMHandler(w http.ResponseWriter, r *http.Request) {
	llm := a.llmClient()
	if llm == nil {
		a.writeProblem(w, r, http.StatusServiceUnavailable, "LLM is not configured", "LLM is not configured")
		return
	}

	var body struct {
		Action  string     `json:"action"`
		Prompt  string     `json:"prompt"`
		SQL     string     `json:"sql"`
		Error   string     `json:"error"`
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}

	action := strings.TrimSpace(body.Action)
	switch action {
	case llmActionGenerateSQL, llmActionFixSQL, llmActionOptimizeSQL, llmActionExplainResults, llmActionExplainError, llmActionAskRun, llmActionCreateChart, llmActionReviewSQL, llmActionSuggestQuestions, llmActionAnalyzeQuality:
	default:
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported LLM action", "unsupported LLM action")
		return
	}

	prompt := strings.TrimSpace(body.Prompt)
	sqlText := strings.TrimSpace(body.SQL)
	errorText := strings.TrimSpace(body.Error)
	if prompt == "" && sqlText == "" && errorText == "" && len(body.Rows) == 0 {
		a.writeProblem(w, r, http.StatusBadRequest, "Missing LLM input", "prompt, sql, error, or rows are required")
		return
	}
	switch action {
	case llmActionFixSQL:
		if sqlText == "" || errorText == "" {
			a.writeProblem(w, r, http.StatusBadRequest, "Missing SQL repair context", "fix_sql requires the current SQL and a database error")
			return
		}
	case llmActionOptimizeSQL:
		if sqlText == "" {
			a.writeProblem(w, r, http.StatusBadRequest, "Missing SQL", "optimize_sql requires the current SQL")
			return
		}
	case llmActionReviewSQL:
		if sqlText == "" {
			a.writeProblem(w, r, http.StatusBadRequest, "Missing SQL", "review_sql requires the current SQL")
			return
		}
	case llmActionSuggestQuestions:
		if len(body.Columns) == 0 || len(body.Rows) == 0 {
			a.writeProblem(w, r, http.StatusBadRequest, "Missing result", "suggest_questions requires a query result")
			return
		}
	case llmActionAnalyzeQuality:
		if len(body.Columns) == 0 || len(body.Rows) == 0 {
			a.writeProblem(w, r, http.StatusBadRequest, "Missing result", "analyze_quality requires a query result")
			return
		}
	}

	req := a.buildLLMRequest(r.Context(), action, prompt, sqlText, errorText, body.Columns, body.Rows)
	var out string
	var err error
	if action == llmActionGenerateSQL || action == llmActionFixSQL || action == llmActionOptimizeSQL || action == llmActionCreateChart {
		out, err = a.completeSQLWithToolCalls(r.Context(), llm, req)
	} else {
		out, err = llm.Complete(r.Context(), req)
	}
	if err != nil {
		a.writeProblem(w, r, http.StatusBadGateway, "LLM request failed", err.Error())
		return
	}
	if action == llmActionGenerateSQL || action == llmActionFixSQL || action == llmActionOptimizeSQL {
		parsed := parseLLMSQLResponse(out)
		if strings.TrimSpace(parsed.SQL) != "" {
			parsed.Review = a.reviewGeneratedSQL(r.Context(), llm, prompt, parsed.SQL)
		}
		resp := map[string]any{
			"action":      parsed.Action,
			"mode":        action,
			"text":        parsed.SQL,
			"sql":         parsed.SQL,
			"explanation": parsed.Explanation,
			"review":      parsed.Review,
			"follow_up":   parsed.FollowUp,
		}
		if parsed.Chart != nil {
			resp["chart"] = parsed.Chart
		}
		a.writeJSON(w, http.StatusOK, resp)
		return
	}
	if action == llmActionSuggestQuestions {
		parsed := parseLLMSQLResponse(out)
		suggestions := parsed.Suggestions
		if len(suggestions) == 0 && parsed.Action != "clarify" {
			suggestions = fallbackLLMSuggestions(out)
		}
		responseAction := parsed.Action
		if len(suggestions) > 0 {
			responseAction = "suggestions"
		}
		resp := map[string]any{
			"action":      responseAction,
			"suggestions": suggestions,
			"explanation": parsed.Explanation,
			"follow_up":   parsed.FollowUp,
		}
		a.writeJSON(w, http.StatusOK, resp)
		return
	}
	if action == llmActionCreateChart {
		parsed := parseLLMSQLResponse(out)
		parsed.Chart = constrainLLMChartSpec(parsed.Chart, body.Columns)
		resp := map[string]any{
			"action":      parsed.Action,
			"explanation": parsed.Explanation,
			"follow_up":   parsed.FollowUp,
		}
		if parsed.Chart != nil {
			resp["chart"] = parsed.Chart
		}
		a.writeJSON(w, http.StatusOK, resp)
		return
	}
	if action == llmActionReviewSQL {
		a.writeJSON(w, http.StatusOK, map[string]string{"text": trimForLLM(stripMarkdownCodeFence(out), maxLLMReviewChars)})
		return
	}

	a.writeJSON(w, http.StatusOK, map[string]string{"text": out})
}

// buildLLMRequest assembles the LLMRequest (schema context + prompt/sql/
// error/result) shared by apiLLMHandler and apiLLMPreviewHandler, so the
// "what gets sent to the AI" preview is built from the exact same code path
// as a real request.
func (a *App) buildLLMRequest(ctx context.Context, action, prompt, sqlText, errorText string, columns []string, rows [][]string) LLMRequest {
	var resultCtx *LLMResultContext
	if len(columns) > 0 || len(rows) > 0 {
		resultCtx = summarizeLLMResult(columns, rows)
	}
	schema := ""
	if action != llmActionReviewSQL {
		schema = a.llmSchemaContext(ctx, action, prompt, sqlText, errorText)
	}
	return LLMRequest{
		Action: action,
		Prompt: prompt,
		Schema: schema,
		SQL:    sqlText,
		Error:  errorText,
		Result: resultCtx,
	}
}

// apiLLMPreviewHandler returns the exact system/user messages that would be
// sent to the configured LLM for the given action/prompt/sql/error/result —
// without actually calling the LLM. This powers the SQL editor's "Details"
// panel so users can see (and trust) what data leaves the app.
func (a *App) apiLLMPreviewHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action  string     `json:"action"`
		Prompt  string     `json:"prompt"`
		SQL     string     `json:"sql"`
		Error   string     `json:"error"`
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}
	action := strings.TrimSpace(body.Action)
	switch action {
	case llmActionGenerateSQL, llmActionFixSQL, llmActionOptimizeSQL, llmActionExplainResults, llmActionExplainError, llmActionAskRun, llmActionCreateChart, llmActionReviewSQL, llmActionSuggestQuestions, llmActionAnalyzeQuality:
	default:
		action = llmActionAskRun
	}

	req := a.buildLLMRequest(r.Context(), action,
		strings.TrimSpace(body.Prompt), strings.TrimSpace(body.SQL), strings.TrimSpace(body.Error),
		body.Columns, body.Rows)

	// "system"/"user" are the ground truth — byte-for-byte what a real
	// request would send (minified JSON and all) — so charCount reflects
	// the actual payload size. "userDisplay" is a separate, pretty-printed
	// rendering for the Details panel; it's never the value used to compute
	// size or sent anywhere, just easier to read.
	messages := buildLLMMessages(req)
	displayMessages := buildLLMMessagesForDisplay(req)
	system, user, userDisplay := "", "", ""
	if len(messages) > 0 {
		system = messages[0].Content
	}
	if len(messages) > 1 {
		user = messages[1].Content
	}
	if len(displayMessages) > 1 {
		userDisplay = displayMessages[1].Content
	}
	settings := a.runtimeSettingsView()
	a.writeJSON(w, http.StatusOK, map[string]interface{}{
		"configured":  a.llmClient() != nil,
		"model":       settings.LLMModel,
		"baseURL":     settings.LLMBaseURL,
		"system":      system,
		"user":        user,
		"userDisplay": userDisplay,
		"charCount":   len(system) + len(user),
	})
}

// apiLLMHealthHandler performs a tiny server-side completion request so admins
// can verify remote OpenAI-compatible providers such as LM Studio.
func (a *App) apiLLMHealthHandler(w http.ResponseWriter, r *http.Request) {
	llm := a.llmClient()
	if llm == nil {
		a.writeProblem(w, r, http.StatusServiceUnavailable, "LLM is not configured", "LLM is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := llm.Complete(ctx, LLMRequest{
		Action: "health_check",
		Prompt: "Reply with OK.",
		Schema: `{"dialect":{"name":"health-check"},"skill":{"name":"health_check","purpose":"Verify provider connectivity.","instructions":["Reply with OK."],"output_contract":"Return plain text."},"retrieval":{"truncated":false},"tables":[]}`,
	})
	if err != nil {
		a.writeProblem(w, r, http.StatusBadGateway, "LLM request failed", err.Error())
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "response": trimForLLM(out, 200)})
}

// apiLLMRunHandler turns a natural-language prompt into a read-only SQL query,
// executes it, and asks the LLM for a short result/error explanation.
func (a *App) apiLLMRunHandler(w http.ResponseWriter, r *http.Request) {
	llm := a.llmClient()
	if llm == nil {
		a.writeProblem(w, r, http.StatusServiceUnavailable, "LLM is not configured", "LLM is not configured")
		return
	}

	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "Request body must be valid JSON.")
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		a.writeProblem(w, r, http.StatusBadRequest, "Missing prompt", "prompt is required")
		return
	}

	generated, err := a.generateSQLFromPrompt(r.Context(), prompt)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadGateway, "LLM request failed", err.Error())
		return
	}
	if generated.Action == "clarify" {
		a.writeJSON(w, http.StatusOK, map[string]any{
			"action":      "clarify",
			"explanation": generated.Explanation,
			"follow_up":   generated.FollowUp,
		})
		return
	}

	sqlText := strings.TrimSpace(generated.SQL)
	review := a.reviewGeneratedSQL(r.Context(), llm, prompt, sqlText)
	if err := a.validateAutoRunnableSQL(r.Context(), sqlText); err != nil {
		a.writeJSON(w, http.StatusOK, map[string]any{
			"action":      "blocked",
			"sql":         sqlText,
			"explanation": generated.Explanation,
			"review":      review,
			"error":       err.Error(),
		})
		return
	}

	result := a.executeSQL(r.Context(), sqlText)
	if result.Err != "" {
		explanation, explainErr := a.explainLLMError(r.Context(), prompt, sqlText, result.Err)
		if explainErr != nil {
			explanation = explainErr.Error()
		}
		a.writeJSON(w, http.StatusOK, map[string]any{
			"action":      "error",
			"sql":         sqlText,
			"error":       result.Err,
			"explanation": explanation,
			"review":      review,
		})
		return
	}

	explanation := generated.Explanation
	if aiText, err := a.explainLLMResults(r.Context(), prompt, sqlText, result); err == nil && strings.TrimSpace(aiText) != "" {
		explanation = aiText
	}

	a.writeJSON(w, http.StatusOK, map[string]any{
		"action":      "sql",
		"sql":         sqlText,
		"explanation": explanation,
		"review":      review,
		"columns":     result.Columns,
		"rows":        result.Rows,
		"affected":    result.Affected,
		"elapsed_ms":  result.Elapsed.Milliseconds(),
	})
}
