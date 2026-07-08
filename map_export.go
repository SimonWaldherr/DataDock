package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/SimonWaldherr/datadock/internal/standards"
	"github.com/SimonWaldherr/datadock/internal/typed"
	"github.com/jonas-p/go-shp"
)

type exportOptions struct {
	Explode           bool
	SimplifyTolerance float64
	BBox              *geoBBox
	Fields            []string
	DropFields        []string
}

type geoBBox struct {
	MinX float64
	MinY float64
	MaxX float64
	MaxY float64
}

type geoJSONSummary struct {
	Type          string         `json:"type"`
	Features      int            `json:"features"`
	GeometryTypes map[string]int `json:"geometry_types"`
	BBox          []float64      `json:"bbox,omitempty"`
	Properties    []string       `json:"properties"`
}

func exportOptionsFromValues(values url.Values) (exportOptions, error) {
	var opts exportOptions
	opts.Explode = parseBoolish(values.Get("explode"))
	if raw := strings.TrimSpace(values.Get("simplify")); raw != "" {
		tolerance, err := strconv.ParseFloat(raw, 64)
		if err != nil || tolerance < 0 {
			return opts, fmt.Errorf("simplify must be a non-negative number")
		}
		opts.SimplifyTolerance = tolerance
	}
	if raw := strings.TrimSpace(values.Get("bbox")); raw != "" {
		bbox, err := parseGeoBBox(raw)
		if err != nil {
			return opts, err
		}
		opts.BBox = &bbox
	}
	opts.Fields = splitCSVOption(values.Get("fields"))
	opts.DropFields = splitCSVOption(values.Get("drop"))
	return opts, nil
}

func parseBoolish(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func splitCSVOption(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		name := strings.TrimSpace(part)
		key := strings.ToLower(name)
		if name != "" && !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	return out
}

func parseGeoBBox(raw string) (geoBBox, error) {
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return geoBBox{}, fmt.Errorf("bbox must be minx,miny,maxx,maxy")
	}
	vals := make([]float64, 4)
	for i, part := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return geoBBox{}, fmt.Errorf("bbox must contain numeric coordinates")
		}
		vals[i] = v
	}
	if vals[0] > vals[2] || vals[1] > vals[3] {
		return geoBBox{}, fmt.Errorf("bbox min values must be <= max values")
	}
	return geoBBox{MinX: vals[0], MinY: vals[1], MaxX: vals[2], MaxY: vals[3]}, nil
}

func applyGeoJSONExportOptions(fc geoJSONFeatureCollection, opts exportOptions) geoJSONFeatureCollection {
	if opts.Explode {
		fc = explodeGeoJSONFeatureCollection(fc)
	}
	if opts.SimplifyTolerance > 0 {
		fc = simplifyGeoJSONFeatureCollection(fc, opts.SimplifyTolerance)
	}
	if opts.BBox != nil {
		fc = filterGeoJSONFeatureCollectionByBBox(fc, *opts.BBox)
	}
	if len(opts.Fields) > 0 || len(opts.DropFields) > 0 {
		fc = filterGeoJSONProperties(fc, opts.Fields, opts.DropFields)
	}
	return fc
}

func writeGeoJSONExportWithOptions(w http.ResponseWriter, columns []string, rows [][]string, kinds []typed.Kind, filenameBase string, opts exportOptions) error {
	w.Header().Set("Content-Type", standards.MediaTypeGeoJSON)
	w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "geojson"))
	fc := applyGeoJSONExportOptions(buildGeoJSONFeatureCollection(columns, rows, kinds), opts)
	return json.NewEncoder(w).Encode(fc)
}

func writeGeoJSONSummaryExport(w http.ResponseWriter, columns []string, rows [][]string, kinds []typed.Kind, filenameBase string, opts exportOptions) error {
	w.Header().Set("Content-Type", standards.MediaTypeJSON)
	w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "geojson-summary.json"))
	fc := applyGeoJSONExportOptions(buildGeoJSONFeatureCollection(columns, rows, kinds), opts)
	return json.NewEncoder(w).Encode(summarizeGeoJSONFeatureCollection(fc))
}

