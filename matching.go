package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SimonWaldherr/datadock/internal/match"
)

// matchAllowedMethods mirrors the match.Method constants; kept here (rather
// than exported from internal/match) so the HTTP layer can validate
// user-submitted method names with a clear error before the engine ever
// silently treats an unknown method as "no comparison".
var matchAllowedMethods = map[string]match.Method{
	"exact":      match.MethodExact,
	"exact_ci":   match.MethodExactCI,
	"normalized": match.MethodNormalized,
	"similarity": match.MethodSimilarity,
	"token_set":  match.MethodTokenSet,
	"address":    match.MethodAddress,
	"numeric":    match.MethodNumeric,
	"ean":        match.MethodEAN,
}

// MatchFieldSpec is the user-facing configuration for one compared column
// pair, as submitted from the /match form. SourceColumn and TargetColumn
// need not share a name — comparing "company_name" against "kunde_name" is
// exactly the cross-ERP case this feature exists for.
type MatchFieldSpec struct {
	SourceColumn string
	TargetColumn string
	Method       string
	Weight       float64
	Tolerance    float64
	// Group links this field to others sharing the same non-empty name into
	// a composite key (e.g. "street" + "postal code") — see
	// match.FieldRule.Group.
	Group string
}

// MatchRequest configures one matching run between two tables, which may
// live on different connections (e.g. two ERP systems' customer or article
// master data). It is intentionally domain-agnostic: nothing here mentions
// customers, articles, or any other entity type.
type MatchRequest struct {
	SourceConnID    string
	TargetConnID    string
	SourceTable     string
	TargetTable     string
	SourceKeyColumn string
	TargetKeyColumn string
	Fields          []MatchFieldSpec
	AutoThreshold   float64
	ReviewThreshold float64
	NoBlocking      bool
}

// MatchResultRow is one candidate pair, ready for display, CSV export, or
// persistence.
type MatchResultRow struct {
	SourceKey   string
	SourceLabel string
	TargetKey   string
	TargetLabel string
	Score       float64
	ScorePct    int // Score rounded to a whole percentage, for display without a template math helper
	Status      string
}

// MatchRunSummary is the full outcome of one matching run.
type MatchRunSummary struct {
	SourceTable      string
	TargetTable      string
	TotalSource      int
	TotalTarget      int
	AutoCount        int
	ReviewCount      int
	UnmatchedSources int
	BlockingUsed     bool
	Results          []MatchResultRow
}

