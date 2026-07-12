package main

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dbcatalog "github.com/SimonWaldherr/datadock/internal/catalog"
	"github.com/SimonWaldherr/datadock/internal/resultutil"
	"github.com/SimonWaldherr/datadock/internal/sqlutil"
	tinysql "github.com/SimonWaldherr/tinySQL"
	"github.com/robfig/cron/v3"
)

// App holds the shared application state.
type App struct {
	nativeDB          *tinysql.DB
	sqlDB             *sql.DB
	tenant            string
	tpl               *template.Template
	settingsMu        sync.RWMutex
	llm               LLMClient
	llmConfig         LLMConfig
	embeddingClient   EmbeddingClient
	embeddingConfig   EmbeddingConfig
	auditLog          bool
	dialect           DialectProfile
	conns             *ConnectionManager
	connectTimeout    time.Duration
	queryTimeout      time.Duration
	readOnlyMode      bool
	pageSize          int
	matchMaxRows      int
	defaultTheme      string
	defaultDensity    string
	port              int
	adminPasswordHash string
	verbose           *VerboseLogger
	audit             *AuditLogger
	authMode          AuthMode

	// listenAddr and allowInsecureRemote are set once at startup (see
	// main.go) and read-only afterward: they record where the HTTP server
	// actually bound, so applyRuntimeSettings can refuse to switch into
	// AuthModeNone on a server that's reachable beyond localhost, even if
	// that switch is later requested at runtime rather than via flags.
	listenAddr          string
	allowInsecureRemote bool

	// authModeExplicit records whether -auth-mode/$DATADOCK_AUTH_MODE was
	// set for this process, as opposed to defaulting silently. It's set
	// once at startup (main.go) and gates whether the first-run setup page
	// shows the "Nur ich / Team" mode chooser: an operator who already told
	// DataDock which mode to use shouldn't be asked again.
	authModeExplicit bool

	// Sessions are process-local on purpose: user accounts persist in
	// __datadock_users, but every restart requires a fresh login before
	// credential storage or server-wide settings can be changed.
	adminAuthMu         sync.Mutex
	adminAuthedSessions map[string]sessionAuth

	// matchCron runs saved Match Configurations (see match_config.go) on a
	// cron schedule (see match_schedule.go). It is DataDock's own scheduler,
	// separate from tinySQL's built-in job scheduler, because that one only
	// ever executes raw SQL text against the embedded tinySQL engine, not
	// arbitrary Go operations like a cross-connection match run.
	matchCronMu      sync.Mutex
	matchCron        *cron.Cron
	matchCronEntries map[string]cron.EntryID

	// logicIndexMu guards concurrent SQL-logic reindex runs (see
	// logic_search.go): logicIndexing marks a connection ID as currently
	// being reindexed (rejecting a second concurrent trigger for the same
	// connection) and logicIndexStatus holds the last completed run's report
	// so the connections page can show it after a reload.
	logicIndexMu     sync.Mutex
	logicIndexing    map[string]bool
	logicIndexStatus map[string]LogicIndexReport
}

// sessionAuth is the value stored per authenticated session: which user it
// belongs to, their role at the time of login, and when the session expires.
// Role is snapshotted at login rather than looked up fresh on every request
// so a role change takes effect on that user's next login, not mid-session —
// consistent with how a password change today doesn't invalidate other live
// sessions either (see users.go).
type sessionAuth struct {
	Username string
	Role     Role
	Expiry   time.Time
}

// Column describes a single column returned by a query.
type Column struct {
	Name     string `json:"name"`
	TypeName string `json:"typeName"`
}

func (a *App) setVerboseLogger(verbose *VerboseLogger) {
	a.settingsMu.Lock()
	a.verbose = verbose
	a.settingsMu.Unlock()
	if a.conns != nil {
		a.conns.SetVerbose(verbose)
	}
}

func (a *App) setAuditLogger(audit *AuditLogger) {
	a.settingsMu.Lock()
	a.audit = audit
	a.auditLog = audit.Enabled()
	a.settingsMu.Unlock()
}

func (a *App) currentAuthMode() AuthMode {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	if a.authMode == "" {
		return AuthModeLocal
	}
	return a.authMode
}

func (a *App) auditLogger() *AuditLogger {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.audit
}

func (a *App) verboseLogger() *VerboseLogger {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.verbose
}

