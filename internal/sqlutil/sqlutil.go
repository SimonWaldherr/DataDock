package sqlutil

import "strings"

type StatementClass string

const (
	StatementUnknown       StatementClass = "unknown"
	StatementReadQuery     StatementClass = "read_query"
	StatementWriteDML      StatementClass = "write_dml"
	StatementDDL           StatementClass = "ddl"
	StatementProcedureCall StatementClass = "procedure_call"
	StatementScript        StatementClass = "script"
)

func Classify(query string) StatementClass {
	tokens := sqlTokens(query)
	if len(tokens) == 0 {
		return StatementUnknown
	}
	if containsStatementSeparator(query) {
		return StatementScript
	}
	return classifyTokens(tokens)
}

func IsResultProducing(query string) bool {
	return Classify(query) == StatementReadQuery
}

func classifyTokens(tokens []string) StatementClass {
	first := strings.ToUpper(tokens[0])
	if first == "WITH" {
		first = cteMainVerb(tokens)
	}
	if first == "EXPLAIN" {
		if wrapped, analyzing := explainWrappedStatement(tokens); analyzing {
			return classifyTokens(wrapped)
		}
		return StatementReadQuery
	}
	if first == "PRAGMA" {
		if pragmaHasSideEffect(tokens) {
			return StatementWriteDML
		}
		return StatementReadQuery
	}
	switch first {
	case "SELECT", "SHOW", "DESCRIBE", "DESC":
		if containsVolatileCall(tokens) {
			return StatementWriteDML
		}
		return StatementReadQuery
	case "INSERT", "UPDATE", "DELETE", "MERGE", "REPLACE", "TRUNCATE":
		return StatementWriteDML
	case "CREATE", "DROP", "ALTER", "REFRESH", "GRANT", "REVOKE":
		return StatementDDL
	case "CALL", "EXEC", "EXECUTE":
		return StatementProcedureCall
	default:
		return StatementUnknown
	}
}

// pragmaSideEffectNames are PRAGMA options that change connection/database
// state on SQLite/tinySQL rather than simply reporting it. A dialect-
// agnostic classifier can't reliably tell a getter ("PRAGMA foreign_keys;")
// from a setter ("PRAGMA foreign_keys = OFF;") apart without a real
// expression parser, so any mention of one of these names is treated as a
// write — over-blocking a rare read-only getter is an acceptable cost for
// closing a real path to disabling FK/journal/durability guarantees through
// what every maintenance-mode, read-only-role, and audit-log check
// otherwise assumes is an inert introspection query. Not exhaustive.
var pragmaSideEffectNames = map[string]bool{
	"FOREIGN_KEYS":    true,
	"JOURNAL_MODE":    true,
	"SYNCHRONOUS":     true,
	"LOCKING_MODE":    true,
	"WRITABLE_SCHEMA": true,
	"AUTO_VACUUM":     true,
	"CACHE_SIZE":      true,
	"APPLICATION_ID":  true,
	"USER_VERSION":    true,
	"SCHEMA_VERSION":  true,
	"WAL_CHECKPOINT":  true,
	"OPTIMIZE":        true,
}

func pragmaHasSideEffect(tokens []string) bool {
	for _, tok := range tokens[1:] {
		if pragmaSideEffectNames[tok] {
			return true
		}
	}
	return false
}

// volatileFunctionNames are callables that mutate server/session state or
// consume unbounded resources even when invoked from inside an ordinary
// SELECT, so they pass every check keyed off the leading statement verb.
// Not exhaustive — a real allowlist would need per-dialect knowledge this
// classifier doesn't have; this denylist targets the specific,
// well-known, high-impact cases.
var volatileFunctionNames = map[string]bool{
	"PG_TERMINATE_BACKEND": true, // PostgreSQL: kill another session
	"PG_CANCEL_BACKEND":    true, // PostgreSQL: cancel another session's query
	"PG_RELOAD_CONF":       true, // PostgreSQL: reload server configuration
	"SETVAL":               true, // PostgreSQL: set a sequence's current value
	"NEXTVAL":              true, // PostgreSQL: advance a sequence
	"SLEEP":                true, // MySQL: SLEEP(n) as a resource-exhaustion/DoS primitive
	"BENCHMARK":            true, // MySQL: BENCHMARK(count, expr), CPU-exhaustion primitive
	"LOAD_FILE":            true, // MySQL: arbitrary local file read
}

func containsVolatileCall(tokens []string) bool {
	for i, tok := range tokens {
		if volatileFunctionNames[tok] && i+1 < len(tokens) && tokens[i+1] == "(" {
			return true
		}
	}
	return false
}

