package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SimonWaldherr/datadock/internal/resultutil"
	"github.com/SimonWaldherr/datadock/internal/sqlutil"
)

const (
	llmActionGenerateSQL    = "generate_sql"
	llmActionAskRun         = "ask_and_run"
	llmActionExplainResults = "explain_results"
	llmActionExplainError   = "explain_error"
	maxLLMPromptChars       = 8000
	maxLLMContextRows       = 20
	maxLLMSampleValues      = 3
	maxRAGTables            = 8
	maxLLMTopValues         = 8
	maxLLMContextRequests   = 2
)

type LLMResultContext = resultutil.Summary
type LLMColumnProfile = resultutil.ColumnProfile
type LLMValueCount = resultutil.ValueCount

type LLMClient interface {
	Complete(ctx context.Context, req LLMRequest) (string, error)
}

type LLMConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
	Verbose *VerboseLogger
}

type LLMRequest struct {
	Action string
	Prompt string
	Schema string
	SQL    string
	Error  string
	Result *LLMResultContext
}

type LLMSQLResponse struct {
	Action      string   `json:"action"`
	SQL         string   `json:"sql,omitempty"`
	Explanation string   `json:"explanation,omitempty"`
	FollowUp    string   `json:"follow_up,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	Tables      []string `json:"tables,omitempty"`
}

type OpenAICompatibleClient struct {
	cfg        LLMConfig
	httpClient *http.Client
}

func NewOpenAICompatibleClient(cfg LLMConfig) *OpenAICompatibleClient {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 45 * time.Second
	}
	httpClient := &http.Client{Timeout: cfg.Timeout}
	if cfg.Verbose.Enabled() {
		httpClient = cfg.Verbose.HTTPClient(cfg.Timeout)
	}
	return &OpenAICompatibleClient{
		cfg:        cfg,
		httpClient: httpClient,
	}
}

func (c *OpenAICompatibleClient) Complete(ctx context.Context, req LLMRequest) (string, error) {
	if strings.TrimSpace(c.cfg.BaseURL) == "" {
		return "", errors.New("LLM base URL is not configured")
	}
	if strings.TrimSpace(c.cfg.Model) == "" {
		return "", errors.New("LLM model is not configured")
	}

	messages := buildLLMMessages(req)
	body := chatCompletionRequest{
		Model:       c.cfg.Model,
		Messages:    messages,
		Temperature: 0.1,
		MaxTokens:   1200,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsURL(c.cfg.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("LLM request failed: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	if c.cfg.Verbose.Enabled() {
		c.cfg.Verbose.Log(VerboseEvent{
			System:        "llm",
			Direction:     "inbound",
			Operation:     req.Action,
			Target:        chatCompletionsURL(c.cfg.BaseURL),
			Status:        resp.Status,
			ResponseBytes: int64(len(respBody)),
			Preview:       string(respBody),
		})
	}

	var out chatCompletionResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("LLM response contained no choices")
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("LLM response was empty")
	}
	return content, nil
}

type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// buildLLMMessages assembles the exact system/user messages sent to the
// LLM. Any embedded JSON (the RAG schema context, the result sample) is
// always minified: it's tokens the model has to pay for and whitespace adds
// nothing to its ability to parse it. For a human-readable rendering of the
// same content (e.g. the SQL editor's "Details" preview), use
// buildLLMMessagesForDisplay instead — never the other way around, so what's
// previewed can never drift from what's actually sent.
func buildLLMMessages(req LLMRequest) []chatMessage {
	identity := func(s string) string { return s }
	return buildLLMMessagesWithFormatting(req, identity, json.Marshal)
}

// buildLLMMessagesForDisplay returns the same messages as buildLLMMessages
// but with embedded JSON pretty-printed, for display only. req.Schema is
// normally a minified JSON object, but this also handles any surrounding prose
// defensively by formatting embedded {...} blocks individually.
func buildLLMMessagesForDisplay(req LLMRequest) []chatMessage {
	indent := func(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
	return buildLLMMessagesWithFormatting(req, prettyPrintEmbeddedJSON, indent)
}

func buildLLMMessagesWithFormatting(req LLMRequest, formatSchema func(string) string, marshalResult func(any) ([]byte, error)) []chatMessage {
	system := "You are DataDock's SQL assistant. Answer using only the provided RAG context. " +
		"Follow the SQL dialect profile and active skill instructions exactly. " +
		"Use only retrieved tables and columns. If more schema context is needed, request it before final SQL. If the request remains ambiguous, ask one concrete clarification. " +
		"Prefer safe, read-only SELECT queries unless the user explicitly asks for data changes. " +
		"For SQL generation, return exactly one JSON object and no markdown or extra prose: " +
		`{"action":"sql","sql":"SELECT ...","explanation":"..."}, {"action":"context","kind":"schema","tables":["table_name"],"explanation":"..."}, or {"action":"clarify","follow_up":"...","explanation":"..."}. ` +
		"Keep explanations concise and practical."

	var user strings.Builder
	user.WriteString("Action: ")
	user.WriteString(req.Action)
	user.WriteString("\nTask: ")
	user.WriteString(llmTaskInstruction(req))
	user.WriteString("\n\nRAG context:\n")
	user.WriteString(formatSchema(req.Schema))

	if strings.TrimSpace(req.SQL) != "" {
		user.WriteString("\n\nCurrent SQL:\n")
		user.WriteString(trimForLLM(req.SQL, maxLLMPromptChars))
	}
	if strings.TrimSpace(req.Prompt) != "" {
		user.WriteString("\n\nUser request:\n")
		user.WriteString(trimForLLM(req.Prompt, maxLLMPromptChars))
	}
	if strings.TrimSpace(req.Error) != "" {
		user.WriteString("\n\nError:\n")
		user.WriteString(trimForLLM(req.Error, maxLLMPromptChars))
	}
	if req.Result != nil {
		b, _ := marshalResult(req.Result)
		user.WriteString("\n\nResult sample JSON:\n")
		user.WriteString(trimForLLM(string(b), maxLLMPromptChars))
	}

	return []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user.String()},
	}
}

func llmTaskInstruction(req LLMRequest) string {
	switch req.Action {
	case llmActionGenerateSQL, llmActionAskRun:
		if strings.TrimSpace(req.SQL) != "" {
			if strings.TrimSpace(req.Prompt) == "" {
				return "Optimize and refine the current SQL while preserving its intent. Return only the SQL JSON contract."
			}
			return "Generate one useful SQL query from the user request. Treat the current SQL as the draft to refine unless the request says otherwise. Request more context only if required."
		}
		return "Generate one useful SQL query from the user request. Request more context only if required."
	case llmActionExplainResults:
		return "Explain the result sample for the current SQL in plain language."
	case llmActionExplainError:
		return "Explain the SQL/database error and suggest a correction when the schema context supports one."
	default:
		return "Assist with SQL using only the retrieved context."
	}
}

// prettyPrintEmbeddedJSON scans s for top-level {...} objects and replaces
// each one that's valid JSON with an indented rendering, leaving any
// surrounding prose untouched. Used only for display (buildLLMMessagesForDisplay):
// the RAG context is normally minified JSON on its own, but this keeps the
// display path tolerant of surrounding prose from older or external callers.
func prettyPrintEmbeddedJSON(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '{' {
			out.WriteByte(s[i])
			i++
			continue
		}
		end := matchingBraceEnd(s, i)
		if end < 0 {
			out.WriteByte(s[i])
			i++
			continue
		}
		var v any
		candidate := s[i : end+1]
		if err := json.Unmarshal([]byte(candidate), &v); err == nil {
			if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
				out.Write(pretty)
				i = end + 1
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// matchingBraceEnd returns the index of the '}' that closes the '{' at
// s[start], respecting braces that appear inside quoted string values, or -1
// if s[start] isn't '{' or has no matching close.
func matchingBraceEnd(s string, start int) int {
	if start >= len(s) || s[start] != '{' {
		return -1
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func parseLLMSQLResponse(text string) LLMSQLResponse {
	text = strings.TrimSpace(stripMarkdownCodeFence(text))
	var out LLMSQLResponse
	if err := json.Unmarshal([]byte(text), &out); err == nil && (out.Action != "" || out.SQL != "" || out.FollowUp != "") {
		return normalizeLLMSQLResponse(out)
	}
	if out, ok := parseLooseLLMSQLResponse(text); ok {
		return normalizeLLMSQLResponse(out)
	}
	return LLMSQLResponse{Action: "sql", SQL: text}
}

func normalizeLLMSQLResponse(out LLMSQLResponse) LLMSQLResponse {
	out.Action = strings.ToLower(strings.TrimSpace(out.Action))
	out.SQL = strings.TrimSpace(stripMarkdownCodeFence(out.SQL))
	out.Explanation = strings.TrimSpace(out.Explanation)
	out.FollowUp = strings.TrimSpace(out.FollowUp)
	out.Kind = strings.ToLower(strings.TrimSpace(out.Kind))
	if out.Action == "context" && out.Kind == "" {
		out.Kind = "schema"
	}
	if len(out.Tables) > maxRAGTables {
		out.Tables = out.Tables[:maxRAGTables]
	}
	if out.Action == "" && out.SQL != "" {
		out.Action = "sql"
	}
	return out
}

func parseLooseLLMSQLResponse(text string) (LLMSQLResponse, bool) {
	sqlText, ok := looseJSONStringField(text, "sql")
	if !ok {
		return LLMSQLResponse{}, false
	}
	action, _ := looseJSONStringField(text, "action")
	explanation, _ := looseJSONStringField(text, "explanation")
	followUp, _ := looseJSONStringField(text, "follow_up")
	kind, _ := looseJSONStringField(text, "kind")
	return LLMSQLResponse{
		Action:      action,
		SQL:         sqlText,
		Explanation: explanation,
		FollowUp:    followUp,
		Kind:        kind,
	}, true
}

func looseJSONStringField(text, field string) (string, bool) {
	key := `"` + field + `"`
	idx := strings.Index(text, key)
	if idx < 0 {
		return "", false
	}
	rest := text[idx+len(key):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return "", false
	}
	rest = strings.TrimLeft(rest[colon+1:], " \t\r\n")
	if !strings.HasPrefix(rest, `"`) {
		return "", false
	}
	var b strings.Builder
	escaped := false
	for i := 1; i < len(rest); i++ {
		ch := rest[i]
		if escaped {
			b.WriteByte('\\')
			b.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			raw := b.String()
			if unquoted, err := strconv.Unquote(`"` + strings.ReplaceAll(raw, "\n", `\n`) + `"`); err == nil {
				return unquoted, true
			}
			return raw, true
		}
		b.WriteByte(ch)
	}
	return "", false
}