// runMatch loads the configured columns from both tables, scores every
// plausible pair with the internal/match engine, and returns the results
// sorted by descending score (best matches first).
func (a *App) runMatch(ctx context.Context, sessionID string, req MatchRequest) (MatchRunSummary, error) {
	if len(req.Fields) == 0 {
		return MatchRunSummary{}, fmt.Errorf("at least one field to compare is required")
	}
	source := a.conns.GetFor(sessionID, req.SourceConnID)
	if source == nil {
		return MatchRunSummary{}, fmt.Errorf("source connection %q not found", req.SourceConnID)
	}
	target := a.conns.GetFor(sessionID, req.TargetConnID)
	if target == nil {
		return MatchRunSummary{}, fmt.Errorf("target connection %q not found", req.TargetConnID)
	}
	if strings.TrimSpace(req.SourceTable) == "" || strings.TrimSpace(req.TargetTable) == "" {
		return MatchRunSummary{}, fmt.Errorf("source and target tables are required")
	}

	sourceCtx := contextWithActiveConnection(ctx, source)
	targetCtx := contextWithActiveConnection(ctx, target)

	sourceMeta, err := a.tableMeta(sourceCtx, req.SourceTable)
	if err != nil {
		return MatchRunSummary{}, fmt.Errorf("source table: %w", err)
	}
	targetMeta, err := a.tableMeta(targetCtx, req.TargetTable)
	if err != nil {
		return MatchRunSummary{}, fmt.Errorf("target table: %w", err)
	}

	sourceKeyCol, err := canonicalColumn(sourceMeta, req.SourceKeyColumn)
	if err != nil {
		return MatchRunSummary{}, fmt.Errorf("source key column: %w", err)
	}
	targetKeyCol, err := canonicalColumn(targetMeta, req.TargetKeyColumn)
	if err != nil {
		return MatchRunSummary{}, fmt.Errorf("target key column: %w", err)
	}

	fieldRules := make([]match.FieldRule, len(req.Fields))
	resolvedFields := make([]MatchFieldSpec, len(req.Fields))
	for i, f := range req.Fields {
		method, ok := matchAllowedMethods[strings.TrimSpace(f.Method)]
		if !ok {
			return MatchRunSummary{}, fmt.Errorf("field %d: unknown comparison method %q", i+1, f.Method)
		}
		srcCol, err := canonicalColumn(sourceMeta, f.SourceColumn)
		if err != nil {
			return MatchRunSummary{}, fmt.Errorf("field %d source column: %w", i+1, err)
		}
		tgtCol, err := canonicalColumn(targetMeta, f.TargetColumn)
		if err != nil {
			return MatchRunSummary{}, fmt.Errorf("field %d target column: %w", i+1, err)
		}
		weight := f.Weight
		if weight <= 0 {
			weight = 1
		}
		fieldRules[i] = match.FieldRule{Label: srcCol, Method: method, Weight: weight, Tolerance: f.Tolerance, Group: strings.TrimSpace(f.Group)}
		resolvedFields[i] = MatchFieldSpec{SourceColumn: srcCol, TargetColumn: tgtCol}
	}

	sourceRows, sourceDisplay, err := a.loadMatchRows(sourceCtx, source, sourceMeta.Name, sourceKeyCol, resolvedFields, true)
	if err != nil {
		return MatchRunSummary{}, fmt.Errorf("reading source rows: %w", err)
	}
	targetRows, targetDisplay, err := a.loadMatchRows(targetCtx, target, targetMeta.Name, targetKeyCol, resolvedFields, false)
	if err != nil {
		return MatchRunSummary{}, fmt.Errorf("reading target rows: %w", err)
	}

	autoThreshold := req.AutoThreshold
	if autoThreshold <= 0 {
		autoThreshold = 0.9
	}
	reviewThreshold := req.ReviewThreshold
	if reviewThreshold <= 0 {
		reviewThreshold = 0.6
	}

	candidates, stats, err := match.Match(sourceRows, targetRows, match.Options{
		Fields:          fieldRules,
		AutoThreshold:   autoThreshold,
		ReviewThreshold: reviewThreshold,
		NoBlocking:      req.NoBlocking,
	})
	if err != nil {
		return MatchRunSummary{}, err
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })

	summary := MatchRunSummary{
		SourceTable:      sourceMeta.Name,
		TargetTable:      targetMeta.Name,
		TotalSource:      stats.TotalSource,
		TotalTarget:      stats.TotalTarget,
		AutoCount:        stats.AutoCount,
		ReviewCount:      stats.ReviewCount,
		UnmatchedSources: stats.UnmatchedSources,
		BlockingUsed:     stats.BlockingUsed,
		Results:          make([]MatchResultRow, 0, len(candidates)),
	}
	for _, c := range candidates {
		summary.Results = append(summary.Results, MatchResultRow{
			SourceKey:   sourceRows[c.SourceIdx].Key,
			SourceLabel: sourceDisplay[c.SourceIdx],
			TargetKey:   targetRows[c.TargetIdx].Key,
			TargetLabel: targetDisplay[c.TargetIdx],
			Score:       c.Score,
			ScorePct:    int(c.Score*100 + 0.5),
			Status:      c.Status,
		})
	}
	return summary, nil
}

// loadMatchRows reads the key column and every configured field column for
// one side of a match run — in the same field order used on both sides, so
// match.Row.Values[i] always belongs to the same FieldRule on both sides —
// and builds a simple joined display label per row for the review UI.
func (a *App) loadMatchRows(ctx context.Context, conn *DBConnection, table, keyColumn string, fields []MatchFieldSpec, isSource bool) ([]match.Row, []string, error) {
	cols := make([]string, 0, len(fields)+1)
	cols = append(cols, keyColumn)
	for _, f := range fields {
		if isSource {
			cols = append(cols, f.SourceColumn)
		} else {
			cols = append(cols, f.TargetColumn)
		}
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = conn.QuoteIdent(c)
	}
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(quoted, ", "), conn.QuoteIdent(table))
	rows, err := a.queryConn(ctx, conn, "match.read_rows", query)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	_, scanned, err := scanRows(rows)
	if err != nil {
		return nil, nil, err
	}
	if maxRows := a.currentMatchMaxRows(); len(scanned) > maxRows {
		return nil, nil, fmt.Errorf(
			"table %q has %d rows, over the %d-row limit for the interactive matcher; raise \"Matching Max Rows\" in Admin Settings if your server has the memory for it",
			table, len(scanned), maxRows)
	}

	out := make([]match.Row, len(scanned))
	labels := make([]string, len(scanned))
	for i, r := range scanned {
		values := append([]string(nil), r[1:]...)
		out[i] = match.Row{Key: r[0], Values: values}
		if label := joinNonEmpty(values, " | "); label != "" {
			labels[i] = label
		} else {
			labels[i] = r[0]
		}
	}
	return out, labels, nil
}