func writeKMLExport(w http.ResponseWriter, columns []string, rows [][]string, kinds []typed.Kind, filenameBase string, opts exportOptions) error {
	w.Header().Set("Content-Type", "application/vnd.google-earth.kml+xml; charset=utf-8")
	w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "kml"))
	fc := applyGeoJSONExportOptions(buildGeoJSONFeatureCollection(columns, rows, kinds), opts)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(kmlDocumentFromFeatures(fc))
}

func writeGPXExport(w http.ResponseWriter, columns []string, rows [][]string, kinds []typed.Kind, filenameBase string, opts exportOptions) error {
	w.Header().Set("Content-Type", "application/gpx+xml; charset=utf-8")
	w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "gpx"))
	fc := applyGeoJSONExportOptions(buildGeoJSONFeatureCollection(columns, rows, kinds), opts)
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(gpxDocumentFromFeatures(fc))
}

func writeHTMLExport(w http.ResponseWriter, columns []string, rows [][]string, filenameBase string) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "html"))
	_, err := fmt.Fprint(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>",
		html.EscapeString(filenameBase), "</title></head><body><table><thead><tr>")
	if err != nil {
		return err
	}
	for _, col := range columns {
		if _, err := fmt.Fprint(w, "<th>", html.EscapeString(col), "</th>"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "</tr></thead><tbody>"); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := fmt.Fprint(w, "<tr>"); err != nil {
			return err
		}
		for i := range columns {
			value := ""
			if i < len(row) {
				value = row[i]
			}
			if _, err := fmt.Fprint(w, "<td>", html.EscapeString(value), "</td>"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprint(w, "</tr>"); err != nil {
			return err
		}
	}
	_, err = fmt.Fprint(w, "</tbody></table></body></html>")
	return err
}

func writeSQLiteExport(w http.ResponseWriter, columns []string, rows [][]string, filenameBase string) error {
	tmp, err := os.CreateTemp("", "datadock-export-*.sqlite")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	db, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return err
	}
	defer db.Close()

	table := safeSQLiteIdentifier(safeExportFilenameBase(filenameBase))
	columnNames := uniqueSQLiteColumns(columns)
	defs := make([]string, len(columnNames))
	for i, col := range columnNames {
		defs[i] = quoteSQLiteIdentifier(col) + " TEXT"
	}
	if _, err := db.Exec("CREATE TABLE " + quoteSQLiteIdentifier(table) + " (" + strings.Join(defs, ", ") + ")"); err != nil {
		return err
	}
	placeholders := strings.Repeat("?,", len(columnNames))
	placeholders = strings.TrimSuffix(placeholders, ",")
	insertSQL := "INSERT INTO " + quoteSQLiteIdentifier(table) + " (" + quoteSQLiteIdentifiers(columnNames) + ") VALUES (" + placeholders + ")"
	stmt, err := db.Prepare(insertSQL)
	if err != nil {
		return err
	}
	for _, row := range rows {
		values := make([]any, len(columnNames))
		for i := range columnNames {
			if i < len(row) {
				values[i] = row[i]
			} else {
				values[i] = ""
			}
		}
		if _, err := stmt.Exec(values...); err != nil {
			_ = stmt.Close()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	if _, err := db.Exec("VACUUM"); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "sqlite"))
	_, err = w.Write(data)
	return err
}

