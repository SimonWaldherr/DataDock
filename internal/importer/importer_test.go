package importer

import (
	"context"
	"strings"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

func TestImportCSVInfersColumnTypes(t *testing.T) {
	db := tinysql.NewDB()
	csvData := strings.NewReader("id,amount,active,created,code\n1,12.5,true,2026-07-05T21:25:49Z,00123\n2,13.0,false,2026-07-06T00:00:00Z,00456\n")
	res, err := ImportCSV(context.Background(), db, "default", "typed_import", csvData, &ImportOptions{
		CreateTable:   true,
		HeaderMode:    "present",
		TypeInference: true,
		StrictTypes:   true,
	})
	if err != nil {
		t.Fatalf("ImportCSV: %v", err)
	}
	if res.RowsInserted != 2 {
		t.Fatalf("RowsInserted = %d, want 2", res.RowsInserted)
	}
	want := []tinysql.ColType{
		tinysql.IntType,
		tinysql.FloatType,
		tinysql.BoolType,
		tinysql.DateTimeType,
		tinysql.TextType,
	}
	if len(res.ColumnTypes) != len(want) {
		t.Fatalf("ColumnTypes len = %d, want %d", len(res.ColumnTypes), len(want))
	}
	for i := range want {
		if res.ColumnTypes[i] != want[i] {
			t.Fatalf("ColumnTypes[%d] = %v, want %v", i, res.ColumnTypes[i], want[i])
		}
	}
}

func TestImportGeoJSONCreatesGeometryAndPropertyColumns(t *testing.T) {
	db := tinysql.NewDB()
	body := strings.NewReader(`{"type":"FeatureCollection","features":[{"type":"Feature","geometry":{"type":"Point","coordinates":[11.5761,48.1372]},"properties":{"name":"Munich","population":1500000}}]}`)
	res, err := ImportGeoJSON(context.Background(), db, "default", "geo_import", body, &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportGeoJSON: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
	}
	for _, want := range []string{"geometry", "geometry_type", "name", "population"} {
		if !containsColumn(res.ColumnNames, want) {
			t.Fatalf("expected column %q in %#v", want, res.ColumnNames)
		}
	}
	if res.ColumnTypes[len(res.ColumnTypes)-1] != tinysql.IntType {
		t.Fatalf("population type = %v, want %v", res.ColumnTypes[len(res.ColumnTypes)-1], tinysql.IntType)
	}
}

func containsColumn(columns []string, want string) bool {
	for _, col := range columns {
		if col == want {
			return true
		}
	}
	return false
}