func joinNonEmpty(values []string, sep string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, sep)
}

// canonicalColumn resolves a (possibly differently-cased) user-supplied
// column name to the exact name reported by the database, so it can be
// safely quoted for case-sensitive dialects (PostgreSQL) and so an
// unrecognized column name fails with a clear error up front instead of a
// raw driver error deep inside a query.
//
// name is deliberately NOT trimmed before the comparison below: real-world
// schemas do have column names with meaningful leading/trailing spaces
// (e.g. a SQL Server export column literally named "Name "), and trimming
// would silently turn a correct match into a spurious "column not found".
// Only the blank-check uses a trimmed copy, since "" and "   " both mean
// "nothing selected".
func canonicalColumn(meta TableMeta, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("column name is required")
	}
	for _, col := range meta.Columns {
		if strings.EqualFold(col.Name, name) {
			return col.Name, nil
		}
	}
	// List what actually came back for meta.Name so a mismatch is
	// immediately diagnosable — e.g. a "Table" select that changed after
	// the field mapping was built against an earlier table (a stale
	// mapping, not a database/schema resolution problem), or a genuinely
	// unexpected schema.
	available := make([]string, len(meta.Columns))
	for i, col := range meta.Columns {
		available[i] = col.Name
	}
	return "", fmt.Errorf("column %q not found on table %q; available columns: %s", name, meta.Name, strings.Join(available, ", "))
}

// matchResultColumns is the fixed schema used both for the CSV export and
// for the crosswalk table created by saveMatchResults, kept in one place so
// the two stay in sync.
var matchResultColumns = []string{"source_key", "source_label", "target_key", "target_label", "score", "status", "matched_at"}

// ensureMatchResultTable creates the crosswalk table on conn if it doesn't
// already exist. An existing table is left as-is and simply appended to by
// saveMatchResults, so repeated match runs build a history — the same
// pattern external/prodmapper uses for its hist.CoBS_Mapping table — rather
// than each run clobbering the last.
func (a *App) ensureMatchResultTable(ctx context.Context, conn *DBConnection, tableName string) error {
	if _, err := a.tableMeta(contextWithActiveConnection(ctx, conn), tableName); err == nil {
		return nil
	}
	textType := migrationColumnType(conn, "TEXT")
	floatType := migrationColumnType(conn, "FLOAT")
	ddl := fmt.Sprintf(
		"CREATE TABLE %s (%s %s, %s %s, %s %s, %s %s, %s %s, %s %s, %s %s)",
		conn.QuoteIdent(tableName),
		conn.QuoteIdent(matchResultColumns[0]), textType,
		conn.QuoteIdent(matchResultColumns[1]), textType,
		conn.QuoteIdent(matchResultColumns[2]), textType,
		conn.QuoteIdent(matchResultColumns[3]), textType,
		conn.QuoteIdent(matchResultColumns[4]), floatType,
		conn.QuoteIdent(matchResultColumns[5]), textType,
		conn.QuoteIdent(matchResultColumns[6]), textType,
	)
	_, err := a.execConn(ctx, conn, "match.create_table", ddl)
	return err
}

// saveMatchResults appends rows to (creating first if needed) a crosswalk
// table on saveConn. It returns the number of rows written.
func (a *App) saveMatchResults(ctx context.Context, saveConn *DBConnection, tableName string, rows []MatchResultRow, matchedAt time.Time) (int, error) {
	tableName = strings.TrimSpace(tableName)
	if tableName == "" {
		return 0, fmt.Errorf("result table name is required")
	}
	if !isValidIdentifier(tableName) {
		return 0, fmt.Errorf("result table name may only contain letters, digits, and underscores")
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := a.ensureMatchResultTable(ctx, saveConn, tableName); err != nil {
		return 0, fmt.Errorf("creating result table: %w", err)
	}

	quoted := make([]string, len(matchResultColumns))
	placeholders := make([]string, len(matchResultColumns))
	for i, c := range matchResultColumns {
		quoted[i] = saveConn.QuoteIdent(c)
		placeholders[i] = saveConn.Placeholder(i + 1)
	}
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		saveConn.QuoteIdent(tableName), strings.Join(quoted, ", "), strings.Join(placeholders, ", "))

	stamp := matchedAt.UTC().Format(time.RFC3339)
	written := 0
	for _, row := range rows {
		if _, err := a.execConn(ctx, saveConn, "match.save", insertSQL,
			row.SourceKey, row.SourceLabel, row.TargetKey, row.TargetLabel, row.Score, row.Status, stamp,
		); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}