func writeShapefileZipExport(w http.ResponseWriter, columns []string, rows [][]string, kinds []typed.Kind, filenameBase string, opts exportOptions) error {
	fc := applyGeoJSONExportOptions(buildGeoJSONFeatureCollection(columns, rows, kinds), opts)
	shapeType, geomType := shapefileGeometryType(fc)
	if shapeType == shp.NULL {
		return fmt.Errorf("shapefile export requires point, linestring, or polygon geometries")
	}

	tmpDir, err := os.MkdirTemp("", "datadock-shp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	base := safeExportFilenameBase(filenameBase)
	if base == "" {
		base = "export"
	}
	shpPath := filepath.Join(tmpDir, base+".shp")
	writer, err := shp.Create(shpPath, shapeType)
	if err != nil {
		return err
	}
	fields := shapefileFields(fc)
	if len(fields) > 0 {
		if err := writer.SetFields(fields); err != nil {
			writer.Close()
			return err
		}
	}
	fieldNames := shapefileFieldNames(fc)
	for _, feature := range fc.Features {
		if strings.ToLower(geoJSONGeometryType(feature.Geometry)) != geomType {
			continue
		}
		shape := geoJSONGeometryToShape(feature.Geometry, geomType)
		if shape == nil {
			continue
		}
		row := int(writer.Write(shape))
		for i, name := range fieldNames {
			value := ""
			if feature.Properties != nil {
				value = fmt.Sprint(feature.Properties[name])
			}
			if len(value) > 254 {
				value = value[:254]
			}
			if err := writer.WriteAttribute(row, i, value); err != nil {
				writer.Close()
				return err
			}
		}
	}
	writer.Close()
	if err := normalizeShapefileDBFName(tmpDir, base); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "shp.zip"))
	zipWriter := zip.NewWriter(w)
	for _, ext := range []string{".shp", ".shx", ".dbf"} {
		if err := addFileToZip(zipWriter, filepath.Join(tmpDir, base+ext), base+ext); err != nil {
			_ = zipWriter.Close()
			return err
		}
	}
	return zipWriter.Close()
}

func normalizeShapefileDBFName(tmpDir, base string) error {
	want := filepath.Join(tmpDir, base+".dbf")
	if _, err := os.Stat(want); err == nil {
		return nil
	}
	legacy := filepath.Join(tmpDir, base+"dbf")
	if _, err := os.Stat(legacy); err != nil {
		return err
	}
	return os.Rename(legacy, want)
}

func addFileToZip(zipWriter *zip.Writer, path, name string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := zipWriter.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	return err
}

func summarizeGeoJSONFeatureCollection(fc geoJSONFeatureCollection) geoJSONSummary {
	summary := geoJSONSummary{
		Type:          "FeatureCollectionSummary",
		Features:      len(fc.Features),
		GeometryTypes: map[string]int{},
	}
	props := map[string]bool{}
	var bbox *geoBBox
	for _, feature := range fc.Features {
		typ := geoJSONGeometryType(feature.Geometry)
		if typ == "" {
			typ = "Unknown"
		}
		summary.GeometryTypes[typ]++
		for key := range feature.Properties {
			props[key] = true
		}
		if b, ok := geometryBBox(feature.Geometry); ok {
			if bbox == nil {
				cp := b
				bbox = &cp
			} else {
				bbox.MinX = math.Min(bbox.MinX, b.MinX)
				bbox.MinY = math.Min(bbox.MinY, b.MinY)
				bbox.MaxX = math.Max(bbox.MaxX, b.MaxX)
				bbox.MaxY = math.Max(bbox.MaxY, b.MaxY)
			}
		}
	}
	for key := range props {
		summary.Properties = append(summary.Properties, key)
	}
	sort.Strings(summary.Properties)
	if bbox != nil {
		summary.BBox = []float64{bbox.MinX, bbox.MinY, bbox.MaxX, bbox.MaxY}
	}
	return summary
}

func explodeGeoJSONFeatureCollection(fc geoJSONFeatureCollection) geoJSONFeatureCollection {
	out := geoJSONFeatureCollection{Type: "FeatureCollection"}
	for _, feature := range fc.Features {
		geometries := explodeGeoJSONGeometry(feature.Geometry)
		if len(geometries) == 0 {
			continue
		}
		for _, geometry := range geometries {
			out.Features = append(out.Features, geoJSONFeature{
				Type:       "Feature",
				Geometry:   geometry,
				Properties: cloneProperties(feature.Properties),
			})
		}
	}
	return out
}

