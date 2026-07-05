package main

import (
	"context"
	"fmt"
	"strings"
)

type MigrationSummary struct {
	SourceID    string
	TargetID    string
	SourceTable string
	TargetTable string
	Created     bool
	Rows        int
}

func (a *App) migrateTable(ctx context.Context, sourceID, targetID, sourceTable, targetTable string, createTarget bool) (MigrationSummary, error) {
	sourceID = strings.TrimSpace(sourceID)
	targetID = strings.TrimSpace(targetID)
	sourceTable = strings.TrimSpace(sourceTable)
	targetTable = strings.TrimSpace(targetTable)
	if sourceID == "" || targetID == "" {
		return MigrationSummary{}, fmt.Errorf("source and target connections are required")
	}
	if sourceTable == "" {
		return MigrationSummary{}, fmt.Errorf("source table is required")
	}
	if targetTable == "" {
		targetTable = sourceTable
	}
	if !isValidIdentifier(targetTable) {
		return MigrationSummary{}, fmt.Errorf("target table may only contain letters, digits, and underscores")
	}

	source := a.conns.Get(sourceID)
	target := a.conns.Get(targetID)
	if source == nil {
		return MigrationSummary{}, fmt.Errorf("source connection %q not found", sourceID)
	}
	if target == nil {
		return MigrationSummary{}, fmt.Errorf("target connection %q not found", targetID)
	}
	if source.ID == target.ID && strings.EqualFold(sourceTable, targetTable) {
		return MigrationSummary{}, fmt.Errorf("source and target must not be the same table on the same connection")
	}

	sourceCtx := contextWithActiveConnection(ctx, source)
	meta, err := a.tableMeta(sourceCtx, sourceTable)
	if err != nil {
		return MigrationSummary{}, err
	}

	summary := MigrationSummary{
		SourceID:    source.ID,
		TargetID:    target.ID,
		SourceTable: meta.Name,
		TargetTable: targetTable,
	}
	if createTarget {
		if err := createMigratedTable(ctx, target, targetTable, meta.Columns); err != nil {
			return summary, err
		}
		summary.Created = true
	}

	rows, err := source.DB.QueryContext(ctx, "SELECT * FROM "+source.QuoteIdent(meta.Name))
	if err != nil {
		return summary, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return summary, err
	}
	if len(columns) == 0 {
		return summary, nil
	}

	insertSQL := migrationInsertSQL(target, targetTable, columns)
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return summary, err
		}
		if _, err := target.DB.ExecContext(ctx, insertSQL, values...); err != nil {
			return summary, err
		}
		summary.Rows++
	}
	return summary, rows.Err()
}

func createMigratedTable(ctx context.Context, target *DBConnection, table string, columns []Column) error {
	if len(columns) == 0 {
		return fmt.Errorf("source table has no columns")
	}
	defs := make([]string, 0, len(columns))
	for _, col := range columns {
		if !isValidIdentifier(col.Name) {
			return fmt.Errorf("column %q cannot be migrated automatically", col.Name)
		}
		defs = append(defs, target.QuoteIdent(col.Name)+" "+migrationColumnType(target, col.TypeName))
	}
	ddl := fmt.Sprintf("CREATE TABLE %s (%s)", target.QuoteIdent(table), strings.Join(defs, ", "))
	_, err := target.DB.ExecContext(ctx, ddl)
	return err
}

func migrationInsertSQL(target *DBConnection, table string, columns []string) string {
	quoted := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, col := range columns {
		quoted[i] = target.QuoteIdent(col)
		placeholders[i] = target.Placeholder(i + 1)
	}
	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		target.QuoteIdent(table),
		strings.Join(quoted, ", "),
		strings.Join(placeholders, ", "),
	)
}

func migrationColumnType(target *DBConnection, sourceType string) string {
	t := strings.ToUpper(strings.TrimSpace(sourceType))
	switch target.Dialect.Name {
	case "PostgreSQL":
		return postgresMigrationType(t)
	case "MariaDB/MySQL":
		return mysqlMigrationType(t)
	case "Microsoft SQL Server":
		return mssqlMigrationType(t)
	default:
		return sqliteLikeMigrationType(t)
	}
}

func sqliteLikeMigrationType(t string) string {
	switch {
	case strings.Contains(t, "INT"):
		return "INT"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"), strings.Contains(t, "DEC"), strings.Contains(t, "NUM"):
		return "FLOAT"
	case strings.Contains(t, "BOOL"), t == "BIT":
		return "BOOL"
	default:
		return "TEXT"
	}
}

func postgresMigrationType(t string) string {
	switch {
	case strings.Contains(t, "INT"):
		return "BIGINT"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"), strings.Contains(t, "DEC"), strings.Contains(t, "NUM"):
		return "DOUBLE PRECISION"
	case strings.Contains(t, "BOOL"), t == "BIT":
		return "BOOLEAN"
	default:
		return "TEXT"
	}
}

func mysqlMigrationType(t string) string {
	switch {
	case strings.Contains(t, "INT"):
		return "BIGINT"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"), strings.Contains(t, "DEC"), strings.Contains(t, "NUM"):
		return "DOUBLE"
	case strings.Contains(t, "BOOL"), t == "BIT":
		return "BOOLEAN"
	default:
		return "TEXT"
	}
}

func mssqlMigrationType(t string) string {
	switch {
	case strings.Contains(t, "INT"):
		return "BIGINT"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"), strings.Contains(t, "DEC"), strings.Contains(t, "NUM"):
		return "FLOAT"
	case strings.Contains(t, "BOOL"), t == "BIT":
		return "BIT"
	default:
		return "NVARCHAR(MAX)"
	}
}
