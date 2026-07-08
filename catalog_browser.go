package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// CatalogItem is a single browsable object (table, view, or procedure/function).
type CatalogItem struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // "table" | "view" | "procedure" | "function"
}

// CatalogSchema groups the objects that live under one schema (or, for
// dialects without a separate schema concept such as MySQL/SQLite/tinySQL,
// the single implicit schema of a database).
type CatalogSchema struct {
	Name       string        `json:"name"`
	Tables     []CatalogItem `json:"tables,omitempty"`
	Views      []CatalogItem `json:"views,omitempty"`
	Procedures []CatalogItem `json:"procedures,omitempty"`
}

// CatalogDatabase groups the schemas that live under one database on the
// connected server. For single-database dialects (SQLite, tinySQL) there is
// always exactly one CatalogDatabase with an empty Name.
type CatalogDatabase struct {
	Name       string          `json:"name"`
	Current    bool            `json:"current"`
	NeedsFetch bool            `json:"needsFetch,omitempty"`
	Schemas    []CatalogSchema `json:"schemas,omitempty"`
}

func (a *App) catalogTreeWithSystem(ctx context.Context, includeSystem bool) ([]CatalogDatabase, error) {
	ctx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	conn := a.activeConn(ctx)
	if conn == nil {
		return nil, fmt.Errorf("no active connection")
	}
	if conn.IsTinySQL() {
		objects := a.tableObjectsWithSystem(ctx, includeSystem)
		return []CatalogDatabase{{Name: "", Current: true, Schemas: []CatalogSchema{catalogSchemaFromObjects("", objects)}}}, nil
	}
	return conn.ListCatalog(ctx)
}

func catalogSchemaFromObjects(schema string, objects []TableObject) CatalogSchema {
	s := CatalogSchema{Name: schema}
	for _, o := range objects {
		item := CatalogItem(o)
		if o.Kind == "view" {
			s.Views = append(s.Views, item)
		} else {
			s.Tables = append(s.Tables, item)
		}
	}
	return s
}

// ListCatalog discovers every database/schema/object this connection's
// credentials can see on the server, not just the single database named in
// the connection string. PostgreSQL cannot query across databases on one
// connection, so other databases are returned with NeedsFetch=true and are
// only populated on demand via ExpandCatalogDatabase. MySQL and SQL Server
// support cross-database queries (via a global information_schema, and
// three-part naming, respectively), so their full catalog is fetched eagerly
// in one pass.
func (c *DBConnection) ListCatalog(ctx context.Context) ([]CatalogDatabase, error) {
	switch c.Dialect.Name {
	case "PostgreSQL":
		return c.listCatalogPostgres(ctx)
	case "MariaDB/MySQL":
		return c.listCatalogMySQL(ctx)
	case "Microsoft SQL Server":
		return c.listCatalogMSSQL(ctx)
	default: // SQLite and anything unrecognized
		objects, err := c.tableObjects(ctx)
		if err != nil {
			return nil, err
		}
		return []CatalogDatabase{{Name: "", Current: true, Schemas: []CatalogSchema{catalogSchemaFromObjects("", objects)}}}, nil
	}
}

// ExpandCatalogDatabase lazily loads the schemas/tables/views/procedures for
// one database that ListCatalog returned with NeedsFetch=true: PostgreSQL
// (which needs a separate connection to browse another database) and SQL
// Server (which technically could list every database in one eager pass via
// three-part names, but for a real server with many databases — some with
// thousands of tables — that made every single page load do a sequential
// per-database round trip and occasionally exceed the query timeout,
// intermittently rendering the sidebar empty; see listCatalogMSSQL).
func (c *DBConnection) ExpandCatalogDatabase(ctx context.Context, database string) (CatalogDatabase, error) {
	switch c.Dialect.Name {
	case "PostgreSQL":
		return c.expandPostgresDatabase(ctx, database)
	case "Microsoft SQL Server":
		schemas, err := c.mssqlSchemasForDatabase(ctx, database)
		if err != nil {
			return CatalogDatabase{}, err
		}
		return CatalogDatabase{Name: database, Schemas: schemas}, nil
	default:
		// Other dialects already return everything eagerly from ListCatalog.
		all, err := c.ListCatalog(ctx)
		if err != nil {
			return CatalogDatabase{}, err
		}
		for _, db := range all {
			if strings.EqualFold(db.Name, database) {
				return db, nil
			}
		}
		return CatalogDatabase{}, fmt.Errorf("database %q not found", database)
	}
}

