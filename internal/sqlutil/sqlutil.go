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
	switch first {
	case "SELECT", "SHOW", "EXPLAIN", "DESCRIBE", "DESC":
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
		case "SELECT", "INSERT", "UPDATE", "DELETE", "MERGE", "REPLACE":
			if depth == 0 {
				return tok
			}
		}
	}
	return "WITH"
}

func containsStatementSeparator(query string) bool {
	inSingle := false
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
			i++
			for i < len(query) {
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
