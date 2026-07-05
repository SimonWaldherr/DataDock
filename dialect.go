package main

import "strings"

type DialectProfile struct {
	Name              string   `json:"name"`
	Aliases           []string `json:"-"`
	IdentifierQuote   string   `json:"identifier_quote"`
	PlaceholderStyle  string   `json:"placeholder_style"`
	LimitSyntax       string   `json:"limit_syntax"`
	StringConcat      string   `json:"string_concat,omitempty"`
	CaseInsensitiveOp string   `json:"case_insensitive_operator,omitempty"`
	SupportsReturning bool     `json:"supports_returning"`
	Notes             []string `json:"notes"`
	BlockedAutoWords  []string `json:"-"`
}

func DialectProfileForName(name string) DialectProfile {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = "tinysql"
	}
	for _, p := range dialectProfiles() {
		if strings.EqualFold(name, p.Name) {
			return p
		}
		for _, alias := range p.Aliases {
			if name == strings.ToLower(alias) {
				return p
			}
		}
	}
	p := tinySQLDialectProfile()
	p.Notes = append(p.Notes, "Unknown requested dialect; using tinySQL-compatible guidance.")
	return p
}

func dialectProfiles() []DialectProfile {
	return []DialectProfile{
		tinySQLDialectProfile(),
		{
			Name:              "SQLite",
			Aliases:           []string{"sqlite3"},
			IdentifierQuote:   `"identifier"`,
			PlaceholderStyle:  "?",
			LimitSyntax:       "LIMIT n OFFSET m",
			StringConcat:      "||",
			SupportsReturning: true,
			Notes: []string{
				"Use SQLite-compatible functions.",
				"Use date/time functions such as strftime when needed.",
				"Use double quotes for identifiers only when needed.",
			},
			BlockedAutoWords: defaultBlockedAutoWords(),
		},
		{
			Name:              "PostgreSQL",
			Aliases:           []string{"postgres", "pg"},
			IdentifierQuote:   `"identifier"`,
			PlaceholderStyle:  "$1, $2, ...",
			LimitSyntax:       "LIMIT n OFFSET m",
			StringConcat:      "||",
			CaseInsensitiveOp: "ILIKE",
			SupportsReturning: true,
			Notes: []string{
				"Use PostgreSQL syntax, not MySQL backticks or MSSQL TOP.",
				"Use ILIKE for case-insensitive text matching.",
				"Use double quotes for identifiers only when needed.",
			},
			BlockedAutoWords: defaultBlockedAutoWords(),
		},
		{
			Name:              "MariaDB/MySQL",
			Aliases:           []string{"mysql", "mariadb"},
			IdentifierQuote:   "`identifier`",
			PlaceholderStyle:  "?",
			LimitSyntax:       "LIMIT n OFFSET m",
			StringConcat:      "CONCAT(a, b)",
			SupportsReturning: false,
			Notes: []string{
				"Use MySQL/MariaDB syntax, including backticks for escaped identifiers.",
				"Do not use PostgreSQL ILIKE or MSSQL TOP.",
				"Use LIMIT for pagination.",
			},
			BlockedAutoWords: defaultBlockedAutoWords(),
		},
		{
			Name:              "Microsoft SQL Server",
			Aliases:           []string{"mssql", "sqlserver", "sql-server"},
			IdentifierQuote:   "[identifier]",
			PlaceholderStyle:  "@p1, @p2, ...",
			LimitSyntax:       "TOP n or OFFSET m ROWS FETCH NEXT n ROWS ONLY",
			StringConcat:      "+",
			SupportsReturning: false,
			Notes: []string{
				"Use T-SQL syntax.",
				"Use TOP for simple limits and OFFSET/FETCH for pagination.",
				"Use square brackets for escaped identifiers.",
				"Do not use PostgreSQL ILIKE or MySQL backticks.",
			},
			BlockedAutoWords: defaultBlockedAutoWords(),
		},
	}
}

func tinySQLDialectProfile() DialectProfile {
	return DialectProfile{
		Name:              "tinySQL",
		Aliases:           []string{"tiny", "tinysql"},
		IdentifierQuote:   `"identifier"`,
		PlaceholderStyle:  "?",
		LimitSyntax:       "LIMIT n OFFSET m",
		StringConcat:      "|| when supported; otherwise use simple expressions",
		SupportsReturning: true,
		Notes: []string{
			"Use tinySQL's SQLite-like subset.",
			"Prefer straightforward SELECT, JOIN, WHERE, GROUP BY, ORDER BY, LIMIT syntax.",
			"Avoid vendor-specific PostgreSQL, MySQL, or SQL Server functions unless explicitly supported by the schema/database.",
		},
		BlockedAutoWords: defaultBlockedAutoWords(),
	}
}

func defaultBlockedAutoWords() []string {
	return []string{"DROP", "DELETE", "UPDATE", "ALTER", "TRUNCATE", "ATTACH", "PRAGMA", "INSERT", "CREATE", "MERGE", "EXEC", "CALL"}
}
