// Package main: queries.go centralizes SQL query text so the shape of every
// statement DataDock runs against its own metadata tables (and, further
// below, against managed connections) lives in one place instead of being
// scattered across the feature files that execute them. Two kinds of entry
// live here:
//
//   - const strings for queries that are fixed regardless of dialect or
//     runtime input (DataDock's own tinySQL-backed system tables always use
//     "?" placeholders and never need identifier quoting).
//   - functions for queries that depend on a dialect's identifier-quoting/
//     placeholder style (via *DBConnection) or on runtime values like table
//     or column names, which a plain constant can't express.
//
// Callers execute these through the existing a.queryConn/a.execConn/
// conn.queryContext helpers exactly as before; only the SQL text itself
// moved.
package main

import (
	"fmt"
	"strings"
)

// ─── Runtime settings (__datadock_settings) ────────────────────────────────
//
// A flat key/value store (no PRIMARY KEY/UNIQUE constraint) for every
// server-wide setting the Admin page can change at runtime — dialect,
// timeouts, LLM config, page size, theme, auth mode, and the encoded managed
// connection list. Because there's no unique constraint to UPSERT against,
// every write is a delete-then-insert (see App.saveSetting).

const (
	// settingsEnsureTableSQL creates the settings table on first use; a
	// second CREATE against an already-existing table has its "already
	// exists" error swallowed by the caller (ensureRuntimeSettingsTable).
	settingsEnsureTableSQL = "CREATE TABLE " + runtimeSettingsTable + " (setting_key TEXT, setting_value TEXT)"
	// settingsDeleteSQL removes any existing row for a key, the first half
	// of the delete-then-insert upsert saveSetting performs.
	settingsDeleteSQL = "DELETE FROM " + runtimeSettingsTable + " WHERE setting_key = ?"
	// settingsInsertSQL writes one setting_key/setting_value pair, the
	// second half of saveSetting's upsert.
	settingsInsertSQL = "INSERT INTO " + runtimeSettingsTable + " (setting_key, setting_value) VALUES (?, ?)"
	// settingsSelectAllSQL loads every stored key/value pair at once, used
	// by loadRuntimeSettings to hydrate the App's in-memory config at
	// startup.
	settingsSelectAllSQL = "SELECT setting_key, setting_value FROM " + runtimeSettingsTable
	// settingsSelectOneSQL reads a single setting's current value by key
	// (e.g. the encoded managed-connections list via loadSetting).
	settingsSelectOneSQL = "SELECT setting_value FROM " + runtimeSettingsTable + " WHERE setting_key = ?"
)

// ─── Saved Match Configurations (__datadock_match_configs) ─────────────────
//
// One row per named Matching wizard setup (see MatchConfig in
// match_config.go): the whole config is serialized to JSON and stored under
// its name, so "run this match again" or "schedule this match" only need a
// name lookup instead of re-entering every field.

const (
	// matchConfigsEnsureTableSQL creates the saved-configurations table on
	// first use.
	matchConfigsEnsureTableSQL = "CREATE TABLE " + matchConfigsTable + " (name TEXT, config_json TEXT, updated_at TEXT)"
	// matchConfigsDeleteSQL removes a configuration by name — the first half
	// of saveMatchConfig's delete-then-insert upsert, and also all of
	// deleteMatchConfig.
	matchConfigsDeleteSQL = "DELETE FROM " + matchConfigsTable + " WHERE name = ?"
	// matchConfigsInsertSQL writes a configuration's JSON blob under its
	// name, the second half of saveMatchConfig's upsert.
	matchConfigsInsertSQL = "INSERT INTO " + matchConfigsTable + " (name, config_json, updated_at) VALUES (?, ?, ?)"
	// matchConfigsSelectOneSQL fetches one configuration's JSON blob by name
	// (loadMatchConfig), used both to run/edit a saved match and to resolve
	// a Match Schedule's target configuration.
	matchConfigsSelectOneSQL = "SELECT config_json FROM " + matchConfigsTable + " WHERE name = ?"
	// matchConfigsSelectAllSQL lists every saved configuration's JSON blob,
	// for the Matching wizard's "Saved configurations" picker
	// (listMatchConfigs).
	matchConfigsSelectAllSQL = "SELECT config_json FROM " + matchConfigsTable
)

// ─── Match Schedules (__datadock_match_schedules) ──────────────────────────
//
// At most one recurring cron schedule per saved MatchConfig (see
// MatchSchedule in match_schedule.go), plus the outcome of its most recent
// run so the UI can show "last ran 2h ago: ok" without re-running anything.