func (a *App) generateSQLFromPrompt(ctx context.Context, prompt string) (LLMSQLResponse, error) {
	llm := a.llmClient()
	if llm == nil {
		return LLMSQLResponse{}, errors.New("LLM is not configured")
	}
	req := LLMRequest{
		Action: llmActionGenerateSQL,
		Prompt: prompt,
		Schema: a.llmSchemaContext(ctx, llmActionGenerateSQL, prompt, "", ""),
	}
	out, err := a.completeSQLWithContextRequests(ctx, llm, req)
	if err != nil {
		return LLMSQLResponse{}, err
	}
	return parseLLMSQLResponse(out), nil
}

func (a *App) completeSQLWithContextRequests(ctx context.Context, llm LLMClient, req LLMRequest) (string, error) {
	var out string
	var err error
	for i := 0; i <= maxLLMContextRequests; i++ {
		out, err = llm.Complete(ctx, req)
		if err != nil {
			return "", err
		}
		parsed := parseLLMSQLResponse(out)
		if parsed.Action != "context" {
			return out, nil
		}
		extra := a.requestedLLMContext(ctx, req, parsed)
		if strings.TrimSpace(extra) == "" {
			return `{"action":"clarify","follow_up":"Which table or field should I inspect?","explanation":"The model requested additional context but did not name a retrievable table."}`, nil
		}
		req.Schema = strings.TrimSpace(req.Schema) + "\n\n" + extra
	}
	return `{"action":"clarify","follow_up":"Which table or fields should I use?","explanation":"More schema context was needed than DataDock can safely fetch in one request."}`, nil
}

