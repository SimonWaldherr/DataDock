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
