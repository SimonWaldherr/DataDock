package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const spatialReportsTable = "__datadock_spatial_reports"

const (
	spatialReportsEnsureTableSQL = "CREATE TABLE " + spatialReportsTable + " (table_name TEXT, report_json TEXT, updated_at TEXT)"
	spatialReportsDeleteSQL      = "DELETE FROM " + spatialReportsTable + " WHERE table_name = ?"
	spatialReportsInsertSQL      = "INSERT INTO " + spatialReportsTable + " (table_name, report_json, updated_at) VALUES (?, ?, ?)"
	spatialReportsSelectSQL      = "SELECT report_json FROM " + spatialReportsTable + " WHERE table_name = ?"
)

// SpatialImportReport records chain-of-custody data for an import plus a
// compact quality profile. It describes the normalized rows in DataDock, not
// merely the original upload.
type SpatialImportReport struct {
	Table               string         `json:"table"`
	SourceName          string         `json:"source_name"`
	SourceFormat        string         `json:"source_format"`
	SourceSHA256        string         `json:"source_sha256"`
	SourceSizeBytes     int            `json:"source_size_bytes"`
	ImportedAt          string         `json:"imported_at"`
	Rows                int            `json:"rows"`
	GeometryColumn      string         `json:"geometry_column,omitempty"`
	LongitudeColumn     string         `json:"longitude_column,omitempty"`
	LatitudeColumn      string         `json:"latitude_column,omitempty"`
	GeometryTypes       map[string]int `json:"geometry_types,omitempty"`
	ValidGeometryRows   int            `json:"valid_geometry_rows"`
	MissingGeometryRows int            `json:"missing_geometry_rows"`
	InvalidGeometryRows int            `json:"invalid_geometry_rows"`
	BBox                []float64      `json:"bbox,omitempty"`
	Status              string         `json:"status"`
}

func (a *App) ensureSpatialReportsTable(ctx context.Context) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "spatial_report.ensure_table", spatialReportsEnsureTableSQL)
	if err == nil || strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return fmt.Errorf("ensure spatial reports table: %w", err)
}

func (a *App) recordSpatialImportReport(ctx context.Context, table, sourceName, sourceFormat string, source []byte) (*SpatialImportReport, error) {
	report, err := a.inspectSpatialTable(ctx, table)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(source)
	report.SourceName = strings.TrimSpace(sourceName)
	report.SourceFormat = strings.ToLower(strings.TrimSpace(sourceFormat))
	report.SourceSHA256 = fmt.Sprintf("%x", hash[:])
	report.SourceSizeBytes = len(source)
	report.ImportedAt = time.Now().UTC().Format(time.RFC3339)
	if err := a.ensureSpatialReportsTable(ctx); err != nil {
		return nil, err
	}
	body, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	if _, err := a.execConn(ctx, a.localTinySQLConn(), "spatial_report.replace", spatialReportsDeleteSQL, report.Table); err != nil {
		return nil, err
	}
	if _, err := a.execConn(ctx, a.localTinySQLConn(), "spatial_report.insert", spatialReportsInsertSQL, report.Table, string(body), report.ImportedAt); err != nil {
		return nil, err
	}
	return report, nil
}

func (a *App) deleteSpatialImportReport(ctx context.Context, table string) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "spatial_report.delete", spatialReportsDeleteSQL, table)
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "not found") || strings.Contains(strings.ToLower(err.Error()), "does not exist")) {
		return nil
	}
	return err
}

func (a *App) hasSpatialImportReport(ctx context.Context, table string) bool {
	_, err := a.loadSpatialImportReport(ctx, table)
	return err == nil
}

func (a *App) loadSpatialImportReport(ctx context.Context, table string) (*SpatialImportReport, error) {
	if !isValidIdentifier(table) || isDataDockSystemObject(table) {
		return nil, fmt.Errorf("invalid table")
	}
	var raw string
	if err := a.sqlDB.QueryRowContext(ctx, spatialReportsSelectSQL, table).Scan(&raw); err != nil {
		return nil, err
	}
	var report SpatialImportReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return nil, err
	}
	return &report, nil
}

