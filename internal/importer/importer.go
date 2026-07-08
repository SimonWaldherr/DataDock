package importer

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/SimonWaldherr/datadock/internal/typed"
	tinysql "github.com/SimonWaldherr/tinySQL"
	"gopkg.in/yaml.v3"
)

type ImportOptions struct {
	BatchSize           int
	NullLiterals        []string
	CreateTable         bool
	Truncate            bool
	HeaderMode          string
	DelimiterCandidates []rune
	TableName           string
	SampleBytes         int
	SampleRecords       int
	TypeInference       bool
	DateTimeFormats     []string
	StrictTypes         bool
}

type ImportResult struct {
	RowsInserted int64
	RowsSkipped  int64
	Delimiter    rune
	HadHeader    bool
	Encoding     string
	LineEnding   string
	ColumnNames  []string
	ColumnTypes  []tinysql.ColType
	Errors       []string
}

type FuzzyImportOptions struct {
	*ImportOptions
}

func ImportCSV(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	opts = normalizeOptions(opts)
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	delimiter := ','
	if len(opts.DelimiterCandidates) > 0 {
		delimiter = opts.DelimiterCandidates[0]
	} else if strings.Count(string(data), "\t") > strings.Count(string(data), ",") {
		delimiter = '\t'
	}
	reader := csv.NewReader(strings.NewReader(string(data)))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	return importRecords(ctx, db, tenant, tableName, records, delimiter, opts)
}

func FuzzyImportCSV(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *FuzzyImportOptions) (*ImportResult, error) {
	var base *ImportOptions
	if opts != nil {
		base = opts.ImportOptions
	}
	return ImportCSV(ctx, db, tenant, tableName, src, base)
}

func ImportJSON(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	var values []map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	return importObjects(ctx, db, tenant, tableName, values, opts)
}

func FuzzyImportJSON(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *FuzzyImportOptions) (*ImportResult, error) {
	var base *ImportOptions
	if opts != nil {
		base = opts.ImportOptions
	}
	return ImportJSON(ctx, db, tenant, tableName, src, base)
}

// ImportNDJSON reads newline-delimited JSON: one JSON object per record,
// rather than the single top-level array ImportJSON expects. It uses a
// streaming decoder instead of splitting on "\n" so pretty-printed or
// multi-line records still parse correctly.
func ImportNDJSON(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	dec := json.NewDecoder(src)
	var values []map[string]any
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		values = append(values, obj)
	}
	return importObjects(ctx, db, tenant, tableName, values, opts)
}

func FuzzyImportNDJSON(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *FuzzyImportOptions) (*ImportResult, error) {
	var base *ImportOptions
	if opts != nil {
		base = opts.ImportOptions
	}
	return ImportNDJSON(ctx, db, tenant, tableName, src, base)
}

// importObjects converts decoded JSON/YAML objects into the same
// header+rows shape ImportCSV produces, then delegates to importRecords.
func importObjects(ctx context.Context, db *tinysql.DB, tenant, tableName string, values []map[string]any, opts *ImportOptions) (*ImportResult, error) {
	opts = normalizeOptions(opts)
	columns := orderedJSONColumns(values)
	records := [][]string{columns}
	for _, obj := range values {
		row := make([]string, len(columns))
		for i, col := range columns {
			if v, ok := obj[col]; ok && v != nil {
				row[i] = fmt.Sprint(v)
			}
		}
		records = append(records, row)
	}
	headerMode := opts.HeaderMode
	opts.HeaderMode = "present"
	res, err := importRecords(ctx, db, tenant, tableName, records, ',', opts)
	opts.HeaderMode = headerMode
	return res, err
}

func ImportYAML(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	var values []map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	for _, obj := range values {
		for k, v := range obj {
			obj[k] = normalizeYAMLValue(v)
		}
	}
	return importObjects(ctx, db, tenant, tableName, values, opts)
}