func explodeGeoJSONGeometry(geometry map[string]any) []map[string]any {
	switch strings.ToLower(geoJSONGeometryType(geometry)) {
	case "multipoint":
		points := toPointList(geometry["coordinates"])
		out := make([]map[string]any, 0, len(points))
		for _, point := range points {
			out = append(out, map[string]any{"type": "Point", "coordinates": point})
		}
		return out
	case "multilinestring":
		lines := toLineList(geometry["coordinates"])
		out := make([]map[string]any, 0, len(lines))
		for _, line := range lines {
			out = append(out, map[string]any{"type": "LineString", "coordinates": line})
		}
		return out
	case "multipolygon":
		polygons := toPolygonList(geometry["coordinates"])
		out := make([]map[string]any, 0, len(polygons))
		for _, polygon := range polygons {
			out = append(out, map[string]any{"type": "Polygon", "coordinates": polygon})
		}
		return out
	case "geometrycollection":
		var out []map[string]any
		if items, ok := geometry["geometries"].([]any); ok {
			for _, item := range items {
				if child, ok := item.(map[string]any); ok {
					out = append(out, explodeGeoJSONGeometry(child)...)
				}
			}
		}
		return out
	default:
		if isGeoJSONGeometry(geometry) {
			return []map[string]any{cloneGeometry(geometry)}
		}
		return nil
	}
}

func simplifyGeoJSONFeatureCollection(fc geoJSONFeatureCollection, tolerance float64) geoJSONFeatureCollection {
	out := geoJSONFeatureCollection{Type: "FeatureCollection", Features: make([]geoJSONFeature, 0, len(fc.Features))}
	for _, feature := range fc.Features {
		out.Features = append(out.Features, geoJSONFeature{
			Type:       "Feature",
			Geometry:   simplifyGeoJSONGeometry(feature.Geometry, tolerance),
			Properties: cloneProperties(feature.Properties),
		})
	}
	return out
}

func simplifyGeoJSONGeometry(geometry map[string]any, tolerance float64) map[string]any {
	switch strings.ToLower(geoJSONGeometryType(geometry)) {
	case "linestring":
		return map[string]any{"type": "LineString", "coordinates": simplifyLine(toPointList(geometry["coordinates"]), tolerance, 2, false)}
	case "polygon":
		return map[string]any{"type": "Polygon", "coordinates": simplifyPolygon(toLineList(geometry["coordinates"]), tolerance)}
	case "multilinestring":
		lines := toLineList(geometry["coordinates"])
		out := make([][][]float64, 0, len(lines))
		for _, line := range lines {
			out = append(out, simplifyLine(line, tolerance, 2, false))
		}
		return map[string]any{"type": "MultiLineString", "coordinates": out}
	case "multipolygon":
		polygons := toPolygonList(geometry["coordinates"])
		out := make([][][][]float64, 0, len(polygons))
		for _, polygon := range polygons {
			out = append(out, simplifyPolygon(polygon, tolerance))
		}
		return map[string]any{"type": "MultiPolygon", "coordinates": out}
	default:
		return cloneGeometry(geometry)
	}
}

func filterGeoJSONFeatureCollectionByBBox(fc geoJSONFeatureCollection, bbox geoBBox) geoJSONFeatureCollection {
	out := geoJSONFeatureCollection{Type: "FeatureCollection"}
	for _, feature := range fc.Features {
		featureBBox, ok := geometryBBox(feature.Geometry)
		if ok && bboxesIntersect(featureBBox, bbox) {
			out.Features = append(out.Features, feature)
		}
	}
	return out
}

func filterGeoJSONProperties(fc geoJSONFeatureCollection, fields, dropFields []string) geoJSONFeatureCollection {
	keep := map[string]bool{}
	for _, field := range fields {
		keep[strings.ToLower(field)] = true
	}
	drop := map[string]bool{}
	for _, field := range dropFields {
		drop[strings.ToLower(field)] = true
	}
	out := geoJSONFeatureCollection{Type: "FeatureCollection", Features: make([]geoJSONFeature, 0, len(fc.Features))}
	for _, feature := range fc.Features {
		props := map[string]any{}
		for key, value := range feature.Properties {
			lower := strings.ToLower(key)
			if len(keep) > 0 && !keep[lower] {
				continue
			}
			if drop[lower] {
				continue
			}
			props[key] = value
		}
		out.Features = append(out.Features, geoJSONFeature{Type: "Feature", Geometry: feature.Geometry, Properties: props})
	}
	return out
}

