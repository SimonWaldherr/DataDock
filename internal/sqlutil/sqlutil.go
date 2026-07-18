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
	switch first {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "PRAGMA":
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