func (a *App) queryConn(ctx context.Context, conn *DBConnection, operation, query string, args ...any) (*sql.Rows, error) {
	if conn == nil || conn.DB == nil {
		return nil, fmt.Errorf("database connection is not available")
	}
	start := time.Now()
	if a.verboseLogger().Enabled() {
		a.verboseLogger().Log(VerboseEvent{
			System:    "database",
			Direction: "outbound",
			Operation: operation,
			Target:    conn.verboseTarget(),
			SQL:       query,
			ArgsCount: len(args),
		})
	}
	rows, err := conn.DB.QueryContext(ctx, query, args...)
	if a.verboseLogger().Enabled() {
		event := VerboseEvent{
			System:    "database",
			Direction: "inbound",
			Operation: operation,
			Target:    conn.verboseTarget(),
			Duration:  time.Since(start),
			Status:    "ok",
		}
		if err != nil {
			event.Status = "error"
			event.Error = err.Error()
		}
		a.verboseLogger().Log(event)
	}
	return rows, err
}

func (a *App) execConn(ctx context.Context, conn *DBConnection, operation, query string, args ...any) (sql.Result, error) {
	if conn == nil || conn.DB == nil {
		return nil, fmt.Errorf("database connection is not available")
	}
	start := time.Now()
	if a.verboseLogger().Enabled() {
		a.verboseLogger().Log(VerboseEvent{
			System:    "database",
			Direction: "outbound",
			Operation: operation,
			Target:    conn.verboseTarget(),
			SQL:       query,
			ArgsCount: len(args),
		})
	}
	res, err := conn.DB.ExecContext(ctx, query, args...)
	if a.verboseLogger().Enabled() {
		event := VerboseEvent{
			System:    "database",
			Direction: "inbound",
			Operation: operation,
			Target:    conn.verboseTarget(),
			Duration:  time.Since(start),
			Status:    "ok",
		}
		if err != nil {
			event.Status = "error"
			event.Error = err.Error()
		} else if n, nerr := res.RowsAffected(); nerr == nil {
			event.Preview = fmt.Sprintf("rows_affected=%d", n)
		}
		a.verboseLogger().Log(event)
	}
	return res, err
}

// TableMeta holds metadata about a table.
type TableMeta struct {
	Name     string
	Kind     string
	Columns  []Column
	HasID    bool
	RowCount int
}

// TableObject describes a browsable database object in the active connection.
type TableObject struct {
	Name string
	Kind string
}

// ColumnDetail describes one column for the read-only "Structure" view:
// richer than Column (name/type only), with nullability/default/primary-key
// information used to render a proper structure table.
type ColumnDetail struct {
	Name     string `json:"name"`
	TypeName string `json:"typeName"`
	// Nullable is "yes", "no", or "unknown" (when the dialect doesn't expose
	// this cheaply, e.g. tinySQL) — a tri-state rather than a bool so the UI
	// doesn't overstate confidence by defaulting to "yes".
	Nullable   string `json:"nullable"`
	Default    string `json:"default,omitempty"`
	PrimaryKey bool   `json:"primaryKey"`
}

// TableScript holds ready-to-run SQL snippets and metadata for a table/view,
// generated server-side (so dialect-specific quoting/TOP-vs-LIMIT syntax and
// DDL retrieval are handled once) for the sidebar's SSMS-style quick
// actions: "Select Top 1000 Rows", "Script as INSERT/UPDATE", the read-only
// "Structure" view, and a view's CREATE/ALTER definition.
type TableScript struct {
	Name              string             `json:"name"`
	Kind              string             `json:"kind"`
	HasID             bool               `json:"hasId"`
	Columns           []Column           `json:"columns"`
	SelectTop         string             `json:"selectTop"`
	InsertTmpl        string             `json:"insertTmpl,omitempty"`
	UpdateTmpl        string             `json:"updateTmpl,omitempty"`
	Structure         []ColumnDetail     `json:"structure,omitempty"`
	CreateSQL         string             `json:"createSQL,omitempty"`         // views only: the view's CREATE statement
	AlterSQL          string             `json:"alterSQL,omitempty"`          // views only: an ALTER VIEW variant, when the dialect supports it
	DDLError          string             `json:"ddlError,omitempty"`          // views only: why CreateSQL couldn't be fetched
	DependsOn         []ObjectDependency `json:"dependsOn,omitempty"`         // what this object references
	Dependents        []ObjectDependency `json:"dependents,omitempty"`        // what references this object
	DependenciesError string             `json:"dependenciesError,omitempty"` // why dependency analysis wasn't available
}