func geometryBBox(geometry map[string]any) (geoBBox, bool) {
	var points [][]float64
	collectGeometryPoints(geometry, &points)
	if len(points) == 0 {
		return geoBBox{}, false
	}
	bbox := geoBBox{MinX: points[0][0], MinY: points[0][1], MaxX: points[0][0], MaxY: points[0][1]}
	for _, point := range points[1:] {
		bbox.MinX = math.Min(bbox.MinX, point[0])
		bbox.MinY = math.Min(bbox.MinY, point[1])
		bbox.MaxX = math.Max(bbox.MaxX, point[0])
		bbox.MaxY = math.Max(bbox.MaxY, point[1])
	}
	return bbox, true
}

func collectGeometryPoints(geometry map[string]any, points *[][]float64) {
	switch strings.ToLower(geoJSONGeometryType(geometry)) {
	case "point":
		if point, ok := toPoint(geometry["coordinates"]); ok {
			*points = append(*points, point)
		}
	case "multipoint", "linestring":
		*points = append(*points, toPointList(geometry["coordinates"])...)
	case "multilinestring", "polygon":
		for _, line := range toLineList(geometry["coordinates"]) {
			*points = append(*points, line...)
		}
	case "multipolygon":
		for _, polygon := range toPolygonList(geometry["coordinates"]) {
			for _, ring := range polygon {
				*points = append(*points, ring...)
			}
		}
	case "geometrycollection":
		if items, ok := geometry["geometries"].([]any); ok {
			for _, item := range items {
				if child, ok := item.(map[string]any); ok {
					collectGeometryPoints(child, points)
				}
			}
		}
	}
}

func bboxesIntersect(a, b geoBBox) bool {
	return a.MinX <= b.MaxX && a.MaxX >= b.MinX && a.MinY <= b.MaxY && a.MaxY >= b.MinY
}

func simplifyLine(points [][]float64, tolerance float64, minPoints int, closed bool) [][]float64 {
	if len(points) <= minPoints || tolerance <= 0 {
		return points
	}
	input := points
	if closed && samePoint(points[0], points[len(points)-1]) && len(points) > 1 {
		input = points[:len(points)-1]
	}
	if len(input) <= minPoints {
		return points
	}
	keep := make([]bool, len(input))
	keep[0] = true
	keep[len(input)-1] = true
	douglasPeucker(input, tolerance, keep, 0, len(input)-1)
	out := make([][]float64, 0, len(input))
	for i, point := range input {
		if keep[i] {
			out = append(out, point)
		}
	}
	if len(out) < minPoints {
		return points
	}
	if closed {
		out = append(out, append([]float64(nil), out[0]...))
		if len(out) < 4 {
			return points
		}
	}
	return out
}

func simplifyPolygon(rings [][][]float64, tolerance float64) [][][]float64 {
	out := make([][][]float64, 0, len(rings))
	for _, ring := range rings {
		out = append(out, simplifyLine(ring, tolerance, 4, true))
	}
	return out
}

func douglasPeucker(points [][]float64, tolerance float64, keep []bool, start, end int) {
	if end <= start+1 {
		return
	}
	maxDistance := 0.0
	maxIndex := start
	for i := start + 1; i < end; i++ {
		distance := perpendicularDistance(points[i], points[start], points[end])
		if distance > maxDistance {
			maxDistance = distance
			maxIndex = i
		}
	}
	if maxDistance > tolerance {
		keep[maxIndex] = true
		douglasPeucker(points, tolerance, keep, start, maxIndex)
		douglasPeucker(points, tolerance, keep, maxIndex, end)
	}
}

func perpendicularDistance(point, start, end []float64) float64 {
	if samePoint(start, end) {
		return math.Hypot(point[0]-start[0], point[1]-start[1])
	}
	numerator := math.Abs((end[1]-start[1])*point[0] - (end[0]-start[0])*point[1] + end[0]*start[1] - end[1]*start[0])
	denominator := math.Hypot(end[1]-start[1], end[0]-start[0])
	return numerator / denominator
}

