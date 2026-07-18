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

func (a *App) migrateTable(ctx context.Context, sessionID, sourceID, targetID, sourceTable, targetTable string, createTarget bool) (MigrationSummary, error) {
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

	source := a.conns.GetFor(sessionID, sourceID)
	target := a.conns.GetFor(sessionID, targetID)
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
		if err := a.createMigratedTable(ctx, target, targetTable, meta.Columns); err != nil {
			return summary, err
		}
		summary.Created = true
	}

	rows, err := a.queryConn(ctx, source, "migration.read", migrationSelectAllQuery(source, meta.Name))
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
		if _, err := a.execConn(ctx, target, "migration.insert", insertSQL, values...); err != nil {
			return summary, err
		}
		summary.Rows++
	}
	return summary, rows.Err()
}

func (a *App) createMigratedTable(ctx context.Context, target *DBConnection, table string, columns []Column) error {
	if len(columns) == 0 {
		return fmt.Errorf("source table has no columns")
	}
	for _, col := range columns {
		if !isValidIdentifier(col.Name) {
			return fmt.Errorf("column %q cannot be migrated automatically", col.Name)
		}
	}
	_, err := a.execConn(ctx, target, "migration.create_table", migrationCreateTableDDL(target, table, columns))
	return err
}

// migrationTypeClass is a source column type collapsed into the handful of
// buckets migrationColumnType actually distinguishes between, independent
// of the target dialect.
type migrationTypeClass int

const (
	migrationTypeText migrationTypeClass = iota
	migrationTypeInt
	migrationTypeFloat
	migrationTypeBool
)

func classifyMigrationType(t string) migrationTypeClass {
	switch {
	case strings.Contains(t, "INT"):
		return migrationTypeInt
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"), strings.Contains(t, "DEC"), strings.Contains(t, "NUM"):
		return migrationTypeFloat
	case strings.Contains(t, "BOOL"), t == "BIT":
		return migrationTypeBool
	default:
		return migrationTypeText
	}
}

// migrationTypeLiterals maps each migrationTypeClass to its literal type
// name per target dialect. Every dialect classifies a source type
// identically (classifyMigrationType); only the literal returned per class
// actually varies, so this replaces four near-identical per-dialect
// classify-and-switch functions with one lookup table.
var migrationTypeLiterals = map[string]map[migrationTypeClass]string{
	"PostgreSQL": {
		migrationTypeInt:   "BIGINT",
		migrationTypeFloat: "DOUBLE PRECISION",
		migrationTypeBool:  "BOOLEAN",
		migrationTypeText:  "TEXT",
	},
	"MariaDB/MySQL": {
		migrationTypeInt:   "BIGINT",
		migrationTypeFloat: "DOUBLE",
		migrationTypeBool:  "BOOLEAN",
		migrationTypeText:  "TEXT",
	},
	"Microsoft SQL Server": {
		migrationTypeInt:   "BIGINT",
		migrationTypeFloat: "FLOAT",
		migrationTypeBool:  "BIT",
		migrationTypeText:  "NVARCHAR(MAX)",
	},
}

// sqliteLikeMigrationTypeLiterals is the fallback for tinySQL, SQLite, and
// any other dialect without a more specific entry above.
var sqliteLikeMigrationTypeLiterals = map[migrationTypeClass]string{
	migrationTypeInt:   "INT",
	migrationTypeFloat: "FLOAT",
	migrationTypeBool:  "BOOL",
	migrationTypeText:  "TEXT",
}

func migrationColumnType(target *DBConnection, sourceType string) string {
	t := strings.ToUpper(strings.TrimSpace(sourceType))
	class := classifyMigrationType(t)
	literals, ok := migrationTypeLiterals[target.Dialect.Name]
	if !ok {
		literals = sqliteLikeMigrationTypeLiterals
	}
	return literals[class]
}