// buildTableScript assembles the quick-action SQL snippets, structure, and
// (for views) DDL for meta using the currently active connection's dialect.
func (a *App) buildTableScript(ctx context.Context, meta TableMeta) TableScript {
	conn := a.activeConn(ctx)
	script := TableScript{Name: meta.Name, Kind: meta.Kind, HasID: meta.HasID, Columns: meta.Columns}
	script.SelectTop = conn.selectPageSQL(meta.Name, "", "", 1000, 0)
	script.Structure = conn.buildColumnStructure(ctx, meta)
	if meta.Kind != "view" {
		script.InsertTmpl = buildInsertTemplateSQL(conn, meta)
		script.UpdateTmpl = buildUpdateTemplateSQL(conn, meta)
	} else {
		createSQL, err := conn.fetchViewDefinition(ctx, meta.Name)
		if err != nil {
			script.DDLError = err.Error()
		} else {
			script.CreateSQL = createSQL
			script.AlterSQL = viewCreateToAlter(conn.Dialect.Name, createSQL)
		}
	}
	dependsOn, dependents, err := conn.fetchDependencies(ctx, meta.Name, meta.Kind)
	if err != nil {
		script.DependenciesError = err.Error()
	} else {
		script.DependsOn = dependsOn
		script.Dependents = dependents
	}
	return script
}

// QueryResult holds the result of an arbitrary SQL query.
type QueryResult struct {
	Columns        []string
	Rows           [][]string
	Affected       int64
	Elapsed        time.Duration
	Err            string
	StatementClass sqlutil.StatementClass
	Offset         int
	Limit          int
	HasMore        bool
}

func (a *App) activeConn(ctx context.Context) *DBConnection {
	if conn := activeConnectionFromContext(ctx); conn != nil {
		return conn
	}
	if a.conns != nil {
		if conn := a.conns.Active(); conn != nil {
			return conn
		}
	}
	return a.localTinySQLConn()
}

func (a *App) localTinySQLConn() *DBConnection {
	return &DBConnection{
		ID:      defaultConnectionID,
		Name:    "tinySQL",
		Kind:    "tinysql",
		Dialect: DialectProfileForName("tinysql"),
		DB:      a.sqlDB,
		Native:  a.nativeDB,
		Verbose: a.verboseLogger(),
	}
}

func (a *App) withQueryTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := a.currentQueryTimeout()
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

func (a *App) withConnectTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := a.currentConnectTimeout()
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// tableObjects returns a sorted list of browsable tables and views.
func (a *App) tableObjects(ctx context.Context) []TableObject {
	return a.tableObjectsWithSystem(ctx, false)
}

func (a *App) tableObjectsWithSystem(ctx context.Context, includeSystem bool) []TableObject {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	if conn == nil {
		return nil
	}
	if !conn.IsTinySQL() {
		objects, err := conn.tableObjects(ctx)
		if err == nil {
			return objects
		}
		return nil
	}

	if objects := a.tinySQLCatalogObjects(ctx, includeSystem); len(objects) > 0 {
		return objects
	}
	return a.tinySQLNativeObjects(ctx, includeSystem)
}

func (a *App) tinySQLNativeObjects(ctx context.Context, includeSystem bool) []TableObject {
	objects := make([]TableObject, 0)
	seen := make(map[string]bool)
	tables := a.nativeDB.ListTables(a.tenant)
	for _, t := range tables {
		if t != nil && (includeSystem || !isDataDockSystemObject(t.Name)) {
			objects = append(objects, TableObject{Name: t.Name, Kind: "table"})
			seen[strings.ToLower(t.Name)] = true
		}
	}
	for _, view := range a.tinySQLViewNames(ctx) {
		if !seen[strings.ToLower(view)] {
			objects = append(objects, TableObject{Name: view, Kind: "view"})
			seen[strings.ToLower(view)] = true
		}
	}
	sortTableObjects(objects)
	return objects
}