const (
	// matchSchedulesEnsureTableSQL creates the schedules table on first use.
	matchSchedulesEnsureTableSQL = "CREATE TABLE " + matchSchedulesTable +
		" (config_name TEXT, cron_expr TEXT, enabled INT, last_run_at TEXT, last_status TEXT, last_rows INT)"
	// matchSchedulesDeleteSQL removes a schedule by its configuration name —
	// the first half of saveMatchSchedule's delete-then-insert upsert, and
	// also all of deleteMatchSchedule.
	matchSchedulesDeleteSQL = "DELETE FROM " + matchSchedulesTable + " WHERE config_name = ?"
	// matchSchedulesInsertSQL writes a schedule row, the second half of
	// saveMatchSchedule's upsert.
	matchSchedulesInsertSQL = "INSERT INTO " + matchSchedulesTable +
		" (config_name, cron_expr, enabled, last_run_at, last_status, last_rows) VALUES (?, ?, ?, ?, ?, ?)"
	// matchSchedulesSelectOneSQL fetches one schedule by configuration name
	// (loadMatchSchedule), used when registering/unregistering its cron
	// entry and when deleting the underlying MatchConfig.
	matchSchedulesSelectOneSQL = "SELECT config_name, cron_expr, enabled, last_run_at, last_status, last_rows FROM " +
		matchSchedulesTable + " WHERE config_name = ?"
	// matchSchedulesSelectAllSQL lists every schedule, used both by the
	// Match Schedules admin page and to re-register every enabled schedule
	// with the cron runner at startup (startMatchScheduler).
	matchSchedulesSelectAllSQL = "SELECT config_name, cron_expr, enabled, last_run_at, last_status, last_rows FROM " + matchSchedulesTable
	// matchSchedulesRecordRunSQL updates a schedule's last-run bookkeeping
	// after each cron firing (recordMatchScheduleRun). It's UPDATE-only, not
	// an upsert: a schedule row is guaranteed to already exist by the time a
	// cron callback fires, since registerMatchScheduleEntry is only ever
	// called after saveMatchSchedule has written it.
	matchSchedulesRecordRunSQL = "UPDATE " + matchSchedulesTable +
		" SET last_run_at = ?, last_status = ?, last_rows = ? WHERE config_name = ?"
)

// ─── Local user accounts (__datadock_users) ────────────────────────────────
//
// One row per local account (see User in users.go). There's no PRIMARY
// KEY/UNIQUE constraint here either — createUser enforces case-insensitive
// username uniqueness in Go before inserting.

const (
	// usersEnsureTableSQL creates the users table on first use.
	usersEnsureTableSQL = "CREATE TABLE " + usersTable + " (username TEXT, password_hash TEXT, role TEXT, created_at TEXT, disabled TEXT)"
	// usersSelectAllSQL lists every user (listUsers), the basis for every
	// other lookup in this file (by-username, admin counting, ...) since
	// there's no indexed way to filter server-side.
	usersSelectAllSQL = "SELECT username, password_hash, role, created_at, disabled FROM " + usersTable
	// usersInsertSQL creates a new user account (createUser).
	usersInsertSQL = "INSERT INTO " + usersTable + " (username, password_hash, role, created_at, disabled) VALUES (?, ?, ?, ?, ?)"
	// usersUpdateRoleSQL changes a user's role (updateUserRole).
	usersUpdateRoleSQL = "UPDATE " + usersTable + " SET role = ? WHERE username = ?"
	// usersUpdateDisabledSQL enables/disables a user's account without
	// deleting it (setUserDisabled).
	usersUpdateDisabledSQL = "UPDATE " + usersTable + " SET disabled = ? WHERE username = ?"
	// usersUpdatePasswordSQL sets a new password hash for a user
	// (setUserPasswordHash).
	usersUpdatePasswordSQL = "UPDATE " + usersTable + " SET password_hash = ? WHERE username = ?"
	// usersDeleteSQL removes a user account (deleteUser).
	usersDeleteSQL = "DELETE FROM " + usersTable + " WHERE username = ?"
)

// ─── Indexed object logic embeddings (__datadock_object_embeddings) ────────
//
// One row per (connection, fully-qualified view/procedure/function name):
// its definition's content hash (to skip re-embedding unchanged objects on
// reindex), which embedding model produced the stored vector, and the
// vector itself. See logic_search.go for the indexing/search logic that
// reads and writes this table.

