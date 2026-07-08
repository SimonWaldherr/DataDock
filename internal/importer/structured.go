package importer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tinysql "github.com/SimonWaldherr/tinySQL"
	_ "modernc.org/sqlite"
)

func ImportSQLite(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "datadock-sqlite-*.sqlite")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", tmpName)
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, err
	}
	records, err := sqliteRecords(ctx, sqlDB)
	if err != nil {
		return nil, err
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

func sqliteRecords(ctx context.Context, db *sql.DB) ([][]string, error) {
	tableRows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer tableRows.Close()
	var tables []string
	columnSeen := map[string]bool{"source_table": true}
	var columns []string
	for tableRows.Next() {
		var table string
		if err := tableRows.Scan(&table); err != nil {
			return nil, err
		}
		tables = append(tables, table)
		cols, err := sqliteTableColumns(ctx, db, table)
		if err != nil {
			return nil, err
		}
		for _, col := range cols {
			name := sanitizeIdentifier(col, "col")
			if !columnSeen[name] {
				columnSeen[name] = true
				columns = append(columns, name)
			}
		}
	}
	if err := tableRows.Err(); err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("sqlite database contains no user tables or views")
	}
	header := append([]string{"source_table"}, columns...)
	records := [][]string{header}
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, `SELECT * FROM `+quoteIdent(table))
		if err != nil {
			return nil, err
		}
		names, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, err
		}
		nameIndex := map[string]int{}
		for i, name := range names {
			nameIndex[sanitizeIdentifier(name, "col")] = i
		}
		for rows.Next() {
			values := make([]any, len(names))
			ptrs := make([]any, len(names))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			record := make([]string, len(header))
			record[0] = table
			for i, col := range columns {
				if idx, ok := nameIndex[col]; ok {
					record[i+1] = importValueString(values[idx])
				}
			}
			records = append(records, record)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return records, nil
}

func sqliteTableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

func ImportDuckDBManifest(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	return importFileManifest(ctx, db, tenant, tableName, "duckdb", src, opts, "DuckDB files are detected and cataloged; row import needs a DuckDB reader or export to SQLite/Parquet.")
}

func ImportColumnarManifest(ctx context.Context, db *tinysql.DB, tenant, tableName, format string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	return importFileManifest(ctx, db, tenant, tableName, format, src, opts, "Columnar file detected. Full row import requires a Parquet/Arrow reader dependency; this import records file-level metadata.")
}

func importFileManifest(ctx context.Context, db *tinysql.DB, tenant, tableName, format string, src io.Reader, opts *ImportOptions, note string) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	records := [][]string{{"file_format", "size_bytes", "magic", "note"}}
	records = append(records, []string{format, strconv.Itoa(len(data)), fileMagicHex(data), note})
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

func fileMagicHex(data []byte) string {
	n := len(data)
	if n > 16 {
		n = 16
	}
	return strings.ToUpper(hex.EncodeToString(data[:n]))
}

var (
	htmlTableRE = regexp.MustCompile(`(?is)<table\b[^>]*>(.*?)</table>`)
	htmlRowRE   = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	htmlCellRE  = regexp.MustCompile(`(?is)<t[hd]\b[^>]*>(.*?)</t[hd]>`)
	htmlTagRE   = regexp.MustCompile(`(?is)<[^>]+>`)
)

func ImportHTMLTables(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	var records [][]string
	maxCols := 0
	tables := htmlTableRE.FindAllSubmatch(data, -1)
	for tableIdx, table := range tables {
		rows := htmlRowRE.FindAllSubmatch(table[1], -1)
		for rowIdx, row := range rows {
			cells := htmlCellRE.FindAllSubmatch(row[1], -1)
			if len(cells) == 0 {
				continue
			}
			record := []string{strconv.Itoa(tableIdx + 1), strconv.Itoa(rowIdx + 1)}
			for _, cell := range cells {
				record = append(record, cleanHTMLCell(string(cell[1])))
			}
			if len(record)-2 > maxCols {
				maxCols = len(record) - 2
			}
			records = append(records, record)
		}
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("html contains no importable tables")
	}
	header := []string{"table_index", "row_index"}
	for i := 1; i <= maxCols; i++ {
		header = append(header, fmt.Sprintf("col_%d", i))
	}
	out := [][]string{header}
	for _, record := range records {
		for len(record) < len(header) {
			record = append(record, "")
		}
		out = append(out, record)
	}
	return importSpatialRecords(ctx, db, tenant, tableName, out, opts)
}

