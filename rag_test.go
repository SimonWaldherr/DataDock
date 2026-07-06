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

	// The RAG JSON sent to the LLM is always minified (no space after ':',
	// no newlines) — token cost with zero benefit to the model otherwise.
	snapshot := app.schemaSnapshotFor(context.Background(), llmActionGenerateSQL, "show customer email addresses by country", "", "")
	if !strings.Contains(snapshot, `"name":"text_to_sql"`) {
		t.Fatalf("expected text_to_sql skill in snapshot: %s", snapshot)
	}
	if strings.Contains(snapshot, "\n") || strings.Contains(snapshot, "  ") {
		t.Fatalf("expected minified (no indentation) snapshot: %s", snapshot)
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
	if len(decoded.Tables) != maxRAGTables {
		t.Fatalf("expected the 11-table schema to be truncated down to %d tables, got %d", maxRAGTables, len(decoded.Tables))
	}
	// Retrieval/debug metadata (which tables were considered, why, whether
	// the result was truncated) is deliberately not sent to the LLM: the
	// tables[] array above already says what was selected, and the model
	// doesn't need to know how that selection was made.
	if strings.Contains(snapshot, `"retrieval"`) {
		t.Fatalf("retrieval metadata should not be part of what's sent to the LLM: %s", snapshot)
	}
	if strings.Contains(snapshot, `"output_contract"`) {
		t.Fatalf("output_contract duplicates the system prompt and should not be sent: %s", snapshot)
	}
}

func TestLLMSchemaContextTinySQLUsesCompactRAGSnapshot(t *testing.T) {
	app := newTestApp(t)
	for i := 0; i < 10; i++ {
		if _, err := app.sqlDB.Exec(fmt.Sprintf("CREATE TABLE filler_%02d (id INT, value TEXT)", i)); err != nil {
			t.Fatalf("create filler table: %v", err)
		}
	}
	if _, err := app.sqlDB.Exec("CREATE TABLE event_logs (id INT, event_type TEXT, severity TEXT)"); err != nil {
		t.Fatalf("create event_logs: %v", err)
	}

	ctx := app.llmSchemaContext(context.Background(), llmActionGenerateSQL, "", "SELECT * FROM event_logs", "")
	if strings.Contains(ctx, "tinySQL agent profile") {
		t.Fatalf("tinySQL LLM context should use compact RAG JSON, got: %s", ctx)
	}
	var decoded ragContextDoc
	if err := json.Unmarshal([]byte(ctx), &decoded); err != nil {
		t.Fatalf("expected JSON RAG context: %v\n%s", err, ctx)
	}
	if len(decoded.Tables) > maxRAGTables {
		t.Fatalf("expected at most %d tables, got %d", maxRAGTables, len(decoded.Tables))
	}
	found := false
	for _, table := range decoded.Tables {
		if table.Name == "event_logs" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected current SQL table in compact RAG context: %s", ctx)
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