const (
	// objectEmbeddingsEnsureTableSQL creates the embeddings table on first
	// use.
	objectEmbeddingsEnsureTableSQL = "CREATE TABLE " + objectEmbeddingsTable +
		" (connection_id TEXT, object_name TEXT, object_kind TEXT, definition_hash TEXT, embed_model TEXT, embedding VECTOR, updated_at TEXT)"
	// objectEmbeddingsDeleteSQL removes every row for one object, regardless
	// of which embed_model it was stored under — the first half of
	// reindexConnectionLogic's delete-then-insert upsert. Not filtering by
	// embed_model here is deliberate: it's what makes switching the
	// configured embedding model self-healing, pruning the old vector on the
	// very next reindex instead of leaving an orphaned, never-matched row
	// behind (see objectEmbeddingsSelectHashSQL, which DOES filter by
	// embed_model, for the other half of why this works).
	objectEmbeddingsDeleteSQL = "DELETE FROM " + objectEmbeddingsTable + " WHERE connection_id = ? AND object_name = ?"
	// objectEmbeddingsInsertSQL writes one object's embedding. The vector
	// arrives as a JSON-encoded string bound through the normal "?"
	// placeholder — VEC_FROM_JSON(?) is still plain textual substitution
	// under tinySQL's driver, so this is exactly as safe as binding any
	// other string value.
	objectEmbeddingsInsertSQL = "INSERT INTO " + objectEmbeddingsTable +
		" (connection_id, object_name, object_kind, definition_hash, embed_model, embedding, updated_at) VALUES (?, ?, ?, ?, ?, VEC_FROM_JSON(?), ?)"
	// objectEmbeddingsSelectHashSQL looks up the stored definition hash for
	// one object under the CURRENT embed_model specifically (not any model),
	// so reindexConnectionLogic only skips re-embedding when this exact
	// model has already embedded this exact content.
	objectEmbeddingsSelectHashSQL = "SELECT definition_hash FROM " + objectEmbeddingsTable +
		" WHERE connection_id = ? AND object_name = ? AND embed_model = ?"
	// objectEmbeddingsSelectNamesSQL lists every currently-indexed object
	// name for a connection, so reindexConnectionLogic can diff it against
	// the freshly-enumerated catalog and delete rows for objects that no
	// longer exist (renamed/dropped views or routines).
	objectEmbeddingsSelectNamesSQL = "SELECT object_name FROM " + objectEmbeddingsTable + " WHERE connection_id = ?"
	// objectEmbeddingsDeleteForConnectionSQL removes every embedding for a
	// connection, called when that connection itself is deleted (see
	// admin_auth.go) so a later connection ID reuse can't resurface stale
	// vectors against an unrelated database.
	objectEmbeddingsDeleteForConnectionSQL = "DELETE FROM " + objectEmbeddingsTable + " WHERE connection_id = ?"
)

// objectEmbeddingsExactSearchQuery is the correctness fallback for a scoped
// retrieval. VEC_SEARCH itself cannot push connection/model predicates into
// its table-valued function, so callers use it only after its candidate window
// did not contain enough authorized hits.
func objectEmbeddingsExactSearchQuery(topK int) string {
	return fmt.Sprintf(
		"SELECT object_name, object_kind, VEC_COSINE_SIMILARITY(embedding, VEC_FROM_JSON(?)) AS score FROM %s "+
			"WHERE connection_id = ? AND embed_model = ? ORDER BY score DESC LIMIT %d",
		objectEmbeddingsTable, topK)
}

// objectEmbeddingsVectorSearchQuery retrieves a widened native VEC_SEARCH
// candidate window. Scope and metadata predicates remain outside VEC_SEARCH
// and are applied before rows leave the database driver.
func objectEmbeddingsVectorSearchQuery(candidates int, indexMode string) string {
	if indexMode != "hnsw" {
		indexMode = "flat"
	}
	return fmt.Sprintf(
		"SELECT object_name, object_kind, 1.0 - _vec_distance AS score FROM VEC_SEARCH('%s', 'embedding', VEC_FROM_JSON(?), %d, 'cosine', '%s') "+
			"WHERE connection_id = ? AND embed_model = ? ORDER BY _vec_rank",
		objectEmbeddingsTable, candidates, indexMode)
}

// ─── Matching engine (reads both sides, writes the crosswalk table) ────────

// matchSelectRowsQuery builds the SELECT loadMatchRows uses to read one side
// of a match: the key column followed by every configured field column, in
// order, quoted per conn's dialect.
func matchSelectRowsQuery(conn *DBConnection, table string, cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = conn.QuoteIdent(c)
	}
	return fmt.Sprintf("SELECT %s FROM %s", strings.Join(quoted, ", "), conn.QuoteIdent(table))
}

// matchResultTableDDL builds the CREATE TABLE statement for a Matching
// crosswalk table (see matchResultColumns), typed per conn's dialect via
// migrationColumnType so it works on any of the supported target engines.
func matchResultTableDDL(conn *DBConnection, tableName string) string {
	textType := migrationColumnType(conn, "TEXT")
	floatType := migrationColumnType(conn, "FLOAT")
	return fmt.Sprintf(
		"CREATE TABLE %s (%s %s, %s %s, %s %s, %s %s, %s %s, %s %s, %s %s)",
		conn.QuoteIdent(tableName),
		conn.QuoteIdent(matchResultColumns[0]), textType,
		conn.QuoteIdent(matchResultColumns[1]), textType,
		conn.QuoteIdent(matchResultColumns[2]), textType,
		conn.QuoteIdent(matchResultColumns[3]), textType,
		conn.QuoteIdent(matchResultColumns[4]), floatType,
		conn.QuoteIdent(matchResultColumns[5]), textType,
		conn.QuoteIdent(matchResultColumns[6]), textType,
	)
}