func cleanHTMLCell(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = htmlTagRE.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

func ImportICalendar(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	lines := unfoldedLines(string(data))
	records := [][]string{{"component", "summary", "start", "end", "uid", "location", "description", "properties"}}
	for i := 0; i < len(lines); i++ {
		if strings.EqualFold(lines[i], "BEGIN:VEVENT") || strings.EqualFold(lines[i], "BEGIN:VTODO") {
			component := strings.TrimPrefix(strings.ToUpper(lines[i]), "BEGIN:")
			props := map[string]string{}
			for i++; i < len(lines); i++ {
				if strings.EqualFold(lines[i], "END:"+component) {
					break
				}
				key, value := propertyLine(lines[i])
				if key != "" {
					props[key] = value
				}
			}
			records = append(records, []string{
				component, props["SUMMARY"], firstNonEmpty(props["DTSTART"], props["DUE"]), props["DTEND"],
				props["UID"], props["LOCATION"], props["DESCRIPTION"], mustJSON(props),
			})
		}
	}
	if len(records) == 1 {
		return nil, fmt.Errorf("icalendar contains no VEVENT or VTODO components")
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

func ImportVCard(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	lines := unfoldedLines(string(data))
	records := [][]string{{"full_name", "name", "organization", "title", "email", "tel", "url", "properties"}}
	for i := 0; i < len(lines); i++ {
		if !strings.EqualFold(lines[i], "BEGIN:VCARD") {
			continue
		}
		props := map[string][]string{}
		for i++; i < len(lines); i++ {
			if strings.EqualFold(lines[i], "END:VCARD") {
				break
			}
			key, value := propertyLine(lines[i])
			if key != "" {
				props[key] = append(props[key], value)
			}
		}
		records = append(records, []string{
			firstSlice(props["FN"]), firstSlice(props["N"]), firstSlice(props["ORG"]), firstSlice(props["TITLE"]),
			strings.Join(props["EMAIL"], "; "), strings.Join(props["TEL"], "; "), strings.Join(props["URL"], "; "), mustJSON(props),
		})
	}
	if len(records) == 1 {
		return nil, fmt.Errorf("vcard contains no VCARD entries")
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

func unfoldedLines(s string) []string {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var out []string
	for _, line := range raw {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(out) > 0 {
				out[len(out)-1] += strings.TrimLeft(line, " \t")
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func propertyLine(line string) (string, string) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", ""
	}
	key := strings.ToUpper(strings.TrimSpace(line[:idx]))
	if semi := strings.Index(key, ";"); semi >= 0 {
		key = key[:semi]
	}
	return key, strings.TrimSpace(line[idx+1:])
}

func firstSlice(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func ImportMessagePack(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	value, rest, err := decodeMsgPack(data)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("messagepack contains trailing data")
	}
	return importDecodedValue(ctx, db, tenant, tableName, value, opts)
}

func ImportCBOR(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	value, rest, err := decodeCBOR(data)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("cbor contains trailing data")
	}
	return importDecodedValue(ctx, db, tenant, tableName, value, opts)
}

func ImportBSON(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	var values []map[string]any
	for len(data) > 0 {
		doc, rest, err := decodeBSONDocument(data)
		if err != nil {
			return nil, err
		}
		values = append(values, doc)
		data = rest
	}
	return importObjects(ctx, db, tenant, tableName, values, opts)
}

func importDecodedValue(ctx context.Context, db *tinysql.DB, tenant, tableName string, value any, opts *ImportOptions) (*ImportResult, error) {
	switch v := value.(type) {
	case []any:
		values := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if obj, ok := item.(map[string]any); ok {
				values = append(values, obj)
			} else {
				values = append(values, map[string]any{"value": normalizeDecodedScalar(item)})
			}
		}
		return importObjects(ctx, db, tenant, tableName, values, opts)
	case map[string]any:
		return importObjects(ctx, db, tenant, tableName, []map[string]any{v}, opts)
	default:
		return importObjects(ctx, db, tenant, tableName, []map[string]any{{"value": normalizeDecodedScalar(v)}}, opts)
	}
}

func normalizeDecodedScalar(v any) any {
	switch x := v.(type) {
	case map[string]any, []any:
		return mustJSON(x)
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	default:
		return x
	}
}

func decodeMsgPack(data []byte) (any, []byte, error) {
	if len(data) == 0 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	b := data[0]
	rest := data[1:]
	switch {
	case b <= 0x7f:
		return int64(b), rest, nil
	case b >= 0xe0:
		return int64(int8(b)), rest, nil
	case b >= 0xa0 && b <= 0xbf:
		return readString(rest, int(b&0x1f))
	case b >= 0x90 && b <= 0x9f:
		return decodeMsgPackArray(rest, int(b&0x0f))
	case b >= 0x80 && b <= 0x8f:
		return decodeMsgPackMap(rest, int(b&0x0f))
	}
	switch b {
	case 0xc0:
		return nil, rest, nil
	case 0xc2:
		return false, rest, nil
	case 0xc3:
		return true, rest, nil
	case 0xc4:
		if len(rest) < 1 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return readBytes(rest[1:], int(rest[0]))
	case 0xc5:
		if len(rest) < 2 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return readBytes(rest[2:], int(binary.BigEndian.Uint16(rest[:2])))
	case 0xca:
		if len(rest) < 4 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(rest[:4]))), rest[4:], nil
	case 0xcb:
		if len(rest) < 8 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return math.Float64frombits(binary.BigEndian.Uint64(rest[:8])), rest[8:], nil
	case 0xcc:
		if len(rest) < 1 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return uint64(rest[0]), rest[1:], nil
	case 0xcd:
		if len(rest) < 2 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return uint64(binary.BigEndian.Uint16(rest[:2])), rest[2:], nil
	case 0xce:
		if len(rest) < 4 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return uint64(binary.BigEndian.Uint32(rest[:4])), rest[4:], nil
	case 0xcf:
		if len(rest) < 8 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return binary.BigEndian.Uint64(rest[:8]), rest[8:], nil
	case 0xd0:
		if len(rest) < 1 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return int64(int8(rest[0])), rest[1:], nil
	case 0xd1:
		if len(rest) < 2 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return int64(int16(binary.BigEndian.Uint16(rest[:2]))), rest[2:], nil
	case 0xd2:
		if len(rest) < 4 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return int64(int32(binary.BigEndian.Uint32(rest[:4]))), rest[4:], nil
	case 0xd3:
		if len(rest) < 8 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return int64(binary.BigEndian.Uint64(rest[:8])), rest[8:], nil
	case 0xd9:
		if len(rest) < 1 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return readString(rest[1:], int(rest[0]))
	case 0xda:
		if len(rest) < 2 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return readString(rest[2:], int(binary.BigEndian.Uint16(rest[:2])))
	case 0xdc:
		if len(rest) < 2 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return decodeMsgPackArray(rest[2:], int(binary.BigEndian.Uint16(rest[:2])))
	case 0xde:
		if len(rest) < 2 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return decodeMsgPackMap(rest[2:], int(binary.BigEndian.Uint16(rest[:2])))
	default:
		return nil, nil, fmt.Errorf("unsupported messagepack type 0x%x", b)
	}
}

func decodeMsgPackArray(data []byte, n int) ([]any, []byte, error) {
	out := make([]any, 0, n)
	rest := data
	for i := 0; i < n; i++ {
		v, r, err := decodeMsgPack(rest)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, v)
		rest = r
	}
	return out, rest, nil
}

func decodeMsgPackMap(data []byte, n int) (map[string]any, []byte, error) {
	out := make(map[string]any, n)
	rest := data
	for i := 0; i < n; i++ {
		k, r, err := decodeMsgPack(rest)
		if err != nil {
			return nil, nil, err
		}
		v, r, err := decodeMsgPack(r)
		if err != nil {
			return nil, nil, err
		}
		out[fmt.Sprint(k)] = v
		rest = r
	}
	return out, rest, nil
}

func decodeCBOR(data []byte) (any, []byte, error) {
	if len(data) == 0 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	major := data[0] >> 5
	ai := data[0] & 0x1f
	value, rest, err := cborUint(ai, data[1:])
	if err != nil {
		return nil, nil, err
	}
	switch major {
	case 0:
		return value, rest, nil
	case 1:
		return -1 - int64(value), rest, nil
	case 2:
		return readBytes(rest, int(value))
	case 3:
		return readString(rest, int(value))
	case 4:
		out := make([]any, 0, value)
		for i := uint64(0); i < value; i++ {
			var v any
			v, rest, err = decodeCBOR(rest)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, v)
		}
		return out, rest, nil
	case 5:
		out := make(map[string]any, value)
		for i := uint64(0); i < value; i++ {
			var k, v any
			k, rest, err = decodeCBOR(rest)
			if err != nil {
				return nil, nil, err
			}
			v, rest, err = decodeCBOR(rest)
			if err != nil {
				return nil, nil, err
			}
			out[fmt.Sprint(k)] = v
		}
		return out, rest, nil
	case 7:
		switch ai {
		case 20:
			return false, data[1:], nil
		case 21:
			return true, data[1:], nil
		case 22:
			return nil, data[1:], nil
		case 26:
			if len(data) < 5 {
				return nil, nil, io.ErrUnexpectedEOF
			}
			return float64(math.Float32frombits(binary.BigEndian.Uint32(data[1:5]))), data[5:], nil
		case 27:
			if len(data) < 9 {
				return nil, nil, io.ErrUnexpectedEOF
			}
			return math.Float64frombits(binary.BigEndian.Uint64(data[1:9])), data[9:], nil
		}
	}
	return nil, nil, fmt.Errorf("unsupported cbor major type %d", major)
}