// normalizeYAMLValue converts yaml.v3's decoded types (map[string]interface{}
// for mappings, but nested sequences/mappings can still surface as
// map[string]interface{}/[]interface{} with unexported-friendly types) into
// values that fmt.Sprint renders sensibly, and flattens nested structures to
// their JSON form instead of Go's default struct-ish %v output.
func normalizeYAMLValue(v any) any {
	switch v.(type) {
	case map[string]any, []any:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return v
}

func FuzzyImportYAML(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *FuzzyImportOptions) (*ImportResult, error) {
	var base *ImportOptions
	if opts != nil {
		base = opts.ImportOptions
	}
	return ImportYAML(ctx, db, tenant, tableName, src, base)
}

// xmlImportDoc mirrors the shape DataDock's own XML export produces
// (<result><columns><column>...</columns><rows><row><cell name="..">v</cell>
// ...</row></rows></result>), so exporting a table/query as XML and
// reimporting it round-trips.
type xmlImportDoc struct {
	XMLName xml.Name       `xml:"result"`
	Columns []string       `xml:"columns>column"`
	Rows    []xmlImportRow `xml:"rows>row"`
}

type xmlImportRow struct {
	Cells []xmlImportCell `xml:"cell"`
}

type xmlImportCell struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}

func ImportXML(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	var doc xmlImportDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	columns := doc.Columns
	if len(columns) == 0 {
		seen := map[string]bool{}
		for _, row := range doc.Rows {
			for _, cell := range row.Cells {
				if cell.Name != "" && !seen[cell.Name] {
					seen[cell.Name] = true
					columns = append(columns, cell.Name)
				}
			}
		}
	}
	opts = normalizeOptions(opts)
	if len(columns) == 0 {
		return &ImportResult{Encoding: "utf-8", LineEnding: "\n"}, nil
	}
	records := [][]string{columns}
	for _, row := range doc.Rows {
		values := make(map[string]string, len(row.Cells))
		for _, cell := range row.Cells {
			values[cell.Name] = cell.Value
		}
		record := make([]string, len(columns))
		for i, col := range columns {
			record[i] = values[col]
		}
		records = append(records, record)
	}
	headerMode := opts.HeaderMode
	opts.HeaderMode = "present"
	res, err := importRecords(ctx, db, tenant, tableName, records, ',', opts)
	opts.HeaderMode = headerMode
	return res, err
}

func FuzzyImportXML(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *FuzzyImportOptions) (*ImportResult, error) {
	var base *ImportOptions
	if opts != nil {
		base = opts.ImportOptions
	}
	return ImportXML(ctx, db, tenant, tableName, src, base)
}

func ImportKML(context.Context, *tinysql.DB, string, string, io.Reader, *ImportOptions) (*ImportResult, error) {
	return nil, fmt.Errorf("kml import is not supported in standalone datadock yet")
}

func importRecords(ctx context.Context, db *tinysql.DB, tenant, tableName string, records [][]string, delimiter rune, opts *ImportOptions) (*ImportResult, error) {
	res := &ImportResult{Delimiter: delimiter, Encoding: "utf-8", LineEnding: "\n"}
	if len(records) == 0 {
		return res, nil
	}
	header := hasHeader(records, opts.HeaderMode)
	res.HadHeader = header
	columns := make([]string, len(records[0]))
	if header {
		for i, name := range records[0] {
			columns[i] = sanitizeIdentifier(name, fmt.Sprintf("col_%d", i+1))
		}
		records = records[1:]
	} else {
		for i := range columns {
			columns[i] = fmt.Sprintf("col_%d", i+1)
		}
	}
	res.ColumnNames = columns
	columnKinds := make([]typed.Kind, len(columns))
	for i := range columnKinds {
		columnKinds[i] = typed.KindText
	}
	if opts.TypeInference {
		columnKinds = typed.InferColumns(records, len(columns))
	}
	res.ColumnTypes = make([]tinysql.ColType, len(columns))
	for i, kind := range columnKinds {
		res.ColumnTypes[i] = tinysqlType(kind)
	}
	if opts.Truncate {
		if _, err := exec(ctx, db, tenant, "DELETE FROM "+quoteIdent(tableName)); err != nil {
			return nil, err
		}
	}
	if opts.CreateTable {
		defs := make([]string, len(columns))
		for i, col := range columns {
			defs[i] = quoteIdent(col) + " " + sqlType(columnKinds[i])
		}
		if _, err := exec(ctx, db, tenant, "CREATE TABLE "+quoteIdent(tableName)+" ("+strings.Join(defs, ", ")+")"); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return nil, err
		}
	}
	for _, record := range records {
		values := make([]string, len(columns))
		for i := range columns {
			if i < len(record) {
				values[i] = typedSQLLiteral(record[i], columnKinds[i])
			} else {
				values[i] = "NULL"
			}
		}
		if _, err := exec(ctx, db, tenant, "INSERT INTO "+quoteIdent(tableName)+" ("+joinQuoted(columns)+") VALUES ("+strings.Join(values, ", ")+")"); err != nil {
			if opts.StrictTypes {
				return nil, err
			}
			res.RowsSkipped++
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		res.RowsInserted++
	}
	return res, nil
}

