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
	"github.com/SimonWaldherr/datadock/internal/sqlutil"
	tinysql "github.com/SimonWaldherr/tinySQL"
)

// App holds the shared application state.
type App struct {
	nativeDB       *tinysql.DB
	sqlDB          *sql.DB
	tenant         string
	tpl            *template.Template
	settingsMu     sync.RWMutex
	llm            LLMClient
	llmConfig      LLMConfig
	auditLog       bool
	dialect        DialectProfile
	conns          *ConnectionManager
	connectTimeout time.Duration
	queryTimeout   time.Duration
	readOnlyMode   bool
	pageSize       int
	defaultTheme   string
	defaultDensity string
	verbose        *VerboseLogger
}

// Column describes a single column returned by a query.
type Column struct {
	Name     string
	TypeName string
}

func (a *App) setVerboseLogger(verbose *VerboseLogger) {
	a.settingsMu.Lock()
	a.verbose = verbose
	a.settingsMu.Unlock()
	if a.conns != nil {
		a.conns.SetVerbose(verbose)
	}
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

// QueryResult holds the result of an arbitrary SQL query.
type QueryResult struct {
	Columns  []string
	Rows     [][]string
	Affected int64
	Elapsed  time.Duration
	Err      string
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

	if objects := a.tinySQLCatalogObjects(ctx); len(objects) > 0 {
		return objects
	}
	return a.tinySQLNativeObjects(ctx)
}

func (a *App) tinySQLNativeObjects(ctx context.Context) []TableObject {
	objects := make([]TableObject, 0)
	seen := make(map[string]bool)
	tables := a.nativeDB.ListTables(a.tenant)
	for _, t := range tables {
		if t != nil && !isDataDockSystemObject(t.Name) {
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

func (a *App) tinySQLCatalogObjects(ctx context.Context) []TableObject {
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
		if isDataDockSystemObject(name) {
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
	names := make([]string, 0, len(objects))
	for _, obj := range objects {
		names = append(names, obj.Name)
	}
	return names
}

func (a *App) tableObjectKind(ctx context.Context, name string) string {
	for _, obj := range a.tableObjects(ctx) {
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
	meta := TableMeta{Name: found.Name, Kind: a.tableObjectKind(ctx, found.Name)}

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
	if rows, err := a.queryConn(ctx, a.localTinySQLConn(), "metadata.row_count", "SELECT COUNT(*) FROM "+quoteName(meta.Name)); err == nil {
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
	for _, obj := range a.tinySQLCatalogObjects(ctx) {
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
	if rows, err := a.queryConn(ctx, conn, "metadata.row_count", "SELECT COUNT(*) FROM "+conn.QuoteIdent(name)); err == nil {
		if rows.Next() {
			_ = rows.Scan(&meta.RowCount)
		}
		rows.Close()
	}
	return meta, nil
}

// tableRows returns a page of rows from a table.
func (a *App) tableRows(ctx context.Context, name string, page int) ([]Column, [][]string, error) {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	if page < 1 {
		page = 1
	}
	pageSize := a.currentPageSize()
	offset := (page - 1) * pageSize

	conn := a.activeConn(ctx)
	query := conn.selectPageSQL(name, "", "asc", pageSize, offset)
	rows, err := a.queryConn(ctx, conn, "table.rows", query)
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

// getRecord fetches a single record by id.
func (a *App) getRecord(ctx context.Context, table string, id string) ([]Column, []string, error) {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	rows, err := a.queryConn(ctx, conn, "record.get",
		fmt.Sprintf("SELECT * FROM %s WHERE %s = %s", conn.QuoteIdent(table), conn.QuoteIdent("id"), conn.Placeholder(1)), parseID(id))
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
	if rows, err := a.queryConn(ctx, conn, "record.next_id", "SELECT MAX("+conn.QuoteIdent("id")+") FROM "+conn.QuoteIdent(table)); err == nil {
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

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		conn.QuoteIdent(table),
		strings.Join(colNames, ", "),
		strings.Join(placeholders, ", "),
	)
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

	query := fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s = %s",
		conn.QuoteIdent(table),
		strings.Join(setClauses, ", "),
		conn.QuoteIdent("id"),
		conn.Placeholder(len(args)),
	)
	_, err := a.execConn(ctx, conn, "record.update", query, args...)
	return err
}

// deleteRecord deletes a record by id.
func (a *App) deleteRecord(ctx context.Context, table string, id string) error {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = %s", conn.QuoteIdent(table), conn.QuoteIdent("id"), conn.Placeholder(1))
	_, err := a.execConn(ctx, conn, "record.delete", query, parseID(id))
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
	result := QueryResult{}

	if !isResultQuerySQL(query) && a.currentReadOnlyMode() {
		result.Err = "maintenance mode is active: only SELECT/WITH/SHOW/EXPLAIN queries are allowed"
		result.Elapsed = time.Since(start)
		return result
	}

	if isResultQuerySQL(query) {
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

func (a *App) queryRows(ctx context.Context, query string) ([]string, [][]string, error) { //nolint:gosec
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	rows, err := a.queryConn(ctx, conn, "query.rows", query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanRows(rows *sql.Rows) ([]string, [][]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}

	result := make([][]string, 0)
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

func (c *DBConnection) tableObjects(ctx context.Context) ([]TableObject, error) {
	var query string
	switch c.Dialect.Name {
	case "SQLite":
		query = "SELECT name, type FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name"
	case "PostgreSQL":
		query = "SELECT CASE WHEN table_schema = 'public' THEN table_name ELSE table_schema || '.' || table_name END AS name, table_type FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema') AND table_type IN ('BASE TABLE','VIEW') ORDER BY table_schema, table_name"
	case "MariaDB/MySQL":
		query = "SELECT table_name, table_type FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type IN ('BASE TABLE','VIEW') ORDER BY table_name"
	case "Microsoft SQL Server":
		query = "SELECT table_schema + '.' + table_name AS name, table_type FROM information_schema.tables WHERE table_type IN ('BASE TABLE','VIEW') ORDER BY table_schema, table_name"
	default:
		query = "SELECT name, type FROM sqlite_master WHERE type IN ('table','view') ORDER BY name"
	}
	rows, err := c.queryContext(ctx, "metadata.table_objects", query)
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

func (c *DBConnection) tableNames(ctx context.Context) ([]string, error) {
	objects, err := c.tableObjects(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(objects))
	for _, obj := range objects {
		names = append(names, obj.Name)
	}
	return names, nil
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
		return TableMeta{}, fmt.Errorf("table %q not found", name)
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
	if rows, err := c.queryContext(ctx, "metadata.row_count", "SELECT COUNT(*) FROM "+c.QuoteIdent(canonical)); err == nil {
		if rows.Next() {
			_ = rows.Scan(&meta.RowCount)
		}
		rows.Close()
	}
	return meta, nil
}

func (c *DBConnection) selectPageSQL(table, sortCol, dir string, limit, offset int) string {
	quotedTable := c.QuoteIdent(table)
	orderClause := ""
	if sortCol != "" {
		orderClause = " ORDER BY " + c.QuoteIdent(sortCol)
		if dir == "desc" {
			orderClause += " DESC"
		} else {
			orderClause += " ASC"
		}
	}
	switch c.Dialect.Name {
	case "Microsoft SQL Server":
		if limit <= 0 {
			return "SELECT TOP 0 * FROM " + quotedTable
		}
		if orderClause == "" {
			orderClause = " ORDER BY (SELECT NULL)"
		}
		return fmt.Sprintf("SELECT * FROM %s%s OFFSET %d ROWS FETCH NEXT %d ROWS ONLY", quotedTable, orderClause, offset, limit)
	default:
		if limit <= 0 {
			return "SELECT * FROM " + quotedTable + " LIMIT 0"
		}
		return fmt.Sprintf("SELECT * FROM %s%s LIMIT %d OFFSET %d", quotedTable, orderClause, limit, offset)
	}
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
