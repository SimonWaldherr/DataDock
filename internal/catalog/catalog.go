package catalog

import (
	"context"
	"encoding/json"
	"strings"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type Object struct {
	Name string
	Type string
}

type AgentContextConfig struct {
	MaxTables          int
	MaxColumnsPerTable int
	MaxViews           int
	MaxJobs            int
	MaxChars           int
}

func DefaultAgentContextConfig() AgentContextConfig {
	return AgentContextConfig{MaxTables: 8, MaxColumnsPerTable: 10, MaxViews: 8, MaxJobs: 8, MaxChars: 8000}
}

func ListObjects(ctx context.Context, db *tinysql.DB, tenant string) ([]Object, error) {
	_ = ctx
	var objects []Object
	for _, table := range db.ListTables(tenant) {
		if table != nil {
			objects = append(objects, Object{Name: table.Name, Type: "TABLE"})
		}
	}
	if db.Catalog() != nil {
		for _, view := range db.Catalog().GetViews() {
			if view != nil {
				objects = append(objects, Object{Name: view.Name, Type: "VIEW"})
			}
		}
	}
	return objects, nil
}

func BuildAgentContext(ctx context.Context, db *tinysql.DB, tenant string, cfg AgentContextConfig) (string, error) {
	objects, err := ListObjects(ctx, db, tenant)
	if err != nil {
		return "", err
	}
	type columnDoc struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	type tableDoc struct {
		Name    string      `json:"name"`
		Kind    string      `json:"kind"`
		Columns []columnDoc `json:"columns"`
		Rows    int         `json:"rows"`
	}
	doc := struct {
		Tables []tableDoc `json:"tables"`
	}{}
	limit := cfg.MaxTables
	if limit <= 0 || limit > len(objects) {
		limit = len(objects)
	}
	for _, obj := range objects[:limit] {
		for _, table := range db.ListTables(tenant) {
			if table == nil || !strings.EqualFold(table.Name, obj.Name) {
				continue
			}
			td := tableDoc{Name: table.Name, Kind: "table", Rows: len(table.Rows)}
			colLimit := cfg.MaxColumnsPerTable
			if colLimit <= 0 || colLimit > len(table.Cols) {
				colLimit = len(table.Cols)
			}
			for _, col := range table.Cols[:colLimit] {
				td.Columns = append(td.Columns, columnDoc{Name: col.Name, Type: col.Type.String()})
			}
			doc.Tables = append(doc.Tables, td)
			break
		}
	}
	// Minified: this profile is sent to the LLM as-is, and pretty-printing
	// whitespace would just be wasted tokens (any human-facing display of
	// it re-indents on the way out instead, see beautifyJSONForDisplay).
	data, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	out := string(data)
	if cfg.MaxChars > 0 && len(out) > cfg.MaxChars {
		out = out[:cfg.MaxChars]
	}
	return "tinySQL agent profile\n" + out, nil
}
