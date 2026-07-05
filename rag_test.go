package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSchemaSnapshotForUsesSkillAndRAGSelection(t *testing.T) {
	app := newTestApp(t)
	for i := 0; i < 10; i++ {
		if _, err := app.sqlDB.Exec(fmt.Sprintf("CREATE TABLE filler_%02d (id INT, value TEXT)", i)); err != nil {
			t.Fatalf("create filler table: %v", err)
		}
	}
	if _, err := app.sqlDB.Exec("CREATE TABLE customers (id INT, email TEXT, country TEXT)"); err != nil {
		t.Fatalf("create customers: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO customers (id, email, country) VALUES (1, 'ada@example.test', 'DE')"); err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	snapshot := app.schemaSnapshotFor(context.Background(), llmActionGenerateSQL, "show customer email addresses by country", "", "")
	if !strings.Contains(snapshot, `"name": "text_to_sql"`) {
		t.Fatalf("expected text_to_sql skill in snapshot: %s", snapshot)
	}
	if !strings.Contains(snapshot, `"customers"`) {
		t.Fatalf("expected customers table in retrieved context: %s", snapshot)
	}
	if !strings.Contains(snapshot, `"ada@example.test"`) {
		t.Fatalf("expected sample value in retrieved context: %s", snapshot)
	}

	var decoded ragContextDoc
	if err := json.Unmarshal([]byte(snapshot), &decoded); err != nil {
		t.Fatalf("snapshot should decode as ragContextDoc: %v", err)
	}
	if len(decoded.Tables) > maxRAGTables {
		t.Fatalf("expected at most %d retrieved tables, got %d", maxRAGTables, len(decoded.Tables))
	}
	if !decoded.Retrieval.Truncated {
		t.Fatalf("expected truncated retrieval for large schema: %#v", decoded.Retrieval)
	}
}

func TestTokenizeRAGQuery(t *testing.T) {
	got := tokenizeRAGQuery("Show the top customer emails by country and SQL")
	joined := strings.Join(got, ",")
	for _, want := range []string{"customer", "emails", "country"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected token %q in %v", want, got)
		}
	}
	if strings.Contains(joined, "the") || strings.Contains(joined, "sql") {
		t.Fatalf("expected stop words to be removed, got %v", got)
	}
}
