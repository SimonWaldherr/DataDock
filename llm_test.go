package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type fakeLLMClient struct {
	lastReqs []LLMRequest
	text     string
	texts    []string
	err      error
}

func (f *fakeLLMClient) Complete(ctx context.Context, req LLMRequest) (string, error) {
	f.lastReqs = append(f.lastReqs, req)
	if len(f.texts) > 0 {
		text := f.texts[0]
		f.texts = f.texts[1:]
		return text, f.err
	}
	return f.text, f.err
}

func TestChatCompletionsURL(t *testing.T) {
	tests := map[string]string{
		"https://api.openai.com/v1":                 "https://api.openai.com/v1/chat/completions",
		"http://127.0.0.1:1234/v1":                  "http://127.0.0.1:1234/v1/chat/completions",
		"http://127.0.0.1:1234":                     "http://127.0.0.1:1234/v1/chat/completions",
		"http://127.0.0.1:1234/v1/chat/completions": "http://127.0.0.1:1234/v1/chat/completions",
	}
	for in, want := range tests {
		if got := chatCompletionsURL(in); got != want {
			t.Fatalf("chatCompletionsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAPILLMRequiresConfiguration(t *testing.T) {
	app := newTestApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/llm", strings.NewReader(`{"action":"generate_sql","prompt":"show people"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	app.apiLLMHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBuildLLMMessagesTreatsSQLOnlyGenerateAsOptimization(t *testing.T) {
	messages := buildLLMMessages(LLMRequest{
		Action: llmActionGenerateSQL,
		Schema: `{"tables":[{"name":"datadock_demo_events"}]}`,
		SQL:    "SELECT * FROM datadock_demo_events",
	})

	if len(messages) != 2 {
		t.Fatalf("expected system and user messages, got %d", len(messages))
	}
	user := messages[1].Content
	if !strings.Contains(user, "Task: Optimize and refine the current SQL while preserving its intent.") {
		t.Fatalf("expected SQL-only generation to be framed as optimization, got: %s", user)
	}
	if strings.Contains(user, "User prompt:") || strings.Contains(user, "User request:") {
		t.Fatalf("did not expect an empty user prompt/request block, got: %s", user)
	}
	if !strings.Contains(user, "Current SQL:\nSELECT * FROM datadock_demo_events") {
		t.Fatalf("expected current SQL in message, got: %s", user)
	}
}

func TestBuildLLMMessagesIncludesUserRequestWhenPresent(t *testing.T) {
	messages := buildLLMMessages(LLMRequest{
		Action: llmActionGenerateSQL,
		Prompt: "show critical events",
		Schema: `{"tables":[{"name":"datadock_demo_events"}]}`,
		SQL:    "SELECT * FROM datadock_demo_events",
	})

	user := messages[1].Content
	if !strings.Contains(user, "Task: Generate one useful SQL query from the user request. Treat the current SQL as the draft to refine unless the request says otherwise.") {
		t.Fatalf("expected prompt+SQL task framing, got: %s", user)
	}
	if !strings.Contains(user, "User request:\nshow critical events") {
		t.Fatalf("expected user request block, got: %s", user)
	}
}

func TestAPILLMHealthHandler(t *testing.T) {
	app := newTestApp(t)
	app.llm = &fakeLLMClient{text: "OK"}

	req := httptest.NewRequest(http.MethodGet, "/api/llm/health", nil)
	w := httptest.NewRecorder()
	app.apiLLMHealthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) || !strings.Contains(w.Body.String(), "OK") {
		t.Fatalf("unexpected health response: %s", w.Body.String())
	}
}

func TestAPILLMHealthRequiresConfiguration(t *testing.T) {
	app := newTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/api/llm/health", nil)
	w := httptest.NewRecorder()
	app.apiLLMHealthHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAPILLMGenerateSQL(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	fake := &fakeLLMClient{text: "```sql\nSELECT name FROM people LIMIT 10\n```"}
	app.llm = fake

	req := httptest.NewRequest(http.MethodPost, "/api/llm", strings.NewReader(`{"action":"generate_sql","prompt":"list names"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	app.apiLLMHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := resp["text"]; got != "SELECT name FROM people LIMIT 10" {
		t.Fatalf("unexpected text: %q", got)
	}
	if !strings.Contains(fake.lastReqs[0].Schema, "people") || !strings.Contains(fake.lastReqs[0].Schema, `"skill"`) {
		t.Fatalf("expected schema to include people table, got %q", fake.lastReqs[0].Schema)
	}
}

func TestAPILLMGenerateSQLCanRequestAdditionalContext(t *testing.T) {
	app := newTestApp(t)
	for i := 0; i < 10; i++ {
		if _, err := app.sqlDB.Exec("CREATE TABLE filler_" + strconv.Itoa(i) + " (id INT, value TEXT)"); err != nil {
			t.Fatalf("create filler table: %v", err)
		}
	}
	if _, err := app.sqlDB.Exec("CREATE TABLE invoices (id INT, status TEXT, amount INT)"); err != nil {
		t.Fatalf("create invoices: %v", err)
	}
	fake := &fakeLLMClient{texts: []string{
		`{"action":"tool_call","tool":"datadock.schema.search","arguments":{"query":"invoice status amount","tables":["invoices"]},"explanation":"Need invoice columns."}`,
		`{"action":"sql","sql":"SELECT status, SUM(amount) AS total_amount FROM invoices GROUP BY status","explanation":"Summarizes invoices by status."}`,
	}}
	app.llm = fake

	req := httptest.NewRequest(http.MethodPost, "/api/llm", strings.NewReader(`{"action":"generate_sql","prompt":"invoice totals by status"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	app.apiLLMHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.lastReqs) != 2 {
		t.Fatalf("expected two LLM calls, got %d", len(fake.lastReqs))
	}
	if !strings.Contains(fake.lastReqs[1].Schema, "MCP tool result") || !strings.Contains(fake.lastReqs[1].Schema, `"tool":"datadock.schema.search"`) || !strings.Contains(fake.lastReqs[1].Schema, `"invoices"`) {
		t.Fatalf("expected MCP tool result in second call, got %s", fake.lastReqs[1].Schema)
	}
	if !strings.Contains(w.Body.String(), "SUM(amount)") {
		t.Fatalf("expected generated SQL response, got %s", w.Body.String())
	}
}

func TestAPILLMGenerateSQLExtractsLooseJSONSQLOnly(t *testing.T) {
	app := newTestApp(t)
	fake := &fakeLLMClient{text: `{
  "action": "sql",
  "sql": "
SELECT p.id, p.name, p.role, d.name AS department_name
FROM datadock_demo_people p
JOIN datadock_demo_departments d ON p.department_id = d.id
ORDER BY p.name, d.name;
  ",
  "explanation": "
Selected all people joined with departments.
"
}`}
	app.llm = fake

	req := httptest.NewRequest(http.MethodPost, "/api/llm", strings.NewReader(`{"action":"generate_sql","prompt":"people departments"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	app.apiLLMHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if strings.Contains(resp["sql"], "{") || !strings.HasPrefix(resp["sql"], "SELECT p.id") {
		t.Fatalf("expected only SQL in sql field, got: %q", resp["sql"])
	}
	if !strings.Contains(resp["explanation"], "Selected all people") {
		t.Fatalf("expected explanation metadata, got: %#v", resp)
	}
}

func TestAPILLMRunExecutesSafeGeneratedSQL(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.sqlDB.Exec("CREATE TABLE people (id INT, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := app.sqlDB.Exec("INSERT INTO people (id, name) VALUES (1, 'Ada')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	app.llm = &fakeLLMClient{texts: []string{
		`{"action":"sql","sql":"SELECT name FROM people","explanation":"Lists names."}`,
		"One row with Ada.",
	}}

	req := httptest.NewRequest(http.MethodPost, "/api/llm/run", strings.NewReader(`{"prompt":"show names"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	app.apiLLMRunHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["action"] != "sql" {
		t.Fatalf("expected sql action, got %#v", resp["action"])
	}
	if resp["sql"] != "SELECT name FROM people" {
		t.Fatalf("unexpected sql: %#v", resp["sql"])
	}
	if !strings.Contains(w.Body.String(), "Ada") {
		t.Fatalf("expected result row in response: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "One row with Ada.") {
		t.Fatalf("expected explanation in response: %s", w.Body.String())
	}
}

func TestAPILLMRunBlocksGeneratedDML(t *testing.T) {
	app := newTestApp(t)
	app.llm = &fakeLLMClient{text: `{"action":"sql","sql":"DELETE FROM people","explanation":"Deletes rows."}`}

	req := httptest.NewRequest(http.MethodPost, "/api/llm/run", strings.NewReader(`{"prompt":"delete people"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	app.apiLLMRunHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"action":"blocked"`) {
		t.Fatalf("expected blocked action, got %s", w.Body.String())
	}
}

func TestSummarizeLLMResultPivotsLargeResult(t *testing.T) {
	var rows [][]string
	for i := 0; i < 100; i++ {
		status := "paid"
		if i%4 == 0 {
			status = "open"
		}
		rows = append(rows, []string{status, "10"})
	}

	ctx := summarizeLLMResult([]string{"status", "amount"}, rows)
	if !ctx.Summarized {
		t.Fatal("expected large result to be summarized")
	}
	if len(ctx.Rows) != maxLLMContextRows {
		t.Fatalf("expected %d sample rows, got %d", maxLLMContextRows, len(ctx.Rows))
	}
	if ctx.TotalRows != 100 {
		t.Fatalf("expected total_rows=100, got %d", ctx.TotalRows)
	}
	if len(ctx.Profile) != 2 {
		t.Fatalf("expected 2 column profiles, got %d", len(ctx.Profile))
	}
	if ctx.Profile[0].Distinct != 2 {
		t.Fatalf("expected 2 distinct status values, got %#v", ctx.Profile[0])
	}
	if !ctx.Profile[1].Numeric || ctx.Profile[1].Sum != "1000" || ctx.Profile[1].Avg != "10" {
		t.Fatalf("expected numeric amount profile, got %#v", ctx.Profile[1])
	}
}

func TestAPILLMExplainResultsSendsSummaryForLargeResult(t *testing.T) {
	app := newTestApp(t)
	fake := &fakeLLMClient{text: "summary"}
	app.llm = fake

	var rows []string
	for i := 0; i < maxLLMContextRows+5; i++ {
		rows = append(rows, `["paid","10"]`)
	}
	body := `{"action":"explain_results","prompt":"explain","columns":["status","amount"],"rows":[` + strings.Join(rows, ",") + `]}`
	req := httptest.NewRequest(http.MethodPost, "/api/llm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	app.apiLLMHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.lastReqs) != 1 || fake.lastReqs[0].Result == nil {
		t.Fatalf("expected one LLM request with result context")
	}
	if !fake.lastReqs[0].Result.Summarized {
		t.Fatalf("expected summarized result context, got %#v", fake.lastReqs[0].Result)
	}
	if len(fake.lastReqs[0].Result.Rows) != maxLLMContextRows {
		t.Fatalf("expected capped sample rows, got %d", len(fake.lastReqs[0].Result.Rows))
	}
	if len(fake.lastReqs[0].Result.Profile) != 2 {
		t.Fatalf("expected column profiles, got %#v", fake.lastReqs[0].Result)
	}
}