func (a *App) requestedLLMContext(ctx context.Context, req LLMRequest, parsed LLMSQLResponse) string {
	var terms []string
	for _, table := range parsed.Tables {
		table = strings.TrimSpace(table)
		if table != "" {
			terms = append(terms, table)
		}
	}
	query := strings.Join(terms, " ")
	if query == "" {
		query = strings.TrimSpace(req.Prompt + " " + req.SQL)
	}
	if query == "" {
		return ""
	}
	kind := parsed.Kind
	if kind == "" {
		kind = "schema"
	}
	snapshot := a.schemaSnapshotFor(ctx, req.Action, query, "", "")
	return "Additional requested RAG context (" + kind + "):\n" + snapshot
}

func (a *App) explainLLMResults(ctx context.Context, prompt, sql string, result QueryResult) (string, error) {
	if len(result.Columns) == 0 {
		return "", nil
	}
	llm := a.llmClient()
	if llm == nil {
		return "", errors.New("LLM is not configured")
	}
	return llm.Complete(ctx, LLMRequest{
		Action: llmActionExplainResults,
		Prompt: prompt,
		Schema: a.llmSchemaContext(ctx, llmActionExplainResults, prompt, sql, ""),
		SQL:    sql,
		Result: summarizeLLMResult(result.Columns, result.Rows),
	})
}

