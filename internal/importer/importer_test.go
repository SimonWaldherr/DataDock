package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tinysql "github.com/SimonWaldherr/tinySQL"
	"github.com/jonas-p/go-shp"
	_ "modernc.org/sqlite"
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

func TestImportKMLCreatesGeoJSONGeometry(t *testing.T) {
	db := tinysql.NewDB()
	body := strings.NewReader(`<?xml version="1.0" encoding="UTF-8"?>
<kml><Document><Placemark><name>Brandenburg Gate</name><ExtendedData><Data name="category"><value>landmark</value></Data></ExtendedData><Point><coordinates>13.3777,52.5163,34</coordinates></Point></Placemark></Document></kml>`)
	res, err := ImportKML(context.Background(), db, "default", "kml_import", body, &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportKML: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
	}
	for _, want := range []string{"geometry", "geometry_type", "name", "category"} {
		if !containsColumn(res.ColumnNames, want) {
			t.Fatalf("expected column %q in %#v", want, res.ColumnNames)
		}
	}
	rows := selectRows(t, db, "SELECT geometry_type, name, category FROM kml_import")
	if got := rowStrings(rows)[0]; got[0] != "Point" || got[1] != "Brandenburg Gate" || got[2] != "landmark" {
		t.Fatalf("unexpected KML row: %#v", got)
	}
}

func TestImportOSMImportsTaggedWaysWithGeometry(t *testing.T) {
	db := tinysql.NewDB()
	body := strings.NewReader(`<osm version="0.6">
<node id="1" lat="52.0" lon="13.0"/>
<node id="2" lat="52.1" lon="13.1"/>
<way id="10"><nd ref="1"/><nd ref="2"/><tag k="highway" v="residential"/><tag k="name" v="Main Street"/></way>
</osm>`)
	res, err := ImportOSM(context.Background(), db, "default", "osm_import", body, &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportOSM: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT record_type, osm_id, geometry_type, name, highway FROM osm_import")
	got := rowStrings(rows)[0]
	if got[0] != "way" || got[1] != "10" || got[2] != "LineString" || got[3] != "Main Street" || got[4] != "residential" {
		t.Fatalf("unexpected OSM row: %#v", got)
	}
}

func TestImportRoutingGraphImportsNodesAndEdges(t *testing.T) {
	db := tinysql.NewDB()
	body := strings.NewReader(`{"nodes":[{"id":"a","lat":52.0,"lon":13.0}],"edges":[{"id":"e1","from":"a","to":"b","distance":123.4,"geometry":{"type":"LineString","coordinates":[[13,52],[13.1,52.1]]}}]}`)
	res, err := ImportRoutingGraph(context.Background(), db, "default", "rg_import", body, &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportRoutingGraph: %v", err)
	}
	if res.RowsInserted != 2 {
		t.Fatalf("RowsInserted = %d, want 2", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT record_type, id, from_id, to_id, geometry_type FROM rg_import")
	got := rowStrings(rows)
	if got[0][0] != "node" || got[0][1] != "a" || got[0][4] != "Point" {
		t.Fatalf("unexpected node row: %#v", got[0])
	}
	if got[1][0] != "edge" || got[1][1] != "e1" || got[1][2] != "a" || got[1][3] != "b" || got[1][4] != "LineString" {
		t.Fatalf("unexpected edge row: %#v", got[1])
	}
}

func TestImportShapefileZipCreatesGeoJSONGeometry(t *testing.T) {
	db := tinysql.NewDB()
	path := createTestShapefileZip(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shapefile zip: %v", err)
	}
	res, err := ImportShapefileZip(context.Background(), db, "default", "shape_import", bytes.NewReader(data), &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportShapefileZip: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT geometry_type, shape_type, name FROM shape_import")
	got := rowStrings(rows)[0]
	if got[0] != "Point" || got[1] != "Point" || got[2] != "Munich" {
		t.Fatalf("unexpected shapefile row: %#v", got)
	}
}

func TestImportMBTilesIndexesMetadataAndTiles(t *testing.T) {
	db := tinysql.NewDB()
	path := createTestMBTiles(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mbtiles: %v", err)
	}
	res, err := ImportMBTiles(context.Background(), db, "default", "mbtiles_import", bytes.NewReader(data), &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportMBTiles: %v", err)
	}
	if res.RowsInserted != 2 {
		t.Fatalf("RowsInserted = %d, want 2", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT record_type, name, value, zoom_level, tile_size FROM mbtiles_import")
	got := rowStrings(rows)
	if got[0][0] != "metadata" || got[0][1] != "name" || got[0][2] != "Test Tiles" {
		t.Fatalf("unexpected metadata row: %#v", got[0])
	}
	if got[1][0] != "tile" || got[1][3] != "0" || got[1][4] != "4" {
		t.Fatalf("unexpected tile row: %#v", got[1])
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

func selectRows(t *testing.T, db *tinysql.DB, query string) *tinysql.ResultSet {
	t.Helper()
	rows, err := exec(context.Background(), db, "default", query)
	if err != nil {
		t.Fatalf("select %q: %v", query, err)
	}
	return rows
}

func rowStrings(rs *tinysql.ResultSet) [][]string {
	out := make([][]string, len(rs.Rows))
	for i, row := range rs.Rows {
		out[i] = make([]string, len(rs.Cols))
		for j, col := range rs.Cols {
			out[i][j] = strings.TrimSpace(fmt.Sprint(row[col]))
		}
	}
	return out
}

func createTestMBTiles(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/test.mbtiles"
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqlDB.Close()
	stmts := []string{
		"CREATE TABLE metadata (name TEXT, value TEXT)",
		"CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)",
		"INSERT INTO metadata (name, value) VALUES ('name', 'Test Tiles')",
		"INSERT INTO tiles (zoom_level, tile_column, tile_row, tile_data) VALUES (0, 0, 0, x'01020304')",
	}
	for _, stmt := range stmts {
		if _, err := sqlDB.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	return path
}

func createTestShapefileZip(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "places")
	writer, err := shp.Create(base+".shp", shp.POINT)
	if err != nil {
		t.Fatalf("create shapefile: %v", err)
	}
	if err := writer.SetFields([]shp.Field{shp.StringField("name", 32)}); err != nil {
		t.Fatalf("set fields: %v", err)
	}
	row := writer.Write(&shp.Point{X: 11.5761, Y: 48.1372})
	if err := writer.WriteAttribute(int(row), 0, "Munich"); err != nil {
		t.Fatalf("write attribute: %v", err)
	}
	writer.Close()
	if err := os.Rename(base+"dbf", base+".dbf"); err != nil {
		t.Fatalf("rename dbf: %v", err)
	}

	zipPath := filepath.Join(dir, "places.zip")
	zipFile, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(zipFile)
	for _, ext := range []string{".shp", ".shx", ".dbf"} {
		name := "places" + ext
		body, err := os.ReadFile(base + ext)
		if err != nil {
			t.Fatalf("read %s: %v", ext, err)
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	if err := zipFile.Close(); err != nil {
		t.Fatalf("close zip file: %v", err)
	}
	return zipPath
}