func cborUint(ai byte, data []byte) (uint64, []byte, error) {
	switch {
	case ai < 24:
		return uint64(ai), data, nil
	case ai == 24:
		if len(data) < 1 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return uint64(data[0]), data[1:], nil
	case ai == 25:
		if len(data) < 2 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return uint64(binary.BigEndian.Uint16(data[:2])), data[2:], nil
	case ai == 26:
		if len(data) < 4 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return uint64(binary.BigEndian.Uint32(data[:4])), data[4:], nil
	case ai == 27:
		if len(data) < 8 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return binary.BigEndian.Uint64(data[:8]), data[8:], nil
	default:
		return 0, nil, fmt.Errorf("unsupported cbor additional info %d", ai)
	}
}

func decodeBSONDocument(data []byte) (map[string]any, []byte, error) {
	if len(data) < 5 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	size := int(int32(binary.LittleEndian.Uint32(data[:4])))
	if size < 5 || size > len(data) {
		return nil, nil, fmt.Errorf("invalid bson document size %d", size)
	}
	body := data[4 : size-1]
	doc := map[string]any{}
	for len(body) > 0 {
		typ := body[0]
		body = body[1:]
		keyEnd := bytes.IndexByte(body, 0)
		if keyEnd < 0 {
			return nil, nil, fmt.Errorf("invalid bson key")
		}
		key := string(body[:keyEnd])
		body = body[keyEnd+1:]
		var value any
		var err error
		value, body, err = decodeBSONValue(typ, body)
		if err != nil {
			return nil, nil, err
		}
		doc[key] = value
	}
	return doc, data[size:], nil
}

