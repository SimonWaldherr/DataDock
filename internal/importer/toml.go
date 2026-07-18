package importer

import (
	"context"
	"io"

	"github.com/BurntSushi/toml"
	tinysql "github.com/SimonWaldherr/tinySQL"
)

// ImportTOML reads a TOML document and imports it as one or more rows.
// TOML's root is always a table (map), never a bare array or scalar like
// JSON/YAML can be, so there's no single universal "this is the row list"
// convention the way a top-level JSON array is. Two shapes are supported:
//
//   - An array-of-tables ("[[section]]...[[section]]") at any top-level
//     key imports that array's entries as rows — this is TOML's own idiom
//     for repeated records, e.g. Cargo.lock's "[[package]]" blocks.
//   - Anything else (a flat table with no array-of-tables) imports as a
//     single row, matching how ImportJSON/ImportYAML treat a lone object.
//
// If more than one top-level key holds an array-of-tables, the first one
// in iteration order wins; TOML has no defined key order once decoded into
// a map, so which one that is isn't guaranteed. Most real TOML files
// (config files, Cargo.lock-style manifests) only have one such array.
func ImportTOML(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, err
	}
	if rows, ok := tomlArrayOfTables(doc); ok {
		return importObjects(ctx, db, tenant, tableName, rows, opts)
	}
	for k, v := range doc {
		doc[k] = normalizeTOMLValue(v)
	}
	return importObjects(ctx, db, tenant, tableName, []map[string]any{doc}, opts)
}

// tomlArrayOfTables looks for the first top-level key whose value is an
// array where every element is itself a table, and returns those entries
// (with nested values normalized) as import rows.
func tomlArrayOfTables(doc map[string]any) ([]map[string]any, bool) {
	for _, v := range doc {
		var candidates []any
		switch x := v.(type) {
		case []map[string]any:
			rows := make([]map[string]any, len(x))
			for i, row := range x {
				rows[i] = normalizeTOMLTable(row)
			}
			return rows, true
		case []any:
			candidates = x
		default:
			continue
		}
		if len(candidates) == 0 {
			continue
		}
		rows := make([]map[string]any, 0, len(candidates))
		allTables := true
		for _, item := range candidates {
			table, ok := item.(map[string]any)
			if !ok {
				allTables = false
				break
			}
			rows = append(rows, normalizeTOMLTable(table))
		}
		if allTables {
			return rows, true
		}
	}
	return nil, false
}

func normalizeTOMLTable(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = normalizeTOMLValue(v)
	}
	return out
}

// normalizeTOMLValue flattens nested tables/arrays to a JSON string (like
// ImportYAML's normalizeYAMLValue does for nested YAML structures) so
// fmt.Sprint renders every scalar leaf value sensibly.
func normalizeTOMLValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return mustJSON(x)
	case []map[string]any:
		return mustJSON(x)
	case []any:
		return mustJSON(x)
	default:
		return x
	}
}
