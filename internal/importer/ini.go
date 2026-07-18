package importer

import (
	"context"
	"fmt"
	"io"
	"strings"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// iniDefaultSectionName labels key/value pairs that appear before any
// [section] header. Deliberately not "" — the type-inference importer
// pipeline treats an empty string value as blank/NULL for any column
// (matching CSV null-handling conventions), which would make the default
// section indistinguishable from a NULL section in the imported table.
const iniDefaultSectionName = "(default)"

// ImportINI reads a classic .ini file (sections of key=value pairs) and
// imports one row per section, with "section" as an extra column plus one
// column per key seen anywhere in the file. Key/value pairs that appear
// before any [section] header are grouped under iniDefaultSectionName.
// Both "=" and ":" key/value separators are accepted, ";" and "#" start a
// comment line, and a quoted value ("..." or '...') has its surrounding
// quotes stripped.
func ImportINI(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	sections, order, err := parseINI(string(data))
	if err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("ini file contains no sections or key/value pairs")
	}
	values := make([]map[string]any, 0, len(order))
	for _, name := range order {
		displayName := name
		if displayName == "" {
			displayName = iniDefaultSectionName
		}
		row := map[string]any{"section": displayName}
		for k, v := range sections[name] {
			row[k] = v
		}
		values = append(values, row)
	}
	return importObjects(ctx, db, tenant, tableName, values, opts)
}

func parseINI(text string) (map[string]map[string]string, []string, error) {
	sections := map[string]map[string]string{}
	var order []string
	ensureSection := func(name string) {
		if _, ok := sections[name]; !ok {
			sections[name] = map[string]string{}
			order = append(order, name)
		}
	}
	current := ""
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for lineNum, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(line[1 : len(line)-1])
			ensureSection(current)
			continue
		}
		key, value, ok := splitINILine(line)
		if !ok {
			return nil, nil, fmt.Errorf("ini line %d is not a section header, comment, or key/value pair: %q", lineNum+1, raw)
		}
		ensureSection(current)
		sections[current][key] = value
	}
	// Drop the synthetic default section if the file never actually had any
	// pre-section keys; a file that's all [section] blocks shouldn't get a
	// spurious empty-section row.
	if entries, ok := sections[""]; ok && len(entries) == 0 {
		delete(sections, "")
		filtered := order[:0]
		for _, name := range order {
			if name != "" {
				filtered = append(filtered, name)
			}
		}
		order = filtered
	}
	return sections, order, nil
}

func splitINILine(line string) (key, value string, ok bool) {
	idx := strings.IndexAny(line, "=:")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	if key == "" {
		return "", "", false
	}
	return key, unquoteINIValue(strings.TrimSpace(line[idx+1:])), true
}

func unquoteINIValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