func decodeBSONValue(typ byte, data []byte) (any, []byte, error) {
	switch typ {
	case 0x01:
		if len(data) < 8 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(data[:8])), data[8:], nil
	case 0x02:
		s, rest, err := bsonString(data)
		return s, rest, err
	case 0x03:
		return decodeBSONDocument(data)
	case 0x04:
		doc, rest, err := decodeBSONDocument(data)
		if err != nil {
			return nil, nil, err
		}
		keys := make([]string, 0, len(doc))
		for key := range doc {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			ai, _ := strconv.Atoi(keys[i])
			aj, _ := strconv.Atoi(keys[j])
			return ai < aj
		})
		out := make([]any, 0, len(keys))
		for _, key := range keys {
			out = append(out, doc[key])
		}
		return out, rest, nil
	case 0x05:
		if len(data) < 5 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		n := int(int32(binary.LittleEndian.Uint32(data[:4])))
		if n < 0 || len(data) < 5+n {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return append([]byte(nil), data[5:5+n]...), data[5+n:], nil
	case 0x08:
		if len(data) < 1 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return data[0] != 0, data[1:], nil
	case 0x09:
		if len(data) < 8 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		ms := int64(binary.LittleEndian.Uint64(data[:8]))
		return time.UnixMilli(ms).UTC().Format(time.RFC3339), data[8:], nil
	case 0x0a:
		return nil, data, nil
	case 0x10:
		if len(data) < 4 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return int64(int32(binary.LittleEndian.Uint32(data[:4]))), data[4:], nil
	case 0x12:
		if len(data) < 8 {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return int64(binary.LittleEndian.Uint64(data[:8])), data[8:], nil
	default:
		return nil, nil, fmt.Errorf("unsupported bson type 0x%x", typ)
	}
}

func bsonString(data []byte) (string, []byte, error) {
	if len(data) < 4 {
		return "", nil, io.ErrUnexpectedEOF
	}
	n := int(int32(binary.LittleEndian.Uint32(data[:4])))
	if n <= 0 || len(data) < 4+n {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(data[4 : 4+n-1]), data[4+n:], nil
}

func readString(data []byte, n int) (string, []byte, error) {
	if n < 0 || len(data) < n {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(data[:n]), data[n:], nil
}

func readBytes(data []byte, n int) ([]byte, []byte, error) {
	if n < 0 || len(data) < n {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return append([]byte(nil), data[:n]...), data[n:], nil
}

func importValueString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		if utf8.Valid(x) {
			return string(x)
		}
		return base64.StdEncoding.EncodeToString(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(x)
	}
}