// ── PostgreSQL ──────────────────────────────────────────────────────────────

func (c *DBConnection) listCatalogPostgres(ctx context.Context) ([]CatalogDatabase, error) {
	current := c.queryScalar(ctx, pgCurrentDatabaseQuery)
	dbNames, err := c.queryStrings(ctx, pgListDatabasesQuery)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogDatabase, 0, len(dbNames))
	for _, name := range dbNames {
		isCurrent := strings.EqualFold(name, current)
		if !isCurrent {
			out = append(out, CatalogDatabase{Name: name, NeedsFetch: true})
			continue
		}
		schemas, err := c.postgresSchemas(ctx, c.queryContext)
		if err != nil {
			return nil, err
		}
		out = append(out, CatalogDatabase{Name: name, Current: true, Schemas: schemas})
	}
	return out, nil
}

func (c *DBConnection) expandPostgresDatabase(ctx context.Context, database string) (CatalogDatabase, error) {
	db, err := c.crossDatabaseHandle(database)
	if err != nil {
		return CatalogDatabase{}, err
	}
	queryFn := func(ctx context.Context, operation, query string, args ...any) (*sql.Rows, error) {
		return db.QueryContext(ctx, query, args...)
	}
	schemas, err := c.postgresSchemas(ctx, queryFn)
	if err != nil {
		return CatalogDatabase{}, err
	}
	return CatalogDatabase{Name: database, Schemas: schemas}, nil
}

type queryFunc func(ctx context.Context, operation, query string, args ...any) (*sql.Rows, error)

func (c *DBConnection) postgresSchemas(ctx context.Context, query queryFunc) ([]CatalogSchema, error) {
	bySchema := map[string]*CatalogSchema{}
	var order []string
	ensure := func(name string) *CatalogSchema {
		if s, ok := bySchema[name]; ok {
			return s
		}
		s := &CatalogSchema{Name: name}
		bySchema[name] = s
		order = append(order, name)
		return s
	}

	rows, err := query(ctx, "catalog.postgres.tables", pgListTablesQuery)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var schema, name, kind string
		if err := rows.Scan(&schema, &name, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		s := ensure(schema)
		item := CatalogItem{Name: name, Kind: normalizeDBObjectKind(kind)}
		if item.Kind == "view" {
			s.Views = append(s.Views, item)
		} else {
			s.Tables = append(s.Tables, item)
		}
	}
	rows.Close()

	procRows, err := query(ctx, "catalog.postgres.procedures", pgListProceduresQuery)
	if err == nil {
		for procRows.Next() {
			var schema, name, kind string
			if err := procRows.Scan(&schema, &name, &kind); err != nil {
				break
			}
			s := ensure(schema)
			s.Procedures = append(s.Procedures, CatalogItem{Name: name, Kind: kind})
		}
		procRows.Close()
	}

	sort.Strings(order)
	out := make([]CatalogSchema, 0, len(order))
	for _, name := range order {
		out = append(out, *bySchema[name])
	}
	return out, nil
}

// crossDatabaseHandle returns a cached (or newly opened) *sql.DB pointed at
// database on the same server, reusing this connection's host/credentials.
func (c *DBConnection) crossDatabaseHandle(database string) (*sql.DB, error) {
	c.crossDBMu.Lock()
	defer c.crossDBMu.Unlock()
	if c.crossDB == nil {
		c.crossDB = make(map[string]*sql.DB)
	}
	if db, ok := c.crossDB[database]; ok {
		return db, nil
	}
	dsn, ok := swapPostgresDSNDatabase(c.DSN, database)
	if !ok {
		return nil, fmt.Errorf("cannot determine a connection string for database %q", database)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	c.crossDB[database] = db
	return db, nil
}

var pgDBNameRe = regexp.MustCompile(`(?i)dbname=\S+`)

// swapPostgresDSNDatabase rewrites dsn (either postgres://... URL form or
// libpq keyword form) to point at newDB instead of whatever database it
// currently names, keeping host/user/credentials/params unchanged.
func swapPostgresDSNDatabase(dsn, newDB string) (string, bool) {
	dsn = strings.TrimSpace(dsn)
	lower := strings.ToLower(dsn)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", false
		}
		u.Path = "/" + newDB
		return u.String(), true
	}
	if pgDBNameRe.MatchString(dsn) {
		return pgDBNameRe.ReplaceAllString(dsn, "dbname="+newDB), true
	}
	if dsn == "" {
		return "dbname=" + newDB, true
	}
	return dsn + " dbname=" + newDB, true
}