func samePoint(a, b []float64) bool {
	return len(a) >= 2 && len(b) >= 2 && a[0] == b[0] && a[1] == b[1]
}

func toPoint(v any) ([]float64, bool) {
	switch coords := v.(type) {
	case []float64:
		if len(coords) >= 2 {
			return []float64{coords[0], coords[1]}, true
		}
	case []any:
		if len(coords) >= 2 {
			x, okX := numericAny(coords[0])
			y, okY := numericAny(coords[1])
			if okX && okY {
				return []float64{x, y}, true
			}
		}
	}
	return nil, false
}

func toPointList(v any) [][]float64 {
	switch coords := v.(type) {
	case [][]float64:
		return coords
	case []any:
		out := make([][]float64, 0, len(coords))
		for _, item := range coords {
			if point, ok := toPoint(item); ok {
				out = append(out, point)
			}
		}
		return out
	default:
		return nil
	}
}

func toLineList(v any) [][][]float64 {
	switch coords := v.(type) {
	case [][][]float64:
		return coords
	case []any:
		out := make([][][]float64, 0, len(coords))
		for _, item := range coords {
			line := toPointList(item)
			if len(line) > 0 {
				out = append(out, line)
			}
		}
		return out
	default:
		return nil
	}
}

func toPolygonList(v any) [][][][]float64 {
	switch coords := v.(type) {
	case [][][][]float64:
		return coords
	case []any:
		out := make([][][][]float64, 0, len(coords))
		for _, item := range coords {
			polygon := toLineList(item)
			if len(polygon) > 0 {
				out = append(out, polygon)
			}
		}
		return out
	default:
		return nil
	}
}

func numericAny(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func cloneGeometry(geometry map[string]any) map[string]any {
	if geometry == nil {
		return nil
	}
	body, err := json.Marshal(geometry)
	if err != nil {
		return geometry
	}
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return geometry
	}
	return normalizeJSONNumbers(out).(map[string]any)
}

func cloneProperties(props map[string]any) map[string]any {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]any, len(props))
	for key, value := range props {
		out[key] = value
	}
	return out
}

func geoJSONGeometryType(geometry map[string]any) string {
	if geometry == nil {
		return ""
	}
	return stringValue(geometry["type"])
}

type kmlDocument struct {
	XMLName  xml.Name       `xml:"kml"`
	Xmlns    string         `xml:"xmlns,attr"`
	Document kmlDocumentTag `xml:"Document"`
}

type kmlDocumentTag struct {
	Placemarks []kmlPlacemark `xml:"Placemark"`
}

type kmlPlacemark struct {
	Name        string         `xml:"name,omitempty"`
	Description string         `xml:"description,omitempty"`
	Point       *kmlPoint      `xml:"Point,omitempty"`
	LineString  *kmlLineString `xml:"LineString,omitempty"`
	Polygon     *kmlPolygon    `xml:"Polygon,omitempty"`
}

type kmlPoint struct {
	Coordinates string `xml:"coordinates"`
}

type kmlLineString struct {
	Coordinates string `xml:"coordinates"`
}

type kmlPolygon struct {
	Outer kmlBoundary   `xml:"outerBoundaryIs"`
	Inner []kmlBoundary `xml:"innerBoundaryIs,omitempty"`
}

type kmlBoundary struct {
	Ring kmlLineString `xml:"LinearRing"`
}

func kmlDocumentFromFeatures(fc geoJSONFeatureCollection) kmlDocument {
	doc := kmlDocument{Xmlns: "http://www.opengis.net/kml/2.2"}
	for _, feature := range explodeGeoJSONFeatureCollection(fc).Features {
		pm, ok := kmlPlacemarkFromFeature(feature)
		if ok {
			doc.Document.Placemarks = append(doc.Document.Placemarks, pm)
		}
	}
	return doc
}

