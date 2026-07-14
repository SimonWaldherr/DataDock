package main

import (
	"sort"
	"strings"
	"unicode"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type LLMSkillProfile struct {
	Name         string   `json:"name"`
	Purpose      string   `json:"purpose"`
	Instructions []string `json:"instructions"`
}

type ragContextDoc struct {
	Dialect       DialectProfile       `json:"dialect"`
	Skill         LLMSkillProfile      `json:"skill"`
	Tables        []ragTableDoc        `json:"tables"`
	Relationships []ragRelationshipDoc `json:"relationships,omitempty"`
}

// ragRetrievalDoc records which tables the lexical RAG search picked and
// why. selectRAGTables/selectRAGTableNames still return it for callers that
// want to log/inspect the decision, but it's deliberately NOT a field on
// ragContextDoc (so it's never sent to the LLM): the model doesn't need to
// know how its context was assembled, only what's in it, and the tables[]
// array above already lists exactly what was selected — shipping this too
// would just be duplicated tokens.
type ragRetrievalDoc struct {
	Method         string   `json:"method"`
	QueryTerms     []string `json:"query_terms,omitempty"`
	SelectedTables []string `json:"selected_tables,omitempty"`
	TotalTables    int      `json:"total_tables,omitempty"`
	MaxTables      int      `json:"max_tables,omitempty"`
	Truncated      bool     `json:"truncated"`
}

type ragColumnDoc struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Constraint string   `json:"constraint,omitempty"`
	References string   `json:"references,omitempty"`
	Samples    []string `json:"samples,omitempty"`
}

type ragTableDoc struct {
	Name    string         `json:"name"`
	Kind    string         `json:"kind,omitempty"`
	Rows    int            `json:"rows"`
	Columns []ragColumnDoc `json:"columns"`
}

type ragRelationshipDoc struct {
	From     string `json:"from"`
	To       string `json:"to"`
	OnDelete string `json:"on_delete,omitempty"`
	OnUpdate string `json:"on_update,omitempty"`
}

func llmSkillForAction(action string) LLMSkillProfile {
	switch action {
	case llmActionGenerateSQL, llmActionAskRun:
		return LLMSkillProfile{
			Name:    "text_to_sql",
			Purpose: "Turn a natural-language request into one useful SQL query for the active database.",
			Instructions: []string{
				"Use only tables and columns present in the retrieved schema context.",
				"If the request is ambiguous or required tables are missing, return action=clarify with one concrete follow-up question.",
				"Prefer read-only result queries.",
				"Use explicit JOIN syntax and qualify ambiguous columns.",
				"Match the active dialect profile exactly.",
			},
		}
	case llmActionFixSQL:
		return LLMSkillProfile{
			Name:    "sql_repair",
			Purpose: "Correct a failing SQL query while preserving the user's intent.",
			Instructions: []string{
				"Use the supplied database error and the active dialect profile.",
				"Use only tables and columns present in the retrieved schema context.",
				"Return one corrected read-only query when possible.",
				"Do not execute the correction or claim it was executed.",
				"Ask one concrete clarification when the error cannot be resolved from context.",
			},
		}
	case llmActionOptimizeSQL:
		return LLMSkillProfile{
			Name:    "sql_optimizer",
			Purpose: "Improve an existing SQL draft without changing the intended result.",
			Instructions: []string{
				"Preserve the query's output semantics unless the user explicitly asks for a different result.",
				"Use only tables and columns present in the retrieved schema context.",
				"Improve clarity, efficiency, or active-dialect correctness only when the context supports it.",
				"Return one reviewable SQL draft and never execute or claim to execute it.",
				"Ask one concrete clarification when preserving the intended result is ambiguous.",
			},
		}
	case llmActionExplainResults:
		return LLMSkillProfile{
			Name:    "result_explainer",
			Purpose: "Explain SQL result rows in natural language.",
			Instructions: []string{
				"Explain what the result shows, not how SQL generally works.",
				"Refer to column names and notable values from the result sample.",
				"Keep the answer concise and practical.",
				"Mention limitations when only a sample of rows is available.",
			},
		}
	case llmActionExplainError:
		return LLMSkillProfile{
			Name:    "sql_error_explainer",
			Purpose: "Explain a SQL/database error and suggest a correction.",
			Instructions: []string{
				"Explain the likely cause of the error in plain language.",
				"Use the active dialect and retrieved schema context.",
				"If useful, suggest a corrected query.",
				"Do not invent tables or columns that are absent from context.",
			},
		}
	case llmActionCreateChart:
		return LLMSkillProfile{
			Name:    "result_to_chart",
			Purpose: "Turn a tabular result sample into one safe chart specification.",
			Instructions: []string{
				"Use only column names present in the result sample.",
				"Return a compact chart JSON object, not JavaScript.",
				"Prefer bar charts for categories and line charts for time-like dimensions.",
				"Use aggregation=sum for numeric measures unless counting rows is more appropriate.",
			},
		}
	case llmActionReviewSQL:
		return LLMSkillProfile{
			Name:    "sql_reviewer",
			Purpose: "Independently review generated SQL without schema context.",
			Instructions: []string{
				"Describe the SQL intent.",
				"Call out safety concerns and whether it appears read-only.",
				"Do not assume unavailable schema details.",
			},
		}
	case llmActionSuggestQuestions:
		return LLMSkillProfile{
			Name:    "result_follow_ups",
			Purpose: "Suggest useful next questions about a SQL result without running anything.",
			Instructions: []string{
				"Use only the current result sample and retrieved schema context.",
				"Suggest three or four concrete, answerable questions.",
				"Do not invent findings or claim that a suggestion has been executed.",
				"Return the structured suggestions contract.",
			},
		}
	case llmActionAnalyzeQuality:
		return LLMSkillProfile{
			Name:    "result_quality_analyst",
			Purpose: "Identify observable data-quality signals in a bounded SQL result summary.",
			Instructions: []string{
				"Base observations only on the supplied result summary and retrieved schema context.",
				"Check for missing values, inconsistent categories, duplicates, and numeric anomalies when the summary supports it.",
				"Separate observed signals from recommended checks and mention sampling limitations.",
				"Do not invent findings, mutate data, generate SQL, or claim that anything was executed.",
			},
		}
	default:
		return LLMSkillProfile{
			Name:    "sql_assistant",
			Purpose: "Assist with SQL using retrieved schema and dialect context.",
			Instructions: []string{
				"Use retrieved context only.",
				"Ask for clarification when context is insufficient.",
			},
		}
	}
}