// queryScalar runs a single-value query and returns "" on any error — used
// for best-effort metadata lookups (e.g. "which database is current") where
// a failure shouldn't abort the whole catalog listing.
func (c *DBConnection) queryScalar(ctx context.Context, query string) string {
	rows, err := c.queryContext(ctx, "catalog.scalar", query)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var v string
	if rows.Next() {
		_ = rows.Scan(&v)
	}
	return v
}

func (c *DBConnection) queryStrings(ctx context.Context, query string) ([]string, error) {
	rows, err := c.queryContext(ctx, "catalog.strings", query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ── MySQL / MariaDB ─────────────────────────────────────────────────────────

// listCatalogMySQL fetches every accessible database in a single query pair:
// MySQL's information_schema spans the whole server regardless of the
// connection's default database, so no reconnect is needed. In MySQL,
// "database" and "schema" are synonyms, so each CatalogDatabase gets exactly
// one unnamed CatalogSchema.
func (c *DBConnection) listCatalogMySQL(ctx context.Context) ([]CatalogDatabase, error) {
	current := c.queryScalar(ctx, mysqlCurrentDatabaseQuery)
	byDB := map[string]*CatalogSchema{}
	var order []string
	ensure := func(name string) *CatalogSchema {
		if s, ok := byDB[name]; ok {
			return s
		}
		s := &CatalogSchema{}
		byDB[name] = s
		order = append(order, name)
		return s
	}

	rows, err := c.queryContext(ctx, "catalog.mysql.tables", mysqlListTablesQuery)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var db, name, kind string
		if err := rows.Scan(&db, &name, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		s := ensure(db)
		item := CatalogItem{Name: name, Kind: normalizeDBObjectKind(kind)}
		if item.Kind == "view" {
			s.Views = append(s.Views, item)
		} else {
			s.Tables = append(s.Tables, item)
		}
	}
	rows.Close()

	procRows, err := c.queryContext(ctx, "catalog.mysql.procedures", mysqlListProceduresQuery)
	if err == nil {
		for procRows.Next() {
			var db, name, kind string
			if err := procRows.Scan(&db, &name, &kind); err != nil {
				break
			}
			ensure(db).Procedures = append(ensure(db).Procedures, CatalogItem{Name: name, Kind: strings.ToLower(kind)})
		}
		procRows.Close()
	}

	sort.Strings(order)
	out := make([]CatalogDatabase, 0, len(order))
	for _, name := range order {
		out = append(out, CatalogDatabase{Name: name, Current: strings.EqualFold(name, current), Schemas: []CatalogSchema{*byDB[name]}})
	}
	return out, nil
}

// ── Microsoft SQL Server ────────────────────────────────────────────────────

// listCatalogMSSQL lists every accessible database (via sys.databases) but,
// like PostgreSQL, only eagerly fetches the CURRENTLY connected database's
// schemas/tables — other databases come back with NeedsFetch=true and are
// only queried (via a three-part name: db.sys.objects, ...) when the user
// actually expands them. SQL Server *can* query cross-database on a single
// connection without reconnecting, so this is a performance/reliability
// choice rather than a technical necessity: eagerly walking every database
// on a real server (some with hundreds or thousands of tables) turned every
// page load into N sequential round trips inside one query timeout, which
// occasionally lost the race and rendered the sidebar empty.
func (c *DBConnection) listCatalogMSSQL(ctx context.Context) ([]CatalogDatabase, error) {
	current := c.queryScalar(ctx, mssqlCurrentDatabaseQuery)
	dbNames, err := c.queryStrings(ctx, mssqlListDatabasesQuery)
	if err != nil {
		return nil, err
	}
	// The current database's own system databases (e.g. master, if that's
	// what was connected to) should still be browsable.
	if current != "" && !containsFold(dbNames, current) {
		dbNames = append([]string{current}, dbNames...)
		sort.Strings(dbNames)
	}

	out := make([]CatalogDatabase, 0, len(dbNames))
	for _, dbName := range dbNames {
		isCurrent := strings.EqualFold(dbName, current)
		if !isCurrent {
			out = append(out, CatalogDatabase{Name: dbName, NeedsFetch: true})
			continue
		}
		schemas, err := c.mssqlSchemasForDatabase(ctx, dbName)
		if err != nil {
			// A database the login can connect to but not read metadata for
			// (permission edge cases) shouldn't abort the whole tree.
			continue
		}
		out = append(out, CatalogDatabase{Name: dbName, Current: true, Schemas: schemas})
	}
	return out, nil
}

func (c *DBConnection) mssqlSchemasForDatabase(ctx context.Context, dbName string) ([]CatalogSchema, error) {
	qdb := mssqlBracketQualify(dbName)
	bySchema := map[string]*CatalogSchema{}
	var order []string
	ensure := func(name string) *CatalogSchema {
		if s, ok := bySchema[name]; ok {
			return s
		}
		s := &CatalogSchema{Name: name}
		bySchema[name] = s
		order = append(order, name)
		return s
	}

	rows, err := c.queryContext(ctx, "catalog.mssql.tables", mssqlListTablesQuery(qdb))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var schema, name, kind string
		if err := rows.Scan(&schema, &name, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		s := ensure(schema)
		item := CatalogItem{Name: name, Kind: normalizeDBObjectKind(kind)}
		if item.Kind == "view" {
			s.Views = append(s.Views, item)
		} else {
			s.Tables = append(s.Tables, item)
		}
	}
	rows.Close()

	procRows, err := c.queryContext(ctx, "catalog.mssql.procedures", mssqlListProceduresQuery(qdb))
	if err == nil {
		for procRows.Next() {
			var schema, name, kind string
			if err := procRows.Scan(&schema, &name, &kind); err != nil {
				break
			}
			ensure(schema).Procedures = append(ensure(schema).Procedures, CatalogItem{Name: name, Kind: kind})
		}
		procRows.Close()
	}

	sort.Strings(order)
	out := make([]CatalogSchema, 0, len(order))
	for _, name := range order {
		out = append(out, *bySchema[name])
	}
	return out, nil
}

// probeObjectKind best-effort determines whether a fully-qualified name from
// the multi-database catalog tree (e.g. "otherdb.dbo.orders" on SQL Server,
// or "otherdb.orders" on MySQL) is a table or a view, so table_view.html can
// still hide table-only actions for it. Defaults to "table" when the lookup
// itself isn't supported or fails.
func (c *DBConnection) probeObjectKind(ctx context.Context, qualifiedName string) string {
	parts := strings.Split(qualifiedName, ".")
	var db, schema, tbl string
	switch len(parts) {
	case 2:
		schema, tbl = parts[0], parts[1]
	case 3:
		db, schema, tbl = parts[0], parts[1], parts[2]
	default:
		return "table"
	}

	if c.Dialect.Name == "MariaDB/MySQL" {
		targetDB := schema
		if targetDB == "" {
			targetDB = db
		}
		schema = targetDB
	}
	query := probeObjectKindQuery(c, db)
	if query == "" {
		return "table"
	}

	kind := "table"
	rows, err := c.queryContext(ctx, "metadata.probe_kind", query, schema, tbl)
	if err != nil {
		return kind
	}
	defer rows.Close()
	if rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil {
			kind = normalizeDBObjectKind(t)
		}
	}
	return kind
}

// ── Column structure (nullable/default/primary key) ─────────────────────────

// splitQualifiedName breaks a dotted identifier ("table", "schema.table", or
// "database.schema.table") into its parts.
func splitQualifiedName(qualifiedName string) (db, schema, table string) {
	parts := strings.Split(qualifiedName, ".")
	switch len(parts) {
	case 1:
		return "", "", parts[0]
	case 2:
		return "", parts[0], parts[1]
	case 3:
		return parts[0], parts[1], parts[2]
	default:
		return "", "", qualifiedName
	}
}

// buildColumnStructure enriches meta.Columns with nullability/default/PK
// info for the read-only "Structure" view, best-effort per dialect. tinySQL
// falls back to just name+type (no extra query), which is still useful.
func (c *DBConnection) buildColumnStructure(ctx context.Context, meta TableMeta) []ColumnDetail {
	nullable, defaults := c.columnNullability(ctx, meta.Name)
	out := make([]ColumnDetail, 0, len(meta.Columns))
	for _, col := range meta.Columns {
		d := ColumnDetail{
			Name:       col.Name,
			TypeName:   col.TypeName,
			PrimaryKey: strings.EqualFold(col.Name, "id"),
			Nullable:   "unknown",
		}
		key := strings.ToLower(col.Name)
		if v, ok := nullable[key]; ok {
			if v {
				d.Nullable = "yes"
			} else {
				d.Nullable = "no"
			}
		}
		d.Default = defaults[key]
		out = append(out, d)
	}
	return out
}

func (c *DBConnection) columnNullability(ctx context.Context, qualifiedName string) (map[string]bool, map[string]string) {
	nullable := map[string]bool{}
	defaults := map[string]string{}
	db, schema, tbl := splitQualifiedName(qualifiedName)

	scan := func(rows *sql.Rows, err error) {
		if err != nil || rows == nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var name, isNullable string
			var def sql.NullString
			if err := rows.Scan(&name, &isNullable, &def); err != nil {
				continue
			}
			nullable[strings.ToLower(name)] = strings.EqualFold(isNullable, "YES")
			if def.Valid {
				defaults[strings.ToLower(name)] = def.String
			}
		}
	}

	switch c.Dialect.Name {
	case "PostgreSQL":
		if schema == "" {
			schema = "public"
		}
		scan(c.queryContext(ctx, "metadata.columns", pgColumnsQuery(c), schema, tbl))
	case "MariaDB/MySQL":
		targetDB := schema
		if targetDB == "" {
			targetDB = db
		}
		if targetDB != "" {
			scan(c.queryContext(ctx, "metadata.columns", mysqlColumnsQuery(c, targetDB), targetDB, tbl))
		} else {
			scan(c.queryContext(ctx, "metadata.columns", mysqlColumnsQuery(c, ""), tbl))
		}
	case "Microsoft SQL Server":
		if schema == "" {
			schema = "dbo"
		}
		scan(c.queryContext(ctx, "metadata.columns", mssqlColumnsQuery(c, db), schema, tbl))
	case "SQLite":
		rows, err := c.queryContext(ctx, "metadata.columns", sqliteColumnsQuery(c, tbl))
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var cid, notnull, pk int
				var name, ctype string
				var dflt sql.NullString
				if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
					nullable[strings.ToLower(name)] = notnull == 0
					if dflt.Valid {
						defaults[strings.ToLower(name)] = dflt.String
					}
				}
			}
		}
	}
	return nullable, defaults
}