func kmlPlacemarkFromFeature(feature geoJSONFeature) (kmlPlacemark, bool) {
	pm := kmlPlacemark{Name: firstStringProperty(feature.Properties, "name", "title", "id")}
	switch strings.ToLower(geoJSONGeometryType(feature.Geometry)) {
	case "point":
		point, ok := toPoint(feature.Geometry["coordinates"])
		if !ok {
			return pm, false
		}
		pm.Point = &kmlPoint{Coordinates: kmlCoordinates([][]float64{point})}
	case "linestring":
		line := toPointList(feature.Geometry["coordinates"])
		if len(line) == 0 {
			return pm, false
		}
		pm.LineString = &kmlLineString{Coordinates: kmlCoordinates(line)}
	case "polygon":
		rings := toLineList(feature.Geometry["coordinates"])
		if len(rings) == 0 {
			return pm, false
		}
		pm.Polygon = &kmlPolygon{Outer: kmlBoundary{Ring: kmlLineString{Coordinates: kmlCoordinates(rings[0])}}}
		for _, ring := range rings[1:] {
			pm.Polygon.Inner = append(pm.Polygon.Inner, kmlBoundary{Ring: kmlLineString{Coordinates: kmlCoordinates(ring)}})
		}
	default:
		return pm, false
	}
	return pm, true
}

func kmlCoordinates(points [][]float64) string {
	parts := make([]string, 0, len(points))
	for _, point := range points {
		parts = append(parts, strconv.FormatFloat(point[0], 'f', -1, 64)+","+strconv.FormatFloat(point[1], 'f', -1, 64)+",0")
	}
	return strings.Join(parts, " ")
}

type gpxDocument struct {
	XMLName xml.Name   `xml:"gpx"`
	Version string     `xml:"version,attr"`
	Creator string     `xml:"creator,attr"`
	Xmlns   string     `xml:"xmlns,attr"`
	Wpts    []gpxPoint `xml:"wpt,omitempty"`
	Trks    []gpxTrack `xml:"trk,omitempty"`
}

type gpxPoint struct {
	Lat  string `xml:"lat,attr"`
	Lon  string `xml:"lon,attr"`
	Name string `xml:"name,omitempty"`
}

type gpxTrack struct {
	Name string        `xml:"name,omitempty"`
	Segs []gpxTrackSeg `xml:"trkseg"`
}

type gpxTrackSeg struct {
	Points []gpxPoint `xml:"trkpt"`
}

func gpxDocumentFromFeatures(fc geoJSONFeatureCollection) gpxDocument {
	doc := gpxDocument{Version: "1.1", Creator: "DataDock", Xmlns: "http://www.topografix.com/GPX/1/1"}
	for _, feature := range explodeGeoJSONFeatureCollection(fc).Features {
		name := firstStringProperty(feature.Properties, "name", "title", "id")
		switch strings.ToLower(geoJSONGeometryType(feature.Geometry)) {
		case "point":
			if point, ok := toPoint(feature.Geometry["coordinates"]); ok {
				doc.Wpts = append(doc.Wpts, gpxPointFromCoord(point, name))
			}
		case "linestring":
			line := toPointList(feature.Geometry["coordinates"])
			if len(line) > 0 {
				doc.Trks = append(doc.Trks, gpxTrack{Name: name, Segs: []gpxTrackSeg{{Points: gpxPointsFromCoords(line)}}})
			}
		case "polygon":
			for _, ring := range toLineList(feature.Geometry["coordinates"]) {
				if len(ring) > 0 {
					doc.Trks = append(doc.Trks, gpxTrack{Name: name, Segs: []gpxTrackSeg{{Points: gpxPointsFromCoords(ring)}}})
				}
			}
		}
	}
	return doc
}

func gpxPointFromCoord(point []float64, name string) gpxPoint {
	return gpxPoint{
		Lat:  strconv.FormatFloat(point[1], 'f', -1, 64),
		Lon:  strconv.FormatFloat(point[0], 'f', -1, 64),
		Name: name,
	}
}

func gpxPointsFromCoords(points [][]float64) []gpxPoint {
	out := make([]gpxPoint, 0, len(points))
	for _, point := range points {
		out = append(out, gpxPointFromCoord(point, ""))
	}
	return out
}