type tableScore struct {
	name  string
	score int
}

func selectRAGTables(tables []*tinysql.Table, query string) (map[string]bool, ragRetrievalDoc) {
	terms := tokenizeRAGQuery(query)
	selected := make(map[string]bool)
	retrieval := ragRetrievalDoc{
		Method:      "lexical-table-column-rag",
		QueryTerms:  terms,
		TotalTables: len(tables),
		MaxTables:   maxRAGTables,
	}

	if len(tables) <= maxRAGTables || len(terms) == 0 {
		for _, t := range tables {
			if t != nil {
				selected[strings.ToLower(t.Name)] = true
				retrieval.SelectedTables = append(retrieval.SelectedTables, t.Name)
			}
		}
		sort.Strings(retrieval.SelectedTables)
		return selected, retrieval
	}

	var scores []tableScore
	for _, t := range tables {
		if t == nil {
			continue
		}
		score := scoreTableForTerms(t, terms)
		scores = append(scores, tableScore{name: t.Name, score: score})
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return strings.ToLower(scores[i].name) < strings.ToLower(scores[j].name)
		}
		return scores[i].score > scores[j].score
	})

	limit := maxRAGTables
	if len(scores) < limit {
		limit = len(scores)
	}
	for i := 0; i < limit; i++ {
		selected[strings.ToLower(scores[i].name)] = true
		retrieval.SelectedTables = append(retrieval.SelectedTables, scores[i].name)
	}
	retrieval.Truncated = len(scores) > limit
	return selected, retrieval
}

func selectRAGTableNames(names []string, query string) []string {
	terms := tokenizeRAGQuery(query)
	if len(names) <= maxRAGTables || len(terms) == 0 {
		out := append([]string(nil), names...)
		sort.Strings(out)
		if len(out) > maxRAGTables {
			return out[:maxRAGTables]
		}
		return out
	}
	scores := make([]tableScore, 0, len(names))
	for _, name := range names {
		score := 0
		lowerName := strings.ToLower(name)
		for _, term := range terms {
			if term == lowerName {
				score += 8
			} else if strings.Contains(lowerName, term) || strings.Contains(term, lowerName) {
				score += 4
			}
		}
		scores = append(scores, tableScore{name: name, score: score})
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return strings.ToLower(scores[i].name) < strings.ToLower(scores[j].name)
		}
		return scores[i].score > scores[j].score
	})
	limit := maxRAGTables
	if len(scores) < limit {
		limit = len(scores)
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, scores[i].name)
	}
	return out
}

func scoreTableForTerms(t *tinysql.Table, terms []string) int {
	tableName := strings.ToLower(t.Name)
	score := 0
	for _, term := range terms {
		if term == tableName {
			score += 8
		} else if strings.Contains(tableName, term) || strings.Contains(term, tableName) {
			score += 4
		}
		for _, col := range t.Cols {
			colName := strings.ToLower(col.Name)
			if term == colName {
				score += 5
			} else if strings.Contains(colName, term) || strings.Contains(term, colName) {
				score += 2
			}
			if col.ForeignKey != nil {
				ref := strings.ToLower(col.ForeignKey.Table + " " + col.ForeignKey.Column)
				if strings.Contains(ref, term) {
					score++
				}
			}
		}
	}
	return score
}

func tokenizeRAGQuery(query string) []string {
	seen := make(map[string]bool)
	var terms []string
	for _, raw := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	}) {
		raw = strings.Trim(raw, "_")
		if len(raw) < 2 || ragStopWords[raw] || seen[raw] {
			continue
		}
		seen[raw] = true
		terms = append(terms, raw)
	}
	sort.Strings(terms)
	return terms
}

var ragStopWords = map[string]bool{
	"and": true, "are": true, "asc": true, "bei": true, "das": true, "der": true, "die": true,
	"for": true, "from": true, "how": true, "ich": true, "mit": true, "not": true, "oder": true,
	"select": true, "show": true, "sql": true, "the": true, "und": true, "was": true, "wer": true,
	"where": true, "with": true, "wie": true, "von": true,
}
