package main

// A minimal, hand-rolled OOXML (.xlsx) codec: just enough of the
// spreadsheetml format to write a single-sheet result export and read one
// or more worksheets back in, without pulling in a full spreadsheet library
// for either direction.

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/SimonWaldherr/datadock/internal/typed"
)

func writeXLSX(w http.ResponseWriter, columns []string, rows [][]string) error {
	zw := zip.NewWriter(w)
	kinds := typed.InferColumns(rows, len(columns))
	files := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
			`</Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
			`</Relationships>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
			`</Relationships>`,
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="Result" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/styles.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
			`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><numFmts count="3"><numFmt numFmtId="164" formatCode="yyyy-mm-dd"/><numFmt numFmtId="165" formatCode="yyyy-mm-dd hh:mm:ss"/><numFmt numFmtId="166" formatCode="hh:mm:ss"/></numFmts><fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts><fills count="1"><fill><patternFill patternType="none"/></fill></fills><borders count="1"><border/></borders><cellXfs count="4"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/><xf numFmtId="165" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/><xf numFmtId="166" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs></styleSheet>`,
		"xl/worksheets/sheet1.xml": xlsxSheetXML(columns, rows, kinds),
	}
	order := []string{
		"[Content_Types].xml",
		"_rels/.rels",
		"xl/workbook.xml",
		"xl/_rels/workbook.xml.rels",
		"xl/styles.xml",
		"xl/worksheets/sheet1.xml",
	}
	for _, name := range order {
		fw, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if _, err := fw.Write([]byte(files[name])); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

func xlsxSheetXML(columns []string, rows [][]string, kinds []typed.Kind) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	writeXLSXRow(&b, 1, columns)
	for i, row := range rows {
		writeXLSXTypedRow(&b, i+2, row, kinds, len(columns))
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

func writeXLSXRow(b *strings.Builder, rowNum int, values []string) {
	b.WriteString(`<row r="`)
	b.WriteString(strconv.Itoa(rowNum))
	b.WriteString(`">`)
	for i, value := range values {
		b.WriteString(`<c r="`)
		b.WriteString(xlsxCellRef(i+1, rowNum))
		b.WriteString(`" t="inlineStr"><is><t>`)
		b.WriteString(xmlText(value))
		b.WriteString(`</t></is></c>`)
	}
	b.WriteString(`</row>`)
}

func writeXLSXTypedRow(b *strings.Builder, rowNum int, values []string, kinds []typed.Kind, columnCount int) {
	b.WriteString(`<row r="`)
	b.WriteString(strconv.Itoa(rowNum))
	b.WriteString(`">`)
	for i := 0; i < columnCount; i++ {
		value := ""
		if i < len(values) {
			value = values[i]
		}
		kind := typed.KindText
		if i < len(kinds) {
			kind = kinds[i]
		}
		writeXLSXTypedCell(b, i+1, rowNum, value, kind)
	}
	b.WriteString(`</row>`)
}

func writeXLSXTypedCell(b *strings.Builder, col, rowNum int, value string, kind typed.Kind) {
	ref := xlsxCellRef(col, rowNum)
	classified := typed.Classify(value)
	if classified.Kind == typed.KindBlank {
		b.WriteString(`<c r="`)
		b.WriteString(ref)
		b.WriteString(`"/>`)
		return
	}
	switch kind {
	case typed.KindInt:
		if classified.Kind == typed.KindInt {
			writeXLSXNumericCell(b, ref, strconv.FormatInt(classified.Int, 10), "")
			return
		}
	case typed.KindFloat:
		if classified.Kind == typed.KindFloat || classified.Kind == typed.KindInt {
			writeXLSXNumericCell(b, ref, strconv.FormatFloat(classified.Float, 'g', -1, 64), "")
			return
		}
	case typed.KindBool:
		if classified.Kind == typed.KindBool {
			b.WriteString(`<c r="`)
			b.WriteString(ref)
			b.WriteString(`" t="b"><v>`)
			if classified.Bool {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
			b.WriteString(`</v></c>`)
			return
		}
	case typed.KindDate, typed.KindDateTime, typed.KindTime:
		if classified.Kind == kind {
			if serial, ok := typed.ExcelSerial(classified, kind); ok {
				style := "1"
				if kind == typed.KindDateTime {
					style = "2"
				} else if kind == typed.KindTime {
					style = "3"
				}
				writeXLSXNumericCell(b, ref, strconv.FormatFloat(serial, 'f', -1, 64), style)
				return
			}
		}
	}
	writeXLSXTextCell(b, ref, value)
}

func writeXLSXNumericCell(b *strings.Builder, ref, value, style string) {
	b.WriteString(`<c r="`)
	b.WriteString(ref)
	if style != "" {
		b.WriteString(`" s="`)
		b.WriteString(style)
	}
	b.WriteString(`"><v>`)
	b.WriteString(value)
	b.WriteString(`</v></c>`)
}

func writeXLSXTextCell(b *strings.Builder, ref, value string) {
	b.WriteString(`<c r="`)
	b.WriteString(ref)
	b.WriteString(`" t="inlineStr"><is><t>`)
	b.WriteString(xmlText(value))
	b.WriteString(`</t></is></c>`)
}

func xlsxCellRef(col, row int) string {
	var letters []byte
	for col > 0 {
		col--
		letters = append([]byte{byte('A' + col%26)}, letters...)
		col /= 26
	}
	return string(letters) + strconv.Itoa(row)
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

type xlsxSharedStrings struct {
	Items []xlsxSharedString `xml:"si"`
}

type xlsxSharedString struct {
	TextParts []string `xml:"t"`
}

type xlsxWorksheet struct {
	Rows []xlsxRow `xml:"sheetData>row"`
}

type xlsxRow struct {
	Cells []xlsxCell `xml:"c"`
}

type xlsxCell struct {
	Ref         string `xml:"r,attr"`
	Type        string `xml:"t,attr"`
	Value       string `xml:"v"`
	InlineValue string `xml:"is>t"`
}

func xlsxToCSV(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}
	shared, err := readXLSXSharedStrings(files["xl/sharedStrings.xml"])
	if err != nil {
		return nil, err
	}
	var sheetNames []string
	for name := range files {
		if strings.HasPrefix(name, "xl/worksheets/sheet") && strings.HasSuffix(name, ".xml") {
			sheetNames = append(sheetNames, name)
		}
	}
	sort.Slice(sheetNames, func(i, j int) bool {
		return xlsxSheetNumber(sheetNames[i]) < xlsxSheetNumber(sheetNames[j])
	})
	if len(sheetNames) == 0 {
		return nil, fmt.Errorf("no xl/worksheets/sheet*.xml files found")
	}
	if len(sheetNames) == 1 {
		return xlsxRowsToCSV(readXLSXSheetRows(files[sheetNames[0]], shared))
	}

	var allRows [][]string
	var header []string
	maxCols := 0
	for _, name := range sheetNames {
		rows, err := readXLSXSheetRows(files[name], shared)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			continue
		}
		if len(header) == 0 {
			header = append([]string{"sheet"}, rows[0]...)
			maxCols = len(header)
		}
		sheetLabel := strings.TrimSuffix(strings.TrimPrefix(name, "xl/worksheets/"), ".xml")
		for _, row := range rows[1:] {
			record := append([]string{sheetLabel}, row...)
			if len(record) > maxCols {
				maxCols = len(record)
			}
			allRows = append(allRows, record)
		}
	}
	if len(header) == 0 {
		return nil, fmt.Errorf("xlsx contains no worksheet rows")
	}
	for len(header) < maxCols {
		header = append(header, fmt.Sprintf("col_%d", len(header)))
	}
	return xlsxRowsToCSV(append([][]string{header}, allRows...), nil)
}

func readXLSXSheetRows(sheet *zip.File, shared []string) ([][]string, error) {
	if sheet == nil {
		return nil, fmt.Errorf("worksheet not found")
	}
	rc, err := sheet.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var ws xlsxWorksheet
	if err := xml.NewDecoder(rc).Decode(&ws); err != nil {
		return nil, err
	}
	var rows [][]string
	maxCols := 0
	for _, row := range ws.Rows {
		out := make([]string, 0, len(row.Cells))
		for _, cell := range row.Cells {
			colIdx := xlsxColumnIndex(cell.Ref)
			for len(out) < colIdx {
				out = append(out, "")
			}
			out = append(out, xlsxCellValue(cell, shared))
		}
		if len(out) > maxCols {
			maxCols = len(out)
		}
		rows = append(rows, out)
	}
	return rows, nil
}

func xlsxRowsToCSV(rows [][]string, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}
	for _, row := range rows {
		for len(row) < maxCols {
			row = append(row, "")
		}
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func xlsxSheetNumber(name string) int {
	base := strings.TrimSuffix(strings.TrimPrefix(name, "xl/worksheets/sheet"), ".xml")
	n, err := strconv.Atoi(base)
	if err != nil {
		return 0
	}
	return n
}

func readXLSXSharedStrings(f *zip.File) ([]string, error) {
	if f == nil {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var ss xlsxSharedStrings
	if err := xml.NewDecoder(rc).Decode(&ss); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ss.Items))
	for _, item := range ss.Items {
		out = append(out, strings.Join(item.TextParts, ""))
	}
	return out, nil
}

func xlsxCellValue(cell xlsxCell, shared []string) string {
	switch cell.Type {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(cell.Value))
		if err == nil && i >= 0 && i < len(shared) {
			return shared[i]
		}
		return ""
	case "inlineStr":
		return cell.InlineValue
	default:
		return cell.Value
	}
}

func xlsxColumnIndex(ref string) int {
	if ref == "" {
		return 0
	}
	idx := 0
	for _, r := range ref {
		if r >= 'A' && r <= 'Z' {
			idx = idx*26 + int(r-'A'+1)
		} else if r >= 'a' && r <= 'z' {
			idx = idx*26 + int(r-'a'+1)
		} else {
			break
		}
	}
	if idx <= 0 {
		return 0
	}
	return idx - 1
}