// matchResultInsertQuery builds the parameterized INSERT saveMatchResults
// runs once per result row.
func matchResultInsertQuery(conn *DBConnection, tableName string) string {
	quoted := make([]string, len(matchResultColumns))
	placeholders := make([]string, len(matchResultColumns))
	for i, c := range matchResultColumns {
		quoted[i] = conn.QuoteIdent(c)
		placeholders[i] = conn.Placeholder(i + 1)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		conn.QuoteIdent(tableName), strings.Join(quoted, ", "), strings.Join(placeholders, ", "))
}

// ─── Sidebar quick-action scripts (Select Top / Script as INSERT/UPDATE) ───

// buildInsertTemplateSQL renders an editable "Script as INSERT" snippet for
// meta, SSMS-style: column names with <placeholder> values so the user only
// has to fill in the blanks. The "id" column is assumed to be generated by
// the database and is omitted. This is a display-only template, never
// executed by DataDock itself.
func buildInsertTemplateSQL(c *DBConnection, meta TableMeta) string {
	var names, placeholders []string
	for _, col := range meta.Columns {
		if strings.EqualFold(col.Name, "id") {
			continue
		}
		names = append(names, c.QuoteIdent(col.Name))
		placeholders = append(placeholders, "<"+col.Name+">")
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("INSERT INTO %s (%s)\nVALUES (%s);", c.QuoteIdent(meta.Name), strings.Join(names, ", "), strings.Join(placeholders, ", "))
}

// buildUpdateTemplateSQL renders an editable "Script as UPDATE" snippet for
// meta. When the table has an "id" column it's used for the WHERE clause;
// otherwise a generic <condition> placeholder is left for the user to fill
// in. Also display-only, never executed by DataDock itself.
func buildUpdateTemplateSQL(c *DBConnection, meta TableMeta) string {
	var sets []string
	whereCol := ""
	for _, col := range meta.Columns {
		if strings.EqualFold(col.Name, "id") {
			whereCol = col.Name
			continue
		}
		sets = append(sets, fmt.Sprintf("%s = <%s>", c.QuoteIdent(col.Name), col.Name))
	}
	if len(sets) == 0 {
		return ""
	}
	where := "<condition>"
	if whereCol != "" {
		where = fmt.Sprintf("%s = <%s>", c.QuoteIdent(whereCol), whereCol)
	}
	return fmt.Sprintf("UPDATE %s\nSET %s\nWHERE %s;", c.QuoteIdent(meta.Name), strings.Join(sets, ",\n    "), where)
}

// sampleColumnValuesQuery builds the small "peek at a few values" query used
// for LLM schema-context sampling: SQL Server needs TOP instead of LIMIT.
func sampleColumnValuesQuery(c *DBConnection, table, column string, limit int) string {
	if c.Dialect.Name == "Microsoft SQL Server" {
		return fmt.Sprintf("SELECT TOP %d %s FROM %s", limit, c.QuoteIdent(column), c.QuoteIdent(table))
	}
	return fmt.Sprintf("SELECT %s FROM %s LIMIT %d", c.QuoteIdent(column), c.QuoteIdent(table), limit)
}

// llmSampleColumnValuesQuery is sampleColumnValuesQuery's tinySQL-native-path
// counterpart: it quotes with the local quoteName() helper instead of a
// *DBConnection, for App.sampleColumnValues, which samples directly against
// the local tinySQL engine for LLM RAG context rather than a managed
// connection.
func llmSampleColumnValuesQuery(table, column string, limit int) string {
	return fmt.Sprintf("SELECT %s FROM %s LIMIT %d", quoteName(column), quoteName(table), limit)
}

// ─── Catalog browsing: PostgreSQL ───────────────────────────────────────────

const (
	// pgCurrentDatabaseQuery reports which database the connection is on,
	// used to distinguish it from the other databases pgListDatabasesQuery
	// surfaces in the multi-database catalog tree.
	pgCurrentDatabaseQuery = "SELECT current_database()"
	// pgListDatabasesQuery lists every database the current role may
	// CONNECT to (excluding templates), for the catalog tree's per-server
	// database picker.
	pgListDatabasesQuery = "SELECT datname FROM pg_database WHERE datistemplate = false AND has_database_privilege(current_user, datname, 'CONNECT') ORDER BY datname"
	// pgListTablesQuery lists every table/view in every non-system schema of
	// the current database, for one database node in the catalog tree.
	pgListTablesQuery = "SELECT table_schema, table_name, table_type FROM information_schema.tables " +
		"WHERE table_schema NOT IN ('pg_catalog','information_schema') ORDER BY table_schema, table_name"
	// pgListProceduresQuery lists every stored procedure/function in every
	// non-system schema, distinguishing the two via pg_proc.prokind.
	pgListProceduresQuery = "SELECT n.nspname, p.proname, CASE WHEN p.prokind = 'f' THEN 'function' ELSE 'procedure' END " +
		"FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace " +
		"WHERE n.nspname NOT IN ('pg_catalog','information_schema') ORDER BY n.nspname, p.proname"
)

// pgColumnsQuery lists column name/nullability/default for one table.
func pgColumnsQuery(c *DBConnection) string {
	return "SELECT column_name, is_nullable, column_default FROM information_schema.columns WHERE table_schema = " +
		c.Placeholder(1) + " AND table_name = " + c.Placeholder(2)
}

// ─── Catalog browsing: MySQL / MariaDB ──────────────────────────────────────

const (
	// mysqlCurrentDatabaseQuery reports the connection's current database
	// (MySQL/MariaDB have no separate schema concept from database).
	mysqlCurrentDatabaseQuery = "SELECT DATABASE()"
	// mysqlListTablesQuery lists every table/view in every non-system
	// database on the server, for the catalog tree.
	mysqlListTablesQuery = "SELECT table_schema, table_name, table_type FROM information_schema.tables " +
		"WHERE table_schema NOT IN ('information_schema','mysql','performance_schema','sys') " +
		"ORDER BY table_schema, table_name"
	// mysqlListProceduresQuery lists every stored procedure/function in
	// every non-system database on the server.
	mysqlListProceduresQuery = "SELECT routine_schema, routine_name, routine_type FROM information_schema.routines " +
		"WHERE routine_schema NOT IN ('information_schema','mysql','performance_schema','sys') " +
		"ORDER BY routine_schema, routine_name"
)

// mysqlColumnsQuery lists column name/nullability/default for one table,
// scoped to targetDB if known or the connection's current database otherwise
// (MySQL/MariaDB have no separate schema concept from database).
func mysqlColumnsQuery(c *DBConnection, targetDB string) string {
	if targetDB != "" {
		return "SELECT column_name, is_nullable, column_default FROM information_schema.columns WHERE table_schema = " +
			c.Placeholder(1) + " AND table_name = " + c.Placeholder(2)
	}
	return "SELECT column_name, is_nullable, column_default FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = " +
		c.Placeholder(1)
}

// ─── Catalog browsing: Microsoft SQL Server ─────────────────────────────────

const (
	// mssqlCurrentDatabaseQuery reports the connection's current database.
	mssqlCurrentDatabaseQuery = "SELECT DB_NAME()"
	// mssqlListDatabasesQuery lists every user database (database_id > 4
	// skips the four fixed system databases: master, tempdb, model, msdb)
	// that's online (state = 0) and accessible to the current login, for the
	// catalog tree's per-server database picker.
	mssqlListDatabasesQuery = "SELECT name FROM sys.databases WHERE database_id > 4 AND state = 0 AND HAS_DBACCESS(name) = 1 ORDER BY name"
)

// mssqlBracketQualify escapes name for use as a SQL Server "[name]"
// delimited identifier fragment (doubling any embedded "]").
func mssqlBracketQualify(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// mssqlListTablesQuery lists tables/views in database qdb (an already
// bracket-quoted database name, or "" for the current database). It queries
// sys.objects rather than information_schema.tables so is_ms_shipped can
// filter out SQL Server's own built-in objects (MSreplication_options,
// spt_fallback_*, spt_monitor, spt_values, ...) that exist by default in
// every database and would otherwise be surfaced as if they were part of
// the user's schema — including into the LLM's RAG context.
func mssqlListTablesQuery(qdb string) string {
	prefix := ""
	if qdb != "" {
		prefix = qdb + "."
	}
	return "SELECT s.name, o.name, CASE WHEN o.type = 'V' THEN 'VIEW' ELSE 'BASE TABLE' END " +
		"FROM " + prefix + "sys.objects o JOIN " + prefix + "sys.schemas s ON s.schema_id = o.schema_id " +
		"WHERE o.type IN ('U','V') AND o.is_ms_shipped = 0 ORDER BY s.name, o.name"
}

// mssqlListProceduresQuery lists procedures and functions in database qdb
// (an already bracket-quoted database name, or "" for the current database).
func mssqlListProceduresQuery(qdb string) string {
	prefix := ""
	if qdb != "" {
		prefix = qdb + "."
	}
	return "SELECT SCHEMA_NAME(schema_id), name, 'procedure' FROM " + prefix + "sys.procedures WHERE is_ms_shipped = 0 " +
		"UNION ALL " +
		"SELECT SCHEMA_NAME(schema_id), name, 'function' FROM " + prefix + "sys.objects WHERE type IN ('FN','IF','TF') AND is_ms_shipped = 0 " +
		"ORDER BY 1, 2"
}

// mssqlColumnsQuery lists column name/nullability/default for one table in
// database db (bare name, not yet bracket-quoted; "" for the current
// database).
func mssqlColumnsQuery(c *DBConnection, db string) string {
	infoTable := "information_schema.columns"
	if db != "" {
		infoTable = mssqlBracketQualify(db) + ".information_schema.columns"
	}
	return "SELECT column_name, is_nullable, column_default FROM " + infoTable + " WHERE table_schema = " +
		c.Placeholder(1) + " AND table_name = " + c.Placeholder(2)
}

// ─── Catalog browsing: SQLite ────────────────────────────────────────────────

// sqliteColumnsQuery lists a table's columns via PRAGMA table_info, which
// returns (in order) cid, name, type, notnull, dflt_value, pk — unlike every
// other dialect here, this isn't a SELECT, so it can't take bind parameters
// and the table name is inlined (quoted) instead.
func sqliteColumnsQuery(c *DBConnection, table string) string {
	return "PRAGMA table_info(" + c.QuoteIdent(table) + ")"
}

// ─── Cross-dialect: probing whether a fully-qualified name is a table or a
// view (used for the multi-database catalog tree, see probeObjectKind) ──────

// probeObjectKindQuery returns the dialect-appropriate "is this a table or a
// view" query for probeObjectKind, or "" if the dialect isn't supported (the
// caller then falls back to assuming "table"). db is the bare (not yet
// bracket-quoted) database name; only Microsoft SQL Server uses it.
func probeObjectKindQuery(c *DBConnection, db string) string {
	switch c.Dialect.Name {
	case "PostgreSQL", "MariaDB/MySQL":
		return fmt.Sprintf("SELECT table_type FROM information_schema.tables WHERE table_schema = %s AND table_name = %s",
			c.Placeholder(1), c.Placeholder(2))
	case "Microsoft SQL Server":
		if db == "" {
			return ""
		}
		return fmt.Sprintf("SELECT table_type FROM %s.information_schema.tables WHERE table_schema = %s AND table_name = %s",
			mssqlBracketQualify(db), c.Placeholder(1), c.Placeholder(2))
	default:
		return ""
	}
}

// ─── View DDL / routine definitions (see fetchViewDefinition,
// fetchRoutineDefinition, fetchMSSQLModuleDefinition) ───────────────────────

// pgViewDefinitionQuery reads a view's body text (PostgreSQL only exposes the
// body, not a full CREATE statement, via information_schema).
func pgViewDefinitionQuery(c *DBConnection) string {
	return "SELECT view_definition FROM information_schema.views WHERE table_schema = " +
		c.Placeholder(1) + " AND table_name = " + c.Placeholder(2)
}

// mysqlShowCreateViewQuery builds "SHOW CREATE VIEW" for an already
// schema-qualified, already-quoted view name.
func mysqlShowCreateViewQuery(qtable string) string {
	return "SHOW CREATE VIEW " + qtable
}

// sqliteViewDefinitionQuery reads a view's original CREATE VIEW text from
// sqlite_master.
func sqliteViewDefinitionQuery(c *DBConnection) string {
	return "SELECT sql FROM sqlite_master WHERE type = 'view' AND name = " + c.Placeholder(1)
}

// mssqlModuleDefinitionQuery reads sys.sql_modules.definition for any
// SQL-text-defined object (view, procedure, or function) in modulesTable (an
// already database-qualified "sys.sql_modules", or the bare name for the
// current database).
func mssqlModuleDefinitionQuery(c *DBConnection, modulesTable string) string {
	return "SELECT definition FROM " + modulesTable + " WHERE object_id = OBJECT_ID(" + c.Placeholder(1) + ")"
}

// pgFunctionDefQuery reads a function/procedure's full definition via
// pg_get_functiondef, matched by schema+name (LIMIT 1 to tolerate overloads).
func pgFunctionDefQuery(c *DBConnection) string {
	return "SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace " +
		"WHERE n.nspname = " + c.Placeholder(1) + " AND p.proname = " + c.Placeholder(2) + " LIMIT 1"
}

// mysqlShowCreateRoutineQuery builds "SHOW CREATE PROCEDURE/FUNCTION" for an
// already schema-qualified, already-quoted routine name.
func mysqlShowCreateRoutineQuery(showKeyword, qname string) string {
	return "SHOW CREATE " + showKeyword + " " + qname
}

// ─── Object dependency analysis (see fetchDependencies and its per-dialect
// helpers) ────────────────────────────────────────────────────────────────

// mssqlDependencyEdgesQuery builds the sys.sql_expression_dependencies join
// that mssqlDependencyEdges runs in both directions (dependsOn/dependents),
// with the pre-resolved (possibly database-qualified) table names and
// join/select column swap already decided by the caller.
func mssqlDependencyEdgesQuery(c *DBConnection, depsTable, objectsTable, schemasTable, selectCol, joinCol string) string {
	return fmt.Sprintf(
		`SELECT DISTINCT ISNULL(s.name, ''), o.name, `+
			`CASE o.type WHEN 'V' THEN 'view' WHEN 'U' THEN 'table' WHEN 'P' THEN 'procedure' `+
			`WHEN 'FN' THEN 'function' WHEN 'IF' THEN 'function' WHEN 'TF' THEN 'function' ELSE 'object' END `+
			`FROM %s d JOIN %s o ON %s = o.object_id LEFT JOIN %s s ON o.schema_id = s.schema_id `+
			`WHERE %s = OBJECT_ID(%s)`,
		depsTable, objectsTable, selectCol, schemasTable, joinCol, c.Placeholder(1))
}

// postgresViewDependencyEdgesQuery builds the pg_depend/pg_rewrite walk
// postgresViewDependencyEdges runs in both directions, with the
// direction-dependent result/filter column lists already decided by the
// caller (filterCols still has its two %s placeholder slots for the
// dialect's parameter placeholders).
func postgresViewDependencyEdgesQuery(c *DBConnection, resultCols, filterCols string) string {
	return fmt.Sprintf(
		`SELECT DISTINCT %s FROM pg_depend `+
			`JOIN pg_rewrite ON pg_depend.objid = pg_rewrite.oid `+
			`JOIN pg_class dependent_view ON pg_rewrite.ev_class = dependent_view.oid `+
			`JOIN pg_class source_table ON pg_depend.refobjid = source_table.oid `+
			`JOIN pg_namespace dependent_ns ON dependent_ns.oid = dependent_view.relnamespace `+
			`JOIN pg_namespace source_ns ON source_ns.oid = source_table.relnamespace `+
			`WHERE %s AND source_table.oid <> dependent_view.oid AND pg_depend.deptype = 'n'`,
		resultCols, fmt.Sprintf(filterCols, c.Placeholder(1), c.Placeholder(2)))
}

// mysqlViewDependencyEdgesQuery builds the information_schema.view_table_usage
// lookup that mysqlViewDependencyEdges runs in both directions, with the
// direction-dependent select/where column lists already decided by the
// caller (whereCols still has its two %s placeholder slots).
func mysqlViewDependencyEdgesQuery(c *DBConnection, selectCols, whereCols string) string {
	return fmt.Sprintf(
		"SELECT DISTINCT %s FROM information_schema.view_table_usage WHERE %s",
		selectCols, fmt.Sprintf(whereCols, c.Placeholder(1), c.Placeholder(2)))
}

// ─── Migration (copies one table's rows from one connection to another) ────

// migrationSelectAllQuery reads every row/column of a source table, exactly
// as-is, for migrateTable to re-insert on the target connection.
func migrationSelectAllQuery(source *DBConnection, name string) string {
	return "SELECT * FROM " + source.QuoteIdent(name)
}

// migrationCreateTableDDL builds the target-side CREATE TABLE for a
// migrated table, mapping each source column's type via migrationColumnType
// so it's valid on the target's dialect. Callers must have already
// validated every column name with isValidIdentifier.
func migrationCreateTableDDL(target *DBConnection, table string, columns []Column) string {
	defs := make([]string, 0, len(columns))
	for _, col := range columns {
		defs = append(defs, target.QuoteIdent(col.Name)+" "+migrationColumnType(target, col.TypeName))
	}
	return fmt.Sprintf("CREATE TABLE %s (%s)", target.QuoteIdent(table), strings.Join(defs, ", "))
}

// migrationInsertSQL builds the parameterized INSERT migrateTable runs once
// per source row, in the same column order as the source SELECT.
func migrationInsertSQL(target *DBConnection, table string, columns []string) string {
	quoted := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, col := range columns {
		quoted[i] = target.QuoteIdent(col)
		placeholders[i] = target.Placeholder(i + 1)
	}
	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		target.QuoteIdent(table),
		strings.Join(quoted, ", "),
		strings.Join(placeholders, ", "),
	)
}

// ─── Table browsing / row-editing (used by the record CRUD endpoints) ──────

// rowCountQuery counts the rows of an already-resolved table/view name.
func rowCountQuery(conn *DBConnection, name string) string {
	return "SELECT COUNT(*) FROM " + conn.QuoteIdent(name)
}

// nativeRowCountQuery is rowCountQuery's tinySQL-native-path counterpart: it
// quotes with the local quoteName() helper instead of a *DBConnection, for
// callers (App.tableMeta) operating directly on tinySQL before a DBConnection
// wrapper is available.
func nativeRowCountQuery(name string) string {
	return "SELECT COUNT(*) FROM " + quoteName(name)
}

// recordGetQuery builds the SELECT behind getRecord's single-row-by-id fetch.
func recordGetQuery(conn *DBConnection, table string) string {
	return fmt.Sprintf("SELECT * FROM %s WHERE %s = %s", conn.QuoteIdent(table), conn.QuoteIdent("id"), conn.Placeholder(1))
}

// recordMaxIDQuery finds the current highest id, so insertRecord can assign
// the next one (DataDock's tables use application-assigned integer ids
// rather than relying on a dialect-specific auto-increment/serial column).
func recordMaxIDQuery(conn *DBConnection, table string) string {
	return "SELECT MAX(" + conn.QuoteIdent("id") + ") FROM " + conn.QuoteIdent(table)
}

// recordInsertQuery builds the parameterized INSERT for insertRecord, given
// the already-quoted column names and placeholders in matching order.
func recordInsertQuery(conn *DBConnection, table string, colNames, placeholders []string) string {
	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		conn.QuoteIdent(table),
		strings.Join(colNames, ", "),
		strings.Join(placeholders, ", "),
	)
}