func (a *App) explainLLMError(ctx context.Context, prompt, sqlText, errorText string) (string, error) {
	llm := a.llmClient()
	if llm == nil {
		return "", errors.New("LLM is not configured")
	}
	return llm.Complete(ctx, LLMRequest{
		Action: llmActionExplainError,
		Prompt: prompt,
		Schema: a.llmSchemaContext(ctx, llmActionExplainError, prompt, sqlText, errorText),
		SQL:    sqlText,
		Error:  errorText,
	})
}

func chatCompletionsURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, "/chat/completions") {
		return baseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/chat/completions"
	}
	if u, err := url.Parse(baseURL); err == nil && u.Path == "" {
		return baseURL + "/v1/chat/completions"
	}
	return baseURL + "/chat/completions"
}

func trimForLLM(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "\n[truncated]"
}

func stripMarkdownCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) >= 3 && strings.HasPrefix(lines[0], "```") && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
		return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}
	return s
}

func (a *App) schemaSnapshot(ctx context.Context) string {
	return a.schemaSnapshotFor(ctx, "", "", "", "")
}

func (a *App) schemaSnapshotFor(ctx context.Context, action, prompt, sqlText, errorText string) string {
	conn := a.activeConn(ctx)
	if conn != nil && !conn.IsTinySQL() {
		return a.schemaSnapshotForSQLConnection(ctx, conn, action, prompt, sqlText, errorText)
	}
	tables := a.nativeDB.ListTables(a.tenant)
	if len(tables) == 0 {
		return a.emptySchemaSnapshot(ctx, action)
	}

	selected, _ := selectRAGTables(tables, prompt+" "+sqlText+" "+errorText)
	doc := ragContextDoc{
		Dialect: a.currentDialect(),
		Skill:   llmSkillForAction(action),
	}

	for _, table := range tables {
		if table == nil {
			continue
		}
		if !selected[strings.ToLower(table.Name)] {
			continue
		}
		t := ragTableDoc{Name: table.Name, Kind: "table", Rows: len(table.Rows)}
		for _, col := range table.Cols {
			typeName := col.Type.String()
			if typeName == "" {
				typeName = "TEXT"
			}
			colDoc := ragColumnDoc{
				Name:       col.Name,
				Type:       typeName,
				Constraint: col.Constraint.String(),
				Samples:    a.sampleColumnValues(ctx, table.Name, col.Name, maxLLMSampleValues),
			}
			if col.ForeignKey != nil {
				colDoc.References = col.ForeignKey.Table + "." + col.ForeignKey.Column
				doc.Relationships = append(doc.Relationships, ragRelationshipDoc{
					From:     table.Name + "." + col.Name,
					To:       col.ForeignKey.Table + "." + col.ForeignKey.Column,
					OnDelete: col.ForeignKey.OnDelete.String(),
					OnUpdate: col.ForeignKey.OnUpdate.String(),
				})
			}
			t.Columns = append(t.Columns, colDoc)
		}
		doc.Tables = append(doc.Tables, t)
	}
	for _, viewName := range a.tinySQLViewNames(ctx) {
		if len(doc.Tables) >= maxRAGTables {
			break
		}
		meta, err := a.queryBackedMeta(ctx, viewName, "view")
		if err != nil {
			continue
		}
		t := ragTableDoc{Name: meta.Name, Kind: "view", Rows: meta.RowCount}
		for _, col := range meta.Columns {
			t.Columns = append(t.Columns, ragColumnDoc{
				Name:    col.Name,
				Type:    col.TypeName,
				Samples: a.sampleColumnValues(ctx, meta.Name, col.Name, maxLLMSampleValues),
			})
		}
		doc.Tables = append(doc.Tables, t)
	}
	// Minified: this is what actually gets sent to the LLM as the RAG
	// context, and pretty-printing whitespace is pure wasted tokens. The
	// "Details" preview in the SQL editor re-indents a copy for display
	// (see beautifyJSONForDisplay) without changing what's really sent.
	data, err := json.Marshal(doc)
	if err != nil {
		return a.emptySchemaSnapshot(ctx, action)
	}
	return string(data)
}

func (a *App) llmSchemaContext(ctx context.Context, action, prompt, sqlText, errorText string) string {
	return a.schemaSnapshotFor(ctx, action, prompt, sqlText, errorText)
}