func normalizeOptions(opts *ImportOptions) *ImportOptions {
	if opts == nil {
		opts = &ImportOptions{}
	}
	if opts.HeaderMode == "" {
		opts.HeaderMode = "auto"
	}
	return opts
}

func hasHeader(records [][]string, mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "present":
		return true
	case "absent":
		return false
	}
	if len(records) < 2 {
		return true
	}
	for _, value := range records[0] {
		if !identifierRE.MatchString(strings.TrimSpace(value)) {
			return false
		}
	}
	for _, value := range records[1] {
		if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			return true
		}
	}
	return true
}

func orderedJSONColumns(values []map[string]any) []string {
	seen := map[string]bool{}
	var columns []string
	for _, obj := range values {
		for key := range obj {
			if !seen[key] {
				seen[key] = true
				columns = append(columns, key)
			}
		}
	}
	return columns
}

var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func sanitizeIdentifier(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	var b strings.Builder
	for i, r := range s {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" || out[0] >= '0' && out[0] <= '9' {
		return fallback
	}
	return out
}

func exec(ctx context.Context, db *tinysql.DB, tenant, sqlText string) (*tinysql.ResultSet, error) {
	stmt, err := tinysql.ParseSQL(sqlText)
	if err != nil {
		return nil, err
	}
	return tinysql.Execute(ctx, db, tenant, stmt)
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func joinQuoted(columns []string) string {
	quoted := make([]string, len(columns))
	for i, col := range columns {
		quoted[i] = quoteIdent(col)
	}
	return strings.Join(quoted, ", ")
}

func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func sqlType(kind typed.Kind) string {
	switch kind {
	case typed.KindInt:
		return "INT"
	case typed.KindFloat:
		return "FLOAT"
	case typed.KindBool:
		return "BOOL"
	case typed.KindDate:
		return "DATE"
	case typed.KindTime:
		return "TIME"
	case typed.KindDateTime:
		return "DATETIME"
	default:
		return "TEXT"
	}
}

func tinysqlType(kind typed.Kind) tinysql.ColType {
	switch kind {
	case typed.KindInt:
		return tinysql.IntType
	case typed.KindFloat:
		return tinysql.FloatType
	case typed.KindBool:
		return tinysql.BoolType
	case typed.KindDate:
		return tinysql.DateType
	case typed.KindTime:
		return tinysql.TimeType
	case typed.KindDateTime:
		return tinysql.DateTimeType
	default:
		return tinysql.TextType
	}
}

func typedSQLLiteral(raw string, kind typed.Kind) string {
	value := typed.Classify(raw)
	if value.Kind == typed.KindBlank {
		return "NULL"
	}
	switch kind {
	case typed.KindInt:
		if value.Kind == typed.KindInt {
			return strconv.FormatInt(value.Int, 10)
		}
	case typed.KindFloat:
		if value.Kind == typed.KindFloat || value.Kind == typed.KindInt {
			return strconv.FormatFloat(value.Float, 'g', -1, 64)
		}
	case typed.KindBool:
		if value.Kind == typed.KindBool {
			if value.Bool {
				return "TRUE"
			}
			return "FALSE"
		}
	case typed.KindDate, typed.KindTime, typed.KindDateTime:
		if value.Kind == kind {
			return sqlLiteral(strings.TrimSpace(raw))
		}
	}
	return sqlLiteral(raw)
}
