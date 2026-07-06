package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDialectProfileForName(t *testing.T) {
	tests := map[string]string{
		"":          "tinySQL",
		"tinysql":   "tinySQL",
		"sqlite3":   "SQLite",
		"postgres":  "PostgreSQL",
		"mariadb":   "MariaDB/MySQL",
		"mysql":     "MariaDB/MySQL",
		"sqlserver": "Microsoft SQL Server",
		"mssql":     "Microsoft SQL Server",
	}
	for input, want := range tests {
		if got := DialectProfileForName(input).Name; got != want {
			t.Fatalf("DialectProfileForName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSchemaSnapshotIncludesDialectAndSamples(t *testing.T) {
	app := newTestApp(t)
	app.dialect = DialectProfileForName("postgres")
	if _, err := app.sqlDB.Exec("CREATE TABLE people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO people (id, name) VALUES (1, 'Ada')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The snapshot sent to the LLM is always minified (no space after ':'),
	// to avoid spending tokens on pretty-printing whitespace.
	snapshot := app.schemaSnapshot(context.Background())
	if !strings.Contains(snapshot, `"name":"PostgreSQL"`) {
		t.Fatalf("expected PostgreSQL dialect in snapshot: %s", snapshot)
	}
	if strings.Contains(snapshot, "\n") || strings.Contains(snapshot, "  ") {
		t.Fatalf("expected minified (no indentation) snapshot: %s", snapshot)
	}
	if !strings.Contains(snapshot, `"CaseInsensitiveOp":`) && !strings.Contains(snapshot, `"case_insensitive_operator":"ILIKE"`) {
		t.Fatalf("expected dialect rules in snapshot: %s", snapshot)
	}
	if !strings.Contains(snapshot, `"Ada"`) {
		t.Fatalf("expected sample value in snapshot: %s", snapshot)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(snapshot), &decoded); err != nil {
		t.Fatalf("snapshot should be JSON: %v\n%s", err, snapshot)
	}
}

func TestValidateAutoRunnableSQLUsesDialectBlockedWords(t *testing.T) {
	app := newTestApp(t)
	if err := app.validateAutoRunnableSQL(context.Background(), "SELECT * FROM people"); err != nil {
		t.Fatalf("SELECT should be allowed: %v", err)
	}
	if err := app.validateAutoRunnableSQL(context.Background(), "SELECT * FROM people; DELETE FROM people"); err == nil {
		t.Fatal("expected multi-statement SQL to be blocked")
	}
	if err := app.validateAutoRunnableSQL(context.Background(), "WITH x AS (SELECT 1) SELECT * FROM x"); err != nil {
		t.Fatalf("WITH query should be allowed: %v", err)
	}
}