func (a *App) tinySQLCatalogObjects(ctx context.Context, includeSystem bool) []TableObject {
	objects, err := dbcatalog.ListObjects(ctx, a.nativeDB, a.tenant)
	if err != nil {
		return nil
	}
	out := make([]TableObject, 0, len(objects))
	seen := make(map[string]bool)
	for _, obj := range objects {
		name := strings.TrimSpace(obj.Name)
		if name == "" {
			continue
		}
		if !includeSystem && isDataDockSystemObject(name) {
			continue
		}
		kind, ok := normalizeCatalogObjectKind(obj.Type)
		if !ok {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		out = append(out, TableObject{Name: name, Kind: kind})
		seen[key] = true
	}
	sortTableObjects(out)
	return out
}

// tableNames returns a sorted list of table/view names for the current tenant.
func (a *App) tableNames(ctx context.Context) []string {
	objects := a.tableObjects(ctx)
	return tableObjectNames(objects)
}

func tableObjectNames(objects []TableObject) []string {
	names := make([]string, 0, len(objects))
	for _, obj := range objects {
		names = append(names, obj.Name)
	}
	return names
}

func (a *App) tableObjectKindWithSystem(ctx context.Context, name string, includeSystem bool) string {
	for _, obj := range a.tableObjectsWithSystem(ctx, includeSystem) {
		if strings.EqualFold(obj.Name, name) {
			return obj.Kind
		}
	}
	return "table"
}

// tableMeta returns column metadata (and whether an `id` column exists) for a
// table. It uses the native DB for schema info (immune to LIMIT-0 issue).
func (a *App) tableMeta(ctx context.Context, name string) (TableMeta, error) {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	if conn == nil {
		return TableMeta{}, fmt.Errorf("no active connection")
	}
	if !conn.IsTinySQL() {
		return conn.tableMeta(ctx, name)
	}
	tables := a.nativeDB.ListTables(a.tenant)
	var found *tinysql.Table
	for _, t := range tables {
		if t != nil && strings.EqualFold(t.Name, name) {
			found = t
			break
		}
	}
	if found == nil {
		for _, viewName := range a.tinySQLViewNames(ctx) {
			if strings.EqualFold(viewName, name) {
				return a.queryBackedMeta(ctx, viewName, "view")
			}
		}
		return TableMeta{}, fmt.Errorf("table %q not found", name)
	}

	// Use the canonical name from the DB (not the user-provided name) for
	// all subsequent operations to avoid tainted-identifier issues.
	meta := TableMeta{Name: found.Name, Kind: a.tableObjectKindWithSystem(ctx, found.Name, true)}

	for _, sc := range found.Cols {
		typeName := sc.Type.String()
		if typeName == "" {
			typeName = "TEXT"
		}
		col := Column{Name: sc.Name, TypeName: typeName}
		meta.Columns = append(meta.Columns, col)
		if strings.EqualFold(sc.Name, "id") {
			meta.HasID = true
		}
	}

	// Row count (best-effort; ignore error). Use the DB-sourced meta.Name, not
	// the user-provided name, when building the SQL query.
	if rows, err := a.queryConn(ctx, a.localTinySQLConn(), "metadata.row_count", nativeRowCountQuery(meta.Name)); err == nil {
		if rows.Next() {
			_ = rows.Scan(&meta.RowCount)
		}
		rows.Close()
	}

	return meta, nil
}

func (a *App) tinySQLViewNames(ctx context.Context) []string {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	var names []string
	for _, obj := range a.tinySQLCatalogObjects(ctx, false) {
		if obj.Kind == "view" {
			names = append(names, obj.Name)
		}
	}
	sort.Strings(names)
	return names
}

func (a *App) queryBackedMeta(ctx context.Context, name, kind string) (TableMeta, error) {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	query := conn.selectPageSQL(name, "", "asc", 0, 0)
	rows, err := a.queryConn(ctx, conn, "metadata.query_backed_meta", query)
	if err != nil {
		return TableMeta{}, err
	}
	colTypes, err := rows.ColumnTypes()
	rows.Close()
	if err != nil {
		return TableMeta{}, err
	}
	meta := TableMeta{Name: name, Kind: kind}
	for _, ct := range colTypes {
		typeName := ct.DatabaseTypeName()
		if typeName == "" {
			typeName = "TEXT"
		}
		col := Column{Name: ct.Name(), TypeName: typeName}
		meta.Columns = append(meta.Columns, col)
		if strings.EqualFold(col.Name, "id") {
			meta.HasID = true
		}
	}
	if rows, err := a.queryConn(ctx, conn, "metadata.row_count", rowCountQuery(conn, name)); err == nil {
		if rows.Next() {
			_ = rows.Scan(&meta.RowCount)
		}
		rows.Close()
	}
	return meta, nil
}

// getRecord fetches a single record by id.
func (a *App) getRecord(ctx context.Context, table string, id string) ([]Column, []string, error) {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	rows, err := a.queryConn(ctx, conn, "record.get", recordGetQuery(conn, table), parseID(id))
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

	if !rows.Next() {
		return nil, nil, sql.ErrNoRows
	}
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
	return cols, row, nil
}

// insertRecord inserts a new record into a table, assigning the next id.
func (a *App) insertRecord(ctx context.Context, table string, values map[string]string, cols []Column) error {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	// Determine next id via MAX(id)+1.
	conn := a.activeConn(ctx)
	var maxID sql.NullInt64
	if rows, err := a.queryConn(ctx, conn, "record.next_id", recordMaxIDQuery(conn, table)); err == nil {
		if rows.Next() {
			_ = rows.Scan(&maxID)
		}
		rows.Close()
	}
	nextID := maxID.Int64 + 1

	colNames := make([]string, 0, len(cols))
	args := make([]interface{}, 0, len(cols))

	// Always include id first.
	colNames = append(colNames, conn.QuoteIdent("id"))
	args = append(args, nextID)

	for _, col := range cols {
		if strings.EqualFold(col.Name, "id") {
			continue
		}
		colNames = append(colNames, conn.QuoteIdent(col.Name))
		args = append(args, values[col.Name])
	}

	placeholders := make([]string, len(colNames))
	for i := range placeholders {
		placeholders[i] = conn.Placeholder(i + 1)
	}

	query := recordInsertQuery(conn, table, colNames, placeholders)
	_, err := a.execConn(ctx, conn, "record.insert", query, args...)
	return err
}

// updateRecord updates an existing record identified by id.
func (a *App) updateRecord(ctx context.Context, table string, id string, values map[string]string, cols []Column) error {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	setClauses := make([]string, 0, len(cols))
	args := make([]interface{}, 0, len(cols)+1)

	for _, col := range cols {
		if strings.EqualFold(col.Name, "id") {
			continue
		}
		setClauses = append(setClauses, conn.QuoteIdent(col.Name)+" = "+conn.Placeholder(len(args)+1))
		args = append(args, values[col.Name])
	}
	if len(setClauses) == 0 {
		return nil
	}
	args = append(args, parseID(id))

	query := recordUpdateQuery(conn, table, setClauses, conn.Placeholder(len(args)))
	_, err := a.execConn(ctx, conn, "record.update", query, args...)
	return err
}

// deleteRecord deletes a record by id.
func (a *App) deleteRecord(ctx context.Context, table string, id string) error {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	_, err := a.execConn(ctx, conn, "record.delete", recordDeleteQuery(conn, table), parseID(id))
	return err
}

// executeSQL runs an arbitrary SQL statement supplied by the user via the SQL
// editor and returns column/row results. Executing user-supplied SQL is the
// explicit purpose of this function; callers MUST ensure the request comes from
// an authenticated session before invoking it.
func (a *App) executeSQL(ctx context.Context, query string) QueryResult { //nolint:gosec
	start := time.Now()
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	result := QueryResult{StatementClass: classifySQL(query)}

	if !isResultQuerySQL(query) && a.currentReadOnlyMode() {
		result.Err = resultQueryRequiredMessage("maintenance mode is active")
		result.Elapsed = time.Since(start)
		return result
	}

	if queryReturnsRowsInEditor(result.StatementClass) {
		cols, rows, err := a.queryRows(ctx, query)
		if err != nil {
			result.Err = err.Error()
			result.Elapsed = time.Since(start)
			return result
		}
		result.Columns = cols
		result.Rows = rows
	} else {
		conn := a.activeConn(ctx)
		res, err := a.execConn(ctx, conn, "query.exec", query)
		if err != nil {
			result.Err = err.Error()
			result.Elapsed = time.Since(start)
			return result
		}
		n, _ := res.RowsAffected()
		result.Affected = n
	}

	result.Elapsed = time.Since(start)
	return result
}

func (a *App) executeSQLWindow(ctx context.Context, query string, offset, limit int) QueryResult { //nolint:gosec
	start := time.Now()
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	result := QueryResult{
		StatementClass: classifySQL(query),
		Offset:         offset,
		Limit:          limit,
	}

	if !isResultQuerySQL(query) && a.currentReadOnlyMode() {
		result.Err = resultQueryRequiredMessage("maintenance mode is active")
		result.Elapsed = time.Since(start)
		return result
	}

	if queryReturnsRowsInEditor(result.StatementClass) {
		cols, rows, hasMore, err := a.queryRowsWindow(ctx, query, offset, limit)
		if err != nil {
			result.Err = err.Error()
			result.Elapsed = time.Since(start)
			return result
		}
		result.Columns = cols
		result.Rows = rows
		result.HasMore = hasMore
	} else {
		conn := a.activeConn(ctx)
		res, err := a.execConn(ctx, conn, "query.exec", query)
		if err != nil {
			result.Err = err.Error()
			result.Elapsed = time.Since(start)
			return result
		}
		n, _ := res.RowsAffected()
		result.Affected = n
	}

	result.Elapsed = time.Since(start)
	return result
}

func (a *App) queryRows(ctx context.Context, query string) ([]string, [][]string, error) { //nolint:gosec
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	if conn != nil && conn.IsTinySQL() {
		return a.queryTinySQLRows(ctx, query)
	}
	rows, err := a.queryConn(ctx, conn, "query.rows", query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

func (a *App) queryRowsWindow(ctx context.Context, query string, offset, limit int) ([]string, [][]string, bool, error) { //nolint:gosec
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	if conn != nil && conn.IsTinySQL() {
		cols, rows, err := a.queryTinySQLRows(ctx, query)
		if err != nil {
			return nil, nil, false, err
		}
		outCols, outRows, hasMore := sliceRowsWindow(cols, rows, offset, limit)
		return outCols, outRows, hasMore, nil
	}
	rows, err := a.queryConn(ctx, conn, "query.rows", query)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	return scanRowsWindow(rows, offset, limit)
}

func (a *App) queryTinySQLRows(ctx context.Context, query string) ([]string, [][]string, error) {
	rs, err := tinysql.ExecSQL(ctx, a.nativeDB, a.tenant, query)
	if err != nil {
		return nil, nil, err
	}
	cols, rows := resultutil.ResultSetToStringMatrix(rs)
	return cols, rows, nil
}

func scanRows(rows *sql.Rows) ([]string, [][]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}

	result := make([][]string, 0)
	for rows.Next() {
		row, err := scanSQLRow(rows, cols)
		if err != nil {
			return nil, nil, err
		}
		result = append(result, row)
	}
	return cols, result, rows.Err()
}

func scanRowsWindow(rows *sql.Rows, offset, limit int) ([]string, [][]string, bool, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 500
	}

	result := make([][]string, 0, limit)
	seen := 0
	hasMore := false
	for rows.Next() {
		row, err := scanSQLRow(rows, cols)
		if err != nil {
			return nil, nil, false, err
		}
		if seen < offset {
			seen++
			continue
		}
		if len(result) >= limit {
			hasMore = true
			break
		}
		result = append(result, row)
		seen++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	return cols, result, hasMore, nil
}

func sliceRowsWindow(cols []string, rows [][]string, offset, limit int) ([]string, [][]string, bool) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 500
	}
	if offset >= len(rows) {
		return cols, nil, false
	}
	end := offset + limit
	hasMore := end < len(rows)
	if end > len(rows) {
		end = len(rows)
	}
	return cols, rows[offset:end], hasMore
}