// ── View DDL (CREATE/ALTER) ─────────────────────────────────────────────────

// createViewRe matches the leading "CREATE [OR REPLACE] [ALGORITHM=...]
// [DEFINER=...] [SQL SECURITY ...] VIEW" clause so it can be swapped for
// "ALTER VIEW" to offer an editable ALTER statement alongside the original.
var createViewRe = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:OR\s+REPLACE\s+)?(?:ALGORITHM\s*=\s*\S+\s+)?(?:DEFINER\s*=\s*\S+\s+)?(?:SQL\s+SECURITY\s+\S+\s+)?VIEW`)

// viewCreateToAlter derives an "ALTER VIEW ..." statement from a view's
// CREATE statement. SQLite has no ALTER VIEW statement at all (views must be
// dropped and recreated), so this returns "" for it.
func viewCreateToAlter(dialectName, createSQL string) string {
	if dialectName == "SQLite" || !createViewRe.MatchString(createSQL) {
		return ""
	}
	return createViewRe.ReplaceAllString(createSQL, "ALTER VIEW")
}

// fetchViewDefinition retrieves the original CREATE VIEW text for a view,
// dialect by dialect. For PostgreSQL, which only exposes the view's body (not
// a full CREATE statement) via information_schema, one is reconstructed.
func (c *DBConnection) fetchViewDefinition(ctx context.Context, qualifiedName string) (string, error) {
	db, schema, tbl := splitQualifiedName(qualifiedName)
	switch c.Dialect.Name {
	case "PostgreSQL":
		if schema == "" {
			schema = "public"
		}
		def := c.queryScalarArgs(ctx, pgViewDefinitionQuery(c), schema, tbl)
		if strings.TrimSpace(def) == "" {
			return "", fmt.Errorf("view definition not found (insufficient privileges or the view doesn't exist)")
		}
		qualified := tbl
		if schema != "public" {
			qualified = schema + "." + tbl
		}
		return fmt.Sprintf("CREATE OR REPLACE VIEW %s AS\n%s", c.QuoteIdent(qualified), strings.TrimRight(strings.TrimSpace(def), ";")), nil

	case "MariaDB/MySQL":
		qtable := c.QuoteIdent(tbl)
		if schema != "" {
			qtable = c.QuoteIdent(schema) + "." + c.QuoteIdent(tbl)
		}
		rows, err := c.queryContext(ctx, "metadata.show_create_view", mysqlShowCreateViewQuery(qtable))
		if err != nil {
			return "", err
		}
		defer rows.Close()
		if !rows.Next() {
			return "", fmt.Errorf("view definition not found")
		}
		cols, err := rows.Columns()
		if err != nil {
			return "", err
		}
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for i, name := range cols {
			if strings.EqualFold(name, "Create View") && vals[i].Valid {
				return vals[i].String, nil
			}
		}
		return "", fmt.Errorf("view definition not found")

	case "Microsoft SQL Server":
		def, err := c.fetchMSSQLModuleDefinition(ctx, db, schema, tbl)
		if err != nil {
			return "", err
		}
		return def, nil

	case "SQLite":
		def := c.queryScalarArgs(ctx, sqliteViewDefinitionQuery(c), tbl)
		if strings.TrimSpace(def) == "" {
			return "", fmt.Errorf("view definition not found")
		}
		return def, nil

	default:
		return "", fmt.Errorf("view definitions aren't supported for %s", c.Dialect.Name)
	}
}

// fetchMSSQLModuleDefinition reads sys.sql_modules.definition for any
// SQL-text-defined object — view, stored procedure, or function — since
// that catalog view isn't specific to one object type. Shared by
// fetchViewDefinition and fetchRoutineDefinition.
func (c *DBConnection) fetchMSSQLModuleDefinition(ctx context.Context, db, schema, name string) (string, error) {
	qualified := name
	if schema != "" {
		qualified = schema + "." + name
	}
	modulesTable := "sys.sql_modules"
	objectIDArg := qualified
	if db != "" {
		modulesTable = "[" + strings.ReplaceAll(db, "]", "]]") + "].sys.sql_modules"
		objectIDArg = db + "." + qualified
	}
	def := c.queryScalarArgs(ctx, mssqlModuleDefinitionQuery(c, modulesTable), objectIDArg)
	if strings.TrimSpace(def) == "" {
		return "", fmt.Errorf("definition not found (insufficient privileges or the object doesn't exist)")
	}
	return strings.TrimSpace(def), nil
}

// fetchRoutineDefinition returns the CREATE-equivalent source of a stored
// procedure or function, dialect by dialect. kind ("procedure" or
// "function") only matters for MySQL/MariaDB, whose SHOW CREATE syntax and
// result column name differ between the two; MSSQL and PostgreSQL resolve
// either kind through the same catalog mechanism.
func (c *DBConnection) fetchRoutineDefinition(ctx context.Context, qualifiedName, kind string) (string, error) {
	db, schema, name := splitQualifiedName(qualifiedName)
	switch c.Dialect.Name {
	case "Microsoft SQL Server":
		return c.fetchMSSQLModuleDefinition(ctx, db, schema, name)

	case "PostgreSQL":
		if schema == "" {
			schema = "public"
		}
		def := c.queryScalarArgs(ctx, pgFunctionDefQuery(c), schema, name)
		if strings.TrimSpace(def) == "" {
			return "", fmt.Errorf("definition not found (insufficient privileges, an overload mismatch, or the routine doesn't exist)")
		}
		return strings.TrimSpace(def), nil

	case "MariaDB/MySQL":
		qname := c.QuoteIdent(name)
		if schema != "" {
			qname = c.QuoteIdent(schema) + "." + c.QuoteIdent(name)
		}
		showKeyword, resultColumn := "PROCEDURE", "Create Procedure"
		if strings.EqualFold(kind, "function") {
			showKeyword, resultColumn = "FUNCTION", "Create Function"
		}
		rows, err := c.queryContext(ctx, "catalog.show_create_routine", mysqlShowCreateRoutineQuery(showKeyword, qname))
		if err != nil {
			return "", err
		}
		defer rows.Close()
		if !rows.Next() {
			return "", fmt.Errorf("definition not found")
		}
		cols, err := rows.Columns()
		if err != nil {
			return "", err
		}
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for i, colName := range cols {
			if strings.EqualFold(colName, resultColumn) && vals[i].Valid {
				return vals[i].String, nil
			}
		}
		return "", fmt.Errorf("definition not found")

	default:
		return "", fmt.Errorf("routine definitions aren't supported for %s", c.Dialect.Name)
	}
}

// ObjectDependency is one edge in an object's dependency graph: another
// table, view, procedure, or function it references (or that references
// it), used for impact analysis ("what breaks if I change this?").
type ObjectDependency struct {
	Name string `json:"name"` // schema-qualified where the dialect has schemas
	Kind string `json:"kind"` // "table" | "view" | "procedure" | "function" | "object" (kind unknown)
}

// fetchDependencies returns qualifiedName's dependency graph: dependsOn is
// what it references (so you know what else must exist for it to work),
// dependents is what references it (so you know what breaks if you change
// or drop it). kind ("table", "view", "procedure", or "function") narrows
// which dialect-specific mechanism applies — on PostgreSQL and MySQL/
// MariaDB only table/view dependencies are resolvable this way; SQL Server
// resolves any object kind uniformly through the same catalog view.
func (c *DBConnection) fetchDependencies(ctx context.Context, qualifiedName, kind string) (dependsOn, dependents []ObjectDependency, err error) {
	db, schema, name := splitQualifiedName(qualifiedName)
	switch c.Dialect.Name {
	case "Microsoft SQL Server":
		dependsOn, err = c.mssqlDependencyEdges(ctx, db, schema, name, false)
		if err != nil {
			return nil, nil, err
		}
		dependents, err = c.mssqlDependencyEdges(ctx, db, schema, name, true)
		return dependsOn, dependents, err

	case "PostgreSQL":
		if kind != "table" && kind != "view" {
			return nil, nil, fmt.Errorf("dependency analysis is only available for tables and views on %s", c.Dialect.Name)
		}
		if schema == "" {
			schema = "public"
		}
		dependsOn, err = c.postgresViewDependencyEdges(ctx, schema, name, false)
		if err != nil {
			return nil, nil, err
		}
		dependents, err = c.postgresViewDependencyEdges(ctx, schema, name, true)
		return dependsOn, dependents, err

	case "MariaDB/MySQL":
		if kind != "table" && kind != "view" {
			return nil, nil, fmt.Errorf("dependency analysis is only available for tables and views on %s", c.Dialect.Name)
		}
		if schema == "" {
			schema = db
		}
		dependsOn, err = c.mysqlViewDependencyEdges(ctx, schema, name, false)
		if err != nil {
			return nil, nil, err
		}
		dependents, err = c.mysqlViewDependencyEdges(ctx, schema, name, true)
		return dependsOn, dependents, err

	default:
		return nil, nil, fmt.Errorf("dependency analysis isn't supported for %s", c.Dialect.Name)
	}
}

// mssqlDependencyEdges queries sys.sql_expression_dependencies, the same
// catalog view regardless of object kind: reversed=false finds what
// referencing_id (db.schema.name) depends on; reversed=true finds what
// depends on it.
func (c *DBConnection) mssqlDependencyEdges(ctx context.Context, db, schema, name string, reversed bool) ([]ObjectDependency, error) {
	qualified := name
	if schema != "" {
		qualified = schema + "." + name
	}
	depsTable, objectsTable, schemasTable := "sys.sql_expression_dependencies", "sys.objects", "sys.schemas"
	objectIDArg := qualified
	if db != "" {
		quotedDB := "[" + strings.ReplaceAll(db, "]", "]]") + "]"
		depsTable, objectsTable, schemasTable = quotedDB+".sys.sql_expression_dependencies", quotedDB+".sys.objects", quotedDB+".sys.schemas"
		objectIDArg = db + "." + qualified
	}
	joinCol, selectCol := "d.referencing_id", "d.referenced_id"
	if reversed {
		joinCol, selectCol = "d.referenced_id", "d.referencing_id"
	}
	query := mssqlDependencyEdgesQuery(c, depsTable, objectsTable, schemasTable, selectCol, joinCol)
	return c.scanDependencyEdges(ctx, "catalog.mssql.dependencies", query, objectIDArg)
}

// postgresViewDependencyEdges walks pg_depend/pg_rewrite, the mechanism
// PostgreSQL uses to track which views read from which tables/views:
// reversed=false finds what (schema, name) depends on; reversed=true finds
// what depends on it. deptype = 'n' (normal) excludes internal/automatic
// dependencies that aren't meaningful for impact analysis.
func (c *DBConnection) postgresViewDependencyEdges(ctx context.Context, schema, name string, reversed bool) ([]ObjectDependency, error) {
	resultCols, filterCols := "source_ns.nspname, source_table.relname, source_table.relkind", "dependent_ns.nspname = %s AND dependent_view.relname = %s"
	if reversed {
		resultCols, filterCols = "dependent_ns.nspname, dependent_view.relname, dependent_view.relkind", "source_ns.nspname = %s AND source_table.relname = %s"
	}
	query := postgresViewDependencyEdgesQuery(c, resultCols, filterCols)
	rows, err := c.queryContext(ctx, "catalog.postgres.dependencies", query, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectDependency
	for rows.Next() {
		var ns, relname, relkind string
		if err := rows.Scan(&ns, &relname, &relkind); err != nil {
			return nil, err
		}
		kind := "table"
		if relkind == "v" || relkind == "m" {
			kind = "view"
		}
		out = append(out, ObjectDependency{Name: qualifyDependencyName(ns, relname), Kind: kind})
	}
	return out, rows.Err()
}

// mysqlViewDependencyEdges queries information_schema.view_table_usage, the
// only cross-object dependency tracking MySQL/MariaDB expose without
// parsing view/routine bodies: reversed=false finds the tables/views
// (schema, name) — assumed to be a view — reads from; reversed=true finds
// the views that read from (schema, name).
func (c *DBConnection) mysqlViewDependencyEdges(ctx context.Context, schema, name string, reversed bool) ([]ObjectDependency, error) {
	selectCols, whereCols, kind := "table_schema, table_name", "view_schema = %s AND view_name = %s", "table"
	if reversed {
		selectCols, whereCols, kind = "view_schema, view_name", "table_schema = %s AND table_name = %s", "view"
	}
	query := mysqlViewDependencyEdgesQuery(c, selectCols, whereCols)
	rows, err := c.queryContext(ctx, "catalog.mysql.dependencies", query, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectDependency
	for rows.Next() {
		var edgeSchema, edgeName string
		if err := rows.Scan(&edgeSchema, &edgeName); err != nil {
			return nil, err
		}
		out = append(out, ObjectDependency{Name: qualifyDependencyName(edgeSchema, edgeName), Kind: kind})
	}
	return out, rows.Err()
}

// scanDependencyEdges runs query (schema, name, kind) and collects the
// results — shared by every dialect's dependency query.
func (c *DBConnection) scanDependencyEdges(ctx context.Context, op, query string, args ...any) ([]ObjectDependency, error) {
	rows, err := c.queryContext(ctx, op, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectDependency
	for rows.Next() {
		var schema, name, kind string
		if err := rows.Scan(&schema, &name, &kind); err != nil {
			return nil, err
		}
		out = append(out, ObjectDependency{Name: qualifyDependencyName(schema, name), Kind: kind})
	}
	return out, rows.Err()
}

func qualifyDependencyName(schema, name string) string {
	if schema == "" {
		return name
	}
	return schema + "." + name
}

func (c *DBConnection) queryScalarArgs(ctx context.Context, query string, args ...any) string {
	rows, err := c.queryContext(ctx, "catalog.scalar", query, args...)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var v sql.NullString
	if rows.Next() {
		_ = rows.Scan(&v)
	}
	return v.String
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
