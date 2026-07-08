package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
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

func TestImportOSMPBFImportsTaggedWaysWithGeometry(t *testing.T) {
	db := tinysql.NewDB()
	data := createTestOSMPBF()
	res, err := ImportOSMPBF(context.Background(), db, "default", "pbf_import", bytes.NewReader(data), &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportOSMPBF: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT record_type, osm_id, geometry_type, name, highway FROM pbf_import")
	got := rowStrings(rows)[0]
	if got[0] != "way" || got[1] != "10" || got[2] != "LineString" || got[3] != "Main Street" || got[4] != "residential" {
		t.Fatalf("unexpected PBF row: %#v", got)
	}
}

func TestImportGPXImportsWaypointsAndTracks(t *testing.T) {
	db := tinysql.NewDB()
	body := strings.NewReader(`<gpx version="1.1" creator="DataDock">
<wpt lat="48.1372" lon="11.5761"><name>Munich</name><ele>519</ele></wpt>
<trk><name>Walk</name><trkseg><trkpt lat="48.1" lon="11.5"/><trkpt lat="48.2" lon="11.6"/></trkseg></trk>
</gpx>`)
	res, err := ImportGPX(context.Background(), db, "default", "gpx_import", body, &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportGPX: %v", err)
	}
	if res.RowsInserted != 2 {
		t.Fatalf("RowsInserted = %d, want 2", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT record_type, name, geometry_type FROM gpx_import")
	got := rowStrings(rows)
	if got[0][0] != "waypoint" || got[0][1] != "Munich" || got[0][2] != "Point" {
		t.Fatalf("unexpected waypoint row: %#v", got[0])
	}
	if got[1][0] != "track" || got[1][1] != "Walk" || got[1][2] != "LineString" {
		t.Fatalf("unexpected track row: %#v", got[1])
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

func TestImportGeoPackageImportsFeatureLayers(t *testing.T) {
	db := tinysql.NewDB()
	path := createTestGeoPackage(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read geopackage: %v", err)
	}
	res, err := ImportGeoPackage(context.Background(), db, "default", "gpkg_import", bytes.NewReader(data), &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportGeoPackage: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT layer, feature_id, geometry_type, properties FROM gpkg_import")
	got := rowStrings(rows)[0]
	if got[0] != "places" || got[1] != "1" || got[2] != "Point" || !strings.Contains(got[3], "Munich") {
		t.Fatalf("unexpected GeoPackage row: %#v", got)
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

func TestImportSQLiteCombinesTables(t *testing.T) {
	db := tinysql.NewDB()
	path := createTestSQLite(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sqlite: %v", err)
	}
	res, err := ImportSQLite(context.Background(), db, "default", "sqlite_import", bytes.NewReader(data), &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportSQLite: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT source_table, name, age FROM sqlite_import")
	got := rowStrings(rows)[0]
	if got[0] != "people" || got[1] != "Ada" || got[2] != "37" {
		t.Fatalf("unexpected sqlite row: %#v", got)
	}
}

func TestImportHTMLTables(t *testing.T) {
	db := tinysql.NewDB()
	body := strings.NewReader(`<html><body><table><tr><th>Name</th><th>Age</th></tr><tr><td>Ada</td><td>37</td></tr></table></body></html>`)
	res, err := ImportHTMLTables(context.Background(), db, "default", "html_import", body, &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportHTMLTables: %v", err)
	}
	if res.RowsInserted != 2 {
		t.Fatalf("RowsInserted = %d, want 2", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT table_index, row_index, col_1, col_2 FROM html_import")
	got := rowStrings(rows)
	if got[1][0] != "1" || got[1][1] != "2" || got[1][2] != "Ada" || got[1][3] != "37" {
		t.Fatalf("unexpected html row: %#v", got[1])
	}
}

func TestImportCompactBinaryFormats(t *testing.T) {
	tests := []struct {
		name       string
		importFunc func(context.Context, *tinysql.DB, string, string, io.Reader, *ImportOptions) (*ImportResult, error)
		body       []byte
	}{
		{name: "msgpack", importFunc: ImportMessagePack, body: []byte{0x91, 0x82, 0xa4, 'n', 'a', 'm', 'e', 0xa3, 'A', 'd', 'a', 0xa3, 'a', 'g', 'e', 37}},
		{name: "cbor", importFunc: ImportCBOR, body: []byte{0x81, 0xa2, 0x64, 'n', 'a', 'm', 'e', 0x63, 'A', 'd', 'a', 0x63, 'a', 'g', 'e', 0x18, 37}},
		{name: "bson", importFunc: ImportBSON, body: bsonDoc(map[string]any{"name": "Ada", "age": int32(37)})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := tinysql.NewDB()
			res, err := tt.importFunc(context.Background(), db, "default", tt.name+"_import", bytes.NewReader(tt.body), &ImportOptions{
				CreateTable:   true,
				TypeInference: true,
			})
			if err != nil {
				t.Fatalf("%s import: %v", tt.name, err)
			}
			if res.RowsInserted != 1 {
				t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
			}
			rows := selectRows(t, db, "SELECT name, age FROM "+quoteIdent(tt.name+"_import"))
			got := rowStrings(rows)[0]
			if got[0] != "Ada" || got[1] != "37" {
				t.Fatalf("unexpected %s row: %#v", tt.name, got)
			}
		})
	}
}

func TestImportICalendarAndVCard(t *testing.T) {
	db := tinysql.NewDB()
	ics := strings.NewReader("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:1\r\nSUMMARY:Launch\r\nDTSTART:20260708T100000Z\r\nLOCATION:Berlin\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	if _, err := ImportICalendar(context.Background(), db, "default", "ics_import", ics, &ImportOptions{CreateTable: true, TypeInference: true}); err != nil {
		t.Fatalf("ImportICalendar: %v", err)
	}
	rows := selectRows(t, db, "SELECT component, summary, start, location FROM ics_import")
	got := rowStrings(rows)[0]
	if got[0] != "VEVENT" || got[1] != "Launch" || got[2] != "20260708T100000Z" || got[3] != "Berlin" {
		t.Fatalf("unexpected ics row: %#v", got)
	}

	vcf := strings.NewReader("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Ada Lovelace\r\nEMAIL:ada@example.test\r\nTEL:+491234\r\nEND:VCARD\r\n")
	if _, err := ImportVCard(context.Background(), db, "default", "vcf_import", vcf, &ImportOptions{CreateTable: true, TypeInference: true}); err != nil {
		t.Fatalf("ImportVCard: %v", err)
	}
	rows = selectRows(t, db, "SELECT full_name, email, tel FROM vcf_import")
	got = rowStrings(rows)[0]
	if got[0] != "Ada Lovelace" || got[1] != "ada@example.test" || got[2] != "+491234" {
		t.Fatalf("unexpected vcard row: %#v", got)
	}
}

func TestImportColumnarManifest(t *testing.T) {
	db := tinysql.NewDB()
	res, err := ImportColumnarManifest(context.Background(), db, "default", "parquet_import", "parquet", strings.NewReader("PAR1payloadPAR1"), &ImportOptions{
		CreateTable:   true,
		TypeInference: true,
	})
	if err != nil {
		t.Fatalf("ImportColumnarManifest: %v", err)
	}
	if res.RowsInserted != 1 {
		t.Fatalf("RowsInserted = %d, want 1", res.RowsInserted)
	}
	rows := selectRows(t, db, "SELECT file_format, size_bytes FROM parquet_import")
	got := rowStrings(rows)[0]
	if got[0] != "parquet" || got[1] != "15" {
		t.Fatalf("unexpected manifest row: %#v", got)
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

func createTestSQLite(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/test.sqlite"
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqlDB.Close()
	stmts := []string{
		"CREATE TABLE people (name TEXT, age INTEGER)",
		"INSERT INTO people (name, age) VALUES ('Ada', 37)",
	}
	for _, stmt := range stmts {
		if _, err := sqlDB.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	return path
}

func bsonDoc(values map[string]any) []byte {
	var body bytes.Buffer
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch v := values[key].(type) {
		case string:
			body.WriteByte(0x02)
			body.WriteString(key)
			body.WriteByte(0)
			var size [4]byte
			binary.LittleEndian.PutUint32(size[:], uint32(len(v)+1))
			body.Write(size[:])
			body.WriteString(v)
			body.WriteByte(0)
		case int32:
			body.WriteByte(0x10)
			body.WriteString(key)
			body.WriteByte(0)
			var value [4]byte
			binary.LittleEndian.PutUint32(value[:], uint32(v))
			body.Write(value[:])
		}
	}
	body.WriteByte(0)
	var out bytes.Buffer
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(body.Len()+4))
	out.Write(size[:])
	out.Write(body.Bytes())
	return out.Bytes()
}

func createTestGeoPackage(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/test.gpkg"
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqlDB.Close()
	stmts := []string{
		"CREATE TABLE gpkg_contents (table_name TEXT, data_type TEXT, identifier TEXT)",
		"CREATE TABLE gpkg_geometry_columns (table_name TEXT, column_name TEXT, geometry_type_name TEXT, srs_id INTEGER, z TINYINT, m TINYINT)",
		"CREATE TABLE places (fid INTEGER PRIMARY KEY, geom BLOB, name TEXT)",
		"INSERT INTO gpkg_contents (table_name, data_type, identifier) VALUES ('places', 'features', 'places')",
		"INSERT INTO gpkg_geometry_columns (table_name, column_name, geometry_type_name, srs_id, z, m) VALUES ('places', 'geom', 'POINT', 4326, 0, 0)",
	}
	for _, stmt := range stmts {
		if _, err := sqlDB.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if _, err := sqlDB.Exec("INSERT INTO places (fid, geom, name) VALUES (?, ?, ?)", 1, geoPackagePointBlob(11.5761, 48.1372), "Munich"); err != nil {
		t.Fatalf("insert feature: %v", err)
	}
	return path
}

func geoPackagePointBlob(x, y float64) []byte {
	header := []byte{'G', 'P', 0, 1, 0, 0, 0, 0}
	return append(header, wkbPoint(x, y)...)
}

func wkbPoint(x, y float64) []byte {
	var buf bytes.Buffer
	buf.WriteByte(1)
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], 1)
	buf.Write(b4[:])
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], math.Float64bits(x))
	buf.Write(b8[:])
	binary.LittleEndian.PutUint64(b8[:], math.Float64bits(y))
	buf.Write(b8[:])
	return buf.Bytes()
}

func createTestOSMPBF() []byte {
	stringsTable := protoBytes(1, []byte(""))
	for _, value := range []string{"name", "highway", "Main Street", "residential"} {
		stringsTable = append(stringsTable, protoBytes(1, []byte(value))...)
	}
	dense := protoBytes(1, packedSVarintsForTest(1, 1))
	dense = append(dense, protoBytes(8, packedSVarintsForTest(520000000, 1000000))...)
	dense = append(dense, protoBytes(9, packedSVarintsForTest(130000000, 1000000))...)
	dense = append(dense, protoBytes(10, packedVarintsForTest(0, 0))...)
	way := protoVarint(1, 10)
	way = append(way, protoBytes(2, packedVarintsForTest(1, 2))...)
	way = append(way, protoBytes(3, packedVarintsForTest(3, 4))...)
	way = append(way, protoBytes(8, packedSVarintsForTest(1, 1))...)
	group := protoBytes(2, dense)
	group = append(group, protoBytes(3, way)...)
	block := protoBytes(1, stringsTable)
	block = append(block, protoBytes(2, group)...)
	blob := protoBytes(1, block)
	header := protoBytes(1, []byte("OSMData"))
	header = append(header, protoVarint(3, uint64(len(blob)))...)
	var out bytes.Buffer
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(header)))
	out.Write(size[:])
	out.Write(header)
	out.Write(blob)
	return out.Bytes()
}

func protoVarint(field int, value uint64) []byte {
	var out []byte
	out = append(out, encodeUvarint(uint64(field<<3))...)
	out = append(out, encodeUvarint(value)...)
	return out
}

func protoBytes(field int, value []byte) []byte {
	var out []byte
	out = append(out, encodeUvarint(uint64(field<<3|2))...)
	out = append(out, encodeUvarint(uint64(len(value)))...)
	out = append(out, value...)
	return out
}

func packedVarintsForTest(values ...uint64) []byte {
	var out []byte
	for _, value := range values {
		out = append(out, encodeUvarint(value)...)
	}
	return out
}

func packedSVarintsForTest(values ...int64) []byte {
	var out []byte
	for _, value := range values {
		out = append(out, encodeUvarint(encodeZigZag(value))...)
	}
	return out
}

func encodeUvarint(value uint64) []byte {
	var out []byte
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func encodeZigZag(value int64) uint64 {
	return uint64(value<<1) ^ uint64(value>>63)
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