func (a *App) spatialQualityViewHandler(w http.ResponseWriter, r *http.Request) {
	table := r.PathValue("table")
	if !a.canBrowseTableName(r, table) {
		a.renderObjectMissing(w, r, table, fmt.Errorf("table %q not found", table))
		return
	}
	if conn := a.activeConn(r.Context()); conn == nil || !conn.IsTinySQL() {
		a.renderObjectMissing(w, r, table, fmt.Errorf("import reports are available only from the local tinySQL database"))
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	report, err := a.loadSpatialImportReport(ctx, table)
	if err != nil {
		a.renderObjectMissing(w, r, table, fmt.Errorf("no import report is available for %q", table))
		return
	}
	a.render(w, r, "spatial_quality", map[string]any{"Table": table, "Report": report})
}

func (a *App) apiSpatialReportHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn == nil || !conn.IsTinySQL() {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported connection", "import reports are available only from the local tinySQL database")
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	report, err := a.loadSpatialImportReport(ctx, r.PathValue("table"))
	if err != nil {
		a.writeProblem(w, r, http.StatusNotFound, "Import report not found", "no report is available for the requested table")
		return
	}
	a.writeJSON(w, http.StatusOK, report)
}

func (a *App) inspectSpatialTable(ctx context.Context, table string) (*SpatialImportReport, error) {
	if !isValidIdentifier(table) || isDataDockSystemObject(table) {
		return nil, fmt.Errorf("invalid table")
	}
	rows, err := a.sqlDB.QueryContext(ctx, "SELECT * FROM "+quoteName(table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	geometryIndex := spatialGeometryColumnIndex(columns)
	lonIndex, latIndex := detectLonLatColumns(columns)
	report := &SpatialImportReport{Table: table, GeometryTypes: map[string]int{}}
	if geometryIndex >= 0 {
		report.GeometryColumn = columns[geometryIndex]
	} else if lonIndex >= 0 && latIndex >= 0 {
		report.LongitudeColumn = columns[lonIndex]
		report.LatitudeColumn = columns[latIndex]
	}
	bounds := spatialBounds{}
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(values))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		report.Rows++
		if geometryIndex >= 0 {
			raw := spatialValueString(values[geometryIndex])
			if strings.TrimSpace(raw) == "" {
				report.MissingGeometryRows++
				continue
			}
			typeName, points, valid := inspectSpatialGeometry(raw)
			if !valid {
				report.InvalidGeometryRows++
				continue
			}
			report.ValidGeometryRows++
			report.GeometryTypes[typeName]++
			for _, point := range points {
				bounds.Add(point[0], point[1])
			}
			continue
		}
		if lonIndex >= 0 && latIndex >= 0 {
			lonRaw, latRaw := spatialValueString(values[lonIndex]), spatialValueString(values[latIndex])
			if strings.TrimSpace(lonRaw) == "" && strings.TrimSpace(latRaw) == "" {
				report.MissingGeometryRows++
				continue
			}
			lon, lonOK := routingFloat(lonRaw)
			lat, latOK := routingFloat(latRaw)
			if !lonOK || !latOK || lon < -180 || lon > 180 || lat < -90 || lat > 90 {
				report.InvalidGeometryRows++
				continue
			}
			report.ValidGeometryRows++
			report.GeometryTypes["Point"]++
			bounds.Add(lon, lat)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if bounds.Set {
		report.BBox = []float64{bounds.MinLon, bounds.MinLat, bounds.MaxLon, bounds.MaxLat}
	}
	if report.GeometryColumn == "" && report.LongitudeColumn == "" {
		report.Status = "not_spatial"
	} else if report.InvalidGeometryRows > 0 || report.MissingGeometryRows > 0 {
		report.Status = "warning"
	} else {
		report.Status = "ok"
	}
	return report, nil
}

func spatialGeometryColumnIndex(columns []string) int {
	for i, column := range columns {
		name := strings.ToLower(strings.TrimSpace(column))
		if name == "geometry" || name == "geojson" || name == "geom" || name == "shape" {
			return i
		}
	}
	for i, column := range columns {
		name := strings.ToLower(strings.TrimSpace(column))
		if geometryColumnRE.MatchString(name) && !strings.HasSuffix(name, "_type") && name != "geometry_type" {
			return i
		}
	}
	return -1
}

func spatialValueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(value)
	}
}

type spatialBounds struct {
	MinLon, MinLat, MaxLon, MaxLat float64
	Set                            bool
}

func (bounds *spatialBounds) Add(lon, lat float64) {
	if !bounds.Set {
		bounds.MinLon, bounds.MaxLon = lon, lon
		bounds.MinLat, bounds.MaxLat = lat, lat
		bounds.Set = true
		return
	}
	bounds.MinLon = math.Min(bounds.MinLon, lon)
	bounds.MaxLon = math.Max(bounds.MaxLon, lon)
	bounds.MinLat = math.Min(bounds.MinLat, lat)
	bounds.MaxLat = math.Max(bounds.MaxLat, lat)
}

func inspectSpatialGeometry(raw string) (string, [][]float64, bool) {
	geometry, ok := parseGeoJSONObject(raw)
	if !ok || !isGeoJSONGeometry(geometry) {
		return "", nil, false
	}
	typeName := fmt.Sprint(geometry["type"])
	points, valid := spatialGeometryPoints(geometry)
	if !valid || len(points) == 0 {
		return "", nil, false
	}
	return typeName, points, true
}

func spatialGeometryPoints(geometry map[string]any) ([][]float64, bool) {
	typeName := strings.ToLower(fmt.Sprint(geometry["type"]))
	if typeName == "geometrycollection" {
		children, ok := geometry["geometries"].([]any)
		if !ok || len(children) == 0 {
			return nil, false
		}
		var points [][]float64
		for _, child := range children {
			object, ok := child.(map[string]any)
			if !ok {
				return nil, false
			}
			childPoints, valid := spatialGeometryPoints(object)
			if !valid {
				return nil, false
			}
			points = append(points, childPoints...)
		}
		return points, len(points) > 0
	}
	if !geoJSONGeometryNames[typeName] {
		return nil, false
	}
	points, valid := spatialCoordinatePairs(geometry["coordinates"])
	minimum := 1
	if typeName == "linestring" || typeName == "multilinestring" {
		minimum = 2
	}
	if typeName == "polygon" || typeName == "multipolygon" {
		minimum = 4
	}
	return points, valid && len(points) >= minimum
}

func spatialCoordinatePairs(value any) ([][]float64, bool) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, false
	}
	if len(items) >= 2 {
		lon, lonOK := spatialCoordinateFloat(items[0])
		lat, latOK := spatialCoordinateFloat(items[1])
		if lonOK && latOK {
			if lon < -180 || lon > 180 || lat < -90 || lat > 90 {
				return nil, false
			}
			return [][]float64{{lon, lat}}, true
		}
	}
	var points [][]float64
	for _, child := range items {
		childPoints, valid := spatialCoordinatePairs(child)
		if !valid {
			return nil, false
		}
		points = append(points, childPoints...)
	}
	return points, len(points) > 0
}

func spatialCoordinateFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		parsed, err := strconv.ParseFloat(fmt.Sprint(value), 64)
		return parsed, err == nil
	}
}
