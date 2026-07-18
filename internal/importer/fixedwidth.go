package importer

import (
	"context"
	"fmt"
	"io"
	"strings"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// ImportFixedWidth reads a fixed-width text file (the classic mainframe/
// legacy-export format: every row has the same field boundaries measured
// in character positions, with no delimiter at all) and imports it as a
// table. Column boundaries aren't given explicitly; they're inferred from
// whitespace alignment across every line, the same heuristic tools like
// pandas' read_fwf(colspecs="infer") use: a character column that is blank
// (or past the end of the line) in every row is a gap between fields, and
// each maximal run of non-gap columns is one field.
func ImportFixedWidth(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	opts = normalizeOptions(opts)
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	text, encoding, err := decodeDelimitedText(data)
	if err != nil {
		return nil, err
	}
	lines := splitNonEmptyLines(text)
	if len(lines) == 0 {
		return nil, fmt.Errorf("fixed-width file contains no non-blank lines")
	}
	fields := inferFixedWidthFields(lines)
	if len(fields) == 0 {
		return nil, fmt.Errorf("could not infer any fixed-width column boundaries from this file")
	}
	records := make([][]string, len(lines))
	for i, line := range lines {
		records[i] = sliceFixedWidthFields(line, fields)
	}
	headerMode := opts.HeaderMode
	// There's no real delimiter character for a whitespace-alignment-based
	// format; ' ' is stored here only as a human-readable placeholder for
	// ImportResult.Delimiter's JSON representation.
	res, err := importRecords(ctx, db, tenant, tableName, records, ' ', opts)
	opts.HeaderMode = headerMode
	if res != nil {
		res.Encoding = encoding
	}
	return res, err
}

// fixedWidthField is a half-open character-position range [Start, End).
type fixedWidthField struct {
	Start, End int
}

func splitNonEmptyLines(text string) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// inferFixedWidthFields finds character positions that are blank (or past
// the line's own length) in every line, then returns the maximal non-gap
// runs between them as field boundaries.
func inferFixedWidthFields(lines []string) []fixedWidthField {
	maxLen := 0
	for _, line := range lines {
		if n := len([]rune(line)); n > maxLen {
			maxLen = n
		}
	}
	isGap := make([]bool, maxLen)
	for pos := 0; pos < maxLen; pos++ {
		gap := true
		for _, line := range lines {
			r := []rune(line)
			if pos < len(r) && r[pos] != ' ' && r[pos] != '\t' {
				gap = false
				break
			}
		}
		isGap[pos] = gap
	}
	var fields []fixedWidthField
	inField := false
	start := 0
	for pos := 0; pos < maxLen; pos++ {
		if !isGap[pos] && !inField {
			inField = true
			start = pos
		} else if isGap[pos] && inField {
			inField = false
			fields = append(fields, fixedWidthField{Start: start, End: pos})
		}
	}
	if inField {
		fields = append(fields, fixedWidthField{Start: start, End: maxLen})
	}
	return fields
}

func sliceFixedWidthFields(line string, fields []fixedWidthField) []string {
	r := []rune(line)
	out := make([]string, len(fields))
	for i, f := range fields {
		start, end := f.Start, f.End
		if start > len(r) {
			start = len(r)
		}
		if end > len(r) {
			end = len(r)
		}
		if start > end {
			start = end
		}
		out[i] = strings.TrimSpace(string(r[start:end]))
	}
	return out
}
