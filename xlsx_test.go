package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SimonWaldherr/datadock/internal/typed"
)

func TestWriteXLSXStreamedWorksheet(t *testing.T) {
	columns := []string{"id", "text & label", "active", "score", "missing"}
	longText := strings.Repeat("<tag> & München", 1000)
	rows := [][]string{{"123", longText, "true", "1.5"}, {"456", "00123", "false", "2"}}
	w := httptest.NewRecorder()
	if err := writeXLSX(w, columns, rows); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 6 {
		t.Fatalf("got %d archive entries, want 6", len(zr.File))
	}
	f, err := zr.Open("xl/worksheets/sheet1.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	var sheet xlsxWorksheet
	if err := xml.Unmarshal(data, &sheet); err != nil {
		t.Fatal(err)
	}
	if len(sheet.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(sheet.Rows))
	}
	want := [][]xlsxCell{
		{{Ref: "A1", Type: "inlineStr", InlineValue: "id"}, {Ref: "B1", Type: "inlineStr", InlineValue: "text & label"}, {Ref: "C1", Type: "inlineStr", InlineValue: "active"}, {Ref: "D1", Type: "inlineStr", InlineValue: "score"}, {Ref: "E1", Type: "inlineStr", InlineValue: "missing"}},
		{{Ref: "A2", Value: "123"}, {Ref: "B2", Type: "inlineStr", InlineValue: longText}, {Ref: "C2", Type: "b", Value: "1"}, {Ref: "D2", Value: "1.5"}, {Ref: "E2"}},
		{{Ref: "A3", Value: "456"}, {Ref: "B3", Type: "inlineStr", InlineValue: "00123"}, {Ref: "C3", Type: "b", Value: "0"}, {Ref: "D3", Value: "2"}, {Ref: "E3"}},
	}
	for i, row := range sheet.Rows {
		if len(row.Cells) != len(want[i]) {
			t.Fatalf("row %d has %d cells", i, len(row.Cells))
		}
		for j, cell := range row.Cells {
			if cell != want[i][j] {
				t.Errorf("unexpected cell at row %d, column %d", i, j)
			}
		}
	}
}

type failingXLSXWriter struct{ err error }

func (w failingXLSXWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteXLSXSheetPropagatesWriteErrors(t *testing.T) {
	want := errors.New("destination unavailable")
	for _, size := range []int{1, 10000} {
		rows := [][]string{{strings.Repeat("x", size)}}
		err := writeXLSXSheetXML(failingXLSXWriter{want}, []string{"text"}, rows, []typed.Kind{typed.KindText})
		if !errors.Is(err, want) {
			t.Fatalf("text size %d: got %v, want %v", size, err, want)
		}
	}
}