// explainWrappedStatement looks past a leading EXPLAIN for an ANALYZE
// option, in either the "EXPLAIN ANALYZE <stmt>" or the parenthesized
// "EXPLAIN (ANALYZE, ...) <stmt>" form. PostgreSQL (and MySQL 8.0.18+)
// actually execute the wrapped statement whenever ANALYZE is present —
// that's the entire point, since it reports real timing and row counts —
// so a caller must classify by the wrapped statement's verb, not treat
// every EXPLAIN as read-only; EXPLAIN ANALYZE DELETE really deletes rows.
// Returns false (with a nil slice) for a plain EXPLAIN, or for EXPLAIN with
// nothing after it, so the caller falls back to the safe read-only default.
func explainWrappedStatement(tokens []string) ([]string, bool) {
	i := 1
	analyzing := false
	if i < len(tokens) && tokens[i] == "(" {
		depth := 1
		i++
		for i < len(tokens) && depth > 0 {
			switch tokens[i] {
			case "(":
				depth++
			case ")":
				depth--
			case "ANALYZE":
				analyzing = true
			}
			i++
		}
	} else if i < len(tokens) && tokens[i] == "ANALYZE" {
		analyzing = true
		i++
	}
	if !analyzing || i >= len(tokens) {
		return nil, false
	}
	return tokens[i:], true
}

func cteMainVerb(tokens []string) string {
	depth := 0
	for i := 1; i < len(tokens); i++ {
		tok := strings.ToUpper(tokens[i])
		switch tok {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		case "INSERT", "UPDATE", "DELETE", "MERGE", "REPLACE":
			// A data-modifying statement inside a CTE is still a write, even
			// when the outer statement is SELECT (for example `WITH changed
			// AS (DELETE ... RETURNING ...) SELECT * FROM changed`). Returning
			// it immediately keeps read-only authorization and maintenance
			// mode from treating that statement as harmless.
			return tok
		case "SELECT":
			if depth == 0 {
				return tok
			}
		}
	}
	return "WITH"
}

func containsStatementSeparator(query string) bool {
	inSingle := false
	singleEscaped := false
	inDouble := false
	inLineComment := false
	inBlockComment := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		next := byte(0)
		if i+1 < len(query) {
			next = query[i+1]
		}
		if inLineComment {
			if c == '\n' || c == '\r' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			if c == '*' && next == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if inSingle {
			if singleEscaped && c == '\\' {
				i++
				continue
			}
			if c == '\'' {
				if next == '\'' {
					i++
				} else {
					inSingle = false
				}
			}
			continue
		}
		if inDouble {
			if c == '"' {
				if next == '"' {
					i++
				} else {
					inDouble = false
				}
			}
			continue
		}
		if c == '-' && next == '-' {
			inLineComment = true
			i++
			continue
		}
		if c == '/' && next == '*' {
			inBlockComment = true
			i++
			continue
		}
		if c == '\'' {
			inSingle = true
			singleEscaped = isEPrefixedString(query, i)
			continue
		}
		if c == '"' {
			inDouble = true
			continue
		}
		if c == ';' && strings.TrimSpace(query[i+1:]) != "" {
			return true
		}
	}
	return false
}

// isEPrefixedString reports whether the single-quote at query[quoteIdx] is
// immediately preceded by a standalone E/e — PostgreSQL's escape-string
// prefix, the one common case where backslash is a recognized in-string
// escape character even with standard_conforming_strings on (the default
// since Postgres 9.1). MySQL/MariaDB always treat backslash as an escape in
// ordinary strings too, but that can't be detected from the query text
// alone; see the sqlutil package doc for that residual gap.
//
// Within an escape-aware string, a backslash immediately before a quote
// must consume that quote as an escaped character rather than letting it
// pair up with the following quote as a doubled-quote escape — otherwise
// the scanner can close the string later than Postgres actually does and
// hide a real statement separator behind what looks like still-quoted
// text (e.g. `E'\''; DROP TABLE x; --'` reads as one harmless string
// without this check, when Postgres itself treats the DROP as a second,
// executable statement).
func isEPrefixedString(query string, quoteIdx int) bool {
	if quoteIdx == 0 {
		return false
	}
	prev := query[quoteIdx-1]
	if prev != 'E' && prev != 'e' {
		return false
	}
	return quoteIdx < 2 || !isIdentByte(query[quoteIdx-2])
}

func sqlTokens(query string) []string {
	query = strings.TrimSpace(strings.TrimLeft(query, "(\ufeff"))
	var tokens []string
	for i := 0; i < len(query); {
		c := query[i]
		if isSpace(c) || c == ',' {
			i++
			continue
		}
		if c == '-' && i+1 < len(query) && query[i+1] == '-' {
			i += 2
			for i < len(query) && query[i] != '\n' && query[i] != '\r' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(query) && query[i+1] == '*' {
			i += 2
			for i+1 < len(query) && !(query[i] == '*' && query[i+1] == '/') {
				i++
			}
			if i+1 < len(query) {
				i += 2
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote := c
			escaped := quote == '\'' && isEPrefixedString(query, i)
			i++
			for i < len(query) {
				if escaped && query[i] == '\\' {
					i += 2
					continue
				}
				if query[i] == quote {
					if i+1 < len(query) && query[i+1] == quote {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		if c == '(' || c == ')' || c == ';' {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if isIdentByte(c) {
			start := i
			i++
			for i < len(query) && isIdentByte(query[i]) {
				i++
			}
			tokens = append(tokens, strings.ToUpper(query[start:i]))
			continue
		}
		i++
	}
	return tokens
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}
