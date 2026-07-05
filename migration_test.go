package main

import (
	"context"
	"testing"
)

func TestMigrateTableSQLiteToTinySQL(t *testing.T) {
	app := newTestApp(t)
	source, err := OpenManagedConnection(context.Background(), "sqlite-source", "SQLite Source", "sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite source: %v", err)
	}
	t.Cleanup(func() { _ = source.DB.Close() })
	if _, err := source.DB.Exec("CREATE TABLE people (id INTEGER, name TEXT, score REAL)"); err != nil {
		t.Fatalf("create source table: %v", err)
	}
	if _, err := source.DB.Exec("INSERT INTO people (id, name, score) VALUES (1, 'Ada', 98.5), (2, 'Grace', 99.0)"); err != nil {
		t.Fatalf("insert source rows: %v", err)
	}
	if err := app.conns.Add(source); err != nil {
		t.Fatalf("add source connection: %v", err)
	}

	summary, err := app.migrateTable(context.Background(), source.ID, defaultConnectionID, "people", "people_copy", true)
	if err != nil {
		t.Fatalf("migrate table: %v", err)
	}
	if !summary.Created || summary.Rows != 2 {
		t.Fatalf("unexpected summary: %#v", summary)
	}

	var count int
	if err := app.sqlDB.QueryRow("SELECT COUNT(*) FROM people_copy").Scan(&count); err != nil {
		t.Fatalf("count migrated rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 migrated rows, got %d", count)
	}
}

func TestMigrationPlaceholderStyles(t *testing.T) {
	postgres := &DBConnection{Dialect: DialectProfileForName("postgres")}
	mssql := &DBConnection{Dialect: DialectProfileForName("mssql")}
	sqlite := &DBConnection{Dialect: DialectProfileForName("sqlite")}

	if got := postgres.Placeholder(2); got != "$2" {
		t.Fatalf("postgres placeholder = %q", got)
	}
	if got := mssql.Placeholder(2); got != "@p2" {
		t.Fatalf("mssql placeholder = %q", got)
	}
	if got := sqlite.Placeholder(2); got != "?" {
		t.Fatalf("sqlite placeholder = %q", got)
	}
}
