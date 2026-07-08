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
		{name: "ddl", sql: "CREATE TABLE people (id INT)", want: StatementDDL},
		{name: "call", sql: "CALL rebuild_cache()", want: StatementProcedureCall},
		{name: "exec", sql: "EXEC dbo.rebuild_cache", want: StatementProcedureCall},
		{name: "script", sql: "SELECT 1; DELETE FROM people", want: StatementScript},
		{name: "trailing semicolon", sql: "SELECT 1;", want: StatementReadQuery},
		{name: "semicolon in string", sql: "SELECT ';' AS value", want: StatementReadQuery},
		{name: "comment before select", sql: "-- hi\nSELECT 1", want: StatementReadQuery},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.sql); got != tt.want {
				t.Fatalf("Classify(%q) = %q, want %q", tt.sql, got, tt.want)
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