func scanSQLRow(rows *sql.Rows, cols []string) ([]string, error) {
	vals := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	row := make([]string, len(cols))
	for i, v := range vals {
		row[i] = anyToString(v)
	}
	return row, nil
}

func (c *DBConnection) tableObjects(ctx context.Context) ([]TableObject, error) {
	rows, err := c.queryContext(ctx, "metadata.table_objects", tableObjectsQuery(c))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []TableObject
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		objects = append(objects, TableObject{Name: name, Kind: normalizeDBObjectKind(typ)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortTableObjects(objects)
	return objects, nil
}

func (c *DBConnection) tableMeta(ctx context.Context, name string) (TableMeta, error) {
	objects, err := c.tableObjects(ctx)
	if err != nil {
		return TableMeta{}, err
	}
	canonical := ""
	kind := "table"
	for _, obj := range objects {
		if strings.EqualFold(obj.Name, name) {
			canonical = obj.Name
			kind = obj.Kind
			break
		}
	}
	if canonical == "" {
		// Not in this connection's default database/schema scope — this
		// happens for a fully-qualified name from the multi-database catalog
		// tree (e.g. "otherdb.dbo.orders" on SQL Server, or "otherdb.orders"
		// on MySQL, both queryable cross-database on a single connection).
		// Trust the caller-supplied qualified identifier instead of
		// requiring exact tableObjects() membership, which only ever lists
		// the connection's own default database.
		if strings.Contains(name, ".") {
			canonical = name
			kind = c.probeObjectKind(ctx, name)
		} else {
			return TableMeta{}, fmt.Errorf("table %q not found", name)
		}
	}
	query := c.selectPageSQL(canonical, "", "asc", 0, 0)
	rows, err := c.queryContext(ctx, "metadata.table_meta", query)
	if err != nil {
		return TableMeta{}, err
	}
	colTypes, err := rows.ColumnTypes()
	rows.Close()
	if err != nil {
		return TableMeta{}, err
	}
	meta := TableMeta{Name: canonical, Kind: kind}
	for _, ct := range colTypes {
		typeName := ct.DatabaseTypeName()
		if typeName == "" {
			typeName = "TEXT"
		}
		col := Column{Name: ct.Name(), TypeName: typeName}
		meta.Columns = append(meta.Columns, col)
		if strings.EqualFold(col.Name, "id") {
			meta.HasID = true
		}
	}
	if rows, err := c.queryContext(ctx, "metadata.row_count", rowCountQuery(c, canonical)); err == nil {
		if rows.Next() {
			_ = rows.Scan(&meta.RowCount)
		}
		rows.Close()
	}
	return meta, nil
}

func sortTableObjects(objects []TableObject) {
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Kind != objects[j].Kind {
			return objects[i].Kind < objects[j].Kind
		}
		return strings.ToLower(objects[i].Name) < strings.ToLower(objects[j].Name)
	})
}

