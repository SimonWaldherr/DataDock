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

// ─── Runtime settings (__datadock_settings) ────────────────────────────────

const (
	settingsEnsureTableSQL = "CREATE TABLE " + runtimeSettingsTable + " (setting_key TEXT, setting_value TEXT)"
	settingsDeleteSQL      = "DELETE FROM " + runtimeSettingsTable + " WHERE setting_key = ?"
	settingsInsertSQL      = "INSERT INTO " + runtimeSettingsTable + " (setting_key, setting_value) VALUES (?, ?)"
	settingsSelectAllSQL   = "SELECT setting_key, setting_value FROM " + runtimeSettingsTable
	settingsSelectOneSQL   = "SELECT setting_value FROM " + runtimeSettingsTable + " WHERE setting_key = ?"
)

// ─── Saved Match Configurations (__datadock_match_configs) ─────────────────

const (
	matchConfigsEnsureTableSQL = "CREATE TABLE " + matchConfigsTable + " (name TEXT, config_json TEXT, updated_at TEXT)"
	matchConfigsDeleteSQL      = "DELETE FROM " + matchConfigsTable + " WHERE name = ?"
	matchConfigsInsertSQL      = "INSERT INTO " + matchConfigsTable + " (name, config_json, updated_at) VALUES (?, ?, ?)"
	matchConfigsSelectOneSQL   = "SELECT config_json FROM " + matchConfigsTable + " WHERE name = ?"
	matchConfigsSelectAllSQL   = "SELECT config_json FROM " + matchConfigsTable
)

// ─── Match Schedules (__datadock_match_schedules) ──────────────────────────

const (
	matchSchedulesEnsureTableSQL = "CREATE TABLE " + matchSchedulesTable +
		" (config_name TEXT, cron_expr TEXT, enabled INT, last_run_at TEXT, last_status TEXT, last_rows INT)"
	matchSchedulesDeleteSQL = "DELETE FROM " + matchSchedulesTable + " WHERE config_name = ?"
	matchSchedulesInsertSQL = "INSERT INTO " + matchSchedulesTable +
		" (config_name, cron_expr, enabled, last_run_at, last_status, last_rows) VALUES (?, ?, ?, ?, ?, ?)"
	matchSchedulesSelectOneSQL = "SELECT config_name, cron_expr, enabled, last_run_at, last_status, last_rows FROM " +
		matchSchedulesTable + " WHERE config_name = ?"
	matchSchedulesSelectAllSQL = "SELECT config_name, cron_expr, enabled, last_run_at, last_status, last_rows FROM " + matchSchedulesTable
	matchSchedulesRecordRunSQL = "UPDATE " + matchSchedulesTable +
		" SET last_run_at = ?, last_status = ?, last_rows = ? WHERE config_name = ?"
)
