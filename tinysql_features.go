package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

const tinySQLAgentContextProcedure = "datadock_agent_context"

func (a *App) registerTinySQLProcedures() {
	_ = tinysql.RegisterStoredProcedure(tinySQLAgentContextProcedure, func(ctx tinysql.ProcedureContext, args []any) (*tinysql.ResultSet, error) {
		maxTables, maxChars, err := tinySQLAgentContextArgs(args)
		if err != nil {
			return nil, err
		}
		profile := a.buildTinySQLAgentContextProcedureText(maxTables, maxChars)
		return &tinysql.ResultSet{
			Cols: []string{"context"},
			Rows: []tinysql.Row{{"context": profile}},
		}, nil
	})
}

func tinySQLAgentContextArgs(args []any) (int, int, error) {
	maxTables := 12
	maxChars := 6000
	if len(args) > 2 {
		return 0, 0, fmt.Errorf("%s accepts at most 2 arguments: max_tables, max_chars", tinySQLAgentContextProcedure)
	}
	if len(args) >= 1 {
		v, err := positiveIntArg(args[0], "max_tables")
		if err != nil {
			return 0, 0, err
		}
		maxTables = v
	}
	if len(args) == 2 {
		v, err := positiveIntArg(args[1], "max_chars")
		if err != nil {
			return 0, 0, err
		}
		maxChars = v
	}
	return maxTables, maxChars, nil
}

func positiveIntArg(v any, name string) (int, error) {
	switch x := v.(type) {
	case int:
		if x > 0 {
			return x, nil
		}
	case int64:
		if x > 0 && x <= int64(^uint(0)>>1) {
			return int(x), nil
		}
	case float64:
		i := int(x)
		if x > 0 && float64(i) == x {
			return i, nil
		}
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(x))
		if err == nil && i > 0 {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%s must be a positive integer", name)
}

func (a *App) buildTinySQLAgentContext(ctx context.Context, maxTables, maxChars int) (string, error) {
	cfg := tinysql.DefaultAgentContextConfig()
	if maxTables > 0 {
		cfg.MaxTables = maxTables
	}
	if maxChars > 0 {
		cfg.MaxChars = maxChars
	}
	return tinysql.BuildAgentContext(ctx, a.nativeDB, a.tenant, cfg)
}

func (a *App) buildTinySQLAgentContextProcedureText(maxTables, maxChars int) string {
	if maxTables <= 0 {
		maxTables = 12
	}
	if maxChars <= 0 {
		maxChars = 6000
	}
	var b strings.Builder
	b.WriteString("tinySQL DataDock context\n")
	count := 0
	for _, table := range a.nativeDB.ListTables(a.tenant) {
		if table == nil || isDataDockSystemObject(table.Name) {
			continue
		}
		if count >= maxTables || (maxChars > 0 && b.Len() >= maxChars) {
			break
		}
		if count > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("table ")
		b.WriteString(table.Name)
		b.WriteString("(")
		for i, col := range table.Cols {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(col.Name)
			b.WriteByte(' ')
			b.WriteString(col.Type.String())
		}
		b.WriteString(")")
		count++
	}
	if maxChars > 0 && b.Len() > maxChars {
		return b.String()[:maxChars]
	}
	return b.String()
}
