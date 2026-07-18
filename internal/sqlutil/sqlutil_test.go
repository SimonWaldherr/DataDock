package sqlutil

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want StatementClass
	}{
		{name: "select", sql: "SELECT * FROM people", want: StatementReadQuery},
		{name: "pragma", sql: "PRAGMA table_info(people)", want: StatementReadQuery},
		{name: "describe", sql: "DESCRIBE people", want: StatementReadQuery},
		{name: "read cte", sql: "WITH x AS (SELECT 1) SELECT * FROM x", want: StatementReadQuery},
		{name: "write cte", sql: "WITH x AS (SELECT 1) DELETE FROM people", want: StatementWriteDML},
		{name: "write cte with select outer query", sql: "WITH changed AS (DELETE FROM people RETURNING id) SELECT * FROM changed", want: StatementWriteDML},
		{name: "ddl", sql: "CREATE TABLE people (id INT)", want: StatementDDL},
		{name: "call", sql: "CALL rebuild_cache()", want: StatementProcedureCall},
		{name: "exec", sql: "EXEC dbo.rebuild_cache", want: StatementProcedureCall},
		{name: "script", sql: "SELECT 1; DELETE FROM people", want: StatementScript},
		{name: "trailing semicolon", sql: "SELECT 1;", want: StatementReadQuery},
		{name: "semicolon in string", sql: "SELECT ';' AS value", want: StatementReadQuery},
		{name: "comment before select", sql: "-- hi\nSELECT 1", want: StatementReadQuery},
		{name: "plain explain", sql: "EXPLAIN SELECT * FROM people", want: StatementReadQuery},
		{name: "explain analyze select", sql: "EXPLAIN ANALYZE SELECT * FROM people", want: StatementReadQuery},
		{name: "explain analyze delete executes", sql: "EXPLAIN ANALYZE DELETE FROM people", want: StatementWriteDML},
		{name: "explain analyze insert executes", sql: "EXPLAIN ANALYZE INSERT INTO people (id) VALUES (1)", want: StatementWriteDML},
		{name: "explain parenthesized analyze executes", sql: "EXPLAIN (ANALYZE, VERBOSE) DELETE FROM people", want: StatementWriteDML},
		{name: "explain parenthesized verbose only stays read", sql: "EXPLAIN (VERBOSE) SELECT * FROM people", want: StatementReadQuery},
		{name: "explain with nothing after it stays read", sql: "EXPLAIN", want: StatementReadQuery},
		// PostgreSQL's E'' escape-string syntax recognizes backslash as an
		// in-string escape even with standard_conforming_strings on; a
		// tokenizer that only understands doubled-quote escaping can be
		// tricked into treating a stacked statement as part of the string.
		{name: "e-prefixed backslash-quote hides a stacked statement", sql: `SELECT E'\''; DROP TABLE important_data; --'`, want: StatementScript},
		{name: "e-prefixed backslash-quote without a stacked statement stays a read query", sql: `SELECT E'\'' AS value`, want: StatementReadQuery},
		{name: "plain (non-E) string with trailing backslash is unaffected", sql: `SELECT 'C:\Users\test\' AS path`, want: StatementReadQuery},
		{name: "e-prefixed doubled-quote escape still works", sql: `SELECT E'it''s fine' AS value`, want: StatementReadQuery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.sql); got != tt.want {
				t.Fatalf("Classify(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

func TestIsEPrefixedString(t *testing.T) {
	tests := []struct {
		name  string
		query string
		idx   int
		want  bool
	}{
		{name: "standalone E prefix", query: "SELECT E'x'", idx: 8, want: true},
		{name: "lowercase e prefix", query: "select e'x'", idx: 8, want: true},
		{name: "no prefix at all", query: "SELECT 'x'", idx: 7, want: false},
		{name: "word ending in E is not a prefix", query: "SELECT TABLE'x'", idx: 12, want: false},
		{name: "quote at start of query", query: "'x'", idx: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEPrefixedString(tt.query, tt.idx); got != tt.want {
				t.Fatalf("isEPrefixedString(%q, %d) = %v, want %v", tt.query, tt.idx, got, tt.want)
			}
		})
	}
}

func TestIsResultProducing(t *testing.T) {
	if !IsResultProducing("WITH x AS (SELECT 1) SELECT * FROM x") {
		t.Fatal("read CTE should produce results")
	}
	if IsResultProducing("WITH x AS (SELECT 1) UPDATE people SET name = 'Ada'") {
		t.Fatal("write CTE should not be treated as result-producing")
	}
}