func firstStringProperty(props map[string]any, names ...string) string {
	for _, name := range names {
		for key, value := range props {
			if strings.EqualFold(key, name) {
				text := strings.TrimSpace(fmt.Sprint(value))
				if text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func safeSQLiteIdentifier(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "export"
	}
	var b strings.Builder
	for i, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			if i == 0 && unicode.IsDigit(r) {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "export"
	}
	return out
}

func uniqueSQLiteColumns(columns []string) []string {
	out := make([]string, len(columns))
	used := map[string]bool{}
	for i, col := range columns {
		base := safeSQLiteIdentifier(col)
		if base == "" {
			base = fmt.Sprintf("col_%d", i+1)
		}
		name := base
		for suffix := 2; used[strings.ToLower(name)]; suffix++ {
			name = fmt.Sprintf("%s_%d", base, suffix)
		}
		used[strings.ToLower(name)] = true
		out[i] = name
	}
	return out
}

func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteSQLiteIdentifiers(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = quoteSQLiteIdentifier(name)
	}
	return strings.Join(quoted, ", ")
}

func shapefileGeometryType(fc geoJSONFeatureCollection) (shp.ShapeType, string) {
	for _, feature := range fc.Features {
		switch strings.ToLower(geoJSONGeometryType(feature.Geometry)) {
		case "point":
			return shp.POINT, "point"
		case "multipoint":
			return shp.MULTIPOINT, "multipoint"
		case "linestring":
			return shp.POLYLINE, "linestring"
		case "polygon":
			return shp.POLYGON, "polygon"
		}
	}
	return shp.NULL, ""
}

func shapefileFieldNames(fc geoJSONFeatureCollection) []string {
	seen := map[string]bool{}
	var names []string
	for _, feature := range fc.Features {
		for key := range feature.Properties {
			if !seen[key] {
				seen[key] = true
				names = append(names, key)
			}
		}
	}
	sort.Strings(names)
	if len(names) > 64 {
		names = names[:64]
	}
	return names
}

func shapefileFields(fc geoJSONFeatureCollection) []shp.Field {
	names := shapefileFieldNames(fc)
	fields := make([]shp.Field, 0, len(names))
	used := map[string]bool{}
	for _, name := range names {
		dbfName := safeDBFFieldName(name, used)
		fields = append(fields, shp.StringField(dbfName, 254))
	}
	return fields
}

func safeDBFFieldName(name string, used map[string]bool) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	base := b.String()
	if base == "" {
		base = "FIELD"
	}
	if len(base) > 10 {
		base = base[:10]
	}
	out := base
	for suffix := 2; used[out]; suffix++ {
		tail := strconv.Itoa(suffix)
		cut := 10 - len(tail)
		if cut < 1 {
			cut = 1
		}
		out = base
		if len(out) > cut {
			out = out[:cut]
		}
		out += tail
	}
	used[out] = true
	return out
}

func geoJSONGeometryToShape(geometry map[string]any, geomType string) shp.Shape {
	switch geomType {
	case "point":
		point, ok := toPoint(geometry["coordinates"])
		if !ok {
			return nil
		}
		return &shp.Point{X: point[0], Y: point[1]}
	case "multipoint":
		points := shpPoints(toPointList(geometry["coordinates"]))
		if len(points) == 0 {
			return nil
		}
		return &shp.MultiPoint{Box: shp.BBoxFromPoints(points), NumPoints: int32(len(points)), Points: points}
	case "linestring":
		points := shpPoints(toPointList(geometry["coordinates"]))
		if len(points) < 2 {
			return nil
		}
		return shp.NewPolyLine([][]shp.Point{points})
	case "polygon":
		rings := toLineList(geometry["coordinates"])
		parts := make([][]shp.Point, 0, len(rings))
		for _, ring := range rings {
			points := shpPoints(ring)
			if len(points) >= 4 {
				parts = append(parts, points)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		poly := shp.NewPolyLine(parts)
		return (*shp.Polygon)(poly)
	default:
		return nil
	}
}

func shpPoints(points [][]float64) []shp.Point {
	out := make([]shp.Point, 0, len(points))
	for _, point := range points {
		if len(point) >= 2 {
			out = append(out, shp.Point{X: point[0], Y: point[1]})
		}
	}
	return out
}