// recordUpdateQuery builds the parameterized UPDATE for updateRecord, given
// the already-built "col = ?" set clauses and the placeholder for the
// trailing "WHERE id = ?" (its position depends on how many SET args precede
// it, hence being passed in rather than always "1").
func recordUpdateQuery(conn *DBConnection, table string, setClauses []string, idPlaceholder string) string {
	return fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s = %s",
		conn.QuoteIdent(table),
		strings.Join(setClauses, ", "),
		conn.QuoteIdent("id"),
		idPlaceholder,
	)
}

// recordDeleteQuery builds the parameterized DELETE for deleteRecord.
func recordDeleteQuery(conn *DBConnection, table string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s = %s", conn.QuoteIdent(table), conn.QuoteIdent("id"), conn.Placeholder(1))
}

// tableObjectsQuery lists every browsable table/view for conn's *default*
// database/schema scope, dialect by dialect. Unlike the catalog-tree queries
// above (which return schema/name/type as separate columns for the whole
// server), this returns one display-ready "name" column (schema-qualified
// only where the dialect needs it) alongside "table_type", matching what
// App.tableObjects/tableMeta expect.
func tableObjectsQuery(c *DBConnection) string {
	switch c.Dialect.Name {
	case "SQLite":
		return "SELECT name, type FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name"
	case "PostgreSQL":
		return "SELECT CASE WHEN table_schema = 'public' THEN table_name ELSE table_schema || '.' || table_name END AS name, table_type FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema') AND table_type IN ('BASE TABLE','VIEW') ORDER BY table_schema, table_name"
	case "MariaDB/MySQL":
		return "SELECT table_name, table_type FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type IN ('BASE TABLE','VIEW') ORDER BY table_name"
	case "Microsoft SQL Server":
		// sys.objects (not information_schema.tables) so is_ms_shipped can
		// exclude SQL Server's own built-in tables/views
		// (MSreplication_options, spt_fallback_*, spt_monitor, spt_values,
		// ...), which otherwise show up as if they were part of the user's
		// schema — including in the LLM's RAG context.
		return "SELECT s.name + '.' + o.name AS name, CASE WHEN o.type = 'V' THEN 'VIEW' ELSE 'BASE TABLE' END AS table_type " +
			"FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id " +
			"WHERE o.type IN ('U','V') AND o.is_ms_shipped = 0 ORDER BY s.name, o.name"
	default:
		return "SELECT name, type FROM sqlite_master WHERE type IN ('table','view') ORDER BY name"
	}
}

// selectAllQuery builds an unpaged "SELECT * FROM table", used where a full
// unpaged dump is genuinely wanted (the table-view "Export" flow).
func selectAllQuery(conn *DBConnection, table string) string {
	return "SELECT * FROM " + conn.QuoteIdent(table)
}

// createTableQuery builds a "CREATE TABLE" statement from already-quoted,
// already-typed column definitions (see createTableHandler).
func createTableQuery(conn *DBConnection, table string, colDefs []string) string {
	return fmt.Sprintf("CREATE TABLE %s (%s)", conn.QuoteIdent(table), strings.Join(colDefs, ", "))
}

// dropTableQuery builds a "DROP TABLE" statement.
func dropTableQuery(conn *DBConnection, table string) string {
	return "DROP TABLE " + conn.QuoteIdent(table)
}

// selectPageSQL builds a paginated "SELECT * FROM table" for one page of the
// table browser / migration preview, per-dialect: SQL Server has no LIMIT/
// OFFSET and needs TOP (for limit<=0) or ORDER BY ... OFFSET/FETCH, and
// requires an ORDER BY before OFFSET/FETCH can be used at all.
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