func (a *App) schemaSnapshotForSQLConnection(ctx context.Context, conn *DBConnection, action, prompt, sqlText, errorText string) string {
	objects, err := conn.tableObjects(ctx)
	if err != nil || len(objects) == 0 {
		return a.emptySchemaSnapshot(ctx, action)
	}
	names := make([]string, 0, len(objects))
	kinds := make(map[string]string, len(objects))
	for _, obj := range objects {
		names = append(names, obj.Name)
		kinds[strings.ToLower(obj.Name)] = obj.Kind
	}
	selectedNames := selectRAGTableNames(names, prompt+" "+sqlText+" "+errorText)
	doc := ragContextDoc{
		Dialect: conn.Dialect,
		Skill:   llmSkillForAction(action),
	}
	for _, name := range selectedNames {
		meta, err := conn.tableMeta(ctx, name)
		if err != nil {
			continue
		}
		t := ragTableDoc{Name: meta.Name, Kind: kinds[strings.ToLower(meta.Name)], Rows: meta.RowCount}
		for _, col := range meta.Columns {
			t.Columns = append(t.Columns, ragColumnDoc{
				Name:    col.Name,
				Type:    col.TypeName,
				Samples: conn.sampleColumnValues(ctx, meta.Name, col.Name, maxLLMSampleValues),
			})
		}
		doc.Tables = append(doc.Tables, t)
	}
	// Minified: this is what actually gets sent to the LLM as the RAG
	// context, and pretty-printing whitespace is pure wasted tokens. The
	// "Details" preview in the SQL editor re-indents a copy for display
	// (see beautifyJSONForDisplay) without changing what's really sent.
	data, err := json.Marshal(doc)
	if err != nil {
		return a.emptySchemaSnapshot(ctx, action)
	}
	return string(data)
}

func (a *App) emptySchemaSnapshot(ctx context.Context, action string) string {
	dialect := a.currentDialect()
	if conn := a.activeConn(ctx); conn != nil {
		dialect = conn.Dialect
	}
	doc := ragContextDoc{
		Dialect: dialect,
		Skill:   llmSkillForAction(action),
		Tables:  []ragTableDoc{},
	}
	// Minified: this is what actually gets sent to the LLM as the RAG
	// context, and pretty-printing whitespace is pure wasted tokens. The
	// "Details" preview in the SQL editor re-indents a copy for display
	// (see beautifyJSONForDisplay) without changing what's really sent.
	data, err := json.Marshal(doc)
	if err != nil {
		return `{"dialect":{"name":"tinySQL"},"tables":[]}`
	}
	return string(data)
}

func (a *App) sampleColumnValues(ctx context.Context, table, column string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	query := fmt.Sprintf("SELECT %s FROM %s LIMIT %d", quoteName(column), quoteName(table), limit)
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "llm.sample_values", query)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var samples []string
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return samples
		}
		if v == nil {
			continue
		}
		samples = append(samples, anyToString(v))
	}
	return samples
}

func summarizeLLMResult(columns []string, rows [][]string) *LLMResultContext {
	return resultutil.SummarizeMatrix(columns, rows, resultutil.SummaryOptions{
		MaxRows:      maxLLMContextRows,
		MaxExamples:  maxLLMSampleValues,
		MaxTopValues: maxLLMTopValues,
	})
}

func (a *App) validateAutoRunnableSQL(ctx context.Context, sqlText string) error {
	sqlText = strings.TrimSpace(sqlText)
	if sqlText == "" {
		return errors.New("generated SQL is empty")
	}
	if !isSingleSQLStatement(sqlText) {
		return errors.New("generated SQL must contain a single statement")
	}
	class := classifySQL(sqlText)
	if class != sqlutil.StatementReadQuery {
		return errors.New("automatic execution is limited to SELECT, WITH, SHOW, or EXPLAIN")
	}
	dialect := a.currentDialect()
	if conn := a.activeConn(ctx); conn != nil {
		dialect = conn.Dialect
	}
	upper := strings.ToUpper(sqlText)
	for _, word := range dialect.BlockedAutoWords {
		if containsSQLWord(upper, word) {
			return fmt.Errorf("automatic execution blocked because SQL contains %s", word)
		}
	}
	return nil
}

func isSingleSQLStatement(sqlText string) bool {
	return classifySQL(sqlText) != sqlutil.StatementScript
}

func containsSQLWord(upperSQL, word string) bool {
	fields := strings.FieldsFunc(upperSQL, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z')
	})
	for _, f := range fields {
		if f == word {
			return true
		}
	}
	return false
}
