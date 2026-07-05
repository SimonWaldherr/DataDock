package sqlutil

import "strings"

func IsResultProducing(query string) bool {
	q := strings.TrimSpace(query)
	q = strings.TrimLeft(q, "(\ufeff")
	if q == "" {
		return false
	}
	first := strings.ToUpper(firstSQLWord(q))
	switch first {
	case "SELECT", "WITH", "SHOW", "EXPLAIN", "DESCRIBE", "DESC":
		return true
	default:
		return false
	}
}

func firstSQLWord(s string) string {
	for i, r := range s {
		if !(r == '_' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return s[:i]
		}
	}
	return s
}