func normalizeDBObjectKind(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.Contains(s, "view"):
		return "view"
	default:
		return "table"
	}
}

func normalizeCatalogObjectKind(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.Contains(s, "view"):
		return "view", true
	case s == "table", strings.Contains(s, "base table"):
		return "table", true
	default:
		return "", false
	}
}

func isDataDockSystemObject(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "__datadock_")
}

func isResultQuerySQL(query string) bool {
	return sqlutil.IsResultProducing(query)
}

func queryReturnsRowsInEditor(class sqlutil.StatementClass) bool {
	return class == sqlutil.StatementReadQuery || class == sqlutil.StatementProcedureCall
}

func resultQueryRequiredMessage(prefix string) string {
	msg := "only SELECT/WITH/SHOW/EXPLAIN/DESCRIBE/PRAGMA queries are allowed"
	if strings.TrimSpace(prefix) == "" {
		return msg
	}
	return prefix + ": " + msg
}

func classifySQL(query string) sqlutil.StatementClass {
	return sqlutil.Classify(query)
}

// quoteName wraps a table or column name in double-quotes for safety.
func quoteName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// isValidIdentifier checks that a name contains only alphanumerics and
// underscores, preventing unexpected characters in SQL identifiers even when
// combined with quoteName.
func isValidIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}

// sanitizeIdentifier returns a copy of name containing only characters that
// pass isValidIdentifier (letters, digits, underscores). Combined with a prior
// isValidIdentifier guard, the returned string is identical to the input; the
// function's purpose is to break the taint-tracking data flow from user input
// so that static analysis tools can confirm the value is safe.
func sanitizeIdentifier(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			out = append(out, c)
		}
	}
	return string(out)
}

// parseID tries to parse a record id string as an int64. Falls back to the
// original string if it cannot be parsed (e.g. UUID primary keys).
func parseID(id string) interface{} {
	if n, err := strconv.ParseInt(id, 10, 64); err == nil {
		return n
	}
	return id
}

// anyToString converts any SQL value to a display string.
func anyToString(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
